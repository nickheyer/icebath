package googlechat

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	oauthTokenEndpoint  = "https://oauth2.googleapis.com/token"
	defaultUniverse     = "googleapis.com"
	jwtBearerGrant      = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	tokenLifetime       = time.Hour
	refreshThreshold    = 3*time.Minute + 45*time.Second
	cloudSDKClientID    = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	iamScope            = "https://www.googleapis.com/auth/iam"
	cloudPlatformScope  = "https://www.googleapis.com/auth/cloud-platform"
	iamGenerateTokenURL = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken"
)

const adcMissingMessage = "Your default credentials were not found. To set up Application Default Credentials, " +
	"see https://cloud.google.com/docs/authentication/external/set-up-adc for more information."

const cloudSDKQuotaWarning = "Your application has authenticated using end user credentials from Google Cloud SDK " +
	"without a quota project. You might receive a \"quota exceeded\" or \"API not enabled\" error. See " +
	"https://cloud.google.com/docs/authentication/adc-troubleshooting/user-creds"

// Authorization header source for Google API requests
type tokenSource interface {
	authorize(ctx context.Context, h http.Header) error
}

// One Google credential caching its access token until the refresh threshold
type googleCredentials struct {
	kind         string
	quotaProject string
	mint         func(ctx context.Context) (string, time.Time, error)

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// Cached access token, minted again once it nears expiry
func (c *googleCredentials) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && (c.expiry.IsZero() || time.Now().Before(c.expiry.Add(-refreshThreshold))) {
		return c.token, nil
	}
	token, expiry, err := c.mint(ctx)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("%s credentials returned no access token", c.kind)
	}
	c.token, c.expiry = token, expiry
	return token, nil
}

// Sets the bearer token and the quota project header
func (c *googleCredentials) authorize(ctx context.Context, h http.Header) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	h.Set("Authorization", "Bearer "+token)
	if c.quotaProject != "" {
		h.Set("x-goog-user-project", c.quotaProject)
	}
	return nil
}

// HTTP client for token endpoints, honoring the proxy environment
var authHTTPClient = func() *http.Client { return &http.Client{Timeout: 120 * time.Second} }

// Token endpoint rejection with the decoded response body
type tokenError struct {
	msg       string
	retryable bool
}

func (e *tokenError) Error() string { return e.msg }

// String field of a decoded JSON object
func str(info map[string]any, key string) string {
	s, _ := info[key].(string)
	return s
}

// Names of the required keys the info lacks
func missingFields(info map[string]any, keys ...string) []string {
	var missing []string
	for _, k := range keys {
		if _, ok := info[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

// RSA or EC private key from PKCS1, SEC1 or PKCS8 PEM
func parsePrivateKey(pemText string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("no PEM private key found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return k, nil
	case *ecdsa.PrivateKey:
		return k, nil
	}
	return nil, errors.New("private key is neither RSA nor EC")
}

// Compact JWS of the claims, RS256 for RSA keys and ES256 for EC keys
func signJWT(key crypto.Signer, keyID string, claims jwt.MapClaims) (string, error) {
	var method jwt.SigningMethod = jwt.SigningMethodRS256
	if _, ok := key.(*ecdsa.PrivateKey); ok {
		method = jwt.SigningMethodES256
	}
	t := jwt.NewWithClaims(method, claims)
	if keyID != "" {
		t.Header["kid"] = keyID
	}
	return t.SignedString(key)
}

// Whether a token endpoint failure may succeed on retry
func tokenRetryable(status int, data map[string]any) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	for _, v := range []string{str(data, "error"), str(data, "error_description")} {
		if v == "internal_failure" || v == "server_error" || v == "temporarily_unavailable" {
			return true
		}
	}
	return false
}

// Error text of an OAuth error body
func tokenErrorText(status int, raw []byte, data map[string]any) string {
	if code := str(data, "error"); code != "" {
		if desc := str(data, "error_description"); desc != "" {
			return fmt.Sprintf("%s: %s", code, desc)
		}
		return code
	}
	return fmt.Sprintf("token endpoint returned %d: %s", status, strings.TrimSpace(string(raw)))
}

// Posts a token request, retrying transient failures three times with jittered backoff
func postToken(ctx context.Context, endpoint string, form url.Values, jsonBody any, client *http.Client) (map[string]any, error) {
	payload, contentType := []byte(form.Encode()), "application/x-www-form-urlencoded"
	if jsonBody != nil {
		b, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, err
		}
		payload, contentType = b, "application/json"
	}
	if client == nil {
		client = authHTTPClient()
	}
	wait := time.Second
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, time.Duration(float64(wait)*(0.9+rand.Float64()*0.2))); err != nil {
				return nil, err
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		if resp.StatusCode == http.StatusOK && data != nil {
			return data, nil
		}
		retry := tokenRetryable(resp.StatusCode, data)
		last = &tokenError{msg: tokenErrorText(resp.StatusCode, raw, data), retryable: retry}
		if !retry {
			break
		}
	}
	return nil, last
}

// Expiry from expires_in, accepting the string form some services send
func expiryFrom(data map[string]any, now time.Time) time.Time {
	switch v := data["expires_in"].(type) {
	case float64:
		return now.Add(time.Duration(v * float64(time.Second)))
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return now.Add(time.Duration(n) * time.Second)
		}
	}
	return time.Time{}
}

// Access token and expiry of a token endpoint answer
func tokenFrom(data map[string]any, now time.Time) (string, time.Time, error) {
	token := str(data, "access_token")
	if token == "" {
		return "", time.Time{}, errors.New("no access token in token endpoint response")
	}
	return token, expiryFrom(data, now), nil
}

// Service account credentials exchanging a signed JWT grant, self signed outside the default universe
func serviceAccountCredentials(info map[string]any, scopes []string) (*googleCredentials, error) {
	if missing := missingFields(info, "client_email", "token_uri"); len(missing) > 0 {
		return nil, fmt.Errorf("service account info is missing fields %s", strings.Join(missing, ", "))
	}
	pk := str(info, "private_key")
	if pk == "" {
		return nil, errors.New("service account info has no private_key")
	}
	key, err := parsePrivateKey(pk)
	if err != nil {
		return nil, err
	}
	email, tokenURI, keyID := str(info, "client_email"), str(info, "token_uri"), str(info, "private_key_id")
	universe := str(info, "universe_domain")
	scope := strings.Join(scopes, " ")
	c := &googleCredentials{kind: "service_account", quotaProject: str(info, "quota_project_id")}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		expiry := now.Add(tokenLifetime)
		if universe != "" && universe != defaultUniverse {
			claims := jwt.MapClaims{"iss": email, "sub": email, "iat": now.Unix(), "exp": expiry.Unix()}
			if scope != "" {
				claims["scope"] = scope
			}
			token, err := signJWT(key, keyID, claims)
			return token, expiry, err
		}
		assertion, err := signJWT(key, keyID, jwt.MapClaims{"iss": email, "aud": oauthTokenEndpoint, "scope": scope,
			"iat": now.Unix(), "exp": expiry.Unix()})
		if err != nil {
			return "", time.Time{}, err
		}
		data, err := postToken(ctx, tokenURI, url.Values{"assertion": {assertion}, "grant_type": {jwtBearerGrant}}, nil, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		return tokenFrom(data, time.Now().UTC())
	}
	return c, nil
}

// End user credentials refreshed with the refresh token grant, every requested scope required in the grant
func authorizedUserCredentials(info map[string]any, scopes []string) (*googleCredentials, error) {
	if missing := missingFields(info, "refresh_token", "client_id", "client_secret"); len(missing) > 0 {
		return nil, fmt.Errorf("authorized user info is missing fields %s", strings.Join(missing, ", "))
	}
	tokenURI := str(info, "token_uri")
	if tokenURI == "" {
		tokenURI = oauthTokenEndpoint
	}
	clientID, clientSecret, refresh, rapt := str(info, "client_id"), str(info, "client_secret"), str(info, "refresh_token"), str(info, "rapt_token")
	c := &googleCredentials{kind: "authorized_user", quotaProject: str(info, "quota_project_id")}
	if clientID == cloudSDKClientID && c.quotaProject == "" {
		slog.Warn(cloudSDKQuotaWarning)
	}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "client_secret": {clientSecret}, "refresh_token": {refresh}}
		if len(scopes) > 0 {
			form.Set("scope", strings.Join(scopes, " "))
		}
		if rapt != "" {
			form.Set("rapt", rapt)
		}
		data, err := postToken(ctx, tokenURI, form, nil, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		if rt := str(data, "refresh_token"); rt != "" {
			refresh = rt
		}
		if granted := str(data, "scope"); granted != "" && len(scopes) > 0 {
			have := map[string]bool{}
			for _, s := range strings.Fields(granted) {
				have[s] = true
			}
			var missing []string
			for _, s := range scopes {
				if !have[s] {
					missing = append(missing, s)
				}
			}
			if len(missing) > 0 {
				return "", time.Time{}, fmt.Errorf("authorization server did not grant scopes %s", strings.Join(missing, ", "))
			}
		}
		return tokenFrom(data, time.Now().UTC())
	}
	return c, nil
}

// Credentials of the target service account minted by IAM generateAccessToken under the source credentials
func impersonatedCredentials(source *googleCredentials, endpoint string, delegates, scopes []string, lifetime time.Duration) *googleCredentials {
	if lifetime <= 0 {
		lifetime = tokenLifetime
	}
	c := &googleCredentials{kind: "impersonated_service_account"}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		body := map[string]any{"delegates": delegates, "scope": scopes, "lifetime": strconv.Itoa(int(lifetime/time.Second)) + "s"}
		b, err := json.Marshal(body)
		if err != nil {
			return "", time.Time{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := source.authorize(ctx, req.Header); err != nil {
			return "", time.Time{}, err
		}
		resp, err := authHTTPClient().Do(req)
		if err != nil {
			return "", time.Time{}, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var out struct {
			AccessToken string `json:"accessToken"`
			ExpireTime  string `json:"expireTime"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
			return "", time.Time{}, fmt.Errorf("unable to acquire impersonated credentials: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		exp, err := time.Parse(time.RFC3339, out.ExpireTime)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("unable to acquire impersonated credentials: bad expireTime %q", out.ExpireTime)
		}
		return out.AccessToken, exp, nil
	}
	return c
}

// Target principal named in a generateAccessToken URL
func impersonationTarget(impURL string) (string, error) {
	start, end := strings.LastIndex(impURL, "/"), strings.Index(impURL, ":generateAccessToken")
	if start == -1 || end == -1 || end <= start {
		return "", fmt.Errorf("cannot extract target principal from %s", impURL)
	}
	return impURL[start+1 : end], nil
}

// impersonated_service_account file credentials over an authorized user, service account or external account source
func impersonatedFromInfo(info map[string]any, scopes []string) (*googleCredentials, error) {
	impURL := str(info, "service_account_impersonation_url")
	srcInfo, ok := info["source_credentials"].(map[string]any)
	if impURL == "" || !ok {
		return nil, errors.New("impersonated credentials need source_credentials and service_account_impersonation_url")
	}
	var src *googleCredentials
	var err error
	switch typ := str(srcInfo, "type"); typ {
	case "authorized_user":
		src, err = authorizedUserCredentials(srcInfo, nil)
	case "service_account":
		src, err = serviceAccountCredentials(srcInfo, []string{iamScope})
	case "external_account":
		src, err = externalAccountCredentials(srcInfo, []string{cloudPlatformScope})
	case "external_account_authorized_user":
		src, err = externalAuthorizedUserCredentials(srcInfo)
	default:
		return nil, fmt.Errorf("impersonation source credentials of type %q are not supported", typ)
	}
	if err != nil {
		return nil, err
	}
	if _, err := impersonationTarget(impURL); err != nil {
		return nil, err
	}
	var delegates []string
	if raw, ok := info["delegates"].([]any); ok {
		for _, d := range raw {
			if s, ok := d.(string); ok {
				delegates = append(delegates, s)
			}
		}
	}
	c := impersonatedCredentials(src, impURL, delegates, scopes, tokenLifetime)
	c.quotaProject = str(info, "quota_project_id")
	return c, nil
}

// Google Distributed Cloud Hosted service account exchanging an ES256 self signed JWT at token_uri
func gdchCredentials(info map[string]any) (*googleCredentials, error) {
	if str(info, "format_version") != "1" {
		return nil, errors.New("gdch service account info supports format version 1 only")
	}
	if missing := missingFields(info, "project", "private_key_id", "private_key", "name", "token_uri"); len(missing) > 0 {
		return nil, fmt.Errorf("gdch service account info is missing fields %s", strings.Join(missing, ", "))
	}
	key, err := parsePrivateKey(str(info, "private_key"))
	if err != nil {
		return nil, err
	}
	tokenURI, caPath, keyID := str(info, "token_uri"), str(info, "ca_cert_path"), str(info, "private_key_id")
	issuer := fmt.Sprintf("system:serviceaccount:%s:%s", str(info, "project"), str(info, "name"))
	c := &googleCredentials{kind: "gdch_service_account"}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		assertion, err := signJWT(key, keyID, jwt.MapClaims{"iss": issuer, "sub": issuer, "aud": tokenURI, "iat": now.Unix(),
			"exp": now.Add(tokenLifetime).Unix()})
		if err != nil {
			return "", time.Time{}, err
		}
		client := authHTTPClient()
		if caPath != "" {
			pemCA, err := os.ReadFile(caPath)
			if err != nil {
				return "", time.Time{}, err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pemCA)
			client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		data, err := postToken(ctx, tokenURI, nil, map[string]any{"grant_type": "urn:ietf:params:oauth:token-type:token-exchange",
			"audience": nil, "requested_token_type": "urn:ietf:params:oauth:token-type:access_token", "subject_token": assertion,
			"subject_token_type": "urn:k8s:params:oauth:token-type:serviceaccount"}, client)
		if err != nil {
			return "", time.Time{}, err
		}
		return tokenFrom(data, time.Now().UTC())
	}
	return c, nil
}

// Root URL of the GCE metadata server honoring the host overrides
func metadataRoot() string {
	host := os.Getenv("GCE_METADATA_HOST")
	if host == "" {
		host = os.Getenv("GCE_METADATA_ROOT")
	}
	if host == "" {
		host = "metadata.google.internal"
	}
	return "http://" + host + "/computeMetadata/v1/"
}

// Direct client for the link local metadata server
func metadataClient() *http.Client {
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

// Whether the metadata server answers with the Google flavor, three attempts with backoff
func metadataAvailable(ctx context.Context) bool {
	ip := os.Getenv("GCE_METADATA_IP")
	if ip == "" {
		ip = "169.254.169.254"
	}
	client := metadataClient()
	wait := time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if sleepCtx(ctx, wait) != nil {
				return false
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ip, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			slog.Debug(fmt.Sprintf("Compute Engine metadata server unavailable on attempt %d of 3: %v", attempt, err))
			continue
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK && resp.Header.Get("Metadata-Flavor") == "Google"
	}
	if runtime.GOOS != "linux" {
		return false
	}
	product, err := os.ReadFile("/sys/class/dmi/id/product_name")
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(product)), "Google")
}

// JSON metadata value, five attempts over transient statuses and transport errors
func metadataGet(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	target := metadataRoot() + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	client := metadataClient()
	wait := time.Second
	var failure string
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			failure = err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
			failure = fmt.Sprintf("status %d", resp.StatusCode)
			continue
		case http.StatusOK:
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("metadata server returned invalid JSON for %s", path)
			}
			return out, nil
		}
		return nil, fmt.Errorf("metadata server returned %d for %s: %s", resp.StatusCode, path, strings.TrimSpace(string(raw)))
	}
	return nil, fmt.Errorf("metadata server unavailable for %s: %s", path, failure)
}

// Default service account credentials of the Compute Engine, Cloud Run or GKE host
func computeEngineCredentials(scopes []string) *googleCredentials {
	c := &googleCredentials{kind: "compute_engine"}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		params := url.Values{}
		if len(scopes) > 0 {
			params.Set("scopes", strings.Join(scopes, ","))
		}
		data, err := metadataGet(ctx, "instance/service-accounts/default/token", params)
		if err != nil {
			return "", time.Time{}, err
		}
		return tokenFrom(data, time.Now().UTC())
	}
	return c
}

// gcloud configuration directory honoring CLOUDSDK_CONFIG and the Windows APPDATA layout
func cloudSDKConfigDir() string {
	if v, ok := os.LookupEnv("CLOUDSDK_CONFIG"); ok {
		return v
	}
	if runtime.GOOS != "windows" {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, ".config", "gcloud")
	}
	if v, ok := os.LookupEnv("APPDATA"); ok {
		return filepath.Join(v, "gcloud")
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	return filepath.Join(drive+`\`, "gcloud")
}

// Credentials of any supported credential file type
func credentialsFromInfo(info map[string]any, source string, scopes []string) (*googleCredentials, error) {
	var c *googleCredentials
	var err error
	switch typ := str(info, "type"); typ {
	case "authorized_user":
		c, err = authorizedUserCredentials(info, scopes)
	case "service_account":
		c, err = serviceAccountCredentials(info, scopes)
	case "external_account":
		c, err = externalAccountCredentials(info, scopes)
	case "external_account_authorized_user":
		c, err = externalAuthorizedUserCredentials(info)
	case "impersonated_service_account":
		c, err = impersonatedFromInfo(info, scopes)
	case "gdch_service_account":
		c, err = gdchCredentials(info)
	default:
		return nil, fmt.Errorf("credential file %s has unsupported type %q", source, typ)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load %s credentials from %s: %w", str(info, "type"), source, err)
	}
	return c, nil
}

// Credentials from a credential JSON file of any supported type
func credentialsFromFile(path string, scopes []string) (*googleCredentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("credential file %s was not found", path)
	}
	var info map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &info) != nil || info == nil {
		return nil, fmt.Errorf("credential file %s is not valid JSON", path)
	}
	return credentialsFromInfo(info, path, scopes)
}

// Application Default Credentials in the GOOGLE_APPLICATION_CREDENTIALS, gcloud file, metadata server order
func defaultCredentials(ctx context.Context, scopes []string) (*googleCredentials, error) {
	adcFile := filepath.Join(cloudSDKConfigDir(), "application_default_credentials.json")
	if explicit, ok := os.LookupEnv("GOOGLE_APPLICATION_CREDENTIALS"); ok {
		return credentialsFromFile(explicit, scopes)
	}
	if st, err := os.Stat(adcFile); err == nil && st.Mode().IsRegular() {
		return credentialsFromFile(adcFile, scopes)
	}
	if metadataAvailable(ctx) {
		return computeEngineCredentials(scopes), nil
	}
	return nil, errors.New(adcMissingMessage)
}

// Waits for the duration or the context, whichever ends first
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

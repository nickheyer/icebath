package provider

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gcpOAuth2TokenEndpoint = "https://oauth2.googleapis.com/token"
	gcpDefaultUniverse     = "googleapis.com"
	gcpJWTGrantType        = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	gcpRefreshGrantType    = "refresh_token"
	gcpTokenLifetime       = 3600 * time.Second
	gcpRefreshThreshold    = 3*time.Minute + 45*time.Second
	gcpCloudSDKClientID    = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	gcpIAMScope            = "https://www.googleapis.com/auth/iam"
	gcpCloudPlatformScope  = "https://www.googleapis.com/auth/cloud-platform"
)

const gcpCloudSDKMissingCredentials = "Your default credentials were not found. To set up Application Default Credentials, " +
	"see https://cloud.google.com/docs/authentication/external/set-up-adc for more information."

const gcpCloudSDKCredentialsWarning = "Your application has authenticated using end user credentials from Google Cloud SDK " +
	"without a quota project. You might receive a \"quota exceeded\" or \"API not enabled\" error. See the following page " +
	"for troubleshooting: https://cloud.google.com/docs/authentication/adc-troubleshooting/user-creds. "

var gcpValidCredentialTypes = []string{"authorized_user", "service_account", "external_account",
	"external_account_authorized_user", "impersonated_service_account", "gdch_service_account"}

var (
	gcpNow          = time.Now
	gcpHTTPClient   = func() *http.Client { return &http.Client{Timeout: 120 * time.Second} }
	gcpBackoffSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	gcpProductNamePath = "/sys/class/dmi/id/product_name"
	gcpGcloudCommand   = func() string {
		if runtime.GOOS == "windows" {
			return "gcloud.cmd"
		}
		return "gcloud"
	}
)

// Token refresh failure carrying the endpoint response and whether a retry could succeed
type gcpRefreshError struct {
	Msg       string
	Data      any
	Retryable bool
}

func (e *gcpRefreshError) Error() string { return e.Msg }

// Missing or unusable Application Default Credentials
type gcpDefaultCredentialsError struct{ Msg string }

func (e *gcpDefaultCredentialsError) Error() string { return e.Msg }

// Metadata server or token endpoint unreachable
type gcpTransportError struct{ Msg string }

func (e *gcpTransportError) Error() string { return e.Msg }

// One google-auth credential with its cached token, project and the mint step of its kind
type gcpCredentials struct {
	kind           string
	projectID      string
	quotaProjectID string
	scopes         []string
	mu             sync.Mutex
	token          string
	expiry         time.Time
	mint           func(ctx context.Context) (string, time.Time, error)
	projectLookup  func(ctx context.Context) (string, error)
}

// Mints a new token through the credential kind and stores it with its expiry
func (c *gcpCredentials) Refresh(ctx context.Context) error {
	token, expiry, err := c.mint(ctx)
	if err != nil {
		return err
	}
	c.token, c.expiry = token, expiry
	return nil
}

// True once the token is within the google-auth refresh threshold of its expiry
func (c *gcpCredentials) Expired() bool {
	if c.expiry.IsZero() {
		return false
	}
	return !gcpNow().Before(c.expiry.Add(-gcpRefreshThreshold))
}

// True with a token that has not expired
func (c *gcpCredentials) Valid() bool { return c.token != "" && !c.Expired() }

// Bearer and quota project headers, refreshing first when the token is not valid
func (c *gcpCredentials) beforeRequest(ctx context.Context, h http.Header) error {
	if !c.Valid() {
		if err := c.Refresh(ctx); err != nil {
			return err
		}
	}
	h.Set("Authorization", "Bearer "+c.token)
	if c.quotaProjectID != "" {
		h.Set("x-goog-user-project", c.quotaProjectID)
	}
	return nil
}

func gcpScopesString(scopes []string) string { return strings.Join(scopes, " ") }

// Fields the credential info lacks, in the order they were required
func gcpMissingFields(info map[string]any, keys ...string) []string {
	var missing []string
	for _, k := range keys {
		if _, ok := info[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

func gcpStr(info map[string]any, key string) string {
	if v, ok := info[key].(string); ok {
		return v
	}
	return ""
}

// Parses a PEM RSA or EC private key as the google-auth signers accept it
func gcpParsePrivateKey(pemText string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("No key could be detected.")
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
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("No key could be detected.")
	}
	return signer, nil
}

// Compact JWS over the payload, RS256 for RSA keys and ES256 for EC keys, with the key id header
func gcpEncodeJWT(signer crypto.Signer, keyID string, payload map[string]any) (string, error) {
	header := map[string]any{"typ": "JWT", "alg": "RS256"}
	if _, ok := signer.(*ecdsa.PrivateKey); ok {
		header["alg"] = "ES256"
	}
	if keyID != "" {
		header["kid"] = keyID
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(pb)
	digest := sha256.Sum256([]byte(signing))
	var sig []byte
	switch k := signer.(type) {
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		var r, s *big.Int
		r, s, err = ecdsa.Sign(rand.Reader, k, digest[:])
		if err == nil {
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	default:
		sig, err = signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	}
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Retryable token endpoint failures, by status code or OAuth error code
func gcpCanRetry(status int, data any) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	m, ok := data.(map[string]any)
	if !ok {
		return false
	}
	code, okc := m["error"].(string)
	desc, okd := m["error_description"].(string)
	if (m["error"] != nil && !okc) || (m["error_description"] != nil && !okd) {
		return false
	}
	for _, v := range []string{code, desc} {
		if v == "internal_failure" || v == "server_error" || v == "temporarily_unavailable" {
			return true
		}
	}
	return false
}

// Refresh error text from an OAuth error response, the raw response when it carries no error code
func gcpTokenErrorDetails(data any) string {
	if m, ok := data.(map[string]any); ok {
		if code, has := m["error"]; has {
			desc := m["error_description"]
			if desc == nil {
				return fmt.Sprintf("%v: None", code)
			}
			return fmt.Sprintf("%v: %v", code, desc)
		}
	}
	b, _ := json.Marshal(data)
	return string(b)
}

// Posts to a token endpoint, retrying retryable failures three times with jittered exponential backoff
func gcpTokenEndpointRequest(ctx context.Context, tokenURI string, form url.Values, jsonBody any, header http.Header, canRetry bool, client *http.Client) (map[string]any, error) {
	var payload []byte
	contentType := "application/x-www-form-urlencoded"
	if jsonBody != nil {
		b, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, err
		}
		payload, contentType = b, "application/json"
	} else {
		payload = []byte(form.Encode())
	}
	if client == nil {
		client = gcpHTTPClient()
	}
	wait := time.Second
	var data any
	var retryable bool
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			jitter := 1 + (mrand.Float64()*0.2 - 0.1)
			if err := gcpBackoffSleep(ctx, time.Duration(float64(wait)*jitter)); err != nil {
				return nil, err
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &gcpTransportError{Msg: err.Error()}
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var parsed any
		if json.Unmarshal(body, &parsed) != nil {
			parsed = string(body)
		}
		if resp.StatusCode == http.StatusOK {
			m, ok := parsed.(map[string]any)
			if !ok {
				return nil, &gcpRefreshError{Msg: gcpTokenErrorDetails(parsed), Data: parsed}
			}
			return m, nil
		}
		data, retryable = parsed, gcpCanRetry(resp.StatusCode, parsed)
		if !canRetry || !retryable {
			break
		}
	}
	return nil, &gcpRefreshError{Msg: gcpTokenErrorDetails(data), Data: data, Retryable: retryable}
}

// Expiry from expires_in, accepting the JSON string form some services send
func gcpParseExpiry(data map[string]any, now time.Time) (time.Time, error) {
	switch v := data["expires_in"].(type) {
	case nil:
		return time.Time{}, nil
	case float64:
		return now.Add(time.Duration(v * float64(time.Second))), nil
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(time.Duration(n) * time.Second), nil
	}
	return time.Time{}, fmt.Errorf("invalid expires_in %v", data["expires_in"])
}

// ---- service account ----

// Service account credentials from parsed JSON key info, a JWT grant at token_uri or a self signed JWT off the default universe
func gcpServiceAccountFromInfo(info map[string]any, scopes []string) (*gcpCredentials, error) {
	if missing := gcpMissingFields(info, "client_email", "token_uri"); len(missing) > 0 {
		return nil, fmt.Errorf("Service account info was not in the expected format, missing fields %s.", strings.Join(missing, ", "))
	}
	pk, ok := info["private_key"].(string)
	if !ok {
		return nil, errors.New("The private_key field was not found in the service account info.")
	}
	signer, err := gcpParsePrivateKey(pk)
	if err != nil {
		return nil, err
	}
	email, tokenURI, keyID := gcpStr(info, "client_email"), gcpStr(info, "token_uri"), gcpStr(info, "private_key_id")
	universe := gcpStr(info, "universe_domain")
	if universe == "" {
		universe = gcpDefaultUniverse
	}
	c := &gcpCredentials{kind: "service_account", projectID: gcpStr(info, "project_id"), quotaProjectID: gcpStr(info, "quota_project_id"), scopes: scopes}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		now := gcpNow().UTC()
		expiry := now.Add(gcpTokenLifetime)
		if universe != gcpDefaultUniverse {
			payload := map[string]any{"iss": email, "sub": email, "iat": now.Unix(), "exp": expiry.Unix()}
			if len(scopes) > 0 {
				payload["scope"] = gcpScopesString(scopes)
			}
			jwt, err := gcpEncodeJWT(signer, keyID, payload)
			return jwt, expiry, err
		}
		assertion, err := gcpEncodeJWT(signer, keyID, map[string]any{
			"iat": now.Unix(), "exp": expiry.Unix(), "iss": email, "aud": gcpOAuth2TokenEndpoint, "scope": gcpScopesString(scopes),
		})
		if err != nil {
			return "", time.Time{}, err
		}
		data, err := gcpTokenEndpointRequest(ctx, tokenURI, url.Values{"assertion": {assertion}, "grant_type": {gcpJWTGrantType}}, nil, nil, true, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		token, ok := data["access_token"].(string)
		if !ok {
			return "", time.Time{}, &gcpRefreshError{Msg: "No access token in response.", Data: data}
		}
		exp, err := gcpParseExpiry(data, gcpNow().UTC())
		return token, exp, err
	}
	return c, nil
}

// ---- authorized user ----

// End user credentials refreshed with the refresh token grant, verifying every requested scope was granted
func gcpAuthorizedUserFromInfo(info map[string]any, scopes []string) (*gcpCredentials, error) {
	if missing := gcpMissingFields(info, "refresh_token", "client_id", "client_secret"); len(missing) > 0 {
		return nil, fmt.Errorf("Authorized user info was not in the expected format, missing fields %s.", strings.Join(missing, ", "))
	}
	tokenURI := gcpStr(info, "token_uri")
	if tokenURI == "" {
		tokenURI = gcpOAuth2TokenEndpoint
	}
	clientID, clientSecret := gcpStr(info, "client_id"), gcpStr(info, "client_secret")
	refreshToken, rapt := gcpStr(info, "refresh_token"), gcpStr(info, "rapt_token")
	c := &gcpCredentials{kind: "authorized_user", quotaProjectID: gcpStr(info, "quota_project_id"), scopes: scopes}
	if clientID == gcpCloudSDKClientID && c.quotaProjectID == "" {
		slog.Default().Warn(gcpCloudSDKCredentialsWarning)
	}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		if refreshToken == "" || tokenURI == "" || clientID == "" || clientSecret == "" {
			return "", time.Time{}, &gcpRefreshError{Msg: "The credentials do not contain the necessary fields need to refresh the access token. " +
				"You must specify refresh_token, token_uri, client_id, and client_secret."}
		}
		form := url.Values{"grant_type": {gcpRefreshGrantType}, "client_id": {clientID}, "client_secret": {clientSecret}, "refresh_token": {refreshToken}}
		if len(scopes) > 0 {
			form.Set("scope", gcpScopesString(scopes))
		}
		if rapt != "" {
			form.Set("rapt", rapt)
		}
		data, err := gcpTokenEndpointRequest(ctx, tokenURI, form, nil, nil, true, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		token, ok := data["access_token"].(string)
		if !ok {
			return "", time.Time{}, &gcpRefreshError{Msg: "No access token in response.", Data: data}
		}
		if rt, ok := data["refresh_token"].(string); ok && rt != "" {
			refreshToken = rt
		}
		exp, err := gcpParseExpiry(data, gcpNow().UTC())
		if err != nil {
			return "", time.Time{}, err
		}
		if granted, ok := data["scope"].(string); ok && len(scopes) > 0 {
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
				return "", time.Time{}, &gcpRefreshError{Msg: fmt.Sprintf("Not all requested scopes were granted by the authorization server, missing scopes %s.", strings.Join(missing, ", "))}
			}
		}
		return token, exp, nil
	}
	return c, nil
}

// ---- GCE metadata server ----

func gcpMetadataRoot() string {
	host := os.Getenv("GCE_METADATA_HOST")
	if host == "" {
		host = os.Getenv("GCE_METADATA_ROOT")
	}
	if host == "" {
		host = "metadata.google.internal"
	}
	return "http://" + host + "/computeMetadata/v1/"
}

func gcpMetadataIPRoot() string {
	ip := os.Getenv("GCE_METADATA_IP")
	if ip == "" {
		ip = "169.254.169.254"
	}
	return "http://" + ip
}

func gcpMetadataClient() *http.Client {
	c := gcpHTTPClient()
	c.Timeout = 3 * time.Second
	return c
}

// True when the metadata server answers with the Google flavor header, three attempts with backoff
func gcpMetadataPing(ctx context.Context) bool {
	client := gcpMetadataClient()
	wait := time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if gcpBackoffSleep(ctx, wait) != nil {
				return false
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gcpMetadataIPRoot(), nil)
		if err != nil {
			return false
		}
		req.Header.Set("metadata-flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			slog.Default().Warn(fmt.Sprintf("Compute Engine Metadata server unavailable on attempt %d of 3. Reason: %v", attempt, err))
			continue
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK && resp.Header.Get("metadata-flavor") == "Google"
	}
	return false
}

// Linux hosts whose DMI product name marks a Google Compute Engine machine
func gcpDetectGCEResidencyLinux() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile(gcpProductNamePath)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(b)), "Google")
}

func gcpIsOnGCE(ctx context.Context) bool {
	if gcpMetadataPing(ctx) {
		return true
	}
	if runtime.GOOS == "windows" {
		return false
	}
	return gcpDetectGCEResidencyLinux()
}

// One metadata path, JSON decoded when the server says so, five attempts over retryable statuses and transport errors
func gcpMetadataGet(ctx context.Context, path string, params url.Values) (any, error) {
	target := gcpMetadataRoot() + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	client := gcpMetadataClient()
	wait := time.Second
	var failure string
	var resp *http.Response
	var body []byte
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			if err := gcpBackoffSleep(ctx, wait); err != nil {
				return nil, err
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("metadata-flavor", "Google")
		r, err := client.Do(req)
		if err != nil {
			slog.Default().Warn(fmt.Sprintf("Compute Engine Metadata server unavailable on attempt %d of 5. Reason: %v", attempt, err))
			failure = err.Error()
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		r.Body.Close()
		switch r.StatusCode {
		case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
			slog.Default().Warn(fmt.Sprintf("Compute Engine Metadata server unavailable on attempt %d of 5. Response status: %d", attempt, r.StatusCode))
			failure = string(b)
			continue
		}
		resp, body = r, b
		break
	}
	if resp == nil {
		return nil, &gcpTransportError{Msg: fmt.Sprintf("Failed to retrieve %s from the Google Compute Engine metadata service. Compute Engine Metadata server unavailable due to %s", target, failure)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &gcpTransportError{Msg: fmt.Sprintf("Failed to retrieve %s from the Google Compute Engine metadata service. Status: %d Response:\n%s", target, resp.StatusCode, body)}
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("content-type")); mt == "application/json" {
		var out any
		if err := json.Unmarshal(body, &out); err != nil {
			s := string(body)
			if len(s) > 20 {
				s = s[:20]
			}
			return nil, &gcpTransportError{Msg: "Received invalid JSON from the Google Compute Engine metadata service: " + s}
		}
		return out, nil
	}
	return string(body), nil
}

// Compute Engine default service account credentials minted by the metadata server
func gcpComputeEngineCredentials(scopes []string) *gcpCredentials {
	c := &gcpCredentials{kind: "compute_engine", scopes: scopes}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		info, err := gcpMetadataGet(ctx, "instance/service-accounts/default/", url.Values{"recursive": {"true"}})
		if err == nil {
			if m, ok := info.(map[string]any); ok && c.scopes == nil {
				if raw, ok := m["scopes"].([]any); ok {
					for _, s := range raw {
						if str, ok := s.(string); ok {
							c.scopes = append(c.scopes, str)
						}
					}
				}
			}
			params := url.Values{}
			if len(c.scopes) > 0 {
				params.Set("scopes", strings.Join(c.scopes, ","))
			}
			var tok any
			if tok, err = gcpMetadataGet(ctx, "instance/service-accounts/default/token", params); err == nil {
				m, _ := tok.(map[string]any)
				token, _ := m["access_token"].(string)
				secs, _ := m["expires_in"].(float64)
				return token, gcpNow().UTC().Add(time.Duration(secs * float64(time.Second))), nil
			}
		}
		return "", time.Time{}, &gcpRefreshError{Msg: err.Error()}
	}
	return c
}

// ---- Application Default Credentials ----

// gcloud configuration directory honoring CLOUDSDK_CONFIG and the Windows APPDATA layout
func gcpCloudSDKConfigPath() string {
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
	return filepath.Join(drive, `\`, "gcloud")
}

func gcpCloudSDKADCPath() string {
	return filepath.Join(gcpCloudSDKConfigPath(), "application_default_credentials.json")
}

// Active gcloud project from gcloud config get project, empty when gcloud is absent or fails
func gcpCloudSDKProjectID(ctx context.Context) string {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, gcpGcloudCommand(), "config", "get", "project")
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// Credentials and project from a credential JSON file of any supported type
func gcpLoadCredentialsFromFile(ctx context.Context, path string, scopes []string) (*gcpCredentials, string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, "", &gcpDefaultCredentialsError{Msg: fmt.Sprintf("File %s was not found.", path)}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", &gcpDefaultCredentialsError{Msg: fmt.Sprintf("File %s was not found.", path)}
	}
	var info map[string]any
	if json.Unmarshal(raw, &info) != nil || info == nil {
		return nil, "", &gcpDefaultCredentialsError{Msg: fmt.Sprintf("File %s is not a valid json file.", path)}
	}
	return gcpLoadCredentialsFromInfo(ctx, info, path, scopes)
}

// Dispatches credential info on its type field the way google.auth.load_credentials_from_dict does
func gcpLoadCredentialsFromInfo(ctx context.Context, info map[string]any, source string, scopes []string) (*gcpCredentials, string, error) {
	wrap := func(kind string, err error) error {
		return &gcpDefaultCredentialsError{Msg: fmt.Sprintf("Failed to load %s credentials from %s: %v", kind, source, err)}
	}
	switch typ := gcpStr(info, "type"); typ {
	case "authorized_user":
		c, err := gcpAuthorizedUserFromInfo(info, scopes)
		if err != nil {
			return nil, "", wrap("authorized user", err)
		}
		return c, "", nil
	case "service_account":
		c, err := gcpServiceAccountFromInfo(info, scopes)
		if err != nil {
			return nil, "", wrap("service account", err)
		}
		return c, c.projectID, nil
	case "external_account":
		c, err := gcpExternalAccountFromInfo(info, scopes)
		if err != nil {
			return nil, "", wrap("external account", err)
		}
		project, _ := c.projectLookup(ctx)
		return c, project, nil
	case "external_account_authorized_user":
		c, err := gcpExternalAuthorizedUserFromInfo(info, scopes)
		if err != nil {
			return nil, "", wrap("external account authorized user", err)
		}
		return c, "", nil
	case "impersonated_service_account":
		c, err := gcpImpersonatedFromInfo(ctx, info, source, scopes)
		if err != nil {
			return nil, "", wrap("impersonated service account", err)
		}
		return c, "", nil
	case "gdch_service_account":
		c, err := gcpGDCHFromInfo(info)
		if err != nil {
			return nil, "", wrap("GDCH service account", err)
		}
		return c, gcpStr(info, "project"), nil
	default:
		return nil, "", &gcpDefaultCredentialsError{Msg: fmt.Sprintf("The file %s does not have a valid type. Type is %s, expected one of ('%s').",
			source, typ, strings.Join(gcpValidCredentialTypes, "', '"))}
	}
}

// gcloud application default credentials file, falling back to the gcloud active project
func gcpCloudSDKCredentials(ctx context.Context, scopes []string) (*gcpCredentials, string, error) {
	path := gcpCloudSDKADCPath()
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, "", nil
	}
	c, project, err := gcpLoadCredentialsFromFile(ctx, path, scopes)
	if err != nil {
		return nil, "", err
	}
	if project == "" {
		project = gcpCloudSDKProjectID(ctx)
	}
	return c, project, nil
}

// GOOGLE_APPLICATION_CREDENTIALS, delegating to the gcloud flow when it names the gcloud ADC file
func gcpExplicitEnvironCredentials(ctx context.Context, scopes []string) (*gcpCredentials, string, error) {
	explicit, ok := os.LookupEnv("GOOGLE_APPLICATION_CREDENTIALS")
	if !ok {
		return nil, "", nil
	}
	if explicit == gcpCloudSDKADCPath() {
		return gcpCloudSDKCredentials(ctx, scopes)
	}
	return gcpLoadCredentialsFromFile(ctx, explicit, scopes)
}

// Compute Engine metadata credentials when the host is a GCE machine
func gcpGCECredentials(ctx context.Context, scopes []string) (*gcpCredentials, string, error) {
	if !gcpIsOnGCE(ctx) {
		slog.Default().Warn("Authentication failed using Compute Engine authentication due to unavailable metadata server.")
		return nil, "", nil
	}
	project := ""
	if v, err := gcpMetadataGet(ctx, "project/project-id", nil); err == nil {
		project, _ = v.(string)
	}
	return gcpComputeEngineCredentials(scopes), project, nil
}

// Application Default Credentials in the google.auth.default order with the GOOGLE_CLOUD_PROJECT override
func gcpDefaultCredentials(ctx context.Context, scopes []string) (*gcpCredentials, string, error) {
	explicitProject, ok := os.LookupEnv("GOOGLE_CLOUD_PROJECT")
	if !ok {
		explicitProject = os.Getenv("GCLOUD_PROJECT")
	}
	checkers := []func(context.Context, []string) (*gcpCredentials, string, error){
		gcpExplicitEnvironCredentials, gcpCloudSDKCredentials, gcpGCECredentials,
	}
	for _, check := range checkers {
		c, project, err := check(ctx, scopes)
		if err != nil {
			return nil, "", err
		}
		if c == nil {
			continue
		}
		effective := explicitProject
		if effective == "" {
			effective = project
		}
		if effective == "" && c.projectLookup != nil {
			effective, _ = c.projectLookup(ctx)
		}
		if effective == "" {
			slog.Default().Warn("No project ID could be determined. Consider running `gcloud config set project` or setting the GOOGLE_CLOUD_PROJECT environment variable")
		}
		return c, effective, nil
	}
	return nil, "", &gcpDefaultCredentialsError{Msg: gcpCloudSDKMissingCredentials}
}

// ---- impersonated service account ----

const gcpIAMGenerateAccessTokenURL = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken"

// Token for the target principal minted by IAM generateAccessToken under the source credential
func gcpImpersonatedCredentials(source *gcpCredentials, target string, delegates []string, scopes []string, lifetime time.Duration, endpoint string) *gcpCredentials {
	if endpoint == "" {
		endpoint = fmt.Sprintf(gcpIAMGenerateAccessTokenURL, target)
	}
	if lifetime <= 0 {
		lifetime = gcpTokenLifetime
	}
	c := &gcpCredentials{kind: "impersonated_service_account", scopes: scopes}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		if !source.Valid() {
			if err := source.Refresh(ctx); err != nil {
				return "", time.Time{}, err
			}
		}
		body := map[string]any{"delegates": delegates, "scope": scopes, "lifetime": strconv.Itoa(int(lifetime/time.Second)) + "s"}
		if delegates == nil {
			body["delegates"] = nil
		}
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := source.beforeRequest(ctx, req.Header); err != nil {
			return "", time.Time{}, err
		}
		resp, err := gcpHTTPClient().Do(req)
		if err != nil {
			return "", time.Time{}, &gcpTransportError{Msg: err.Error()}
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", time.Time{}, &gcpRefreshError{Msg: "Unable to acquire impersonated credentials", Data: string(raw)}
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", time.Time{}, &gcpRefreshError{Msg: "Unable to acquire impersonated credentials", Data: string(raw)}
		}
		token, _ := out["accessToken"].(string)
		exp, err := time.Parse("2006-01-02T15:04:05Z", gcpStr(out, "expireTime"))
		if err != nil {
			return "", time.Time{}, &gcpRefreshError{Msg: "Unable to acquire impersonated credentials", Data: string(raw)}
		}
		return token, exp, nil
	}
	return c
}

// impersonated_service_account file credentials over an authorized user, service account or external account source
func gcpImpersonatedFromInfo(ctx context.Context, info map[string]any, source string, scopes []string) (*gcpCredentials, error) {
	impURL := gcpStr(info, "service_account_impersonation_url")
	srcInfo, ok := info["source_credentials"].(map[string]any)
	if impURL == "" || !ok {
		return nil, errors.New("The provided entry is missing required fields: source_credentials, service_account_impersonation_url")
	}
	var src *gcpCredentials
	var err error
	switch typ := gcpStr(srcInfo, "type"); typ {
	case "authorized_user":
		src, err = gcpAuthorizedUserFromInfo(srcInfo, nil)
	case "service_account":
		src, err = gcpServiceAccountFromInfo(srcInfo, []string{gcpIAMScope})
	case "external_account":
		src, err = gcpExternalAccountFromInfo(srcInfo, []string{gcpCloudPlatformScope})
	case "external_account_authorized_user":
		src, err = gcpExternalAuthorizedUserFromInfo(srcInfo, nil)
	default:
		return nil, fmt.Errorf("source credential of type %s is not supported.", typ)
	}
	if err != nil {
		return nil, err
	}
	start := strings.LastIndex(impURL, "/")
	end := strings.Index(impURL, ":generateAccessToken")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("Cannot extract target principal from %s", impURL)
	}
	var delegates []string
	if raw, ok := info["delegates"].([]any); ok {
		for _, d := range raw {
			if s, ok := d.(string); ok {
				delegates = append(delegates, s)
			}
		}
	}
	c := gcpImpersonatedCredentials(src, impURL[start+1:end], delegates, scopes, gcpTokenLifetime, impURL)
	c.quotaProjectID = gcpStr(info, "quota_project_id")
	return c, nil
}

// ---- Google Distributed Cloud Hosted ----

// GDCH service account credentials exchanging an ES256 self signed JWT at token_uri
func gcpGDCHFromInfo(info map[string]any) (*gcpCredentials, error) {
	if v := gcpStr(info, "format_version"); v != "1" {
		return nil, errors.New("Only format version 1 is supported")
	}
	if missing := gcpMissingFields(info, "project", "private_key_id", "private_key", "name", "token_uri"); len(missing) > 0 {
		return nil, fmt.Errorf("Service account info was not in the expected format, missing fields %s.", strings.Join(missing, ", "))
	}
	signer, err := gcpParsePrivateKey(gcpStr(info, "private_key"))
	if err != nil {
		return nil, err
	}
	tokenURI, caPath := gcpStr(info, "token_uri"), gcpStr(info, "ca_cert_path")
	issuer := fmt.Sprintf("system:serviceaccount:%s:%s", gcpStr(info, "project"), gcpStr(info, "name"))
	keyID := gcpStr(info, "private_key_id")
	c := &gcpCredentials{kind: "gdch_service_account", projectID: gcpStr(info, "project")}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		now := gcpNow().UTC()
		jwt, err := gcpEncodeJWT(signer, keyID, map[string]any{"iss": issuer, "sub": issuer, "aud": tokenURI, "iat": now.Unix(), "exp": now.Add(gcpTokenLifetime).Unix()})
		if err != nil {
			return "", time.Time{}, err
		}
		client := gcpHTTPClient()
		if caPath != "" {
			pemCA, err := os.ReadFile(caPath)
			if err != nil {
				return "", time.Time{}, err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pemCA)
			client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		data, err := gcpTokenEndpointRequest(ctx, tokenURI, nil, map[string]any{
			"grant_type": "urn:ietf:params:oauth:token-type:token-exchange", "audience": nil,
			"requested_token_type": "urn:ietf:params:oauth:token-type:access_token", "subject_token": jwt,
			"subject_token_type": "urn:k8s:params:oauth:token-type:serviceaccount",
		}, nil, true, client)
		if err != nil {
			return "", time.Time{}, err
		}
		token, ok := data["access_token"].(string)
		if !ok {
			return "", time.Time{}, &gcpRefreshError{Msg: "No access token in response.", Data: data}
		}
		exp, err := gcpParseExpiry(data, gcpNow().UTC())
		return token, exp, err
	}
	return c, nil
}

// ---- external account (workload and workforce identity federation) ----

const (
	gcpSTSGrantType          = "urn:ietf:params:oauth:grant-type:token-exchange"
	gcpSTSRequestedTokenType = "urn:ietf:params:oauth:token-type:access_token"
	gcpAWSSubjectTokenType   = "urn:ietf:params:aws:token-type:aws4_request"
	gcpAWSDefaultVerifyURL   = "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"
)

// STS or OAuth error response failure
type gcpOAuthError struct{ Msg, Body string }

func (e *gcpOAuthError) Error() string { return e.Msg }

// Error text from an STS error body in the google-auth oauth2 utils format
func gcpSTSErrorDetails(body string) string {
	var data map[string]any
	if json.Unmarshal([]byte(body), &data) != nil {
		return body
	}
	code, ok := data["error"]
	if !ok {
		return body
	}
	out := fmt.Sprintf("Error code %v", code)
	if d, ok := data["error_description"]; ok {
		out += fmt.Sprintf(": %v", d)
	}
	if u, ok := data["error_uri"]; ok {
		out += fmt.Sprintf(" - %v", u)
	}
	return out
}

// Posts an STS form request with optional basic client authentication
func gcpSTSRequest(ctx context.Context, tokenURL string, form url.Values, clientID, clientSecret string, client *http.Client) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientID != "" && clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}
	if client == nil {
		client = gcpHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &gcpTransportError{Msg: err.Error()}
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &gcpOAuthError{Msg: gcpSTSErrorDetails(string(raw)), Body: string(raw)}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &gcpOAuthError{Msg: string(raw), Body: string(raw)}
	}
	return out, nil
}

// Project number from an STS audience of the form //iam.googleapis.com/projects/N/locations/...
func gcpAudienceProjectNumber(audience string) string {
	parts := strings.Split(audience, "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// Subject token reader for one external account credential source
type gcpSubjectTokenSupplier func(ctx context.Context) (string, error)

// External account credentials exchanging a federated subject token at STS, optionally impersonating a service account
func gcpExternalAccountFromInfo(info map[string]any, scopes []string) (*gcpCredentials, error) {
	audience, subjectType := gcpStr(info, "audience"), gcpStr(info, "subject_token_type")
	src, _ := info["credential_source"].(map[string]any)
	if audience == "" || subjectType == "" || src == nil {
		return nil, errors.New("Error code invalid_request: the external account info is missing audience, subject_token_type or credential_source")
	}
	universe := gcpStr(info, "universe_domain")
	if universe == "" {
		universe = gcpDefaultUniverse
	}
	tokenURL := gcpStr(info, "token_url")
	if tokenURL == "" {
		tokenURL = "https://sts." + universe + "/v1/token"
	}
	workforceUserProject := gcpStr(info, "workforce_pool_user_project")
	if workforceUserProject != "" && !gcpWorkforcePoolAudience.MatchString(audience) {
		return nil, errors.New("workforce_pool_user_project should not be set for non-workforce pool credentials.")
	}
	clientID, clientSecret := gcpStr(info, "client_id"), gcpStr(info, "client_secret")
	impURL := gcpStr(info, "service_account_impersonation_url")
	lifetime := gcpTokenLifetime
	if imp, ok := info["service_account_impersonation"].(map[string]any); ok {
		if secs, ok := imp["token_lifetime_seconds"].(float64); ok {
			lifetime = time.Duration(secs) * time.Second
		}
	}
	var supplier gcpSubjectTokenSupplier
	var stsClient *http.Client
	var err error
	_, executable := src["executable"]
	switch {
	case subjectType == gcpAWSSubjectTokenType:
		supplier, err = gcpAWSSubjectToken(src, audience)
	case executable:
		supplier, err = gcpExecutableSubjectToken(src, audience, subjectType, impURL)
	default:
		supplier, stsClient, err = gcpIdentityPoolSubjectToken(src)
		if err == nil && stsClient != nil && gcpStr(info, "token_url") == "" {
			tokenURL = "https://sts.mtls." + universe + "/v1/token"
		}
	}
	if err != nil {
		return nil, err
	}
	exchange := func(ctx context.Context, scopes []string) (string, time.Time, error) {
		now := gcpNow().UTC()
		subject, err := supplier(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		form := url.Values{"grant_type": {gcpSTSGrantType}, "audience": {audience}, "requested_token_type": {gcpSTSRequestedTokenType},
			"subject_token": {subject}, "subject_token_type": {subjectType}}
		if len(scopes) > 0 {
			form.Set("scope", gcpScopesString(scopes))
		}
		if workforceUserProject != "" && clientID == "" {
			opts, _ := json.Marshal(map[string]string{"userProject": workforceUserProject})
			form.Set("options", string(opts))
		}
		data, err := gcpSTSRequest(ctx, tokenURL, form, clientID, clientSecret, stsClient)
		if err != nil {
			return "", time.Time{}, err
		}
		token, _ := data["access_token"].(string)
		exp, err := gcpParseExpiry(data, now)
		return token, exp, err
	}
	c := &gcpCredentials{kind: "external_account", quotaProjectID: gcpStr(info, "quota_project_id"), scopes: scopes}
	if impURL != "" {
		start, end := strings.LastIndex(impURL, "/"), strings.Index(impURL, ":generateAccessToken")
		if start == -1 || end == -1 || end <= start {
			return nil, errors.New("Unable to determine target principal from service account impersonation URL.")
		}
		source := &gcpCredentials{kind: "external_account", scopes: []string{gcpCloudPlatformScope}}
		source.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, source.scopes) }
		imp := gcpImpersonatedCredentials(source, impURL[start+1:end], nil, scopes, lifetime, impURL)
		c.mint = imp.mint
	} else {
		c.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, c.scopes) }
	}
	projectNumber := gcpAudienceProjectNumber(audience)
	if projectNumber == "" {
		projectNumber = workforceUserProject
	}
	c.projectLookup = func(ctx context.Context) (string, error) {
		if c.projectID != "" {
			return c.projectID, nil
		}
		if projectNumber == "" || len(c.scopes) == 0 {
			return "", nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager."+universe+"/v1/projects/"+projectNumber, nil)
		if err != nil {
			return "", err
		}
		if err := c.beforeRequest(ctx, req.Header); err != nil {
			return "", err
		}
		resp, err := gcpHTTPClient().Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var out map[string]any
		if json.NewDecoder(resp.Body).Decode(&out) == nil && resp.StatusCode == http.StatusOK {
			c.projectID = gcpStr(out, "projectId")
		}
		return c.projectID, nil
	}
	return c, nil
}

// Subject token from a text or JSON payload as the credential source format declares it
func gcpParseSubjectToken(payload []byte, format map[string]any, where string) (string, error) {
	typ := gcpStr(format, "type")
	if typ == "" || typ == "text" {
		if len(payload) == 0 {
			return "", &gcpRefreshError{Msg: "Missing subject_token in the credential_source file"}
		}
		return string(payload), nil
	}
	field := gcpStr(format, "subject_token_field_name")
	var data map[string]any
	if json.Unmarshal(payload, &data) != nil {
		return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to parse subject_token from JSON file '%s' using key '%s'", where, field)}
	}
	token, ok := data[field].(string)
	if !ok {
		return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to parse subject_token from JSON file '%s' using key '%s'", where, field)}
	}
	if token == "" {
		return "", &gcpRefreshError{Msg: "Missing subject_token in the credential_source file"}
	}
	return token, nil
}

// Identity pool subject token from a file, a URL or an X.509 workload certificate with its mTLS STS client
func gcpIdentityPoolSubjectToken(src map[string]any) (gcpSubjectTokenSupplier, *http.Client, error) {
	if _, ok := src["environment_id"]; ok {
		return nil, nil, errors.New("Invalid Identity Pool credential_source field 'environment_id'")
	}
	file, urlStr := gcpStr(src, "file"), gcpStr(src, "url")
	cert, hasCert := src["certificate"].(map[string]any)
	count := 0
	for _, set := range []bool{file != "", urlStr != "", hasCert} {
		if set {
			count++
		}
	}
	if count > 1 {
		return nil, nil, errors.New("Ambiguous credential_source. 'file', 'url', and 'certificate' are mutually exclusive..")
	}
	if count == 0 {
		return nil, nil, errors.New("Missing credential_source. A 'file', 'url', or 'certificate' must be provided.")
	}
	format, _ := src["format"].(map[string]any)
	if typ := gcpStr(format, "type"); typ != "" && typ != "text" && typ != "json" {
		return nil, nil, fmt.Errorf("Invalid credential_source format '%s'", typ)
	}
	if gcpStr(format, "type") == "json" && gcpStr(format, "subject_token_field_name") == "" {
		return nil, nil, errors.New("Missing subject_token_field_name for JSON credential_source format")
	}
	switch {
	case file != "":
		return func(ctx context.Context) (string, error) {
			if _, err := os.Stat(file); err != nil {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("File '%s' was not found.", file)}
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return "", err
			}
			return gcpParseSubjectToken(raw, format, file)
		}, nil, nil
	case urlStr != "":
		headers, _ := src["headers"].(map[string]any)
		return func(ctx context.Context) (string, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
			if err != nil {
				return "", err
			}
			for k, v := range headers {
				req.Header.Set(k, fmt.Sprint(v))
			}
			resp, err := gcpHTTPClient().Do(req)
			if err != nil {
				return "", &gcpTransportError{Msg: err.Error()}
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return "", &gcpRefreshError{Msg: "Unable to retrieve Identity Pool subject token", Data: string(raw)}
			}
			return gcpParseSubjectToken(raw, format, urlStr)
		}, nil, nil
	}
	return gcpX509SubjectToken(cert)
}

// Workload certificate chain as the subject token, with an STS client presenting that certificate
func gcpX509SubjectToken(cert map[string]any) (gcpSubjectTokenSupplier, *http.Client, error) {
	useDefault, _ := cert["use_default_certificate_config"].(bool)
	location := gcpStr(cert, "certificate_config_location")
	if useDefault && location != "" {
		return nil, nil, errors.New("Invalid certificate configuration, certificate_config_location cannot be specified when use_default_certificate_config = true.")
	}
	if !useDefault && location == "" {
		return nil, nil, errors.New("Invalid certificate configuration, use_default_certificate_config should be true if no certificate_config_location is provided.")
	}
	if location == "" {
		location = os.Getenv("GOOGLE_API_CERTIFICATE_CONFIG")
	}
	if location == "" {
		location = filepath.Join(gcpCloudSDKConfigPath(), "certificate_config.json")
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		return nil, nil, fmt.Errorf("Certificate config file %s was not found.", location)
	}
	var conf struct {
		CertConfigs struct {
			Workload struct {
				CertPath string `json:"cert_path"`
				KeyPath  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil || conf.CertConfigs.Workload.CertPath == "" || conf.CertConfigs.Workload.KeyPath == "" {
		return nil, nil, fmt.Errorf("Certificate config file %s does not contain a workload cert_path and key_path.", location)
	}
	pair, err := tls.LoadX509KeyPair(conf.CertConfigs.Workload.CertPath, conf.CertConfigs.Workload.KeyPath)
	if err != nil {
		return nil, nil, err
	}
	chain := []string{base64.StdEncoding.EncodeToString(pair.Certificate[0])}
	if trust := gcpStr(cert, "trust_chain_path"); trust != "" {
		pemChain, err := os.ReadFile(trust)
		if err != nil {
			return nil, nil, fmt.Errorf("Trust chain file '%s' was not found.", trust)
		}
		for i := 0; ; i++ {
			var block *pem.Block
			block, pemChain = pem.Decode(pemChain)
			if block == nil {
				break
			}
			if bytes.Equal(block.Bytes, pair.Certificate[0]) {
				if i != 0 {
					return nil, nil, fmt.Errorf("The leaf certificate must be at the top of the trust chain file")
				}
				continue
			}
			chain = append(chain, base64.StdEncoding.EncodeToString(block.Bytes))
		}
	}
	client := gcpHTTPClient()
	client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	token, _ := json.Marshal(chain)
	return func(context.Context) (string, error) { return string(token), nil }, client, nil
}

// AWS GetCallerIdentity request signed with the environment or EC2 metadata credentials, serialized as the subject token
func gcpAWSSubjectToken(src map[string]any, audience string) (gcpSubjectTokenSupplier, error) {
	m := gcpAWSEnvironmentID.FindStringSubmatch(gcpStr(src, "environment_id"))
	if m == nil {
		return nil, errors.New("No valid AWS 'credential_source' provided")
	}
	if m[1] != "1" {
		return nil, fmt.Errorf("aws version '%s' is not supported in the current build.", m[1])
	}
	regionURL, credsURL, sessionURL := gcpStr(src, "region_url"), gcpStr(src, "url"), gcpStr(src, "imdsv2_session_token_url")
	verifyURL := gcpStr(src, "regional_cred_verification_url")
	if verifyURL == "" {
		verifyURL = gcpAWSDefaultVerifyURL
	}
	return func(ctx context.Context) (string, error) {
		client := gcpHTTPClient()
		region := os.Getenv("AWS_REGION")
		if region == "" {
			region = os.Getenv("AWS_DEFAULT_REGION")
		}
		envCreds := awsCredentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
		metaHeaders := http.Header{}
		if sessionURL != "" && (region == "" || !envCreds.valid()) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
			body, status, err := gcpDo(client, req)
			if err != nil || status != http.StatusOK {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to retrieve AWS Session Token: %s", body)}
			}
			metaHeaders.Set("X-aws-ec2-metadata-token", body)
		}
		get := func(target string, extra map[string]string) (string, int, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return "", 0, err
			}
			for k, vs := range metaHeaders {
				req.Header[k] = vs
			}
			for k, v := range extra {
				req.Header.Set(k, v)
			}
			return gcpDo(client, req)
		}
		if region == "" {
			if regionURL == "" {
				return "", &gcpRefreshError{Msg: "Unable to determine AWS region"}
			}
			body, status, err := get(regionURL, nil)
			if err != nil || status != http.StatusOK {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to retrieve AWS region: %s", body)}
			}
			if body != "" {
				region = body[:len(body)-1]
			}
		}
		creds := envCreds
		if !creds.valid() {
			if credsURL == "" {
				return "", &gcpRefreshError{Msg: "Unable to determine the AWS metadata server security credentials endpoint"}
			}
			role, status, err := get(credsURL, nil)
			if err != nil || status != http.StatusOK {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to retrieve AWS role name %s", role)}
			}
			body, status, err := get(credsURL+"/"+role, map[string]string{"Content-Type": "application/json"})
			if err != nil || status != http.StatusOK {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to retrieve AWS security credentials %s", body)}
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("Unable to retrieve AWS security credentials %s", body)}
			}
			creds = awsCredentials{AccessKeyID: gcpStr(out, "AccessKeyId"), SecretAccessKey: gcpStr(out, "SecretAccessKey"), SessionToken: gcpStr(out, "Token")}
		}
		target := strings.ReplaceAll(verifyURL, "{region}", region)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("x-goog-cloud-target-resource", audience)
		if _, err := sigV4Sign(req, nil, creds, "sts", region, gcpNow()); err != nil {
			return "", err
		}
		headers := []map[string]string{{"key": "Authorization", "value": req.Header.Get("Authorization")},
			{"key": "host", "value": req.URL.Host}, {"key": "x-amz-date", "value": req.Header.Get("X-Amz-Date")}}
		if creds.SessionToken != "" {
			headers = append(headers, map[string]string{"key": "x-amz-security-token", "value": creds.SessionToken})
		}
		headers = append(headers, map[string]string{"key": "x-goog-cloud-target-resource", "value": audience})
		signed, _ := json.Marshal(map[string]any{"headers": headers, "method": "POST", "url": target})
		return url.QueryEscape(string(signed)), nil
	}, nil
}

func gcpDo(client *http.Client, req *http.Request) (string, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return err.Error(), 0, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	return string(raw), resp.StatusCode, nil
}

// Pluggable auth subject token from an allowed executable, reusing an unexpired cached output file
func gcpExecutableSubjectToken(src map[string]any, audience, subjectType, impURL string) (gcpSubjectTokenSupplier, error) {
	exe, _ := src["executable"].(map[string]any)
	command := gcpStr(exe, "command")
	if command == "" {
		return nil, errors.New("Missing command field. Executable command must be provided.")
	}
	timeout := 30 * time.Second
	if ms, ok := exe["timeout_millis"].(float64); ok {
		if ms < 5000 || ms > 120000 {
			return nil, errors.New("Timeout must be between 5 and 120 seconds.")
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	outputFile := gcpStr(exe, "output_file")
	return func(ctx context.Context) (string, error) {
		if os.Getenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES") != "1" {
			return "", errors.New("Pluggable Auth executables need to be explicitly allowed to run by setting the " +
				"GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES environment Variable to 1.")
		}
		if outputFile != "" {
			if raw, err := os.ReadFile(outputFile); err == nil {
				var cached map[string]any
				if json.Unmarshal(raw, &cached) == nil {
					token, err := gcpParseExecutableResponse(cached)
					var refresh *gcpRefreshError
					switch {
					case err == nil && cached["expiration_time"] != nil:
						return token, nil
					case err != nil && !errors.As(err, &refresh):
						return "", err
					}
				}
			}
		}
		argv, err := awsPOSIXSplit(command)
		if err != nil || len(argv) == 0 {
			return "", fmt.Errorf("Missing command field. Executable command must be provided.")
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		env := append(os.Environ(), "GOOGLE_EXTERNAL_ACCOUNT_AUDIENCE="+audience, "GOOGLE_EXTERNAL_ACCOUNT_TOKEN_TYPE="+subjectType,
			"GOOGLE_EXTERNAL_ACCOUNT_INTERACTIVE=0")
		if impURL != "" {
			start, end := strings.LastIndex(impURL, "/"), strings.Index(impURL, ":generateAccessToken")
			if start != -1 && end > start {
				env = append(env, "GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL="+impURL[start+1:end])
			}
		}
		if outputFile != "" {
			env = append(env, "GOOGLE_EXTERNAL_ACCOUNT_OUTPUT_FILE="+outputFile)
		}
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && runCtx.Err() == nil {
				return "", &gcpRefreshError{Msg: fmt.Sprintf("Executable exited with non-zero return code %d. Error: %s", exitErr.ExitCode(), out.String())}
			}
			return "", err
		}
		var response map[string]any
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			return "", fmt.Errorf("Unable to parse the executable response: %s", out.String())
		}
		token, err := gcpParseExecutableResponse(response)
		if err != nil {
			return "", err
		}
		if outputFile != "" && response["expiration_time"] == nil {
			return "", errors.New("The executable response must contain an expiration_time for successful responses when an output_file has been specified in the configuration.")
		}
		return token, nil
	}, nil
}

// Subject token from a version 1 executable response, an error for failures, expiry and unknown token types
func gcpParseExecutableResponse(resp map[string]any) (string, error) {
	version, ok := resp["version"].(float64)
	if !ok {
		return "", errors.New("The executable response is missing the version field.")
	}
	if version > 1 {
		return "", &gcpRefreshError{Msg: fmt.Sprintf("Executable returned unsupported version %v.", version)}
	}
	success, ok := resp["success"].(bool)
	if !ok {
		return "", errors.New("The executable response is missing the success field.")
	}
	if !success {
		code, okc := resp["code"]
		msg, okm := resp["message"]
		if !okc || !okm {
			return "", errors.New("Error code and message fields are required in the response.")
		}
		return "", &gcpRefreshError{Msg: fmt.Sprintf("Executable returned unsuccessful response: code: %v, message: %v.", code, msg)}
	}
	if exp, ok := resp["expiration_time"].(float64); ok && exp < float64(gcpNow().Unix()) {
		return "", &gcpRefreshError{Msg: "The token returned by the executable is expired."}
	}
	typ, ok := resp["token_type"].(string)
	if !ok {
		return "", errors.New("The executable response is missing the token_type field.")
	}
	switch typ {
	case "urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token":
		return gcpStr(resp, "id_token"), nil
	case "urn:ietf:params:oauth:token-type:saml2":
		return gcpStr(resp, "saml_response"), nil
	}
	return "", &gcpRefreshError{Msg: "Executable returned unsupported token type."}
}

// Workforce identity end user credentials refreshed at token_url with basic client authentication
func gcpExternalAuthorizedUserFromInfo(info map[string]any, scopes []string) (*gcpCredentials, error) {
	if missing := gcpMissingFields(info, "audience", "refresh_token", "token_url", "client_id", "client_secret"); len(missing) == 5 {
		return nil, errors.New("Token should be created with fields to make it valid (`token` and `expiry`), or fields to allow it to refresh (`refresh_token`, `token_url`, `client_id`, `client_secret`).")
	}
	refreshToken, tokenURL := gcpStr(info, "refresh_token"), gcpStr(info, "token_url")
	clientID, clientSecret := gcpStr(info, "client_id"), gcpStr(info, "client_secret")
	c := &gcpCredentials{kind: "external_account_authorized_user", quotaProjectID: gcpStr(info, "quota_project_id"), scopes: scopes}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		if refreshToken == "" || tokenURL == "" || clientID == "" || clientSecret == "" {
			return "", time.Time{}, &gcpRefreshError{Msg: "The credentials do not contain the necessary fields need to refresh the access token. " +
				"You must specify refresh_token, token_url, client_id, and client_secret."}
		}
		now := gcpNow().UTC()
		data, err := gcpSTSRequest(ctx, tokenURL, url.Values{"grant_type": {gcpRefreshGrantType}, "refresh_token": {refreshToken}}, clientID, clientSecret, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		if rt, ok := data["refresh_token"].(string); ok {
			refreshToken = rt
		}
		token, _ := data["access_token"].(string)
		exp, err := gcpParseExpiry(data, now)
		return token, exp, err
	}
	return c, nil
}

var (
	gcpWorkforcePoolAudience = regexp.MustCompile(`^//iam\.googleapis\.com/locations/[^/]+/workforcePools/`)
	gcpAWSEnvironmentID      = regexp.MustCompile(`^aws(\d+)$`)
)

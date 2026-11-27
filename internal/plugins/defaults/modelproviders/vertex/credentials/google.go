package credentials

import (
	"bytes"
	"cmp"
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
	mrand "math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/credentials/aws"
	"github.com/nickheyer/icebath/internal/terminal"
)

const (
	tokenEndpoint   = "https://oauth2.googleapis.com/token"
	defaultUniverse = "googleapis.com"
	jwtGrant        = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	tokenLifetime   = time.Hour
	// A token this close to its expiry is minted again
	refreshThreshold = 3*time.Minute + 45*time.Second
	// Client of gcloud's own sign-in, whose credentials carry no quota project unless one is set
	cloudSDKClient = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	iamScope       = "https://www.googleapis.com/auth/iam"
	cloudPlatform  = "https://www.googleapis.com/auth/cloud-platform"
	stsGrant       = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessType     = "urn:ietf:params:oauth:token-type:access_token"
	awsSubjectType = "urn:ietf:params:aws:token-type:aws4_request"
	awsVerify      = "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"
	generateToken  = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken"
	missingADC     = "Your default credentials were not found. To set up Application Default Credentials, see " +
		"https://cloud.google.com/docs/authentication/external/set-up-adc for more information."
	quotaWarning = "Your application has authenticated using end user credentials from Google Cloud SDK without a quota project. You might " +
		"receive a \"quota exceeded\" or \"API not enabled\" error. See the following page for troubleshooting: " +
		"https://cloud.google.com/docs/authentication/adc-troubleshooting/user-creds. "
)

var (
	types             = []string{"authorized_user", "service_account", "external_account", "external_account_authorized_user", "impersonated_service_account", "gdch_service_account"}
	workforceAudience = regexp.MustCompile(`^//iam\.googleapis\.com/locations/[^/]+/workforcePools/`)
	awsEnvironment    = regexp.MustCompile(`^aws(\d+)$`)
)

// Variables Google's credentials read, and the programs they run are given: those of the profile they are loaded for
type environment struct {
	lookup func(name string) (string, bool)
	all    func() map[string]string
}

func (e environment) get(name string) string {
	value, _ := e.lookup(name)
	return value
}

// The variables as a program is given them, each NAME=value, with the extra ones after
func (e environment) list(extra ...string) []string {
	var out []string
	for name, value := range e.all() {
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return append(out, extra...)
}

// One Google credential: what mints its tokens, the token it last minted, and its project
type google struct {
	kind    string
	project string
	quota   string
	scopes  []string
	mu      sync.Mutex
	token   string
	expiry  time.Time
	mint    func(ctx context.Context) (string, time.Time, error)
	lookup  func(ctx context.Context) (string, error)
}

// Whether the token is within the refresh threshold of its expiry
func (g *google) expired() bool {
	return !g.expiry.IsZero() && !time.Now().Before(g.expiry.Add(-refreshThreshold))
}

// Token minted now, and kept with its expiry
func (g *google) refresh(ctx context.Context) error {
	token, expiry, err := g.mint(ctx)
	if err != nil {
		return err
	}
	g.token, g.expiry = token, expiry
	return nil
}

// Bearer and quota project of a request made with the credential, minted first when the token isn't valid
func (g *google) authorize(ctx context.Context, header http.Header) error {
	if g.token == "" || g.expired() {
		if err := g.refresh(ctx); err != nil {
			return err
		}
	}
	header.Set("Authorization", "Bearer "+g.token)
	if g.quota != "" {
		header.Set("x-goog-user-project", g.quota)
	}
	return nil
}

func text(info map[string]any, key string) string {
	v, _ := info[key].(string)
	return v
}

func missing(info map[string]any, keys ...string) []string {
	var out []string
	for _, k := range keys {
		if _, ok := info[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// Refused token request, with what the endpoint answered
type refusal struct {
	message   string
	retryable bool
}

func (e *refusal) Error() string { return e.message }

func signerOf(pemText string) (crypto.Signer, error) {
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

// Compact JWS of the claims, RS256 or ES256 by the key, with its key id
func jwt(signer crypto.Signer, keyID string, claims map[string]any) (string, error) {
	header := map[string]any{"typ": "JWT", "alg": "RS256"}
	if _, ok := signer.(*ecdsa.PrivateKey); ok {
		header["alg"] = "ES256"
	}
	if keyID != "" {
		header["kid"] = keyID
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	digest := sha256.Sum256([]byte(signing))
	var signature []byte
	switch key := signer.(type) {
	case *rsa.PrivateKey:
		signature, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		r, s, e := ecdsa.Sign(rand.Reader, key, digest[:])
		err = e
		if err == nil {
			signature = make([]byte, 64)
			r.FillBytes(signature[:32])
			s.FillBytes(signature[32:])
		}
	default:
		signature, err = signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	}
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

var client = &http.Client{Timeout: 120 * time.Second}

// Posts to a token endpoint a form, or JSON when body is given, three times over a refusal that may pass with a backoff that grows
func post(ctx context.Context, endpoint string, form url.Values, body map[string]any, with *http.Client) (map[string]any, error) {
	payload, kind := []byte(form.Encode()), "application/x-www-form-urlencoded"
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload, kind = encoded, "application/json"
	}
	if with == nil {
		with = client
	}
	wait := time.Second
	var last any
	retryable := false
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			jitter := 1 + mrand.Float64()*0.2 - 0.1
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(float64(wait) * jitter)):
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", kind)
		resp, err := with.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			parsed = string(raw)
		}
		if resp.StatusCode == http.StatusOK {
			if m, ok := parsed.(map[string]any); ok {
				return m, nil
			}
			return nil, &refusal{message: details(parsed)}
		}
		last, retryable = parsed, canRetry(resp.StatusCode, parsed)
		if !retryable {
			break
		}
	}
	return nil, &refusal{message: details(last), retryable: retryable}
}

func canRetry(status int, data any) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	m, _ := data.(map[string]any)
	for _, v := range []string{text(m, "error"), text(m, "error_description")} {
		if v == "internal_failure" || v == "server_error" || v == "temporarily_unavailable" {
			return true
		}
	}
	return false
}

// Text of an OAuth error answer, the answer itself when it names no error
func details(data any) string {
	if m, ok := data.(map[string]any); ok {
		if code, ok := m["error"]; ok {
			if desc, ok := m["error_description"]; ok && desc != nil {
				return fmt.Sprintf("%v: %v", code, desc)
			}
			return fmt.Sprintf("%v: None", code)
		}
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

// Expiry an answer's expires_in names, given as a number or as text
func expiryOf(data map[string]any, now time.Time) (time.Time, error) {
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

// Access token of an answer with its expiry
func minted(data map[string]any) (string, time.Time, error) {
	token, ok := data["access_token"].(string)
	if !ok {
		return "", time.Time{}, &refusal{message: "No access token in response."}
	}
	expiry, err := expiryOf(data, time.Now().UTC())
	return token, expiry, err
}

// Service account key: a JWT grant at its token_uri, or a self-signed JWT outside Google's default universe
func serviceAccount(info map[string]any, scopes []string) (*google, error) {
	if m := missing(info, "client_email", "token_uri"); len(m) > 0 {
		return nil, fmt.Errorf("Service account info was not in the expected format, missing fields %s.", strings.Join(m, ", "))
	}
	pk, ok := info["private_key"].(string)
	if !ok {
		return nil, errors.New("The private_key field was not found in the service account info.")
	}
	signer, err := signerOf(pk)
	if err != nil {
		return nil, err
	}
	email, tokenURI, keyID := text(info, "client_email"), text(info, "token_uri"), text(info, "private_key_id")
	universe := cmp.Or(text(info, "universe_domain"), defaultUniverse)
	g := &google{kind: "service_account", project: text(info, "project_id"), quota: text(info, "quota_project_id"), scopes: scopes}
	g.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		expiry := now.Add(tokenLifetime)
		if universe != defaultUniverse {
			claims := map[string]any{"iss": email, "sub": email, "iat": now.Unix(), "exp": expiry.Unix()}
			if len(scopes) > 0 {
				claims["scope"] = strings.Join(scopes, " ")
			}
			token, err := jwt(signer, keyID, claims)
			return token, expiry, err
		}
		assertion, err := jwt(signer, keyID, map[string]any{"iat": now.Unix(), "exp": expiry.Unix(), "iss": email, "aud": tokenEndpoint,
			"scope": strings.Join(scopes, " ")})
		if err != nil {
			return "", time.Time{}, err
		}
		data, err := post(ctx, tokenURI, url.Values{"assertion": {assertion}, "grant_type": {jwtGrant}}, nil, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		return minted(data)
	}
	return g, nil
}

// End user credentials renewed with their refresh token, every scope asked for required to be granted
func authorizedUser(info map[string]any, scopes []string) (*google, error) {
	if m := missing(info, "refresh_token", "client_id", "client_secret"); len(m) > 0 {
		return nil, fmt.Errorf("Authorized user info was not in the expected format, missing fields %s.", strings.Join(m, ", "))
	}
	tokenURI := cmp.Or(text(info, "token_uri"), tokenEndpoint)
	id, secret, refresh, rapt := text(info, "client_id"), text(info, "client_secret"), text(info, "refresh_token"), text(info, "rapt_token")
	g := &google{kind: "authorized_user", quota: text(info, "quota_project_id"), scopes: scopes}
	if id == cloudSDKClient && g.quota == "" {
		slog.Warn(quotaWarning)
	}
	g.mint = func(ctx context.Context) (string, time.Time, error) {
		if refresh == "" || tokenURI == "" || id == "" || secret == "" {
			return "", time.Time{}, &refusal{message: "The credentials do not contain the necessary fields need to refresh the access token. " +
				"You must specify refresh_token, token_uri, client_id, and client_secret."}
		}
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "client_secret": {secret}, "refresh_token": {refresh}}
		if len(scopes) > 0 {
			form.Set("scope", strings.Join(scopes, " "))
		}
		if rapt != "" {
			form.Set("rapt", rapt)
		}
		data, err := post(ctx, tokenURI, form, nil, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		if rotated := text(data, "refresh_token"); rotated != "" {
			refresh = rotated
		}
		token, expiry, err := minted(data)
		if err != nil {
			return "", time.Time{}, err
		}
		if granted, ok := data["scope"].(string); ok && len(scopes) > 0 {
			have := map[string]bool{}
			for _, s := range strings.Fields(granted) {
				have[s] = true
			}
			var lacking []string
			for _, s := range scopes {
				if !have[s] {
					lacking = append(lacking, s)
				}
			}
			if len(lacking) > 0 {
				return "", time.Time{}, &refusal{message: fmt.Sprintf("Not all requested scopes were granted by the authorization server, missing scopes %s.", strings.Join(lacking, ", "))}
			}
		}
		return token, expiry, nil
	}
	return g, nil
}

// ---- metadata server ----

func metadataRoot(e environment) string {
	return "http://" + cmp.Or(e.get("GCE_METADATA_HOST"), e.get("GCE_METADATA_ROOT"), "metadata.google.internal") + "/computeMetadata/v1/"
}

var metadataClient = &http.Client{Timeout: 3 * time.Second}

// Whether this runs on Compute Engine: its metadata server answers as Google's, or its DMI name says so
func onGCE(ctx context.Context, e environment) bool {
	wait := time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(wait):
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cmp.Or(e.get("GCE_METADATA_IP"), "169.254.169.254"), nil)
		if err != nil {
			return false
		}
		req.Header.Set("metadata-flavor", "Google")
		resp, err := metadataClient.Do(req)
		if err != nil {
			slog.Warn(fmt.Sprintf("Compute Engine Metadata server unavailable on attempt %d of 3. Reason: %v", attempt, err))
			continue
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK && resp.Header.Get("metadata-flavor") == "Google"
	}
	if runtime.GOOS != "linux" {
		return false
	}
	name, err := os.ReadFile("/sys/class/dmi/id/product_name")
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(name)), "Google")
}

// One metadata path, JSON when the server says so, five tries over answers that may pass
func metadata(ctx context.Context, e environment, path string, params url.Values) (any, error) {
	target := metadataRoot(e) + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	wait := time.Second
	failure := ""
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("metadata-flavor", "Google")
		resp, err := metadataClient.Do(req)
		if err != nil {
			failure = err.Error()
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusTooManyRequests:
			failure = string(body)
			continue
		case http.StatusOK:
		default:
			return nil, fmt.Errorf("Failed to retrieve %s from the Google Compute Engine metadata service. Status: %d Response:\n%s", target, resp.StatusCode, body)
		}
		if kind, _, _ := mime.ParseMediaType(resp.Header.Get("content-type")); kind == "application/json" {
			var out any
			if err := json.Unmarshal(body, &out); err != nil {
				return nil, fmt.Errorf("Received invalid JSON from the Google Compute Engine metadata service: %.20s", body)
			}
			return out, nil
		}
		return string(body), nil
	}
	return nil, fmt.Errorf("Failed to retrieve %s from the Google Compute Engine metadata service. Compute Engine Metadata server unavailable due to %s", target, failure)
}

// Default service account of the Compute Engine machine, its tokens minted by the metadata server
func computeEngine(e environment, scopes []string) *google {
	g := &google{kind: "compute_engine", scopes: scopes}
	g.mint = func(ctx context.Context) (string, time.Time, error) {
		info, err := metadata(ctx, e, "instance/service-accounts/default/", url.Values{"recursive": {"true"}})
		if err != nil {
			return "", time.Time{}, err
		}
		if m, ok := info.(map[string]any); ok && g.scopes == nil {
			for _, s := range anySlice(m["scopes"]) {
				if str, ok := s.(string); ok {
					g.scopes = append(g.scopes, str)
				}
			}
		}
		params := url.Values{}
		if len(g.scopes) > 0 {
			params.Set("scopes", strings.Join(g.scopes, ","))
		}
		answer, err := metadata(ctx, e, "instance/service-accounts/default/token", params)
		if err != nil {
			return "", time.Time{}, err
		}
		m, _ := answer.(map[string]any)
		seconds, _ := m["expires_in"].(float64)
		return text(m, "access_token"), time.Now().UTC().Add(time.Duration(seconds * float64(time.Second))), nil
	}
	return g
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// ---- Application Default Credentials ----

// gcloud's configuration folder, CLOUDSDK_CONFIG when set
func cloudSDK(e environment) string {
	if v, ok := e.lookup("CLOUDSDK_CONFIG"); ok {
		return v
	}
	if runtime.GOOS != "windows" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "gcloud")
	}
	if v, ok := e.lookup("APPDATA"); ok {
		return filepath.Join(v, "gcloud")
	}
	return filepath.Join(cmp.Or(e.get("SystemDrive"), "C:"), `\`, "gcloud")
}

func adcFile(e environment) string {
	return filepath.Join(cloudSDK(e), "application_default_credentials.json")
}

// Project gcloud is set to, empty when gcloud isn't here or says none
func gcloudProject(ctx context.Context, e environment) string {
	name := "gcloud"
	if runtime.GOOS == "windows" {
		name = "gcloud.cmd"
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, "config", "get", "project")
	cmd.Env, cmd.Stdout = e.list(), &out
	if cmd.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// Credentials of a JSON file of any of Google's credential types, with the project it names
func fromFile(ctx context.Context, e environment, path string, scopes []string) (*google, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("File %s was not found.", path)
	}
	var info map[string]any
	if json.Unmarshal(raw, &info) != nil || info == nil {
		return nil, "", fmt.Errorf("File %s is not a valid json file.", path)
	}
	return fromInfo(ctx, e, info, path, scopes)
}

func fromInfo(ctx context.Context, e environment, info map[string]any, source string, scopes []string) (*google, string, error) {
	wrap := func(kind string, err error) error {
		return fmt.Errorf("Failed to load %s credentials from %s: %v", kind, source, err)
	}
	switch kind := text(info, "type"); kind {
	case "authorized_user":
		g, err := authorizedUser(info, scopes)
		if err != nil {
			return nil, "", wrap("authorized user", err)
		}
		return g, "", nil
	case "service_account":
		g, err := serviceAccount(info, scopes)
		if err != nil {
			return nil, "", wrap("service account", err)
		}
		return g, g.project, nil
	case "external_account":
		g, err := externalAccount(e, info, scopes)
		if err != nil {
			return nil, "", wrap("external account", err)
		}
		project, _ := g.lookup(ctx)
		return g, project, nil
	case "external_account_authorized_user":
		g, err := externalUser(info, scopes)
		if err != nil {
			return nil, "", wrap("external account authorized user", err)
		}
		return g, "", nil
	case "impersonated_service_account":
		g, err := impersonatedFrom(e, info, scopes)
		if err != nil {
			return nil, "", wrap("impersonated service account", err)
		}
		return g, "", nil
	case "gdch_service_account":
		g, err := gdch(info)
		if err != nil {
			return nil, "", wrap("GDCH service account", err)
		}
		return g, text(info, "project"), nil
	default:
		return nil, "", fmt.Errorf("The file %s does not have a valid type. Type is %s, expected one of ('%s').", source, kind, strings.Join(types, "', '"))
	}
}

// Application Default Credentials in Google's order: GOOGLE_APPLICATION_CREDENTIALS, gcloud's file, then the Compute Engine machine;
// GOOGLE_CLOUD_PROJECT naming the project over the one they carry
func adc(ctx context.Context, e environment, scopes []string) (*google, string, error) {
	explicit, ok := e.lookup("GOOGLE_CLOUD_PROJECT")
	if !ok {
		explicit = e.get("GCLOUD_PROJECT")
	}
	checks := []func() (*google, string, error){
		func() (*google, string, error) {
			path, ok := e.lookup("GOOGLE_APPLICATION_CREDENTIALS")
			if !ok {
				return nil, "", nil
			}
			if path != adcFile(e) {
				return fromFile(ctx, e, path, scopes)
			}
			return sdk(ctx, e, scopes)
		},
		func() (*google, string, error) { return sdk(ctx, e, scopes) },
		func() (*google, string, error) {
			if !onGCE(ctx, e) {
				slog.Warn("Authentication failed using Compute Engine authentication due to unavailable metadata server.")
				return nil, "", nil
			}
			project := ""
			if v, err := metadata(ctx, e, "project/project-id", nil); err == nil {
				project, _ = v.(string)
			}
			return computeEngine(e, scopes), project, nil
		},
	}
	for _, check := range checks {
		g, project, err := check()
		if err != nil {
			return nil, "", err
		}
		if g == nil {
			continue
		}
		project = cmp.Or(explicit, project)
		if project == "" && g.lookup != nil {
			project, _ = g.lookup(ctx)
		}
		if project == "" {
			slog.Warn("No project ID could be determined. Consider running `gcloud config set project` or setting the GOOGLE_CLOUD_PROJECT environment variable")
		}
		return g, project, nil
	}
	return nil, "", errors.New(missingADC)
}

// gcloud's application default credentials, its project from gcloud when the file names none
func sdk(ctx context.Context, e environment, scopes []string) (*google, string, error) {
	path := adcFile(e)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, "", nil
	}
	g, project, err := fromFile(ctx, e, path, scopes)
	if err != nil {
		return nil, "", err
	}
	return g, cmp.Or(project, gcloudProject(ctx, e)), nil
}

// ---- impersonation ----

// Tokens of the target service account, minted by IAM's generateAccessToken under the source credential
func impersonated(source *google, target string, delegates, scopes []string, lifetime time.Duration, endpoint string) *google {
	endpoint = cmp.Or(endpoint, fmt.Sprintf(generateToken, target))
	if lifetime <= 0 {
		lifetime = tokenLifetime
	}
	g := &google{kind: "impersonated_service_account", scopes: scopes}
	g.mint = func(ctx context.Context) (string, time.Time, error) {
		body := map[string]any{"delegates": delegates, "scope": scopes, "lifetime": strconv.Itoa(int(lifetime/time.Second)) + "s"}
		encoded, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if err := source.authorize(ctx, req.Header); err != nil {
			return "", time.Time{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", time.Time{}, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var out map[string]any
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
			return "", time.Time{}, &refusal{message: "Unable to acquire impersonated credentials: " + string(raw)}
		}
		expiry, err := time.Parse("2006-01-02T15:04:05Z", text(out, "expireTime"))
		if err != nil {
			return "", time.Time{}, &refusal{message: "Unable to acquire impersonated credentials: " + string(raw)}
		}
		return text(out, "accessToken"), expiry, nil
	}
	return g
}

// Service account the impersonation URL names, between its last slash and :generateAccessToken
func principal(endpoint string) (string, bool) {
	start, end := strings.LastIndex(endpoint, "/"), strings.Index(endpoint, ":generateAccessToken")
	if start == -1 || end == -1 || end <= start {
		return "", false
	}
	return endpoint[start+1 : end], true
}

func impersonatedFrom(e environment, info map[string]any, scopes []string) (*google, error) {
	endpoint := text(info, "service_account_impersonation_url")
	source, ok := info["source_credentials"].(map[string]any)
	if endpoint == "" || !ok {
		return nil, errors.New("The provided entry is missing required fields: source_credentials, service_account_impersonation_url")
	}
	var base *google
	var err error
	switch kind := text(source, "type"); kind {
	case "authorized_user":
		base, err = authorizedUser(source, nil)
	case "service_account":
		base, err = serviceAccount(source, []string{iamScope})
	case "external_account":
		base, err = externalAccount(e, source, []string{cloudPlatform})
	case "external_account_authorized_user":
		base, err = externalUser(source, nil)
	default:
		return nil, fmt.Errorf("source credential of type %s is not supported.", kind)
	}
	if err != nil {
		return nil, err
	}
	target, ok := principal(endpoint)
	if !ok {
		return nil, fmt.Errorf("Cannot extract target principal from %s", endpoint)
	}
	var delegates []string
	for _, d := range anySlice(info["delegates"]) {
		if s, ok := d.(string); ok {
			delegates = append(delegates, s)
		}
	}
	g := impersonated(base, target, delegates, scopes, tokenLifetime, endpoint)
	g.quota = text(info, "quota_project_id")
	return g, nil
}

// Google Distributed Cloud Hosted service account, an ES256 self-signed JWT exchanged at its token_uri
func gdch(info map[string]any) (*google, error) {
	if text(info, "format_version") != "1" {
		return nil, errors.New("Only format version 1 is supported")
	}
	if m := missing(info, "project", "private_key_id", "private_key", "name", "token_uri"); len(m) > 0 {
		return nil, fmt.Errorf("Service account info was not in the expected format, missing fields %s.", strings.Join(m, ", "))
	}
	signer, err := signerOf(text(info, "private_key"))
	if err != nil {
		return nil, err
	}
	tokenURI, ca := text(info, "token_uri"), text(info, "ca_cert_path")
	issuer := fmt.Sprintf("system:serviceaccount:%s:%s", text(info, "project"), text(info, "name"))
	g := &google{kind: "gdch_service_account", project: text(info, "project")}
	g.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		assertion, err := jwt(signer, text(info, "private_key_id"), map[string]any{"iss": issuer, "sub": issuer, "aud": tokenURI, "iat": now.Unix(),
			"exp": now.Add(tokenLifetime).Unix()})
		if err != nil {
			return "", time.Time{}, err
		}
		with := client
		if ca != "" {
			pemCA, err := os.ReadFile(ca)
			if err != nil {
				return "", time.Time{}, err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pemCA)
			with = &http.Client{Timeout: client.Timeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
		}
		data, err := post(ctx, tokenURI, nil, map[string]any{"grant_type": stsGrant, "audience": nil, "requested_token_type": accessType,
			"subject_token": assertion, "subject_token_type": "urn:k8s:params:oauth:token-type:serviceaccount"}, with)
		if err != nil {
			return "", time.Time{}, err
		}
		return minted(data)
	}
	return g, nil
}

// ---- workload and workforce identity federation ----

// Error text of an STS answer
func stsDetails(body string) string {
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

// Posts an STS form, with the client's basic authentication when it has a secret
func stsPost(ctx context.Context, endpoint string, form url.Values, id, secret string, with *http.Client) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if id != "" && secret != "" {
		req.SetBasicAuth(id, secret)
	}
	if with == nil {
		with = client
	}
	resp, err := with.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	var out map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
		return nil, &refusal{message: stsDetails(string(raw))}
	}
	return out, nil
}

// Federated credential: a subject token from a file, an address, a certificate, AWS or a program, exchanged at STS and optionally used to
// impersonate a service account
func externalAccount(e environment, info map[string]any, scopes []string) (*google, error) {
	audience, subjectType := text(info, "audience"), text(info, "subject_token_type")
	source, _ := info["credential_source"].(map[string]any)
	if audience == "" || subjectType == "" || source == nil {
		return nil, errors.New("Error code invalid_request: the external account info is missing audience, subject_token_type or credential_source")
	}
	universe := cmp.Or(text(info, "universe_domain"), defaultUniverse)
	tokenURL := cmp.Or(text(info, "token_url"), "https://sts."+universe+"/v1/token")
	workforceProject := text(info, "workforce_pool_user_project")
	if workforceProject != "" && !workforceAudience.MatchString(audience) {
		return nil, errors.New("workforce_pool_user_project should not be set for non-workforce pool credentials.")
	}
	id, secret := text(info, "client_id"), text(info, "client_secret")
	impersonation := text(info, "service_account_impersonation_url")
	lifetime := tokenLifetime
	if imp, ok := info["service_account_impersonation"].(map[string]any); ok {
		if seconds, ok := imp["token_lifetime_seconds"].(float64); ok {
			lifetime = time.Duration(seconds) * time.Second
		}
	}
	var subject func(ctx context.Context) (string, error)
	var sts *http.Client
	var err error
	_, executable := source["executable"]
	switch {
	case subjectType == awsSubjectType:
		subject, err = awsSubject(e, source, audience)
	case executable:
		subject, err = programSubject(e, source, audience, subjectType, impersonation)
	default:
		subject, sts, err = poolSubject(e, source)
		if err == nil && sts != nil && text(info, "token_url") == "" {
			tokenURL = "https://sts.mtls." + universe + "/v1/token"
		}
	}
	if err != nil {
		return nil, err
	}
	exchange := func(ctx context.Context, scopes []string) (string, time.Time, error) {
		now := time.Now().UTC()
		token, err := subject(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		form := url.Values{"grant_type": {stsGrant}, "audience": {audience}, "requested_token_type": {accessType}, "subject_token": {token},
			"subject_token_type": {subjectType}}
		if len(scopes) > 0 {
			form.Set("scope", strings.Join(scopes, " "))
		}
		if workforceProject != "" && id == "" {
			options, _ := json.Marshal(map[string]string{"userProject": workforceProject})
			form.Set("options", string(options))
		}
		data, err := stsPost(ctx, tokenURL, form, id, secret, sts)
		if err != nil {
			return "", time.Time{}, err
		}
		expiry, err := expiryOf(data, now)
		return text(data, "access_token"), expiry, err
	}
	g := &google{kind: "external_account", quota: text(info, "quota_project_id"), scopes: scopes}
	if impersonation != "" {
		target, ok := principal(impersonation)
		if !ok {
			return nil, errors.New("Unable to determine target principal from service account impersonation URL.")
		}
		base := &google{kind: "external_account", scopes: []string{cloudPlatform}}
		base.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, base.scopes) }
		g.mint = impersonated(base, target, nil, scopes, lifetime, impersonation).mint
	} else {
		g.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, g.scopes) }
	}
	number := projectNumber(audience)
	if number == "" {
		number = workforceProject
	}
	g.lookup = func(ctx context.Context) (string, error) {
		if g.project != "" || number == "" || len(g.scopes) == 0 {
			return g.project, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager."+universe+"/v1/projects/"+number, nil)
		if err != nil {
			return "", err
		}
		if err := g.authorize(ctx, req.Header); err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var out map[string]any
		if json.NewDecoder(resp.Body).Decode(&out) == nil && resp.StatusCode == http.StatusOK {
			g.project = text(out, "projectId")
		}
		return g.project, nil
	}
	return g, nil
}

// Project number of an audience //iam.googleapis.com/projects/N/locations/...
func projectNumber(audience string) string {
	parts := strings.Split(audience, "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// Subject token of a text or JSON payload as the source's format says
func subjectOf(payload []byte, format map[string]any, where string) (string, error) {
	if kind := text(format, "type"); kind == "" || kind == "text" {
		if len(payload) == 0 {
			return "", &refusal{message: "Missing subject_token in the credential_source file"}
		}
		return string(payload), nil
	}
	field := text(format, "subject_token_field_name")
	var data map[string]any
	token := ""
	if json.Unmarshal(payload, &data) == nil {
		token, _ = data[field].(string)
	}
	if _, ok := data[field].(string); !ok {
		return "", &refusal{message: fmt.Sprintf("Unable to parse subject_token from JSON file '%s' using key '%s'", where, field)}
	}
	if token == "" {
		return "", &refusal{message: "Missing subject_token in the credential_source file"}
	}
	return token, nil
}

// Identity pool subject token of a file, an address or an X.509 workload certificate, the last with its mTLS STS client
func poolSubject(e environment, source map[string]any) (func(context.Context) (string, error), *http.Client, error) {
	if _, ok := source["environment_id"]; ok {
		return nil, nil, errors.New("Invalid Identity Pool credential_source field 'environment_id'")
	}
	path, address := text(source, "file"), text(source, "url")
	certificate, hasCertificate := source["certificate"].(map[string]any)
	count := 0
	for _, set := range []bool{path != "", address != "", hasCertificate} {
		if set {
			count++
		}
	}
	switch {
	case count > 1:
		return nil, nil, errors.New("Ambiguous credential_source. 'file', 'url', and 'certificate' are mutually exclusive..")
	case count == 0:
		return nil, nil, errors.New("Missing credential_source. A 'file', 'url', or 'certificate' must be provided.")
	}
	format, _ := source["format"].(map[string]any)
	if kind := text(format, "type"); kind != "" && kind != "text" && kind != "json" {
		return nil, nil, fmt.Errorf("Invalid credential_source format '%s'", kind)
	}
	if text(format, "type") == "json" && text(format, "subject_token_field_name") == "" {
		return nil, nil, errors.New("Missing subject_token_field_name for JSON credential_source format")
	}
	switch {
	case path != "":
		return func(context.Context) (string, error) {
			raw, err := os.ReadFile(path)
			if err != nil {
				return "", &refusal{message: fmt.Sprintf("File '%s' was not found.", path)}
			}
			return subjectOf(raw, format, path)
		}, nil, nil
	case address != "":
		headers, _ := source["headers"].(map[string]any)
		return func(ctx context.Context) (string, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
			if err != nil {
				return "", err
			}
			for k, v := range headers {
				req.Header.Set(k, fmt.Sprint(v))
			}
			resp, err := client.Do(req)
			if err != nil {
				return "", err
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return "", &refusal{message: "Unable to retrieve Identity Pool subject token: " + string(raw)}
			}
			return subjectOf(raw, format, address)
		}, nil, nil
	}
	return certificateSubject(e, certificate)
}

// Workload certificate chain as the subject token, with an STS client presenting the certificate
func certificateSubject(e environment, certificate map[string]any) (func(context.Context) (string, error), *http.Client, error) {
	useDefault, _ := certificate["use_default_certificate_config"].(bool)
	location := text(certificate, "certificate_config_location")
	switch {
	case useDefault && location != "":
		return nil, nil, errors.New("Invalid certificate configuration, certificate_config_location cannot be specified when use_default_certificate_config = true.")
	case !useDefault && location == "":
		return nil, nil, errors.New("Invalid certificate configuration, use_default_certificate_config should be true if no certificate_config_location is provided.")
	}
	location = cmp.Or(location, e.get("GOOGLE_API_CERTIFICATE_CONFIG"), filepath.Join(cloudSDK(e), "certificate_config.json"))
	raw, err := os.ReadFile(location)
	if err != nil {
		return nil, nil, fmt.Errorf("Certificate config file %s was not found.", location)
	}
	var config struct {
		Configs struct {
			Workload struct {
				Cert string `json:"cert_path"`
				Key  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}
	if err := json.Unmarshal(raw, &config); err != nil || config.Configs.Workload.Cert == "" || config.Configs.Workload.Key == "" {
		return nil, nil, fmt.Errorf("Certificate config file %s does not contain a workload cert_path and key_path.", location)
	}
	pair, err := tls.LoadX509KeyPair(config.Configs.Workload.Cert, config.Configs.Workload.Key)
	if err != nil {
		return nil, nil, err
	}
	chain := []string{base64.StdEncoding.EncodeToString(pair.Certificate[0])}
	if trust := text(certificate, "trust_chain_path"); trust != "" {
		pemChain, err := os.ReadFile(trust)
		if err != nil {
			return nil, nil, fmt.Errorf("Trust chain file '%s' was not found.", trust)
		}
		for i := 0; ; i++ {
			var block *pem.Block
			if block, pemChain = pem.Decode(pemChain); block == nil {
				break
			}
			if bytes.Equal(block.Bytes, pair.Certificate[0]) {
				if i != 0 {
					return nil, nil, errors.New("The leaf certificate must be at the top of the trust chain file")
				}
				continue
			}
			chain = append(chain, base64.StdEncoding.EncodeToString(block.Bytes))
		}
	}
	token, _ := json.Marshal(chain)
	mtls := &http.Client{Timeout: client.Timeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}}
	return func(context.Context) (string, error) { return string(token), nil }, mtls, nil
}

// AWS GetCallerIdentity request signed with the environment's or the EC2 instance's keys, serialized as the subject token
func awsSubject(e environment, source map[string]any, audience string) (func(context.Context) (string, error), error) {
	m := awsEnvironment.FindStringSubmatch(text(source, "environment_id"))
	if m == nil {
		return nil, errors.New("No valid AWS 'credential_source' provided")
	}
	if m[1] != "1" {
		return nil, fmt.Errorf("aws version '%s' is not supported in the current build.", m[1])
	}
	regionURL, keysURL, sessionURL := text(source, "region_url"), text(source, "url"), text(source, "imdsv2_session_token_url")
	verify := cmp.Or(text(source, "regional_cred_verification_url"), awsVerify)
	return func(ctx context.Context) (string, error) {
		region := cmp.Or(e.get("AWS_REGION"), e.get("AWS_DEFAULT_REGION"))
		keys := aws.Keys{AccessKeyID: e.get("AWS_ACCESS_KEY_ID"), SecretAccessKey: e.get("AWS_SECRET_ACCESS_KEY"), SessionToken: e.get("AWS_SESSION_TOKEN")}
		header := http.Header{}
		if sessionURL != "" && (region == "" || !keys.Valid()) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
			body, status := do(req)
			if status != http.StatusOK {
				return "", &refusal{message: "Unable to retrieve AWS Session Token: " + body}
			}
			header.Set("X-aws-ec2-metadata-token", body)
		}
		get := func(target string, extra map[string]string) (string, int) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err.Error(), 0
			}
			for k, vs := range header {
				req.Header[k] = vs
			}
			for k, v := range extra {
				req.Header.Set(k, v)
			}
			return do(req)
		}
		if region == "" {
			if regionURL == "" {
				return "", &refusal{message: "Unable to determine AWS region"}
			}
			body, status := get(regionURL, nil)
			if status != http.StatusOK {
				return "", &refusal{message: "Unable to retrieve AWS region: " + body}
			}
			if body != "" {
				region = body[:len(body)-1]
			}
		}
		if !keys.Valid() {
			if keysURL == "" {
				return "", &refusal{message: "Unable to determine the AWS metadata server security credentials endpoint"}
			}
			role, status := get(keysURL, nil)
			if status != http.StatusOK {
				return "", &refusal{message: "Unable to retrieve AWS role name " + role}
			}
			body, status := get(keysURL+"/"+role, map[string]string{"Content-Type": "application/json"})
			var out map[string]any
			if status != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
				return "", &refusal{message: "Unable to retrieve AWS security credentials " + body}
			}
			keys = aws.Keys{AccessKeyID: text(out, "AccessKeyId"), SecretAccessKey: text(out, "SecretAccessKey"), SessionToken: text(out, "Token")}
		}
		target := strings.ReplaceAll(verify, "{region}", region)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("x-goog-cloud-target-resource", audience)
		if err := aws.Sign(req, nil, keys, "sts", region, time.Now()); err != nil {
			return "", err
		}
		headers := []map[string]string{{"key": "Authorization", "value": req.Header.Get("Authorization")}, {"key": "host", "value": req.URL.Host},
			{"key": "x-amz-date", "value": req.Header.Get("X-Amz-Date")}}
		if keys.SessionToken != "" {
			headers = append(headers, map[string]string{"key": "x-amz-security-token", "value": keys.SessionToken})
		}
		headers = append(headers, map[string]string{"key": "x-goog-cloud-target-resource", "value": audience})
		signed, _ := json.Marshal(map[string]any{"headers": headers, "method": "POST", "url": target})
		return url.QueryEscape(string(signed)), nil
	}, nil
}

func do(req *http.Request) (string, int) {
	resp, err := client.Do(req)
	if err != nil {
		return err.Error(), 0
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	return string(raw), resp.StatusCode
}

// Subject token of an allowed program, its cached output file reused while unexpired
func programSubject(e environment, source map[string]any, audience, subjectType, impersonation string) (func(context.Context) (string, error), error) {
	program, _ := source["executable"].(map[string]any)
	command := text(program, "command")
	if command == "" {
		return nil, errors.New("Missing command field. Executable command must be provided.")
	}
	timeout := 30 * time.Second
	if ms, ok := program["timeout_millis"].(float64); ok {
		if ms < 5000 || ms > 120000 {
			return nil, errors.New("Timeout must be between 5 and 120 seconds.")
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	output := text(program, "output_file")
	return func(ctx context.Context) (string, error) {
		if e.get("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES") != "1" {
			return "", errors.New("Pluggable Auth executables need to be explicitly allowed to run by setting the GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES " +
				"environment Variable to 1.")
		}
		if output != "" {
			if raw, err := os.ReadFile(output); err == nil {
				var cached map[string]any
				if json.Unmarshal(raw, &cached) == nil {
					token, err := programToken(cached)
					_, refused := errors.AsType[*refusal](err)
					switch {
					case err == nil && cached["expiration_time"] != nil:
						return token, nil
					case err != nil && !refused:
						return "", err
					}
				}
			}
		}
		argv, err := terminal.Split(command)
		if err != nil || len(argv) == 0 {
			return "", errors.New("Missing command field. Executable command must be provided.")
		}
		run, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(run, argv[0], argv[1:]...)
		env := e.list("GOOGLE_EXTERNAL_ACCOUNT_AUDIENCE="+audience, "GOOGLE_EXTERNAL_ACCOUNT_TOKEN_TYPE="+subjectType,
			"GOOGLE_EXTERNAL_ACCOUNT_INTERACTIVE=0")
		if target, ok := principal(impersonation); ok {
			env = append(env, "GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL="+target)
		}
		if output != "" {
			env = append(env, "GOOGLE_EXTERNAL_ACCOUNT_OUTPUT_FILE="+output)
		}
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			if exit, ok := errors.AsType[*exec.ExitError](err); ok && run.Err() == nil {
				return "", &refusal{message: fmt.Sprintf("Executable exited with non-zero return code %d. Error: %s", exit.ExitCode(), out.String())}
			}
			return "", err
		}
		var response map[string]any
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			return "", fmt.Errorf("Unable to parse the executable response: %s", out.String())
		}
		token, err := programToken(response)
		if err != nil {
			return "", err
		}
		if output != "" && response["expiration_time"] == nil {
			return "", errors.New("The executable response must contain an expiration_time for successful responses when an output_file has been specified in the configuration.")
		}
		return token, nil
	}, nil
}

// Subject token of a version 1 program answer
func programToken(response map[string]any) (string, error) {
	version, ok := response["version"].(float64)
	if !ok {
		return "", errors.New("The executable response is missing the version field.")
	}
	if version > 1 {
		return "", &refusal{message: fmt.Sprintf("Executable returned unsupported version %v.", version)}
	}
	success, ok := response["success"].(bool)
	if !ok {
		return "", errors.New("The executable response is missing the success field.")
	}
	if !success {
		code, hasCode := response["code"]
		message, hasMessage := response["message"]
		if !hasCode || !hasMessage {
			return "", errors.New("Error code and message fields are required in the response.")
		}
		return "", &refusal{message: fmt.Sprintf("Executable returned unsuccessful response: code: %v, message: %v.", code, message)}
	}
	if expires, ok := response["expiration_time"].(float64); ok && expires < float64(time.Now().Unix()) {
		return "", &refusal{message: "The token returned by the executable is expired."}
	}
	kind, ok := response["token_type"].(string)
	if !ok {
		return "", errors.New("The executable response is missing the token_type field.")
	}
	switch kind {
	case "urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token":
		return text(response, "id_token"), nil
	case "urn:ietf:params:oauth:token-type:saml2":
		return text(response, "saml_response"), nil
	}
	return "", &refusal{message: "Executable returned unsupported token type."}
}

// Workforce identity end user renewed at its token_url with basic client authentication
func externalUser(info map[string]any, scopes []string) (*google, error) {
	if len(missing(info, "audience", "refresh_token", "token_url", "client_id", "client_secret")) == 5 {
		return nil, errors.New("Token should be created with fields to make it valid (`token` and `expiry`), or fields to allow it to refresh " +
			"(`refresh_token`, `token_url`, `client_id`, `client_secret`).")
	}
	refresh, tokenURL, id, secret := text(info, "refresh_token"), text(info, "token_url"), text(info, "client_id"), text(info, "client_secret")
	g := &google{kind: "external_account_authorized_user", quota: text(info, "quota_project_id"), scopes: scopes}
	g.mint = func(ctx context.Context) (string, time.Time, error) {
		if refresh == "" || tokenURL == "" || id == "" || secret == "" {
			return "", time.Time{}, &refusal{message: "The credentials do not contain the necessary fields need to refresh the access token. " +
				"You must specify refresh_token, token_url, client_id, and client_secret."}
		}
		now := time.Now().UTC()
		data, err := stsPost(ctx, tokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, id, secret, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		if rotated, ok := data["refresh_token"].(string); ok {
			refresh = rotated
		}
		expiry, err := expiryOf(data, now)
		return text(data, "access_token"), expiry, err
	}
	return g, nil
}

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Versions of the pinned azure-identity and msal whose heads the Entra token requests carry
const (
	azureIdentityVersion = "1.26.0"
	msalVersion          = "1.39.0"
)

// MSAL api id of acquire_token_for_client and the refresh reasons its current telemetry header names
const (
	msalAcquireTokenForClientID = 730
	msalAtAbsent                = 2
	msalAtExpired               = 3
	msalAtAging                 = 4
)

// Hosts MSAL knows without instance discovery
var msalWellKnownAuthorityHosts = map[string]bool{"login.microsoftonline.com": true, "login.microsoft.com": true, "login.windows.net": true,
	"sts.windows.net": true, "login.chinacloudapi.cn": true, "login.partner.microsoftonline.cn": true, "login.microsoftonline.de": true,
	"login-us.microsoftonline.com": true, "login.microsoftonline.us": true, "login.usgovcloudapi.net": true, "login.sovcloud-identity.fr": true,
	"login.sovcloud-identity.de": true, "login.sovcloud-identity.sg": true}

// Hosts MSAL treats as B2C authorities known to the developer
var msalWellKnownB2CHosts = []string{"b2clogin.com", "b2clogin.cn", "b2clogin.us", "b2clogin.de", "ciamlogin.com"}

// User-Agent of the azure-identity pipeline, AZURE_HTTP_USER_AGENT appended when set
func azureIdentityUserAgent() string {
	ua := "azsdk-python-identity/" + azureIdentityVersion + " Python/" + pinnedPythonRuntimeVersion() + " (" + pyplatform.Platform() + ")"
	if extra, ok := os.LookupEnv("AZURE_HTTP_USER_AGENT"); ok {
		ua += " " + extra
	}
	return ua
}

// Python sys.platform of the host as MSAL reports it in x-client-os
func msalClientOS() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin", "linux", "android", "ios":
		return runtime.GOOS
	case "aix":
		return "aix"
	}
	major, _, _ := strings.Cut(pyplatform.Release(), ".")
	return runtime.GOOS + major
}

// Authority azure-identity normalizes AZURE_AUTHORITY_HOST into, https required
func azureIdentityAuthority() (string, error) {
	authority, ok := os.LookupEnv("AZURE_AUTHORITY_HOST")
	if !ok {
		authority = "login.microsoftonline.com"
	}
	parsed, err := pyurl.Parse(authority)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" {
		return "https://" + strings.TrimRight(authority, " /"), nil
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("'%s' is an invalid authority. The value must be a TLS protected (https) URL.", authority)
	}
	return strings.TrimRight(authority, " /"), nil
}

// azure-identity pipeline over the requests transport with its request id, user agent and retry policies
type azureIdentityPipeline struct{ client *http.Client }

// Transport under the azure-identity pipeline, the requests wire transport when nil
var azureIdentityTransport http.RoundTripper

func newAzureIdentityPipeline() *azureIdentityPipeline {
	tr := azureIdentityTransport
	if tr == nil {
		tr = pyStackWireTransport(pyhttpx.RequestsProxy, 300*time.Second, 10)
	}
	return &azureIdentityPipeline{client: &http.Client{Transport: pyhttpx.WireTransport{Base: tr, Decoders: requestsDecoders}, CheckRedirect: httpxNoRedirects}}
}

// Statuses RetryPolicy treats as safe, every other status under 999 retried
func azureRetryableStatus(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return status >= 506 && status < 999
}

// Runs one request through RequestIdPolicy, UserAgentPolicy and RetryPolicy, the form body urlencoded as requests sends it
func (p *azureIdentityPipeline) run(ctx context.Context, method, target string, headers []pyhttpx.HeaderLine, form [][2]string, retryPOST bool) (int, []byte, error) {
	requestID, err := uuid.NewUUID()
	if err != nil {
		return 0, nil, err
	}
	call := append(append([]pyhttpx.HeaderLine(nil), headers...), pyhttpx.HeaderLine{Name: "x-ms-client-request-id", Value: requestID.String()},
		pyhttpx.HeaderLine{Name: "User-Agent", Value: azureIdentityUserAgent()})
	body := pyurl.URLEncode(form)
	total, connects, reads, statuses := 10, 3, 3, 3
	history := 0
	for {
		var reader io.Reader
		if form != nil {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return 0, nil, err
		}
		resp, err := p.client.Do(pyhttpx.WithWire(req, requestsLines(call...)))
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, err
			}
			var op *net.OpError
			if errors.As(err, &op) && op.Op == "dial" {
				connects--
			} else if method == http.MethodPost && !retryPOST {
				return 0, nil, err
			} else {
				reads--
			}
			total, history = total-1, history+1
			if total < 0 || connects < 0 || reads < 0 {
				return 0, nil, err
			}
			if err := azureRetrySleep(ctx, history, 0); err != nil {
				return 0, nil, err
			}
			continue
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return 0, nil, rerr
		}
		retryAfter, hasRetryAfter := azureRetryAfter(resp.Header.Get("Retry-After"))
		retry := false
		if resp.StatusCode >= 400 {
			methodOK := method != http.MethodPost && method != http.MethodPatch || retryPOST || resp.StatusCode == 500 || resp.StatusCode == 503 || resp.StatusCode == 504
			retry = resp.Header.Get("Retry-After") != "" || methodOK && total > 0 && azureRetryableStatus(resp.StatusCode)
		}
		if !retry {
			return resp.StatusCode, raw, nil
		}
		total, statuses, history = total-1, statuses-1, history+1
		if total < 0 || statuses < 0 {
			return resp.StatusCode, raw, nil
		}
		wait := time.Duration(0)
		if hasRetryAfter {
			wait = retryAfter
		}
		if err := azureRetrySleep(ctx, history, wait); err != nil {
			return 0, nil, err
		}
	}
}

// Seconds or HTTP date of a Retry-After value
func azureRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(math.Max(f, 0) * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// Retry-After wait when given, else the exponential backoff of 0.8 seconds doubled per earlier error, capped at 120
func azureRetrySleep(ctx context.Context, history int, retryAfter time.Duration) error {
	wait := retryAfter
	if wait <= 0 && history > 1 {
		wait = time.Duration(math.Min(120, 0.8*math.Pow(2, float64(history-1))) * float64(time.Second))
	}
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// MSAL telemetry buffer of one app, cache hits and failed requests since the last successful token response
type msalTelemetry struct {
	succeeded   int
	failures    [][3]string
	failureSize int
}

// client-request-id and the current and last telemetry headers of one token request
func (t *msalTelemetry) headers(api, reason int, correlation string) []pyhttpx.HeaderLine {
	var reqs, errs []string
	for _, f := range t.failures {
		reqs = append(reqs, f[0]+","+f[1])
		errs = append(errs, f[2])
	}
	return []pyhttpx.HeaderLine{
		{Name: "client-request-id", Value: correlation},
		{Name: "x-client-current-telemetry", Value: fmt.Sprintf("4|%d,%d|", api, reason)},
		{Name: "x-client-last-telemetry", Value: fmt.Sprintf("4|%d|%s|%s|", t.succeeded, strings.Join(reqs, ","), strings.Join(errs, ","))},
	}
}

// Keeps a failed response while the last header stays under 350 characters, a success clearing the buffer
func (t *msalTelemetry) update(api int, correlation string, result map[string]any) {
	if len(result) == 0 {
		return
	}
	e, failed := result["error"]
	if !failed {
		*t = msalTelemetry{}
		return
	}
	text := fmt.Sprint(e)
	size := utf8.RuneCountInString(fmt.Sprintf(",%d,%s,%s", api, correlation, text))
	if t.failureSize+size < 350 {
		t.failureSize += size
		t.failures = append(t.failures, [3]string{strconv.Itoa(api), correlation, text})
	}
}

// Access token MSAL caches for one scope set, refresh_on zero when the response gave no refresh_in
type msalCachedToken struct {
	token                string
	expiresOn, refreshOn int64
}

// MSAL ConfidentialClientApplication of one azure-identity credential
type msalConfidentialApp struct {
	mu       sync.Mutex
	clientID string
	// Tenant authority URL the app was built for
	authority string
	// Form fields after client_id and grant_type, client_info then the secret or the signed assertion for the token URL
	credential    func(ctx context.Context, tokenURL string) ([][2]string, error)
	pipeline      *azureIdentityPipeline
	tokenEndpoint string
	telemetry     msalTelemetry
	tokens        map[string]msalCachedToken
}

func newMSALConfidentialApp(tenant, clientID string, credential func(ctx context.Context, tokenURL string) ([][2]string, error)) (*msalConfidentialApp, error) {
	authority, err := azureIdentityAuthority()
	if err != nil {
		return nil, err
	}
	return &msalConfidentialApp{clientID: clientID, authority: authority + "/" + tenant, credential: credential, pipeline: newAzureIdentityPipeline(), tokens: map[string]msalCachedToken{}}, nil
}

// Token endpoint from tenant discovery, preceded by instance discovery for an authority host MSAL does not know
func (a *msalConfidentialApp) discover(ctx context.Context) error {
	if a.tokenEndpoint != "" {
		return nil
	}
	u, err := url.Parse(a.authority)
	if err != nil {
		return err
	}
	instance := strings.ToLower(u.Host)
	parts := strings.Split(u.Path, "/")
	tenant := ""
	if len(parts) > 1 {
		tenant = parts[1]
	}
	knownToDeveloper := strings.EqualFold(tenant, "adfs") && !strings.HasSuffix(instance, ".ciamlogin.com") || len(parts) == 3 && strings.HasPrefix(strings.ToLower(parts[2]), "b2c_")
	for _, d := range msalWellKnownB2CHosts {
		knownToDeveloper = knownToDeveloper || strings.HasSuffix(instance, "."+d)
	}
	version := "/v2.0"
	if strings.EqualFold(tenant, "adfs") && !strings.HasSuffix(instance, ".ciamlogin.com") {
		version = ""
	}
	discovery := *u
	discovery.Path = u.Path + version + "/.well-known/openid-configuration"
	endpoint := discovery.String()
	if !msalWellKnownAuthorityHosts[instance] && !knownToDeveloper {
		authorize := "https://" + instance + u.Path + "/oauth2/v2.0/authorize"
		status, raw, err := a.pipeline.run(ctx, http.MethodGet, "https://login.microsoftonline.com/common/discovery/instance?authorization_endpoint="+authorize+"&api-version=1.0", nil, nil, false)
		if err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		if payload["error"] == "invalid_instance" {
			return fmt.Errorf("The authority provided, %s, is not well-known. If this authority is valid and trustworthy, you can disable this check by passing in 'disable_instance_discovery=True' when constructing the credential.", strings.TrimSuffix(a.authority, "/"+tenant))
		}
		next, ok := payload["tenant_discovery_endpoint"].(string)
		if !ok {
			return fmt.Errorf("KeyError: 'tenant_discovery_endpoint' (HTTP %d)", status)
		}
		endpoint = next
	}
	status, raw, err := a.pipeline.run(ctx, http.MethodGet, endpoint, nil, nil, false)
	if err != nil {
		return err
	}
	if status >= 400 && status < 500 {
		return fmt.Errorf("OIDC Discovery failed on %s. HTTP status: %d, Error: %s", endpoint, status, string(raw))
	}
	if status != 200 {
		return &entraAuthError{Msg: "Unexpected response from Microsoft Entra ID"}
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	token, _ := config["token_endpoint"].(string)
	if token == "" {
		return errors.New("token_endpoint not found in configuration")
	}
	a.tokenEndpoint = token
	return nil
}

// acquire_token_for_client, a cached token counted as a hit or a token request carrying the refresh reason
func (a *msalConfidentialApp) acquireTokenForClient(ctx context.Context, scopes []string) (EntraAccessToken, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.discover(ctx); err != nil {
		return EntraAccessToken{}, err
	}
	key := strings.Join(scopes, " ")
	now := entraNow().Unix()
	cached, ok := a.tokens[key]
	reason := msalAtAbsent
	switch {
	case !ok:
	case cached.expiresOn-now < 300:
		reason = msalAtExpired
	case cached.refreshOn != 0 && cached.refreshOn < now:
		reason = msalAtAging
	default:
		a.telemetry.succeeded++
		return EntraAccessToken{Token: cached.token, ExpiresOn: cached.expiresOn, RefreshOn: cached.refreshOn}, nil
	}
	tok, err := a.requestToken(ctx, scopes, reason)
	if err != nil && reason == msalAtAging {
		return EntraAccessToken{Token: cached.token, ExpiresOn: cached.expiresOn, RefreshOn: cached.refreshOn}, nil
	}
	return tok, err
}

// One client credentials token request with the MSAL default, telemetry and pipeline headers
func (a *msalConfidentialApp) requestToken(ctx context.Context, scopes []string, reason int) (EntraAccessToken, error) {
	correlation := uuid.NewString()
	extra, err := a.credential(ctx, a.tokenEndpoint)
	if err != nil {
		return EntraAccessToken{}, err
	}
	form := append([][2]string{{"client_id", a.clientID}, {"grant_type", "client_credentials"}}, extra...)
	if scope := strings.Join(scopes, " "); scope != "" {
		form = append(form, [2]string{"scope", scope})
	}
	headers := []pyhttpx.HeaderLine{{Name: "Content-Type", Value: "application/x-www-form-urlencoded"}, {Name: "Accept", Value: "application/json"},
		{Name: "x-client-sku", Value: "MSAL.Python"}, {Name: "x-client-ver", Value: msalVersion}, {Name: "x-client-os", Value: msalClientOS()},
		{Name: "x-ms-lib-capability", Value: "retry-after, h429"}}
	headers = append(headers, a.telemetry.headers(msalAcquireTokenForClientID, reason, correlation)...)
	requestTime := entraNow().Unix()
	status, raw, err := a.pipeline.run(ctx, http.MethodPost, a.tokenEndpoint, headers, form, true)
	if err != nil {
		return EntraAccessToken{}, &entraAuthError{Msg: err.Error()}
	}
	if status >= 500 {
		return EntraAccessToken{}, &entraAuthError{Msg: "Unexpected response from Microsoft Entra ID"}
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Authentication failed: %s", strings.TrimSpace(string(raw)))}
	}
	a.telemetry.update(msalAcquireTokenForClientID, correlation, result)
	if _, ok := result["access_token"]; !ok {
		return EntraAccessToken{}, &entraAuthError{Msg: fmt.Sprintf("Authentication failed: %v", firstNonNil(result["error_description"], result["error"]))}
	}
	token, _ := result["access_token"].(string)
	now := entraNow().Unix()
	expiresIn, ok := entraInt(result["expires_in"])
	if !ok {
		expiresIn = 600
		if on, has := entraInt(result["expires_on"]); has {
			expiresIn = on - now
		}
	}
	entry := msalCachedToken{token: token, expiresOn: now + expiresIn}
	if in, ok := entraInt(result["refresh_in"]); ok {
		entry.refreshOn = now + in
	}
	a.tokens[strings.Join(scopes, " ")] = entry
	return EntraAccessToken{Token: token, ExpiresOn: requestTime + expiresIn, RefreshOn: entry.refreshOn}, nil
}

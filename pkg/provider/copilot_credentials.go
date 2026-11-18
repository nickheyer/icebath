package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// GitHub App client id of VS Code Copilot, minting ghu_ tokens the Copilot API exchanges
const copilotOAuthClientID = "Iv1.b507a08c87ecfe98"

const (
	copilotClassicPATPrefix           = "ghp_"
	copilotDeviceCodePollInterval     = 5
	copilotDeviceCodePollSafetyMargin = 3
	copilotJWTRefreshMarginSeconds    = 120
	copilotEditorVersion              = "vscode/1.104.1"
	copilotExchangeUserAgent          = "GitHubCopilotChat/0.26.7"
	copilotExchangeMaxAttempts        = 3
	copilotExchangeBackoffBase        = 1.5
	copilotJWTDiskFilename            = ".copilot_jwt.json"
	copilotJWTDiskMaxBytes            = 1_048_576
	copilotFailureTTLTransient        = 60.0
	copilotFailureTTLPermanent        = 1800.0
	copilotDNSGrace                   = 5 * time.Second
	copilotGitHubModelsFreeTierHost   = "models.inference.ai.azure.com"
	copilotRequestUserAgent           = "IcebathAgent/1.0"
)

var (
	copilotSupportedPrefixes = []string{"gho_", "github_pat_", "ghu_"}
	copilotEnvVars           = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
	copilotPoolSources       = []string{"env:COPILOT_GITHUB_TOKEN", "env:GH_TOKEN", "env:GITHUB_TOKEN"}
	copilotTokenExchangeURL  = "https://api.github.com/copilot_internal/v2/token"
	copilotPermanentStatuses = map[int]bool{401: true, 403: true, 404: true}
	copilotStaleMarkers      = []string{"model_not_available_for_integrator", "not available for integrator",
		"model_not_supported", "the requested model is not supported"}
	copilotGitHubModelsHint = []string{
		"   💡 GitHub Models free tier (models.inference.ai.azure.com) caps every",
		"      request at ~8K tokens. Icebath's system prompt + tool schemas baseline",
		"      exceeds that floor, so this endpoint cannot run an agentic loop.",
		"      Use the `copilot` provider with a Copilot subscription token (`icebath",
		"      setup` → GitHub Copilot), or pick any other provider.",
	}
	copilotDeviceTerminalErrors = map[string]string{"expired_token": "  ✗ Device code expired. Please try again.",
		"access_denied": "  ✗ Authorization was denied."}
	copilotProxyEpRe = pyre.MustCompile(`(?:^|;)\s*proxy-ep=([^;\s]+)`)
)

// One exchanged Copilot API token, its expiry epoch and the account base url, empty base meaning none
type copilotJWT struct {
	token     string
	expiresAt float64
	baseURL   string
}

var (
	copilotStateMu         sync.Mutex
	copilotJWTCache        = map[string]copilotJWT{}
	copilotFailureCache    = map[string]float64{}
	copilotExchangeLocks   = map[string]*sync.Mutex{}
	copilotRawDegradeWarns = map[string]bool{}
)

// Wall clock epoch seconds as Python time.time
func copilotNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// HTTP failure worded like urllib HTTPError with its status code
type copilotHTTPError struct {
	Code   int
	Reason string
}

func (e *copilotHTTPError) Error() string { return fmt.Sprintf("HTTP %d %s", e.Code, e.Reason) }

// Reply whose JSON is not an object, its plain kind
type copilotNotDictError struct{ Type string }

func (e *copilotNotDictError) Error() string {
	return fmt.Sprintf("the Copilot reply is %s, expected a mapping", e.Type)
}

// Python truth of a decoded JSON or config value, empty objects and zero big ints false
func cpTruthy(v any) bool {
	switch t := v.(type) {
	case *pyjson.Object:
		return t.Len() > 0
	case *big.Int:
		return t.Sign() != 0
	}
	return pyTruthy(v)
}

// Python str of value or else, the value counting only when truthy
func cpOrStr(v any) string {
	if !cpTruthy(v) {
		return ""
	}
	return pyStr(v)
}

// Key of a decoded JSON object, false for a missing key or a non object
func cpGet(v any, key string) (any, bool) {
	switch o := v.(type) {
	case *pyjson.Object:
		return o.Get(key)
	case map[string]any:
		val, ok := o[key]
		return val, ok
	}
	return nil, false
}

// Key of a decoded JSON object or the default when missing
func cpGetOr(v any, key string, def any) any {
	if val, ok := cpGet(v, key); ok {
		return val
	}
	return def
}

// Reports a decoded JSON object
func cpIsDict(v any) bool {
	switch v.(type) {
	case *pyjson.Object, map[string]any:
		return true
	}
	return false
}

// Values of a decoded JSON object in key order
func cpValues(v any) []any {
	switch o := v.(type) {
	case *pyjson.Object:
		out := make([]any, 0, len(o.Keys))
		for _, k := range o.Keys {
			out = append(out, o.Vals[k])
		}
		return out
	case map[string]any:
		out := make([]any, 0, len(o))
		for _, k := range config.MappingKeys(o) {
			out = append(out, o[k])
		}
		return out
	}
	return nil
}

// Python float of a JSON value, false where float raises
func cpFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(n), true
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	case float64:
		return n, true
	case int:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(pystr.Strip(strings.ReplaceAll(n, "_", "")), 64)
		return f, err == nil
	}
	return 0, false
}

// Python float of value or zero, the value counting only when truthy
func cpFloatOrZero(v any) (float64, bool) {
	if !cpTruthy(v) {
		return 0, true
	}
	return cpFloat(v)
}

// Positive number test of isinstance int or float with a value above zero
func cpPositiveNumber(v any) (float64, bool) {
	switch v.(type) {
	case bool, int64, *big.Int, float64, int:
		f, _ := cpFloat(v)
		return f, f > 0
	}
	return 0, false
}

// Shared HTTP client with Python urllib per operation timeouts and environment proxies
func copilotHTTPClient(timeout time.Duration, redirect func(*http.Request, []*http.Request) error) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 probeProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
		},
		Timeout:       timeout,
		CheckRedirect: redirect,
	}
}

// Reason phrase the server sent with the status line
func copilotReason(resp *http.Response) string {
	return strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
}

// Redirect policy of urllib keeping request headers on each hop and refusing a 307 or 308 of a POST
func copilotKeepHeadersRedirect(req *http.Request, via []*http.Request) error {
	if req.Response != nil && via[0].Method == http.MethodPost && (req.Response.StatusCode == 307 || req.Response.StatusCode == 308) {
		return &copilotHTTPError{Code: req.Response.StatusCode, Reason: copilotReason(req.Response)}
	}
	if len(via) >= 10 {
		return &copilotHTTPError{Code: req.Response.StatusCode, Reason: "The HTTP server returned a redirect error that would lead to an infinite loop."}
	}
	methodChanged := req.Method != via[0].Method
	for k, v := range via[0].Header {
		if methodChanged && (k == "Content-Length" || k == "Content-Type") {
			continue
		}
		req.Header[k] = append([]string(nil), v...)
	}
	return nil
}

// Sends the request and decodes its UTF-8 JSON body, a non 2xx status raising the urllib HTTPError form
func copilotDoJSON(client *http.Client, req *http.Request) (any, error) {
	resp, err := client.Do(req)
	if err != nil {
		var he *copilotHTTPError
		if errors.As(err, &he) {
			return nil, he
		}
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			return nil, fmt.Errorf("could not reach GitHub: %v", ue.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &copilotHTTPError{Code: resp.StatusCode, Reason: copilotReason(resp)}
	}
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, errors.New("the Copilot reply is not valid UTF-8")
	}
	return pyjson.Loads(string(body))
}

// POST of urlencoded fields answered with a JSON object, the GitHub device flow wire
func copilotPostForm(ctx context.Context, rawURL string, fields [][2]string, timeout time.Duration) (any, error) {
	form := url.Values{}
	for _, kv := range fields {
		form.Set(kv[0], kv[1])
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", copilotRequestUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	data, err := copilotDoJSON(copilotHTTPClient(timeout, copilotKeepHeadersRedirect), req)
	if err != nil {
		return nil, err
	}
	if !cpIsDict(data) {
		return nil, &copilotNotDictError{Type: errchain.Kind(data)}
	}
	return data, nil
}

// GET of a JSON document that never carries Authorization to another origin
func copilotGetJSON(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return copilotDoJSON(copilotHTTPClient(timeout, credentialRedirectPolicy(rawURL)), req)
}

// Usable token or the reason it cannot reach the Copilot API
func validateCopilotToken(token string) (bool, string) {
	token = pystr.Strip(token)
	if token == "" {
		return false, "Empty token"
	}
	if strings.HasPrefix(token, copilotClassicPATPrefix) {
		return false, "Classic Personal Access Tokens (ghp_*) are not supported by the " +
			"Copilot API. Use one of:\n" +
			"  → `icebath auth add copilot` to sign in with GitHub (device code)\n" +
			"  → A fine-grained PAT (github_pat_*) with Copilot Requests permission"
	}
	for _, p := range copilotSupportedPrefixes {
		if strings.HasPrefix(token, p) {
			return true, "OK"
		}
	}
	return false, "Unsupported GitHub token format for the Copilot API. " +
		"Supported token prefixes: " + strings.Join(copilotSupportedPrefixes, ", ") + "."
}

// First supported GitHub token in COPILOT_GITHUB_TOKEN, GH_TOKEN then GITHUB_TOKEN with its env name
func resolveCopilotToken(ctx context.Context) (string, string, error) {
	for _, envVar := range copilotEnvVars {
		raw, _, err := GetSecret(ctx, envVar)
		if err != nil {
			return "", "", err
		}
		val := pystr.Strip(raw)
		if val == "" {
			continue
		}
		valid, msg := validateCopilotToken(val)
		if valid {
			return val, envVar, nil
		}
		pkgLog().Warn(fmt.Sprintf("Token from %s is not supported: %s", envVar, msg))
	}
	return "", "", nil
}

// Python str of a JSON value or the default when the key is missing
func cpStrOr(v any, key, def string) string {
	val, ok := cpGet(v, key)
	if !ok {
		return def
	}
	return pyStr(val)
}

// Device code poll interval of a reply, five when absent and at least one
func copilotStartInterval(data any) (float64, error) {
	raw, ok := cpGet(data, "interval")
	if !ok {
		return copilotDeviceCodePollInterval, nil
	}
	f, isNum := cpFloat(raw)
	if _, isStr := raw.(string); isStr || !isNum {
		return 0, fmt.Errorf("the device code interval is %s, expected a number", errchain.Kind(raw))
	}
	return math.Max(f, 1), nil
}

// GitHub OAuth device code flow printing to out, empty on cancel, denial, expiry or timeout
func copilotDeviceCodeLogin(ctx context.Context, out io.Writer, host string, timeout time.Duration) (string, error) {
	domain := strings.TrimRight(host, "/")
	deviceData, err := copilotPostForm(ctx, "https://"+domain+"/login/device/code",
		[][2]string{{"client_id", copilotOAuthClientID}, {"scope", "read:user"}}, 15*time.Second)
	var notDict *copilotNotDictError
	if errors.As(err, &notDict) {
		return "", err
	}
	if err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		pkgLog().Error(fmt.Sprintf("Failed to initiate device authorization: %v", err))
		fmt.Fprintf(out, "  ✗ Failed to start device authorization: %v\n", err)
		return "", nil
	}
	verificationURI := cpStrOr(deviceData, "verification_uri", "https://github.com/login/device")
	userCode := cpStrOr(deviceData, "user_code", "")
	deviceCode := cpStrOr(deviceData, "device_code", "")
	interval, err := copilotStartInterval(deviceData)
	if err != nil {
		return "", err
	}
	if !cpTruthy(cpGetOr(deviceData, "device_code", "")) || !cpTruthy(cpGetOr(deviceData, "user_code", "")) {
		fmt.Fprintln(out, "  ✗ GitHub did not return a device code.")
		return "", nil
	}
	fmt.Fprintf(out, "\n  Open this URL in your browser: %s\n  Enter this code: %s\n\n", verificationURI, userCode)
	fmt.Fprint(out, "  Waiting for authorization...")
	pollFields := [][2]string{{"client_id", copilotOAuthClientID}, {"device_code", deviceCode},
		{"grant_type", "urn:ietf:params:oauth:grant-type:device_code"}}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration((interval + copilotDeviceCodePollSafetyMargin) * float64(time.Second))):
		}
		result, err := copilotPostForm(ctx, "https://"+domain+"/login/oauth/access_token", pollFields, 10*time.Second)
		if err != nil {
			var notDict *copilotNotDictError
			if errors.As(err, &notDict) {
				return "", err
			}
			fmt.Fprint(out, ".")
			continue
		}
		if token := cpGetOr(result, "access_token", nil); cpTruthy(token) {
			fmt.Fprintln(out, " ✓")
			return pyStr(token), nil
		}
		errValue := cpGetOr(result, "error", "")
		errText, _ := errValue.(string)
		if errText == "slow_down" {
			if n, ok := cpPositiveNumber(cpGetOr(result, "interval", nil)); ok {
				interval = math.Trunc(n)
			} else {
				interval += 5
			}
		}
		if errText == "authorization_pending" || errText == "slow_down" {
			fmt.Fprint(out, ".")
			continue
		}
		if cpTruthy(errValue) {
			msg, known := copilotDeviceTerminalErrors[errText]
			if !known {
				msg = "  ✗ Authorization failed: " + pyStr(errValue)
			}
			fmt.Fprintln(out, "\n"+msg)
			return "", nil
		}
	}
	fmt.Fprintln(out, "\n  ✗ Timed out waiting for authorization.")
	return "", nil
}

// First sixteen hex digits of the token's SHA-256, the cache key that never stores the token
func copilotTokenFingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// Bounded read of the persisted JWT store, nil when missing, oversized, unreadable or not an object
func copilotReadJWTStore(path string) *pyjson.Object {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Size() > copilotJWTDiskMaxBytes {
		pkgLog().Debug(fmt.Sprintf("Persisted Copilot JWT store exceeds %d bytes; ignoring", copilotJWTDiskMaxBytes))
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to read persisted Copilot JWT store: %v", err))
		return nil
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	loaded, err := pyjson.Loads(text)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to read persisted Copilot JWT store: %v", err))
		return nil
	}
	obj, _ := loaded.(*pyjson.Object)
	return obj
}

// Profile aware path of the exchanged JWT cache file
func copilotJWTDiskPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), copilotJWTDiskFilename)
}

// Runs op against the disk store, logging and swallowing every failure
func copilotWithJWTStore(ctx context.Context, verb string, op func(path string, store *pyjson.Object) error) {
	path := copilotJWTDiskPath(ctx)
	if err := op(path, copilotReadJWTStore(path)); err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to %s Copilot JWT: %v", verb, err))
	}
}

// Writes the JWT store compactly with owner only permissions
func copilotWriteJWTStore(ctx context.Context, path string, store *pyjson.Object) error {
	return config.AtomicJSONWriteMode(ctx, path, store, 0o600)
}

// Drops the cached exchange of a raw token in process and on disk and its failure memo
func evictCachedExchangedToken(ctx context.Context, raw string) {
	if raw == "" {
		return
	}
	fp := copilotTokenFingerprint(raw)
	copilotStateMu.Lock()
	delete(copilotJWTCache, fp)
	delete(copilotFailureCache, fp)
	copilotStateMu.Unlock()
	copilotWithJWTStore(ctx, "evict cached", func(path string, store *pyjson.Object) error {
		if store != nil {
			if _, ok := store.Get(fp); ok {
				store.Delete(fp)
				return copilotWriteJWTStore(ctx, path, store)
			}
		}
		return nil
	})
}

// Persisted exchange for a fingerprint, false when absent or unusable
func copilotLoadJWTFromDisk(ctx context.Context, fp string) (copilotJWT, bool) {
	var out copilotJWT
	found := false
	copilotWithJWTStore(ctx, "load persisted", func(_ string, store *pyjson.Object) error {
		if store == nil {
			return nil
		}
		entry, ok := store.Get(fp)
		if !ok || !cpIsDict(entry) {
			return nil
		}
		token := cpGetOr(entry, "api_token", "")
		expiresAt, ok := cpFloatOrZero(cpGetOr(entry, "expires_at", 0))
		if !ok {
			return fmt.Errorf("could not convert expires_at to float: %s", pyStr(cpGetOr(entry, "expires_at", 0)))
		}
		if !cpTruthy(token) || expiresAt == 0 {
			return nil
		}
		base, _ := cpGetOr(entry, "base_url", nil).(string)
		out, found = copilotJWT{token: pyStr(token), expiresAt: expiresAt, baseURL: base}, true
		return nil
	})
	return out, found
}

// Persists an exchange with owner only permissions, pruning expired entries
func copilotSaveJWTToDisk(ctx context.Context, fp string, jwt copilotJWT) {
	copilotWithJWTStore(ctx, "persist", func(path string, store *pyjson.Object) error {
		now := copilotNow()
		kept := pyjson.NewObject()
		if store != nil {
			for _, k := range store.Keys {
				v := store.Vals[k]
				if !cpIsDict(v) {
					continue
				}
				exp, ok := cpFloatOrZero(cpGetOr(v, "expires_at", 0))
				if !ok {
					return fmt.Errorf("could not convert expires_at to float: %s", pyStr(cpGetOr(v, "expires_at", 0)))
				}
				if exp > now {
					kept.Set(k, v)
				}
			}
		}
		var base any
		if jwt.baseURL != "" {
			base = jwt.baseURL
		}
		kept.Set(fp, pyjson.ObjectOf("api_token", jwt.token, "expires_at", jwt.expiresAt, "base_url", base))
		return copilotWriteJWTStore(ctx, path, kept)
	})
}

// Per fingerprint single flight lock
func copilotExchangeLockFor(fp string) *sync.Mutex {
	copilotStateMu.Lock()
	defer copilotStateMu.Unlock()
	lock, ok := copilotExchangeLocks[fp]
	if !ok {
		lock = &sync.Mutex{}
		copilotExchangeLocks[fp] = lock
	}
	return lock
}

// Exchange GET capped at timeout plus the DNS grace, failing fast on 401 403 and 404
func copilotFetchExchangeWithRetry(ctx context.Context, raw string, timeout time.Duration, fp string) (any, error) {
	var lastErr error
	permanent := false
	for attempt := 1; attempt <= copilotExchangeMaxAttempts; attempt++ {
		data, err := copilotExchangeOnce(ctx, raw, timeout)
		if err == nil {
			copilotStateMu.Lock()
			delete(copilotFailureCache, fp)
			copilotStateMu.Unlock()
			return data, nil
		}
		lastErr = err
		var httpErr *copilotHTTPError
		permanent = errors.As(err, &httpErr) && copilotPermanentStatuses[httpErr.Code]
		if permanent {
			pkgLog().Debug(fmt.Sprintf("Copilot token exchange rejected (HTTP %d); not retrying", httpErr.Code))
			break
		}
		if attempt < copilotExchangeMaxAttempts {
			sleep := copilotExchangeBackoffBase * float64(attempt)
			pkgLog().Debug(fmt.Sprintf("Copilot token exchange attempt %d/%d failed (%v); retrying in %.1fs",
				attempt, copilotExchangeMaxAttempts, err, sleep))
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(sleep * float64(time.Second))):
			}
		}
	}
	ttl := copilotFailureTTLTransient
	if permanent {
		ttl = copilotFailureTTLPermanent
	}
	copilotStateMu.Lock()
	copilotFailureCache[fp] = copilotNow() + ttl
	copilotStateMu.Unlock()
	return nil, fmt.Errorf("Copilot token exchange failed after %d attempts: %w", copilotExchangeMaxAttempts, lastErr)
}

// One bounded exchange request decoded as JSON
func copilotExchangeOnce(ctx context.Context, raw string, timeout time.Duration) (any, error) {
	capCtx, cancel := context.WithTimeout(ctx, timeout+copilotDNSGrace)
	defer cancel()
	req, err := http.NewRequestWithContext(capCtx, http.MethodGet, copilotTokenExchangeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+raw)
	req.Header.Set("User-Agent", copilotExchangeUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", copilotEditorVersion)
	client := copilotHTTPClient(timeout, copilotKeepHeadersRedirect)
	client.Timeout = 0
	data, err := copilotDoJSON(client, req)
	if err != nil && capCtx.Err() != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("copilot token exchange exceeded hard cap of %.0fs (DNS/getaddrinfo hang?)", (timeout + copilotDNSGrace).Seconds())
	}
	return data, err
}

// Fresh when the expiry is more than the refresh margin away
func copilotEntryFresh(e copilotJWT, ok bool) bool {
	return ok && copilotNow() < e.expiresAt-copilotJWTRefreshMarginSeconds
}

// Error naming how long the failure memo still blocks an exchange
func copilotRecentFailure(fp string) error {
	copilotStateMu.Lock()
	until := copilotFailureCache[fp]
	copilotStateMu.Unlock()
	if now := copilotNow(); now < until {
		return fmt.Errorf("Copilot token exchange recently failed; skipping re-attempt for another %ds", int(until-now))
	}
	return nil
}

// Exchanges a raw GitHub token for a Copilot API token with its expiry and account base url
func exchangeCopilotToken(ctx context.Context, raw string, timeout time.Duration) (copilotJWT, error) {
	fp := copilotTokenFingerprint(raw)
	copilotStateMu.Lock()
	cached, ok := copilotJWTCache[fp]
	copilotStateMu.Unlock()
	if copilotEntryFresh(cached, ok) {
		return cached, nil
	}
	if err := copilotRecentFailure(fp); err != nil {
		return copilotJWT{}, err
	}
	lock := copilotExchangeLockFor(fp)
	lock.Lock()
	defer lock.Unlock()
	return exchangeCopilotTokenLocked(ctx, raw, fp, timeout)
}

// The exchange under the fingerprint lock, rechecking memory then disk before the network
func exchangeCopilotTokenLocked(ctx context.Context, raw, fp string, timeout time.Duration) (copilotJWT, error) {
	copilotStateMu.Lock()
	cached, ok := copilotJWTCache[fp]
	copilotStateMu.Unlock()
	if !copilotEntryFresh(cached, ok) {
		cached, ok = copilotLoadJWTFromDisk(ctx, fp)
	}
	if copilotEntryFresh(cached, ok) {
		copilotStateMu.Lock()
		copilotJWTCache[fp] = cached
		copilotStateMu.Unlock()
		return cached, nil
	}
	if err := copilotRecentFailure(fp); err != nil {
		return copilotJWT{}, err
	}
	data, err := copilotFetchExchangeWithRetry(ctx, raw, timeout, fp)
	if err != nil {
		return copilotJWT{}, err
	}
	if !cpIsDict(data) {
		return copilotJWT{}, &copilotNotDictError{Type: errchain.Kind(data)}
	}
	tokenValue := cpGetOr(data, "token", "")
	if !cpTruthy(tokenValue) {
		return copilotJWT{}, errors.New("Copilot token exchange returned empty token")
	}
	apiToken := pyStr(tokenValue)
	expiresAt, okFloat := cpFloatOrZero(cpGetOr(data, "expires_at", nil))
	if !okFloat {
		return copilotJWT{}, fmt.Errorf("the Copilot token expiry %s is not a number", pyStr(cpGetOr(data, "expires_at", nil)))
	}
	if expiresAt == 0 {
		expiresAt = copilotNow() + 1800
	}
	baseURL := ""
	if endpoints := cpGetOr(data, "endpoints", nil); cpIsDict(endpoints) {
		baseURL = strings.TrimRight(pystr.Strip(cpOrStr(cpGetOr(endpoints, "api", nil))), "/")
	}
	if baseURL == "" {
		baseURL = deriveCopilotBaseURLFromProxyEp(apiToken)
	}
	jwt := copilotJWT{token: apiToken, expiresAt: expiresAt, baseURL: baseURL}
	copilotStateMu.Lock()
	copilotJWTCache[fp] = jwt
	copilotStateMu.Unlock()
	copilotSaveJWTToDisk(ctx, fp, jwt)
	shownBase := "None"
	if baseURL != "" {
		shownBase = baseURL
	}
	pkgLog().Debug(fmt.Sprintf("Copilot token exchanged, expires_at=%s, base_url=%s", pyFloatRepr(expiresAt), shownBase))
	return jwt, nil
}

// Copilot API base url from the token's proxy-ep field with proxy swapped for api, empty when absent
func deriveCopilotBaseURLFromProxyEp(token string) string {
	m, err := copilotProxyEpRe.FindStringMatch(token)
	if err != nil || m == nil {
		return ""
	}
	proxyEp := m.GroupByNumber(1).String()
	if rest, ok := strings.CutPrefix(proxyEp, "https://"); ok {
		proxyEp = rest
	} else if rest, ok := strings.CutPrefix(proxyEp, "http://"); ok {
		proxyEp = rest
	}
	proxyEp = strings.TrimRight(proxyEp, "/")
	if rest, ok := strings.CutPrefix(proxyEp, "proxy."); ok {
		proxyEp = "api." + rest
	}
	return "https://" + proxyEp
}

// Exchanged token and base url, the raw token and no base url when the exchange fails
func getCopilotAPIToken(ctx context.Context, raw string) (string, string) {
	if raw == "" {
		return raw, ""
	}
	jwt, err := exchangeCopilotToken(ctx, raw, 10*time.Second)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Copilot token exchange failed, using raw token: %v", err))
		return raw, ""
	}
	return jwt.token, jwt.baseURL
}

// Editor attribution and initiator headers every Copilot API request carries
func copilotRequestHeaders(isAgentTurn, isVision bool) map[string]string {
	initiator := "agent"
	if !isAgentTurn {
		initiator = "user"
	}
	headers := map[string]string{"Editor-Version": copilotEditorVersion, "User-Agent": copilotRequestUserAgent,
		"Copilot-Integration-Id": "vscode-chat", "Openai-Intent": "conversation-edits", "x-initiator": initiator}
	if isVision {
		headers["Copilot-Vision-Request"] = "true"
	}
	return headers
}

// Warns once per token per process when the exchange degraded to the raw token
func copilotWarnRawDegradationOnce(token string) {
	fingerprint := "unknown"
	if token != "" {
		fingerprint = copilotTokenFingerprint(token)
	}
	copilotStateMu.Lock()
	seen := copilotRawDegradeWarns[fingerprint]
	copilotRawDegradeWarns[fingerprint] = true
	copilotStateMu.Unlock()
	if seen {
		return
	}
	pkgLog().Warn("Copilot token exchange degraded to RAW token (exchange " +
		"unavailable); enterprise-only models may 400 with " +
		"model_not_available_for_integrator until exchange recovers.")
}

// Forces a fresh exchange after a 401, the resolved raw token when the exchange is unavailable
func copilotRefreshExchangedToken(ctx context.Context) (string, string, bool, error) {
	newToken, tokenSource, err := resolveCopilotToken(ctx)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Copilot credential refresh failed: %v", err))
		return "", "", false, nil
	}
	newToken = pystr.Strip(newToken)
	enterpriseBase := ""
	if newToken == "" {
		return "", "", false, nil
	}
	evictCachedExchangedToken(ctx, newToken)
	apiToken, exchangedBase := getCopilotAPIToken(ctx, newToken)
	if pystr.Strip(apiToken) != "" {
		newToken, enterpriseBase = pystr.Strip(apiToken), exchangedBase
	}
	pkgLog().Info("Copilot credentials refreshed from " + tokenSource)
	return newToken, enterpriseBase, true, nil
}

// Re-exchanges after a stale credential 400, declining when the exchange still degrades to raw
func copilotReexchangeStaleToken(ctx context.Context) (string, string, bool, error) {
	rawToken, tokenSource, err := resolveCopilotToken(ctx)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Copilot stale-credential recovery failed: %v", err))
		return "", "", false, nil
	}
	if pystr.Strip(rawToken) == "" {
		return "", "", false, nil
	}
	rawToken = pystr.Strip(rawToken)
	evictCachedExchangedToken(ctx, rawToken)
	apiToken, enterpriseBase := getCopilotAPIToken(ctx, rawToken)
	if pystr.Strip(apiToken) == "" {
		return "", "", false, nil
	}
	if apiToken == rawToken && enterpriseBase == "" {
		pkgLog().Warn("Copilot stale-credential recovery: exchange still degraded to raw token; skipping retry " +
			"(network/exchange endpoint unavailable).")
		return "", "", false, nil
	}
	pkgLog().Info(fmt.Sprintf("Copilot credentials re-exchanged after stale-credential 400 (source=%s)", tokenSource))
	return pystr.Strip(apiToken), enterpriseBase, true, nil
}

// Drops only the in process JWT after an auxiliary 401 and exchanges again, failing when the exchange fails
func copilotRevalidateInMemoryToken(ctx context.Context) (string, string, bool, error) {
	rawToken, _, err := resolveCopilotToken(ctx)
	if err != nil {
		return "", "", false, err
	}
	if pystr.Strip(rawToken) == "" {
		return "", "", false, nil
	}
	copilotStateMu.Lock()
	delete(copilotJWTCache, copilotTokenFingerprint(rawToken))
	copilotStateMu.Unlock()
	jwt, err := exchangeCopilotToken(ctx, rawToken, 10*time.Second)
	if err != nil {
		return "", "", false, err
	}
	return jwt.token, jwt.baseURL, true, nil
}

// Runtime refresher per refresh reason
var copilotRuntimeRefreshers = map[string]func(ctx context.Context) (string, string, bool, error){
	"unauthorized":           copilotRefreshExchangedToken,
	"stale":                  copilotReexchangeStaleToken,
	"auxiliary_unauthorized": copilotRevalidateInMemoryToken,
}

// Exchanged Copilot token and source, the raw env token without a GitHub call for a home that never chose Copilot
func copilotResolveAPIKeySecret(ctx context.Context) (string, string, bool) {
	token, source, err := resolveCopilotToken(ctx)
	if err == nil && token != "" {
		if !githubCallsAllowed(ctx) {
			return token, source, true
		}
		apiToken, _ := getCopilotAPIToken(ctx, token)
		return apiToken, source, true
	}
	return "", "", true
}

// Account base url from the exchange, else the env override, else the registry default
func copilotRuntimeBaseURL(ctx context.Context, _ string, def, envURL string) string {
	baseURL := def
	if envURL != "" {
		baseURL = strings.TrimRight(envURL, "/")
	}
	rawToken, _, err := resolveCopilotToken(ctx)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Copilot base URL resolution fell back to default: %v", err))
		return baseURL
	}
	if rawToken != "" && githubCallsAllowed(ctx) {
		_, resolved := getCopilotAPIToken(ctx, rawToken)
		if resolved = pystr.Strip(resolved); resolved != "" {
			baseURL = resolved
		}
	}
	return baseURL
}

// Exchanges a raw GitHub token still sitting in the selected pool row, warning once when it stays raw
func copilotPreparePoolEntry(ctx context.Context, entry *PooledCredential, apiKey string) string {
	if apiKey == "" {
		return apiKey
	}
	if valid, _ := validateCopilotToken(apiKey); !valid {
		return apiKey
	}
	apiToken, enterpriseBase := getCopilotAPIToken(ctx, apiKey)
	if apiToken == apiKey && enterpriseBase == "" {
		copilotWarnRawDegradationOnce(apiKey)
		return apiKey
	}
	entry.AccessToken = apiToken
	if enterpriseBase != "" {
		entry.BaseURL = enterpriseBase
	}
	return apiToken
}

// Pool row of the env token, exchanged only for a home that chose Copilot, nothing when its source is suppressed
func copilotSeedPoolCredentials(ctx context.Context, suppressed map[string]bool, _ bool) []map[string]any {
	all := true
	for _, s := range copilotPoolSources {
		if !suppressed[s] {
			all = false
			break
		}
	}
	if all {
		return []map[string]any{}
	}
	token, source, err := resolveCopilotToken(ctx)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Copilot token seed failed: %v", err))
		return []map[string]any{}
	}
	if token == "" {
		return []map[string]any{}
	}
	sourceName := "env:" + source
	if suppressed[sourceName] {
		return []map[string]any{}
	}
	apiToken, enterpriseBase := token, ""
	if githubCallsAllowed(ctx) {
		apiToken, enterpriseBase = getCopilotAPIToken(ctx, token)
		if apiToken == token && enterpriseBase == "" {
			copilotWarnRawDegradationOnce(token)
		}
	}
	var base any
	if enterpriseBase != "" {
		base = enterpriseBase
	}
	return []map[string]any{{"source": sourceName, "access_token": apiToken, "base_url": base, "label": source}}
}

// Every env source variant of the Copilot token with the removal hints, nothing for a non env source
func copilotRemoveCredentialSource(source string) ([]string, []string, bool) {
	if !strings.HasPrefix(source, "env:") {
		return nil, nil, false
	}
	return append([]string(nil), copilotPoolSources...), []string{
		"Suppressed all copilot env token sources — they will not be re-seeded.",
		"Note: Your shell environment is unchanged.",
		"Run `icebath auth add copilot` to re-enable if needed.",
	}, true
}

// Runtime credential refresh for the named reason, nothing for an unknown reason
func copilotRefreshRuntimeCredentials(ctx context.Context, reason string) (string, string, bool, error) {
	refresher, ok := copilotRuntimeRefreshers[reason]
	if !ok {
		return "", "", false, nil
	}
	return refresher(ctx)
}

// Status line after a successful refresh, empty for the generic wording
func copilotCredentialRefreshNotice(reason string) string {
	switch reason {
	case "stale":
		return "Copilot credential re-exchanged after model_not_available 400. Retrying request..."
	case "unauthorized":
		return "Copilot credentials refreshed after 401. Retrying request..."
	}
	return ""
}

// A 400 carrying an integrator or model not supported marker, meaning a stale or degraded credential
func copilotIsStaleCredentialError(statusCode int, message string) bool {
	lowered := strings.ToLower(message)
	if statusCode != 400 && !strings.Contains(lowered, "error code: 400") {
		return false
	}
	for _, marker := range copilotStaleMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// GitHub Models free tier hint for a 413 there, nothing elsewhere
func copilotRequestRejectionHint(statusCode int, baseURL string) []string {
	if statusCode == 413 && HostMatches(baseURL, copilotGitHubModelsFreeTierHost) {
		return append([]string(nil), copilotGitHubModelsHint...)
	}
	return []string{}
}

// Sign in hint appended to the missing credentials message
func copilotMissingCredentialsHint() any { return "Run `icebath auth add copilot` to sign in." }

// Sign in evidence for copilot-acp from a supported env token only, the CLI's own files never read
func copilotCLIAuthEvidence(ctx context.Context) (bool, string, bool) {
	for _, envVar := range copilotEnvVars {
		raw, _, err := GetSecret(ctx, envVar)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("copilot-acp env token evidence check failed: %v", err))
			return false, "", true
		}
		val := pystr.Strip(raw)
		if valid, _ := validateCopilotToken(val); val != "" && valid {
			return true, "env: " + envVar, true
		}
	}
	return false, "", true
}

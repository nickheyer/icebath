package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	solsticeInferenceBaseURL = "https://generativelanguage.googleapis.com/v1alpha"
	solsticeConfigPath       = "/api/oauth/gemini-auth/config"
	solsticeTimeout          = 30 * time.Second
	solsticeGenerateMethod   = "generateContentPerUserQuota"
	solsticeStreamMethod     = "streamGenerateContentPerUserQuota"
	solsticeMissingKeyError  = "Solstice needs a signed-in account. Run `icebath auth add solstice`."
)

// Hardcoded per user quota models, lite last as the reserve once the larger models are exhausted
var solsticeFallbackModels = []string{"gemini-3.1-pro", "gemini-4-argon", "gemini-3.8-flash", "gemini-3.5-flash-lite"}

var solsticeTokenPaths = map[string]string{"authorization_code": "/api/oauth/gemini-auth/exchange", "refresh_token": "/api/oauth/gemini-auth/refresh"}

// Broker config the login and refresh share, client id and scope discovered per login
func solsticeBaseCfg() OAuthPKCEConfig {
	cfg := NewOAuthPKCEConfig("", "", "")
	cfg.Label = "Solstice"
	cfg.RedirectPath = "/gemini-auth/callback"
	cfg.TokenRequest = solsticeBrokerTokenRequest
	cfg.ExtraAuthorizeParams = map[string]string{"access_type": "offline", "prompt": "consent"}
	return cfg
}

// Profiles the bundled solstice plugin registers
func solsticePluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "solstice", display: "Solstice", desc: "Solstice (your own per-user quota, OAuth sign-in)",
		authType: AuthTypeOAuthExternal, base: solsticeInferenceBaseURL, hidden: true, noHealth: true, noListing: true, vision: true,
		fallbacks: solsticeFallbackModels}, "solstice-oauth")
	p.DefaultAuxModel = "gemini-flash-lite-latest"
	p.AuthHandler = solsticeAuthHandler
	p.RefreshCredential = PKCERefreshCredential(solsticeBaseCfg())
	p.CreateClientHook = func(_ context.Context, clientKwargs map[string]any) (Client, error) {
		return newSolsticeClient(clientKwargs)
	}
	p.GetMaxTokensHook = func(string) int { return 0 }
	return []Profile{p}
}

// Login handler discovering the broker client on add, every other action on the base config
func solsticeAuthHandler(action string, args []string, provider string) (bool, error) {
	cfg := solsticeBaseCfg()
	if action == "add" {
		client, err := solsticeDiscoverClient()
		if err != nil {
			return false, err
		}
		cfg.ClientID, cfg.AuthorizeURL, cfg.Scopes = client["client_id"], client["authorize_url"], strings.Fields(client["scope"])
	}
	return PKCEAuthHandler(cfg)(action, args, provider)
}

func solsticeErr(message, code string, relogin bool, cause error) *AuthError {
	return &AuthError{Message: "solstice: " + message, Provider: "solstice", Code: code, ReloginRequired: relogin, Err: cause}
}

// Nous portal the user is signed into, the portal override first
func solsticeNASBaseURL(env PoolEnv) (string, error) {
	state, err := env.Store.GetProviderAuthState("nous")
	if err != nil {
		return "", err
	}
	return nousPortalBaseURLIn(env, state), nil
}

// Portal request carrying the user's Nous access token, every failure an auth error with its code
func solsticeNASRequest(method, path string, body map[string]string) (map[string]any, error) {
	env := pkceEnvFn()
	token, err := resolveNousAccessToken(env, NousAccessTokenOptions{})
	if err != nil {
		return nil, solsticeErr("sign in to Nous Portal first (`icebath auth add nous`).", "solstice_nous_login_required", true, err)
	}
	resp, err := solsticeNASSend(env, method, path, token, body)
	if err != nil {
		return nil, solsticeErr("Nous portal request failed: "+pkceTransportErrorName(err), "solstice_broker_unreachable", false, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, solsticeErr("not available on this Nous account yet.", "solstice_not_configured", false, nil)
	}
	if resp.StatusCode >= 400 {
		return nil, solsticeErr("Nous portal returned HTTP "+strconv.Itoa(resp.StatusCode)+".", "solstice_broker_failed", false, nil)
	}
	if readErr != nil {
		return nil, readErr
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("solstice: Nous portal answered invalid JSON: %w", err)
	}
	return payload, nil
}

func solsticeNASSend(env PoolEnv, method, path, token string, body map[string]string) (*http.Response, error) {
	base, err := solsticeNASBaseURL(env)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := env.HTTP
	if client == nil {
		client = oauthHTTPClient(env, solsticeTimeout, defaultOAuthVerify())
	}
	return client.Do(req)
}

// Client id, authorize URL and scope the portal serves for the shared Google client
func solsticeDiscoverClient() (map[string]string, error) {
	payload, err := solsticeNASRequest(http.MethodGet, solsticeConfigPath, nil)
	if err != nil {
		return nil, err
	}
	keys := []string{"client_id", "authorize_url", "scope"}
	var missing []string
	out := map[string]string{}
	for _, k := range keys {
		v := strings.TrimSpace(pyOrStr(payload[k]))
		if v == "" {
			missing = append(missing, k)
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, solsticeErr("Nous portal config is missing "+strings.Join(missing, ", ")+".", "solstice_config_invalid", false, nil)
	}
	return out, nil
}

// Grant sent to the portal broker, a refresh it rejects becoming a dead grant that needs a new login
func solsticeBrokerTokenRequest(grant map[string]string) (map[string]any, error) {
	path, ok := solsticeTokenPaths[grant["grant_type"]]
	if !ok {
		return nil, fmt.Errorf("KeyError: %s", pyStringRepr(grant["grant_type"]))
	}
	body := map[string]string{}
	for _, k := range []string{"code", "code_verifier", "redirect_uri", "refresh_token"} {
		if v, present := grant[k]; present {
			body[k] = v
		}
	}
	payload, err := solsticeNASRequest(http.MethodPost, path, body)
	if err != nil {
		var ae *AuthError
		if grant["grant_type"] == "refresh_token" && errors.As(err, &ae) && ae.Code == "solstice_broker_failed" {
			return nil, solsticeErr("the stored sign-in was rejected; run `icebath auth add solstice` again.", "invalid_grant", true, err)
		}
		return nil, err
	}
	return payload, nil
}

// Native Gemini client on the per user quota methods with the OAuth access token as a bearer
func newSolsticeClient(kwargs map[string]any) (Client, error) {
	rawKey, present := kwargs["api_key"]
	if !present {
		return nil, errors.New("TypeError: SolsticeClient.__init__() missing 1 required keyword-only argument: 'api_key'")
	}
	apiKey := pyStr(rawKey)
	if rawKey == nil || strings.TrimSpace(apiKey) == "" {
		return nil, errors.New(solsticeMissingKeyError)
	}
	base := pyStr(kwargs["base_url"])
	if kwargs["base_url"] == nil || base == "" {
		base = solsticeInferenceBaseURL
	}
	route := Route{Provider: "solstice", BaseURL: base, APIKey: apiKey, Mode: ModeGeminiNative,
		TokenSource: func(context.Context) (string, error) { return apiKey, nil }}
	if hdrs, ok := kwargs["default_headers"].(map[string]string); ok {
		route.Headers = copyStringMap(hdrs)
	} else if hdrs, ok := kwargs["default_headers"].(map[string]any); ok {
		route.Headers = map[string]string{}
		for k, v := range hdrs {
			route.Headers[k] = pyStr(v)
		}
	}
	switch t := kwargs["timeout"].(type) {
	case time.Duration:
		route.Timeout = t
	case float64:
		route.Timeout = time.Duration(t * float64(time.Second))
	case int:
		route.Timeout = time.Duration(t) * time.Second
	}
	if hc, ok := kwargs["http_client"].(*http.Client); ok && hc != nil {
		route.Transport = hc.Transport
	}
	client, err := newGeminiNativeClient(route)
	if err != nil {
		return nil, err
	}
	gc := client.(*geminiNativeClient)
	gc.generateMethod, gc.streamMethod = solsticeGenerateMethod, solsticeStreamMethod
	return gc, nil
}

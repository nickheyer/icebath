package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

const xaiRelogin = "Re-authenticate with `icebath model`."

func xaiErr(message, code string, relogin bool) *Error {
	return &Error{Message: message, Provider: ProviderXAIOAuth, Code: code, Relogin: relogin}
}

func tokenPairOf(tokens *doc) (string, string) {
	return stripped(tokens.value("access_token")), stripped(tokens.value("refresh_token"))
}

func xaiStateUsable(state *doc) bool {
	access, refresh := tokenPairOf(state.obj("tokens"))
	return state != nil && access != "" && refresh != ""
}

func (s *Store) xaiStateFromStore(store *doc, state *doc) *doc {
	if xaiStateUsable(state) {
		return state
	}
	for _, v := range store.obj("credential_pool").list(ProviderXAIOAuth) {
		entry := asRow(v)
		access, refresh := tokenPairOf(entry)
		if access == "" || refresh == "" {
			continue
		}
		merged := state.clone()
		if merged == nil {
			merged = newDoc()
		}
		tokens := newDoc()
		tokens.set("access_token", access)
		tokens.set("refresh_token", refresh)
		tokenType := textOf(entry.value("token_type"))
		if tokenType == "" {
			tokenType = "Bearer"
		}
		tokens.set("token_type", tokenType)
		merged.set("tokens", tokens)
		if truthy(entry.value("last_refresh")) {
			merged.set("last_refresh", entry.value("last_refresh"))
		}
		if !merged.has("auth_mode") {
			merged.set("auth_mode", "oauth_pkce")
		}
		return merged
	}
	return state
}

type xaiTokens struct {
	tokens      *doc
	lastRefresh string
	discovery   *doc
	redirectURI string
}

func (s *Store) readXAITokens(ctx context.Context, locked bool) (xaiTokens, error) {
	if locked {
		lctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
		if err != nil {
			return xaiTokens{}, err
		}
		defer unlock()
		ctx = lctx
	}
	store, err := s.load(s.authPath)
	if err != nil {
		return xaiTokens{}, err
	}
	state := s.xaiStateFromStore(store, s.loadState(store, ProviderXAIOAuth))
	if !xaiStateUsable(state) {
		root := s.loadRoot()
		if global := s.xaiStateFromStore(root, providerStateIn(root, ProviderXAIOAuth)); xaiStateUsable(global) {
			state = global
		}
	}
	if state.size() == 0 {
		return xaiTokens{}, xaiErr("No xAI OAuth credentials stored. Select xAI Grok OAuth (SuperGrok / Premium+) in `icebath model`.", "xai_auth_missing", true)
	}
	tokens := state.obj("tokens")
	if tokens == nil {
		return xaiTokens{}, xaiErr("xAI OAuth state is missing tokens. "+xaiRelogin, "xai_auth_invalid_shape", true)
	}
	access, refresh := tokenPairOf(tokens)
	if access == "" {
		return xaiTokens{}, xaiErr("xAI OAuth state is missing access_token. "+xaiRelogin, "xai_auth_missing_access_token", true)
	}
	if refresh == "" {
		return xaiTokens{}, xaiErr("xAI OAuth state is missing refresh_token. "+xaiRelogin, "xai_auth_missing_refresh_token", true)
	}
	return xaiTokens{tokens: tokens, lastRefresh: state.str("last_refresh"), discovery: state.obj("discovery"), redirectURI: stripped(state.value("redirect_uri"))}, nil
}

func (s *Store) writeThroughToRoot(ctx context.Context, provider string, state *doc) {
	if s.rootAuth == "" {
		return
	}
	if err := s.persistStateTo(ctx, provider, state, s.rootAuth, false); err != nil {
		slog.Debug(fmt.Sprintf("%s: write-through to global root failed", provider), "error", err)
	}
}

func (s *Store) saveXAITokens(ctx context.Context, tokens *doc, discovery *doc, redirectURI, lastRefresh, authMode string, setActive bool) error {
	if lastRefresh == "" {
		lastRefresh = isoStampZ(time.Now())
	}
	if authMode == "" {
		authMode = "oauth_device_code"
	}
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	state, source := s.stateWithSource(store, ProviderXAIOAuth)
	if state == nil {
		state = newDoc()
	}
	state.set("tokens", tokens)
	state.set("last_refresh", lastRefresh)
	state.set("auth_mode", authMode)
	if discovery.size() > 0 {
		state.set("discovery", discovery)
	}
	if redirectURI != "" {
		state.set("redirect_uri", redirectURI)
	}
	if source != "" && s.rootAuth != "" && samePath(source, s.rootAuth) {
		s.writeThroughToRoot(ctx, ProviderXAIOAuth, state)
		return nil
	}
	if err := s.storeProviderState(ctx, store, ProviderXAIOAuth, state, setActive); err != nil {
		return err
	}
	return s.save(ctx, store, s.authPath)
}

func xaiJWTExp(accessToken string) (float64, bool) {
	if !strings.Contains(accessToken, ".") {
		return 0, false
	}
	parts := strings.Split(accessToken, ".")
	claims := decodeJWTClaims(parts[0] + "." + parts[1] + ".")
	return numberOf(claims.value("exp"))
}

func xaiAccessTokenIsExpiring(accessToken string, skew time.Duration) bool {
	exp, ok := xaiJWTExp(accessToken)
	return ok && exp <= nowEpoch()+max(0, skew.Seconds())
}

func xaiProactiveRefreshSkew(accessToken string) time.Duration {
	exp, ok := xaiJWTExp(accessToken)
	if !ok {
		return XAIAccessTokenRefreshSkew
	}
	if remaining := exp - nowEpoch(); remaining > 0 && remaining <= 45*60 {
		return min(120*time.Second, XAIAccessTokenRefreshSkew)
	}
	return XAIAccessTokenRefreshSkew
}

func xaiURLProblem(raw string) (problem, host string) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "malformed", ""
	}
	host = strings.ToLower(parsed.Hostname())
	switch {
	case parsed.Scheme != "https":
		return "scheme", host
	case host == "":
		return "host", host
	case host != "x.ai" && !strings.HasSuffix(host, ".x.ai"):
		return "origin", host
	}
	return "", host
}

func xaiValidateOAuthEndpoint(endpoint, field string) error {
	problem, host := xaiURLProblem(endpoint)
	switch problem {
	case "malformed":
		_, err := url.Parse(endpoint)
		return err
	case "scheme":
		return xaiErr(fmt.Sprintf("xAI OIDC discovery returned a non-HTTPS %s: '%s'.", field, endpoint), "xai_discovery_invalid", false)
	case "host":
		return xaiErr(fmt.Sprintf("xAI OIDC discovery %s is missing a hostname: '%s'.", field, endpoint), "xai_discovery_invalid", false)
	case "origin":
		return xaiErr(fmt.Sprintf("xAI OIDC discovery %s host '%s' is not on the xAI origin "+
			"(expected x.ai or a *.x.ai subdomain). Refusing to use a cached "+
			"endpoint that may have been substituted by a MITM during initial "+
			"discovery; re-authenticate with `icebath model` to re-fetch.", field, host), "xai_discovery_invalid", false)
	}
	return nil
}

func xaiValidateInferenceBaseURL(value, fallback string) string {
	candidate := strings.TrimRight(strings.TrimSpace(value), "/")
	if candidate == "" {
		return fallback
	}
	problem, host := xaiURLProblem(candidate)
	switch problem {
	case "":
		return candidate
	case "malformed":
		slog.Warn(fmt.Sprintf("Ignoring malformed xAI base_url override '%s'; using %s instead.", candidate, fallback))
	case "scheme":
		slog.Warn(fmt.Sprintf("Refusing non-HTTPS xAI base_url override '%s' (xai-oauth bearer would be sent in cleartext); falling back to %s.", candidate, fallback))
	case "host":
		slog.Warn(fmt.Sprintf("Ignoring xAI base_url override '%s' with no hostname; using %s instead.", candidate, fallback))
	default:
		slog.Warn(fmt.Sprintf("Refusing xAI base_url override '%s' — host '%s' is not on the xAI origin "+
			"(expected x.ai or a *.x.ai subdomain). The xai-oauth bearer is only "+
			"valid against xAI's inference API; sending it elsewhere would leak "+
			"the credential. Falling back to %s.", candidate, host, fallback))
	}
	return fallback
}

func (s *Store) xaiDiscovery(ctx context.Context, timeout time.Duration) (*doc, error) {
	r, err := s.getReply(ctx, nil, XAIOAuthDiscoveryURL, map[string]string{"Accept": "application/json"}, timeout)
	if err != nil {
		return nil, &Error{Message: "xAI OIDC discovery failed: " + transportFailure(err), Provider: ProviderXAIOAuth, Code: "xai_discovery_failed", Cause: err}
	}
	if r.status != 200 {
		return nil, xaiErr(fmt.Sprintf("xAI OIDC discovery returned status %d.", r.status), "xai_discovery_failed", false)
	}
	parsed, err := r.value()
	if err != nil {
		return nil, xaiErr(fmt.Sprintf("xAI OIDC discovery returned invalid JSON: %v", err), "xai_discovery_invalid_json", false)
	}
	payload, ok := parsed.(*doc)
	if !ok {
		return nil, xaiErr("xAI OIDC discovery response was not a JSON object.", "xai_discovery_incomplete", false)
	}
	endpoints := newDoc()
	for _, k := range []string{"authorization_endpoint", "token_endpoint"} {
		v := stripped(payload.value(k))
		if v == "" {
			return nil, xaiErr("xAI OIDC discovery response was missing required endpoints.", "xai_discovery_incomplete", false)
		}
		endpoints.set(k, v)
	}
	for _, k := range endpoints.keys {
		if err := xaiValidateOAuthEndpoint(endpoints.str(k), k); err != nil {
			return nil, err
		}
	}
	return endpoints, nil
}

func xaiTokensFromPayload(payload *doc, access, fallbackRefresh string) *doc {
	tokens := newDoc()
	tokens.set("access_token", access)
	refreshRaw := payload.value("refresh_token")
	if !truthy(refreshRaw) {
		refreshRaw = fallbackRefresh
	}
	tokens.set("refresh_token", stripped(refreshRaw))
	tokens.set("id_token", stripped(payload.value("id_token")))
	tokens.set("expires_in", payload.value("expires_in"))
	tokenType := stripped(payload.value("token_type"))
	if tokenType == "" {
		tokenType = "Bearer"
	}
	tokens.set("token_type", tokenType)
	return tokens
}

func (s *Store) refreshXAIPure(ctx context.Context, refreshToken, tokenEndpoint string, timeout time.Duration) (*doc, string, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, "", xaiErr("xAI OAuth is missing refresh_token. "+xaiRelogin, "xai_auth_missing_refresh_token", true)
	}
	endpoint := strings.TrimSpace(tokenEndpoint)
	if endpoint == "" {
		discovery, err := s.xaiDiscovery(ctx, timeout)
		if err != nil {
			return nil, "", err
		}
		endpoint = discovery.str("token_endpoint")
	}
	if err := xaiValidateOAuthEndpoint(endpoint, "token_endpoint"); err != nil {
		return nil, "", err
	}
	form := formOf("grant_type", "refresh_token", "client_id", XAIOAuthClientID, "refresh_token", refreshToken)
	r, err := s.postForm(ctx, nil, endpoint, form, nil, max(5*time.Second, timeout))
	if err != nil {
		return nil, "", err
	}
	if r.status != 200 {
		suffix := ""
		if detail := strings.TrimSpace(wireMask(string(r.body))); detail != "" {
			suffix = " Response: " + detail
		}
		if r.status == 403 {
			return nil, "", xaiErr("xAI token refresh failed with HTTP 403."+suffix+
				" This OAuth account is not authorized for xAI API"+
				" access — xAI may be restricting API/OAuth use to"+
				" specific SuperGrok tiers despite the in-app"+
				" subscription being active. Re-logging in won't"+
				" change that; set ``XAI_API_KEY`` and switch to"+
				" ``provider: xai`` (API-key path) if available, or"+
				" upgrade your subscription at https://x.ai/grok.", "xai_oauth_tier_denied", false)
		}
		return nil, "", xaiErr("xAI token refresh failed."+suffix, "xai_refresh_failed", r.status == 400 || r.status == 401)
	}
	parsed, perr := r.value()
	if perr != nil {
		return nil, "", xaiErr(fmt.Sprintf("xAI token refresh returned invalid JSON: %v", perr), "xai_refresh_invalid_json", false)
	}
	payload, ok := parsed.(*doc)
	if !ok {
		return nil, "", xaiErr("xAI token refresh response was not a JSON object.", "xai_refresh_invalid_response", true)
	}
	access := stripped(payload.value("access_token"))
	if access == "" {
		return nil, "", xaiErr("xAI token refresh response was missing access_token.", "xai_refresh_missing_access_token", true)
	}
	return xaiTokensFromPayload(payload, access, refreshToken), isoStampZ(time.Now()), nil
}

func (s *Store) refreshXAITokens(ctx context.Context, tokens *doc, tokenEndpoint, redirectURI string, timeout time.Duration) (*doc, error) {
	authMode := "oauth_device_code"
	if state, err := s.providerState(ctx, ProviderXAIOAuth); err == nil && state.str("auth_mode") != "" {
		authMode = state.str("auth_mode")
	}
	refreshed, lastRefresh, err := s.refreshXAIPure(ctx, stripped(tokens.value("refresh_token")), tokenEndpoint, timeout)
	if err != nil {
		return nil, err
	}
	updated := tokens.clone()
	updated.set("access_token", refreshed.value("access_token"))
	updated.set("refresh_token", refreshed.value("refresh_token"))
	if truthy(refreshed.value("id_token")) {
		updated.set("id_token", refreshed.value("id_token"))
	}
	if refreshed.value("expires_in") != nil {
		updated.set("expires_in", refreshed.value("expires_in"))
	}
	if truthy(refreshed.value("token_type")) {
		updated.set("token_type", refreshed.value("token_type"))
	}
	discovery := newDoc()
	discovery.set("token_endpoint", tokenEndpoint)
	if err := s.saveXAITokens(ctx, updated, discovery, redirectURI, lastRefresh, authMode, false); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) quarantineXAITokens(ctx context.Context, cause *Error) {
	store, err := s.load(s.authPath)
	if err != nil {
		slog.Debug("xAI OAuth: failed to persist quarantined state", "error", err)
		return
	}
	state := s.loadState(store, ProviderXAIOAuth)
	if state == nil {
		state = newDoc()
	}
	tokens := state.obj("tokens").clone()
	if tokens == nil {
		tokens = newDoc()
	}
	tokens.del("access_token")
	tokens.del("refresh_token")
	state.set("tokens", tokens)
	state.set("last_auth_error", lastAuthErrorMarker(ProviderXAIOAuth, cause, "runtime_refresh_failure", "xai_refresh_failed"))
	if err := s.storeProviderState(ctx, store, ProviderXAIOAuth, state, false); err != nil {
		slog.Debug("xAI OAuth: failed to persist quarantined state", "error", err)
		return
	}
	if err := s.save(ctx, store, s.authPath); err != nil {
		slog.Debug("xAI OAuth: failed to persist quarantined state", "error", err)
	}
}

func (s *Store) xaiInferenceBaseURL(ctx context.Context) string {
	override := strings.TrimRight(strings.TrimSpace(s.lookupEnv(ctx, "ICEBATH_XAI_BASE_URL")), "/")
	if override == "" {
		override = strings.TrimRight(strings.TrimSpace(s.lookupEnv(ctx, "XAI_BASE_URL")), "/")
	}
	return xaiValidateInferenceBaseURL(override, DefaultXAIOAuthBaseURL)
}

func (s *Store) ResolveXAI(ctx context.Context, o ResolveOptions) (RuntimeCredentials, error) {
	ctx = s.bind(ctx)
	shouldRefresh := func(tokens *doc) bool {
		access := stripped(tokens.value("access_token"))
		skew := o.RefreshSkew
		if skew == 0 {
			skew = xaiProactiveRefreshSkew(access)
		}
		return o.ForceRefresh || (!o.NoRefresh && xaiAccessTokenIsExpiring(access, skew))
	}
	data, err := s.readXAITokens(ctx, true)
	if err != nil {
		return RuntimeCredentials{}, err
	}
	tokens := data.tokens.clone()
	timeout := seconds(s.envFloat(ctx, "ICEBATH_XAI_REFRESH_TIMEOUT_SECONDS", 20))
	if shouldRefresh(tokens) {
		lctx, unlock, lerr := s.lock(ctx, s.authPath, max(LockTimeout, timeout+5*time.Second))
		if lerr != nil {
			return RuntimeCredentials{}, lerr
		}
		defer unlock()
		if data, err = s.readXAITokens(lctx, false); err != nil {
			return RuntimeCredentials{}, err
		}
		tokens = data.tokens.clone()
		if shouldRefresh(tokens) {
			endpoint := stripped(data.discovery.value("token_endpoint"))
			if endpoint == "" {
				discovery, derr := s.xaiDiscovery(lctx, timeout)
				if derr != nil {
					return RuntimeCredentials{}, derr
				}
				endpoint = discovery.str("token_endpoint")
			}
			refreshed, rerr := s.refreshXAITokens(lctx, tokens, endpoint, data.redirectURI, timeout)
			if rerr != nil {
				var ae *Error
				if errors.As(rerr, &ae) && IsTerminalRefreshError(rerr, ProviderXAIOAuth) {
					s.quarantineXAITokens(lctx, ae)
				}
				return RuntimeCredentials{}, rerr
			}
			tokens = refreshed
		}
	}
	return RuntimeCredentials{
		Provider: ProviderXAIOAuth, BaseURL: s.xaiInferenceBaseURL(ctx), APIKey: NewSecret(stripped(tokens.value("access_token"))),
		Source: "icebath-auth-store", LastRefresh: data.lastRefresh, AuthMode: "oauth_device_code",
	}, nil
}

func (s *Store) XAIStatus(ctx context.Context) Status {
	ctx = s.bind(ctx)
	return s.poolFirstOAuthStatus(ctx, ProviderXAIOAuth, "oauth_device_code",
		func(token string) bool { return xaiAccessTokenIsExpiring(token, 0) },
		func() (RuntimeCredentials, error) { return s.ResolveXAI(ctx, ResolveOptions{NoRefresh: true}) }, nil)
}

type xaiLogin struct {
	tokens      *doc
	discovery   *doc
	baseURL     string
	lastRefresh string
}

func (s *Store) xaiDeviceCodeLogin(ctx context.Context, c Console, timeout time.Duration, openBrowser bool) (xaiLogin, error) {
	discovery, err := s.xaiDiscovery(ctx, timeout)
	if err != nil {
		return xaiLogin{}, err
	}
	timeout = max(20*time.Second, timeout)
	r, err := s.postForm(ctx, nil, XAIOAuthDeviceCodeURL, formOf("client_id", XAIOAuthClientID, "scope", XAIOAuthScope), nil, timeout)
	if err != nil {
		return xaiLogin{}, err
	}
	if r.status != 200 {
		message := fmt.Sprintf("xAI device-code request failed (HTTP %d).", r.status)
		if detail := strings.TrimSpace(wireMask(string(r.body))); detail != "" {
			message += " Response: " + detail
		}
		return xaiLogin{}, xaiErr(message, "device_code_request_failed", false)
	}
	device, err := r.object()
	if err != nil {
		return xaiLogin{}, err
	}
	missing := []string{}
	for _, k := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"} {
		if !device.has(k) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return xaiLogin{}, xaiErr("xAI device-code response missing fields: "+strings.Join(missing, ", "), "device_code_invalid", false)
	}
	interval := max(1, coerceTTL(device.value("interval")))
	verification := textOf(device.value("verification_uri_complete"))
	if verification == "" {
		verification = textOf(device.value("verification_uri"))
	}
	if err := s.PrintDeviceCodeInstructions(ctx, c, verification, textOf(device.value("user_code")),
		openBrowser && !s.isRemoteSession(ctx) && s.canOpenGraphicalBrowser(ctx), "", true); err != nil {
		return xaiLogin{}, err
	}
	c.printf("Waiting for approval (polling every %ds)...\n", interval)
	tokenEndpoint := discovery.str("token_endpoint")
	deviceCode := textOf(device.value("device_code"))
	raw, err := PollDeviceToken(ctx, DevicePollOptions{
		Post: func(ctx context.Context) (DeviceReply, error) {
			return s.deviceReply(s.postForm(ctx, nil, tokenEndpoint,
				formOf("grant_type", DeviceCodeGrantType, "client_id", XAIOAuthClientID, "device_code", deviceCode), nil, timeout))
		},
		ExpiresIn: time.Duration(coerceTTL(device.value("expires_in"))) * time.Second,
		Interval:  time.Duration(interval) * time.Second,
		Validate: func(payload json.RawMessage) error {
			d, _ := parseObject(payload)
			for _, f := range [][2]string{{"access_token", "an"}, {"refresh_token", "a"}} {
				if !truthy(d.value(f[0])) {
					return xaiErr(fmt.Sprintf("xAI device-code token response did not include %s %s.", f[1], f[0]), "xai_device_token_invalid", false)
				}
			}
			return nil
		},
		OnNonJSONError: func(DeviceReply) error {
			return xaiErr("xAI device-code token polling returned a non-JSON error response.", "xai_device_token_failed", false)
		},
		OnError: func(r DeviceReply, payload json.RawMessage) error {
			d, _ := parseObject(payload)
			description := textOf(d.value("error_description"))
			if description == "" {
				description = textOf(d.value("error"))
			}
			if description == "" {
				description = wireMask(string(r.Body))
			}
			return &SignInFlowError{Err: xaiErr("xAI device-code token polling failed: "+description, "xai_device_token_failed", false), OAuthCode: textOf(d.value("error"))}
		},
		OnTimeout: func(cause error) error {
			return &Error{Message: "Timed out waiting for xAI device authorization.", Provider: ProviderXAIOAuth, Code: "device_code_timeout", Cause: cause}
		},
	})
	if err != nil {
		return xaiLogin{}, err
	}
	payload, _ := parseObject(raw)
	access, refresh := tokenPairOf(payload)
	if access == "" || refresh == "" {
		return xaiLogin{}, xaiErr("xAI device-code token response was missing required tokens.", "xai_device_token_invalid", false)
	}
	return xaiLogin{tokens: xaiTokensFromPayload(payload, access, refresh), discovery: discovery, baseURL: s.xaiInferenceBaseURL(ctx), lastRefresh: isoStampZ(time.Now())}, nil
}

type SignInFlowError struct {
	Err       *Error
	OAuthCode string
}

func (e *SignInFlowError) Error() string { return e.Err.Error() }

func (e *SignInFlowError) Unwrap() error { return e.Err }

func (s *Store) LoginXAI(ctx context.Context, c Console, o LoginOptions) error {
	ctx = s.bind(ctx)
	if o.UpdateConfig == nil {
		return errors.New("auth: xAI login needs the config update function")
	}
	if !o.ForceNewLogin {
		accepted, err := s.OfferExistingCredentials(ctx, c, ExistingOffer{
			Provider: ProviderXAIOAuth, DisplayName: "xAI OAuth", DefaultBaseURL: DefaultXAIOAuthBaseURL,
			Resolve: func(ctx context.Context) (Secret, string, error) {
				creds, err := s.ResolveXAI(ctx, ResolveOptions{})
				return creds.APIKey, creds.BaseURL, err
			},
			IsExpiring:   xaiAccessTokenIsExpiring,
			UpdateConfig: o.UpdateConfig,
		})
		if err != nil || accepted {
			return err
		}
	}
	c.println("")
	c.println("Signing in to xAI Grok OAuth (SuperGrok / Premium+)...")
	c.println("(icebath creates its own local OAuth session)")
	c.println("")
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	creds, err := s.xaiDeviceCodeLogin(ctx, c, timeout, !o.NoBrowser && !s.isRemoteSession(ctx))
	if err != nil {
		return err
	}
	if err := s.saveXAITokens(ctx, creds.tokens, creds.discovery, "", creds.lastRefresh, "oauth_device_code", true); err != nil {
		return err
	}
	if _, err := s.Unsuppress(ctx, ProviderXAIOAuth, "device_code"); err != nil {
		return err
	}
	baseURL := creds.baseURL
	if baseURL == "" {
		baseURL = DefaultXAIOAuthBaseURL
	}
	configPath, err := o.UpdateConfig(ctx, ProviderXAIOAuth, baseURL)
	if err != nil {
		return err
	}
	s.PrintLoginSuccess(c, ProviderXAIOAuth, configPath, true)
	return nil
}

func (s *Store) SaveXAITokens(ctx context.Context, tokens json.RawMessage, setActive bool) error {
	d, err := parseObject(tokens)
	if err != nil {
		return err
	}
	return s.saveXAITokens(s.bind(ctx), d, nil, "", "", "oauth_device_code", setActive)
}

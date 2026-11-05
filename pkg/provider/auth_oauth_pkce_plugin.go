package provider

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// PKCEPoolSource marks loopback PKCE rows as manual so load_pool reseeding never prunes them
const PKCEPoolSource = "manual:loopback_pkce"

var pkceLoopbackLiterals = map[string]bool{"127.0.0.1": true, "::1": true}

// OAuthPKCEConfig is the public client OAuth metadata a provider plugin declares, PKCE replacing any client secret
type OAuthPKCEConfig struct {
	ClientID             string
	AuthorizeURL         string
	TokenURL             string
	Scopes               []string
	RedirectPort         int
	RedirectPath         string
	Audience             string
	ExtraAuthorizeParams map[string]string
	ExtraTokenParams     map[string]string
	AllowedHosts         []string
	TimeoutSeconds       float64
	Label                string
	// Brokered token exchange called with the grant fields instead of posting token_url
	TokenRequest func(grant map[string]string) (map[string]any, error)
}

// NewOAuthPKCEConfig fills the declared endpoints with the callback path and timeout defaults
func NewOAuthPKCEConfig(clientID, authorizeURL, tokenURL string) OAuthPKCEConfig {
	return OAuthPKCEConfig{ClientID: clientID, AuthorizeURL: authorizeURL, TokenURL: tokenURL, RedirectPath: "/callback", TimeoutSeconds: 180}
}

// Process home pool env the plugin hooks read and write, swappable so tests can inject transports
var pkceEnvFn = func() PoolEnv { return modelsPoolEnv(context.Background()) }

// Console the login and handler messages print to
var pkceOut io.Writer = os.Stdout

func pkceErr(provider, message, code string) *AuthError {
	return &AuthError{Message: provider + ": " + message, Provider: provider, Code: code}
}

func pkceHostAllowed(host string, allowlist []string) bool {
	for _, apex := range allowlist {
		if host == apex || strings.HasSuffix(host, "."+apex) {
			return true
		}
	}
	return false
}

// Lowercased endpoint host, refusing a hostless URL and plain HTTP anywhere but a loopback literal
func pkceEndpointHost(provider, name, raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	host := ""
	scheme := ""
	if err == nil {
		host, scheme = strings.ToLower(parsed.Hostname()), strings.ToLower(parsed.Scheme)
	}
	if host == "" {
		return "", pkceErr(provider, fmt.Sprintf("OAuth %s has no host.", name), "oauth_endpoint_invalid")
	}
	if scheme != "https" && !(scheme == "http" && pkceLoopbackLiterals[host]) {
		return "", pkceErr(provider, fmt.Sprintf("OAuth %s must use HTTPS.", name), "oauth_endpoint_invalid")
	}
	return host, nil
}

// ValidateOAuthPKCEConfig refuses a misdeclared config before any network request of login or refresh
func ValidateOAuthPKCEConfig(provider string, cfg OAuthPKCEConfig) error {
	if cfg.TokenRequest != nil {
		return nil
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return pkceErr(provider, "OAuth client_id is missing.", "oauth_client_id_missing")
	}
	authorizeHost, err := pkceEndpointHost(provider, "authorize_url", cfg.AuthorizeURL)
	if err != nil {
		return err
	}
	tokenHost, err := pkceEndpointHost(provider, "token_url", cfg.TokenURL)
	if err != nil {
		return err
	}
	var allowlist []string
	for _, h := range cfg.AllowedHosts {
		allowlist = append(allowlist, strings.ToLower(h))
	}
	if len(allowlist) == 0 {
		allowlist = []string{authorizeHost}
	}
	if !pkceHostAllowed(tokenHost, allowlist) {
		sorted := append([]string{}, allowlist...)
		sort.Strings(sorted)
		reprs := make([]string, len(sorted))
		for i, h := range sorted {
			reprs[i] = pyRepr(h)
		}
		return pkceErr(provider, fmt.Sprintf("OAuth token_url host %s is not on the allowlist [%s].", pyRepr(tokenHost), strings.Join(reprs, ", ")), "oauth_token_host_rejected")
	}
	if cfg.RedirectPort < 0 || cfg.RedirectPort > 65535 {
		return pkceErr(provider, "OAuth redirect_port must be within 0..65535.", "oauth_redirect_invalid")
	}
	return nil
}

// Exception class name the token POST transport failure would carry, never the payload
func pkceTransportErrorName(err error) string {
	var opErr *net.OpError
	isDial := errors.As(err, &opErr) && opErr.Op == "dial"
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if isDial {
			return "ConnectTimeout"
		}
		return "ReadTimeout"
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if isDial || errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) {
		return "ConnectError"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "RemoteProtocolError"
	}
	return "ReadError"
}

// Token endpoint client that never follows a redirect, a 3xx being an answer and not a token
func pkceTokenClient(env PoolEnv) *http.Client {
	var client http.Client
	if env.HTTP != nil {
		client = *env.HTTP
	} else {
		client = *oauthHTTPClient(env, 30*time.Second, defaultOAuthVerify())
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// Posts the token endpoint or the plugin broker and returns the rotated pool fields
func pkcePostToken(env PoolEnv, provider string, cfg OAuthPKCEConfig, data map[string]string, code string) (map[string]any, error) {
	if cfg.TokenRequest != nil {
		grant := make(map[string]string, len(data))
		for k, v := range data {
			grant[k] = v
		}
		payload, err := cfg.TokenRequest(grant)
		if err != nil {
			return nil, err
		}
		return pkceTokenFields(provider, payload, data, code)
	}
	form := url.Values{}
	for k, v := range cfg.ExtraTokenParams {
		form.Set(k, v)
	}
	for k, v := range data {
		form.Set(k, v)
	}
	form.Set("client_id", cfg.ClientID)
	if cfg.Audience != "" && !form.Has("audience") {
		form.Set("audience", cfg.Audience)
	}
	resp, err := oauthPostForm(env.ctx(), pkceTokenClient(env), cfg.TokenURL, form, map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, &AuthError{Message: provider + ": OAuth token request failed: " + pkceTransportErrorName(err), Provider: provider, Code: code, Err: err}
	}
	if resp.Status >= 400 {
		return nil, pkceTokenHTTPError(provider, resp, code)
	}
	payload, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	obj, ok := payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(payload))
	}
	return pkceTokenFields(provider, obj, data, code)
}

func pyTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case float64:
		return "float"
	case bool:
		return "bool"
	case nil:
		return "NoneType"
	}
	return "dict"
}

// Pool fields of a token payload, a response without refresh_token keeping the one that was sent
func pkceTokenFields(provider string, payload map[string]any, data map[string]string, code string) (map[string]any, error) {
	accessToken := strings.TrimSpace(orEmptyStr(payload["access_token"]))
	if accessToken == "" {
		return nil, pkceErr(provider, "OAuth token response carried no access_token.", code)
	}
	expiresIn, ok := payload["expires_in"]
	if !ok {
		expiresIn = float64(0)
	}
	ttl := coerceTTLSeconds(expiresIn)
	refresh := orEmptyStr(payload["refresh_token"])
	if refresh == "" {
		refresh = data["refresh_token"]
	}
	var refreshToken, expiresAtMS any
	if r := strings.TrimSpace(refresh); r != "" {
		refreshToken = r
	}
	if ttl != 0 {
		expiresAtMS = time.Now().UnixMilli() + int64(ttl)*1000
	}
	return map[string]any{"access_token": accessToken, "refresh_token": refreshToken, "expires_at_ms": expiresAtMS, "last_refresh": utcNowZ()}, nil
}

// Maps a failed token response, a grant dead JSON error becoming the error code, the body never logged
func pkceTokenHTTPError(provider string, resp *oauthHTTPResponse, fallbackCode string) *AuthError {
	errorCode := ""
	if payload, err := resp.JSON(); err == nil {
		if obj, ok := payload.(map[string]any); ok {
			errorCode = strings.TrimSpace(orEmptyStr(obj["error"]))
		}
	}
	if oauthGrantDeadCodes[errorCode] {
		return pkceErr(provider, fmt.Sprintf("OAuth token request failed with HTTP %d (%s).", resp.Status, errorCode), errorCode)
	}
	return pkceErr(provider, fmt.Sprintf("OAuth token request failed with HTTP %d.", resp.Status), fallbackCode)
}

// Canonical profile name for the credential pool when the typed provider is an alias
func pkcePoolProvider(provider string) string {
	raw := strings.ToLower(strings.TrimSpace(provider))
	if profile, ok := ProfileFor(raw); ok {
		return profile.Name
	}
	return raw
}

func pkceTokenURLSafe(n int) string {
	raw := make([]byte, n)
	fillRandom(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Query string in declaration order with later keys replacing earlier ones in place, quote_plus encoded
func orderedQuery(pairs [][2]string) string {
	var keys []string
	values := map[string]string{}
	for _, kv := range pairs {
		if _, seen := values[kv[0]]; !seen {
			keys = append(keys, kv[0])
		}
		values[kv[0]] = kv[1]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = url.QueryEscape(k) + "=" + url.QueryEscape(values[k])
	}
	return strings.Join(parts, "&")
}

// LoginOAuthPKCE runs the browser authorization code plus PKCE flow and returns the pool fields of the new grant
func LoginOAuthPKCE(provider string, cfg OAuthPKCEConfig, openBrowser bool) (map[string]any, error) {
	return pkceLogin(pkceEnvFn(), pkceOut, provider, cfg, openBrowser)
}

func pkceLogin(env PoolEnv, out io.Writer, provider string, cfg OAuthPKCEConfig, openBrowser bool) (map[string]any, error) {
	if err := ValidateOAuthPKCEConfig(provider, cfg); err != nil {
		return nil, err
	}
	path := cfg.RedirectPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	label := cfg.Label
	if label == "" {
		label = provider
	}
	errFn := func(message, code string) *AuthError { return pkceErr(provider, message, code) }
	handler, result := newLoopbackCallbackHandler(path, label)
	server, err := bindLoopbackCallbackServer("127.0.0.1", cfg.RedirectPort, handler, errFn, "oauth_callback_bind_failed")
	if err != nil {
		return nil, err
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", server.Port(), path)
	verifier := pkceCodeVerifier(64)
	state := pkceTokenURLSafe(32)
	pairs := make([][2]string, 0, len(cfg.ExtraAuthorizeParams)+8)
	extraKeys := make([]string, 0, len(cfg.ExtraAuthorizeParams))
	for k := range cfg.ExtraAuthorizeParams {
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		pairs = append(pairs, [2]string{k, cfg.ExtraAuthorizeParams[k]})
	}
	pairs = append(pairs, [2]string{"client_id", cfg.ClientID}, [2]string{"response_type", "code"}, [2]string{"redirect_uri", redirectURI},
		[2]string{"state", state}, [2]string{"code_challenge", pkceCodeChallenge(verifier)}, [2]string{"code_challenge_method", "S256"})
	if len(cfg.Scopes) > 0 {
		pairs = append(pairs, [2]string{"scope", strings.Join(cfg.Scopes, " ")})
	}
	if cfg.Audience != "" {
		pairs = append(pairs, [2]string{"audience", cfg.Audience})
	}
	sep := "?"
	if parsed, err := url.Parse(cfg.AuthorizeURL); err == nil && parsed.RawQuery != "" {
		sep = "&"
	}
	authorizeURL := cfg.AuthorizeURL + sep + orderedQuery(pairs)
	fmt.Fprintf(out, "\nOpen this URL to authorize Icebath with %s:\n  %s\n\n", label, authorizeURL)
	fmt.Fprintf(out, "Waiting for callback on %s (timeout %ds, Ctrl+C to cancel)...\n", redirectURI, int(cfg.TimeoutSeconds))
	printLoopbackSSHHint(out, redirectURI, "")
	if openBrowser && canOpenGraphicalBrowser() {
		if openBrowserFn(authorizeURL) != nil {
			fmt.Fprintln(out, "Could not open the browser automatically; use the URL above.")
		}
	}
	callback, err := serveLoopbackCallback(server, result, time.Duration(cfg.TimeoutSeconds*float64(time.Second)), errFn, "oauth_callback_timeout")
	if err != nil {
		if env.ctx().Err() != nil {
			fmt.Fprintln(out, "\nLogin cancelled.")
			return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
		return nil, err
	}
	if truthyAny(callback["error"]) {
		detail := orEmptyStr(callback["error_description"])
		if detail == "" {
			detail = anyString(callback["error"])
		}
		return nil, errFn("authorization failed: "+detail, "oauth_authorization_denied")
	}
	if subtle.ConstantTimeCompare([]byte(orEmptyStr(callback["state"])), []byte(state)) != 1 {
		return nil, errFn("callback state mismatch — the redirect did not come from this login. Aborting.", "oauth_state_mismatch")
	}
	code := strings.TrimSpace(orEmptyStr(callback["code"]))
	if code == "" {
		return nil, errFn("callback carried no authorization code.", "oauth_no_code")
	}
	return pkcePostToken(env, provider, cfg, map[string]string{
		"grant_type": "authorization_code", "code": code, "redirect_uri": redirectURI, "code_verifier": verifier,
	}, "oauth_token_exchange_failed")
}

// A non blank access token whose expiry is unset or still ahead of now
func pkceIsUsable(accessToken, expiresAtMS any, nowMS int64) bool {
	if strings.TrimSpace(orEmptyStr(accessToken)) == "" {
		return false
	}
	if expiresAtMS == nil {
		return true
	}
	ts, ok := anyFloat(expiresAtMS)
	return ok && int64(ts) > nowMS
}

func pkceEntryExpiry(entry PooledCredential) any {
	if entry.ExpiresAtMS == 0 {
		return nil
	}
	return entry.ExpiresAtMS
}

// PKCEAuthHandler owns add, status and logout for the provider and declines refresh to the pool's generic refresh
func PKCEAuthHandler(cfg OAuthPKCEConfig) func(action string, args []string, provider string) (bool, error) {
	return func(action string, args []string, provider string) (bool, error) {
		env := pkceEnvFn()
		out := pkceOut
		provider = pkcePoolProvider(provider)
		label := cfg.Label
		if label == "" {
			label = provider
		}
		switch action {
		case "add":
			noBrowser := false
			for _, a := range args {
				noBrowser = noBrowser || a == "--no-browser"
			}
			tokens, err := pkceLogin(env, out, provider, cfg, !noBrowser)
			if err != nil {
				return false, err
			}
			idBytes := make([]byte, 3)
			fillRandom(idBytes)
			entry := PooledCredential{Provider: provider, ID: hex.EncodeToString(idBytes), Label: label, AuthType: AuthTypeOAuth, Priority: 0, Source: PKCEPoolSource,
				Extra: map[string]any{"oauth_pkce": map[string]any{"client_id": cfg.ClientID, "scope": strings.Join(cfg.Scopes, " ")}}}
			entry = ApplyPluginRefreshResult(entry, tokens)
			added, err := LoadPool(env, provider).AddEntry(entry)
			if err != nil {
				return false, err
			}
			fmt.Fprintf(out, "Signed in to %s; credential %s added to the pool.\n", label, added.ID)
			return true, nil
		case "status":
			entries := LoadPool(env, provider).Entries()
			nowMS := time.Now().UnixMilli()
			usable := false
			for _, e := range entries {
				usable = usable || pkceIsUsable(e.AccessToken, pkceEntryExpiry(e), nowMS)
			}
			switch {
			case len(entries) == 0:
				fmt.Fprintf(out, "%s: logged out\n", provider)
			case usable:
				fmt.Fprintf(out, "%s: logged in\n  auth_type: oauth (pkce)\n  credentials: %d\n", provider, len(entries))
			default:
				fmt.Fprintf(out, "%s: expired (needs refresh) — run `icebath auth refresh %s`\n", provider, provider)
			}
			return true, nil
		case "logout":
			pool := LoadPool(env, provider)
			count := len(pool.Entries())
			for index := count; index >= 1; index-- {
				pool.RemoveIndex(index)
			}
			fmt.Fprintf(out, "Logged out of %s (%d credential(s) removed)\n", provider, count)
			return true, nil
		}
		return false, nil
	}
}

// PKCERefreshCredential rotates one pooled row through the refresh grant, adopting a peer rotation found on disk
func PKCERefreshCredential(cfg OAuthPKCEConfig) func(entry PooledCredential) (map[string]any, error) {
	return func(entry PooledCredential) (map[string]any, error) {
		return pkceRefresh(pkceEnvFn(), cfg, entry)
	}
}

func pkceRefresh(env PoolEnv, cfg OAuthPKCEConfig, entry PooledCredential) (map[string]any, error) {
	provider := entry.Provider
	if err := ValidateOAuthPKCEConfig(provider, cfg); err != nil {
		return nil, err
	}
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rows, err := env.Store.ReadCredentialPool(provider)
	if err != nil {
		return nil, err
	}
	onDisk := map[string]any{}
	for _, row := range rows {
		if row != nil && row["id"] == entry.ID {
			onDisk = row
			break
		}
	}
	diskRefresh := strings.TrimSpace(orEmptyStr(onDisk["refresh_token"]))
	if diskRefresh != "" && diskRefresh != entry.RefreshToken && pkceIsUsable(onDisk["access_token"], onDisk["expires_at_ms"], time.Now().UnixMilli()) {
		env.log().Debug(fmt.Sprintf("%s entry %s: adopting a peer's rotation, refresh token not spent", provider, entry.ID))
		return map[string]any{"access_token": onDisk["access_token"], "refresh_token": onDisk["refresh_token"],
			"expires_at_ms": onDisk["expires_at_ms"], "last_refresh": onDisk["last_refresh"]}, nil
	}
	refreshToken := diskRefresh
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(entry.RefreshToken)
	}
	if refreshToken == "" {
		return nil, pkceErr(provider, "no refresh_token on the pooled credential.", "oauth_refresh_no_token")
	}
	data := map[string]string{"grant_type": "refresh_token", "refresh_token": refreshToken}
	if len(cfg.Scopes) > 0 {
		data["scope"] = strings.Join(cfg.Scopes, " ")
	}
	return pkcePostToken(env, provider, cfg, data, "oauth_refresh_failed")
}

// PKCEProviderProfileHooks are the auth handler and refresh hooks a plugin registers for its provider
func PKCEProviderProfileHooks(cfg OAuthPKCEConfig) ProviderProfileHooks {
	return ProviderProfileHooks{AuthHandler: PKCEAuthHandler(cfg), RefreshCredential: PKCERefreshCredential(cfg)}
}

package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

func openRouterErr(message, code string, relogin bool) *Error {
	return &Error{Message: message, Provider: ProviderOpenRoute, Code: code, Relogin: relogin}
}

func (s *Store) openRouterExchangeCode(ctx context.Context, code, verifier string, timeout time.Duration) (Secret, error) {
	payload := newDoc()
	payload.set("code", code)
	payload.set("code_verifier", verifier)
	payload.set("code_challenge_method", "S256")
	r, err := s.postJSON(ctx, nil, OpenRouterAuthKeysURL, payload, nil, timeout)
	if err != nil {
		return Secret{}, &Error{Message: "OpenRouter code exchange failed: " + transportFailure(err), Provider: ProviderOpenRoute, Code: "openrouter_token_exchange_failed", Cause: err}
	}
	if r.status == 403 {
		return Secret{}, openRouterErr("OpenRouter rejected the authorization code (invalid, already used, or older than 10 minutes). "+
			"Run the login again.", "openrouter_token_exchange_denied", true)
	}
	if r.status >= 400 {
		message := fmt.Sprintf("OpenRouter code exchange failed (HTTP %d).", r.status)
		detail := strings.TrimSpace(wireMask(string(r.body)))
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		if detail != "" {
			message += " Response: " + detail
		}
		return Secret{}, openRouterErr(message, "openrouter_token_exchange_failed", false)
	}
	parsed, perr := r.value()
	if perr != nil {
		return Secret{}, openRouterErr("OpenRouter code exchange returned a non-JSON body.", "openrouter_token_exchange_invalid", false)
	}
	body, _ := parsed.(*doc)
	key := stripped(body.value("key"))
	if key == "" {
		return Secret{}, openRouterErr("OpenRouter code exchange response did not include a 'key'.", "openrouter_token_exchange_invalid", false)
	}
	return NewSecret(key), nil
}

func (s *Store) OpenRouterLogin(ctx context.Context, c Console, o LoginOptions) (Secret, error) {
	ctx = s.bind(ctx)
	verifier, err := pkceCodeVerifier()
	if err != nil {
		return Secret{}, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	challenge := pkceCodeChallenge(verifier)
	code := ""
	if s.isRemoteSession(ctx) {
		headless := formOf("code_challenge", challenge, "code_challenge_method", "S256", "key_label", "icebath")
		c.printf("Remote session detected — using OpenRouter's headless flow.\n"+
			"Open this URL in a browser on any machine, authorize, then paste the code shown:\n  %s\n\n", OpenRouterAuthURL+"?"+encodeForm(headless))
		if c.Secret == nil {
			return Secret{}, errors.New("auth: the console cannot read a masked authorization code")
		}
		entered, rerr := c.Secret("Authorization code: ")
		if rerr != nil {
			return Secret{}, rerr
		}
		if code = strings.TrimSpace(entered); code == "" {
			return Secret{}, openRouterErr("No authorization code entered.", "openrouter_auth_no_code", false)
		}
	} else {
		nonce, nerr := randomURLToken(16)
		if nerr != nil {
			return Secret{}, nerr
		}
		path := "/callback/" + nonce
		server, berr := s.bindLoopback("127.0.0.1", 0, path, "OpenRouter", ProviderOpenRoute, "openrouter_callback_bind_failed")
		if berr != nil {
			return Secret{}, berr
		}
		redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", server.port(), path)
		authURL := OpenRouterAuthURL + "?" + encodeForm(formOf("callback_url", redirectURI, "code_challenge", challenge, "code_challenge_method", "S256"))
		c.printf("Open this URL to authorize icebath with OpenRouter:\n  %s\n\nDocs: %s\n", authURL, OpenRouterOAuthDocsURL)
		if !o.NoBrowser && s.canOpenGraphicalBrowser(ctx) {
			if s.openBrowser(ctx, authURL) {
				c.println("Browser opened for OpenRouter authorization.")
			} else {
				c.println("Could not open the browser automatically; use the URL above.")
			}
		}
		c.println("Waiting for the OpenRouter callback...")
		callback, werr := server.wait(ctx, timeout, ProviderOpenRoute, "openrouter_callback_timeout")
		if werr != nil {
			return Secret{}, werr
		}
		if callback.failure != "" {
			detail := callback.errorDescription
			if detail == "" {
				detail = callback.failure
			}
			return Secret{}, openRouterErr("OpenRouter authorization failed: "+detail, "openrouter_auth_denied", false)
		}
		if code = strings.TrimSpace(callback.code); code == "" {
			return Secret{}, openRouterErr("OpenRouter callback did not carry an authorization code.", "openrouter_auth_no_code", false)
		}
	}
	c.println("Exchanging the authorization code for an OpenRouter API key...")
	return s.openRouterExchangeCode(ctx, code, verifier, 20*time.Second)
}

const pkcePoolSource = "manual:loopback_pkce"

type PKCEConfig struct {
	ClientID             string
	AuthorizeURL         string
	TokenURL             string
	Scopes               []string
	RedirectPort         int
	RedirectPath         string
	Audience             string
	ExtraAuthorizeParams [][2]string
	ExtraTokenParams     [][2]string
	AllowedHosts         []string
	Timeout              time.Duration
	Label                string
	TokenRequest         func(ctx context.Context, grant [][2]string) (json.RawMessage, error)
}

func formPairs(form *doc) [][2]string {
	out := make([][2]string, 0, form.size())
	for _, k := range form.keys {
		v, _ := form.vals[k].(string)
		out = append(out, [2]string{k, v})
	}
	return out
}

func pkceErr(provider, message, code string) *Error {
	return &Error{Message: provider + ": " + message, Provider: provider, Code: code}
}

func pkceEndpointHost(provider, name, raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	host := ""
	if err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	if host == "" {
		return "", pkceErr(provider, "OAuth "+name+" has no host.", "oauth_endpoint_invalid")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (host == "127.0.0.1" || host == "::1")) {
		return "", pkceErr(provider, "OAuth "+name+" must use HTTPS.", "oauth_endpoint_invalid")
	}
	return host, nil
}

func ValidatePKCEConfig(provider string, cfg PKCEConfig) error {
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
	allowlist := []string{}
	for _, h := range cfg.AllowedHosts {
		allowlist = append(allowlist, strings.ToLower(h))
	}
	if len(allowlist) == 0 {
		allowlist = []string{authorizeHost}
	}
	if !slices.ContainsFunc(allowlist, func(apex string) bool { return tokenHost == apex || strings.HasSuffix(tokenHost, "."+apex) }) {
		sort.Strings(allowlist)
		return pkceErr(provider, fmt.Sprintf("OAuth token_url host '%s' is not on the allowlist ['%s'].", tokenHost, strings.Join(allowlist, "', '")), "oauth_token_host_rejected")
	}
	if cfg.RedirectPort < 0 || cfg.RedirectPort > 65535 {
		return pkceErr(provider, "OAuth redirect_port must be within 0..65535.", "oauth_redirect_invalid")
	}
	return nil
}

type pkceTokens struct {
	access      string
	refresh     string
	expiresAtMS int64
	lastRefresh string
}

func pkceTokenFields(provider string, payload *doc, grant *doc, code string) (pkceTokens, error) {
	access := stripped(payload.value("access_token"))
	if access == "" {
		return pkceTokens{}, pkceErr(provider, "OAuth token response carried no access_token.", code)
	}
	refreshRaw := payload.value("refresh_token")
	if !truthy(refreshRaw) {
		refreshRaw = grant.value("refresh_token")
	}
	out := pkceTokens{access: access, refresh: stripped(refreshRaw), lastRefresh: isoStampZ(time.Now())}
	if ttl := coerceTTL(payload.value("expires_in")); ttl > 0 {
		out.expiresAtMS = time.Now().UnixMilli() + ttl*1000
	}
	return out, nil
}

func (s *Store) pkcePostToken(ctx context.Context, provider string, cfg PKCEConfig, grant *doc, code string) (pkceTokens, error) {
	if cfg.TokenRequest != nil {
		raw, err := cfg.TokenRequest(ctx, formPairs(grant))
		if err != nil {
			return pkceTokens{}, err
		}
		payload, perr := parseObject(raw)
		if perr != nil {
			return pkceTokens{}, perr
		}
		return pkceTokenFields(provider, payload, grant, code)
	}
	form := newDoc()
	for _, kv := range cfg.ExtraTokenParams {
		form.set(kv[0], kv[1])
	}
	for _, k := range grant.keys {
		form.set(k, grant.vals[k])
	}
	form.set("client_id", cfg.ClientID)
	if cfg.Audience != "" && !form.has("audience") {
		form.set("audience", cfg.Audience)
	}
	r, err := s.postForm(ctx, nil, cfg.TokenURL, form, nil, 30*time.Second)
	if err != nil {
		e := pkceErr(provider, "OAuth token request failed: "+transportFailure(err), code)
		e.Cause = err
		return pkceTokens{}, e
	}
	if r.status >= 400 {
		oauthCode := ""
		if payload, perr := r.object(); perr == nil {
			oauthCode = stripped(payload.value("error"))
		}
		if slices.Contains(oauthGrantDeadCodes, oauthCode) {
			return pkceTokens{}, pkceErr(provider, fmt.Sprintf("OAuth token request failed with HTTP %d (%s).", r.status, oauthCode), oauthCode)
		}
		return pkceTokens{}, pkceErr(provider, fmt.Sprintf("OAuth token request failed with HTTP %d.", r.status), code)
	}
	payload, perr := r.object()
	if perr != nil {
		return pkceTokens{}, perr
	}
	return pkceTokenFields(provider, payload, grant, code)
}

func (s *Store) pkceLogin(ctx context.Context, c Console, provider string, cfg PKCEConfig, openBrowser bool) (pkceTokens, error) {
	if err := ValidatePKCEConfig(provider, cfg); err != nil {
		return pkceTokens{}, err
	}
	path := cfg.RedirectPath
	if path == "" {
		path = "/callback"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	label := cfg.Label
	if label == "" {
		label = provider
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	server, err := s.bindLoopback("127.0.0.1", cfg.RedirectPort, path, label, provider, "oauth_callback_bind_failed")
	if err != nil {
		err.(*Error).Message = provider + ": " + err.(*Error).Message
		return pkceTokens{}, err
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", server.port(), path)
	verifier, err := pkceCodeVerifier()
	if err != nil {
		return pkceTokens{}, err
	}
	state, err := randomURLToken(32)
	if err != nil {
		return pkceTokens{}, err
	}
	params := newDoc()
	for _, kv := range cfg.ExtraAuthorizeParams {
		params.set(kv[0], kv[1])
	}
	params.set("client_id", cfg.ClientID)
	params.set("response_type", "code")
	params.set("redirect_uri", redirectURI)
	params.set("state", state)
	params.set("code_challenge", pkceCodeChallenge(verifier))
	params.set("code_challenge_method", "S256")
	if len(cfg.Scopes) > 0 {
		params.set("scope", strings.Join(cfg.Scopes, " "))
	}
	if cfg.Audience != "" {
		params.set("audience", cfg.Audience)
	}
	separator := "?"
	if parsed, perr := url.Parse(cfg.AuthorizeURL); perr == nil && parsed.RawQuery != "" {
		separator = "&"
	}
	authorizeURL := cfg.AuthorizeURL + separator + encodeForm(params)
	c.printf("\nOpen this URL to authorize icebath with %s:\n  %s\n\n", label, authorizeURL)
	c.printf("Waiting for callback on %s (timeout %ds, Ctrl+C to cancel)...\n", redirectURI, int(timeout.Seconds()))
	s.printLoopbackSSHHint(ctx, c, redirectURI, "")
	if openBrowser && s.canOpenGraphicalBrowser(ctx) {
		if _, err := s.tryOpenBrowser(ctx, authorizeURL); err != nil {
			c.println("Could not open the browser automatically; use the URL above.")
		}
	}
	callback, err := server.wait(ctx, timeout, provider, "oauth_callback_timeout")
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			c.println("\nLogin cancelled.")
		}
		var ae *Error
		if errors.As(err, &ae) {
			ae.Message = provider + ": " + ae.Message
		}
		return pkceTokens{}, err
	}
	if callback.failure != "" {
		detail := callback.errorDescription
		if detail == "" {
			detail = callback.failure
		}
		return pkceTokens{}, pkceErr(provider, "authorization failed: "+detail, "oauth_authorization_denied")
	}
	if subtle.ConstantTimeCompare([]byte(callback.state), []byte(state)) != 1 {
		return pkceTokens{}, pkceErr(provider, "callback state mismatch — the redirect did not come from this login. Aborting.", "oauth_state_mismatch")
	}
	code := strings.TrimSpace(callback.code)
	if code == "" {
		return pkceTokens{}, pkceErr(provider, "callback carried no authorization code.", "oauth_no_code")
	}
	return s.pkcePostToken(ctx, provider, cfg, formOf(
		"grant_type", "authorization_code", "code", code, "redirect_uri", redirectURI, "code_verifier", verifier,
	), "oauth_token_exchange_failed")
}

func pkceUsable(access string, expiresAtMS, nowMS int64) bool {
	return strings.TrimSpace(access) != "" && (expiresAtMS == 0 || expiresAtMS > nowMS)
}

func (s *Store) PKCEAuth(ctx context.Context, c Console, provider string, cfg PKCEConfig, action string, noBrowser bool) (bool, error) {
	ctx = s.bind(ctx)
	label := cfg.Label
	if label == "" {
		label = provider
	}
	if action != "add" && action != "status" && action != "logout" {
		return false, nil
	}
	var tokens pkceTokens
	if action == "add" {
		var err error
		if tokens, err = s.pkceLogin(ctx, c, provider, cfg, !noBrowser); err != nil {
			return false, err
		}
	}
	pool, err := s.Pool(ctx, provider)
	if err != nil {
		return false, err
	}
	switch action {
	case "add":
		entry := Entry{Provider: provider, ID: shortID(), Label: label, AuthType: AuthTypeOAuth, Source: pkcePoolSource,
			AccessToken: NewSecret(tokens.access), RefreshToken: NewSecret(tokens.refresh), ExpiresAtMS: tokens.expiresAtMS, LastRefresh: tokens.lastRefresh}
		meta := newDoc()
		meta.set("client_id", cfg.ClientID)
		meta.set("scope", strings.Join(cfg.Scopes, " "))
		entry.setExtra("oauth_pkce", meta)
		added, err := pool.Add(ctx, entry)
		if err != nil {
			return false, err
		}
		c.printf("Signed in to %s; credential %s added to the pool.\n", label, added.ID)
		return true, nil
	case "status":
		entries := pool.Entries()
		nowMS := time.Now().UnixMilli()
		switch {
		case len(entries) == 0:
			c.printf("%s: logged out\n", provider)
		case slices.ContainsFunc(entries, func(e Entry) bool { return pkceUsable(e.AccessToken.Reveal(), e.ExpiresAtMS, nowMS) }):
			c.printf("%s: logged in\n  auth_type: oauth (pkce)\n  credentials: %d\n", provider, len(entries))
		default:
			c.printf("%s: expired (needs refresh) — run `icebath auth refresh %s`\n", provider, provider)
		}
		return true, nil
	case "logout":
		count := len(pool.Entries())
		for index := count; index > 0; index-- {
			if _, _, err := pool.RemoveIndex(ctx, index); err != nil {
				return false, err
			}
		}
		c.printf("Logged out of %s (%d credential(s) removed)\n", provider, count)
		return true, nil
	}
	return false, nil
}

type pkceRefresher struct {
	s   *Store
	cfg PKCEConfig
}

func (s *Store) PKCERefresher(cfg PKCEConfig) Refresher { return pkceRefresher{s: s, cfg: cfg} }

func (p pkceRefresher) Refresh(ctx context.Context, r RefreshRequest) (Entry, error) {
	entry := r.Entry
	provider := entry.Provider
	if err := ValidatePKCEConfig(provider, p.cfg); err != nil {
		return Entry{}, err
	}
	ctx = p.s.bind(ctx)
	ctx, unlock, err := p.s.lock(ctx, p.s.authPath, LockTimeout)
	if err != nil {
		return Entry{}, err
	}
	defer unlock()
	rows, err := p.s.readPool(ctx, provider)
	if err != nil {
		return Entry{}, err
	}
	var disk *doc
	for _, row := range rows {
		if rowID(row) == entry.ID {
			disk = row
			break
		}
	}
	diskRefresh := stripped(disk.value("refresh_token"))
	diskExpiry, _ := intOf(disk.value("expires_at_ms"))
	if diskRefresh != "" && diskRefresh != entry.RefreshToken.Reveal() && pkceUsable(textOf(disk.value("access_token")), diskExpiry, time.Now().UnixMilli()) {
		slog.Debug(fmt.Sprintf("%s entry %s: adopting a peer's rotation, refresh token not spent", provider, entry.ID))
		entry.AccessToken = NewSecret(textOf(disk.value("access_token")))
		entry.RefreshToken = NewSecret(diskRefresh)
		entry.ExpiresAtMS = diskExpiry
		entry.LastRefresh = disk.str("last_refresh")
		return entry, nil
	}
	refreshToken := diskRefresh
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(entry.RefreshToken.Reveal())
	}
	if refreshToken == "" {
		return Entry{}, pkceErr(provider, "no refresh_token on the pooled credential.", "oauth_refresh_no_token")
	}
	grant := formOf("grant_type", "refresh_token", "refresh_token", refreshToken)
	if len(p.cfg.Scopes) > 0 {
		grant.set("scope", strings.Join(p.cfg.Scopes, " "))
	}
	tokens, err := p.s.pkcePostToken(ctx, provider, p.cfg, grant, "oauth_refresh_failed")
	if err != nil {
		return Entry{}, err
	}
	entry.AccessToken, entry.RefreshToken = NewSecret(tokens.access), NewSecret(tokens.refresh)
	entry.ExpiresAtMS, entry.LastRefresh = tokens.expiresAtMS, tokens.lastRefresh
	return entry, nil
}

package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	codexMissingAccessMsg       = "Codex auth is missing access_token. Run `%s` to re-authenticate."
	codexMissingRefreshMsg      = "Codex auth is missing refresh_token. Run `%s` to re-authenticate."
	codexNoCredentialsMsg       = "No Codex credentials stored. Run `%s` to authenticate."
	codexAuthBodyMaxBytes       = 1024 * 1024
	codexQuotaProbeMinInterval  = 300 * time.Second
	codexOAuthAuthorizeURL      = "https://auth.openai.com/oauth/authorize"
	codexOAuthBrowserScope      = "openid profile email offline_access"
	codexBrowserCallbackPort    = 1455
	codexBrowserCallbackPath    = "/auth/callback"
	codexBrowserCallbackTimeout = 300 * time.Second
	codexBrowserPortBusyCode    = "codex_browser_port_busy"
	codexIssuer                 = "https://auth.openai.com"
)

type codexProbe struct {
	at     time.Time
	result *bool
}

func codexErr(message, code string, relogin bool) *Error {
	return &Error{Message: message, Provider: ProviderCodex, Code: code, Relogin: relogin}
}

func retryAfterSeconds(h http.Header) (int, bool) {
	d, ok := httpclient.RetryAfter(h, time.Now())
	if !ok {
		return 0, false
	}
	return int(d.Seconds()), true
}

func stripped(v any) string { return strings.TrimSpace(textOf(v)) }

func clearPoolEntryStatus(entry *doc) {
	for _, f := range poolStatusFields {
		entry.set(f, nil)
	}
}

func codexAccessTokenIsExpiring(token string, skew time.Duration) bool {
	exp, ok := numberOf(decodeJWTClaims(token).value("exp"))
	return ok && exp <= nowEpoch()+max(0, skew.Seconds())
}

func (s *Store) codexBaseURL(ctx context.Context) string {
	if v := strings.TrimRight(strings.TrimSpace(s.lookupEnv(ctx, "ICEBATH_CODEX_BASE_URL")), "/"); v != "" {
		return v
	}
	return DefaultCodexBaseURL
}

func (s *Store) codexPoolRouteBaseURL(ctx context.Context, entryBaseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(entryBaseURL), "/")
	if override := strings.TrimRight(strings.TrimSpace(s.lookupEnv(ctx, "ICEBATH_CODEX_BASE_URL")), "/"); override != "" {
		return override
	}
	if base == "" || base == DefaultCodexBaseURL {
		if cfg, err := s.cfg.LoadReadonly(ctx); err == nil && !cfg.Model.Bare && strings.ToLower(strings.TrimSpace(cfg.Model.Provider)) == ProviderCodex {
			if configured := strings.TrimRight(strings.TrimSpace(cfg.Model.BaseURL), "/"); configured != "" {
				base = configured
			}
		}
	}
	if base == "" {
		return DefaultCodexBaseURL
	}
	return base
}

type RuntimeCredentials struct {
	Provider    string
	BaseURL     string
	APIKey      Secret
	Tokens      TokenSource
	Source      string
	LastRefresh string
	AuthMode    string
	ExpiresAtMS int64
	AuthFile    string
}

func (s *Store) codexRuntime(ctx context.Context, key, source, lastRefresh, baseURL string) RuntimeCredentials {
	if baseURL == "" {
		baseURL = s.codexBaseURL(ctx)
	}
	return RuntimeCredentials{Provider: ProviderCodex, BaseURL: baseURL, APIKey: NewSecret(key), Source: source, LastRefresh: lastRefresh, AuthMode: "chatgpt"}
}

func (s *Store) readCodexTokens(ctx context.Context, locked bool) (*doc, string, error) {
	if locked {
		lctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
		if err != nil {
			return nil, "", err
		}
		defer unlock()
		ctx = lctx
	}
	store, err := s.load(s.authPath)
	if err != nil {
		return nil, "", err
	}
	state := s.loadState(store, ProviderCodex)
	relogin := s.reloginCommand(ProviderCodex)
	if state.size() == 0 {
		return nil, "", codexErr(fmt.Sprintf(codexNoCredentialsMsg, relogin), "codex_auth_missing", true)
	}
	tokens := state.obj("tokens")
	if tokens == nil {
		return nil, "", codexErr(fmt.Sprintf("Codex auth state is missing tokens. Run `%s` to re-authenticate.", relogin), "codex_auth_invalid_shape", true)
	}
	if strings.TrimSpace(tokens.str("access_token")) == "" {
		return nil, "", codexErr(fmt.Sprintf(codexMissingAccessMsg, relogin), "codex_auth_missing_access_token", true)
	}
	if strings.TrimSpace(tokens.str("refresh_token")) == "" {
		return nil, "", codexErr(fmt.Sprintf(codexMissingRefreshMsg, relogin), "codex_auth_missing_refresh_token", true)
	}
	return tokens, state.str("last_refresh"), nil
}

func syncCodexPoolEntries(store, tokens *doc, lastRefresh string, previous *doc) {
	access := tokens.value("access_token")
	if !truthy(access) {
		return
	}
	entries, ok := store.obj("credential_pool").value(ProviderCodex).([]any)
	if !ok {
		return
	}
	prev := previous.value("access_token")
	for _, v := range entries {
		entry := asRow(v)
		if entry == nil {
			continue
		}
		source := entry.str("source")
		alias := source == SourceManualDeviceCode && truthy(prev) && valueEqual(entry.value("access_token"), prev)
		if source != "device_code" && !alias {
			continue
		}
		entry.set("access_token", access)
		if refresh := tokens.value("refresh_token"); truthy(refresh) {
			entry.set("refresh_token", refresh)
		}
		if lastRefresh != "" {
			entry.set("last_refresh", lastRefresh)
		}
		clearPoolEntryStatus(entry)
	}
}

func (s *Store) saveCodexTokens(ctx context.Context, tokens *doc, lastRefresh, label string, setActive, writeThrough bool) error {
	if lastRefresh == "" {
		lastRefresh = isoStampZ(time.Now())
	}
	return s.transact(ctx, ProviderCodex, LockTimeout, func(tx *stateTx) error {
		state := tx.state.clone()
		if state == nil {
			state = newDoc()
		}
		previous := state.obj("tokens")
		state.set("tokens", tokens)
		state.set("last_refresh", lastRefresh)
		state.set("auth_mode", "chatgpt")
		if strings.TrimSpace(label) != "" {
			state.set("label", strings.TrimSpace(label))
		}
		target, targetPath, active := tx.store, s.authPath, setActive
		if writeThrough && tx.source != "" && !samePath(tx.source, s.authPath) {
			rootStore, err := s.load(tx.source)
			if err != nil {
				return err
			}
			target, targetPath, active = rootStore, tx.source, false
		}
		if err := s.storeProviderState(tx.ctx, target, ProviderCodex, state, active); err != nil {
			return err
		}
		syncCodexPoolEntries(target, tokens, lastRefresh, previous)
		return s.save(tx.ctx, target, targetPath)
	})
}

func codexPrincipalIdentity(accessToken string) (account, subject string, ok bool) {
	claims := decodeJWTClaims(accessToken)
	account = strings.TrimSpace(claims.obj("https://api.openai.com/auth").str("chatgpt_account_id"))
	subject = strings.TrimSpace(claims.str("sub"))
	return account, subject, account != "" && subject != ""
}

func (s *Store) importCodexCLITokens(ctx context.Context) *doc {
	codexHome := strings.TrimSpace(s.lookupEnv(ctx, "CODEX_HOME"))
	if codexHome == "" {
		userHome, err := home.UserHome()
		if err != nil {
			return nil
		}
		codexHome = filepath.Join(userHome, ".codex")
	}
	authPath := filepath.Join(home.ExpandUser(codexHome), "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		return nil
	}
	parsed, err := parseObject(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil
	}
	tokens := parsed.obj("tokens")
	if tokens == nil || !truthy(tokens.value("access_token")) || !truthy(tokens.value("refresh_token")) {
		return nil
	}
	if codexAccessTokenIsExpiring(tokens.str("access_token"), 0) {
		slog.Debug(fmt.Sprintf("Codex CLI tokens at %s are expired — not importing.", authPath))
		return nil
	}
	return tokens.clone()
}

func (s *Store) recoverCodexTokensFromCLI(ctx context.Context, reason, observed string) (*doc, error) {
	if !s.adoptExternalLogins(ctx) {
		return nil, nil
	}
	imported := s.importCodexCLITokens(ctx)
	if imported == nil || stripped(imported.value("access_token")) == "" || stripped(imported.value("refresh_token")) == "" {
		return nil, nil
	}
	observed = strings.TrimSpace(observed)
	var adopted *doc
	err := s.transact(ctx, ProviderCodex, LockTimeout, func(tx *stateTx) error {
		if stripped(tx.state.obj("tokens").value("access_token")) != observed {
			slog.Info(fmt.Sprintf("Codex CLI recovery not applied (%s): the credential was re-authenticated meanwhile.", reason))
			return nil
		}
		knownAccount, knownSubject, known := codexPrincipalIdentity(observed)
		if account, subject, ok := codexPrincipalIdentity(imported.str("access_token")); known && ok && (account != knownAccount || subject != knownSubject) {
			slog.Warn(fmt.Sprintf("Codex CLI recovery refused (%s): the Codex CLI login belongs to a different ChatGPT "+
				"workspace than the icebath credential. Run `%s` to re-authenticate it.", reason, s.reloginCommand(ProviderCodex)))
			return nil
		}
		slog.Info(fmt.Sprintf("Codex auth recovered from Codex CLI auth.json (%s).", reason))
		if err := s.saveCodexTokens(tx.ctx, imported, "", "", true, false); err != nil {
			return err
		}
		adopted = imported.clone()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return adopted, nil
}

func tlsInteropHint(err error) string {
	text := err.Error()
	var he *httpclient.Error
	tlsPhase := errors.As(err, &he) && he.Phase == httpclient.PhaseTLS
	if !tlsPhase && !strings.Contains(text, "tls:") && !strings.Contains(text, "UNEXPECTED_EOF") {
		return ""
	}
	return " This looks like a TLS handshake failure rather than a Codex outage: some networks reject" +
		" the larger TLS 1.3 ClientHello sent by default (post-quantum hybrid" +
		" groups). Workaround: restrict the key exchange groups to classic curves" +
		" (x25519:secp256r1:secp384r1:x448), or test with TLS 1.2 — see the Codex note in" +
		" https://icebath.app/docs/integrations/providers"
}

func (s *Store) codexExchange(ctx context.Context, method, endpoint string, header map[string]string, body []byte, timeout time.Duration) (reply, error) {
	r, err := s.exchange(ctx, nil, method, endpoint, header, body, timeout)
	if err != nil {
		return r, err
	}
	if len(r.body) > codexAuthBodyMaxBytes {
		host := endpoint
		if parsed, perr := url.Parse(endpoint); perr == nil {
			host = parsed.Hostname()
		}
		return reply{}, codexErr(fmt.Sprintf("Codex auth response from %s exceeded %d KiB; refusing to parse it.", host, codexAuthBodyMaxBytes/1024),
			"codex_auth_response_too_large", false)
	}
	return r, nil
}

func (s *Store) codexLoginPost(ctx context.Context, endpoint string, header map[string]string, body []byte, failure, code string) (reply, error) {
	for attempt := 1; ; attempt++ {
		r, err := s.codexExchange(ctx, http.MethodPost, endpoint, header, body, 15*time.Second)
		if err == nil {
			return r, nil
		}
		if attempt == 3 || !IsNetworkError(err) {
			detail := transportFailure(err)
			var ae *Error
			if errors.As(err, &ae) {
				detail = ae.Message
			}
			return r, &Error{Message: fmt.Sprintf("%s: %s%s", failure, detail, tlsInteropHint(err)), Provider: ProviderCodex, Code: code, Cause: err}
		}
		if serr := cancellableSleep(ctx, time.Duration(attempt)*time.Second, nil); serr != nil {
			return r, serr
		}
	}
}

func codexQuotaExhaustedError(retryAfter int, known bool) *Error {
	message := "Codex provider quota exhausted (429). Credentials are still valid; retry after the usage limit resets."
	e := codexErr(message, CodexRateLimitedCode, false)
	if known {
		e.Message = fmt.Sprintf("Codex provider quota exhausted (429); retry after %ds. Credentials are still valid.", retryAfter)
		e.RetryAfter = time.Duration(retryAfter) * time.Second
	}
	retryable := true
	e.Retryable = &retryable
	return e
}

func (s *Store) codexRefreshFailureError(r reply) *Error {
	code := "codex_refresh_failed"
	message := fmt.Sprintf("Codex token refresh failed with status %d.", r.status)
	if payload, err := r.object(); err == nil {
		if nested := payload.obj("error"); nested != nil {
			nestedCode := nested.value("code")
			if !truthy(nestedCode) {
				nestedCode = nested.value("type")
			}
			if c, ok := nestedCode.(string); ok && strings.TrimSpace(c) != "" {
				code = strings.TrimSpace(c)
			}
			if m := strings.TrimSpace(nested.str("message")); m != "" {
				message = "Codex token refresh failed: " + m
			}
		} else if c := strings.TrimSpace(payload.str("error")); c != "" {
			code = c
			desc := payload.value("error_description")
			if !truthy(desc) {
				desc = payload.value("message")
			}
			if d, ok := desc.(string); ok && strings.TrimSpace(d) != "" {
				message = "Codex token refresh failed: " + strings.TrimSpace(d)
			}
		}
	}
	if code == "refresh_token_reused" {
		message = "Codex refresh token was already consumed by another client (e.g. Codex CLI or VS Code extension). " +
			"Run `codex` in your terminal to generate fresh tokens, " +
			fmt.Sprintf("then run `%s` to re-authenticate.", s.reloginCommand(ProviderCodex))
	}
	relogin := code == "invalid_grant" || code == "invalid_token" || code == "invalid_request" || code == "refresh_token_reused" ||
		r.status == 401 || r.status == 403
	return codexErr(message, code, relogin)
}

type rotatedPair struct {
	access      string
	refresh     string
	lastRefresh string
}

func (s *Store) refreshCodexPure(ctx context.Context, refreshToken string, timeout time.Duration) (rotatedPair, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return rotatedPair{}, codexErr(fmt.Sprintf(codexMissingRefreshMsg, s.reloginCommand(ProviderCodex)), "codex_auth_missing_refresh_token", true)
	}
	form := formOf("grant_type", "refresh_token", "refresh_token", refreshToken, "client_id", CodexOAuthClientID)
	r, err := s.codexExchange(ctx, http.MethodPost, CodexOAuthTokenURL, map[string]string{
		"Accept": "application/json", "User-Agent": s.userAgent(), "Content-Type": "application/x-www-form-urlencoded",
	}, []byte(encodeForm(form)), max(5*time.Second, timeout))
	if err != nil {
		return rotatedPair{}, err
	}
	if r.status == 429 {
		after, known := retryAfterSeconds(r.header)
		return rotatedPair{}, codexQuotaExhaustedError(after, known)
	}
	if r.status != 200 {
		return rotatedPair{}, s.codexRefreshFailureError(r)
	}
	parsed, perr := r.value()
	if perr != nil {
		return rotatedPair{}, &Error{Message: "Codex token refresh returned invalid JSON.", Provider: ProviderCodex, Code: "codex_refresh_invalid_json", Cause: perr}
	}
	payload, _ := parsed.(*doc)
	access := strings.TrimSpace(payload.str("access_token"))
	if access == "" {
		return rotatedPair{}, codexErr("Codex token refresh response was missing access_token.", "codex_refresh_missing_access_token", true)
	}
	out := rotatedPair{access: access, refresh: strings.TrimSpace(refreshToken), lastRefresh: isoStampZ(time.Now())}
	if next := strings.TrimSpace(payload.str("refresh_token")); next != "" {
		out.refresh = next
	}
	return out, nil
}

func (s *Store) refreshCodexAuthTokens(ctx context.Context, tokens *doc, timeout time.Duration) (*doc, error) {
	var result *doc
	err := s.transact(ctx, ProviderCodex, max(LockTimeout, timeout+5*time.Second), func(tx *stateTx) error {
		stored := tx.state.obj("tokens")
		storedAccess, storedRefresh := stripped(stored.value("access_token")), stripped(stored.value("refresh_token"))
		if storedAccess != "" && storedRefresh != "" && storedRefresh != stripped(tokens.value("refresh_token")) {
			slog.Info("Codex refresh token already rotated by a peer — adopting the stored pair.")
			result = tokens.clone()
			result.set("access_token", storedAccess)
			result.set("refresh_token", storedRefresh)
			return nil
		}
		refreshed, err := s.refreshCodexPure(tx.ctx, textOf(tokens.value("refresh_token")), timeout)
		if err != nil {
			var ae *Error
			if !errors.As(err, &ae) || !ae.Relogin {
				return err
			}
			code := ae.Code
			if code == "" {
				code = "auth_error"
			}
			imported, rerr := s.recoverCodexTokensFromCLI(tx.ctx, "refresh_token rejected: "+code, storedAccess)
			if rerr != nil {
				return rerr
			}
			if imported == nil {
				return err
			}
			result = imported
			return nil
		}
		result = tokens.clone()
		result.set("access_token", refreshed.access)
		result.set("refresh_token", refreshed.refresh)
		return s.saveCodexTokens(tx.ctx, result, "", "", true, true)
	})
	return result, err
}

type ResolveOptions struct {
	ForceRefresh     bool
	NoRefresh        bool
	RefreshSkew      time.Duration
	ReadOnly         bool
	AsTokenSource    bool
	StaleAccessToken Secret
}

func (s *Store) ResolveCodex(ctx context.Context, o ResolveOptions) (RuntimeCredentials, error) {
	ctx = s.bind(ctx)
	skew := o.RefreshSkew
	if skew == 0 {
		skew = CodexAccessTokenRefreshSkew
	}
	var readErr error
	var tokens *doc
	lastRefresh, observed := "", ""
	if o.ReadOnly {
		tokens, lastRefresh, readErr = s.readCodexTokens(ctx, false)
	} else {
		lctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
		if err != nil {
			return RuntimeCredentials{}, err
		}
		if store, lerr := s.load(s.authPath); lerr == nil {
			observed = s.loadState(store, ProviderCodex).obj("tokens").str("access_token")
		}
		tokens, lastRefresh, readErr = s.readCodexTokens(lctx, false)
		unlock()
	}
	var ae *Error
	if readErr != nil && !errors.As(readErr, &ae) {
		return RuntimeCredentials{}, readErr
	}
	if ae != nil && !o.ReadOnly && ae.Relogin && (ae.Code == "codex_auth_missing_access_token" || ae.Code == "codex_auth_missing_refresh_token" || ae.Code == "codex_auth_invalid_shape") {
		imported, rerr := s.recoverCodexTokensFromCLI(ctx, ae.Code, observed)
		if rerr != nil {
			return RuntimeCredentials{}, rerr
		}
		if imported != nil {
			tokens, lastRefresh = imported, imported.str("last_refresh")
		}
	}
	if tokens == nil {
		poolToken, poolBase := s.poolCodexCredential(ctx)
		if poolToken != "" && o.ForceRefresh && !o.ReadOnly {
			hint := NewSecret(poolToken)
			poolToken = ""
			pool, perr := s.Pool(ctx, ProviderCodex)
			if perr != nil {
				return RuntimeCredentials{}, perr
			}
			refreshed, ok, rerr := pool.TryRefreshMatching(ctx, hint, "")
			if rerr != nil {
				return RuntimeCredentials{}, rerr
			}
			if ok {
				poolToken = refreshed.RuntimeAPIKey().Reveal()
			}
		}
		if poolToken != "" {
			return s.codexRuntime(ctx, poolToken, "credential_pool", "", s.codexPoolRouteBaseURL(ctx, poolBase)), nil
		}
		if limited := s.codexPoolRateLimitStatus(ctx); limited != nil {
			if !o.ReadOnly {
				if restored, known := s.probeCodexPoolEntryQuotaRestored(ctx, limited); known && restored {
					slog.Info("Codex quota restored upstream — clearing stale pool cooldown(s).")
					s.ClearCodexPoolQuotaCooldowns(ctx, "")
					if poolToken, poolBase = s.poolCodexCredential(ctx); poolToken != "" {
						return s.codexRuntime(ctx, poolToken, "credential_pool", "", s.codexPoolRouteBaseURL(ctx, poolBase)), nil
					}
				}
			}
			if resetAt, ok := numberOf(limited.value("reset_at")); ok && resetAt > nowEpoch() {
				return RuntimeCredentials{}, codexQuotaExhaustedError(int(resetAt-nowEpoch()), true)
			}
			return RuntimeCredentials{}, codexQuotaExhaustedError(0, false)
		}
		if readErr != nil {
			return RuntimeCredentials{}, readErr
		}
		return RuntimeCredentials{}, codexErr(fmt.Sprintf(codexNoCredentialsMsg, s.reloginCommand(ProviderCodex)), "codex_auth_missing", true)
	}
	access := stripped(tokens.value("access_token"))
	timeout := seconds(s.envFloat(ctx, "ICEBATH_CODEX_REFRESH_TIMEOUT_SECONDS", 20))
	shouldRefresh := func(token string) bool {
		if o.ReadOnly {
			return false
		}
		return o.ForceRefresh || (!o.NoRefresh && codexAccessTokenIsExpiring(token, skew))
	}
	if shouldRefresh(access) {
		lctx, unlock, err := s.lock(ctx, s.authPath, max(LockTimeout, timeout+5*time.Second))
		if err != nil {
			return RuntimeCredentials{}, err
		}
		defer unlock()
		tokens, lastRefresh, err = s.readCodexTokens(lctx, false)
		if err != nil {
			return RuntimeCredentials{}, err
		}
		if shouldRefresh(stripped(tokens.value("access_token"))) {
			if tokens, err = s.refreshCodexAuthTokens(lctx, tokens, timeout); err != nil {
				return RuntimeCredentials{}, err
			}
		}
		access = stripped(tokens.value("access_token"))
	}
	return s.codexRuntime(ctx, access, "icebath-auth-store", lastRefresh, ""), nil
}

func isCodexRateLimitShaped(code int, reason, message string) bool {
	reason, message = strings.ToLower(reason), strings.ToLower(message)
	if code == 429 {
		return true
	}
	for _, k := range []string{"rate_limit", "usage_limit", "quota"} {
		if strings.Contains(reason, k) {
			return true
		}
	}
	for _, k := range []string{"rate limit", "usage limit", "quota"} {
		if strings.Contains(message, k) {
			return true
		}
	}
	return false
}

func rowIsRateLimitExhausted(row *doc) bool {
	code, _ := intOf(row.value("last_error_code"))
	return row.str("last_status") == StatusExhausted && isCodexRateLimitShaped(int(code), textOf(row.value("last_error_reason")), textOf(row.value("last_error_message")))
}

func codexProbeKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

func (s *Store) codexUsageProbeURL(ctx context.Context, baseURL string) string {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		normalized = s.codexBaseURL(ctx)
	}
	normalized = strings.TrimSuffix(normalized, "/codex")
	if strings.Contains(normalized, "/backend-api") {
		return normalized + "/wham/usage"
	}
	return normalized + "/api/codex/usage"
}

func CodexAccountHeaders(accessToken string) map[string]string {
	headers := map[string]string{}
	auth := decodeJWTClaims(strings.TrimSpace(accessToken)).obj("https://api.openai.com/auth")
	if account := auth.str("chatgpt_account_id"); account != "" {
		headers["ChatGPT-Account-ID"] = account
	}
	residency := auth.str("chatgpt_data_residency")
	if residency == "" {
		residency = auth.str("chatgpt_compute_residency")
	}
	if residency != "" {
		headers["x-openai-internal-codex-residency"] = residency
	}
	return headers
}

func (s *Store) probeCodexQuotaRestored(ctx context.Context, accessToken, baseURL string) (restored, known bool) {
	token := strings.TrimSpace(accessToken)
	if token == "" || decodeJWTClaims(token).size() == 0 {
		return false, false
	}
	key := codexProbeKey(token)
	now := time.Now()
	s.mu.Lock()
	if cached, ok := s.codexProbes[key]; ok && now.Sub(cached.at) < codexQuotaProbeMinInterval {
		s.mu.Unlock()
		if cached.result == nil {
			return false, false
		}
		return *cached.result, true
	}
	s.codexProbes[key] = codexProbe{at: now}
	s.mu.Unlock()
	var result *bool
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "User-Agent": "codex-cli"}
	for k, v := range CodexAccountHeaders(token) {
		headers[k] = v
	}
	r, err := s.codexExchange(ctx, http.MethodGet, s.codexUsageProbeURL(ctx, baseURL), headers, nil, 10*time.Second)
	switch {
	case err != nil:
		slog.Debug("Codex quota probe failed", "error", transportFailure(err))
	case r.status == 200:
		if payload, perr := r.object(); perr == nil {
			windows := []*doc{payload.obj("rate_limit")}
			for _, extra := range payload.list("additional_rate_limits") {
				windows = append(windows, asRow(extra).obj("rate_limit"))
			}
			worst, seen := 0.0, false
			for _, w := range windows {
				for _, k := range []string{"primary_window", "secondary_window"} {
					if used, ok := numberOf(w.obj(k).value("used_percent")); ok {
						worst, seen = max(worst, used), true
					}
				}
			}
			if seen {
				ok := worst < 100.0
				result = &ok
			}
		}
	case r.status == 429:
		no := false
		result = &no
	}
	s.mu.Lock()
	s.codexProbes[key] = codexProbe{at: now, result: result}
	s.mu.Unlock()
	if result == nil {
		return false, false
	}
	return *result, true
}

func (s *Store) refreshExpiredCodexProbeToken(ctx context.Context, accessToken, refreshToken string) (rotatedPair, bool) {
	token, refresh := strings.TrimSpace(accessToken), strings.TrimSpace(refreshToken)
	if token == "" || refresh == "" || !codexAccessTokenIsExpiring(token, 0) {
		return rotatedPair{}, false
	}
	key := codexProbeKey(token)
	now := time.Now()
	s.mu.Lock()
	cached, ok := s.codexProbes[key]
	s.mu.Unlock()
	if ok && now.Sub(cached.at) < codexQuotaProbeMinInterval {
		return rotatedPair{}, false
	}
	fresh, err := s.refreshCodexPure(ctx, refresh, 20*time.Second)
	if err != nil {
		slog.Debug("Codex pre-probe token refresh failed", "error", transportFailure(err))
		s.mu.Lock()
		s.codexProbes[key] = codexProbe{at: now}
		s.mu.Unlock()
		return rotatedPair{}, false
	}
	return fresh, true
}

func (s *Store) probeCodexPoolEntryQuotaRestored(ctx context.Context, row *doc) (restored, known bool) {
	token := stripped(row.value("access_token"))
	if fresh, ok := s.refreshExpiredCodexProbeToken(ctx, token, textOf(row.value("refresh_token"))); ok {
		token = fresh.access
		func() {
			lctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
			if err != nil {
				slog.Debug("Failed to persist refreshed Codex pool tokens", "error", err)
				return
			}
			defer unlock()
			store, err := s.load(s.authPath)
			if err != nil {
				return
			}
			for _, v := range store.obj("credential_pool").list(ProviderCodex) {
				if disk := asRow(v); disk != nil && valueEqual(disk.value("id"), row.value("id")) {
					disk.set("access_token", fresh.access)
					disk.set("refresh_token", fresh.refresh)
					disk.set("last_refresh", fresh.lastRefresh)
					if err := s.save(lctx, store, s.authPath); err != nil {
						slog.Debug("Failed to persist refreshed Codex pool tokens", "error", err)
					}
					return
				}
			}
		}()
	}
	if token == "" {
		return false, false
	}
	return s.probeCodexQuotaRestored(ctx, token, s.codexPoolRouteBaseURL(ctx, textOf(row.value("base_url"))))
}

func (s *Store) ClearCodexPoolQuotaCooldowns(ctx context.Context, accessToken string) int {
	ctx = s.bind(ctx)
	target := s.authPath
	if !s.profileOwnsPoolProvider(ProviderCodex) && s.rootAuth != "" {
		target = s.rootAuth
	}
	lctx, unlock, err := s.lock(ctx, target, LockTimeout)
	if err != nil {
		slog.Debug("Failed to clear Codex pool quota cooldowns", "error", err)
		return 0
	}
	defer unlock()
	store, err := s.load(target)
	if err != nil {
		return 0
	}
	cleared := 0
	for _, v := range store.obj("credential_pool").list(ProviderCodex) {
		entry := asRow(v)
		if entry == nil || (accessToken != "" && textOf(entry.value("access_token")) != accessToken) {
			continue
		}
		if rowIsRateLimitExhausted(entry) {
			clearPoolEntryStatus(entry)
			cleared++
		}
	}
	if cleared > 0 {
		if err := s.save(lctx, store, target); err != nil {
			slog.Debug("Failed to clear Codex pool quota cooldowns", "error", err)
		}
	}
	return cleared
}

func (s *Store) codexPoolRateLimitStatus(ctx context.Context) *doc {
	rows, err := s.readPool(ctx, ProviderCodex)
	if err != nil {
		return nil
	}
	now := nowEpoch()
	for _, entry := range rows {
		token := strings.TrimSpace(entry.str("access_token"))
		if token == "" || !rowIsRateLimitExhausted(entry) {
			continue
		}
		resetAt, ok := parseAbsolute(entry.value("last_error_reset_at"))
		if ok && resetAt <= now {
			continue
		}
		out := newDoc()
		out.set("label", entry.value("label"))
		out.set("last_refresh", entry.value("last_refresh"))
		if ok {
			out.set("reset_at", floatNum(resetAt))
		} else {
			out.set("reset_at", nil)
		}
		out.set("reason", entry.value("last_error_reason"))
		out.set("message", entry.value("last_error_message"))
		out.set("access_token", token)
		out.set("refresh_token", entry.value("refresh_token"))
		out.set("id", entry.value("id"))
		out.set("base_url", entry.value("base_url"))
		return out
	}
	return nil
}

func (s *Store) poolCodexCredential(ctx context.Context) (token, baseURL string) {
	rows, err := s.readPool(ctx, ProviderCodex)
	if err != nil {
		return "", ""
	}
	for _, entry := range rows {
		resetAt, ok := parseAbsolute(entry.value("last_error_reset_at"))
		if t := strings.TrimSpace(entry.str("access_token")); t != "" && !(ok && resetAt > nowEpoch()) {
			return t, stripped(entry.value("base_url"))
		}
	}
	return "", ""
}

func (s *Store) poolFirstOAuthStatus(ctx context.Context, provider, authMode string, isExpiring func(token string) bool,
	resolve func() (RuntimeCredentials, error), onPoolMiss func() (Status, bool)) Status {
	if pool, err := s.Pool(ctx, provider); err == nil && pool.HasCredentials() {
		if entry, ok, perr := pool.Peek(ctx); perr == nil && ok {
			key := entry.RuntimeAPIKey()
			if key.Empty() {
				key = entry.AccessToken
			}
			if !key.Empty() && !isExpiring(key.Reveal()) {
				base := entry.RuntimeBaseURL(ctx)
				if base == "" {
					base = entry.BaseURL
				}
				return Status{LoggedIn: true, AuthStore: s.authPath, LastRefresh: entry.LastRefresh, AuthMode: authMode,
					Source: "pool:" + entry.Label, APIKey: key, BaseURL: strings.TrimRight(base, "/")}
			}
		}
		if onPoolMiss != nil {
			if degraded, ok := onPoolMiss(); ok {
				return degraded
			}
		}
	}
	creds, err := resolve()
	if err != nil {
		return Status{AuthStore: s.authPath, Error: err.Error()}
	}
	return Status{LoggedIn: true, AuthStore: s.authPath, LastRefresh: creds.LastRefresh, AuthMode: creds.AuthMode,
		Source: creds.Source, APIKey: creds.APIKey, BaseURL: creds.BaseURL}
}

func (s *Store) CodexStatus(ctx context.Context) Status {
	ctx = s.bind(ctx)
	status := s.poolFirstOAuthStatus(ctx, ProviderCodex, "chatgpt",
		func(token string) bool { return codexAccessTokenIsExpiring(token, 0) },
		func() (RuntimeCredentials, error) { return s.ResolveCodex(ctx, ResolveOptions{ReadOnly: true}) },
		func() (Status, bool) {
			limited := s.codexPoolRateLimitStatus(ctx)
			if limited == nil {
				return Status{}, false
			}
			label := textOf(limited.value("label"))
			if label == "" {
				label = "unknown"
			}
			message := textOf(limited.value("message"))
			if message == "" {
				message = "Codex provider quota exhausted; retry after the usage limit resets."
			}
			st := Status{LoggedIn: true, AuthStore: s.authPath, LastRefresh: textOf(limited.value("last_refresh")), AuthMode: "chatgpt",
				Source: "pool:" + label, RateLimited: true, ErrorCode: CodexRateLimitedCode, Error: message}
			if resetAt, ok := numberOf(limited.value("reset_at")); ok {
				st.ResetAt = timeOf(resetAt)
			}
			return st, true
		})
	if strings.HasPrefix(status.Source, "pool:") {
		status.BaseURL = s.codexPoolRouteBaseURL(ctx, status.BaseURL)
	}
	return status
}

func codexLoginRateLimitedError(r reply, during string) *Error {
	hint := " Wait a minute and run the login again."
	if after, ok := retryAfterSeconds(r.header); ok {
		hint = fmt.Sprintf(" Try again in about %ds.", after)
	}
	return codexErr(fmt.Sprintf("OpenAI is rate-limiting Codex login requests (HTTP 429)%s. "+
		"This is a temporary throttle on OpenAI's side, not a credential problem.%s", during, hint), CodexRateLimitedCode, false)
}

type CodexLogin struct {
	Tokens      *doc
	BaseURL     string
	LastRefresh string
	Source      string
}

func (s *Store) codexRequestDeviceCode(ctx context.Context, c Console) (*doc, error) {
	body, _ := (func() *doc { d := newDoc(); d.set("client_id", CodexOAuthClientID); return d })().MarshalJSON()
	var r reply
	for attempt := 1; attempt <= 4; attempt++ {
		var err error
		r, err = s.codexLoginPost(ctx, codexIssuer+"/api/accounts/deviceauth/usercode", map[string]string{"Content-Type": "application/json"}, body,
			"Failed to request device code", "device_code_request_failed")
		if err != nil {
			return nil, err
		}
		if r.status != 429 {
			break
		}
		if attempt < 4 {
			delay := 1 << attempt
			if after, ok := retryAfterSeconds(r.header); ok {
				delay = after
			}
			delay = max(1, min(delay, 60))
			c.printf("OpenAI is rate-limiting login requests (429); retrying in %ds...\n", delay)
			if err := cancellableSleep(ctx, time.Duration(delay)*time.Second, nil); err != nil {
				return nil, err
			}
		}
	}
	if r.status == 429 {
		return nil, codexLoginRateLimitedError(r, "")
	}
	if r.status != 200 {
		return nil, codexErr(fmt.Sprintf("Device code request returned status %d.", r.status), "device_code_request_error", false)
	}
	data, err := r.object()
	if err != nil {
		return nil, err
	}
	if !truthy(data.value("user_code")) || !truthy(data.value("device_auth_id")) {
		return nil, codexErr("Device code response missing required fields.", "device_code_incomplete", false)
	}
	return data, nil
}

func (s *Store) codexPollAuthorizationCode(ctx context.Context, c Console, deviceAuthID, userCode string, interval time.Duration) (*doc, error) {
	payload := newDoc()
	payload.set("device_auth_id", deviceAuthID)
	payload.set("user_code", userCode)
	body, _ := payload.MarshalJSON()
	start := time.Now()
	blips := 0
	for time.Since(start) < 15*time.Minute {
		if err := cancellableSleep(ctx, interval, nil); err != nil {
			if errors.Is(err, context.Canceled) {
				c.println("\nLogin cancelled.")
				return nil, ErrCancelled
			}
			return nil, err
		}
		r, err := s.codexExchange(ctx, http.MethodPost, codexIssuer+"/api/accounts/deviceauth/token", map[string]string{"Content-Type": "application/json"}, body, 15*time.Second)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				c.println("\nLogin cancelled.")
				return nil, ErrCancelled
			}
			if !IsNetworkError(err) {
				detail := transportFailure(err)
				var ae *Error
				if errors.As(err, &ae) {
					detail = ae.Message
				}
				return nil, &Error{Message: fmt.Sprintf("Device auth polling request failed: %s%s", detail, tlsInteropHint(err)), Provider: ProviderCodex, Code: "device_code_poll_error", Cause: err}
			}
			blips++
			if blips >= 6 {
				return nil, &Error{Message: fmt.Sprintf("Device auth polling request failed after %d consecutive transport errors: %s%s", blips, transportFailure(err), tlsInteropHint(err)),
					Provider: ProviderCodex, Code: "device_code_poll_error", Cause: err}
			}
			c.println("Transient network error while waiting for sign-in; retrying...")
			continue
		}
		blips = 0
		if r.status == 200 {
			return r.object()
		}
		if r.status != 403 && r.status != 404 {
			return nil, codexErr(fmt.Sprintf("Device auth polling returned status %d.", r.status), "device_code_poll_error", false)
		}
	}
	return nil, codexErr("Login timed out after 15 minutes.", "device_code_timeout", false)
}

func (s *Store) codexExchangeCode(ctx context.Context, code, redirectURI, verifier string) (*doc, error) {
	form := formOf("grant_type", "authorization_code", "code", code, "redirect_uri", redirectURI, "client_id", CodexOAuthClientID, "code_verifier", verifier)
	r, err := s.codexLoginPost(ctx, CodexOAuthTokenURL, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(encodeForm(form)),
		"Token exchange failed", "token_exchange_failed")
	if err != nil {
		return nil, err
	}
	if r.status == 429 {
		return nil, codexLoginRateLimitedError(r, " during token exchange")
	}
	if r.status != 200 {
		return nil, codexErr(fmt.Sprintf("Token exchange returned status %d.", r.status), "token_exchange_error", false)
	}
	tokens, err := r.object()
	if err != nil {
		return nil, err
	}
	if !truthy(tokens.value("access_token")) {
		return nil, codexErr("Token exchange did not return an access_token.", "token_exchange_no_access_token", false)
	}
	return tokens, nil
}

func (s *Store) codexLoginResult(ctx context.Context, tokens *doc, source string) CodexLogin {
	pair := newDoc()
	pair.set("access_token", textOf(tokens.value("access_token")))
	pair.set("refresh_token", textOf(tokens.value("refresh_token")))
	return CodexLogin{Tokens: pair, BaseURL: s.codexBaseURL(ctx), LastRefresh: isoStampZ(time.Now()), Source: source}
}

func (s *Store) codexDeviceCodeLogin(ctx context.Context, c Console) (CodexLogin, error) {
	device, err := s.codexRequestDeviceCode(ctx, c)
	if err != nil {
		return CodexLogin{}, err
	}
	userCode := textOf(device.value("user_code"))
	interval := int64(5)
	if n, ok := intOf(device.value("interval")); ok {
		interval = n
	} else if n := coerceTTL(device.value("interval")); n > 0 {
		interval = n
	}
	c.println("To continue, follow these steps:\n")
	c.println("  1. Open this URL in your browser:")
	c.printf("     \033[94m%s/codex/device\033[0m\n\n", codexIssuer)
	c.println("  2. Enter this code:")
	c.printf("     \033[94m%s\033[0m\n\n", userCode)
	c.println("Waiting for sign-in... (press Ctrl+C to cancel)")
	codeResp, err := s.codexPollAuthorizationCode(ctx, c, textOf(device.value("device_auth_id")), userCode, time.Duration(max(3, interval))*time.Second)
	if err != nil {
		return CodexLogin{}, err
	}
	code, verifier := textOf(codeResp.value("authorization_code")), textOf(codeResp.value("code_verifier"))
	if code == "" || verifier == "" {
		return CodexLogin{}, codexErr("Device auth response missing authorization_code or code_verifier.", "device_code_incomplete_exchange", false)
	}
	tokens, err := s.codexExchangeCode(ctx, code, codexIssuer+"/deviceauth/callback", verifier)
	if err != nil {
		return CodexLogin{}, err
	}
	return s.codexLoginResult(ctx, tokens, "device-code"), nil
}

func (s *Store) codexBrowserLogin(ctx context.Context, c Console, openBrowser bool, timeout time.Duration) (CodexLogin, error) {
	verifier, err := pkceCodeVerifier()
	if err != nil {
		return CodexLogin{}, err
	}
	state, err := randomURLToken(32)
	if err != nil {
		return CodexLogin{}, err
	}
	server, err := s.bindLoopback("127.0.0.1", codexBrowserCallbackPort, codexBrowserCallbackPath, "OpenAI Codex", ProviderCodex, codexBrowserPortBusyCode)
	if err != nil {
		return CodexLogin{}, err
	}
	redirectURI := fmt.Sprintf("http://localhost:%d%s", server.port(), codexBrowserCallbackPath)
	authURL := codexOAuthAuthorizeURL + "?" + encodeForm(formOf(
		"response_type", "code", "client_id", CodexOAuthClientID, "redirect_uri", redirectURI, "scope", codexOAuthBrowserScope,
		"code_challenge", pkceCodeChallenge(verifier), "code_challenge_method", "S256", "id_token_add_organizations", "true", "state", state))
	c.println("")
	c.println("Signing in to OpenAI Codex (browser authorization)...")
	c.println("(icebath creates its own session — won't affect Codex CLI or VS Code)")
	c.println("")
	c.printf("Open this URL to authorize icebath:\n  %s\n\n", authURL)
	s.printLoopbackSSHHint(ctx, c, redirectURI, "")
	if openBrowser && s.canOpenGraphicalBrowser(ctx) {
		if s.openBrowser(ctx, authURL) {
			c.println("Browser opened for OpenAI authorization.")
		} else {
			c.println("Could not open the browser automatically; use the URL above.")
		}
	}
	if timeout <= 0 {
		timeout = codexBrowserCallbackTimeout
	}
	c.printf("Waiting for the OpenAI callback on %s (timeout %ds, Ctrl+C to cancel)...\n", redirectURI, int(timeout.Seconds()))
	callback, err := server.wait(ctx, timeout, ProviderCodex, "codex_browser_callback_timeout")
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			c.println("\nLogin cancelled.")
		}
		return CodexLogin{}, err
	}
	if callback.failure != "" {
		detail := callback.errorDescription
		if detail == "" {
			detail = callback.failure
		}
		return CodexLogin{}, codexErr("OpenAI authorization failed: "+detail, "codex_browser_auth_denied", false)
	}
	if subtle.ConstantTimeCompare([]byte(callback.state), []byte(state)) != 1 {
		return CodexLogin{}, codexErr("Authorization callback state mismatch — the redirect did not come from this login. Aborting.", "codex_browser_state_mismatch", false)
	}
	code := strings.TrimSpace(callback.code)
	if code == "" {
		return CodexLogin{}, codexErr("Authorization callback did not carry a code.", "codex_browser_no_code", false)
	}
	c.println("Exchanging the authorization code for Codex tokens...")
	tokens, err := s.codexExchangeCode(ctx, code, redirectURI, verifier)
	if err != nil {
		return CodexLogin{}, err
	}
	return s.codexLoginResult(ctx, tokens, "loopback_pkce"), nil
}

type LoginOptions struct {
	Browser       bool
	NoBrowser     bool
	Timeout       time.Duration
	ForceNewLogin bool
	Region        string
	UpdateConfig  func(ctx context.Context, provider, baseURL string) (configPath string, err error)
}

func (s *Store) codexLoginFlow(ctx context.Context, c Console, o LoginOptions) config.CodexLoginFlow {
	if o.Browser {
		return config.LoginBrowser
	}
	cfg, err := s.cfg.LoadReadonly(ctx)
	if err != nil {
		return config.LoginDeviceCode
	}
	flow := config.CodexLoginFlow(strings.ToLower(strings.TrimSpace(string(cfg.Auth.CodexLoginFlow))))
	switch flow {
	case "":
		return config.LoginDeviceCode
	case config.LoginDeviceCode, config.LoginBrowser:
		return flow
	}
	c.printf("Ignoring unknown auth.codex_login_flow '%s' (expected one of device_code, browser).\n", flow)
	return config.LoginDeviceCode
}

func (s *Store) CodexOAuthLogin(ctx context.Context, c Console, o LoginOptions) (CodexLogin, error) {
	ctx = s.bind(ctx)
	if s.codexLoginFlow(ctx, c, o) == config.LoginBrowser {
		creds, err := s.codexBrowserLogin(ctx, c, !o.NoBrowser, o.Timeout)
		var ae *Error
		if err == nil || !errors.As(err, &ae) || ae.Code != codexBrowserPortBusyCode {
			return creds, err
		}
		c.printf("Port %d is already in use (a Codex CLI sign-in may be running). "+
			"OpenAI only redirects to that port, so falling back to the device-code login.\n\n", codexBrowserCallbackPort)
	}
	c.println("Signing in to OpenAI Codex...")
	c.println("(icebath creates its own session — won't affect Codex CLI or VS Code)")
	c.println("")
	return s.codexDeviceCodeLogin(ctx, c)
}

func (s *Store) SaveCodexLogin(ctx context.Context, login CodexLogin, label string, setActive bool) error {
	return s.saveCodexTokens(s.bind(ctx), login.Tokens, login.LastRefresh, label, setActive, false)
}

func (s *Store) LoginCodex(ctx context.Context, c Console, o LoginOptions) error {
	ctx = s.bind(ctx)
	if o.UpdateConfig == nil {
		return errors.New("auth: Codex login needs the config update function")
	}
	if !o.ForceNewLogin {
		accepted, err := s.OfferExistingCredentials(ctx, c, ExistingOffer{
			Provider: ProviderCodex, DisplayName: "Codex", DefaultBaseURL: DefaultCodexBaseURL,
			ExpiredNotice: "Existing Codex credentials are expired. Starting fresh login...",
			Resolve: func(ctx context.Context) (Secret, string, error) {
				creds, err := s.ResolveCodex(ctx, ResolveOptions{})
				return creds.APIKey, creds.BaseURL, err
			},
			IsExpiring:   codexAccessTokenIsExpiring,
			UpdateConfig: o.UpdateConfig,
		})
		if err != nil || accepted {
			return err
		}
		if cli := s.importCodexCLITokens(ctx); cli != nil {
			c.println("Found existing Codex CLI credentials at ~/.codex/auth.json")
			c.println("icebath will create its own session to avoid conflicts with Codex CLI / VS Code.")
			if PromptYesNo(c, "Import these credentials? (a separate login is recommended) [y/N]: ", false) {
				if err := s.saveCodexTokens(ctx, cli, "", "", true, false); err != nil {
					return err
				}
				configPath, err := o.UpdateConfig(ctx, ProviderCodex, s.codexBaseURL(ctx))
				if err != nil {
					return err
				}
				c.println("")
				c.println("Credentials imported. Note: if Codex CLI refreshes its token,")
				c.println("icebath will keep working independently with its own session.")
				c.printf("  Config updated: %s (model.provider=openai-codex)\n", configPath)
				return nil
			}
		}
	}
	c.println("")
	creds, err := s.CodexOAuthLogin(ctx, c, o)
	if err != nil {
		return err
	}
	if err := s.saveCodexTokens(ctx, creds.Tokens, creds.LastRefresh, "", true, false); err != nil {
		return err
	}
	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = DefaultCodexBaseURL
	}
	configPath, err := o.UpdateConfig(ctx, ProviderCodex, baseURL)
	if err != nil {
		return err
	}
	s.PrintLoginSuccess(c, ProviderCodex, configPath, true)
	return nil
}

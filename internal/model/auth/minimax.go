package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

const minimaxErrorBodyLimit = 16 * 1024

func minimaxErr(message, code string, relogin bool) *Error {
	return &Error{Message: message, Provider: ProviderMiniMax, Code: code, Relogin: relogin}
}

func boundedErrorText(body []byte, limit int) string {
	text := wireMask(strings.ToValidUTF8(string(body[:min(len(body), limit)]), "�"))
	if len(body) > limit {
		text += "...[truncated]"
	}
	return text
}

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func minimaxExpiryUnix(expiredIn int64, now time.Time) float64 {
	if expiredIn > now.UnixMilli()/2 {
		return float64(expiredIn) / 1000.0
	}
	return epochOf(now) + float64(max(1, expiredIn))
}

func minimaxExpiryFields(state *doc, expiredIn int64) {
	now := time.Now()
	expires := minimaxExpiryUnix(expiredIn, now)
	state.set("obtained_at", isoStamp(now))
	state.set("expires_at", isoStamp(timeOf(expires)))
	state.set("expires_in", intNum(max(0, int64(expires-epochOf(now)))))
}

func (s *Store) minimaxSaveState(ctx context.Context, state *doc, setActive bool) error {
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return err
	}
	if err := s.storeProviderState(ctx, store, ProviderMiniMax, state, setActive); err != nil {
		return err
	}
	return s.save(ctx, store, s.authPath)
}

func (s *Store) MiniMaxLogin(ctx context.Context, c Console, o LoginOptions) error {
	ctx = s.bind(ctx)
	region := o.Region
	if region == "" {
		region = "global"
	}
	portal, inference := MiniMaxOAuthGlobalBase, MiniMaxOAuthGlobalInference
	if region == "cn" {
		portal, inference = MiniMaxOAuthCNBase, MiniMaxOAuthCNInference
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	raw, err := randomURLToken(64)
	if err != nil {
		return err
	}
	verifier := raw[:min(len(raw), 96)]
	state, err := randomURLToken(16)
	if err != nil {
		return err
	}
	openBrowser := !o.NoBrowser && !s.isRemoteSession(ctx)
	c.printf("Starting icebath login via MiniMax (%s) OAuth...\n", region)
	c.printf("Portal: %s\n", portal)
	r, err := s.postForm(ctx, nil, portal+"/oauth/code", formOf(
		"response_type", "code", "client_id", MiniMaxOAuthClientID, "scope", MiniMaxOAuthScope,
		"code_challenge", pkceCodeChallenge(verifier), "code_challenge_method", "S256", "state", state,
	), map[string]string{"x-request-id": requestID()}, timeout)
	if err != nil {
		return err
	}
	if r.status != 200 {
		body := boundedErrorText(r.body, minimaxErrorBodyLimit)
		if body == "" {
			body = http.StatusText(r.status)
		}
		return minimaxErr("MiniMax OAuth authorization failed: "+body, "authorization_failed", false)
	}
	code, err := r.object()
	if err != nil {
		return err
	}
	for _, f := range []string{"user_code", "verification_uri", "expired_in"} {
		if !code.has(f) {
			return minimaxErr("MiniMax OAuth response missing field: "+f, "authorization_incomplete", false)
		}
	}
	if code.str("state") != state {
		return minimaxErr("MiniMax OAuth state mismatch (possible CSRF).", "state_mismatch", false)
	}
	userCode := textOf(code.value("user_code"))
	if err := s.PrintDeviceCodeInstructions(ctx, c, textOf(code.value("verification_uri")), userCode, openBrowser && s.canOpenGraphicalBrowser(ctx), "", false); err != nil {
		return err
	}
	c.println("Waiting for approval...")
	expiredIn, _ := intOf(code.value("expired_in"))
	intervalMS := int64(2000)
	if n, ok := intOf(code.value("interval")); ok && n != 0 {
		intervalMS = n
	}
	token, err := s.minimaxPollToken(ctx, portal, userCode, verifier, expiredIn, intervalMS, timeout)
	if err != nil {
		return err
	}
	auth := newDoc()
	auth.set("provider", ProviderMiniMax)
	auth.set("region", region)
	auth.set("portal_base_url", portal)
	auth.set("inference_base_url", inference)
	auth.set("client_id", MiniMaxOAuthClientID)
	auth.set("scope", MiniMaxOAuthScope)
	if token.has("token_type") {
		auth.set("token_type", token.value("token_type"))
	} else {
		auth.set("token_type", "Bearer")
	}
	auth.set("access_token", token.value("access_token"))
	auth.set("refresh_token", token.value("refresh_token"))
	auth.set("resource_url", token.value("resource_url"))
	tokenExpiry, _ := intOf(token.value("expired_in"))
	minimaxExpiryFields(auth, tokenExpiry)
	if err := s.minimaxSaveState(ctx, auth, true); err != nil {
		return err
	}
	c.println("✓ MiniMax OAuth login successful.")
	if msg := textOf(token.value("notification_message")); msg != "" {
		c.printf("Note from MiniMax: %s\n", msg)
	}
	return nil
}

func (s *Store) minimaxPollToken(ctx context.Context, portal, userCode, verifier string, expiredIn, intervalMS int64, timeout time.Duration) (*doc, error) {
	deadline := minimaxExpiryUnix(expiredIn, time.Now())
	interval := max(2*time.Second, time.Duration(intervalMS)*time.Millisecond)
	for deadline > nowEpoch() {
		r, err := s.postForm(ctx, nil, portal+"/oauth/token", formOf(
			"grant_type", MiniMaxOAuthGrantType, "client_id", MiniMaxOAuthClientID, "user_code", userCode, "code_verifier", verifier,
		), nil, timeout)
		if err != nil {
			return nil, err
		}
		if r.status != 200 {
			text := boundedErrorText(r.body, minimaxErrorBodyLimit)
			msg := text
			if len(r.body) <= minimaxErrorBodyLimit && len(r.body) > 0 {
				if payload, perr := parseObject(r.body); perr == nil {
					if status := textOf(payload.obj("base_resp").value("status_msg")); status != "" {
						msg = wireMask(status)
					}
				}
			}
			if msg == "" {
				msg = "unknown"
			}
			return nil, minimaxErr("MiniMax OAuth error: "+msg, "token_exchange_failed", false)
		}
		payload, perr := parseObject(r.body)
		if perr != nil {
			payload = newDoc()
		}
		switch payload.str("status") {
		case "error":
			return nil, minimaxErr("MiniMax OAuth reported an error. Please try again later.", "authorization_denied", false)
		case "success":
			for _, k := range []string{"access_token", "refresh_token", "expired_in"} {
				if !truthy(payload.value(k)) {
					return nil, minimaxErr("MiniMax OAuth success payload missing required token fields.", "token_incomplete", false)
				}
			}
			return payload, nil
		}
		if err := cancellableSleep(ctx, interval, nil); err != nil {
			return nil, err
		}
	}
	return nil, minimaxErr("MiniMax OAuth timed out before authorization completed.", "timeout", false)
}

func (s *Store) refreshMiniMaxState(ctx context.Context, state *doc, timeout time.Duration, force bool) (*doc, error) {
	if !truthy(state.value("refresh_token")) {
		return nil, minimaxErr("MiniMax OAuth state has no refresh_token; please re-login.", "no_refresh_token", true)
	}
	expiresAt := 0.0
	if t, ok := parseISO(state.str("expires_at"), time.Local); ok {
		expiresAt = epochOf(t)
	}
	if !force && expiresAt-nowEpoch() > MiniMaxOAuthRefreshSkew.Seconds() {
		return state, nil
	}
	r, err := s.postForm(ctx, nil, textOf(state.value("portal_base_url"))+"/oauth/token", formOf(
		"grant_type", "refresh_token", "client_id", textOf(state.value("client_id")), "refresh_token", textOf(state.value("refresh_token")),
	), nil, timeout)
	if err != nil {
		return nil, err
	}
	if r.status != 200 {
		lower := strings.ToLower(string(r.body[:min(len(r.body), minimaxErrorBodyLimit)]))
		relogin := strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "refresh_token_reused") || strings.Contains(lower, "invalid_refresh_token")
		body := boundedErrorText(r.body, minimaxErrorBodyLimit)
		if body == "" {
			body = http.StatusText(r.status)
		}
		return nil, minimaxErr("MiniMax OAuth refresh failed: "+body, "refresh_failed", relogin)
	}
	payload, err := r.object()
	if err != nil {
		return nil, err
	}
	if payload.str("status") != "success" {
		return nil, minimaxErr("MiniMax OAuth refresh did not return success.", "refresh_failed", true)
	}
	next := state.clone()
	next.set("access_token", payload.value("access_token"))
	if payload.has("refresh_token") {
		next.set("refresh_token", payload.value("refresh_token"))
	}
	expiredIn, _ := intOf(payload.value("expired_in"))
	minimaxExpiryFields(next, expiredIn)
	if err := s.minimaxSaveState(ctx, next, false); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *Store) minimaxFreshState(ctx context.Context) (*doc, error) {
	ctx, unlock, err := s.lock(ctx, s.authPath, max(LockTimeout, 20*time.Second))
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := s.providerState(ctx, ProviderMiniMax)
	if err != nil {
		return nil, err
	}
	if !truthy(state.value("access_token")) {
		return nil, minimaxErr("Not logged into MiniMax OAuth. Run `icebath model` and select MiniMax (OAuth).", "not_logged_in", true)
	}
	fresh, err := s.refreshMiniMaxState(ctx, state, 15*time.Second, false)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) && ae.Relogin && truthy(state.value("refresh_token")) {
			quarantineFlatOAuthState(state, ProviderMiniMax, ae)
			if serr := s.minimaxSaveState(ctx, state, false); serr != nil {
				slog.Debug("MiniMax OAuth: failed to persist quarantined state", "error", serr)
			}
		}
		return nil, err
	}
	return fresh, nil
}

type minimaxTokenSource struct{ s *Store }

func (m minimaxTokenSource) Kind() SourceKind { return SourceMiniMaxOAuth }

func (m minimaxTokenSource) Token(ctx context.Context) (Token, error) {
	state, err := m.s.minimaxFreshState(m.s.bind(ctx))
	if err != nil {
		return Token{}, err
	}
	token := textOf(state.value("access_token"))
	if token == "" {
		return Token{}, minimaxErr("MiniMax OAuth state has no access_token after refresh.", "no_access_token", true)
	}
	out := Token{Value: NewSecret(token)}
	if t, ok := parseISO(state.str("expires_at"), time.Local); ok {
		out.Expiry = t
	}
	return out, nil
}

func (s *Store) MiniMaxTokenSource() TokenSource { return minimaxTokenSource{s: s} }

func (s *Store) ResolveMiniMax(ctx context.Context, o ResolveOptions) (RuntimeCredentials, error) {
	state, err := s.minimaxFreshState(s.bind(ctx))
	if err != nil {
		return RuntimeCredentials{}, err
	}
	creds := RuntimeCredentials{Provider: ProviderMiniMax, BaseURL: strings.TrimRight(textOf(state.value("inference_base_url")), "/"), Source: "oauth"}
	if o.AsTokenSource {
		creds.Tokens = s.MiniMaxTokenSource()
	} else {
		creds.APIKey = NewSecret(textOf(state.value("access_token")))
	}
	return creds, nil
}

func (s *Store) MiniMaxStatus(ctx context.Context) (Status, error) {
	state, err := s.providerState(s.bind(ctx), ProviderMiniMax)
	if err != nil {
		return Status{}, err
	}
	if !truthy(state.value("access_token")) {
		return Status{Provider: ProviderMiniMax}, nil
	}
	valid := true
	if raw, ok := state.value("expires_at").(string); ok {
		if t, parsed := parseISO(raw, time.Local); parsed {
			valid = epochOf(t) > nowEpoch()
		}
	}
	region := "global"
	if state.has("region") {
		region = textOf(state.value("region"))
	}
	return Status{LoggedIn: valid, Provider: ProviderMiniMax,
		Extra: map[string]string{"region": region, "expires_at": textOf(state.value("expires_at"))}}, nil
}

func (s *Store) qwenAuthPath() string {
	userHome, err := home.UserHome()
	if err != nil {
		userHome = "~"
	}
	return userHome + string(os.PathSeparator) + ".qwen" + string(os.PathSeparator) + "oauth_creds.json"
}

const qwenRerun = "Re-run 'qwen auth qwen-oauth'."

func qwenErr(message, code string) *Error {
	return &Error{Message: message, Provider: ProviderQwen, Code: code}
}

func (s *Store) readQwenTokens() (*doc, error) {
	path := s.qwenAuthPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, qwenErr("Qwen CLI credentials not found. Run 'qwen auth qwen-oauth' first.", "qwen_auth_missing")
	}
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("Failed to read Qwen CLI credentials from %s: %v", path, err), Provider: ProviderQwen, Code: "qwen_auth_read_failed", Cause: err}
	}
	parsed, perr := parseJSON(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if perr != nil {
		return nil, &Error{Message: fmt.Sprintf("Failed to read Qwen CLI credentials from %s: %v", path, perr), Provider: ProviderQwen, Code: "qwen_auth_read_failed", Cause: perr}
	}
	tokens, ok := parsed.(*doc)
	if !ok {
		return nil, qwenErr(fmt.Sprintf("Invalid Qwen CLI credentials in %s.", path), "qwen_auth_invalid")
	}
	return tokens, nil
}

func qwenAccessTokenIsExpiring(expiry any, skew time.Duration) bool {
	ms, ok := intOf(expiry)
	if !ok {
		if text, isText := expiry.(string); isText {
			n := coerceTTL(text)
			if n == 0 {
				return true
			}
			ms = n
		} else {
			return true
		}
	}
	return (nowEpoch()+max(0, skew.Seconds()))*1000 >= float64(ms)
}

func (s *Store) refreshQwenTokens(ctx context.Context, tokens *doc, timeout time.Duration) (*doc, error) {
	refreshToken := stripped(tokens.value("refresh_token"))
	if refreshToken == "" {
		return nil, qwenErr("Qwen OAuth refresh token missing. "+qwenRerun, "qwen_refresh_token_missing")
	}
	r, err := s.postForm(ctx, nil, QwenOAuthTokenURL, formOf("grant_type", "refresh_token", "refresh_token", refreshToken, "client_id", QwenOAuthClientID), nil, timeout)
	if err != nil {
		return nil, &Error{Message: "Qwen OAuth refresh failed: " + transportFailure(err), Provider: ProviderQwen, Code: "qwen_refresh_failed", Cause: err}
	}
	if r.status >= 400 {
		message := "Qwen OAuth refresh failed. " + qwenRerun
		if body := strings.TrimSpace(wireMask(string(r.body))); body != "" {
			message += " Response: " + body
		}
		return nil, qwenErr(message, "qwen_refresh_failed")
	}
	parsed, perr := r.value()
	if perr != nil {
		return nil, qwenErr(fmt.Sprintf("Qwen OAuth refresh returned invalid JSON: %v", perr), "qwen_refresh_invalid_json")
	}
	payload, _ := parsed.(*doc)
	access := stripped(payload.value("access_token"))
	if access == "" {
		return nil, qwenErr("Qwen OAuth refresh response missing access_token.", "qwen_refresh_invalid_response")
	}
	expiresIn := int64(6 * 60 * 60)
	switch v := payload.value("expires_in").(type) {
	case json.Number:
		if n, ok := intOf(v); ok {
			expiresIn = n
		}
	case string:
		if n, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64); perr == nil {
			expiresIn = n
		}
	case bool:
		expiresIn = 0
		if v {
			expiresIn = 1
		}
	}
	carried := func(key string, fallback any) any {
		if payload.has(key) {
			return payload.value(key)
		}
		return fallback
	}
	orDefault := func(v any, fallback string) string {
		if !truthy(v) {
			return fallback
		}
		return textOf(v)
	}
	tokenTypeFallback := any("Bearer")
	if tokens.has("token_type") {
		tokenTypeFallback = tokens.value("token_type")
	}
	resourceFallback := any("portal.qwen.ai")
	if tokens.has("resource_url") {
		resourceFallback = tokens.value("resource_url")
	}
	tokenType := strings.TrimSpace(orDefault(carried("token_type", tokenTypeFallback), "Bearer"))
	if tokenType == "" {
		tokenType = "Bearer"
	}
	refreshed := newDoc()
	refreshed.set("access_token", access)
	refreshed.set("expiry_date", intNum(time.Now().UnixMilli()+max(1, expiresIn)*1000))
	refreshed.set("refresh_token", strings.TrimSpace(orDefault(carried("refresh_token", refreshToken), refreshToken)))
	refreshed.set("resource_url", strings.TrimSpace(orDefault(carried("resource_url", resourceFallback), "portal.qwen.ai")))
	refreshed.set("token_type", tokenType)
	if err := s.savePrivateJSON(ctx, s.qwenAuthPath(), refreshed, false); err != nil {
		return nil, err
	}
	return refreshed, nil
}

func (s *Store) ResolveQwen(ctx context.Context, o ResolveOptions) (RuntimeCredentials, error) {
	ctx = s.bind(ctx)
	tokens, err := s.readQwenTokens()
	if err != nil {
		return RuntimeCredentials{}, err
	}
	skew := o.RefreshSkew
	if skew == 0 {
		skew = QwenAccessTokenRefreshSkew
	}
	if o.ForceRefresh || (!o.NoRefresh && qwenAccessTokenIsExpiring(tokens.value("expiry_date"), skew)) {
		if tokens, err = s.refreshQwenTokens(ctx, tokens, 20*time.Second); err != nil {
			return RuntimeCredentials{}, err
		}
	}
	access := stripped(tokens.value("access_token"))
	if access == "" {
		return RuntimeCredentials{}, qwenErr("Qwen OAuth access token missing. "+qwenRerun, "qwen_access_token_missing")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(s.lookupEnv(ctx, "ICEBATH_QWEN_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = DefaultQwenBaseURL
	}
	expiry, _ := intOf(tokens.value("expiry_date"))
	return RuntimeCredentials{Provider: ProviderQwen, BaseURL: baseURL, APIKey: NewSecret(access), Source: "qwen-cli", ExpiresAtMS: expiry, AuthFile: s.qwenAuthPath()}, nil
}

func (s *Store) MarkQwenActive(ctx context.Context, baseURL string) error {
	state := newDoc()
	if baseURL != "" {
		state.set("base_url", baseURL)
	}
	return s.saveActiveState(ctx, ProviderQwen, state)
}

func (s *Store) QwenStatus(ctx context.Context) (Status, error) {
	creds, err := s.ResolveQwen(ctx, ResolveOptions{})
	if err != nil {
		var ae *Error
		if !errors.As(err, &ae) {
			return Status{}, err
		}
		return Status{Provider: ProviderQwen, Error: err.Error(), Extra: map[string]string{"auth_file": s.qwenAuthPath()}}, nil
	}
	extra := map[string]string{"auth_file": creds.AuthFile}
	if creds.ExpiresAtMS != 0 {
		extra["expires_at_ms"] = fmt.Sprint(creds.ExpiresAtMS)
	}
	return Status{LoggedIn: true, Provider: ProviderQwen, Source: creds.Source, APIKey: creds.APIKey, Extra: extra}, nil
}

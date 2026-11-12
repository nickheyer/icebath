package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const nativeExpiredDetail = "Native login expired or unknown; restart sign-in."

var authNoStore = []string{"Cache-Control", "no-store, no-cache, must-revalidate"}

func dashboardAuthRoutesLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.dashboard_auth.routes")
}

// Normalised X-Forwarded-Prefix for cookie paths and redirect URLs
func authPrefix(conn *HTTPConnection) string { return PrefixFromRequest(conn) }

// Audit record of an auth event with the client ip appended
func authAudit(ctx context.Context, conn *HTTPConnection, event AuditEvent, fields ...any) {
	auditLog(ctx, event, append(fields, "ip", clientIP(conn))...)
}

// Absolute callback URL the IDP returns to, the public URL whole when declared, else url_for under the prefix
func authRedirectURI(ctx context.Context, conn *HTTPConnection) (string, error) {
	if publicURL := gateway.ResolvePublicURL(ctx, listenerStateOf(conn.App().State)); publicURL != "" {
		return publicURL + "/auth/callback", nil
	}
	base, err := conn.URLFor("auth_callback", map[string]string{})
	if err != nil {
		return "", err
	}
	prefix := authPrefix(conn)
	if prefix == "" {
		return base.String(), nil
	}
	parsed, err := pyurl.Parse(base.String())
	if err != nil {
		return "", err
	}
	parsed.Path = prefix + parsed.Path
	return pyurl.Unparse(parsed), nil
}

// Segments of a provider flat state and verifier PKCE string
func providerPKCESegments(cookiePayload map[string]string) map[string]string {
	out := map[string]string{}
	for _, seg := range strings.Split(cookiePayload["icebath_session_pkce"], ";") {
		if k, v, ok := strings.Cut(seg, "="); ok {
			out[k] = v
		}
	}
	return out
}

// URL decoded target when it is a safe same origin path, empty otherwise
func validatePostLoginTarget(raw string) string {
	decoded := ""
	if raw != "" {
		decoded = pyurl.Unquote(raw)
	}
	if decoded != "" && isSafeNextPath(decoded) {
		return decoded
	}
	return ""
}

func authSetPKCE(resp ASGIResponse, conn *HTTPConnection, payload map[string]string) {
	setPKCECookie(resp, payload, detectHTTPS(conn), authPrefix(conn), webappCookieGateway(conn))
}

func authClearPKCE(resp ASGIResponse, conn *HTTPConnection) {
	clearPKCECookie(resp, detectHTTPS(conn), authPrefix(conn), webappCookieGateway(conn))
}

// Session cookies of a verified login and the Webapp sign in binding
func authSetSession(resp ASGIResponse, conn *HTTPConnection, session *gateway.DashboardSession) {
	setSessionCookies(resp, session.AccessToken, session.RefreshToken, int(accessTokenMaxAge(session)), detectHTTPS(conn),
		authPrefix(conn), session.Provider, webappCookieGateway(conn))
	webappSignInBind(resp, conn)
}

// JSON body of the native token and refresh endpoints carrying the bearer tokens
func bearerPayload(session *gateway.DashboardSession) *pyjson.Object {
	return pyjson.ObjectOf("access_token", session.AccessToken, "refresh_token", session.RefreshToken,
		"token_type", "Bearer", "expires_at", session.ExpiresAt, "provider", session.Provider, "user_id", session.UserID)
}

// Desktop loopback redirect carrying the one time code minted for a completed native login
func finishNativeLogin(ctx context.Context, conn *HTTPConnection, brokerState string, session *gateway.DashboardSession, provider string) (string, error) {
	pending, err := GetNativePending(brokerState, nil)
	var code string
	if err == nil {
		code, err = CompleteNativePending(brokerState, session, nil)
	}
	if err != nil {
		var nf *NativeFlowError
		if errors.As(err, &nf) {
			authAudit(ctx, conn, AuditNativeTokenFailure, "provider", provider, "reason", "pending_not_found")
			return "", NewHTTPException(400, nativeExpiredDetail)
		}
		return "", err
	}
	sep := "?"
	if strings.Contains(pending.RedirectURI, "?") {
		sep = "&"
	}
	query := pyurl.URLEncode([][2]string{{"code", code}, {"state", pending.ClientState}})
	authAudit(ctx, conn, AuditNativeCodeIssued, "provider", provider, "user_id", session.UserID)
	return pending.RedirectURI + sep + query, nil
}

func loginFailure(ctx context.Context, conn *HTTPConnection, provider, reason string, extra ...any) {
	authAudit(ctx, conn, AuditLoginFailure, append([]any{"provider", provider, "reason", reason}, extra...)...)
}

func loginSuccess(ctx context.Context, conn *HTTPConnection, session *gateway.DashboardSession, provider string) {
	authAudit(ctx, conn, AuditLoginSuccess, "provider", provider, "user_id", session.UserID, "email", session.Email, "org_id", session.OrgID)
}

// Target with the proxy prefix prepended unless it already carries it
func externalizePostLoginTarget(target, prefix string) string {
	prefix = strings.TrimRight(prefix, "/")
	if target == "" || prefix == "" {
		return target
	}
	if target == prefix || strings.HasPrefix(target, prefix+"/") {
		return target
	}
	return prefix + target
}

// Landing of a verified login, the native loopback redirect when a broker state rides along, true for native
func completeLogin(ctx context.Context, conn *HTTPConnection, provider string, session *gateway.DashboardSession, brokerState, nextRaw string) (string, bool, error) {
	loginSuccess(ctx, conn, session, provider)
	if brokerState != "" {
		target, err := finishNativeLogin(ctx, conn, brokerState, session, provider)
		return target, true, err
	}
	landing := validatePostLoginTarget(nextRaw)
	if landing == "" {
		landing = "/"
	}
	return externalizePostLoginTarget(landing, authPrefix(conn)), false, nil
}

// Redirect to the IDP with the PKCE cookie carrying the provider name and the extra segments
func startUpstreamLogin(ctx context.Context, conn *HTTPConnection, p gateway.DashboardAuthProvider, auditFailure bool, extraPKCE map[string]string) (ASGIResponse, error) {
	redirectURI, err := authRedirectURI(ctx, conn)
	if err != nil {
		return nil, err
	}
	ls, err := p.StartLogin(ctx, redirectURI)
	if err != nil {
		var pe *gateway.ProviderError
		if errors.As(err, &pe) {
			if auditFailure {
				loginFailure(ctx, conn, p.Name(), "provider_unreachable")
			}
			return nil, NewHTTPException(503, "Provider unreachable: "+pe.Error())
		}
		return nil, err
	}
	resp := RedirectResponse(ls.RedirectURL, 302)
	pkce := providerPKCESegments(ls.CookiePayload)
	if _, ok := pkce["provider"]; !ok {
		pkce["provider"] = p.Name()
	}
	for k, v := range extraPKCE {
		pkce[k] = v
	}
	authSetPKCE(resp, conn, pkce)
	return resp, nil
}

// Canonical RFC 8252 loopback redirect URI, a 400 for any other spelling
func validateLoopbackRedirectURI(raw string) (string, error) {
	if raw == "" {
		return "", NewHTTPException(400, "redirect_uri required")
	}
	canonicalErr := NewHTTPException(400, "native redirect_uri must use a canonical loopback URL")
	for _, ch := range raw {
		if ch > unicode.MaxASCII || ch <= 0x20 || ch == 0x7F || ch == '\\' {
			return "", canonicalErr
		}
	}
	parsed, err := pyurl.Parse(raw)
	if err != nil {
		return "", canonicalErr
	}
	hostname := parsed.Hostname()
	port, hasPort, err := parsed.Port()
	if err != nil {
		return "", canonicalErr
	}
	if parsed.Scheme != "http" {
		return "", NewHTTPException(400, "native redirect_uri must be http:// on the loopback interface")
	}
	if strings.Contains(raw, "#") {
		return "", NewHTTPException(400, "native redirect_uri must not contain a fragment")
	}
	if strings.Contains(parsed.Netloc, "@") {
		return "", NewHTTPException(400, "native redirect_uri must not contain userinfo")
	}
	canonicalHost, ok := map[string]string{"127.0.0.1": "127.0.0.1", "::1": "[::1]"}[hostname]
	if !ok {
		return "", NewHTTPException(400, "native redirect_uri host must be a loopback IP literal (127.0.0.1 / ::1)")
	}
	canonicalNetloc := canonicalHost
	if hasPort {
		canonicalNetloc += ":" + strconv.Itoa(port)
	}
	if parsed.Netloc != canonicalNetloc {
		return "", NewHTTPException(400, "native redirect_uri must use a canonical loopback authority")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return pyurl.Unparse(parsed), nil
}

// Provider of a native authorize request, the only interactive provider when none is named
func selectNativeProvider(ctx context.Context, provider string) gateway.DashboardAuthProvider {
	if provider != "" {
		return gateway.GetDashboardAuthProvider(ctx, provider, "")
	}
	candidates := gateway.ListInteractiveDashboardAuthProviders(ctx)
	if len(candidates) == 1 {
		return candidates[0]
	}
	return nil
}

const (
	pwRateMaxAttempts = 10
	pwRateWindowSec   = 60.0
)

var (
	pwRateMaxBuckets = 4096
	pwAttemptsMu     sync.Mutex
	pwAttempts       = pydict.New[string, []float64]()
	pwEpoch          = time.Now()
)

// Monotonic seconds the password rate limiter reads
func pwMonotonic() float64 { return time.Since(pwEpoch).Seconds() }

// Drops expired and empty limiter buckets, the caller holding the lock
func prunePasswordRateBucketsLocked(cutoff float64) {
	for _, key := range pwAttempts.Keys() {
		bucket, _ := pwAttempts.Get(key)
		for len(bucket) > 0 && bucket[0] < cutoff {
			bucket = bucket[1:]
		}
		if len(bucket) == 0 {
			pwAttempts.Delete(key)
			continue
		}
		pwAttempts.Set(key, bucket)
	}
}

// True when the ip spent its login budget, the attempt recorded when allowed and an empty ip sharing one bucket
func passwordRateLimited(ip string) bool {
	now := pwMonotonic()
	cutoff := now - pwRateWindowSec
	pwAttemptsMu.Lock()
	defer pwAttemptsMu.Unlock()
	key := ip
	if key == "" {
		key = "_unknown_"
	}
	maxBuckets := max(1, pwRateMaxBuckets)
	if !pwAttempts.Has(key) && pwAttempts.Len() >= maxBuckets {
		prunePasswordRateBucketsLocked(cutoff)
	}
	for !pwAttempts.Has(key) && pwAttempts.Len() >= maxBuckets {
		pwAttempts.Delete(pwAttempts.Keys()[0])
	}
	bucket, ok := pwAttempts.Get(key)
	if ok {
		pwAttempts.Delete(key)
	}
	for len(bucket) > 0 && bucket[0] < cutoff {
		bucket = bucket[1:]
	}
	if len(bucket) >= pwRateMaxAttempts {
		pwAttempts.Set(key, bucket)
		return true
	}
	pwAttempts.Set(key, append(bucket, now))
	return false
}

// Clears every password limiter bucket
func resetPasswordRateLimit() {
	pwAttemptsMu.Lock()
	defer pwAttemptsMu.Unlock()
	pwAttempts.Clear()
}

var passwordLoginBodyModel = &Model{ClassName: "_PasswordLoginBody", Fields: []ModelField{
	Field("provider", StrLen(nil, Bound(128))),
	Field("username", Str),
	Field("password", Str),
	FieldDefault("next", Str, ""),
}}

var nativeTokenBodyModel = &Model{ClassName: "_NativeTokenBody", Fields: []ModelField{
	Field("code", Str),
	Field("code_verifier", Str),
}}

var nativeRefreshBodyModel = &Model{ClassName: "_NativeRefreshBody", Fields: []ModelField{
	Field("refresh_token", Str),
	FieldDefault("provider", Str, ""),
}}

// Verified session the gate attached to the request, nil when none
func requestSession(conn *HTTPConnection) *gateway.DashboardSession {
	sess, _ := conn.State().Get("session").(*gateway.DashboardSession)
	return sess
}

// Login page with next threaded through each provider option
func loginPage(ctx context.Context, req *Request) (ASGIResponse, error) {
	nextPath := validatePostLoginTarget(req.QueryParams().Get("next"))
	page, err := renderLoginHTML(ctx, nextPath, authPrefix(&req.HTTPConnection))
	if err != nil {
		return nil, err
	}
	return HTMLResponse(page, 200, authNoStore...), nil
}

// Interactive providers offered for sign in, 503 when no session provider is registered
func apiAuthProviders(ctx context.Context) (any, error) {
	if len(gateway.ListSessionDashboardAuthProviders(ctx)) == 0 {
		return MustJSONResponse(map[string]any{"detail": "no auth providers registered"}, 503), nil
	}
	providers := []any{}
	for _, p := range gateway.ListInteractiveDashboardAuthProviders(ctx) {
		providers = append(providers, pyjson.ObjectOf("name", p.Name(), "display_name", p.DisplayName(),
			"supports_password", gateway.DashboardAuthSupportsPassword(p)))
	}
	return pyjson.ObjectOf("providers", providers), nil
}

// Starts the browser login of a provider, the password form for a password provider
func authLogin(ctx context.Context, req *Request, provider, next string) (any, error) {
	conn := &req.HTTPConnection
	p := gateway.GetDashboardAuthProvider(ctx, provider, "")
	if p == nil {
		return nil, NewHTTPException(404, "Unknown provider: "+pyrepr.Repr(provider))
	}
	if !gateway.DashboardAuthSupportsSession(p) {
		return nil, NewHTTPException(404, "Provider does not support interactive login: "+pyrepr.Repr(provider))
	}
	if !gateway.DashboardAuthInteractiveLogin(p) {
		return nil, NewHTTPException(404, "Provider has no interactive login: "+pyrepr.Repr(provider))
	}
	safeNext := validatePostLoginTarget(next)
	if gateway.DashboardAuthSupportsPassword(p) {
		loginURL := authPrefix(conn) + "/login"
		if safeNext != "" {
			loginURL += "?next=" + pyurl.Quote(safeNext, "")
		}
		return RedirectResponse(loginURL, 302), nil
	}
	extra := map[string]string{}
	if safeNext != "" {
		extra["next"] = safeNext
	}
	resp, err := startUpstreamLogin(ctx, conn, p, true, extra)
	if err != nil {
		return nil, err
	}
	authAudit(ctx, conn, AuditLoginStart, "provider", provider)
	return resp, nil
}

// Starts an RFC 8252 native app login under an opaque broker state, a chooser when several providers fit
func authNativeAuthorize(ctx context.Context, req *Request, provider, codeChallenge, codeChallengeMethod, redirectURI, state string) (any, error) {
	conn := &req.HTTPConnection
	if strings.ToUpper(codeChallengeMethod) != "S256" {
		return nil, NewHTTPException(400, "code_challenge_method must be S256")
	}
	if codeChallenge == "" {
		return nil, NewHTTPException(400, "code_challenge required")
	}
	redirectURI, err := validateLoopbackRedirectURI(redirectURI)
	if err != nil {
		return nil, err
	}
	p := selectNativeProvider(ctx, provider)
	if p == nil && provider == "" {
		if candidates := gateway.ListInteractiveDashboardAuthProviders(ctx); len(candidates) > 1 {
			page, err := renderNativeProviderChoiceHTML(candidates, authPrefix(conn)+"/auth/native/authorize", codeChallenge,
				codeChallengeMethod, redirectURI, state, authPrefix(conn))
			if err != nil {
				return nil, err
			}
			return HTMLResponse(page, 200, authNoStore...), nil
		}
	}
	if p == nil {
		return nil, NewHTTPException(404, "Unknown provider: "+pyrepr.Repr(provider))
	}
	if !gateway.DashboardAuthSupportsSession(p) || !gateway.DashboardAuthInteractiveLogin(p) {
		return nil, NewHTTPException(400, "Provider does not support native login: "+pyrepr.Repr(p.Name()))
	}
	brokerState, err := RegisterNativePending(codeChallenge, redirectURI, state, clientIP(conn), nil)
	if err != nil {
		var nf *NativeFlowError
		if errors.As(err, &nf) {
			return nil, NewHTTPException(503, nf.Error())
		}
		return nil, err
	}
	if gateway.DashboardAuthSupportsPassword(p) {
		authAudit(ctx, conn, AuditNativeAuthorizeStart, "provider", p.Name())
		resp := RedirectResponse(authPrefix(conn)+"/login", 302)
		authSetPKCE(resp, conn, map[string]string{"provider": p.Name(), "broker": brokerState})
		return resp, nil
	}
	resp, err := startUpstreamLogin(ctx, conn, p, false, map[string]string{"broker": brokerState})
	if err != nil {
		return nil, err
	}
	authAudit(ctx, conn, AuditNativeAuthorizeStart, "provider", p.Name())
	return resp, nil
}

// Completes the OAuth round trip from the PKCE cookie, setting the session cookies or issuing the native code
func authCallback(ctx context.Context, req *Request, code, state, oauthErr, errorDescription string) (any, error) {
	conn := &req.HTTPConnection
	pkceRaw, _ := readPKCECookie(conn, webappCookieGateway(conn))
	if pkceRaw == "" {
		authAudit(ctx, conn, AuditLoginFailure, "reason", "missing_pkce_cookie")
		return nil, NewHTTPException(400, "Missing PKCE state cookie")
	}
	parts := parsePKCEPayload(pkceRaw)
	providerName := parts["provider"]
	p := gateway.GetDashboardAuthProvider(ctx, providerName, "")
	if p == nil {
		return nil, NewHTTPException(400, "Unknown provider in cookie: "+pyrepr.Repr(providerName))
	}
	if oauthErr != "" {
		loginFailure(ctx, conn, providerName, "idp_error", "error", oauthErr)
		return nil, NewHTTPException(400, "OAuth error from provider: "+oauthErr+" ("+errorDescription+")")
	}
	if state == "" || state != parts["state"] {
		loginFailure(ctx, conn, providerName, "state_mismatch")
		return nil, NewHTTPException(400, "OAuth state mismatch (CSRF check failed)")
	}
	redirectURI, err := authRedirectURI(ctx, conn)
	if err != nil {
		return nil, err
	}
	session, err := p.CompleteLogin(ctx, code, state, parts["verifier"], redirectURI)
	if err != nil {
		var ic *gateway.InvalidCodeError
		var pe *gateway.ProviderError
		switch {
		case errors.As(err, &ic):
			loginFailure(ctx, conn, providerName, "invalid_code")
			return nil, NewHTTPException(400, "Invalid code: "+ic.Error())
		case errors.As(err, &pe):
			loginFailure(ctx, conn, providerName, "provider_unreachable")
			return nil, NewHTTPException(503, "Provider unreachable: "+pe.Error())
		}
		return nil, err
	}
	target, native, err := completeLogin(ctx, conn, providerName, session, parts["broker"], parts["next"])
	if err != nil {
		return nil, err
	}
	resp := RedirectResponse(target, 302)
	if !native {
		authSetSession(resp, conn, session)
	}
	authClearPKCE(resp, conn)
	clearSSOAttemptCookie(resp, authPrefix(conn), webappCookieGateway(conn))
	return resp, nil
}

// Verifies a username and password against a password provider, the native loopback target when a broker rides along
func authPasswordLogin(ctx context.Context, req *Request, body *pyjson.Object) (any, error) {
	conn := &req.HTTPConnection
	provider := pyrepr.Str(body.Vals["provider"])
	if passwordRateLimited(clientIP(conn)) {
		loginFailure(ctx, conn, provider, "rate_limited")
		return nil, NewHTTPException(429, "Too many login attempts. Try again shortly.")
	}
	p := gateway.GetDashboardAuthProvider(ctx, provider, "")
	if p == nil || !gateway.DashboardAuthSupportsPassword(p) {
		loginFailure(ctx, conn, provider, "unknown_password_provider")
		return nil, NewHTTPException(404, "Unknown provider")
	}
	pkceRaw, _ := readPKCECookie(conn, webappCookieGateway(conn))
	pkceParts := map[string]string{}
	if pkceRaw != "" {
		pkceParts = parsePKCEPayload(pkceRaw)
	}
	brokerState := pkceParts["broker"]
	if brokerState != "" && pkceParts["provider"] != provider {
		authAudit(ctx, conn, AuditNativeTokenFailure, "provider", provider, "reason", "provider_mismatch")
		return nil, NewHTTPException(400, "This native sign-in was started for a different provider; "+
			"use that provider's form or restart sign-in.")
	}
	session, err := gateway.DashboardAuthCompletePasswordLogin(ctx, p, pyrepr.Str(body.Vals["username"]), pyrepr.Str(body.Vals["password"]))
	if err != nil {
		var ic *gateway.InvalidCredentialsError
		var ni *gateway.DashboardAuthNotImplementedError
		var pe *gateway.ProviderError
		switch {
		case errors.As(err, &ic):
			loginFailure(ctx, conn, provider, "invalid_credentials")
			return nil, NewHTTPException(401, "Invalid credentials")
		case errors.As(err, &ni):
			return nil, NewHTTPException(500, "Provider misconfigured")
		case errors.As(err, &pe):
			loginFailure(ctx, conn, provider, "provider_unreachable")
			return nil, NewHTTPException(503, "Provider unreachable: "+pe.Error())
		}
		return nil, err
	}
	target, native, err := completeLogin(ctx, conn, provider, session, brokerState, pyrepr.Str(body.Vals["next"]))
	if err != nil {
		return nil, err
	}
	resp := MustJSONResponse(pyjson.ObjectOf("ok", true, "next", target), 200)
	if native {
		authClearPKCE(resp, conn)
	} else {
		authSetSession(resp, conn, session)
	}
	return resp, nil
}

// Revokes the refresh token on every provider, clears the cookies and returns to the login page
func authLogout(ctx context.Context, req *Request) (any, error) {
	conn := &req.HTTPConnection
	_, rt := readSessionCookies(conn, webappCookieGateway(conn))
	if rt != "" {
		for _, provider := range gateway.ListDashboardAuthProviders(ctx, "") {
			if err := provider.RevokeSession(ctx, rt); err != nil {
				dashboardAuthRoutesLog().Warn("dashboard-auth: revoke on " + pyrepr.Repr(provider.Name()) + " failed: " + err.Error())
			}
		}
	}
	providerName, userID := "unknown", ""
	if sess := requestSession(conn); sess != nil {
		providerName, userID = sess.Provider, sess.UserID
	}
	authAudit(ctx, conn, AuditLogout, "provider", providerName, "user_id", userID)
	prefix := authPrefix(conn)
	resp := RedirectResponse(prefix+"/login", 302)
	clearSessionCookies(resp, prefix, webappCookieGateway(conn))
	authClearPKCE(resp, conn)
	return resp, nil
}

// Verified session of the request, 401 without one
func requireAuthSession(conn *HTTPConnection) (*gateway.DashboardSession, error) {
	if sess := requestSession(conn); sess != nil {
		return sess, nil
	}
	return nil, NewHTTPException(401, "Unauthorized")
}

// Verified session as JSON, the loopback identity when the session token already authenticated a loopback bind
func apiAuthMe(req *Request) (any, error) {
	sess := requestSession(&req.HTTPConnection)
	if sess == nil {
		if req.App().State.Bool("auth_required") {
			return nil, NewHTTPException(401, "Unauthorized")
		}
		if !hasValidSessionToken(req) {
			return nil, NewHTTPException(401, "Unauthorized")
		}
		return pyjson.ObjectOf("user_id", "local", "email", "", "display_name", "Local", "org_id", "",
			"provider", "loopback", "expires_at", int64(0)), nil
	}
	return pyjson.ObjectOf("user_id", sess.UserID, "email", sess.Email, "display_name", sess.DisplayName,
		"org_id", sess.OrgID, "provider", sess.Provider, "expires_at", sess.ExpiresAt), nil
}

// Single use ticket for one WebSocket upgrade bound to the Webapp sign in when there is one
func apiAuthWSTicket(ctx context.Context, req *Request) (any, error) {
	conn := &req.HTTPConnection
	sess, err := requireAuthSession(conn)
	if err != nil {
		return nil, err
	}
	var extra *pyjson.Object
	if signedIn := webappSignInOf(conn); signedIn != nil && *signedIn != "" {
		extra = pyjson.ObjectOf("sign_in", *signedIn)
	}
	ticket := MintWSTicket(sess.UserID, sess.Provider, sess.Issuer, extra)
	authAudit(ctx, conn, AuditWSTicketMinted, "provider", sess.Provider, "user_id", sess.UserID)
	return pyjson.ObjectOf("ticket", ticket, "ttl_seconds", int64(WSTicketTTLSeconds)), nil
}

// Exchanges a loopback code and PKCE verifier for bearer tokens, the code consumed on every path
func authNativeToken(ctx context.Context, req *Request, body *pyjson.Object) (any, error) {
	conn := &req.HTTPConnection
	session, err := RedeemNativeCode(pyrepr.Str(body.Vals["code"]), pyrepr.Str(body.Vals["code_verifier"]), nil)
	if err != nil {
		var nf *NativeFlowError
		if errors.As(err, &nf) && nf.Kind == "CodeInvalid" {
			authAudit(ctx, conn, AuditNativeTokenFailure, "reason", "invalid_code_or_pkce")
			return nil, NewHTTPException(400, "Invalid or expired authorization code.")
		}
		return nil, err
	}
	webappGaveNativeApp(ctx, conn, session.AccessToken, session.RefreshToken, session.ExpiresAt)
	authAudit(ctx, conn, AuditNativeTokenSuccess, "provider", session.Provider, "user_id", session.UserID)
	return bearerPayload(session), nil
}

// Rotates a desktop held refresh token, 401 session_expired when every provider rejects it
func authNativeRefresh(ctx context.Context, req *Request, body *pyjson.Object) (any, error) {
	conn := &req.HTTPConnection
	refreshToken := pyrepr.Str(body.Vals["refresh_token"])
	expired := pyjson.ObjectOf("error", "session_expired", "detail", "Refresh token expired or invalid; start a new sign-in.")
	if refreshToken == "" {
		return nil, NewHTTPException(400, "refresh_token required")
	}
	if !webappTokenWorksAlone(conn, refreshToken) {
		authAudit(ctx, conn, AuditRefreshFailure, "reason", "not_a_native_sign_in")
		return MustJSONResponse(expired, 401), nil
	}
	refreshed, err := refreshSessionCoalesced(ctx, refreshToken, pyrepr.Str(body.Vals["provider"]), "native refresh",
		dashboardAuthRoutesLog(), nil, nil)
	if err != nil {
		var pe *gateway.ProviderError
		if errors.As(err, &pe) {
			return nil, NewHTTPException(503, "Auth provider "+pyrepr.Repr(pe.Error())+" unreachable")
		}
		return nil, err
	}
	if refreshed != nil {
		session := refreshed.session
		webappGaveNativeApp(ctx, conn, session.AccessToken, session.RefreshToken, session.ExpiresAt)
		authAudit(ctx, conn, AuditRefreshSuccess, "provider", session.Provider, "user_id", session.UserID)
		return bearerPayload(session), nil
	}
	authAudit(ctx, conn, AuditRefreshFailure, "reason", "all_providers_rejected_rt")
	return MustJSONResponse(expired, 401), nil
}

// Router of the dashboard auth OAuth round trip, mounted at the root
func dashboardAuthRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/login", &Endpoint{Name: "login_page", Handle: func(ctx context.Context, c *Call) (any, error) {
		return loginPage(ctx, c.Request)
	}})
	r.Get("/api/auth/providers", &Endpoint{Name: "auth_providers", Handle: func(ctx context.Context, c *Call) (any, error) {
		return apiAuthProviders(ctx)
	}})
	r.Get("/auth/login", &Endpoint{Name: "auth_login",
		Params: []Param{RequiredQuery("provider", Str), QueryParam("next", Str, "")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return authLogin(ctx, c.Request, c.Str("provider"), c.Str("next"))
		}})
	r.Get("/auth/native/authorize", &Endpoint{Name: "auth_native_authorize",
		Doc: `Begin an RFC 8252 native-app login: stash a pending broker authorization keyed by an
    opaque ` + "``broker_state``" + ` riding in the gateway's own PKCE cookie (the desktop's
    challenge/state never touch it), then run the normal upstream round trip. Password providers
    go to the ` + "``/login``" + ` form instead.`,
		Params: []Param{QueryParam("provider", Str, ""), QueryParam("code_challenge", Str, ""),
			QueryParam("code_challenge_method", Str, ""), QueryParam("redirect_uri", Str, ""), QueryParam("state", Str, "")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return authNativeAuthorize(ctx, c.Request, c.Str("provider"), c.Str("code_challenge"),
				c.Str("code_challenge_method"), c.Str("redirect_uri"), c.Str("state"))
		}})
	r.Get("/auth/callback", &Endpoint{Name: "auth_callback",
		Params: []Param{QueryParam("code", Str, ""), QueryParam("state", Str, ""), QueryParam("error", Str, ""),
			QueryParam("error_description", Str, "")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return authCallback(ctx, c.Request, c.Str("code"), c.Str("state"), c.Str("error"), c.Str("error_description"))
		}})
	r.Post("/auth/password-login", &Endpoint{Name: "auth_password_login",
		Doc: `Authenticate a username/password against a password provider.

    Returns ` + "``{\"ok\": true, \"next\": <path>}``" + ` (the form POSTs via fetch, which follows a 302
    opaquely) and sets the session cookies; with a native ` + "``broker``" + ` handle in the PKCE cookie,
    ` + "``next``" + ` is the desktop's loopback redirect and NO cookies are set. Failures are deliberately
    generic (no username/provider oracle): unknown/non-password provider 404, bad credentials
    401, store unreachable 503, rate limited 429.`,
		Body: []BodyParam{Body("body", passwordLoginBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return authPasswordLogin(ctx, c.Request, c.Object("body"))
		}})
	r.Post("/auth/logout", &Endpoint{Name: "auth_logout", Handle: func(ctx context.Context, c *Call) (any, error) {
		return authLogout(ctx, c.Request)
	}})
	r.Get("/api/auth/me", &Endpoint{Name: "auth_me",
		Doc: `Return the verified session as JSON.

    Gated mode: the auth middleware attached a verified Session — return it.
    Loopback mode (` + "``auth_required``" + ` False): there is no OAuth Session, but the
    legacy ` + "``_SESSION_TOKEN``" + ` middleware has already validated the bearer
    token for non-public ` + "``/api/``" + ` routes. Report the loopback identity
    honestly instead of 401-ing (GH #66223). Belt-and-braces: re-verify the
    token so the handler stays safe even if this route were ever allowlisted.`,
		Handle: func(ctx context.Context, c *Call) (any, error) { return apiAuthMe(c.Request) }})
	r.Post("/api/auth/ws-ticket", &Endpoint{Name: "auth_ws_ticket",
		Doc: `Mint a 30s single-use ticket for a WS upgrade (browsers cannot set
    ` + "``Authorization``" + ` on the upgrade); one ticket per WS.`,
		Handle: func(ctx context.Context, c *Call) (any, error) { return apiAuthWSTicket(ctx, c.Request) }})
	r.Post("/auth/native/token", &Endpoint{Name: "auth_native_token",
		Doc: `Exchange a loopback gateway code + PKCE verifier for bearer tokens. The code is consumed
    on every path (no verifier oracle, no replay); any failure is a generic 400. Tokens go in
    the JSON body; no cookie is set.`,
		Body: []BodyParam{Body("body", nativeTokenBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return authNativeToken(ctx, c.Request, c.Object("body"))
		}})
	r.Post("/auth/native/refresh", &Endpoint{Name: "auth_native_refresh",
		Doc: `Rotate a desktop-held refresh token (mirrors the gate's ` + "``_attempt_refresh``" + `): every
    provider rejecting the RT -> 401 ` + "``session_expired``" + ` (desktop re-logs); none rotated and one
    unreachable -> 503. On the gateway's Webapp behind the gate it renews only refresh tokens
    this listener gave a native app's sign-in (` + "``webapp_sign_in.token_works_alone``" + `).`,
		Body: []BodyParam{Body("body", nativeRefreshBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return authNativeRefresh(ctx, c.Request, c.Object("body"))
		}})
	return r
}

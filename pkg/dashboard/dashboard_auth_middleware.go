package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Auth bootstrap routes and static mounts the gate lets through by prefix
var gatePublicPrefixes = []string{
	"/auth/login", "/auth/callback", "/auth/native/authorize", "/auth/native/token",
	"/auth/native/refresh", "/auth/password-login", "/auth/logout", "/login",
	"/api/auth/providers", "/api/mcp/oauth/callback/",
	"/assets/", "/dashboard-plugins/", "/favicon.ico", "/ds-assets/", "/fonts/", "/fonts-terminal/"}

// Page icons the gate lets through by exact path
var gatePublicIcons = map[string]bool{"/apple-touch-icon.png": true}

// Whether a path bypasses the gate, public API paths and icons exactly and bootstrap prefixes by prefix
func pathIsPublic(path string) bool {
	if PublicAPIPaths[path] || gatePublicIcons[path] {
		return true
	}
	for _, p := range gatePublicPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// 403 detail of a cookie write from another origin on the gateway's Webapp
const crossOriginWriteDetail = "Cookie-authenticated writes must come from the Webapp's own origin. " +
	"Behind a reverse proxy, set webapp.public_url to the URL the browser uses."

// Methods a cookie write origin check lets through
var gateSafeMethods = map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true, "TRACE": true}

func dashboardAuthMiddlewareLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.dashboard_auth.middleware")
}

// URL encoded next value for the login redirect, same origin paths outside auth and the API only, prefix prepended after
func safeNextTarget(conn *HTTPConnection) string {
	path := conn.URL().Path()
	if path == "" || !isSafeNextPath(path) {
		return ""
	}
	if prefix := PrefixFromRequest(conn); prefix != "" {
		path = prefix + path
	}
	if query := conn.URL().Query(); query != "" {
		return pyurl.Quote(path+"?"+query, "")
	}
	return pyurl.Quote(path, "")
}

// Whether the surface requires cookie writes from the listener's own origin
func ownOriginWrites(conn *HTTPConnection) bool {
	return PolicyOf(conn.App().State).OwnOriginCookieWrites
}

// 401 JSON with the login URL for API routes, a 302 to the login page for HTML routes
func unauthResponse(ctx context.Context, conn *HTTPConnection, reason string) ASGIResponse {
	nextParam := safeNextTarget(conn)
	prefix := PrefixFromRequest(conn)
	loginURL := prefix + "/login"
	if nextParam != "" {
		loginURL = prefix + "/login?next=" + nextParam
	}
	if strings.HasPrefix(conn.URL().Path(), "/api/") {
		errName := "unauthenticated"
		if reason == "invalid_or_expired_session" {
			errName = "session_expired"
		}
		auditLog(ctx, AuditSessionRejected, "reason", reason, "path", conn.URL().Path(), "ip", clientIP(conn))
		return MustJSONResponse(pyjson.ObjectOf("error", errName, "detail", "Unauthorized", "reason", reason, "login_url", loginURL), 401)
	}
	return RedirectResponse(loginURL, 302)
}

// 302 straight to the provider login on an unauthenticated page load with exactly one OAuth style provider, nil otherwise
func autoSSOResponse(ctx context.Context, conn *HTTPConnection) ASGIResponse {
	if strings.HasPrefix(conn.URL().Path(), "/api/") {
		return nil
	}
	gw := webappCookieGateway(conn)
	if readSSOAttemptCookie(conn, gw) {
		resp := unauthResponse(ctx, conn, "no_cookie")
		clearSSOAttemptCookie(resp, PrefixFromRequest(conn), gw)
		return resp
	}
	providers := gateway.ListInteractiveDashboardAuthProviders(ctx)
	if len(providers) != 1 || gateway.DashboardAuthSupportsPassword(providers[0]) {
		return nil
	}
	provider := providers[0]
	prefix := PrefixFromRequest(conn)
	nextParam := safeNextTarget(conn)
	authLogin := prefix + "/auth/login?provider=" + pyurl.Quote(provider.Name(), "")
	if nextParam != "" {
		authLogin += "&next=" + nextParam
	}
	resp := RedirectResponse(authLogin, 302)
	setSSOAttemptCookie(resp, detectHTTPS(conn), prefix, gw)
	auditLog(ctx, AuditLoginStart, "provider", provider.Name(), "reason", "auto_sso", "ip", clientIP(conn))
	return resp
}

// Session from the provider stack for an access token, the native bearer path skipping the audit records
func verifyAccessToken(ctx context.Context, conn *HTTPConnection, accessToken, providerHint string, audit bool) (*gateway.DashboardSession, error) {
	phase := "bearer verify"
	if audit {
		phase = "verify"
	}
	onUnreachable := func(p gateway.DashboardAuthProvider) {
		if audit {
			auditLog(ctx, AuditSessionVerifyFailure, "provider", p.Name(), "reason", "provider_unreachable", "ip", clientIP(conn))
		}
	}
	return scanSessionProviders(ctx, providerHint, func(p gateway.DashboardAuthProvider) (*gateway.DashboardSession, error) {
		return p.VerifySession(ctx, accessToken)
	}, providerScan{phase: phase, log: dashboardAuthMiddlewareLog(), onUnreachable: onUnreachable})
}

// Verified session a public path request carries, gate checks without side effects, nil when signed out
func PeekSession(ctx context.Context, conn *HTTPConnection) *gateway.DashboardSession {
	var accessToken, providerHint string
	if bearer := extractBearer(conn); bearer != "" {
		if !webappTokenWorksAlone(conn, bearer) {
			return nil
		}
		accessToken = bearer
	} else {
		gw := webappCookieGateway(conn)
		accessToken, _ = readSessionCookies(conn, gw)
		if accessToken == "" || (webappSignInApplies(conn) && !webappSignInCheck(conn)) {
			return nil
		}
		providerHint = readSessionProvider(conn, gw)
	}
	session, err := verifyAccessToken(ctx, conn, accessToken, providerHint, false)
	if err != nil {
		return nil
	}
	return session
}

// Serves the request under a just rotated session and writes the rotated cookies back
func serveRefreshed(ctx context.Context, req *Request, callNext CallNext, refreshed *refreshedSession) (ASGIResponse, error) {
	conn := &req.HTTPConnection
	req.State().Set("session", refreshed.session)
	resp, err := callNext(ctx, req)
	if err != nil {
		return nil, err
	}
	setSessionCookies(resp, refreshed.session.AccessToken, refreshed.session.RefreshToken, int(accessTokenMaxAge(refreshed.session)),
		detectHTTPS(conn), PrefixFromRequest(conn), refreshed.provider, webappCookieGateway(conn))
	webappSignInRenew(resp, conn)
	auditLog(ctx, AuditRefreshSuccess, "provider", refreshed.provider, "user_id", refreshed.session.UserID, "ip", clientIP(conn))
	return resp, nil
}

// Structured 401 or redirect after a failed refresh, clearing the dead cookies under the active prefix
func sessionExpiredResponse(ctx context.Context, conn *HTTPConnection) ASGIResponse {
	auditLog(ctx, AuditSessionVerifyFailure, "reason", "no_provider_recognises", "ip", clientIP(conn))
	resp := unauthResponse(ctx, conn, "invalid_or_expired_session")
	clearSessionCookies(resp, PrefixFromRequest(conn), webappCookieGateway(conn))
	return resp
}

// Cookie and bearer auth gate engaged only when the app requires auth
func GatedAuthMiddleware(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	conn := &req.HTTPConnection
	if !req.App().State.Bool("auth_required") {
		return callNext(ctx, req)
	}
	if req.State().Bool("token_authenticated") || req.State().Get("native_http_principal") != nil {
		return callNext(ctx, req)
	}
	ownOrigin := ownOriginWrites(conn)
	cookieLogout := ownOrigin && req.URL().Path() == "/auth/logout"
	if pathIsPublic(req.URL().Path()) && !cookieLogout {
		return callNext(ctx, req)
	}
	if bearer := extractBearer(conn); bearer != "" {
		if cookieLogout {
			return callNext(ctx, req)
		}
		if !webappTokenWorksAlone(conn, bearer) {
			return unauthResponse(ctx, conn, "invalid_or_expired_session"), nil
		}
		session, err := verifyAccessToken(ctx, conn, bearer, "", false)
		var perr *gateway.ProviderError
		if errors.As(err, &perr) {
			return unreachableResponse(perr.Msg), nil
		}
		if err != nil {
			return nil, err
		}
		if session != nil {
			req.State().Set("session", session)
			return callNext(ctx, req)
		}
		return unauthResponse(ctx, conn, "invalid_or_expired_session"), nil
	}
	gw := webappCookieGateway(conn)
	at, rt := readSessionCookies(conn, gw)
	providerHint := readSessionProvider(conn, gw)
	if ownOrigin && (at != "" || rt != "") && !gateSafeMethods[req.Method()] && !cookieOriginIsAllowed(ctx, req) {
		return MustJSONResponse(map[string]any{"detail": crossOriginWriteDetail}, 403), nil
	}
	if cookieLogout {
		return callNext(ctx, req)
	}
	if at == "" && rt == "" {
		if auto := autoSSOResponse(ctx, conn); auto != nil {
			return auto, nil
		}
		return unauthResponse(ctx, conn, "no_cookie"), nil
	}
	if webappSignInApplies(conn) && !webappSignInCheck(conn) {
		return unauthResponse(ctx, conn, "signed_out"), nil
	}
	var session *gateway.DashboardSession
	if at != "" {
		var err error
		session, err = verifyAccessToken(ctx, conn, at, providerHint, true)
		var perr *gateway.ProviderError
		if errors.As(err, &perr) {
			return unreachableResponse(perr.Msg), nil
		}
		if err != nil {
			return nil, err
		}
	}
	if session == nil {
		refreshed, err := attemptRefresh(ctx, conn, rt, providerHint)
		var perr *gateway.ProviderError
		if errors.As(err, &perr) {
			return unreachableResponse(perr.Msg), nil
		}
		if err != nil {
			return nil, err
		}
		if refreshed == nil {
			return sessionExpiredResponse(ctx, conn), nil
		}
		return serveRefreshed(ctx, req, callNext, refreshed)
	}
	req.State().Set("session", session)
	resp, err := callNext(ctx, req)
	if err != nil {
		return nil, err
	}
	if providerHint == "" && session.Provider != "" {
		setSessionProviderCookie(resp, session.Provider, detectHTTPS(conn), PrefixFromRequest(conn), gw)
	}
	return resp, nil
}

// Rotates an expired session through the refresh token with coalescing, nil when no provider accepts it
func attemptRefresh(ctx context.Context, conn *HTTPConnection, refreshToken, providerHint string) (*refreshedSession, error) {
	if refreshToken == "" {
		return nil, nil
	}
	auditFailure := func(reason string) func(gateway.DashboardAuthProvider) {
		return func(p gateway.DashboardAuthProvider) {
			auditLog(ctx, AuditRefreshFailure, "provider", p.Name(), "reason", reason, "ip", clientIP(conn))
		}
	}
	return refreshSessionCoalesced(ctx, refreshToken, providerHint, "refresh", dashboardAuthMiddlewareLog(),
		auditFailure("refresh_expired"), auditFailure("provider_unreachable"))
}

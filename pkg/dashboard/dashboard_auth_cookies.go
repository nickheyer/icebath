package dashboard

import (
	"encoding/base64"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	SessionATCookie       = "hermes_session_at"
	SessionRTCookie       = "hermes_session_rt"
	SessionProviderCookie = "hermes_session_provider"
	PKCECookie            = "hermes_session_pkce"
	SSOAttemptCookie      = "hermes_sso_attempt"
	WebappSigninCookie    = "hermes_webapp_signin"
	rtCookieMaxAge        = 30 * 24 * 60 * 60
	pkceCookieMaxAge      = 10 * 60
	ssoAttemptMaxAge      = 60
)

// Cookie name prefixes a reader tries, most strict first
var cookieNameVariants = []string{"__Host-", "__Secure-", ""}

// Pure base64url wire form of the current PKCE payload
var b64URLRE = regexp.MustCompile(`^[A-Za-z0-9_-]+={0,2}$`)

// Cookie prefix variant of a bare name for the request shape
func resolvedCookieName(bare string, useHTTPS bool, prefix string) string {
	if !useHTTPS {
		return bare
	}
	if prefix != "" {
		return "__Secure-" + bare
	}
	return "__Host-" + bare
}

// Bare cookie name as the named gateway's Webapp scopes it, unchanged off the Webapp
func gatewayCookieName(bare, gateway string) string {
	if gateway != "" {
		return bare + "_" + gateway
	}
	return bare
}

// Cookie Path of the proxy prefix, root without one
func cookiePath(prefix string) string {
	if prefix != "" {
		return prefix
	}
	return "/"
}

// HttpOnly Lax attributes on the prefix path, Secure over HTTPS
func commonCookieAttrs(useHTTPS bool, prefix string) CookieOptions {
	path, lax := cookiePath(prefix), "lax"
	return CookieOptions{HTTPOnly: true, SameSite: &lax, Path: &path, Secure: useHTTPS}
}

// PKCE attributes shared by set and clear, SameSite None over HTTPS for the cross site redirect chain
func pkceCookieAttrs(useHTTPS bool, prefix string) CookieOptions {
	attrs := commonCookieAttrs(useHTTPS, prefix)
	if useHTTPS {
		none := "none"
		attrs.SameSite = &none
	}
	return attrs
}

// Sets one auth cookie under its resolved name with the given or common attributes
func setAuthCookie(resp ASGIResponse, bare, value string, maxAge int, useHTTPS bool, prefix string, attrs *CookieOptions) {
	o := commonCookieAttrs(useHTTPS, prefix)
	if attrs != nil {
		o = *attrs
	}
	o.MaxAge = &maxAge
	SetCookie(resp, resolvedCookieName(bare, useHTTPS, prefix), value, o)
}

// Persists the non secret provider routing hint for token refresh
func setSessionProviderCookie(resp ASGIResponse, provider string, useHTTPS bool, prefix, gateway string) {
	if provider != "" {
		setAuthCookie(resp, gatewayCookieName(SessionProviderCookie, gateway), provider, rtCookieMaxAge, useHTTPS, prefix, nil)
	}
}

// Sets the access token cookie, the refresh token cookie when one was returned, and the provider hint
func setSessionCookies(resp ASGIResponse, accessToken, refreshToken string, accessTokenExpiresIn int, useHTTPS bool, prefix, provider, gateway string) {
	setAuthCookie(resp, gatewayCookieName(SessionATCookie, gateway), accessToken, accessTokenExpiresIn, useHTTPS, prefix, nil)
	if refreshToken != "" {
		setAuthCookie(resp, gatewayCookieName(SessionRTCookie, gateway), refreshToken, rtCookieMaxAge, useHTTPS, prefix, nil)
	}
	setSessionProviderCookie(resp, provider, useHTTPS, prefix, gateway)
}

// Emits zero Max Age deletions for every name variant, prefixed ones always Secure
func clearCookieVariants(resp ASGIResponse, bareName, prefix, httpsSameSite string, bareAttrs CookieOptions) {
	for _, v := range []struct{ variant, path string }{{"__Host-", "/"}, {"__Secure-", cookiePath(prefix)}} {
		zero, path, sameSite := 0, v.path, httpsSameSite
		SetCookie(resp, v.variant+bareName, "", CookieOptions{MaxAge: &zero, Path: &path, HTTPOnly: true, SameSite: &sameSite, Secure: true})
	}
	zero := 0
	bareAttrs.MaxAge = &zero
	SetCookie(resp, bareName, "", bareAttrs)
}

// Deletes the access, refresh and provider cookies in every variant on the active path
func clearSessionCookies(resp ASGIResponse, prefix, gateway string) {
	bareAttrs := commonCookieAttrs(false, prefix)
	for _, name := range []string{SessionATCookie, SessionRTCookie, SessionProviderCookie} {
		clearCookieVariants(resp, gatewayCookieName(name, gateway), prefix, "lax", bareAttrs)
	}
}

// Wire value of a PKCE payload, unpadded base64url of its sorted compact JSON
func encodePKCEPayload(parts map[string]string) string {
	obj := pyjson.NewObject()
	for k, v := range parts {
		obj.Set(k, v)
	}
	raw, err := pyjson.Dumps(obj, pyjson.Separators(",", ":"), pyjson.SortKeys(true))
	if err != nil {
		panic(err)
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(raw)), "=")
}

// Sets the PKCE state cookie for the ten minute login round trip
func setPKCECookie(resp ASGIResponse, payload map[string]string, useHTTPS bool, prefix, gateway string) {
	attrs := pkceCookieAttrs(useHTTPS, prefix)
	setAuthCookie(resp, gatewayCookieName(PKCECookie, gateway), encodePKCEPayload(payload), pkceCookieMaxAge, useHTTPS, prefix, &attrs)
}

// Deletes every PKCE cookie variant
func clearPKCECookie(resp ASGIResponse, useHTTPS bool, prefix, gateway string) {
	clearCookieVariants(resp, gatewayCookieName(PKCECookie, gateway), prefix, "none", pkceCookieAttrs(useHTTPS, prefix))
}

// First cookie value across the name variants and whether one was present
func readCookieWithFallback(conn *HTTPConnection, bareName string) (string, bool) {
	cookies := conn.Cookies()
	for _, p := range cookieNameVariants {
		if v, ok := cookies[p+bareName]; ok {
			return v, true
		}
	}
	return "", false
}

// Access and refresh token cookies, empty when absent
func readSessionCookies(conn *HTTPConnection, gateway string) (string, string) {
	at, _ := readCookieWithFallback(conn, gatewayCookieName(SessionATCookie, gateway))
	rt, _ := readCookieWithFallback(conn, gatewayCookieName(SessionRTCookie, gateway))
	return at, rt
}

// Provider routing hint of the session cookies, empty when absent
func readSessionProvider(conn *HTTPConnection, gateway string) string {
	v, _ := readCookieWithFallback(conn, gatewayCookieName(SessionProviderCookie, gateway))
	return v
}

// Sets the Webapp sign in binding with the session cookie policy for a refresh token's lifetime
func setWebappSigninCookie(resp ASGIResponse, value string, useHTTPS bool, prefix, gateway string) {
	setAuthCookie(resp, gatewayCookieName(WebappSigninCookie, gateway), value, rtCookieMaxAge, useHTTPS, prefix, nil)
}

// Webapp sign in binding cookie and whether it was present
func readWebappSigninCookie(conn *HTTPConnection, gateway string) (string, bool) {
	return readCookieWithFallback(conn, gatewayCookieName(WebappSigninCookie, gateway))
}

// PKCE state cookie and whether it was present
func readPKCECookie(conn *HTTPConnection, gateway string) (string, bool) {
	return readCookieWithFallback(conn, gatewayCookieName(PKCECookie, gateway))
}

// Segments of a PKCE cookie through base64url JSON, then the raw flat form, then the URL encoded flat form
func parsePKCEPayload(raw string) map[string]string {
	if b64URLRE.MatchString(raw) {
		padded := raw + strings.Repeat("=", (4-len(raw)%4)%4)
		if data, err := base64.URLEncoding.DecodeString(padded); err == nil {
			if decoded, err := pyjson.Loads(string(data)); err == nil {
				if obj, ok := decoded.(*pyjson.Object); ok {
					out := make(map[string]string, len(obj.Keys))
					for _, k := range obj.Keys {
						out[k] = pyrepr.Str(obj.Vals[k])
					}
					return out
				}
			}
		}
	}
	flat := raw
	if !strings.Contains(raw, ";") {
		flat = pyurl.Unquote(raw)
	}
	out := map[string]string{}
	for _, seg := range strings.Split(flat, ";") {
		if k, v, found := strings.Cut(seg, "="); found {
			out[k] = v
		}
	}
	return out
}

// Sets the auto SSO loop guard marker
func setSSOAttemptCookie(resp ASGIResponse, useHTTPS bool, prefix, gateway string) {
	setAuthCookie(resp, gatewayCookieName(SSOAttemptCookie, gateway), "1", ssoAttemptMaxAge, useHTTPS, prefix, nil)
}

// Whether the auto SSO marker is present in any variant
func readSSOAttemptCookie(conn *HTTPConnection, gateway string) bool {
	v, ok := readCookieWithFallback(conn, gatewayCookieName(SSOAttemptCookie, gateway))
	return ok && v != ""
}

// Deletes the auto SSO marker in every variant
func clearSSOAttemptCookie(resp ASGIResponse, prefix, gateway string) {
	clearCookieVariants(resp, gatewayCookieName(SSOAttemptCookie, gateway), prefix, "lax", commonCookieAttrs(false, prefix))
}

// Whether the request arrived over HTTPS, honouring trusted proxy headers
func detectHTTPS(conn *HTTPConnection) bool {
	return conn.URL().Scheme() == "https"
}

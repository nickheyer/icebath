package dashboard

import (
	"encoding/base64"
	"maps"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Set-Cookie headers of the set, set-pkce and clear endpoints of the cookie test app
func cookieEndpoint(t *testing.T, endpoint string, useHTTPS bool, prefix string) []string {
	t.Helper()
	r := PlainTextResponse("ok", 200)
	switch endpoint {
	case "set":
		setSessionCookies(r, "AT", "RT", 3600, useHTTPS, prefix, "nous", "")
	case "set-pkce":
		setPKCECookie(r, map[string]string{"provider": "plain", "state": "s", "verifier": "v"}, useHTTPS, prefix, "")
	case "clear":
		clearSessionCookies(r, prefix, "")
		clearPKCECookie(r, useHTTPS, prefix, "")
	}
	return r.Header().GetList("set-cookie")
}

func findCookie(t *testing.T, cookies []string, pred func(string) bool) string {
	t.Helper()
	for _, c := range cookies {
		if pred(c) {
			return c
		}
	}
	t.Fatalf("no matching cookie in %q", cookies)
	return ""
}

func startsWith(p string) func(string) bool {
	return func(c string) bool { return strings.HasPrefix(c, p) }
}

func TestSessionCookiesUseHostPrefixOnHttpsDirect(t *testing.T) {
	cookies := cookieEndpoint(t, "set", true, "")
	for _, c := range []string{findCookie(t, cookies, startsWith("__Host-"+SessionATCookie+"=")),
		findCookie(t, cookies, startsWith("__Host-"+SessionRTCookie+"=")),
		findCookie(t, cookies, startsWith("__Host-"+SessionProviderCookie+"=nous"))} {
		if !strings.Contains(c, "HttpOnly") || !strings.Contains(strings.ToLower(c), "samesite=lax") ||
			!strings.Contains(c, "Secure") || !strings.Contains(c, "Path=/") {
			t.Fatalf("cookie %q", c)
		}
	}
}

func TestSessionCookiesUseSecurePrefixWhenProxied(t *testing.T) {
	cookies := cookieEndpoint(t, "set", true, "/hermes")
	at := findCookie(t, cookies, startsWith("__Secure-"+SessionATCookie+"="))
	if !strings.Contains(at, "Path=/hermes") || !strings.Contains(at, "Secure") {
		t.Fatalf("cookie %q", at)
	}
	for _, c := range cookies {
		if strings.HasPrefix(c, "__Host-"+SessionATCookie+"=") {
			t.Fatal("__Host- variant emitted on the prefix path")
		}
	}
}

func TestSessionCookiesUseBareNameOnHttp(t *testing.T) {
	cookies := cookieEndpoint(t, "set", false, "")
	at := findCookie(t, cookies, startsWith(SessionATCookie+"="))
	for _, c := range cookies {
		if strings.HasPrefix(c, "__Host-"+SessionATCookie+"=") || strings.HasPrefix(c, "__Secure-"+SessionATCookie+"=") {
			t.Fatal("prefixed variant emitted on HTTP")
		}
	}
	if strings.Contains(at, "; Secure") {
		t.Fatalf("cookie %q carries Secure on HTTP", at)
	}
}

func TestReadSessionCookiesFromRequestSecurePrefix(t *testing.T) {
	req := NewRequest(&Scope{Type: "http", Method: "GET", Path: "/", Headers: []Header{{"cookie",
		"__Secure-" + SessionATCookie + "=at_value; __Secure-" + SessionRTCookie + "=rt_value"}}}, nil, nil)
	at, rt := readSessionCookies(&req.HTTPConnection, "")
	if at != "at_value" || rt != "rt_value" {
		t.Fatalf("got %q %q", at, rt)
	}
}

func TestSetPkceCookieWireValueIsCookieOctetBase64urlJson(t *testing.T) {
	pkce := findCookie(t, cookieEndpoint(t, "set-pkce", true, ""), startsWith("__Host-"+PKCECookie+"="))
	pair, _, _ := strings.Cut(pkce, ";")
	_, wire, _ := strings.Cut(pair, "=")
	if strings.ContainsAny(wire, ";\"\\") {
		t.Fatalf("non-cookie-octet chars leaked into the wire value: %q", wire)
	}
	const b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	if strings.Trim(wire, b64url) != "" {
		t.Fatalf("non-base64url chars in the wire value: %q", wire)
	}
	raw, err := base64.URLEncoding.DecodeString(wire + strings.Repeat("=", (4-len(wire)%4)%4))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"provider": "plain", "state": "s", "verifier": "v"}
	if !maps.Equal(decoded.Vals, want) {
		t.Fatalf("payload %v", decoded.Vals)
	}
}

func TestEncodeParsePkcePayloadRoundTripsHostileValues(t *testing.T) {
	payload := map[string]string{"provider": "plain", "state": `s;t="a\te"`, "verifier": "v=1%3B;x",
		"next": "/sessions?x=a;b&project=foo%25"}
	if got := parsePKCEPayload(encodePKCEPayload(payload)); !maps.Equal(got, payload) {
		t.Fatalf("got %v", got)
	}
}

func TestParsePkcePayloadOldFormatCookieSurvivesRollingUpgrade(t *testing.T) {
	old := "provider=plain;state=s123;verifier=v456;next=%2Fsessions%3Fx%3Da%3Bb%26project%3Dfoo"
	want := map[string]string{"provider": "plain", "state": "s123", "verifier": "v456",
		"next": "%2Fsessions%3Fx%3Da%3Bb%26project%3Dfoo"}
	if got := parsePKCEPayload(old); !maps.Equal(got, want) {
		t.Fatalf("old-format cookie mis-parsed: %v", got)
	}
}

func TestParsePkcePayload99176UrlEncodedFormatSurvivesUpgrade(t *testing.T) {
	wire := pyurl.Quote("provider=plain;state=s123;verifier=v456;next=%2Fsessions", "")
	if strings.Contains(wire, ";") {
		t.Fatal("quoted wire value kept a raw semicolon")
	}
	want := map[string]string{"provider": "plain", "state": "s123", "verifier": "v456", "next": "%2Fsessions"}
	if got := parsePKCEPayload(wire); !maps.Equal(got, want) {
		t.Fatalf("#99176-format wire value mis-parsed: %v", got)
	}
}

func TestPkceCookieHttpsIsSamesiteNoneSecure(t *testing.T) {
	pkce := findCookie(t, cookieEndpoint(t, "set-pkce", true, ""), startsWith("__Host-"+PKCECookie+"="))
	if !strings.Contains(strings.ToLower(pkce), "samesite=none") || !strings.Contains(pkce, "; Secure") || !strings.Contains(pkce, "HttpOnly") {
		t.Fatalf("cookie %q", pkce)
	}
}

func TestPkceCookieHttpStaysLaxWithoutSecure(t *testing.T) {
	pkce := findCookie(t, cookieEndpoint(t, "set-pkce", false, ""), startsWith(PKCECookie+"="))
	if !strings.Contains(strings.ToLower(pkce), "samesite=lax") || strings.Contains(pkce, "; Secure") {
		t.Fatalf("cookie %q", pkce)
	}
}

func TestClearPkceCookieHttpsMatchesSetShape(t *testing.T) {
	cookies := cookieEndpoint(t, "clear", true, "")
	for _, name := range []string{"__Host-" + PKCECookie, "__Secure-" + PKCECookie, PKCECookie} {
		d := findCookie(t, cookies, startsWith(name+`="`))
		if !strings.Contains(d, "Max-Age=0") || !strings.Contains(strings.ToLower(d), "samesite=none") || !strings.Contains(d, "; Secure") {
			t.Fatalf("deletion %q", d)
		}
	}
}

func TestClearPkceCookieHttpBareDeletionIsInsecureLax(t *testing.T) {
	cookies := cookieEndpoint(t, "clear", false, "")
	bare := findCookie(t, cookies, startsWith(PKCECookie+`="`))
	if !strings.Contains(bare, "Max-Age=0") || !strings.Contains(strings.ToLower(bare), "samesite=lax") || strings.Contains(bare, "; Secure") {
		t.Fatalf("bare deletion %q", bare)
	}
	for _, name := range []string{"__Host-" + PKCECookie, "__Secure-" + PKCECookie} {
		d := findCookie(t, cookies, startsWith(name+`="`))
		if !strings.Contains(d, "Max-Age=0") || !strings.Contains(d, "; Secure") {
			t.Fatalf("deletion %q", d)
		}
	}
}

func TestClearSessionCookiesPrefixedDeletionsCarrySecure(t *testing.T) {
	cookies := cookieEndpoint(t, "clear", true, "")
	for _, name := range []string{SessionATCookie, SessionRTCookie, SessionProviderCookie} {
		host := findCookie(t, cookies, startsWith("__Host-"+name+`="`))
		if !strings.Contains(host, "; Secure") || !(strings.Contains(host, "Path=/;") || strings.HasSuffix(strings.TrimSpace(host), "Path=/")) {
			t.Fatalf("host deletion %q", host)
		}
		if secure := findCookie(t, cookies, startsWith("__Secure-"+name+`="`)); !strings.Contains(secure, "; Secure") {
			t.Fatalf("secure deletion %q", secure)
		}
		bare := findCookie(t, cookies, startsWith(name+`="`))
		if strings.Contains(bare, "; Secure") || !strings.Contains(bare, "Max-Age=0") {
			t.Fatalf("bare deletion %q", bare)
		}
	}
}

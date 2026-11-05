package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOAuthStorageRoundtripTokens(t *testing.T) {
	dir := oauthHome(t)
	s := NewTokenStorage(context.Background(), "test-server", "")
	if tok, err := s.GetTokens(); err != nil || tok != nil {
		t.Fatalf("empty storage returned %v %v", tok, err)
	}
	if err := s.SetTokens(bearer("abc123", "ref456")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp-tokens", "test-server.json")
	if got := readJSONFile(t, path)["access_token"]; got != "abc123" {
		t.Fatalf("access_token %v", got)
	}
}

func TestOAuthStorageTokenFileCreatedWith0600(t *testing.T) {
	dir := oauthHome(t)
	s := NewTokenStorage(context.Background(), "perm-test-server", "")
	if err := s.SetTokens(bearer("secret-abc", "secret-ref")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp-tokens", "perm-test-server.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %o", st.Mode().Perm())
	}
	pst, _ := os.Stat(filepath.Dir(path))
	if pst.Mode().Perm() != 0o700 {
		t.Fatalf("token parent dir mode %o", pst.Mode().Perm())
	}
}

func TestOAuthStorageClientInfoWithSecretUsesClientSecretPost(t *testing.T) {
	dir := oauthHome(t)
	s := NewTokenStorage(context.Background(), "supabase", "")
	info, err := validateClientInfo(map[string]any{"client_id": "client-id", "client_secret": "secret",
		"redirect_uris": []any{"http://127.0.0.1:12345/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientInfo(info); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetClientInfo()
	if err != nil || loaded == nil || derefStr(loaded.TokenEndpointAuthMethod) != "client_secret_post" {
		t.Fatalf("loaded %+v %v", loaded, err)
	}
	if got := readJSONFile(t, filepath.Join(dir, "mcp-tokens", "supabase.client.json"))["token_endpoint_auth_method"]; got != "client_secret_post" {
		t.Fatalf("persisted method %v", got)
	}
}

func TestOAuthStorageClientInfoWithSecretAndNoneMethodIsCoerced(t *testing.T) {
	dir := oauthHome(t)
	path := writeTokenFile(t, dir, "supabase.client.json", map[string]any{"client_id": "client-id", "client_secret": "secret",
		"redirect_uris": []any{"http://127.0.0.1:12345/callback"}, "token_endpoint_auth_method": "none"})
	loaded, err := NewTokenStorage(context.Background(), "supabase", "").GetClientInfo()
	if err != nil || loaded == nil || derefStr(loaded.TokenEndpointAuthMethod) != "client_secret_post" {
		t.Fatalf("loaded %+v %v", loaded, err)
	}
	if got := readJSONFile(t, path)["token_endpoint_auth_method"]; got != "client_secret_post" {
		t.Fatalf("persisted method %v", got)
	}
}

func TestOAuthStorageCorruptTokensReturnsNone(t *testing.T) {
	dir := oauthHome(t)
	s := NewTokenStorage(context.Background(), "bad-server", "")
	writeTokenFile(t, dir, "bad-server.json", "NOT VALID JSON{{{")
	if tok, err := s.GetTokens(); err != nil || tok != nil {
		t.Fatalf("corrupt tokens returned %v %v", tok, err)
	}
	for _, raw := range []string{"NOT VALID JSON{{{", "[]", "null", `"cached-secret"`, "42", "true", "{}"} {
		path := writeTokenFile(t, dir, "bad-server.meta.json", raw)
		if md := s.LoadOAuthMetadata(); md != nil {
			t.Fatalf("%s loaded as %+v", raw, md)
		}
		if data, _ := os.ReadFile(path); string(data) != raw {
			t.Fatalf("%s rewritten as %s", raw, data)
		}
	}
	metadata := map[string]any{"issuer": "https://example.com", "token_endpoint": "https://example.com/token",
		"response_types_supported": []any{"code"}, "authorization_endpoint": "https://example.com/auth"}
	for _, device := range []bool{false, true} {
		if device {
			delete(metadata, "authorization_endpoint")
			metadata["device_authorization_endpoint"] = "https://example.com/device"
		}
		path := writeTokenFile(t, dir, "bad-server.meta.json", metadata)
		md := s.LoadOAuthMetadata()
		if md == nil || md.IsDevice() != device || md.TokenEndpoint != "https://example.com/token" {
			t.Fatalf("device=%v loaded %+v", device, md)
		}
		if got := readJSONFile(t, path); fmt.Sprint(got) != fmt.Sprint(metadata) {
			t.Fatalf("metadata rewritten %v", got)
		}
	}
}

func TestOAuthStorageCorruptTokensWarningNeverEchoesTokenMaterial(t *testing.T) {
	dir := oauthHome(t)
	logs := captureLogs(t)
	secret := "sk-live-QQQQQQQQ"
	writeTokenFile(t, dir, "bad-server.json", map[string]any{"access_token": []any{secret}, "token_type": "Bearer"})
	if tok, err := NewTokenStorage(context.Background(), "bad-server", "").GetTokens(); err != nil || tok != nil {
		t.Fatalf("corrupt tokens returned %v %v", tok, err)
	}
	if logs.count("Corrupt") == 0 || strings.Contains(logs.text(), secret) {
		t.Fatalf("logs %q", logs.text())
	}
}

func TestOAuthBuildScopePassedThrough(t *testing.T) {
	oauthHome(t)
	setConsole(t, true)
	p, err := BuildOAuthAuth(context.Background(), "scoped", "https://example.com/mcp", map[string]any{"scope": "read write admin"}, HTTPOptions{})
	if err != nil || p == nil {
		t.Fatal(err)
	}
	if got := derefStr(p.ClientMetadata().Scope); got != "read write admin" {
		t.Fatalf("scope %q", got)
	}
}

func TestOAuthBuildTokenResponseAccepts201Created(t *testing.T) {
	dir := oauthHome(t)
	setConsole(t, true)
	p, err := BuildOAuthAuth(context.Background(), "supabase", "https://mcp.supabase.com/mcp", nil, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"access_token": "access-token", "token_type": "Bearer", "refresh_token": "refresh-token"}`)
	if err := p.handleTokenResponse(&flowResponse{StatusCode: 201, Body: body}); err != nil {
		t.Fatal(err)
	}
	if tok := p.CurrentTokens(); tok == nil || tok.AccessToken != "access-token" {
		t.Fatalf("current tokens %+v", tok)
	}
	if got := readJSONFile(t, filepath.Join(dir, "mcp-tokens", "supabase.json"))["access_token"]; got != "access-token" {
		t.Fatalf("persisted %v", got)
	}
}

func TestOAuthBuildFailedTokenExchangeCarriesBoundedRedactedExcerpt(t *testing.T) {
	oauthHome(t)
	setConsole(t, true)
	p, err := BuildOAuthAuth(context.Background(), "supabase", "https://mcp.supabase.com/mcp", nil, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waf := "<HTML><HEAD><TITLE>ERROR</TITLE></HEAD><BODY><H1>403 ERROR</H1>\n  Request blocked.\n" +
		"Bearer leaked-bearer-token <PRE>" + strings.Repeat("x", 400) + "</PRE></BODY></HTML>"
	err = p.handleTokenResponse(&flowResponse{StatusCode: 403, Body: []byte(waf)})
	var te *OAuthTokenError
	if !errors.As(err, &te) {
		t.Fatalf("error %T %v", err, err)
	}
	msg := te.Error()
	if !strings.Contains(msg, "Request blocked.") || !strings.Contains(msg, "[REDACTED]") || strings.Contains(msg, "<") ||
		strings.Contains(msg, "leaked-bearer-token") || len([]rune(msg)) > len("Token exchange failed (403): ")+200 {
		t.Fatalf("message %q", msg)
	}
	if p.CurrentTokens() != nil {
		t.Fatal("tokens installed on failure")
	}
}

func TestOAuthCanOpenBrowserFalseInSSH(t *testing.T) {
	t.Setenv("SSH_CLIENT", "1.2.3.4 1234 22")
	if canOpenBrowser() {
		t.Fatal("browser allowed over ssh")
	}
}

func TestOAuthCanOpenBrowserTrueWithDisplay(t *testing.T) {
	t.Setenv("SSH_CLIENT", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	if !canOpenBrowser() {
		t.Fatal("browser refused with display")
	}
}

func TestOAuthPathTraversalDotsAndSlashesSanitized(t *testing.T) {
	dir := oauthHome(t)
	path, _ := filepath.Abs(NewTokenStorage(context.Background(), "../../../etc/passwd", "").TokensPath())
	rel, err := filepath.Rel(filepath.Join(dir, "mcp-tokens"), path)
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, string(filepath.Separator)) {
		t.Fatalf("path %s escapes the token dir", path)
	}
}

func TestOAuthPathTraversalNormalNameUnchanged(t *testing.T) {
	oauthHome(t)
	if p := NewTokenStorage(context.Background(), "my-mcp-server", "").TokensPath(); !strings.Contains(p, "my-mcp-server.json") {
		t.Fatalf("path %s", p)
	}
}

func TestOAuthPathTraversalSpecialCharsSanitized(t *testing.T) {
	oauthHome(t)
	name := filepath.Base(NewTokenStorage(context.Background(), "server@host:8080/path", "").TokensPath())
	if strings.ContainsAny(name, "@:/") {
		t.Fatalf("name %s", name)
	}
}

// Drives one GET through a fresh callback handler, returning its body and result
func fakeCallbackGet(path string) (string, *callbackResult) {
	h, result := makeCallbackHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Body.String(), result
}

func TestOAuthCallbackHandlerWritesToOwnResult(t *testing.T) {
	_, fresh := makeCallbackHandler()
	if code, _, _, _, _ := fresh.snapshot(); code != "" {
		t.Fatal("fresh result has a code")
	}
	_, result := fakeCallbackGet("/callback?code=test123&state=mystate")
	if code, state, _, _, _ := result.snapshot(); code != "test123" || state != "mystate" {
		t.Fatalf("code %q state %q", code, state)
	}
}

func TestOAuthCallbackHandlerCapturesError(t *testing.T) {
	_, result := fakeCallbackGet("/callback?error=access_denied")
	if code, _, errText, _, _ := result.snapshot(); code != "" || errText != "access_denied" {
		t.Fatalf("code %q error %q", code, errText)
	}
}

func TestOAuthCallbackHandlerHostileErrorIsEscaped(t *testing.T) {
	body, result := fakeCallbackGet("/callback?error=" + url.QueryEscape("<script>alert(1)</script>"))
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("body %q", body)
	}
	if _, _, errText, _, _ := result.snapshot(); errText != "<script>alert(1)</script>" {
		t.Fatalf("error %q", errText)
	}
}

func TestOAuthReservedPortCannotBeStolen(t *testing.T) {
	resetOAuthState(t)
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	if ln, err := netListen4(port); err == nil {
		ln.Close()
		t.Fatalf("reserved port %d was bound again", port)
	}
}

func TestOAuthPinnedPortIsNotReserved(t *testing.T) {
	resetOAuthState(t)
	cfg := map[string]any{"redirect_port": 49399}
	port, err := configureCallbackPort(context.Background(), cfg, nil)
	if err != nil || port != 49399 || cfg["_resolved_port"] != 49399 || isReserved(49399) {
		t.Fatalf("port %d cfg %v err %v", port, cfg, err)
	}
}

func TestOAuthWaitForCallbackAdoptsReservedSocket(t *testing.T) {
	resetOAuthState(t)
	cfg := map[string]any{"cimd": false}
	port, err := configureCallbackPort(context.Background(), cfg, nil)
	if err != nil || !isReserved(port) {
		t.Fatalf("port %d reserved %v err %v", port, isReserved(port), err)
	}
	blockingStdin(t)
	ch := runWaiter(ForceInteractiveOAuth(context.Background()), makeCallbackWaiter(port, "", 20*time.Second))
	if err := hitCallbackWhenReady(fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc123&state=xyz", port), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	o := awaitWaiter(t, ch, 20*time.Second)
	if o.err != nil || o.res.Code != "abc123" || o.res.State != "xyz" || isReserved(port) {
		t.Fatalf("outcome %+v %v reserved %v", o.res, o.err, isReserved(port))
	}
}

func TestOAuthConcurrentFlowsKeepTheirOwnCallbackPorts(t *testing.T) {
	resetOAuthState(t)
	blockingStdin(t)
	portA, errA := configureCallbackPort(context.Background(), map[string]any{"cimd": false}, nil)
	waiterA := makeCallbackWaiter(portA, "", 20*time.Second)
	portB, errB := configureCallbackPort(context.Background(), map[string]any{"cimd": false}, nil)
	if errA != nil || errB != nil || portA == portB {
		t.Fatalf("ports %d %d errs %v %v", portA, portB, errA, errB)
	}
	ch := runWaiter(ForceInteractiveOAuth(context.Background()), waiterA)
	if err := hitCallbackWhenReady(fmt.Sprintf("http://127.0.0.1:%d/callback?code=flowA&state=sA", portA), 15*time.Second); err != nil {
		t.Fatal(err)
	}
	o := awaitWaiter(t, ch, 20*time.Second)
	if o.err != nil || o.res.Code != "flowA" || o.res.State != "sA" {
		t.Fatalf("outcome %+v %v", o.res, o.err)
	}
}

// Storage under a temp home whose client file holds the payload verbatim
func seedClientInfo(t *testing.T, payload any) *TokenStorage {
	t.Helper()
	dir := t.TempDir()
	s := NewTokenStorage(context.Background(), "srv", dir)
	writeTokenFile(t, dir, "srv.client.json", payload)
	return s
}

func TestOAuthCachedRedirectSkipsMalformedEntries(t *testing.T) {
	for _, bad := range []string{"http://127.0.0.1:abc/callback", "http://127.0.0.1:99999/callback", "http://[bad/callback"} {
		s := seedClientInfo(t, map[string]any{"client_id": "client-a",
			"redirect_uris": []any{bad, "http://127.0.0.1:1455/callback", "https://proxy.example.com/cb"}})
		if uri, port := cachedRedirect(s); uri != "https://proxy.example.com/cb" || port != 1455 {
			t.Fatalf("%s gave %q %d", bad, uri, port)
		}
	}
}

func TestOAuthCachedRedirectToleratesMisshapedClientInfo(t *testing.T) {
	for _, payload := range []any{[]any{"not", "a", "dict"}, map[string]any{"redirect_uris": 123},
		map[string]any{"redirect_uris": map[string]any{"a": 1}}, map[string]any{"redirect_uris": nil}, map[string]any{"client_id": "c"}} {
		if uri, port := cachedRedirect(seedClientInfo(t, payload)); uri != "" || port != 0 {
			t.Fatalf("%v gave %q %d", payload, uri, port)
		}
	}
}

func TestOAuthMalformedClientInfoFlowReservesFreshEphemeralPort(t *testing.T) {
	for _, payload := range []any{map[string]any{"client_id": "c", "redirect_uris": []any{"http://127.0.0.1:abc/callback"}}, []any{"x"}} {
		resetOAuthState(t)
		s := seedClientInfo(t, payload)
		cfg := map[string]any{"cimd": false}
		port, err := configureCallbackPort(context.Background(), cfg, s)
		if err != nil || port <= 0 || cfg["_resolved_port"] != port || !isReserved(port) {
			t.Fatalf("%v port %d cfg %v err %v", payload, port, cfg, err)
		}
		if info, err := s.GetClientInfo(); info != nil || err != nil {
			t.Fatalf("%v client info %+v %v", payload, info, err)
		}
	}
}

func TestOAuthRemoveTokensRemovesFiles(t *testing.T) {
	dir := oauthHome(t)
	a := writeTokenFile(t, dir, "myserver.json", "{}")
	b := writeTokenFile(t, dir, "myserver.client.json", "{}")
	if err := RemoveOAuthTokens(context.Background(), "myserver", ""); err != nil {
		t.Fatal(err)
	}
	if exists(a) || exists(b) {
		t.Fatal("token files survived")
	}
}

// Seeds client, token and metadata files for the client change tests
func seedClientChange(t *testing.T, clientID, secret string) (*TokenStorage, string) {
	t.Helper()
	dir := oauthHome(t)
	info := map[string]any{"client_id": clientID, "redirect_uris": []any{"http://localhost:1455/callback"}}
	if secret != "" {
		info["client_secret"] = secret
	}
	writeTokenFile(t, dir, "chg-server.client.json", info)
	writeTokenFile(t, dir, "chg-server.json", map[string]any{"access_token": "old-token", "token_type": "Bearer"})
	writeTokenFile(t, dir, "chg-server.meta.json", map[string]any{"issuer": "https://idp.example",
		"authorization_endpoint": "https://idp.example/auth", "token_endpoint": "https://idp.example/token"})
	return NewTokenStorage(context.Background(), "chg-server", ""), filepath.Join(dir, "mcp-tokens")
}

func TestOAuthClientChangeChangedClientIDDropsTokens(t *testing.T) {
	s, d := seedClientChange(t, "client-a", "")
	invalidateTokensOnClientChange(s, "client-b", nil)
	if exists(filepath.Join(d, "chg-server.json")) || exists(filepath.Join(d, "chg-server.meta.json")) || !exists(filepath.Join(d, "chg-server.client.json")) {
		t.Fatal("wrong files survived a client id change")
	}
}

func TestOAuthClientChangeChangedSecretDropsTokens(t *testing.T) {
	s, d := seedClientChange(t, "client-a", "old-secret")
	invalidateTokensOnClientChange(s, "client-a", "new-secret")
	if exists(filepath.Join(d, "chg-server.json")) {
		t.Fatal("tokens survived a secret change")
	}
}

func TestOAuthClientChangeSameClientPreservesTokens(t *testing.T) {
	s, d := seedClientChange(t, "client-a", "sec")
	invalidateTokensOnClientChange(s, "client-a", "sec")
	if !exists(filepath.Join(d, "chg-server.json")) || !exists(filepath.Join(d, "chg-server.meta.json")) {
		t.Fatal("unchanged client lost its tokens")
	}
}

func TestOAuthClientChangeNoPriorClientInfoIsNoop(t *testing.T) {
	dir := oauthHome(t)
	path := writeTokenFile(t, dir, "fresh-server.json", map[string]any{"access_token": "tok", "token_type": "Bearer"})
	invalidateTokensOnClientChange(NewTokenStorage(context.Background(), "fresh-server", ""), "client-x", nil)
	if !exists(path) {
		t.Fatal("tokens removed without prior client info")
	}
}

func TestOAuthClientChangePreregisterFlowInvalidatesEndToEnd(t *testing.T) {
	s, d := seedClientChange(t, "client-a", "")
	cfg := map[string]any{"client_id": "client-b", "_resolved_port": 1455}
	md, err := buildClientMetadata(map[string]any{"client_id": "client-b", "_resolved_port": 1455})
	if err != nil {
		t.Fatal(err)
	}
	if err := maybePreregisterClient(s, cfg, md); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(d, "chg-server.json")) {
		t.Fatal("tokens minted under client-a survived the switch to client-b")
	}
	if got := readJSONFile(t, filepath.Join(d, "chg-server.client.json"))["client_id"]; got != "client-b" {
		t.Fatalf("client_id %v", got)
	}
}

func TestOAuthSuppressInteractiveDisablesStdinPrompts(t *testing.T) {
	setConsole(t, true)
	ctx := context.Background()
	if !isInteractive(ctx) || isInteractive(SuppressInteractiveOAuth(ctx)) || !isInteractive(ctx) {
		t.Fatal("suppression did not scope to its context")
	}
}

func TestOAuthSuppressionPropagatesAcrossGoroutines(t *testing.T) {
	setConsole(t, true)
	ctx := SuppressInteractiveOAuth(context.Background())
	got := make(chan bool)
	go func() { got <- isInteractive(ctx) }()
	if <-got {
		t.Fatal("suppression lost across goroutines")
	}
}

func TestOAuthWaitForCallbackRaisesOnTimeoutInsteadOfInput(t *testing.T) {
	resetOAuthState(t)
	fakeStdin(t, func() (string, error) { return "", nil })
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	_, err = makeCallbackWaiter(port, "", 10*time.Millisecond)(ForceInteractiveOAuth(context.Background()))
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) || !strings.Contains(err.Error(), "callback timed out") {
		t.Fatalf("error %v", err)
	}
}

func TestOAuthBuildNonInteractiveWithoutCachedTokensFailsFast(t *testing.T) {
	oauthHome(t)
	setConsole(t, false)
	_, err := BuildOAuthAuth(context.Background(), "atlassian", "https://mcp.atlassian.com/v1/mcp", nil, HTTPOptions{})
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("error %v", err)
	}
}

func TestOAuthWaitForCallbackRejectsBeforeBindingWhenNonInteractive(t *testing.T) {
	resetOAuthState(t)
	setConsole(t, false)
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = makeCallbackWaiter(port, "", time.Hour)(context.Background())
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) || !strings.Contains(err.Error(), "interactive session") || time.Since(start) > 5*time.Second {
		t.Fatalf("error %v", err)
	}
	if !isReserved(port) {
		t.Fatal("waiter adopted the reservation before refusing")
	}
}

func TestOAuthRedirectHandlerRejectsAndDoesNotOpenBrowser(t *testing.T) {
	resetOAuthState(t)
	setConsole(t, false)
	fakeBrowser(t, func(string) bool { t.Error("must not open browser"); return false })
	err := makeRedirectHandler(49300, "", "")(context.Background(), "https://idp.example.com/authorize?x=1")
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) || !strings.Contains(err.Error(), "browser authorization") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(testStderr.String(), "https://idp.example.com/authorize") {
		t.Fatal("authorization URL printed")
	}
}

func TestOAuthGuardDoesNotFireOnInteractiveRedirect(t *testing.T) {
	resetOAuthState(t)
	setConsole(t, true)
	t.Setenv("SSH_CLIENT", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	fakeBrowser(t, func(string) bool { return false })
	if err := makeRedirectHandler(49302, "", "")(context.Background(), "https://idp.example.com/authorize?x=9"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testStderr.String(), "https://idp.example.com/authorize?x=9") {
		t.Fatalf("stderr %q", testStderr.String())
	}
}

func TestOAuthBuildClientMetadataTokenEndpointAuth(t *testing.T) {
	for _, c := range []struct {
		cfg  map[string]any
		want string
	}{{map[string]any{"cimd": false}, "none"}, {map[string]any{"client_secret": "shh"}, "client_secret_post"}} {
		resetOAuthState(t)
		if _, err := configureCallbackPort(context.Background(), c.cfg, nil); err != nil {
			t.Fatal(err)
		}
		md, err := buildClientMetadata(c.cfg)
		if err != nil || derefStr(md.TokenEndpointAuthMethod) != c.want || !strings.Contains(strings.Join(md.GrantTypes, " "), "authorization_code") ||
			!strings.Contains(strings.Join(md.GrantTypes, " "), "refresh_token") {
			t.Fatalf("%v gave %+v %v", c.cfg, md, err)
		}
	}
}

func TestOAuthResolveRedirectURI(t *testing.T) {
	proxy := "https://oauth.example.ts.net/callback"
	for _, c := range []struct {
		cfg  map[string]any
		want string
	}{{map[string]any{"redirect_uri": proxy}, proxy}, {map[string]any{}, "http://127.0.0.1:1234/callback"},
		{map[string]any{"redirect_host": "localhost"}, "http://localhost:1234/callback"}} {
		if got := resolveRedirectURI(c.cfg, 1234); got != c.want {
			t.Fatalf("%v gave %q", c.cfg, got)
		}
	}
}

func TestOAuthBuildPreservesServerURLPath(t *testing.T) {
	oauthHome(t)
	setConsole(t, true)
	p, err := BuildOAuthAuth(context.Background(), "notion", "https://mcp.notion.com/mcp", map[string]any{}, HTTPOptions{})
	if err != nil || p.serverURL != "https://mcp.notion.com/mcp" {
		t.Fatalf("provider %+v %v", p, err)
	}
}

// Feeds one stdin line to the paste reader
func pasteLine(t *testing.T, result *callbackResult, line string) {
	t.Helper()
	fakeStdin(t, func() (string, error) { return line, nil })
	pasteCallbackReader(result)
}

func TestOAuthPasteReaderParsesPastedCallback(t *testing.T) {
	result := newCallbackResult()
	pasteLine(t, result, "http://127.0.0.1:37949/callback?code=abc&state=xyz\n")
	if code, state, errText, _, _ := result.snapshot(); code != "abc" || state != "xyz" || errText != "" {
		t.Fatalf("code %q state %q error %q", code, state, errText)
	}
}

func TestOAuthPasteReaderSwallowsStdinErrors(t *testing.T) {
	result := newCallbackResult()
	fakeStdin(t, func() (string, error) { return "", errors.New("stdin closed") })
	pasteCallbackReader(result)
	if code, _, _, _, _ := result.snapshot(); code != "" {
		t.Fatalf("code %q", code)
	}
}

func TestOAuthPastePromptNotShownWhenInteractivitySuppressed(t *testing.T) {
	resetOAuthState(t)
	setConsole(t, true)
	for len(testStdinHit) > 0 {
		<-testStdinHit
	}
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	_, err = makeCallbackWaiter(port, "", 10*time.Millisecond)(SuppressInteractiveOAuth(context.Background()))
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) {
		t.Fatalf("error %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if len(testStdinHit) != 0 {
		t.Fatal("stdin read while interactivity was suppressed")
	}
}

func TestOAuthPasteSkipTokensSetSentinel(t *testing.T) {
	for _, token := range []string{"skip", "QUIT"} {
		result := newCallbackResult()
		pasteLine(t, result, token+"\n")
		if code, _, errText, _, _ := result.snapshot(); errText != oauthUserSkippedSentinel || code != "" {
			t.Fatalf("%s gave code %q error %q", token, code, errText)
		}
	}
}

func TestOAuthPasteSkipDoesNotOverwriteHTTPWinner(t *testing.T) {
	result := newCallbackResult()
	result.take("from_http", "x", "", "")
	pasteLine(t, result, "skip\n")
	if code, _, errText, _, _ := result.snapshot(); code != "from_http" || errText != "" {
		t.Fatalf("code %q error %q", code, errText)
	}
}

func TestOAuthWaitForCallbackSkipRaisesNonInteractiveError(t *testing.T) {
	resetOAuthState(t)
	fakeStdin(t, func() (string, error) { return "skip\n", nil })
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	_, err = makeCallbackWaiter(port, "", 10*time.Second)(ForceInteractiveOAuth(context.Background()))
	var ne *OAuthNonInteractiveError
	if !errors.As(err, &ne) || !strings.Contains(err.Error(), "user_skipped") {
		t.Fatalf("error %v", err)
	}
}

func TestOAuthPoisonBacksUpAndRemovesClientAndMeta(t *testing.T) {
	dir := oauthHome(t)
	tokens := writeTokenFile(t, dir, "srv.json", `{"access_token": "keep-me"}`)
	client := writeTokenFile(t, dir, "srv.client.json", `{"client_id": "dead"}`)
	meta := writeTokenFile(t, dir, "srv.meta.json", `{"token_endpoint": "https://idp/token"}`)
	if !NewTokenStorage(context.Background(), "srv", "").PoisonClientRegistration() {
		t.Fatal("poison reported nothing removed")
	}
	bak, _ := os.ReadFile(client + ".bak")
	kept, _ := os.ReadFile(tokens)
	if exists(client) || exists(meta) || string(bak) != `{"client_id": "dead"}` || string(kept) != `{"access_token": "keep-me"}` {
		t.Fatalf("backup %q tokens %q", bak, kept)
	}
}

func TestOAuthCancelledWaiterReleasesPinnedPortForRetry(t *testing.T) {
	resetOAuthState(t)
	blockingStdin(t)
	probe, err := netListen4(0)
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	ctx, cancel := context.WithCancel(ForceInteractiveOAuth(context.Background()))
	ch := runWaiter(ctx, makeCallbackWaiter(port, "", 30*time.Second))
	time.Sleep(300 * time.Millisecond)
	cancel()
	if o := awaitWaiter(t, ch, 10*time.Second); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("error %v", o.err)
	}
	again, err := netListen4(port)
	if err != nil {
		t.Fatalf("retry could not bind %d: %v", port, err)
	}
	again.Close()
}

func TestOAuthFigmaProviderDefaultsSetAllowlistedClientName(t *testing.T) {
	cfg := ApplyOAuthProviderDefaults(map[string]any{}, "figma", "https://mcp.figma.com/mcp")
	if cfg["client_name"] != figmaDCRClientName || cfg["scope"] != figmaDefaultScope {
		t.Fatalf("cfg %v", cfg)
	}
}

func TestOAuthHumanizeNonRegistration403Passthrough(t *testing.T) {
	if msg, ok := HumanizeOAuthRegistrationError("linear", "HTTP 403: insufficient_scope", "https://mcp.linear.app/mcp"); ok {
		t.Fatalf("humanized %q", msg)
	}
}

func TestOAuthHumanize404RegistrationErrorGuidance(t *testing.T) {
	msg, ok := HumanizeOAuthRegistrationError("gmail", "Registration failed: 404 <html>Error 404 (Not Found)!!1</html>", "https://gmailmcp.googleapis.com/mcp/v1")
	if !ok || !strings.Contains(msg, "does not support automatic client registration") || !strings.Contains(msg, "client_id") ||
		!strings.Contains(msg, "icebath mcp login gmail") {
		t.Fatalf("message %q %v", msg, ok)
	}
}

func TestOAuthHumanize404NonRegistrationPassthrough(t *testing.T) {
	if msg, ok := HumanizeOAuthRegistrationError("srv", "HTTP 404: no such tool", "https://example.com/mcp"); ok {
		t.Fatalf("humanized %q", msg)
	}
}

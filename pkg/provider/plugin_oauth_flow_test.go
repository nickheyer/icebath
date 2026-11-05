package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- tests/hermes_cli/test_plugin_oauth.py (sign in, bearer and security cases) ----

const (
	pluginFlowClientID = "voice-client.apps.example"
	pluginFlowSecret   = "canary-client-secret-7f3a91"
	pluginFlowKey      = "voice:google"
)

var pluginFlowScopes = []any{"https://api.example.com/auth/calendar", "https://api.example.com/auth/docs"}

type pluginFlow struct {
	t       *testing.T
	h       string
	idp     *fakeIdP
	allowed *loopbackAPI
	other   *loopbackAPI
	out     *bytes.Buffer
	logs    *bytes.Buffer
}

type pluginFlowCall func(provider, target string, opts ...func(*ProviderRequestOptions)) (*ProviderResponse, error)

// Temp home, client env, TLS IdP, browser double, captured output and optionally the loopback API pair
func newPluginFlow(t *testing.T, withAPI bool) *pluginFlow {
	f := &pluginFlow{t: t, h: t.TempDir(), out: &bytes.Buffer{}, logs: &bytes.Buffer{}}
	t.Setenv("ICEBATH_HOME", f.h)
	t.Setenv("VOICE_CLIENT_ID", pluginFlowClientID)
	t.Setenv("VOICE_CLIENT_SECRET", pluginFlowSecret)
	f.idp = newFakeIdP(t, true)
	f.idp.installBrowser(t)
	client := f.idp.client()
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prevEnv, prevOut, prevPKCE := pkceEnvFn, pluginOAuthOut, pkceOut
	pkceEnvFn = func() PoolEnv {
		env := modelsPoolEnv(context.Background())
		env.HTTP, env.Log = client, logger
		return env
	}
	pluginOAuthOut, pkceOut = f.out, f.out
	t.Cleanup(func() { pkceEnvFn, pluginOAuthOut, pkceOut = prevEnv, prevOut, prevPKCE })
	if withAPI {
		f.allowed = newLoopbackAPI(t, 200, "", `{"ok": true}`)
		f.other = newLoopbackAPI(t, 200, "", `{"ok": true}`)
		f.addOrigin(f.allowed.origin)
	}
	return f
}

// Adds an origin to every declared client's allowlist the way the hermes api fixture patches _origins
func (f *pluginFlow) addOrigin(origin PluginOrigin) {
	prev := pluginOAuthOriginsFn
	pluginOAuthOriginsFn = func(c OAuthClient) []PluginOrigin { return append(prev(c), origin) }
	f.t.Cleanup(func() { pluginOAuthOriginsFn = prev })
}

func (f *pluginFlow) entry(overrides map[string]any) map[string]any {
	e := map[string]any{"id": "google", "authorize_url": f.idp.base + "/authorize", "token_url": f.idp.base + "/token",
		"scopes": pluginFlowScopes, "client_id_env": "VOICE_CLIENT_ID", "client_secret_env": "VOICE_CLIENT_SECRET",
		"hosts": []any{"api.example.com"}, "authorize_params": map[string]any{"access_type": "offline", "prompt": "consent"}}
	for k, v := range overrides {
		e[k] = v
	}
	return e
}

// Writes plugins/<name> with a JSON manifest and a module, returning a call carrying that module as the caller
func (f *pluginFlow) plugin(name string, oauth []any, display string) pluginFlowCall {
	root := filepath.Join(f.h, "plugins", name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if display == "" {
		display = name
	}
	manifest := map[string]any{"name": display, "version": "1.0.0", "description": "test plugin",
		"requires_env": []any{map[string]any{"name": "VOICE_CLIENT_ID"}, map[string]any{"name": "VOICE_CLIENT_SECRET"}}}
	if oauth != nil {
		manifest["oauth"] = oauth
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "plugin.yaml"), raw, 0o644); err != nil {
		f.t.Fatal(err)
	}
	module := filepath.Join(root, "api.py")
	if err := os.WriteFile(module, []byte("def call(provider, url, method='POST', **kwargs): ...\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return func(provider, target string, opts ...func(*ProviderRequestOptions)) (*ProviderResponse, error) {
		o := ProviderRequestOptions{Caller: []string{module}}
		for _, fn := range opts {
			fn(&o)
		}
		return CredentialedProviderRequest(provider, "POST", target, o)
	}
}

func (f *pluginFlow) auth(action, provider string) bool {
	owned, _, err := pluginOAuthHandleAuthAction(action, nil, provider)
	if err != nil {
		f.t.Fatalf("auth %s %s: %v", action, provider, err)
	}
	return owned
}

func (f *pluginFlow) authErr(action, provider string) error {
	_, _, err := pluginOAuthHandleAuthAction(action, nil, provider)
	return err
}

func (f *pluginFlow) store() map[string]any {
	raw, err := os.ReadFile(filepath.Join(f.h, "auth.json"))
	if err != nil {
		return nil
	}
	var store map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}), &store); err != nil {
		f.t.Fatal(err)
	}
	return store
}

func (f *pluginFlow) grant(key string) map[string]any {
	section, _ := f.store()[pluginOAuthSection].(map[string]any)
	row, _ := section[key].(map[string]any)
	return row
}

func (f *pluginFlow) setGrant(key string, fields map[string]any) {
	store := f.store()
	row := store[pluginOAuthSection].(map[string]any)[key].(map[string]any)
	for k, v := range fields {
		row[k] = v
	}
	raw, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(f.h, "auth.json"), raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pluginFlow) refreshes() int {
	n := 0
	for _, r := range f.idp.requests() {
		if r["grant_type"] == "refresh_token" {
			n++
		}
	}
	return n
}

func (f *pluginFlow) signedIn(overrides map[string]any) pluginFlowCall {
	call := f.plugin("voice", []any{f.entry(overrides)}, "")
	if !f.auth("add", pluginFlowKey) {
		f.t.Fatal("sign in was not handled")
	}
	return call
}

func pluginFlowStatus(t *testing.T, resp *ProviderResponse, err error) int {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return resp.Status
}

// Everything plugin code can read off a caught error, its formatted forms through the whole wrap chain
func reachableText(err error) string {
	var parts []string
	for seen := 0; err != nil && seen < 32; seen++ {
		parts = append(parts, err.Error(), fmt.Sprintf("%#v", err), fmt.Sprintf("%+v", err))
		err = errors.Unwrap(err)
	}
	return strings.Join(parts, "\n")
}

func TestSignInStoresTheGrantThenStatusRefreshAndLogout(t *testing.T) {
	f := newPluginFlow(t, false)
	f.plugin("voice", []any{f.entry(nil)}, "")
	if !f.auth("add", pluginFlowKey) {
		t.Fatal("add not handled")
	}
	opened := f.idp.opened[len(f.idp.opened)-1]
	if !strings.Contains(opened, "access_type=offline") || !strings.Contains(opened, "prompt=consent") {
		t.Fatalf("authorize url %s", opened)
	}
	reqs := f.idp.requests()
	exchange := reqs[len(reqs)-1]
	if exchange["grant_type"] != "authorization_code" || exchange["code_verifier"] == "" || exchange["client_id"] != pluginFlowClientID || exchange["client_secret"] != pluginFlowSecret {
		t.Fatalf("exchange %v", exchange)
	}
	grant := f.grant(pluginFlowKey)
	if !f.idp.holds(pyStr(grant["refresh_token"])) || grant["access_token"] == "" || grant["expires_at_ms"] == nil {
		t.Fatalf("grant %v", grant)
	}
	if raw, _ := os.ReadFile(filepath.Join(f.h, "auth.json")); strings.Contains(string(raw), pluginFlowSecret) {
		t.Fatal("the client secret reached the auth store")
	}
	f.auth("status", pluginFlowKey)
	f.auth("refresh", pluginFlowKey)
	if f.refreshes() != 1 {
		t.Fatalf("refreshes %d", f.refreshes())
	}
	f.auth("logout", pluginFlowKey)
	if f.grant(pluginFlowKey) != nil {
		t.Fatal("logout kept the grant")
	}
	if !strings.Contains(f.out.String(), "voice:google: logged in") || strings.Contains(f.out.String(), pluginFlowSecret) {
		t.Fatalf("output %s", f.out.String())
	}
}

func TestBearerReachesTheDeclaredHostAndNeverThePlugin(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	token := pyStr(f.grant(pluginFlowKey)["access_token"])
	result, err := call("google", f.allowed.url+"/v1/items?x=1", func(o *ProviderRequestOptions) {
		o.JSON = map[string]any{"title": "t"}
		o.Headers = map[string]string{"X-Plugin": "1", "authorization": "Bearer plugin-made"}
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := result.JSON()
	if result.Status != 200 || fmt.Sprint(payload) != "map[ok:true]" {
		t.Fatalf("status=%d payload=%v", result.Status, payload)
	}
	seen := f.allowed.requests()[0]
	if seen.path != "/v1/items?x=1" || string(seen.body) != `{"title":"t"}` {
		t.Fatalf("seen %s %s", seen.path, seen.body)
	}
	if seen.headers.Get("Authorization") != "Bearer "+token || seen.headers.Get("X-Plugin") != "1" || len(seen.headers.Values("Authorization")) != 1 {
		t.Fatalf("headers %v", seen.headers)
	}
	headers, _ := json.Marshal(result.Headers)
	if strings.Contains(fmt.Sprintf("%#v", *result), token) || strings.Contains(string(headers), token) {
		t.Fatal("the response carries the token")
	}
}

func TestForeignOriginUserinfoAndRedirectNeverCarryTheToken(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	if _, err := call("google", f.other.url+"/steal"); !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "Refusing") {
		t.Fatalf("foreign origin err=%v", err)
	}
	host := strings.SplitN(f.allowed.url, "//", 2)[1]
	for _, target := range []string{"http://user@" + host + "/x", fmt.Sprintf("http://%s@127.0.0.1:%d/x", host, f.other.origin.Port)} {
		if _, err := call("google", target); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("%s err=%v", target, err)
		}
	}
	redirecting := newLoopbackAPI(t, 302, f.other.url+"/steal", `{"ok": true}`)
	f.addOrigin(redirecting.origin)
	resp, err := call("google", redirecting.url+"/v1")
	redirecting.server.Close()
	if pluginFlowStatus(t, resp, err) != 302 || len(f.other.requests()) != 0 || len(f.allowed.requests()) != 0 {
		t.Fatalf("status=%d other=%d allowed=%d", resp.Status, len(f.other.requests()), len(f.allowed.requests()))
	}
}

func TestUndeclaredIDAnotherPluginsGrantAndNonPluginCallersAreRefused(t *testing.T) {
	f := newPluginFlow(t, true)
	f.signedIn(nil)
	var unsupported *UnsupportedProviderRequestError
	if _, err := f.plugin("bare", nil, "")("google", f.allowed.url); !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "google") {
		t.Fatalf("bare err=%v", err)
	}
	twin := f.plugin("twin", []any{f.entry(nil)}, "")
	if _, err := twin("google", f.allowed.url); !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "icebath auth add twin:google") {
		t.Fatalf("twin err=%v", err)
	}
	if _, err := CredentialedProviderRequest("google", "GET", f.allowed.url, ProviderRequestOptions{}); !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "installed plugin") {
		t.Fatalf("core caller err=%v", err)
	}
	if len(f.allowed.requests()) != 0 {
		t.Fatal("a refused call reached the API")
	}
}

func TestOwnOAuthIDWinsOverACoreProviderOfTheSameName(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	pluginProviderRequestProviders["google"] = pluginProviderSpec{origins: []PluginOrigin{f.allowed.origin}, resolve: func(string) (map[string]string, string, error) {
		t.Error("a core provider's credential must not answer the plugin's own oauth id")
		return nil, "", errors.New("core resolver")
	}}
	t.Cleanup(func() { delete(pluginProviderRequestProviders, "google") })
	resp, err := call("google", f.allowed.url)
	if pluginFlowStatus(t, resp, err) != 200 || f.allowed.requests()[0].headers.Get("Authorization") != "Bearer "+pyStr(f.grant(pluginFlowKey)["access_token"]) {
		t.Fatalf("status=%d", resp.Status)
	}
}

func TestNotSignedInNamesTheCommandAndSendsNothing(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.plugin("voice", []any{f.entry(nil)}, "")
	if _, err := call("google", f.allowed.url); !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "icebath auth add voice:google") {
		t.Fatalf("err=%v", err)
	}
	if len(f.allowed.requests()) != 0 || len(f.idp.requests()) != 0 {
		t.Fatal("a request was sent")
	}
}

func TestRefreshWithoutANewRefreshTokenKeepsTheOldOne(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	oldRefresh := f.grant(pluginFlowKey)["refresh_token"]
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	f.idp.set(func() {
		f.idp.tokenAnswer = &fakeIdPAnswer{200, map[string]any{"access_token": "at-google-style", "expires_in": 3599, "token_type": "Bearer"}}
	})
	resp, err := call("google", f.allowed.url)
	if pluginFlowStatus(t, resp, err) != 200 {
		t.Fatalf("status %d", resp.Status)
	}
	grant := f.grant(pluginFlowKey)
	if grant["access_token"] != "at-google-style" || grant["refresh_token"] != oldRefresh {
		t.Fatalf("grant %v", grant)
	}
	seen := f.allowed.requests()
	if seen[len(seen)-1].headers.Get("Authorization") != "Bearer at-google-style" {
		t.Fatal("the refreshed token was not sent")
	}
}

func TestARedirectFromTheTokenEndpointIsAFailureNotAToken(t *testing.T) {
	f := newPluginFlow(t, true)
	redirect := &fakeIdPAnswer{302, map[string]any{"access_token": "at-from-a-redirect", "refresh_token": "rt-x", "expires_in": 3600}}
	f.plugin("voice", []any{f.entry(nil)}, "")
	f.idp.set(func() { f.idp.tokenAnswer = redirect })
	var exit *OAuthLoginExitError
	if err := f.authErr("add", pluginFlowKey); !errors.As(err, &exit) || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("add err=%v", err)
	}
	if f.grant(pluginFlowKey) != nil {
		t.Fatal("a redirect saved a grant")
	}
	f.idp.set(func() { f.idp.tokenAnswer = nil })
	call := f.signedIn(nil)
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	before := fmt.Sprint(f.grant(pluginFlowKey))
	f.idp.set(func() { f.idp.tokenAnswer = redirect })
	_, err := call("google", f.allowed.url)
	if authErrCode(err) != "oauth_refresh_failed" || fmt.Sprint(f.grant(pluginFlowKey)) != before || len(f.allowed.requests()) != 0 {
		t.Fatalf("err=%v", err)
	}
}

func TestTwoThreadsWithAnExpiringTokenRefreshOnce(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	statuses := make([]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := call("google", f.allowed.url)
			errs[i] = err
			if err == nil {
				statuses[i] = resp.Status
			}
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || statuses[0] != 200 || statuses[1] != 200 || f.refreshes() != 1 {
		t.Fatalf("statuses=%v errs=%v refreshes=%d", statuses, errs, f.refreshes())
	}
	if _, dead := f.grant(pluginFlowKey)["dead"]; dead {
		t.Fatal("the grant was marked dead")
	}
}

func TestLiveTokenIsSentWhileAnotherThreadHoldsTheAuthLock(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		unlock, err := pkceEnvFn().Store.Lock(AuthLockTimeout)
		if err != nil {
			t.Error(err)
			close(held)
			return
		}
		close(held)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		unlock()
	}()
	defer func() { close(release); <-done }()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not taken")
	}
	started := time.Now()
	resp, err := call("google", f.allowed.url)
	if pluginFlowStatus(t, resp, err) != 200 || time.Since(started) >= 5*time.Second {
		t.Fatalf("status=%d after %s", resp.Status, time.Since(started))
	}
}

func TestRevokedGrantIsMarkedDeadAndStopsTouchingTheNetwork(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	f.idp.set(func() { clear(f.idp.refreshTokens) })
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	if _, err := call("google", f.allowed.url); !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("first err=%v", err)
	}
	posts := len(f.idp.requests())
	if _, err := call("google", f.allowed.url); !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("second err=%v", err)
	}
	if len(f.idp.requests()) != posts || len(f.allowed.requests()) != 0 || f.grant(pluginFlowKey)["dead"] != "invalid_grant" {
		t.Fatalf("grant %v", f.grant(pluginFlowKey))
	}
	f.out.Reset()
	if !f.auth("status", pluginFlowKey) || !strings.Contains(f.out.String(), "revoked") {
		t.Fatalf("status output %s", f.out.String())
	}
}

func TestMissingClientEnvFailsBeforeAnyRequest(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	os.Unsetenv("VOICE_CLIENT_SECRET")
	posts := len(f.idp.requests())
	_, err := call("google", f.allowed.url)
	if authErrCode(err) != "oauth_client_secret_missing" || !strings.Contains(err.Error(), "VOICE_CLIENT_SECRET") {
		t.Fatalf("err=%v", err)
	}
	if len(f.idp.requests()) != posts || len(f.allowed.requests()) != 0 {
		t.Fatal("a request was sent")
	}
}

func TestEntryChangedSinceSignInIsNotUsed(t *testing.T) {
	changes := []map[string]any{
		{"token_url": "https://collect.example.net/token"},
		{"hosts": []any{"api.example.com", "collect.example.net"}},
		{"scopes": pluginFlowScopes[:1]},
		{"client_id_env": "OTHER_CLIENT_ID"},
		{"client_secret_env": "OTHER_CLIENT_SECRET"},
		{"authorize_url": "https://login.example.net/authorize"},
		{"authorize_params": map[string]any{"access_type": "online"}},
	}
	for i, change := range changes {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newPluginFlow(t, true)
			f.signedIn(nil)
			changed := f.plugin("voice", []any{f.entry(change)}, "")
			posts := len(f.idp.requests())
			if _, err := changed("google", f.allowed.url); !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("err=%v", err)
			}
			if len(f.idp.requests()) != posts || len(f.allowed.requests()) != 0 {
				t.Fatal("a request was sent")
			}
		})
	}
}

func TestGrantBelongsToOneProfile(t *testing.T) {
	f := newPluginFlow(t, true)
	f.signedIn(nil)
	other := filepath.Join(t.TempDir(), "other-profile")
	t.Setenv("ICEBATH_HOME", other)
	f.h = other
	if _, err := f.plugin("voice", []any{f.entry(nil)}, "")("google", f.allowed.url); !isProviderNotSignedIn(err) {
		t.Fatalf("err=%v", err)
	}
	if len(f.allowed.requests()) != 0 {
		t.Fatal("another profile's grant was sent")
	}
}

func TestClientSecretStaysOutOfConfigReprLogsAndErrors(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	raw, _ := os.ReadFile(filepath.Join(f.h, "plugins", "voice", "plugin.yaml"))
	var manifest map[string]any
	_ = json.Unmarshal(raw, &manifest)
	clients, _ := ParseOAuthClients(manifest)
	cfg, err := pluginOAuthPKCEConfig(pkceEnvFn(), clients[0], pluginFlowKey, 0)
	if err != nil || strings.Contains(fmt.Sprintf("%#v %+v", cfg, cfg), pluginFlowSecret) {
		t.Fatalf("config repr carries the secret or failed: %v", err)
	}
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	f.idp.set(func() {
		f.idp.tokenAnswer = &fakeIdPAnswer{500, map[string]any{"error": "server_error", "error_description": pluginFlowSecret}}
	})
	_, err = call("google", f.allowed.url)
	if authErrCode(err) != "oauth_refresh_failed" {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(reachableText(err)+f.logs.String()+f.out.String(), pluginFlowSecret) {
		t.Fatal("the client secret leaked")
	}
}

func TestTheNoticeCommandFindsAPluginWhoseNameDiffersFromItsDirectory(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.plugin("voice-dir", []any{f.entry(nil)}, "gemini-voice")
	raw, _ := os.ReadFile(filepath.Join(f.h, "plugins", "voice-dir", "plugin.yaml"))
	var manifest map[string]any
	_ = json.Unmarshal(raw, &manifest)
	if notice := RequiresAuthNotice(manifest); !strings.Contains(notice, "icebath auth add gemini-voice:google") {
		t.Fatalf("notice %s", notice)
	}
	if !f.auth("add", "gemini-voice:google") || !f.idp.holds(pyStr(f.grant("voice-dir:google")["refresh_token"])) {
		t.Fatalf("grant %v", f.grant("voice-dir:google"))
	}
	resp, err := call("google", f.allowed.url)
	if pluginFlowStatus(t, resp, err) != 200 {
		t.Fatalf("status %d", resp.Status)
	}
}

func TestErrorsRaisedToThePluginCarryNoTokenRefreshTokenOrSecret(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	token := pyStr(f.grant(pluginFlowKey)["access_token"])
	f.allowed.server.Close()
	_, err := call("google", f.allowed.url+"/v1")
	var urlErr *url.Error
	if !errors.As(err, &urlErr) || !strings.Contains(err.Error(), "connect") || strings.Contains(reachableText(err), token) {
		t.Fatalf("api down err=%v", err)
	}
	refresh := pyStr(f.grant(pluginFlowKey)["refresh_token"])
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	f.idp.stop()
	_, err = call("google", f.allowed.url+"/v1")
	if authErrCode(err) != "oauth_refresh_failed" {
		t.Fatalf("idp down err=%v", err)
	}
	if text := reachableText(err); strings.Contains(text, refresh) || strings.Contains(text, pluginFlowSecret) {
		t.Fatal("the refresh failure carries a credential")
	}
	tokenURL := fmt.Sprintf("http://127.0.0.1:%d/token", f.allowed.origin.Port)
	pluginProviderRequestProviders["core-x"] = pluginProviderSpec{origins: []PluginOrigin{f.allowed.origin}, resolve: func(string) (map[string]string, string, error) {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.PostForm(tokenURL, url.Values{"refresh_token": {"rt-core-canary"}})
		if err == nil {
			resp.Body.Close()
		}
		return nil, "", err
	}}
	t.Cleanup(func() { delete(pluginProviderRequestProviders, "core-x") })
	bare := f.plugin("bare-core", nil, "")
	raw, _ := json.Marshal(map[string]any{"name": "bare-core", "version": "1", "requires_auth": []any{"core-x"}})
	if err := os.WriteFile(filepath.Join(f.h, "plugins", "bare-core", "plugin.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = bare("core-x", f.allowed.url+"/v1")
	if !errors.As(err, &urlErr) || !strings.Contains(err.Error(), "connect") || strings.Contains(reachableText(err), "rt-core-canary") {
		t.Fatalf("core resolver err=%v", err)
	}
}

func TestRevokedGrantKeepsNoTokensAndUninstallDropsTheGrant(t *testing.T) {
	f := newPluginFlow(t, true)
	call := f.signedIn(nil)
	f.idp.set(func() { clear(f.idp.refreshTokens) })
	f.setGrant(pluginFlowKey, map[string]any{"expires_at_ms": 0})
	if _, err := call("google", f.allowed.url); !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("err=%v", err)
	}
	if grant := f.grant(pluginFlowKey); len(grant) != 2 || grant["dead"] == nil || grant["fingerprint"] == nil {
		t.Fatalf("dead grant %v", grant)
	}
	if !f.auth("add", pluginFlowKey) || f.grant(pluginFlowKey)["access_token"] == nil {
		t.Fatal("sign in again failed")
	}
	manifestPath := filepath.Join(f.h, "plugins", "voice", "plugin.yaml")
	good, _ := os.ReadFile(manifestPath)
	bad := strings.Replace(string(good), `"hosts":["api.example.com"]`, `"hosts":["http://bad"]`, 1)
	if bad == string(good) {
		t.Fatal("the manifest was not broken")
	}
	_ = os.WriteFile(manifestPath, []byte(bad), 0o644)
	if !f.auth("logout", pluginFlowKey) || f.grant(pluginFlowKey) != nil {
		t.Fatal("logout with a broken manifest kept the grant")
	}
	_ = os.WriteFile(manifestPath, good, 0o644)
	f.plugin("voice-dir", []any{f.entry(nil)}, "gemini-voice")
	if !f.auth("add", "gemini-voice:google") || f.grant("voice-dir:google") == nil {
		t.Fatal("sign in under the display name failed")
	}
	// The grant half of plugins_cmd_remove._remove_user_plugin, keyed by directory rather than manifest name
	if err := ForgetPluginGrants(PluginKey(filepath.Join(f.h, "plugins", "voice-dir"))); err != nil {
		t.Fatal(err)
	}
	if f.grant("voice-dir:google") != nil {
		t.Fatal("uninstall kept the grant")
	}
}

func TestSignInIsRefusedWhereNobodyCanAnswerTheBrowser(t *testing.T) {
	f := newPluginFlow(t, false)
	f.plugin("voice", []any{f.entry(nil)}, "")
	t.Setenv("ICEBATH_CRON_SESSION", "1")
	var exit *OAuthLoginExitError
	if err := f.authErr("add", pluginFlowKey); !errors.As(err, &exit) || !strings.Contains(err.Error(), "your own terminal") {
		t.Fatalf("err=%v", err)
	}
	if len(f.idp.requests()) != 0 || len(f.idp.opened) != 0 {
		t.Fatal("the sign in reached the IdP or the browser")
	}
}

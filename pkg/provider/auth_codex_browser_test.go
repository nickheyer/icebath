package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Authorize answers 302 with a code, token verifies PKCE and returns tokens, both recorded
type fakeOpenAI struct {
	mu        sync.Mutex
	authorize url.Values
	token     url.Values
	server    *httptest.Server
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			q := r.URL.Query()
			f.mu.Lock()
			f.authorize = q
			f.mu.Unlock()
			redirect := q.Get("redirect_uri") + "?" + url.Values{"code": {"AC-1"}, "state": {q.Get("state")}}.Encode()
			http.Redirect(w, r, redirect, http.StatusFound)
		case http.MethodPost:
			_ = r.ParseForm()
			f.mu.Lock()
			f.token = r.PostForm
			challenge := f.authorize.Get("code_challenge")
			f.mu.Unlock()
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			derived := base64.RawURLEncoding.EncodeToString(sum[:])
			ok := r.PostForm.Get("grant_type") == "authorization_code" && r.PostForm.Get("code") == "AC-1" && derived == challenge
			w.Header().Set("Content-Type", "application/json")
			if !ok {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
				return
			}
			raw, _ := json.Marshal(map[string]any{"access_token": jwtWithClaims(map[string]any{"email": "pkce@example.com"}), "refresh_token": "rt-pkce"})
			_, _ = w.Write(raw)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func pointCodexBrowserAt(t *testing.T, f *fakeOpenAI) {
	t.Helper()
	prevAuth, prevToken, prevPort, prevDevice := codexOAuthAuthorizeURL, codexOAuthTokenURL, codexBrowserCallbackPort, codexDeviceCodeLoginFn
	codexOAuthAuthorizeURL = f.server.URL + "/oauth/authorize"
	codexOAuthTokenURL = f.server.URL + "/oauth/token"
	codexBrowserCallbackPort = 0
	t.Cleanup(func() {
		codexOAuthAuthorizeURL, codexOAuthTokenURL, codexBrowserCallbackPort, codexDeviceCodeLoginFn = prevAuth, prevToken, prevPort, prevDevice
	})
}

func TestBrowserFlagRunsLoopbackPKCEAndStoresLoopbackSource(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	fake := newFakeOpenAI(t)
	pointCodexBrowserAt(t, fake)
	scriptBrowserEnv(t, map[string]string{"BROWSER": "firefox", "DISPLAY": ":0"})
	codexDeviceCodeLoginFn = func(PoolEnv, io.Writer) (map[string]any, error) {
		t.Fatal("--browser must not run the device-code flow")
		return nil, nil
	}
	prevOpen := openBrowserFn
	openBrowserFn = func(target string) error {
		go func() {
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(target)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { openBrowserFn = prevOpen })
	var out bytes.Buffer
	creds, err := codexOAuthLogin(env, &out, CodexLoginArgs{Browser: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tokens := creds["tokens"].(map[string]any)
	if creds["source"] != "loopback_pkce" || tokens["refresh_token"] != "rt-pkce" || tokens["access_token"] != jwtWithClaims(map[string]any{"email": "pkce@example.com"}) {
		t.Fatalf("creds %v", creds)
	}
	fake.mu.Lock()
	authorize, token := fake.authorize, fake.token
	fake.mu.Unlock()
	if authorize.Get("code_challenge_method") != "S256" || authorize.Get("response_type") != "code" {
		t.Fatalf("authorize %v", authorize)
	}
	redirect := authorize.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://localhost:") || !strings.HasSuffix(redirect, "/auth/callback") {
		t.Fatalf("redirect %q", redirect)
	}
	if token.Get("redirect_uri") != redirect || token.Get("client_id") != authorize.Get("client_id") {
		t.Fatalf("token form %v", token)
	}
	if !strings.Contains(out.String(), "Browser opened for OpenAI authorization.") {
		t.Fatalf("output %q", out.String())
	}
}

func TestDefaultIsDeviceCodeAndBusyCallbackPortFallsBack(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	fake := newFakeOpenAI(t)
	pointCodexBrowserAt(t, fake)
	deviceLogins := 0
	codexDeviceCodeLoginFn = func(PoolEnv, io.Writer) (map[string]any, error) {
		deviceLogins++
		return map[string]any{"tokens": map[string]any{"access_token": jwtWithClaims(map[string]any{"email": "device@example.com"}), "refresh_token": "rt-dev"},
			"base_url": DefaultCodexBaseURL, "last_refresh": "2026-01-01T00:00:00Z", "source": "device-code"}, nil
	}
	var out bytes.Buffer
	creds, err := codexOAuthLogin(env, &out, CodexLoginArgs{})
	if err != nil || creds["source"] != "device-code" || deviceLogins != 1 {
		t.Fatalf("default flow creds %v err %v logins %d", creds, err, deviceLogins)
	}
	if strings.Contains(out.String(), "browser authorization") {
		t.Fatal("the loopback listener was started for the default flow")
	}
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()
	codexBrowserCallbackPort = occupant.Addr().(*net.TCPAddr).Port
	out.Reset()
	creds, err = codexOAuthLogin(env, &out, CodexLoginArgs{Browser: true})
	if err != nil || creds["source"] != "device-code" || deviceLogins != 2 {
		t.Fatalf("busy port creds %v err %v logins %d", creds, err, deviceLogins)
	}
	if !strings.Contains(out.String(), "Port 1455 is already in use") {
		t.Fatalf("busy notice missing: %q", out.String())
	}
	writeConfigYAML(t, h, "auth:\n  codex_login_flow: carrier-pigeon\n")
	env = testEnv(t, h)
	out.Reset()
	if flow := codexLoginFlow(env, &out, CodexLoginArgs{}); flow != "device_code" || !strings.Contains(out.String(), "Ignoring unknown auth.codex_login_flow 'carrier-pigeon'") {
		t.Fatalf("unknown flow %q output %q", flow, out.String())
	}
	writeConfigYAML(t, h, "auth:\n  codex_login_flow: browser\n")
	env = testEnv(t, h)
	if flow := codexLoginFlow(env, &out, CodexLoginArgs{}); flow != "browser" {
		t.Fatalf("configured flow %q", flow)
	}
}

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pins the browser heuristics to a scripted environment and PATH for one test
func scriptBrowserEnv(t *testing.T, env map[string]string, onPath ...string) {
	t.Helper()
	prevGetenv, prevLook := oauthGetenv, oauthLookPath
	oauthGetenv = func(key string) string { return env[key] }
	oauthLookPath = func(name string) (string, error) {
		for _, p := range onPath {
			if p == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { oauthGetenv, oauthLookPath = prevGetenv, prevLook })
}

func TestHeadlessLinuxNoDisplayRefuses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the display server requirement is the Linux arm")
	}
	scriptBrowserEnv(t, map[string]string{"TERM": "xterm"}, "google-chrome", "w3m")
	if canOpenGraphicalBrowser() {
		t.Fatal("headless Linux must not auto open a browser")
	}
}

func TestBrowserEnvPointingAtConsoleBrowserRefuses(t *testing.T) {
	scriptBrowserEnv(t, map[string]string{"DISPLAY": ":0", "BROWSER": "/usr/bin/w3m"}, "xdg-open")
	if canOpenGraphicalBrowser() {
		t.Fatal("$BROWSER=w3m must refuse even with a display server")
	}
}

func TestGraphicalBrowserResolution(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PATH based resolution is the Linux arm")
	}
	scriptBrowserEnv(t, map[string]string{"DISPLAY": ":0"}, "xdg-open")
	if !canOpenGraphicalBrowser() {
		t.Fatal("xdg-open under a display must open")
	}
	scriptBrowserEnv(t, map[string]string{"DISPLAY": ":0", "TERM": "xterm"}, "lynx")
	if canOpenGraphicalBrowser() {
		t.Fatal("only a console browser on PATH must refuse")
	}
	scriptBrowserEnv(t, map[string]string{"BROWSER": "firefox"})
	if !canOpenGraphicalBrowser() {
		t.Fatal("a graphical $BROWSER without a display is trusted")
	}
	scriptBrowserEnv(t, map[string]string{"DISPLAY": ":0"})
	if canOpenGraphicalBrowser() {
		t.Fatal("no browser at all must refuse")
	}
}

func TestIsRemoteSession(t *testing.T) {
	scriptBrowserEnv(t, map[string]string{})
	if isRemoteSession() {
		t.Fatal("a plain shell is not remote")
	}
	scriptBrowserEnv(t, map[string]string{"SSH_TTY": "/dev/pts/1"})
	if !isRemoteSession() {
		t.Fatal("SSH_TTY marks a remote session")
	}
	scriptBrowserEnv(t, map[string]string{"CODESPACES": "true"})
	if !isRemoteSession() {
		t.Fatal("a browser only remote IDE marks a remote session")
	}
}

func TestSanitizeNoProxyEntries(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1,localhost,::1,[::1]": "127.0.0.1,localhost,::1",
		"localhost,10.0.0.0/8":          "localhost,10.0.0.0/8",
		"[2001:db8::1]:8080":            "2001:db8::1",
		"::1/128,example.com":           "::1,example.com",
		"*,[::1]":                       "*,[::1]",
		"[junk]":                        "[junk]",
		"":                              "",
	}
	for in, want := range cases {
		if got := sanitizeNoProxyEntries(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
	if got := normalizeProxyURL("socks://127.0.0.1:7890"); got != "socks5://127.0.0.1:7890" {
		t.Fatalf("socks normalization %q", got)
	}
}

func TestCodexHTTPClientConstructsUnderClashIPv6NoProxy(t *testing.T) {
	for _, key := range []string{"NO_PROXY", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1,[::1]")
	t.Setenv("no_proxy", "127.0.0.1,localhost,::1,[::1]")
	client := codexHTTPClient(5 * time.Second)
	if client == nil || client.Transport == nil {
		t.Fatal("client not constructed")
	}
	if got := os.Getenv("NO_PROXY"); got != "127.0.0.1,localhost,::1" {
		t.Fatalf("NO_PROXY not sanitized: %q", got)
	}
}

func TestPKCEHelpers(t *testing.T) {
	verifier := pkceCodeVerifier(64)
	if len(verifier) == 0 || len(verifier) > 128 || strings.ContainsAny(verifier, "+/=") {
		t.Fatalf("verifier %q", verifier)
	}
	sum := sha256.Sum256([]byte(verifier))
	if pkceCodeChallenge(verifier) != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("challenge is not the url safe sha256 of the verifier")
	}
}

func TestLoopbackCallbackHandlerAndServer(t *testing.T) {
	handler, result := newLoopbackCallbackHandler("/auth/callback", "Acme")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other?code=x", nil))
	if rec.Code != http.StatusNotFound || result.landed() {
		t.Fatal("a GET off the expected path must be a 404 that leaves the result untouched")
	}
	server, err := bindLoopbackCallbackServer("127.0.0.1", 0, handler, func(message, code string) *AuthError { return codexErr(message, code, false) }, "bind_failed")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/auth/callback?code=AC-1&state=st")
		if err == nil {
			resp.Body.Close()
		}
	}()
	callback, err := serveLoopbackCallback(server, result, 10*time.Second, func(message, code string) *AuthError { return codexErr(message, code, false) }, "timeout")
	if err != nil {
		t.Fatal(err)
	}
	if callback["code"] != "AC-1" || callback["state"] != "st" || callback["error"] != nil {
		t.Fatalf("callback %v", callback)
	}
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()
	_, err = bindLoopbackCallbackServer("127.0.0.1", occupant.Addr().(*net.TCPAddr).Port, handler, func(message, code string) *AuthError { return codexErr(message, code, false) }, "port_busy")
	if authErrorOf(t, err).Code != "port_busy" {
		t.Fatalf("busy port error %v", err)
	}
}

func TestPrintLoopbackSSHHint(t *testing.T) {
	scriptBrowserEnv(t, map[string]string{"SSH_CLIENT": "1.2.3.4 1 22", "USER": "nick"})
	var out bytes.Buffer
	printLoopbackSSHHint(&out, "http://127.0.0.1:1455/auth/callback", "https://docs.example")
	text := out.String()
	if !strings.Contains(text, "ssh -N -L 1455:127.0.0.1:1455 nick@") || !strings.Contains(text, "Provider docs:      https://docs.example") {
		t.Fatalf("hint %q", text)
	}
	out.Reset()
	printLoopbackSSHHint(&out, "https://auth.example/callback", "")
	if out.Len() != 0 {
		t.Fatal("a non loopback redirect must print nothing")
	}
	scriptBrowserEnv(t, map[string]string{})
	printLoopbackSSHHint(&out, "http://127.0.0.1:1455/auth/callback", "")
	if out.Len() != 0 {
		t.Fatal("a local session must print nothing")
	}
}

// Scripted token endpoint for the generic poller
type scriptedPoster struct {
	steps []scriptedStep
	calls int
}

func (s *scriptedPoster) post() (*oauthHTTPResponse, error) {
	s.calls++
	step := s.steps[0]
	if len(s.steps) > 1 {
		s.steps = s.steps[1:]
	}
	if step.err != nil {
		return nil, step.err
	}
	body := []byte("not json")
	if step.payload != nil {
		resp := jsonResponse(httptest.NewRequest(http.MethodPost, "https://portal.example/api/oauth/token", nil), step.status, step.payload, step.headers)
		raw := new(bytes.Buffer)
		raw.ReadFrom(resp.Body)
		body = raw.Bytes()
	}
	h := http.Header{}
	for k, v := range step.headers {
		h.Set(k, v)
	}
	return &oauthHTTPResponse{Status: step.status, Header: h, Body: body, URL: "https://portal.example/api/oauth/token"}, nil
}

func genericPoll(t *testing.T, poster *scriptedPoster, expiresIn int, cancelled func() bool) (map[string]any, error, []time.Duration) {
	t.Helper()
	var slept []time.Duration
	prev := oauthSleep
	oauthSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { oauthSleep = prev })
	payload, err := pollDeviceTokenGeneric(deviceTokenPoll{
		post: poster.post, expiresIn: expiresIn, pollInterval: 2,
		validateSuccess: func(payload map[string]any) error {
			if _, ok := payload["access_token"]; !ok {
				return errors.New("Token response did not include access_token")
			}
			return nil
		},
		onNonJSONError: func(*oauthHTTPResponse) error { return errors.New("non json") },
		onError: func(_ *oauthHTTPResponse, payload map[string]any) error {
			return DeviceFlowError(anyString(payload["error"]), anyString(payload["error_description"]), "icebath portal")
		},
		onTimeout: func(cause error) error { return &DeviceCodeExpiredError{Message: "expired", Cause: cause} },
		cancelled: cancelled,
	})
	return payload, err, slept
}

func TestPollDeviceTokenGenericPacing(t *testing.T) {
	poster := &scriptedPoster{steps: []scriptedStep{
		{status: 400, payload: map[string]any{"error": "authorization_pending"}},
		{status: 400, payload: map[string]any{"error": "slow_down"}},
		{status: 200, payload: map[string]any{"access_token": "tok"}},
	}}
	payload, err, slept := genericPoll(t, poster, 600, nil)
	if err != nil || payload["access_token"] != "tok" || poster.calls != 3 {
		t.Fatalf("payload %v err %v calls %d", payload, err, poster.calls)
	}
	if len(slept) != 2 || slept[0] != 2*time.Second || slept[1] != 3*time.Second {
		t.Fatalf("pacing %v", slept)
	}
}

func TestPollDeviceTokenGenericEdgeBackoffAndErrors(t *testing.T) {
	poster := &scriptedPoster{steps: []scriptedStep{
		{status: 503},
		{status: 429, headers: map[string]string{"Retry-After": "7"}},
		{status: 403, headers: map[string]string{"x-vercel-mitigated": "challenge"}},
		{status: 200, payload: map[string]any{"access_token": "tok"}},
	}}
	payload, err, slept := genericPoll(t, poster, 600, nil)
	if err != nil || payload["access_token"] != "tok" {
		t.Fatalf("payload %v err %v", payload, err)
	}
	if len(slept) != 3 || slept[0] != 5*time.Second || slept[1] != 7*time.Second || slept[2] != 14*time.Second {
		t.Fatalf("backoff %v", slept)
	}
	poster = &scriptedPoster{steps: []scriptedStep{{status: 400}}}
	_, err, _ = genericPoll(t, poster, 600, nil)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != 400 {
		t.Fatalf("non json 400 must raise for status: %v", err)
	}
	poster = &scriptedPoster{steps: []scriptedStep{{status: 400, payload: map[string]any{"error": "access_denied", "error_description": "declined"}}}}
	_, err, _ = genericPoll(t, poster, 600, nil)
	var copyErr *SignInCopyError
	if !errors.As(err, &copyErr) || copyErr.OAuthErrorCode != "access_denied" || !strings.Contains(copyErr.Message, "Details: access_denied: declined") {
		t.Fatalf("error payload must become provider copy: %v", err)
	}
	poster = &scriptedPoster{steps: []scriptedStep{{status: 200, payload: map[string]any{"token_type": "bearer"}}}}
	_, err, _ = genericPoll(t, poster, 600, nil)
	if err == nil || !strings.Contains(err.Error(), "did not include access_token") {
		t.Fatalf("validation failure %v", err)
	}
}

func TestPollDeviceTokenGenericTimeoutAndCancel(t *testing.T) {
	poster := &scriptedPoster{steps: []scriptedStep{{status: 503}, {status: 503}, {status: 503}}}
	_, err, _ := genericPoll(t, poster, 1, nil)
	var expired *DeviceCodeExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("expected the device code to expire: %v", err)
	}
	if !IsDeviceCodeExpired(err) {
		t.Fatal("expired error not recognised")
	}
	poster = &scriptedPoster{steps: []scriptedStep{{status: 400, payload: map[string]any{"error": "authorization_pending"}}, {status: 200, payload: map[string]any{"access_token": "tok"}}}}
	cancelled := false
	_, err, _ = genericPoll(t, poster, 600, func() bool { return cancelled })
	if err != nil {
		t.Fatal(err)
	}
	cancelled = true
	poster = &scriptedPoster{steps: []scriptedStep{{status: 200, payload: map[string]any{"access_token": "tok"}}}}
	_, err, _ = genericPoll(t, poster, 600, func() bool { return cancelled })
	var flowCancelled *DeviceFlowCancelledError
	if !errors.As(err, &flowCancelled) || poster.calls != 0 || !IsCancelled(err) {
		t.Fatalf("cancel must end the poll before a request: %v calls %d", err, poster.calls)
	}
}

func TestPollForTokenAndRequestDeviceCode(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device/code":
			_ = r.ParseForm()
			if r.Form.Get("scope") != "inference:invoke" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error": "bad scope"}`))
				return
			}
			_, _ = w.Write([]byte(`{"device_code": "dc", "user_code": "UC", "verification_uri": "https://p/x", "expires_in": 600, "interval": 5}`))
		case "/api/oauth/token":
			hits++
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != DeviceCodeGrantType || r.Form.Get("device_code") != "dc" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error": "invalid_request", "error_description": "bad form"}`))
				return
			}
			if hits == 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error": "authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token": "at", "refresh_token": "rt"}`))
		}
	}))
	defer server.Close()
	prev := oauthSleep
	oauthSleep = func(time.Duration) {}
	t.Cleanup(func() { oauthSleep = prev })
	_, env := codexTestEnv(t)
	client := oauthHTTPClient(env, 5*time.Second, defaultOAuthVerify())
	_, err := requestDeviceCode(context.Background(), client, server.URL, "icebath-cli", "inference:invoke")
	if err == nil || !strings.Contains(err.Error(), "Device code response missing fields: verification_uri_complete") {
		t.Fatalf("missing field error %v", err)
	}
	_, err = requestDeviceCode(context.Background(), client, server.URL, "icebath-cli", "")
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != 400 {
		t.Fatalf("status error %v", err)
	}
	payload, err := pollForToken(context.Background(), client, server.URL, "icebath-cli", "dc", 600, 5, nil)
	if err != nil || payload["access_token"] != "at" || hits != 2 {
		t.Fatalf("payload %v err %v hits %d", payload, err, hits)
	}
	_, err = pollForToken(context.Background(), client, server.URL, "icebath-cli", "other", 600, 5, nil)
	var copyErr *SignInCopyError
	if !errors.As(err, &copyErr) || !strings.HasPrefix(copyErr.Message, "Sign-in did not complete: bad form. Run `icebath portal` to try again.") {
		t.Fatalf("error copy %v", err)
	}
	if msg := nousDeviceAuthTimeoutMessage(""); !strings.Contains(msg, "icebath portal") || !strings.Contains(msg, DefaultNousPortalURL+"/login") {
		t.Fatalf("timeout message %q", msg)
	}
}

func TestPromptYesNo(t *testing.T) {
	prev := oauthPromptFn
	t.Cleanup(func() { oauthPromptFn = prev })
	answers := []string{"", "N", "yes", "y"}
	oauthPromptFn = func(string) (string, error) {
		a := answers[0]
		answers = answers[1:]
		return a + "\n", nil
	}
	if !promptYesNo("? ", "y") || promptYesNo("? ", "y") || !promptYesNo("? ", "n") || !promptYesNo("? ", "n") {
		t.Fatal("answers not mapped")
	}
	oauthPromptFn = func(string) (string, error) { return "", errors.New("EOF") }
	if !promptYesNo("? ", "y") || promptYesNo("? ", "n") {
		t.Fatal("EOF must count as the default")
	}
}

func TestPrintDeviceCodeInstructions(t *testing.T) {
	prev := openBrowserFn
	t.Cleanup(func() { openBrowserFn = prev })
	opened := []string{}
	openBrowserFn = func(target string) error { opened = append(opened, target); return nil }
	var out bytes.Buffer
	if err := printDeviceCodeInstructions(&out, "https://p/verify", "UC-1", true, "", false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "  1. Open: https://p/verify") || !strings.Contains(text, "  2. If prompted, enter code: UC-1") || !strings.Contains(text, "(Opened browser for verification)") {
		t.Fatalf("instructions %q", text)
	}
	openBrowserFn = func(string) error { return errors.New("no browser") }
	out.Reset()
	if err := printDeviceCodeInstructions(&out, "https://p/verify", "UC-1", true, "—", true); err != nil || !strings.Contains(out.String(), "Could not open browser automatically — use the URL above.") {
		t.Fatalf("swallowed open failure %v %q", err, out.String())
	}
	if err := printDeviceCodeInstructions(&out, "https://p/verify", "UC-1", true, "", false); err == nil {
		t.Fatal("open failure must surface when not swallowed")
	}
	out.Reset()
	if err := printDeviceCodeInstructions(&out, "https://p/verify", "UC-1", false, "", false); err != nil || strings.Contains(out.String(), "browser") {
		t.Fatalf("no open requested %v %q", err, out.String())
	}
	if len(opened) != 1 {
		t.Fatalf("opened %v", opened)
	}
}

func TestOfferExistingOAuthCredentials(t *testing.T) {
	h, env := codexTestEnv(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	prev := oauthPromptFn
	oauthPromptFn = func(string) (string, error) { return "y\n", nil }
	t.Cleanup(func() { oauthPromptFn = prev })
	never := func(string, time.Duration) bool { return false }
	var out bytes.Buffer
	accepted, err := offerExistingOAuthCredentials(env, &out, "openai-codex",
		func() (map[string]any, error) { return map[string]any{"api_key": "tok", "base_url": testGW}, nil }, never, "Codex", DefaultCodexBaseURL, "")
	if err != nil || !accepted {
		t.Fatalf("accepted %v err %v", accepted, err)
	}
	model := readModelCfg(t, h)
	if model["provider"] != "openai-codex" || model["base_url"] != testGW || !strings.Contains(out.String(), "Login successful!") {
		t.Fatalf("config %v output %q", model, out.String())
	}
	out.Reset()
	accepted, err = offerExistingOAuthCredentials(env, &out, "openai-codex",
		func() (map[string]any, error) { return nil, codexErr("none", "codex_auth_missing", true) }, never, "Codex", DefaultCodexBaseURL, "expired notice")
	if err != nil || accepted || out.Len() != 0 {
		t.Fatalf("auth error must be a quiet no: %v %v %q", accepted, err, out.String())
	}
	always := func(string, time.Duration) bool { return true }
	accepted, err = offerExistingOAuthCredentials(env, &out, "openai-codex",
		func() (map[string]any, error) { return map[string]any{"api_key": "tok"}, nil }, always, "Codex", DefaultCodexBaseURL, "expired notice")
	if err != nil || accepted || !strings.Contains(out.String(), "expired notice") {
		t.Fatalf("expired token must print the notice: %v %v %q", accepted, err, out.String())
	}
}

func TestResolveOAuthVerify(t *testing.T) {
	h, env := codexTestEnv(t)
	for _, key := range []string{"ICEBATH_CA_BUNDLE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"} {
		t.Setenv(key, "")
	}
	insecure := true
	if v := resolveOAuthVerify(env, &insecure, "", nil); !v.Insecure || v.TLS == nil || !v.TLS.InsecureSkipVerify {
		t.Fatalf("insecure %+v", v)
	}
	if v := resolveOAuthVerify(env, nil, "", map[string]any{"tls": map[string]any{"insecure": true}}); !v.Insecure {
		t.Fatal("stored insecure flag ignored")
	}
	logs := &recordingHandler{}
	env.Log = newRecordingLogger(logs)
	if v := resolveOAuthVerify(env, nil, filepath.Join(h, "missing.pem"), nil); v.Insecure || v.CABundle != "" || len(logs.containing("CA bundle path does not exist")) != 1 {
		t.Fatalf("missing bundle %+v logs %v", v, logs.messages)
	}
	bundle := filepath.Join(h, "ca.pem")
	if err := os.WriteFile(bundle, []byte("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := resolveOAuthVerify(env, nil, "", map[string]any{"tls": map[string]any{"ca_bundle": bundle}}); v.CABundle != bundle || v.TLS == nil {
		t.Fatalf("stored bundle %+v", v)
	}
	if state := tlsStateFromVerify(false, bundle); state["ca_bundle"] != bundle || state["insecure"] != false {
		t.Fatalf("tls state %v", state)
	}
}

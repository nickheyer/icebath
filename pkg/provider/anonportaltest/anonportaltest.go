package anonportaltest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	HomeEnv                  = "ICEBATH_HOME"
	PortalBaseURLEnv         = "ICEBATH_PORTAL_BASE_URL"
	CABundleEnv              = "ICEBATH_CA_BUNDLE"
	AnonSecretEnv            = "ICEBATH_ANON_API_SECRET"
	SharedAuthDirEnv         = "ICEBATH_SHARED_AUTH_DIR"
	DesktopEnv               = "ICEBATH_DESKTOP"
	PreviewFullConnectorsEnv = "ICEBATH_PREVIEW_FULL_CONNECTORS"
	Secret                   = "test-secret"
	Welcome                  = "https://welcome-api.nousresearch.com/v1"
)

// Env names unset for the test so no real key or Desktop marker leaks into the portal flows
var ClearedEnv = []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY", DesktopEnv, PreviewFullConnectorsEnv}

type Portal struct {
	URL       string
	Home      string
	SharedDir string
	CAPath    string

	mu            sync.Mutex
	handler       http.Handler
	calls         []string
	tokenRequests []map[string]any
	minted        int
}

// Fake NAS anonymous surface behind a trusted TLS portal with home, stores and portal env isolated
func Install(t testing.TB) *Portal {
	t.Helper()
	return Serve(t, nil)
}

// TLS portal answering through h, the default NAS surface when h is nil, with the portal env pointed at it
func Serve(t testing.TB, h http.Handler) *Portal {
	t.Helper()
	p := &Portal{handler: h, Home: t.TempDir()}
	t.Setenv(HomeEnv, p.Home)
	server := httptest.NewTLSServer(p)
	t.Cleanup(server.Close)
	p.URL = server.URL
	dir := t.TempDir()
	p.CAPath = filepath.Join(dir, "portal-ca.pem")
	p.SharedDir = filepath.Join(dir, "shared-store")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(p.CAPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(CABundleEnv, p.CAPath)
	t.Setenv(PortalBaseURLEnv, p.URL)
	t.Setenv(AnonSecretEnv, Secret)
	t.Setenv(SharedAuthDirEnv, p.SharedDir)
	for _, name := range ClearedEnv {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return p
}

// Records the request path then answers through the installed handler or the default NAS surface
func (p *Portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls = append(p.calls, r.URL.Path)
	h := p.handler
	p.mu.Unlock()
	if h != nil {
		h.ServeHTTP(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	p.mu.Lock()
	status, body := p.answer(r, raw)
	p.mu.Unlock()
	if status == 0 {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// Default NAS answer for one request, status zero dropping the connection where the upstream fake raises
func (p *Portal) answer(r *http.Request, raw []byte) (int, map[string]any) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/anonymous/") && r.Header.Get("x-anonymous-api-secret") == "" {
		return 401, map[string]any{"error": "invalid_shared_secret"}
	}
	switch path {
	case "/api/anonymous/create":
		if len(raw) > 0 && !json.Valid(raw) {
			return 0, nil
		}
		p.minted++
		return 201, CreateBody(p.minted)
	case "/api/anonymous/challenge/status":
		return 200, map[string]any{"status": "none"}
	case "/api/anonymous/token":
		var body any
		if err := json.Unmarshal(raw, &body); err != nil {
			return 0, nil
		}
		var agent any
		if values := r.Header.Values("user-agent"); len(values) > 0 {
			agent = strings.Join(values, ", ")
		}
		p.tokenRequests = append(p.tokenRequests, map[string]any{"body": body, "user_agent": agent})
		req, ok := body.(map[string]any)
		if !ok {
			return 0, nil
		}
		if _, ok := req["token"]; !ok {
			return 0, nil
		}
		return 200, TokenBody(req["purpose"] == "connectors", Welcome)
	}
	return 500, map[string]any{"error": "unexpected " + path}
}

// Number of requests that reached the create endpoint
func (p *Portal) Creates() int {
	n := 0
	for _, path := range p.Calls() {
		if path == "/api/anonymous/create" {
			n++
		}
	}
	return n
}

// Token exchanges the default surface answered, each with its decoded body and user agent
func (p *Portal) TokenRequests() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any{}, p.tokenRequests...)
}

// Every request path the portal received in arrival order
func (p *Portal) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.calls...)
}

// Unsigned anonymous NAS JWT with the default inference claims overridden by claims
func JWT(claims map[string]any) string {
	payload := map[string]any{"sub": "nas_user:1", "client_id": "nas-anonymous", "account_tier": "anonymous",
		"scope": "inference:invoke", "exp": time.Now().Unix() + 900}
	for k, v := range claims {
		payload[k] = v
	}
	seg := func(obj map[string]any) string {
		raw, _ := json.Marshal(obj)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return seg(map[string]any{"alg": "RS256"}) + "." + seg(payload) + ".sig"
}

// Create endpoint body for the nth guest minted
func CreateBody(n int) map[string]any {
	return map[string]any{"user_id": fmt.Sprintf("nas_user:%d", n), "org_id": "nas_org:1",
		"token": fmt.Sprintf("anon_%04d", n), "idle_ttl_days": 14}
}

// Token exchange body, connectors only scope when asked and the inference host unless empty
func TokenBody(connectors bool, inferenceBaseURL string) map[string]any {
	claims := map[string]any{}
	if connectors {
		claims["scope"] = "connectors:invoke"
	}
	body := map[string]any{"access_token": JWT(claims), "token_type": "Bearer", "expires_in": 900,
		"user_id": "nas_user:1", "org_id": "nas_org:1"}
	if inferenceBaseURL != "" {
		body["inference_base_url"] = inferenceBaseURL
	}
	return body
}

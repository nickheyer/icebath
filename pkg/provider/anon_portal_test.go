package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const anonWelcome = "https://welcome-api.nousresearch.com/v1"

// Anonymous NAS JWT with the fake portal's default claims overridden by claims
func anonJWT(claims map[string]any) string {
	payload := map[string]any{"sub": "nas_user:1", "client_id": "nas-anonymous", "account_tier": "anonymous",
		"scope": "inference:invoke", "exp": time.Now().Unix() + 900}
	for k, v := range claims {
		payload[k] = v
	}
	return jwtWithClaims(payload)
}

type anonCanned struct {
	status int
	body   map[string]any
}

// Minimal NAS anonymous surface over TLS that records every call
type anonFakePortal struct {
	mu                    sync.Mutex
	server                *httptest.Server
	URL                   string
	calls                 []string
	deadTokens            map[string]bool
	gateClosed            bool
	minted                int
	inferenceBaseURL      string
	createResponse        *anonCanned
	tokenResponse         *anonCanned
	raiseTransport        bool
	challengeRequired     bool
	challengeNeverClears  bool
	challengeStatuses     []string
	challengeStatusErrors []int
	challengeURL          string
	optionalChallenge     bool
	honoursPurpose        bool
	tokenRequests         []map[string]any
	createRequests        []map[string]any
	afterHandle           func(p *anonFakePortal)
	routes                map[string]func(r *http.Request, req map[string]any) (int, map[string]any)
}

func (p *anonFakePortal) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *anonFakePortal) count(path string) int {
	n := 0
	for _, c := range p.paths() {
		if c == path {
			n++
		}
	}
	return n
}

func (p *anonFakePortal) clearCalls() {
	p.mu.Lock()
	p.calls = nil
	p.mu.Unlock()
}

func (p *anonFakePortal) set(fn func(p *anonFakePortal)) {
	p.mu.Lock()
	fn(p)
	p.mu.Unlock()
}

func (p *anonFakePortal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	p.mu.Lock()
	status, body := p.handle(r, raw)
	after := p.afterHandle
	p.mu.Unlock()
	if status == 0 {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
		return
	}
	if after != nil {
		p.mu.Lock()
		after(p)
		p.mu.Unlock()
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// Answers one request under the portal lock, status zero dropping the connection
func (p *anonFakePortal) handle(r *http.Request, raw []byte) (int, map[string]any) {
	path := r.URL.Path
	p.calls = append(p.calls, path)
	if p.raiseTransport {
		return 0, nil
	}
	if len(path) > len("/api/anonymous/") && path[:len("/api/anonymous/")] == "/api/anonymous/" && r.Header.Get("x-anonymous-api-secret") == "" {
		return 401, map[string]any{"error": "invalid_shared_secret"}
	}
	if p.gateClosed {
		return 401, map[string]any{"error": "invalid_shared_secret"}
	}
	var req map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &req)
	}
	if req == nil {
		req = map[string]any{}
	}
	if route := p.routes[path]; route != nil {
		return route(r, req)
	}
	switch path {
	case "/api/anonymous/create":
		p.createRequests = append(p.createRequests, req)
		if p.createResponse != nil {
			return p.createResponse.status, p.createResponse.body
		}
		p.minted++
		return 201, map[string]any{"user_id": fmt.Sprintf("nas_user:%d", p.minted), "org_id": "nas_org:1",
			"token": fmt.Sprintf("anon_%04d", p.minted), "idle_ttl_days": 14}
	case "/api/anonymous/challenge/status":
		if len(p.challengeStatusErrors) > 0 {
			code := p.challengeStatusErrors[0]
			p.challengeStatusErrors = p.challengeStatusErrors[1:]
			return code, map[string]any{"error": "blip"}
		}
		if len(p.challengeStatuses) > 0 {
			status := p.challengeStatuses[0]
			p.challengeStatuses = p.challengeStatuses[1:]
			return 200, map[string]any{"status": status}
		}
		if p.challengeNeverClears {
			return 200, map[string]any{"status": "pending"}
		}
		p.challengeRequired = false
		return 200, map[string]any{"status": "none"}
	case "/api/anonymous/token":
		p.tokenRequests = append(p.tokenRequests, map[string]any{"body": req, "user_agent": r.Header.Get("user-agent")})
		if p.tokenResponse != nil {
			return p.tokenResponse.status, p.tokenResponse.body
		}
		connectors := p.honoursPurpose && req["purpose"] == "connectors"
		token, _ := req["token"].(string)
		if p.deadTokens[token] {
			return 404, map[string]any{"error": "unknown_token"}
		}
		if p.challengeRequired && !connectors {
			return 428, map[string]any{"error": "challenge_required", "message": "A quick check first.",
				"challenges": []any{map[string]any{"type": "browser", "url": p.challengeURL, "state": "pending",
					"required": true, "expires_in": 600, "interval": 2}},
				"fallback_url": p.URL + "/login"}
		}
		claims := map[string]any{}
		if connectors {
			claims["scope"] = "connectors:invoke"
		}
		body := map[string]any{"access_token": anonJWT(claims), "token_type": "Bearer", "expires_in": 900,
			"user_id": "nas_user:1", "org_id": "nas_org:1"}
		if p.inferenceBaseURL != "" {
			body["inference_base_url"] = p.inferenceBaseURL
		}
		if p.optionalChallenge && !connectors {
			body["challenges"] = []any{map[string]any{"type": "browser", "url": p.challengeURL, "state": "pending",
				"required": false, "expires_in": 600, "interval": 2}}
		}
		return 200, body
	}
	return 500, map[string]any{"error": "unexpected " + path}
}

// Routes every Nous client at a fake portal, isolates the stores and resets the per process memos
func installAnonPortal(t *testing.T) (*anonFakePortal, PoolEnv) {
	t.Helper()
	resetNousSeams(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	fake := &anonFakePortal{deadTokens: map[string]bool{}, inferenceBaseURL: anonWelcome, honoursPurpose: true}
	fake.server = httptest.NewTLSServer(fake)
	t.Cleanup(fake.server.Close)
	fake.URL = fake.server.URL
	fake.challengeURL = fake.URL + "/challenge?code=ticket"
	caPath := filepath.Join(t.TempDir(), "portal-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fake.server.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_CA_BUNDLE", caPath)
	t.Setenv("ICEBATH_PORTAL_BASE_URL", fake.URL)
	t.Setenv(AnonSecretEnv, "test-secret")
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY", "ICEBATH_DESKTOP", PreviewFullConnectorsEnv} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	resetMintMemoForTests()
	resetChallengeForTests()
	sleep, wait, present, stderr := challengeSleep, challengeWait, presentChallengeFn, challengeStderr
	challengeSleep = func(time.Duration) {}
	t.Cleanup(func() {
		challengeSleep, challengeWait, presentChallengeFn, challengeStderr = sleep, wait, present, stderr
		SetFreeTierGatewayHooks(FreeTierGatewayHooks{})
		resetMintMemoForTests()
		resetChallengeForTests()
	})
	return fake, modelsPoolEnv(context.Background())
}

// The env as work for the local Desktop app
func anonDesktopEnv(env PoolEnv) PoolEnv {
	env.Ctx = tools.WithServingLocalDesktop(env.ctx(), true)
	return env
}

// A guest stamped with the welcome trial claim, minted through the fake portal on the desktop backend
func anonClaimedTrial(t *testing.T, env PoolEnv) map[string]any {
	t.Helper()
	state, err := EnsurePortalIdentity(anonDesktopEnv(env), PortalIdentityOptions{Explicit: true, WelcomeClaim: true})
	if err != nil || state == nil {
		t.Fatalf("claim: %v %v", state, err)
	}
	return state
}

// The shared Nous store contents, empty when absent
func anonSharedStore(t *testing.T, env PoolEnv) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(nousSharedStorePath(env))
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

// The profile auth store's providers section
func anonProviders(t *testing.T, env PoolEnv) map[string]any {
	t.Helper()
	data, err := env.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	providers, _ := data["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	return providers
}

// Rewrites the profile's providers.nous fields under the auth store lock
func anonPatchNous(t *testing.T, env PoolEnv, fields map[string]any) {
	t.Helper()
	err := env.Store.Update(func(data AuthData) (bool, error) {
		nous := section(data, "providers")["nous"].(map[string]any)
		for k, v := range fields {
			nous[k] = v
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Writes config.yaml with a nous section of the given keys
func anonWriteNousConfig(t *testing.T, env PoolEnv, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.Store.Home(), "config.yaml"), []byte("nous:\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The coded auth error carried by err
func anonCodeOf(t *testing.T, err error) *AuthError {
	t.Helper()
	ae := anonAuthErrorOf(err)
	if ae == nil {
		var wrapped *AuthError
		if !asAuthError(err, &wrapped) {
			t.Fatalf("not an AuthError: %T %v", err, err)
		}
		ae = wrapped
	}
	return ae
}

func asAuthError(err error, target **AuthError) bool {
	for err != nil {
		if ae := anonAuthErrorOf(err); ae != nil {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func anonRetryable(ae *AuthError) bool { return ae.Retryable != nil && *ae.Retryable }

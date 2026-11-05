package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
)

const (
	planeIDPToken  = "plane-token-1"
	planeNousToken = "nous-access-token-1"
	planeInstance  = "inst-0001"
	planeClientID  = "agent-client"
	planeSecret    = "agent-secret"
)

type planeRequest struct{ method, profile, auth string }

// Config plane double serving one upper document and per profile values over the remote wire contract
type bootPlane struct {
	mu            sync.Mutex
	upper         map[string]any
	values        map[string]map[string]any
	failStatus    int
	requests      []planeRequest
	nousRefreshes int
	onGet         func()
	url           string
	home          string
}

func newBootPlane(t *testing.T) *bootPlane {
	p := &bootPlane{upper: map[string]any{}, values: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func planeMerge(base, over map[string]any) map[string]any {
	out := maps.Clone(base)
	for k, v := range over {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = planeMerge(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func planeSend(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	if etag, ok := body["etag"].(string); ok {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (p *bootPlane) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method == "GET" && p.onGet != nil {
		p.onGet()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.URL.Path {
	case "/api/oauth/token":
		p.nousRefreshes++
		planeSend(w, 200, map[string]any{"access_token": planeNousToken, "refresh_token": "nous-refresh-2", "token_type": "Bearer", "expires_in": 3600})
		return
	case "/token":
		if !strings.Contains(string(raw), "client_secret="+planeSecret) {
			planeSend(w, 401, map[string]any{"error": "invalid_client"})
			return
		}
		planeSend(w, 200, map[string]any{"access_token": planeIDPToken, "token_type": "Bearer", "expires_in": 3600})
		return
	}
	profile := r.URL.Query().Get("profile")
	if profile == "" {
		profile = "default"
	}
	p.requests = append(p.requests, planeRequest{r.Method, profile, r.Header.Get("Authorization")})
	if p.failStatus != 0 {
		planeSend(w, p.failStatus, map[string]any{"error": "unavailable", "message": "plane outage"})
		return
	}
	if auth := r.Header.Get("Authorization"); auth != "Bearer "+planeIDPToken && auth != "Bearer "+planeNousToken {
		planeSend(w, 401, map[string]any{"error": "unauthorized", "message": "bad token"})
		return
	}
	if r.Header.Get("X-Hermes-Instance-Id") != planeInstance {
		planeSend(w, 403, map[string]any{"error": "config_agent_unknown", "message": "unknown instance"})
		return
	}
	levels := []any{map[string]any{"kind": "tenant", "version": 1, "writerConfigVersion": nil},
		map[string]any{"kind": "profile", "version": 1, "writerConfigVersion": config.LatestConfigVersion()}}
	planeSend(w, 200, map[string]any{"instanceId": planeInstance, "profile": profile, "config": planeMerge(p.upper, p.values[profile]),
		"locks": []any{}, "provenance": map[string]any{}, "levels": levels, "profileVersion": 1, "recordVersion": 1,
		"etag": fmt.Sprintf(`"%d"`, len(p.requests))})
}

func (p *bootPlane) gets() []planeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []planeRequest
	for _, r := range p.requests {
		if r.method == "GET" {
			out = append(out, r)
		}
	}
	return out
}

func (p *bootPlane) env(idp bool) map[string]string {
	env := map[string]string{config.BackendEnv: "remote", remote.URLEnv: p.url, remote.InstanceEnv: planeInstance}
	if idp {
		env["GATEWAY_RELAY_IDP_TOKEN_URL"] = p.url + "/token"
		env["GATEWAY_RELAY_IDP_CLIENT_ID"] = planeClientID
		env["GATEWAY_RELAY_IDP_CLIENT_SECRET"] = planeSecret
	}
	return env
}

// Temp profile home served by a fresh remote backend with the deployment in the process env
func setupBootPlane(t *testing.T, idp bool) *bootPlane {
	p := newBootPlane(t)
	p.home = filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(p.home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", p.home)
	for _, name := range append(remote.PlaneCredentialEnvNames, config.BackendEnv) {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	for k, v := range p.env(idp) {
		t.Setenv(k, v)
	}
	saved := remote.BootRetryDelays
	remote.BootRetryDelays = []float64{0, 0}
	remote.ResetForTests()
	t.Cleanup(func() {
		remote.ResetForTests()
		remote.BootRetryDelays = saved
	})
	return p
}

func loadDotenv(p *bootPlane) error {
	_, err := LoadIcebathDotenv(context.Background(), DotenvLoadOptions{Home: p.home})
	return err
}

// The dotenv load boots the profile fetch once per process per profile
func TestRemoteBootFetchRunsInsideLoadIcebathDotenv(t *testing.T) {
	p := setupBootPlane(t, true)
	if err := loadDotenv(p); err != nil || len(p.gets()) != 1 {
		t.Fatalf("first load %v gets %d", err, len(p.gets()))
	}
	if err := loadDotenv(p); err != nil || len(p.gets()) != 1 {
		t.Fatalf("second load %v gets %d", err, len(p.gets()))
	}
}

// An outage through the dotenv load retries twice and then refuses to start
func TestRemoteDotenvBootRetriesThenExitsOnOutage(t *testing.T) {
	p := setupBootPlane(t, true)
	p.failStatus = 503
	err := loadDotenv(p)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "does not start") || len(p.gets()) != 3 {
		t.Fatalf("load %v gets %d", err, len(p.gets()))
	}
}

// A plane refusal through the dotenv load is not retried
func TestRemoteDotenvBootDoesNotRetryARefusal(t *testing.T) {
	p := setupBootPlane(t, true)
	t.Setenv(remote.InstanceEnv, "someone-else")
	err := loadDotenv(p)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "config_agent_unknown") || len(p.gets()) != 1 {
		t.Fatalf("load %v gets %d", err, len(p.gets()))
	}
}

func protectedRefusal(t *testing.T, report *ApplyReport) (msg string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			unavailable, ok := r.(*ConfigBackendUnavailableError)
			if !ok {
				panic(r)
			}
			msg = unavailable.Message
		}
	}()
	refuseProtectedEnvFromSources(report, configBackendView(""), nil, nil, "")
	return ""
}

// A secrets source supplying a plane credential refuses the start, unrelated names pass
func TestSecretSourceSupplyingPlaneCredentialRefusesStart(t *testing.T) {
	setupBootPlane(t, true)
	msg := protectedRefusal(t, &ApplyReport{Provenance: map[string]AppliedVar{"GATEWAY_RELAY_IDP_CLIENT_SECRET": {}},
		Sources: []*SourceReport{{SkippedExisting: []string{remote.URLEnv}}}})
	if !strings.Contains(msg, "GATEWAY_RELAY_IDP_CLIENT_SECRET") || !strings.Contains(msg, remote.URLEnv) {
		t.Fatalf("refusal %q", msg)
	}
	if msg := protectedRefusal(t, &ApplyReport{Provenance: map[string]AppliedVar{"OPENROUTER_API_KEY": {}}}); msg != "" {
		t.Fatalf("unrelated name refused %q", msg)
	}
}

// The file backend protects no plane credential name
func TestFileBackendHasNoProtectedNames(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(config.BackendEnv, "")
	os.Unsetenv(config.BackendEnv)
	if msg := protectedRefusal(t, &ApplyReport{Provenance: map[string]AppliedVar{"GATEWAY_RELAY_IDP_CLIENT_SECRET": {}}}); msg != "" {
		t.Fatalf("file backend refused %q", msg)
	}
}

// Runs fn in its own goroutine, failing the test when it outlives the deadlock bound
func withinBound(t *testing.T, bound time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(bound):
		t.Fatalf("did not finish in %s: the first remote read deadlocked", bound)
	}
}

// A cloud agent first remote read with the auth.json Nous credential completes, refreshing an expired token once
func TestCloudAgentFirstRemoteReadCompletesCold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool
		layout  string
	}{{"valid-token", false, ""}, {"token-refreshed-at-boot", true, ""}, {"home-provider-plugin", false, "model-providers"}, {"flat-provider-plugin", false, "flat"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := setupBootPlane(t, false)
			t.Setenv("HOME", filepath.Dir(p.home))
			if tc.layout != "" {
				dir := filepath.Join(p.home, "plugins", "probe")
				if tc.layout == "model-providers" {
					dir = filepath.Join(p.home, "plugins", "model-providers", "probe")
				}
				os.MkdirAll(dir, 0o755)
				os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: probe\nkind: model-provider\nversion: 0.0.1\n"), 0o644)
			}
			p.upper = map[string]any{"display": map[string]any{"skin": "ares"}}
			token, offset := planeNousToken, time.Hour
			if tc.expired {
				token, offset = "expired-token", -time.Hour
			}
			auth := map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"nous": map[string]any{
				"access_token": token, "refresh_token": "nous-refresh-1", "client_id": "icebath-cli-vps", "portal_base_url": p.url,
				"expires_at": time.Now().Add(offset).UTC().Format("2006-01-02T15:04:05+00:00")}}}
			raw, _ := json.Marshal(auth)
			os.WriteFile(filepath.Join(p.home, "auth.json"), raw, 0o600)
			var skin any
			var err error
			withinBound(t, 60*time.Second, func() {
				var tree config.Tree
				tree, err = config.LoadContext(context.Background(), p.home, os.LookupEnv)
				skin = tree.Get("display.skin")
			})
			if err != nil || skin != "ares" {
				t.Fatalf("load %v skin %v", err, skin)
			}
			gets := p.gets()
			for _, g := range gets {
				if g.auth != "Bearer "+planeNousToken {
					t.Fatalf("auth %q", g.auth)
				}
			}
			want := 0
			if tc.expired {
				want = 1
			}
			if len(gets) == 0 || p.nousRefreshes != want {
				t.Fatalf("gets %d refreshes %d", len(gets), p.nousRefreshes)
			}
			if _, err := os.Stat(filepath.Join(p.home, "config.yaml")); err == nil {
				t.Fatal("config.yaml written")
			}
		})
	}
}

// A concurrent first reader waits for the deployment bootstrap instead of reading the local config.yaml
func TestRemoteConcurrentFirstReaderWaitsForTheBootstrap(t *testing.T) {
	p := setupBootPlane(t, true)
	managed := filepath.Join(t.TempDir(), "managed")
	os.MkdirAll(managed, 0o755)
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	var lines strings.Builder
	for k, v := range p.env(true) {
		fmt.Fprintf(&lines, "%s=%s\n", k, v)
		os.Unsetenv(k)
	}
	os.WriteFile(filepath.Join(p.home, ".env"), []byte(lines.String()), 0o600)
	os.WriteFile(filepath.Join(p.home, "config.yaml"), []byte("display:\n  personality: forbidden-local\n"), 0o644)
	p.values["default"] = map[string]any{"display": map[string]any{"personality": "remote"}}
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	config.ResetBootstrapForTests()
	config.RegisterConfigBootstrapEnv(func(ctx context.Context, names func(string) []string, h, project string) error {
		once.Do(func() { close(entered) })
		<-resume
		return applyConfigBootstrapEnv(ctx, names, h, project)
	})
	t.Cleanup(func() { config.RegisterConfigBootstrapEnv(applyConfigBootstrapEnv) })
	var mu sync.Mutex
	results := map[string]any{}
	firstDone, concurrentDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(firstDone)
		backend, err := config.GetConfigBackend(context.Background())
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results["first"] = err
			return
		}
		results["first"] = backend.Name()
	}()
	<-entered
	go func() {
		defer close(concurrentDone)
		doc, err := config.ReadConfigDoc(context.Background(), filepath.Join(p.home, "config.yaml"))
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results["concurrent"] = err
			return
		}
		results["concurrent"] = doc.(map[string]any)["display"].(map[string]any)["personality"]
	}()
	select {
	case <-concurrentDone:
		t.Fatal("the concurrent reader did not wait for the bootstrap")
	case <-time.After(300 * time.Millisecond):
	}
	close(resume)
	for _, ch := range []chan struct{}{firstDone, concurrentDone} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out")
		}
	}
	if results["first"] != "remote" || results["concurrent"] != "remote" {
		t.Fatalf("results %v", results)
	}
}

// The file backend model switch keeps its key by key round trip with comments and siblings intact
func TestFileBackendModelSwitchStillWritesEveryKey(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(config.BackendEnv, "")
	os.Unsetenv(config.BackendEnv)
	remote.ResetForTests()
	cfg := filepath.Join(h, "config.yaml")
	os.WriteFile(cfg, []byte("# keep me\nmodel:\n  default: old\n  provider: old-p\n  api_mode: x\n  model_slots:\n    fast: mini\n"), 0o644)
	result := ModelSwitchResult{Success: true, NewModel: "new-model", TargetProvider: "new-provider", ProviderChanged: true, IsGlobal: true}
	if err := PersistModelSelection(context.Background(), result, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	doc, err := config.ReadUser(h)
	if err != nil || !strings.Contains(string(data), "# keep me") {
		t.Fatalf("text %q %v", data, err)
	}
	want := map[string]any{"default": "new-model", "provider": "new-provider", "model_slots": map[string]any{"fast": "mini"}}
	if got, _ := json.Marshal(doc["model"]); string(got) != string(mustJSONBytes(json.Marshal(want))) {
		t.Fatalf("model %s", got)
	}
}

func mustJSONBytes(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

const refusingKind = "refuseall_nous"

var (
	refusingOnce  sync.Once
	refusingMu    sync.Mutex
	refusingReads int
)

// File backend whose every config read is recorded and refused
type refusingBackend struct{ config.FileBackend }

func (refusingBackend) Name() string { return refusingKind }

func (refusingBackend) refuse() error {
	refusingMu.Lock()
	refusingReads++
	refusingMu.Unlock()
	return &config.ConfigBackendUnavailable{Message: "this test's backend refuses every read"}
}

func (b refusingBackend) ReadUserDoc(context.Context, string) (any, error) { return nil, b.refuse() }
func (b refusingBackend) ReadUserDocReadonly(context.Context, string) (any, error) {
	return nil, b.refuse()
}
func (b refusingBackend) Version(context.Context, string) ([]any, error) { return nil, b.refuse() }
func (b refusingBackend) Exists(context.Context, string) (bool, error)   { return false, b.refuse() }
func (b refusingBackend) ProbeReadable(context.Context, string) error    { return b.refuse() }
func (b refusingBackend) Locked(context.Context, string, string) (string, bool, error) {
	return "", false, b.refuse()
}

func writeNousAuth(t *testing.T, p *bootPlane, token string, offset time.Duration) {
	t.Helper()
	auth := map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"nous": map[string]any{
		"access_token": token, "refresh_token": "nous-refresh-1", "client_id": "icebath-cli-vps", "portal_base_url": p.url,
		"expires_at": time.Now().Add(offset).UTC().Format("2006-01-02T15:04:05+00:00")}}}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(p.home, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Resolving the Nous token of the cloud credential path needs no config, under a backend refusing every read
func TestResolvingTheNousTokenReadsNoConfig(t *testing.T) {
	p := setupBootPlane(t, false)
	dir := filepath.Join(p.home, "plugins", "model-providers", "probe")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: probe\nkind: model-provider\nversion: 0.0.1\n"), 0o644)
	writeNousAuth(t, p, planeNousToken, time.Hour)
	refusingOnce.Do(func() {
		config.RegisterConfigBackend(refusingKind, func() config.ConfigBackend { return refusingBackend{} })
	})
	t.Setenv(config.BackendEnv, refusingKind)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	refusingMu.Lock()
	refusingReads = 0
	refusingMu.Unlock()
	token, err := remoteConfigNousToken(context.Background(), p.home)
	refusingMu.Lock()
	reads := refusingReads
	refusingMu.Unlock()
	if err != nil || token != planeNousToken || reads != 0 {
		t.Fatalf("token %q %v reads %d", token, err, reads)
	}
}

// Resolving the Nous credential on another goroutine while a profile fetch is in flight never waits for it
func TestColdAuthOnAnotherGoroutineNeverWaitsForTheFetch(t *testing.T) {
	p := setupBootPlane(t, true)
	p.upper = map[string]any{"display": map[string]any{"skin": "ares"}}
	inFetch, resolved := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var waited bool
	p.onGet = func() {
		once.Do(func() {
			close(inFetch)
			select {
			case <-resolved:
				waited = true
			case <-time.After(20 * time.Second):
			}
		})
	}
	go func() {
		<-inFetch
		_, _ = remoteConfigNousToken(context.Background(), p.home)
		close(resolved)
	}()
	if _, err := remote.GetRemoteBackend().Version(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	if !waited {
		t.Fatal("the cold auth resolution waited for the fetch in flight")
	}
	tree, err := config.LoadContext(context.Background(), p.home, os.LookupEnv)
	if err != nil || tree.Get("display.skin") != "ares" {
		t.Fatalf("load %v %v", err, tree.Get("display.skin"))
	}
}

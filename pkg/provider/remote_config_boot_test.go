package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// Remote deployment env pointing at the plane, the IdP client credentials only when idp is set

// Temp profile home served by a fresh remote backend with the deployment in the process env
func setupBootPlane(t *testing.T) *configplane.Plane {
	restoreProcessEnv(t)
	p := configplane.New(t, remoteplane.Rules())
	p.Home = filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(p.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", p.Home)
	for _, name := range append(remote.PlaneCredentialEnvNames, config.BackendEnv) {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	for k, v := range remoteplane.Env(p) {
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

func loadDotenv(p *configplane.Plane) error {
	_, err := LoadIcebathDotenv(context.Background(), DotenvLoadOptions{Home: p.Home})
	return err
}

// The dotenv load boots the profile fetch once per process per profile
func TestBootFetchRunsInsideLoadIcebathDotenv(t *testing.T) {
	p := setupBootPlane(t)
	if err := loadDotenv(p); err != nil || len(p.Gets()) != 1 {
		t.Fatalf("first load %v gets %d", err, len(p.Gets()))
	}
	if err := loadDotenv(p); err != nil || len(p.Gets()) != 1 {
		t.Fatalf("second load %v gets %d", err, len(p.Gets()))
	}
}

// An outage through the dotenv load retries twice and then refuses to start
func TestRemoteDotenvBootRetriesThenExitsOnOutage(t *testing.T) {
	p := setupBootPlane(t)
	p.FailStatus = 503
	err := loadDotenv(p)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "does not start") || len(p.Gets()) != 3 {
		t.Fatalf("load %v gets %d", err, len(p.Gets()))
	}
}

// A plane refusal through the dotenv load is not retried
func TestRemoteDotenvBootDoesNotRetryARefusal(t *testing.T) {
	p := setupBootPlane(t)
	t.Setenv(remote.InstanceEnv, "someone-else")
	err := loadDotenv(p)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "config_agent_unknown") || len(p.Gets()) != 1 {
		t.Fatalf("load %v gets %d", err, len(p.Gets()))
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
	setupBootPlane(t)
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

// A cloud agent first remote read with the auth.json credential completes, refreshing an expired token once
func TestCloudAgentFirstRemoteReadCompletesCold(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout string
	}{{"no-plugin", ""}, {"home-provider-plugin", "model-providers"}, {"flat-provider-plugin", "flat"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := setupBootPlane(t)
			t.Setenv("HOME", filepath.Dir(p.Home))
			if tc.layout != "" {
				dir := filepath.Join(p.Home, "plugins", "probe")
				if tc.layout == "model-providers" {
					dir = filepath.Join(p.Home, "plugins", "model-providers", "probe")
				}
				os.MkdirAll(dir, 0o755)
				os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: probe\nkind: model-provider\nversion: 0.0.1\n"), 0o644)
			}
			p.Upper = map[string]any{"display": map[string]any{"skin": "ares"}}
			var skin any
			var err error
			withinBound(t, 60*time.Second, func() {
				var tree config.Tree
				tree, err = config.LoadContext(context.Background(), p.Home, os.LookupEnv)
				skin = tree.Get("display.skin")
			})
			if err != nil || skin != "ares" {
				t.Fatalf("load %v skin %v", err, skin)
			}
			gets := p.Gets()
			for _, g := range gets {
				if g.Auth != "Bearer "+configplane.Token {
					t.Fatalf("auth %q", g.Auth)
				}
			}
			if len(gets) == 0 {
				t.Fatal("no plane read")
			}
			if _, err := os.Stat(filepath.Join(p.Home, "config.yaml")); err == nil {
				t.Fatal("config.yaml written")
			}
		})
	}
}

// A concurrent first reader waits for the deployment bootstrap instead of reading the local config.yaml
func TestConcurrentFirstReaderWaitsForTheBootstrap(t *testing.T) {
	p := setupBootPlane(t)
	managed := filepath.Join(t.TempDir(), "managed")
	os.MkdirAll(managed, 0o755)
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	var lines strings.Builder
	for k, v := range remoteplane.Env(p) {
		fmt.Fprintf(&lines, "%s=%s\n", k, v)
		os.Unsetenv(k)
	}
	os.WriteFile(filepath.Join(p.Home, ".env"), []byte(lines.String()), 0o600)
	os.WriteFile(filepath.Join(p.Home, "config.yaml"), []byte("display:\n  personality: forbidden-local\n"), 0o644)
	p.Profile("default").Values = map[string]any{"display": map[string]any{"personality": "remote"}}
	p.Profile("default").Version = 1
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
		doc, err := config.ReadConfigDoc(context.Background(), filepath.Join(p.Home, "config.yaml"))
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

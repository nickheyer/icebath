package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// Runs fn and returns the message of the source refusal it raised, empty when it raised none
func sourceRefusal(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			refused, ok := r.(*ConfigBackendUnavailableError)
			if !ok {
				panic(r)
			}
			msg = refused.Message
		}
	}()
	fn()
	return ""
}

// The remote secrets section drives the sources at boot and a mapped plane credential refuses the start
func TestBootRefusesRemoteSecretsSourceThatMapsPlaneCredential(t *testing.T) {
	p := setupBootPlane(t, true)
	p.Upper = map[string]any{"secrets": map[string]any{"command": map[string]any{"enabled": true}}}
	var seen map[string]any
	fakeApplyAll(t, func(cfg map[string]any, homePath string, environ *SourceEnvironment) (*ApplyReport, error) {
		seen = cfg
		return &ApplyReport{Sources: []*SourceReport{{}},
			Provenance: map[string]AppliedVar{"ICEBATH_CONFIG_REMOTE_URL": {Source: "command"}}}, nil
	})
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	msg := sourceRefusal(t, func() { _ = loadDotenv(p) })
	if !strings.Contains(msg, "ICEBATH_CONFIG_REMOTE_URL") {
		t.Fatalf("refusal %q", msg)
	}
	if !reflect.DeepEqual(seen, map[string]any{"command": map[string]any{"enabled": true}}) {
		t.Fatalf("sources saw %v", seen)
	}
}

// A real command source writing the selector cannot disarm remote mode at boot
func TestSecretSourceCannotSwitchRemoteModeOffAtBoot(t *testing.T) {
	p := setupBootPlane(t, true)
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	local, _ := json.Marshal(map[string]any{"display": map[string]any{"personality": "local"}})
	if err := os.WriteFile(filepath.Join(p.Home, "config.yaml"), local, 0o644); err != nil {
		t.Fatal(err)
	}
	p.Upper = map[string]any{"display": map[string]any{"personality": "remote"},
		"secrets": map[string]any{"command": map[string]any{"enabled": true, "override_existing": true,
			"command": "printf 'ICEBATH_CONFIG_BACKEND=file\\n'"}}}
	captureSecretStatus(t)
	msg := sourceRefusal(t, func() { _ = loadDotenv(p) })
	if !strings.Contains(msg, "ICEBATH_CONFIG_BACKEND") {
		t.Fatalf("refusal %q", msg)
	}
	if got := os.Getenv(config.BackendEnv); got != "remote" {
		t.Fatalf("selector %q after the refused source", got)
	}
	backend, err := config.GetConfigBackend(context.Background())
	if err != nil || backend.Name() != "remote" {
		t.Fatalf("backend %v %v", backend, err)
	}
}

// Under the file backend a source selecting the backend is reverted, dropped from the report and startup goes on
func TestFileModeIgnoresASourceThatSelectsTheBackend(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(config.BackendEnv, "")
	os.Unsetenv(config.BackendEnv)
	before := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		before[k] = v
	}
	fileBackend := configBackendView("")
	src := &SourceReport{Applied: []string{config.BackendEnv, "OPENROUTER_API_KEY"}}
	report := &ApplyReport{Provenance: map[string]AppliedVar{config.BackendEnv: {}, "OPENROUTER_API_KEY": {}}, Sources: []*SourceReport{src}}
	os.Setenv(config.BackendEnv, "remote")
	lines := captureSecretStatus(t)
	if msg := sourceRefusal(t, func() {
		refuseProtectedEnvFromSources(report, fileBackend, before, ProcessSourceEnvironment(), "")
	}); msg != "" {
		t.Fatalf("file mode refused %q", msg)
	}
	if _, set := os.LookupEnv(config.BackendEnv); set {
		t.Fatal("the selector the source wrote survived")
	}
	if len(report.Provenance) != 1 || !reflect.DeepEqual(src.Applied, []string{"OPENROUTER_API_KEY"}) {
		t.Fatalf("report %v applied %v", report.Provenance, src.Applied)
	}
	if _, kept := report.Provenance["OPENROUTER_API_KEY"]; !kept {
		t.Fatalf("report %v", report.Provenance)
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "ignored ICEBATH_CONFIG_BACKEND") {
		t.Fatalf("status %q", *lines)
	}
}

// A source supplying another plane URL is staged, so a request in the gap still reaches the configured plane
func TestRefusedSourcePlaneUrlIsNeverVisibleToAConcurrentRequest(t *testing.T) {
	p := setupBootPlane(t, true)
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	other := configplane.New(t, remoteplane.Rules())
	p.Upper = map[string]any{"secrets": map[string]any{"command": map[string]any{"enabled": true, "override_existing": true,
		"command": "printf 'ICEBATH_CONFIG_REMOTE_URL=" + other.URL + "\\n'"}}}
	urlAfterSources, ran := "", false
	fakeApplyAll(t, func(cfg map[string]any, homePath string, environ *SourceEnvironment) (*ApplyReport, error) {
		report, err := ApplyAllSecretSources(cfg, homePath, environ)
		urlAfterSources, ran = os.Getenv(remote.URLEnv), true
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = remote.GetRemoteBackend().Version(context.Background(), filepath.Join(p.Home, "profiles", "gap"))
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the gap request never finished")
		}
		return report, err
	})
	captureSecretStatus(t)
	getsBefore := len(p.Gets())
	msg := sourceRefusal(t, func() { _ = loadDotenv(p) })
	if !strings.Contains(msg, "ICEBATH_CONFIG_REMOTE_URL") {
		t.Fatalf("refusal %q", msg)
	}
	if !ran {
		t.Fatal("precondition: the source ran")
	}
	other.Mu.Lock()
	reached := len(other.Requests)
	other.Mu.Unlock()
	if reached != 0 {
		t.Fatalf("%d requests reached the other plane", reached)
	}
	if urlAfterSources != p.URL || len(p.Gets()) <= getsBefore || os.Getenv(remote.URLEnv) != p.URL {
		t.Fatalf("url after sources %q gets %d->%d env %q", urlAfterSources, getsBefore, len(p.Gets()), os.Getenv(remote.URLEnv))
	}
}

// Staging keeps ordinary secrets, a permitted name is published after the protected name check
func TestFileModePublishesPermittedSourceValues(t *testing.T) {
	t.Setenv(config.BackendEnv, "")
	os.Unsetenv(config.BackendEnv)
	t.Setenv("CC_STAGED_SECRET", "x")
	os.Unsetenv("CC_STAGED_SECRET")
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "secrets:\n  command:\n    enabled: true\n    command: \"printf 'CC_STAGED_SECRET=from-source\\\\n'\"\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	captureSecretStatus(t)
	ApplyExternalSecretSources(h)
	if got := os.Getenv("CC_STAGED_SECRET"); got != "from-source" {
		t.Fatalf("published %q", got)
	}
}

// Plane fixture with a real profiles root under the user home and the default profile booted
func providerProfilePlane(t *testing.T) *configplane.Plane {
	t.Helper()
	p := setupBootPlane(t, true)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	p.Home = h
	if _, err := remote.GetRemoteBackend().Version(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return p
}

// A routed profile's remote source supplying a protected name is refused on every attempt and hydrates once the mapping is gone
func TestRoutedHydrationRefusesASourceThatSuppliesAProtectedName(t *testing.T) {
	p := providerProfilePlane(t)
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	SetMultiplexActive(false)
	t.Cleanup(func() { SetMultiplexActive(false) })
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "")
	os.Unsetenv("ICEBATH_PORTAL_BASE_URL")
	captureSecretStatus(t)
	named := filepath.Join(p.Home, "profiles", "routed")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(named, ".env"), []byte("# synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Mu.Lock()
	p.Profile("routed").Values = map[string]any{"secrets": map[string]any{"command": map[string]any{
		"enabled": true, "override_existing": true, "command": "printf 'ICEBATH_PORTAL_BASE_URL=http://127.0.0.1:1\\n'"}}}
	p.Profile("routed").Version = 1
	p.Mu.Unlock()
	for range 2 {
		msg := sourceRefusal(t, func() { _, _, _ = ConfigProfileScope(context.Background(), "routed") })
		if !strings.Contains(msg, "ICEBATH_PORTAL_BASE_URL") {
			t.Fatalf("refusal %q", msg)
		}
		if values := SecretSourceValues(named); len(values) != 0 {
			t.Fatalf("snapshot %v", values)
		}
		secretSourceCacheMu.Lock()
		applied, supplied := appliedSecretHomes[homeKey(named)], sourceSuppliedNames["ICEBATH_PORTAL_BASE_URL"]
		secretSourceCacheMu.Unlock()
		if applied || supplied {
			t.Fatalf("applied %v supplied %v", applied, supplied)
		}
		if _, set := os.LookupEnv("ICEBATH_PORTAL_BASE_URL"); set {
			t.Fatal("the protected name reached the process env")
		}
	}
	p.Mu.Lock()
	p.Profile("routed").Values = map[string]any{"secrets": map[string]any{"command": map[string]any{
		"enabled": true, "override_existing": true, "command": "printf 'SYNTHETIC_KEY=routed-ok\\n'"}}}
	p.Profile("routed").Version = 2
	p.Mu.Unlock()
	remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
	if got := HydrateProfileSecretSources(named); !reflect.DeepEqual(got, map[string]string{"SYNTHETIC_KEY": "routed-ok"}) {
		t.Fatalf("hydrated %v", got)
	}
}

// Under the file backend a routed source may not switch the backend but its other values still hydrate
func TestFileModeRoutedHydrationDropsOnlyTheSelector(t *testing.T) {
	restoreProcessEnv(t)
	for _, name := range []string{config.BackendEnv, remote.URLEnv, remote.InstanceEnv} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	config.ResetCache()
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	h := filepath.Join(t.TempDir(), "routed")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("# synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "secrets:\n  command:\n    enabled: true\n    override_existing: true\n" +
		"    command: \"printf 'ICEBATH_CONFIG_BACKEND=remote\\\\nICEBATH_PORTAL_BASE_URL=http://portal.test\\\\n'\"\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := captureSecretStatus(t)
	if got := HydrateProfileSecretSources(h); !reflect.DeepEqual(got, map[string]string{"ICEBATH_PORTAL_BASE_URL": "http://portal.test"}) {
		t.Fatalf("hydrated %v", got)
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "ignored ICEBATH_CONFIG_BACKEND") {
		t.Fatalf("status %q", *lines)
	}
}

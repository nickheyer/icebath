package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type recordedOp struct{ op, home string }

type recordingBackend struct {
	FileBackend
	mu    sync.Mutex
	calls []recordedOp
}

func (b *recordingBackend) Name() string { return "recording" }

func (b *recordingBackend) rec(op, h string) {
	b.mu.Lock()
	b.calls = append(b.calls, recordedOp{op, filepath.Clean(h)})
	b.mu.Unlock()
}

func (b *recordingBackend) ReadUserDoc(ctx context.Context, h string) (any, error) {
	b.rec("read", h)
	return b.FileBackend.ReadUserDoc(ctx, h)
}

func (b *recordingBackend) ReadUserDocReadonly(ctx context.Context, h string) (any, error) {
	b.rec("read", h)
	return b.FileBackend.ReadUserDocReadonly(ctx, h)
}

func (b *recordingBackend) Version(ctx context.Context, h string) ([]any, error) {
	b.rec("version", h)
	return b.FileBackend.Version(ctx, h)
}

func (b *recordingBackend) Exists(ctx context.Context, h string) (bool, error) {
	b.rec("exists", h)
	return b.FileBackend.Exists(ctx, h)
}

func (b *recordingBackend) WriteChanges(ctx context.Context, h string, changes Changes) error {
	b.rec("write", h)
	return b.FileBackend.WriteChanges(ctx, h, changes)
}

// Set of operations the backend saw for one home
func (b *recordingBackend) ops(h string) map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]bool{}
	for _, c := range b.calls {
		if c.home == filepath.Clean(h) {
			out[c.op] = true
		}
	}
	return out
}

// Fresh active home with a commented config and every backend operation recorded
func recordingHome(t *testing.T, kind string) (string, *recordingBackend) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# hand-tuned\nmodel:\n  default: my-model\nsecrets:\n  sources: []\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	ResetCache()
	t.Cleanup(ResetCache)
	backend := &recordingBackend{}
	registerTestBackend(t, kind, func() ConfigBackend { return backend })
	t.Setenv(BackendEnv, kind)
	return h, backend
}

func TestPipelinesConsultTheBackendLoadConfig(t *testing.T) {
	h, backend := recordingHome(t, "recording_load")
	tree, err := LoadContext(context.Background(), h, processLookup)
	if err != nil {
		t.Fatal(err)
	}
	if tree["model"].(map[string]any)["default"] != "my-model" {
		t.Fatal(tree["model"])
	}
	if ops := backend.ops(h); !ops["version"] || !ops["read"] {
		t.Fatal(ops)
	}
}

func TestPipelinesConsultTheBackendRawPrimitives(t *testing.T) {
	h, backend := recordingHome(t, "recording_raw")
	ctx := context.Background()
	raw, err := ReadRawConfig(ctx, h)
	if err != nil || raw["model"].(map[string]any)["default"] != "my-model" {
		t.Fatal(raw, err)
	}
	user, err := ReadUserConfigRaw(ctx, filepath.Join(h, "config.yaml"))
	if err != nil || user["model"].(map[string]any)["default"] != "my-model" {
		t.Fatal(user, err)
	}
	if ops := backend.ops(h); !ops["version"] || !ops["read"] {
		t.Fatal(ops)
	}
}

func TestPipelinesConsultTheBackendEffectiveLoader(t *testing.T) {
	h, backend := recordingHome(t, "recording_effective")
	doc, err := LoadUserEffective(filepath.Join(h, "config.yaml"), processLookup)
	if err != nil || doc["model"].(map[string]any)["default"] != "my-model" {
		t.Fatal(doc, err)
	}
	if ops := backend.ops(h); !ops["version"] || !ops["read"] {
		t.Fatal(ops)
	}
}

func TestPipelineSaveConfigWritesOnceThroughTheBackend(t *testing.T) {
	h, backend := recordingHome(t, "recording_writers")
	ctx := context.Background()
	stamped := fmt.Sprintf("_config_version: %d\n# hand-tuned\nmodel:\n  default: my-model\nsecrets:\n  sources: []\n", latestConfigVersion())
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte(stamped), 0o600)
	tree, err := LoadContext(ctx, h, processLookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Clone(map[string]any(tree)).(map[string]any)
	cfg["model"].(map[string]any)["default"] = "other-model"
	if err := SaveConfig(ctx, h, cfg, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, c := range backend.calls {
		if c.op == "write" && c.home == filepath.Clean(h) {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("writes %d", writes)
	}
	text, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if !strings.Contains(string(text), "# hand-tuned") || !strings.Contains(string(text), "other-model") {
		t.Fatal(string(text))
	}
}

func TestBackupConfigSkipsABackendWithoutFileTooling(t *testing.T) {
	h := backendHome(t)
	registerTestBackend(t, "nofile_backup", func() ConfigBackend { return &fakeKindBackend{name: "nofile"} })
	t.Setenv(BackendEnv, "nofile_backup")
	path := filepath.Join(h, "config.yaml")
	os.WriteFile(path, []byte("a: 1\n"), 0o600)
	if got := mustBackup(t, path, "test", DefaultBackupKeep); got != "" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); !os.IsNotExist(err) {
		t.Fatal("no backups dir for a backend without a config file")
	}
}

// Active home and managed dir with the fixture env the effective loader expands
func effectiveHomes(t *testing.T) (string, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	managed := filepath.Join(t.TempDir(), "managed")
	os.MkdirAll(h, 0o700)
	os.MkdirAll(managed, 0o700)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(BackendEnv, "")
	t.Setenv(EnvManagedDir, managed)
	t.Setenv("FIXTURE_USER_KEY", "user-secret")
	t.Setenv("FIXTURE_MANAGED_URL", "https://managed.example")
	resetEffectiveCaches := func() { ResetCache(); InvalidateManagedCache() }
	resetEffectiveCaches()
	t.Cleanup(resetEffectiveCaches)
	return h, managed
}

const effectiveUserYAML = "model:\n  name: user/model\n  api_key: ${FIXTURE_USER_KEY}\nprovider: custom\ndisplay:\n  skin: user-skin\n"

func TestEffectiveIsUserPlusManagedPlusEnvWithNoDefaults(t *testing.T) {
	h, managed := effectiveHomes(t)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte(effectiveUserYAML), 0o600)
	os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("model:\n  base_url: ${FIXTURE_MANAGED_URL}\ndisplay:\n  skin: managed-skin\n"), 0o600)
	InvalidateManagedCache()
	got, err := LoadUserEffective(filepath.Join(h, "config.yaml"), processLookup)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"model":   map[string]any{"default": "user/model", "provider": "custom", "api_key": "user-secret", "base_url": "https://managed.example"},
		"display": map[string]any{"skin": "managed-skin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

func TestBrokenYAMLServesLastGoodAndFailClosedRaises(t *testing.T) {
	h, _ := effectiveHomes(t)
	path := filepath.Join(h, "config.yaml")
	os.WriteFile(path, []byte(effectiveUserYAML), 0o600)
	good, err := LoadUserEffective(path, processLookup)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("model: [unterminated"), 0o600)
	loadCacheMu.Lock()
	rawCache = map[string]rawEntry{}
	effectiveCache = map[string]effectiveEntry{}
	loadCacheMu.Unlock()
	served, err := LoadUserEffective(path, processLookup)
	if err != nil || !reflect.DeepEqual(served, good) {
		t.Fatalf("served %v %v", served, err)
	}
	if _, err := LoadUserEffectiveFailClosed(path, processLookup); err == nil || IsOSError(err) {
		t.Fatalf("fail closed must return the parse error, got %v", err)
	}
}

func TestGoodBackupIsWrittenOnlyForTheActiveHome(t *testing.T) {
	h, _ := effectiveHomes(t)
	other := filepath.Join(t.TempDir(), "other-profile")
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte(effectiveUserYAML), 0o600)
	os.WriteFile(filepath.Join(other, "config.yaml"), []byte(effectiveUserYAML), 0o600)
	if _, err := LoadUserEffective(filepath.Join(other, "config.yaml"), processLookup); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUserEffective(filepath.Join(h, "config.yaml"), processLookup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "backups")); !os.IsNotExist(err) {
		t.Fatal("another profile read must not create backups")
	}
	if matches, _ := filepath.Glob(filepath.Join(h, "backups", "config", "config.yaml.good.*")); len(matches) == 0 {
		t.Fatal("active home keeps a good copy")
	}
}

// Backup path of a config, failing the test when the backend selection errors
func mustBackup(t *testing.T, path, reason string, keep int) string {
	t.Helper()
	dest, err := BackupConfig(context.Background(), path, reason, keep)
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestBackupCallersPropagateAnUnavailableBackend(t *testing.T) {
	h := backendHome(t)
	path := filepath.Join(h, "config.yaml")
	os.WriteFile(path, []byte("model: [unterminated\n"), 0o600)
	t.Setenv(BackendEnv, "bogus")
	var unavailable *ConfigBackendUnavailable
	if got, err := BackupConfig(context.Background(), path, "test", DefaultBackupKeep); got != "" || !errors.As(err, &unavailable) {
		t.Fatalf("backup %q %v", got, err)
	}
	if got, err := LoadNewestGoodBackup(context.Background(), path); got != nil || !errors.As(err, &unavailable) {
		t.Fatalf("good backup %v %v", got, err)
	}
	quietParseFailures(t)
	if err := WarnConfigParseFailure(context.Background(), path, fmt.Errorf("bad yaml"), "defaults"); !errors.As(err, &unavailable) {
		t.Fatalf("parse warning %v", err)
	}
	if _, err := recoverUserRaw(context.Background(), path, fmt.Errorf("bad yaml")); !errors.As(err, &unavailable) {
		t.Fatalf("recovery %v", err)
	}
	if _, err := lastKnownGoodFallback(context.Background(), path, nil, fmt.Errorf("bad yaml"), processLookup); !errors.As(err, &unavailable) {
		t.Fatalf("last known good %v", err)
	}
	if _, err := dockerBackupExisting(context.Background(), path); !errors.As(err, &unavailable) {
		t.Fatalf("docker backups %v", err)
	}
	if _, err := os.Stat(filepath.Join(h, "backups")); !os.IsNotExist(err) {
		t.Fatal("no copy is written when no backend serves config")
	}
}

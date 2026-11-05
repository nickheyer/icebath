package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var _ SystemExitError = (*ConfigBackendUnavailable)(nil)

// Fresh profile home with no backend selector in the environment
func backendHome(t *testing.T) string {
	t.Helper()
	t.Setenv(BackendEnv, "")
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

// Registers a backend kind for one test and removes it afterwards
func registerTestBackend(t *testing.T, kind string, factory func() ConfigBackend) {
	t.Helper()
	RegisterConfigBackend(kind, factory)
	t.Cleanup(func() {
		backendFactoryMu.Lock()
		delete(backendFactories, kind)
		backendFactoryMu.Unlock()
	})
}

type fakeKindBackend struct {
	FileBackend
	name    string
	tooling bool
	mu      sync.Mutex
	events  []string
}

func (b *fakeKindBackend) Name() string                { return b.name }
func (b *fakeKindBackend) SupportsFileTooling() bool   { return b.tooling }
func (b *fakeKindBackend) BootstrapEnvNames() []string { return []string{"FAKE_P21_PLANE"} }
func (b *fakeKindBackend) PreserveReadBase(context.Context) func() {
	b.mu.Lock()
	b.events = append(b.events, "enter")
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.events = append(b.events, "exit")
		b.mu.Unlock()
	}
}
func (b *fakeKindBackend) DoctorWarnings(_ context.Context, home string) []DoctorWarning {
	return []DoctorWarning{{Message: "fake warning for " + filepath.Base(home), Hint: "(fake hint)", Issue: "fake issue"}}
}

func TestDefaultBackendIsFile(t *testing.T) {
	backendHome(t)
	ctx := context.Background()
	b, err := GetConfigBackend(ctx)
	if err != nil || b.Name() != "file" {
		t.Fatalf("backend %v %v", b, err)
	}
	if ok, err := SupportsFileTooling(ctx); !ok || err != nil {
		t.Fatalf("file tooling %v %v", ok, err)
	}
	t.Setenv(BackendEnv, "  FILE ")
	if b, err := GetConfigBackend(ctx); err != nil || b.Name() != "file" {
		t.Fatalf("padded selector %v %v", b, err)
	}
}

func TestUnknownBackendFailsClosed(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	t.Setenv(BackendEnv, "bogus")
	var unavailable *ConfigBackendUnavailable
	if _, err := GetConfigBackend(ctx); !errors.As(err, &unavailable) {
		t.Fatalf("selection %v", err)
	}
	if _, err := ReadConfigDoc(ctx, filepath.Join(h, "config.yaml")); !errors.As(err, &unavailable) {
		t.Fatalf("read %v", err)
	}
	want := "ICEBATH_CONFIG_BACKEND='bogus' is not a config backend this build provides (expected 'file' or 'remote')."
	if unavailable.SystemExitCode() != want {
		t.Fatalf("message %q", unavailable.SystemExitCode())
	}
}

func TestExplicitNonConfigFileNeverReachesTheBackend(t *testing.T) {
	h := backendHome(t)
	t.Setenv(BackendEnv, "bogus")
	other := filepath.Join(h, "import-source.yaml")
	if err := os.WriteFile(other, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := ReadConfigDoc(context.Background(), other)
	if err != nil || !reflect.DeepEqual(doc, map[string]any{"a": 1}) {
		t.Fatalf("doc %v %v", doc, err)
	}
	b, gotHome := LocalFileLayer(filepath.Join(h, "config.yaml"))
	if b.Name() != "file" || gotHome != h {
		t.Fatalf("local layer %v %s", b.Name(), gotHome)
	}
}

func TestFileBackendMissing(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	if ok, err := ConfigExists(ctx, path); ok || err != nil {
		t.Fatalf("exists %v %v", ok, err)
	}
	if _, err := ConfigVersion(ctx, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version %v", err)
	}
	if _, err := ReadConfigDoc(ctx, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read %v", err)
	}
	if err := ProbeConfigReadable(ctx, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe %v", err)
	}
}

func TestVersionOrNoneAndToken(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	if v, err := ConfigVersionOrNone(ctx, path); v != nil || err != nil {
		t.Fatalf("absent %v %v", v, err)
	}
	if v, err := ConfigCacheVersionOrNone(ctx, path); v != nil || err != nil {
		t.Fatalf("absent cache %v %v", v, err)
	}
	if VersionToken(nil) != 0 {
		t.Fatal("empty token")
	}
	if err := os.WriteFile(path, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	version, _ := ConfigVersionOrNone(ctx, path)
	direct, _ := ConfigVersion(ctx, path)
	if !reflect.DeepEqual(version, direct) {
		t.Fatalf("%v != %v", version, direct)
	}
	if tok := VersionToken(version); tok != float64(version[0].(int64))/1e9 || tok <= 0 {
		t.Fatalf("token %v", tok)
	}
	e1, e2 := VersionToken([]any{"etag-1"}), VersionToken([]any{"etag-2"})
	if e1 != VersionToken([]any{"etag-1"}) || e1 == 0 || e1 == e2 {
		t.Fatalf("opaque tokens %v %v", e1, e2)
	}
}

func TestReadVersionAndBOM(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(path, []byte("\ufeffmodel:\n  default: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": map[string]any{"default": "x"}}
	if ok, _ := ConfigExists(ctx, path); !ok {
		t.Fatal("exists")
	}
	if doc, err := ReadConfigDoc(ctx, path); err != nil || !reflect.DeepEqual(doc, want) {
		t.Fatalf("read %v %v", doc, err)
	}
	if doc, err := ReadConfigDocReadonly(ctx, path); err != nil || !reflect.DeepEqual(doc, want) {
		t.Fatalf("readonly %v %v", doc, err)
	}
	v1, _ := ConfigVersion(ctx, path)
	if err := os.WriteFile(path, []byte("model:\n  default: longer-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v2, _ := ConfigVersion(ctx, path); reflect.DeepEqual(v1, v2) {
		t.Fatal("version unchanged")
	}
	if err := ProbeConfigReadable(ctx, path); err != nil {
		t.Fatal(err)
	}
}

func TestWritesPreserveComments(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(path, []byte("# keep me\nmodel:\n  default: a  # inline\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigKey(ctx, path, "model.default", "b"); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigKey(ctx, path, "display.personality", "kawaii"); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), "# keep me") || !strings.Contains(string(text), "# inline") {
		t.Fatalf("comments lost\n%s", text)
	}
	doc, _ := ReadConfigDoc(ctx, path)
	want := map[string]any{"model": map[string]any{"default": "b"}, "display": map[string]any{"personality": "kawaii"}}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("doc %v", doc)
	}
	if err := WriteConfigDocument(ctx, path, map[string]any{"model": map[string]any{"default": "c"}}, ""); err != nil {
		t.Fatal(err)
	}
	text, _ = os.ReadFile(path)
	if !strings.Contains(string(text), "# keep me") {
		t.Fatalf("comment lost\n%s", text)
	}
	doc, _ = ReadConfigDoc(ctx, path)
	if !reflect.DeepEqual(doc, map[string]any{"model": map[string]any{"default": "c"}}) {
		t.Fatalf("doc %v", doc)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestWriteKeysRemovesNilAndQuotesAmbiguousWords(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	if err := os.WriteFile(path, []byte("a:\n  b: 1\n  c: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigKeys(ctx, path, []KeyValue{{"a.b", nil}, {"a.mode", "off"}, {"x.y", nil}}); err != nil {
		t.Fatal(err)
	}
	doc, _ := ReadConfigDoc(ctx, path)
	if !reflect.DeepEqual(doc, map[string]any{"a": map[string]any{"c": 2, "mode": "off"}}) {
		t.Fatalf("doc %v", doc)
	}
	if err := WriteConfigDocument(ctx, path, map[string]any{"a": map[string]any{"c": 2, "mode": "off", "l": []any{"no", 3}}}, ""); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), `"no"`) {
		t.Fatalf("ambiguous list item unquoted\n%s", text)
	}
	if err := WriteConfigKey(ctx, path, "providers.grok-4.6.context_length", 9); err != nil {
		t.Fatal(err)
	}
	if err := WriteChanges(ctx, path, Changes{Set: []KeyValue{{"providers.grok-4\\.6", map[string]any{"ctx": 1}}}}); err != nil {
		t.Fatal(err)
	}
	doc, _ = ReadConfigDoc(ctx, path)
	if got := AsMap(AsMap(doc)["providers"]); !reflect.DeepEqual(got, map[string]any{"grok-4": map[string]any{"6": map[string]any{"context_length": 9}}, "grok-4.6": map[string]any{"ctx": 1}}) {
		t.Fatalf("providers %v", got)
	}
}

func TestDocumentSaveAppendsExtraContentOnlyOnCreate(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	if err := WriteConfigDocument(ctx, path, map[string]any{"a": 1}, "# example\n"); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigDocument(ctx, path, map[string]any{"a": 2}, "# example\n"); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if strings.Count(string(text), "# example") != 1 || !strings.Contains(string(text), "a: 2") {
		t.Fatalf("file\n%s", text)
	}
}

func TestDocumentSaveRefusesAnUnparseableOrNonMappingFile(t *testing.T) {
	h := backendHome(t)
	quietParseFailures(t)
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	path := filepath.Join(h, "config.yaml")
	for body, reason := range map[string]string{
		"model: [unclosed\n": "has a formatting error",
		"- a\n- b\n":         "must start with settings names, but its top level is a list",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := WriteConfigDocument(ctx, path, map[string]any{"a": 1}, "")
		if err == nil || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "so this change was not saved") {
			t.Fatalf("refusal %v", err)
		}
		if text, _ := os.ReadFile(path); string(text) != body {
			t.Fatalf("file replaced %q", text)
		}
	}
	if got, err := RequireReadableConfigBeforeWrite(ctx, filepath.Join(h, "missing", "config.yaml")); err != nil || len(got) != 0 {
		t.Fatalf("missing %v %v", got, err)
	}
}

func TestBackendWithoutAFileRefusesFileTooling(t *testing.T) {
	backendHome(t)
	ctx := context.Background()
	registerTestBackend(t, "nofile_cfg3", func() ConfigBackend { return &fakeKindBackend{name: "remote"} })
	t.Setenv(BackendEnv, "nofile_cfg3")
	if ok, err := SupportsFileTooling(ctx); ok || err != nil {
		t.Fatalf("tooling %v %v", ok, err)
	}
	err := RequireFileTooling(ctx, "config backup")
	var unavailable *ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || err.Error() != "config backup copies the local config.yaml, and the 'remote' config backend has none. It is not available with this backend." {
		t.Fatalf("refusal %v", err)
	}
	t.Setenv(BackendEnv, "")
	if err := RequireFileTooling(ctx, "config backup"); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownKindIsServedThroughProtocolMethodsOnly(t *testing.T) {
	h := backendHome(t)
	ctx := context.Background()
	fake := &fakeKindBackend{name: "fakekind_p21", tooling: true}
	registerTestBackend(t, "fakekind_p21", func() ConfigBackend { return fake })
	bootstrapped.Store(false)
	var asked []string
	RegisterConfigBootstrapEnv(func(ctx context.Context, names func(string) []string, home, project string) error {
		asked = append([]string{BackendEnv}, names("fakekind_p21")...)
		if _, err := GetConfigBackend(ctx); err != nil {
			return err
		}
		os.Setenv(BackendEnv, "fakekind_p21")
		os.Setenv("FAKE_P21_PLANE", "https://plane.test")
		return nil
	})
	t.Cleanup(func() { RegisterConfigBootstrapEnv(nil); os.Unsetenv("FAKE_P21_PLANE") })
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := GetConfigBackend(ctx)
	if err != nil || backend != ConfigBackend(fake) || os.Getenv("FAKE_P21_PLANE") != "https://plane.test" {
		t.Fatalf("backend %v %v", backend, err)
	}
	if !reflect.DeepEqual(asked, []string{BackendEnv, "FAKE_P21_PLANE"}) {
		t.Fatalf("bootstrap names %v", asked)
	}
	doc, err := KeepingReadBase(ctx, func() (any, error) { return ReadConfigDoc(ctx, filepath.Join(h, "config.yaml")) })
	if err != nil || !reflect.DeepEqual(doc, map[string]any{"a": 1}) || !reflect.DeepEqual(fake.events, []string{"enter", "exit"}) {
		t.Fatalf("keeping read base %v %v %v", doc, err, fake.events)
	}
	if w := backend.DoctorWarnings(ctx, h); len(w) != 1 || w[0].Issue != "fake issue" || w[0].Message != "fake warning for home" {
		t.Fatalf("warnings %v", w)
	}
	asked = nil
	if err := BootstrapDeployment(ctx, h, ""); err != nil || len(asked) != 2 {
		t.Fatalf("explicit bootstrap reruns %v %v", asked, err)
	}
}

func TestFileBackendProtocolDefaultsAreNoOps(t *testing.T) {
	ctx := context.Background()
	fb := FileBackend{}
	if fb.BootstrapEnvNames() != nil || fb.DoctorWarnings(ctx, t.TempDir()) != nil || fb.ProtectedEnvNames() != nil || fb.DeploymentEnvNames() != nil {
		t.Fatal("file backend names")
	}
	fb.PreserveReadBase(ctx)()
	if lock, ok, err := fb.Locked(ctx, "/h", "model"); lock != "" || ok || err != nil {
		t.Fatal("locked")
	}
	if err := fb.ApplyInMemory(ctx, "/h", nil); err == nil || err.Error() != "the file backend persists migrations to config.yaml" {
		t.Fatalf("apply in memory %v", err)
	}
	if fb.Destination(ctx, "/h") != filepath.Join("/h", "config.yaml") || !fb.HonorsManagedConfig() || fb.Boot(ctx, "/h") != nil {
		t.Fatal("destination")
	}
	if names, err := DeploymentEnvNames(ctx); names != nil || err != nil {
		t.Fatalf("deployment names %v %v", names, err)
	}
}

func TestKindIsValidatedBeforeAnyLookup(t *testing.T) {
	bootstrapped.Store(true)
	for _, kind := range []string{"../etc", "remote.backend", "remote x", "os;id", "_remote", "9lives",
		strings.Repeat("x", 65), "remote/backend", "plugins.config_backends.remote", "nosuchbackend"} {
		t.Setenv(BackendEnv, kind)
		var unavailable *ConfigBackendUnavailable
		if _, err := GetConfigBackend(context.Background()); !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "is not a config backend") {
			t.Fatalf("%q: %v", kind, err)
		}
	}
}

func TestWriteErrorFamily(t *testing.T) {
	locked := NewConfigLockedError("model.default", "org", "")
	var base *ConfigWriteError
	if !errors.As(locked, &base) || base.Code != "config_key_locked" || locked.Error() != "model.default is locked by the org level" {
		t.Fatalf("locked %v", locked)
	}
	value := NewConfigValueError("secret literal", "")
	if !errors.As(value, &base) || base.Code != "config_value_invalid" {
		t.Fatalf("value %v", value)
	}
	if NewConfigWriteError("x", "").Code != "config_write_failed" {
		t.Fatal("write code")
	}
}

func TestContextScopedSavePrefixesAndPins(t *testing.T) {
	ctx := context.Background()
	if RefusedLockedOmissionPrefixes(ctx) != nil || PinnedKeys(ctx) != nil {
		t.Fatal("defaults")
	}
	outer := RefusingLockedOmissions(ctx, "model")
	inner := RefusingLockedOmissions(outer, "agent", "tools")
	if !reflect.DeepEqual(RefusedLockedOmissionPrefixes(inner), []string{"model", "agent", "tools"}) ||
		!reflect.DeepEqual(RefusedLockedOmissionPrefixes(outer), []string{"model"}) {
		t.Fatal("prefixes")
	}
	pinned := PinningKeys(PinningKeys(ctx, "a.b"), "c")
	if !reflect.DeepEqual(PinnedKeys(pinned), []string{"a.b", "c"}) {
		t.Fatal("pins")
	}
}

package memory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

type choiceProvider struct {
	regProvider
	saved int
}

func (*choiceProvider) ConfigSchema() []map[string]any {
	return []map[string]any{{"key": "mode", "description": "Mode", "default": "one", "choices": []any{"one", "two"}}}
}

func (p *choiceProvider) SaveConfig(map[string]any, string) error { p.saved++; return nil }

func setupFor(t *testing.T) (*MemorySetup, *bytes.Buffer) {
	t.Helper()
	env := testEnv(t)
	t.Setenv("ICEBATH_HOME", env.Home)
	config.ResetCache()
	out := &bytes.Buffer{}
	return NewMemorySetup(env, &cliui.Console{In: strings.NewReader(""), Out: out}, out), out
}

func TestCmdSetupGenericChoiceCancelWritesNothing(t *testing.T) {
	m, _ := setupFor(t)
	p := &choiceProvider{}
	selections := []int{0, setupCancelled}
	var installed []string
	saves := 0
	m.availableProviders = func(context.Context) []setupProvider { return []setupProvider{{"fake", "local", p}} }
	m.cursesSelect = func(context.Context, string, [][2]string, int, int) (int, error) {
		s := selections[0]
		selections = selections[1:]
		return s, nil
	}
	m.installDependencies = func(_ context.Context, name string) error { installed = append(installed, name); return nil }
	m.loadConfig = func(context.Context) (map[string]any, error) { return map[string]any{"memory": map[string]any{}}, nil }
	m.saveConfig = func(context.Context, map[string]any) error { saves++; return nil }
	if err := m.CmdSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0] != "fake" || saves != 0 || p.saved != 0 {
		t.Fatal(installed, saves, p.saved)
	}
	if _, err := os.Stat(filepath.Join(m.Env.Home, ".env")); !os.IsNotExist(err) {
		t.Fatal(".env written")
	}
}

func TestInstallDependenciesPreparesDeclaredExtraEvenIfImportable(t *testing.T) {
	m, _ := setupFor(t)
	dir := filepath.Join(m.Env.Home, "plugins", "mem0")
	os.MkdirAll(dir, 0o755)
	RegisterProvider(ProviderRegistration{Name: "mem0", Source: SourceUser, Home: m.Env.Home, Dir: dir, New: func(context.Context, *tools.Env, LoadOptions) (Provider, error) { return &choiceProvider{}, nil }})
	t.Cleanup(func() { UnregisterProviders(SourceUser, m.Env.Home) })
	var synced []pm.SyncVenvOptions
	prevRead, prevSync, prevRun := setupReadManifest, setupSyncVenv, setupRunningFromSelected
	t.Cleanup(func() { setupReadManifest, setupSyncVenv, setupRunningFromSelected = prevRead, prevSync, prevRun })
	setupReadManifest = func(string) (map[string]any, error) { return map[string]any{"extra": "mem0"}, nil }
	setupSyncVenv = func(_ context.Context, o pm.SyncVenvOptions) error { synced = append(synced, o); return nil }
	setupRunningFromSelected = func(context.Context, string) (bool, string, error) { return true, "", nil }
	if err := m.installDeps(context.Background(), "mem0"); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || len(synced[0].Extras) != 1 || synced[0].Extras[0] != "mem0" || !synced[0].Explicit {
		t.Fatalf("%+v", synced)
	}
}

func TestCmdStatusMemoryToolGate(t *testing.T) {
	for _, c := range []struct {
		memory, user bool
		tool, inj    string
	}{{false, false, "disabled", "disabled"}, {true, false, "enabled", "enabled"}} {
		m, out := setupFor(t)
		for k, v := range map[string]any{"memory.memory_enabled": c.memory, "memory.user_profile_enabled": c.user} {
			if err := config.SetKey(context.Background(), m.Env.Home, k, v); err != nil {
				t.Fatal(err)
			}
		}
		config.ResetCache()
		m.availableProviders = func(context.Context) []setupProvider { return nil }
		if err := m.CmdStatus(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if !regexp.MustCompile(`Memory tool:\s+`+c.tool).MatchString(got) || !regexp.MustCompile(`Memory injection:\s+`+c.inj).MatchString(got) || !regexp.MustCompile(`User profile:\s+disabled`).MatchString(got) {
			t.Fatal(got)
		}
	}
}

func envFileKeys(t *testing.T, h string) map[string]string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(h, ".env"))
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestWriteEnvVarsDenylistAndValidation(t *testing.T) {
	for _, denied := range []string{"LD_PRELOAD", "PYTHONPATH", "ICEBATH_HOME", "PATH"} {
		m, out := setupFor(t)
		if err := m.writeEnvVars(context.Background(), []envWrite{{denied, "x"}, {"OPENROUTER_API_KEY", "sk-or-test-456"}}, ""); err != nil {
			t.Fatal(err)
		}
		keys := envFileKeys(t, m.Env.Home)
		if _, bad := keys[denied]; bad || !strings.Contains(out.String(), "Skipping "+denied) || keys["OPENROUTER_API_KEY"] != "sk-or-test-456" {
			t.Fatal(denied, keys, out.String())
		}
	}
	m, out := setupFor(t)
	if err := m.writeEnvVars(context.Background(), []envWrite{{"FOO BAR", "x"}, {"ICEBATH_LANGFUSE_PUBLIC_KEY", "pk-lf-789"}, {"MEM0_API_KEY", "key1\nEVIL=injected"}}, ""); err != nil {
		t.Fatal(err)
	}
	keys := envFileKeys(t, m.Env.Home)
	if _, bad := keys["FOO BAR"]; bad || keys["FOO"] != "" || !strings.Contains(out.String(), "Skipping FOO BAR") {
		t.Fatal(keys, out.String())
	}
	if keys["ICEBATH_LANGFUSE_PUBLIC_KEY"] != "pk-lf-789" || keys["MEM0_API_KEY"] != "key1EVIL=injected" || keys["EVIL"] != "" {
		t.Fatal(keys)
	}
	if st, err := os.Stat(filepath.Join(m.Env.Home, ".env")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(st, err)
	}
	other := filepath.Join(t.TempDir(), "profile")
	os.MkdirAll(other, 0o755)
	if err := m.writeEnvVars(context.Background(), []envWrite{{"MEM0_API_KEY", "m0-test-key-abc"}}, other); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(other, ".env")); !strings.Contains(string(raw), "MEM0_API_KEY=m0-test-key-abc\n") {
		t.Fatal(string(raw))
	}
}

func TestMissingCatalogProviderRecoveryNamesTheProfileInstallCommand(t *testing.T) {
	h := filepath.Join(t.TempDir(), "profiles", "work")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("memory:\n  provider: honcho\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	out := &bytes.Buffer{}
	env := &tools.Env{Home: h, Values: &sync.Map{}}
	m := NewMemorySetup(env, &cliui.Console{In: strings.NewReader(""), Out: out}, out)
	ctx := context.Background()
	if err := m.CmdStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.CmdSetupProvider(ctx, "honcho"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "icebath -p work plugins install honcho"); got != 2 {
		t.Fatalf("install command named %d times in\n%s", got, out.String())
	}
}

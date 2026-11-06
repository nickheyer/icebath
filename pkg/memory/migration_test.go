package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Swaps the migration seams for one test, presence read from <home>/plugins/<name>
func withMigrationSteps(t *testing.T, catalog func(string) (string, bool)) {
	t.Helper()
	saved, savedLeft := migrationSteps, leftCoreMemoryProviders
	t.Cleanup(func() {
		migrationSteps, leftCoreMemoryProviders = saved, savedLeft
		migrationAttemptedMu.Lock()
		migrationAttempted = map[[2]string]bool{}
		migrationAttemptedMu.Unlock()
	})
	migrationSteps.providerPresent = func(_ context.Context, name, h string) bool {
		st, err := os.Stat(filepath.Join(h, "plugins", name))
		return err == nil && st.IsDir()
	}
	migrationSteps.catalogSource = catalog
	migrationSteps.interactive = func() bool { return false }
}

func writeMigrationHome(t *testing.T, h, body string) {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func catalogAll(name string) (string, bool) { return name, true }

func TestMigrateHomeInstallsTheMissingProviderAndKeepsConfig(t *testing.T) {
	h := t.TempDir()
	writeMigrationHome(t, h, "memory:\n  provider: honcho\n  honcho:\n    workspace: keep-me\n")
	withMigrationSteps(t, catalogAll)
	var calls, said []string
	install := func(name string) (map[string]any, error) {
		calls = append(calls, name)
		if err := os.MkdirAll(filepath.Join(h, "plugins", name), 0o755); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"ok": true}, nil
	}
	say := func(s string) { said = append(said, s) }
	ctx := context.Background()
	if got, err := MigrateMemoryHome(ctx, h, install, say); err != nil || got != "honcho" {
		t.Fatalf("got %q %v", got, err)
	}
	if strings.Contains(said[0], "memory.honcho") || !strings.Contains(said[0], "icebath memory status") {
		t.Fatalf("said %q", said[0])
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if !strings.Contains(string(raw), "workspace: keep-me") {
		t.Fatal("config rewritten")
	}
	if got, err := MigrateMemoryHome(ctx, h, install, say); err != nil || got != "" || len(calls) != 1 {
		t.Fatalf("second run %q %v %v", got, err, calls)
	}
}

func TestMigratePresenceIsCheckedInTheHomeBeingMigrated(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	writeMigrationHome(t, a, "memory:\n  provider: twin\n")
	writeMigrationHome(t, b, "memory:\n  provider: twin\n")
	withMigrationSteps(t, catalogAll)
	migrationSteps.providerPresent = MemoryProviderPresent
	registerUserFlow(t, "twin", b, nil)
	ctx := home.WithOverride(context.Background(), a)
	var installs []string
	quiet := func(string) {}
	if got, _ := MigrateMemoryHome(ctx, b, func(string) (map[string]any, error) {
		installs = append(installs, b)
		return map[string]any{"ok": true}, nil
	}, quiet); got != "" || len(installs) != 0 {
		t.Fatalf("b migrated %q", got)
	}
	if got, _ := MigrateMemoryHome(ctx, a, func(string) (map[string]any, error) { return map[string]any{"ok": true}, nil }, quiet); got != "twin" {
		t.Fatalf("a got %q", got)
	}
}

func TestMigrateProviderUnknownToCatalogIsReportedNotInstalled(t *testing.T) {
	h := t.TempDir()
	writeMigrationHome(t, h, "memory:\n  provider: honcho\n")
	withMigrationSteps(t, func(string) (string, bool) { return "", false })
	var said []string
	got, err := MigrateMemoryHome(context.Background(), h, func(string) (map[string]any, error) {
		t.Fatal("must not install")
		return nil, nil
	}, func(s string) { said = append(said, s) })
	if err != nil || got != "" || !strings.Contains(said[0], "not in the plugin catalog") || !strings.Contains(said[0], "memory.provider") {
		t.Fatalf("%q %v %v", got, err, said)
	}
}

func TestStartupRecoveryAttemptsEachProfileHome(t *testing.T) {
	root := t.TempDir()
	homes := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
	for _, h := range homes {
		writeMigrationHome(t, h, "memory:\n  provider: twin\n")
	}
	withMigrationSteps(t, catalogAll)
	leftCoreMemoryProviders = map[string]bool{"twin": true}
	migrationSteps.lazyInstalls = func(context.Context) bool { return true }
	var installed []string
	migrationSteps.installInto = func(_ context.Context, h string, _ *bool) MemoryInstallFunc {
		return func(name string) (map[string]any, error) {
			if err := os.MkdirAll(filepath.Join(h, "plugins", name), 0o755); err != nil {
				t.Fatal(err)
			}
			installed = append(installed, h)
			return map[string]any{"ok": true}, nil
		}
	}
	var outcomes []bool
	for _, h := range []string{homes[0], homes[1], homes[0]} {
		ok, err := RecoverMemoryProviderAtStartup(home.WithOverride(context.Background(), h), "twin", nil)
		if err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, ok)
	}
	if !(outcomes[0] && outcomes[1] && !outcomes[2]) || len(installed) != 2 || installed[0] != homes[0] || installed[1] != homes[1] {
		t.Fatalf("outcomes %v installed %v", outcomes, installed)
	}
}

func TestUnattendedMigrationInstallCarriesLazyInstallConsent(t *testing.T) {
	cases := []struct {
		name            string
		tty, lazy, want bool
	}{
		{"hindsight", false, true, true}, {"hindsight", false, false, false}, {"hindsight", true, true, false}, {"someplugin", false, true, false},
	}
	for _, tc := range cases {
		withMigrationSteps(t, catalogAll)
		migrationSteps.interactive = func() bool { return tc.tty }
		migrationSteps.lazyInstalls = func(context.Context) bool { return tc.lazy }
		var seen []bool
		provider.InstallPluginManagerHooks(provider.PluginManagerHooks{Install: func(_ context.Context, _ string, consent bool) (map[string]any, error) {
			seen = append(seen, consent)
			return map[string]any{"ok": true}, nil
		}})
		if _, err := memoryInstallInto(context.Background(), t.TempDir(), nil)(tc.name); err != nil || len(seen) != 1 || seen[0] != tc.want {
			t.Fatalf("%+v seen %v %v", tc, seen, err)
		}
	}
}

func TestCatalogInstallHintNeedsAMatchingCategory(t *testing.T) {
	withMigrationSteps(t, catalogAll)
	migrationSteps.catalogEntry = func(name string) (*plugins.PluginCatalogEntry, error) {
		if name == "honcho" {
			return &plugins.PluginCatalogEntry{Name: "honcho", Category: "memory"}, nil
		}
		return nil, nil
	}
	ctx := home.WithOverride(context.Background(), t.TempDir())
	if got := CatalogInstallHint(ctx, "honcho", "memory"); got != "icebath plugins install honcho" {
		t.Fatalf("got %q", got)
	}
	if CatalogInstallHint(ctx, "honcho", "tools") != "" || CatalogInstallHint(ctx, "no-such-provider", "") != "" {
		t.Fatal("hint for a miss")
	}
	migrationSteps.catalogEntry = func(string) (*plugins.PluginCatalogEntry, error) { return nil, errors.New("catalog broken") }
	if CatalogInstallHint(ctx, "honcho", "memory") != "" {
		t.Fatal("hint despite a failing catalog")
	}
}

func TestStartupRecoveryBacksOffAfterAFailedInstall(t *testing.T) {
	h := t.TempDir()
	writeMigrationHome(t, h, "memory:\n  provider: hindsight\n")
	withMigrationSteps(t, catalogAll)
	migrationSteps.lazyInstalls = func(context.Context) bool { return true }
	var attempts, said []string
	migrationSteps.installInto = func(context.Context, string, *bool) MemoryInstallFunc {
		return func(n string) (map[string]any, error) {
			attempts = append(attempts, n)
			return map[string]any{"ok": false, "error": "uv lock exited 1"}, nil
		}
	}
	ctx := home.WithOverride(context.Background(), h)
	for range 3 {
		migrationAttemptedMu.Lock()
		migrationAttempted = map[[2]string]bool{}
		migrationAttemptedMu.Unlock()
		if ok, err := RecoverMemoryProviderAtStartup(ctx, "hindsight", func(s string) { said = append(said, s) }); ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
	}
	if len(attempts) != 1 || !strings.Contains(said[len(said)-1], "`icebath plugins install hindsight`") {
		t.Fatalf("attempts %v said %v", attempts, said)
	}
}

func TestCloneHonchoForProfileRunsThePluginCLIInTheHost(t *testing.T) {
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	var ie *ProviderImportError
	if _, err := CloneHonchoForProfile(ctx, "work"); !errors.As(err, &ie) || ie.Msg != "memory provider 'honcho' is not installed" {
		t.Fatalf("err %v", err)
	}
	registerUserFlow(t, "honcho", h, nil)
	saved := providerModuleCall
	t.Cleanup(func() { providerModuleCall = saved })
	var gotModule string
	providerModuleCall = func(_ context.Context, dir, module, sub, fn string, args []any, _ map[string]any) (any, bool, error) {
		gotModule = module
		if dir != filepath.Join(h, "plugins", "honcho") || sub != "cli" || fn != "clone_honcho_for_profile" || args[0] != "work" {
			t.Fatalf("call %s %s %s %v", dir, sub, fn, args)
		}
		return true, true, nil
	}
	if ok, err := CloneHonchoForProfile(ctx, "work"); !ok || err != nil || !strings.HasPrefix(gotModule, "_icebath_user_memory.honcho__source_") {
		t.Fatalf("%v %v %s", ok, err, gotModule)
	}
	providerModuleCall = func(context.Context, string, string, string, string, []any, map[string]any) (any, bool, error) {
		return nil, true, &plugins.PluginHostError{TypeName: "ConfigWriteRefused", RemoteMessage: "managed config"}
	}
	var refused *ConfigWriteRefused
	if _, err := CloneHonchoForProfile(ctx, "work"); !errors.As(err, &refused) || refused.Error() != "managed config" {
		t.Fatalf("err %v", err)
	}
	providerModuleCall = func(context.Context, string, string, string, string, []any, map[string]any) (any, bool, error) {
		return nil, false, nil
	}
	if _, err := CloneHonchoForProfile(ctx, "work"); !errors.As(err, &ie) || ie.Msg != "memory provider 'honcho' failed to import" {
		t.Fatalf("err %v", err)
	}
}

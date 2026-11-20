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
	"github.com/nickheyer/icebath/pkg/tools"
)

// Swaps the migration seams for one test, presence read from <home>/plugins/<name>
func withMigrationSteps(t *testing.T, catalog func(string) (string, bool)) {
	t.Helper()
	saved := migrationSteps
	t.Cleanup(func() {
		migrationSteps = saved
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
	if got, err := pendingMemoryProvider(ctx, h, say); err != nil || got != "honcho" {
		t.Fatalf("got %q %v", got, err)
	}
	if ok, _ := installPendingMemory(ctx, h, "honcho", install, say); !ok {
		t.Fatalf("install failed, said %q", said)
	}
	if strings.Contains(said[0], "memory.honcho") || !strings.Contains(said[0], "icebath memory status") {
		t.Fatalf("said %q", said[0])
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if !strings.Contains(string(raw), "workspace: keep-me") {
		t.Fatal("config rewritten")
	}
	if got, err := pendingMemoryProvider(ctx, h, say); err != nil || got != "" || len(calls) != 1 {
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
	registerUserProvider(t, "twin", b)
	ctx := home.WithOverride(context.Background(), a)
	quiet := func(string) {}
	if got, _ := pendingMemoryProvider(ctx, b, quiet); got != "" {
		t.Fatalf("b pending %q", got)
	}
	if got, _ := pendingMemoryProvider(ctx, a, quiet); got != "twin" {
		t.Fatalf("a got %q", got)
	}
}

func TestMigrateProviderUnknownToCatalogIsReportedNotInstalled(t *testing.T) {
	h := t.TempDir()
	writeMigrationHome(t, h, "memory:\n  provider: honcho\n")
	withMigrationSteps(t, func(string) (string, bool) { return "", false })
	var said []string
	got, err := pendingMemoryProvider(context.Background(), h, func(s string) { said = append(said, s) })
	if err != nil || got != "" || !strings.Contains(said[0], "not in the plugin catalog") || !strings.Contains(said[0], "memory.provider") {
		t.Fatalf("%q %v %v", got, err, said)
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

// Default home plus named profile homes all configured with twin, a catalog provider stand in
func migrationProfileHomes(t *testing.T, names ...string) []string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	homes := []string{root}
	for _, n := range names {
		homes = append(homes, filepath.Join(root, "profiles", n))
	}
	for _, h := range homes {
		writeMigrationHome(t, h, "memory:\n  provider: twin\n")
	}
	t.Setenv("ICEBATH_HOME", root)
	withMigrationSteps(t, catalogAll)
	return homes
}

func TestStartupHintInstallsIntoTheProfileThatPrintedIt(t *testing.T) {
	homes := migrationProfileHomes(t, "work")
	for _, tc := range []struct{ h, command string }{
		{homes[1], "icebath -p work plugins install twin"}, {homes[0], "icebath plugins install twin"},
	} {
		migrationAttemptedMu.Lock()
		migrationAttempted = map[[2]string]bool{}
		migrationAttemptedMu.Unlock()
		t.Setenv("ICEBATH_HOME", tc.h)
		var said []string
		err := ReportMissingMemoryProvider(context.Background(), "twin", func(s string) { said = append(said, s) })
		if err != nil || len(said) == 0 || !strings.Contains(said[0], "`"+tc.command+"`") {
			t.Fatalf("%s %v said %q", tc.h, err, said)
		}
	}
}

// Registers a user directory provider under home h, retracted when the test ends
func registerUserProvider(t *testing.T, name, h string) {
	t.Helper()
	RegisterProvider(ProviderRegistration{
		Name: name, Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", name),
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) { return nil, nil },
	})
	t.Cleanup(func() { UnregisterProviders(SourceUser, h) })
}

package memory

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	stamp := filepath.Join(home.CacheDir(h), "left-core-hindsight.failed")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stamp, past, past); err != nil {
		t.Fatal(err)
	}
	migrationAttemptedMu.Lock()
	migrationAttempted = map[[2]string]bool{}
	migrationAttemptedMu.Unlock()
	if ok, err := RecoverMemoryProviderAtStartup(ctx, "hindsight", nil); ok || err != nil || len(attempts) != 2 {
		t.Fatalf("%v %v attempts %v", ok, err, attempts)
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

func TestStartupRecoveryNeverAsksADependencyQuestion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		installs bool
	}{{"hindsight", true}, {"someplugin", false}} {
		h := t.TempDir()
		writeMigrationHome(t, h, "memory:\n  provider: "+tc.name+"\n")
		withMigrationSteps(t, catalogAll)
		migrationSteps.interactive = func() bool { return true }
		migrationSteps.lazyInstalls = func(context.Context) bool { return true }
		var consents []bool
		migrationSteps.installInto = func(_ context.Context, _ string, consent *bool) MemoryInstallFunc {
			return func(string) (map[string]any, error) {
				if consent == nil {
					t.Fatal("startup install left the dependency answer to a prompt")
				}
				consents = append(consents, *consent)
				return map[string]any{"ok": false}, nil
			}
		}
		var said []string
		if _, err := RecoverMemoryProviderAtStartup(home.WithOverride(context.Background(), h), tc.name, func(s string) { said = append(said, s) }); err != nil {
			t.Fatal(err)
		}
		if tc.installs && (len(consents) != 1 || !consents[0]) {
			t.Fatalf("%s consents %v", tc.name, consents)
		}
		if !tc.installs && (len(consents) != 0 || !strings.Contains(said[len(said)-1], "`icebath plugins install "+tc.name+"`")) {
			t.Fatalf("%s consents %v said %v", tc.name, consents, said)
		}
	}
}

// Installs plugin manager hooks for one test and puts the previous ones back
func withPluginManagerHooks(t *testing.T, hooks provider.PluginManagerHooks) {
	t.Helper()
	var saved *provider.PluginManagerHooks
	func() {
		defer func() { _ = recover() }()
		saved = provider.PluginHooks()
	}()
	provider.InstallPluginManagerHooks(hooks)
	t.Cleanup(func() {
		if saved == nil {
			saved = &provider.PluginManagerHooks{}
		}
		provider.InstallPluginManagerHooks(*saved)
	})
}

// Default home plus named profile homes all configured with twin, a left core stand in
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
	leftCoreMemoryProviders = map[string]bool{"twin": true}
	return homes
}

func TestUpdateAsksOnceAndNamesEachProfile(t *testing.T) {
	homes := migrationProfileHomes(t, "work-a", "work-b")
	withPluginManagerHooks(t, provider.PluginManagerHooks{DependencyHomes: func() ([]string, error) { return homes, nil }})
	migrationSteps.interactive = func() bool { return true }
	migrationSteps.installInto = func(ctx context.Context, h string, _ *bool) MemoryInstallFunc {
		return func(name string) (map[string]any, error) {
			consented, reason := plugins.ConsentPythonDeps(ctx, name, []string{"twin-client"}, plugins.NewConsole(io.Discard), false)
			if !consented {
				return map[string]any{"ok": false, "error": reason}, nil
			}
			if err := os.MkdirAll(filepath.Join(h, "plugins", name), 0o755); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"ok": true}, nil
		}
	}
	for _, tc := range []struct {
		answer string
		want   []string
	}{{"n", nil}, {"y", []string{"twin", "twin", "twin"}}} {
		var prompts, said []string
		restore := plugins.SwapTerminalIO(func() bool { return true }, func(prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return tc.answer, nil
		})
		got, err := MigrateAllMemoryHomes(context.Background(), func(s string) { said = append(said, s) })
		if err != nil {
			t.Fatal(err)
		}
		restore()
		if strings.Join(got, ",") != strings.Join(tc.want, ",") || len(prompts) != 1 {
			t.Fatalf("%s got %v prompts %v said %q", tc.answer, got, prompts, said)
		}
		if tc.answer == "n" {
			if len(said) < 3 || !strings.Contains(said[1], "[profile 'default']") || !strings.Contains(said[1], "dependency install declined") ||
				!strings.Contains(said[2], "icebath -p work-a plugins install twin") || !strings.Contains(said[2], "icebath -p work-b plugins install twin") {
				t.Fatalf("said %q", said)
			}
			for _, h := range homes {
				if _, err := os.Stat(filepath.Join(h, "plugins")); err == nil {
					t.Fatalf("declined install created %s/plugins", h)
				}
			}
			continue
		}
		var labels []string
		for _, line := range said[1:] {
			labels = append(labels, strings.SplitN(line, "]", 2)[0])
		}
		if strings.Join(labels, "|") != "  [profile 'default'|  [profile 'work-a'|  [profile 'work-b'" {
			t.Fatalf("said %q", said)
		}
	}
}

func TestUnattendedUpdateRefusalInOneProfileDoesNotSkipTheOthers(t *testing.T) {
	homes := migrationProfileHomes(t, "work")
	def, work := homes[0], homes[1]
	migrationSteps.interactive = func() bool { return false }
	migrationSteps.lazyInstalls = func(ctx context.Context) bool { return home.Dir(ctx) == work }
	migrationSteps.installInto = memoryInstallInto
	withPluginManagerHooks(t, provider.PluginManagerHooks{
		DependencyHomes: func() ([]string, error) { return []string{def, work}, nil },
		Install: func(ctx context.Context, name string, consent bool) (map[string]any, error) {
			if !consent {
				return map[string]any{"ok": false, "error": "dependency install skipped (non-interactive)"}, nil
			}
			if err := os.MkdirAll(filepath.Join(home.Dir(ctx), "plugins", name), 0o755); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"ok": true}, nil
		},
	})
	var said []string
	got, err := MigrateAllMemoryHomes(context.Background(), func(s string) { said = append(said, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "twin" {
		t.Fatalf("got %v said %q", got, said)
	}
	if st, err := os.Stat(filepath.Join(work, "plugins", "twin")); err != nil || !st.IsDir() {
		t.Fatalf("work home not migrated %v", err)
	}
	if _, err := os.Stat(filepath.Join(def, "plugins")); err == nil {
		t.Fatal("the refusing default home was installed into")
	}
	for _, line := range said {
		if strings.Contains(line, "either") {
			t.Fatalf("said %q", said)
		}
	}
}

func TestStartupHintInstallsIntoTheProfileThatPrintedIt(t *testing.T) {
	homes := migrationProfileHomes(t, "work")
	migrationSteps.lazyInstalls = func(context.Context) bool { return false }
	for _, tc := range []struct{ h, command string }{
		{homes[1], "icebath -p work plugins install twin"}, {homes[0], "icebath plugins install twin"},
	} {
		migrationAttemptedMu.Lock()
		migrationAttempted = map[[2]string]bool{}
		migrationAttemptedMu.Unlock()
		t.Setenv("ICEBATH_HOME", tc.h)
		var said []string
		ok, err := RecoverMemoryProviderAtStartup(context.Background(), "twin", func(s string) { said = append(said, s) })
		if ok || err != nil || len(said) == 0 || !strings.Contains(said[0], "`"+tc.command+"`") {
			t.Fatalf("%s %v %v said %q", tc.h, ok, err, said)
		}
	}
}

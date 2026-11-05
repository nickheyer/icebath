package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const camofoxSplitEnv = "CAMOFOX_URL=http://localhost:9377\n"

// Fresh attempt set, no Camofox or CDP env and an empty managed scope
func camofoxFresh(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	leftCoreAttemptedMu.Lock()
	prev := leftCoreAttempted
	leftCoreAttempted = map[string]bool{}
	leftCoreAttemptedMu.Unlock()
	t.Cleanup(func() {
		leftCoreAttemptedMu.Lock()
		leftCoreAttempted = prev
		leftCoreAttemptedMu.Unlock()
	})
	for _, name := range []string{"CAMOFOX_URL", "BROWSER_CDP_URL"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	managed := filepath.Join(root, "managed")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return root
}

func camofoxConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.FastSafeLoad(data)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := doc.(map[string]any)
	return m
}

// Runs migrate_config non interactively with the home bound as the active override
func camofoxMigrateConfig(t *testing.T, h string) {
	t.Helper()
	config.ResetCache()
	if _, err := config.MigrateConfig(home.WithOverride(context.Background(), h), h, false, true); err != nil {
		t.Fatal(err)
	}
}

func camofoxInstaller(t *testing.T, h string, calls *[]string) func(string) (map[string]any, error) {
	return func(name string) (map[string]any, error) {
		*calls = append(*calls, name)
		mkPluginDir(t, h, name)
		return map[string]any{"ok": true}, nil
	}
}

// Agent start for the home with the plugin install a stand in
func camofoxBoot(t *testing.T, h string, said *[]string) []string {
	t.Helper()
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "")
	os.Unsetenv("ICEBATH_DISABLE_LAZY_INSTALLS")
	prev := leftCoreInstallInto
	leftCoreInstallInto = func(_ context.Context, target string, _ *bool) func(string) (map[string]any, error) {
		var calls []string
		return camofoxInstaller(t, target, &calls)
	}
	t.Cleanup(func() { leftCoreInstallInto = prev })
	return RecoverLeftCoreAtStartup(context.Background(), func(m string) { *said = append(*said, m) }, false)
}

func camofoxStrings(v any) []string {
	items, _ := v.([]any)
	return pyStrList(items)
}

func TestCamofoxUpgradeConvertsAfterTheConfigStep(t *testing.T) {
	root := camofoxFresh(t)
	t.Setenv(home.EnvHome, filepath.Join(root, "launch"))
	h := leftCoreHome(t, root, "home", camofoxSplitEnv, "_config_version: 51\nplatform_toolsets:\n  cli:\n  - terminal\n  - browser\n  - file\n")
	camofoxMigrateConfig(t, h)
	if v := camofoxConfig(t, h)["_config_version"]; v != config.Version {
		t.Fatalf("stamp %#v", v)
	}
	var calls []string
	if got := MigrateLeftCoreHome(context.Background(), h, camofoxInstaller(t, h, &calls), func(string) {}, false); !slices.Equal(got, []string{"camofox"}) {
		t.Fatalf("installed %v", got)
	}
	cfg := camofoxConfig(t, h)
	if got := camofoxStrings(anyMap(cfg["platform_toolsets"])["cli"]); !slices.Equal(got, []string{"terminal", "file", "camofox"}) {
		t.Fatalf("cli %v", got)
	}
	if got := camofoxStrings(anyMap(cfg["agent"])["disabled_toolsets"]); !slices.Equal(got, []string{"browser"}) {
		t.Fatalf("disabled %v", got)
	}
}

func TestCamofoxHomeFromAfterTheSplitKeepsBrowserExec(t *testing.T) {
	for _, stamp := range []string{"51", "52"} {
		t.Run(stamp, func(t *testing.T) {
			root := camofoxFresh(t)
			t.Setenv(home.EnvHome, filepath.Join(root, "launch"))
			h := leftCoreHome(t, root, "home", "", "_config_version: "+stamp+"\nplatform_toolsets:\n  cli:\n  - terminal\n  - browser\n  - file\n")
			camofoxMigrateConfig(t, h)
			mkPluginDir(t, h, "camofox")
			writeLeftCoreFile(t, filepath.Join(h, ".env"), camofoxSplitEnv)
			before, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
			install := func(name string) (map[string]any, error) {
				t.Fatalf("installed by the user: %s", name)
				return nil, nil
			}
			if got := MigrateLeftCoreHome(context.Background(), h, install, func(string) {}, false); len(got) != 0 {
				t.Fatalf("installed %v", got)
			}
			if after, _ := os.ReadFile(filepath.Join(h, "config.yaml")); !bytes.Equal(after, before) {
				t.Fatalf("config rewritten:\n%s\n---\n%s", before, after)
			}
		})
	}
}

func TestCamofoxAnUnversionedHomeIsConverted(t *testing.T) {
	for name, cfg := range map[string]string{"no-config": "", "unstamped": "platform_toolsets:\n  cli:\n  - terminal\n  - browser\n  - file\n"} {
		t.Run(name, func(t *testing.T) {
			root := camofoxFresh(t)
			t.Setenv(home.EnvHome, filepath.Join(root, "launch"))
			h := leftCoreHome(t, root, "home", camofoxSplitEnv, cfg)
			var calls []string
			if got := MigrateLeftCoreHome(context.Background(), h, camofoxInstaller(t, h, &calls), func(string) {}, false); !slices.Equal(got, []string{"camofox"}) {
				t.Fatalf("installed %v", got)
			}
			if got := camofoxStrings(anyMap(camofoxConfig(t, h)["agent"])["disabled_toolsets"]); !slices.Equal(got, []string{"browser"}) {
				t.Fatalf("disabled %v", got)
			}
		})
	}
}

func TestCamofoxTheConfigYAMLAnEnvOnlyConversionCreatesIsStamped(t *testing.T) {
	root := camofoxFresh(t)
	h := leftCoreHome(t, root, "home", camofoxSplitEnv, "")
	var said []string
	if got := camofoxBoot(t, h, &said); !slices.Equal(got, []string{"camofox"}) {
		t.Fatalf("boot installed %v (%s)", got, strings.Join(said, "\n"))
	}
	cfg := camofoxConfig(t, h)
	if cfg["_config_version"] != config.Version {
		t.Fatalf("stamp %#v", cfg["_config_version"])
	}
	if got := camofoxStrings(cfg["_left_core_used"]); !slices.Equal(got, []string{"camofox"}) {
		t.Fatalf("_left_core_used %v", got)
	}
	leftCoreAttemptedMu.Lock()
	leftCoreAttempted = map[string]bool{}
	leftCoreAttemptedMu.Unlock()
	said = nil
	if got := camofoxBoot(t, h, &said); len(got) != 0 || len(said) != 0 {
		t.Fatalf("next process boot installed %v said %v", got, said)
	}
}

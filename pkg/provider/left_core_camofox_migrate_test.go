package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

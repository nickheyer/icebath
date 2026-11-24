package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Points the home at a fresh directory with no launch folder handed in
func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	isolateMirroredEnv(t)
	return dir
}

// Unsets the launch folder a parent process may have handed this test process, restored after the case
func isolateMirroredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
}

func TestResolvePrefillMessagesFileUsesTopLevel(t *testing.T) {
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "")
	os.Unsetenv("ICEBATH_PREFILL_MESSAGES_FILE")
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("prefill_messages_file: top.json\n"), 0o600)
	if got := resolvePrefillMessagesFile(context.Background()); got != "top.json" {
		t.Fatalf("got %q", got)
	}
}

func TestResolvePrefillMessagesFilePrefersEnv(t *testing.T) {
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("prefill_messages_file: top.json\n"), 0o600)
	t.Setenv("ICEBATH_PREFILL_MESSAGES_FILE", "env.json")
	if got := resolvePrefillMessagesFile(context.Background()); got != "env.json" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadPrefillMessagesReadsAJSONArrayUnderTheHome(t *testing.T) {
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, "prefill.json"), []byte("\ufeff[{\"role\": \"user\", \"content\": \"hi\"}]"), 0o600)
	os.WriteFile(filepath.Join(home, "object.json"), []byte(`{"role": "user"}`), 0o600)
	ctx := context.Background()
	if got := loadPrefillMessages(ctx, "prefill.json", ""); len(got) != 1 || got[0].(map[string]any)["role"] != "user" {
		t.Fatalf("got %v", got)
	}
	if got := loadPrefillMessages(ctx, "object.json", ""); len(got) != 0 {
		t.Fatalf("object accepted %v", got)
	}
	if got := loadPrefillMessages(ctx, "missing.json", ""); len(got) != 0 {
		t.Fatalf("missing accepted %v", got)
	}
	if got := loadPrefillMessages(ctx, "", ""); len(got) != 0 {
		t.Fatalf("empty path gave %v", got)
	}
}

// Writes the home and managed config files of a fresh home
func loadWithManaged(t *testing.T, userYAML, managedYAML string) context.Context {
	t.Helper()
	home := tempHome(t)
	managed := t.TempDir()
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(userYAML), 0o600)
	os.WriteFile(filepath.Join(managed, "config.yaml"), []byte(managedYAML), 0o600)
	config.InvalidateManagedCache()
	return context.Background()
}

func TestCliConfigHonorsManagedSkin(t *testing.T) {
	ctx := loadWithManaged(t, "display:\n  skin: user_skin\n", "display:\n  skin: cyberpunk\n")
	if got := settings.Skin.Get(ctx); got != "cyberpunk" {
		t.Fatalf("skin %q", got)
	}
}

func TestCliConfigManagedLeafPreservesUserSiblings(t *testing.T) {
	ctx := loadWithManaged(t, "display:\n  skin: user_skin\n  show_reasoning: true\n", "display:\n  skin: cyberpunk\n")
	if got := settings.Skin.Get(ctx); got != "cyberpunk" {
		t.Fatalf("skin %q", got)
	}
	if !settings.ShowThinking.Get(settings.WithPlatform(ctx, settings.LocalPlatform)) {
		t.Fatal("user show_reasoning lost under a managed sibling")
	}
}

// Writes a home config.yaml of a fresh home
func loadFromYAML(t *testing.T, yaml string) context.Context {
	t.Helper()
	home := tempHome(t)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(yaml), 0o600)
	return context.Background()
}

func TestRootLevelProviderOverrideTerminalVercelRuntimeKept(t *testing.T) {
	ctx := loadFromYAML(t, "terminal:\n  backend: vercel_sandbox\n  vercel_runtime: python3.13\n")
	if got := settings.TerminalVercelRuntime.Get(ctx); got != "python3.13" {
		t.Fatalf("vercel runtime %q", got)
	}
}

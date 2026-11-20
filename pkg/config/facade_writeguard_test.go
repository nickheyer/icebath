package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Facade home under an administrator managed dir holding the given config and .env text
func managedScopeHome(t *testing.T, managedConfig, managedEnv string) (string, func() string) {
	t.Helper()
	h, _ := facadeHome(t, "")
	managed := filepath.Join(t.TempDir(), "managed")
	os.MkdirAll(managed, 0o755)
	if managedConfig != "" {
		os.WriteFile(filepath.Join(managed, "config.yaml"), []byte(managedConfig), 0o644)
	}
	if managedEnv != "" {
		os.WriteFile(filepath.Join(managed, ".env"), []byte(managedEnv), 0o644)
	}
	t.Setenv(EnvManagedDir, managed)
	InvalidateManagedCache()
	ResetCache()
	out, errOut := captureFacade(t)
	return h, func() string { return out.String() + errOut.String() }
}

func TestConfigSetManagedKeyRejected(t *testing.T) {
	h, output := managedScopeHome(t, "model:\n  default: managed/model\n", "")
	err := SetConfigValue(context.Background(), h, "model.default", "user/override", false, pathCommandSources())
	if code := exitCode(err); code == 0 || code == -1 || !strings.Contains(strings.ToLower(output()), "managed") {
		t.Fatalf("managed set returned %v output %q", err, output())
	}
}

func TestSaveEnvValueManagedKeyRejected(t *testing.T) {
	h, output := managedScopeHome(t, "", "OPENAI_API_BASE=https://org.example/v1\n")
	if err := SaveEnvValue(h, "OPENAI_API_BASE", "https://user.example/v1"); err != nil {
		t.Fatalf("managed env refusal raised %v", err)
	}
	if !strings.Contains(strings.ToLower(output()), "managed") || strings.Contains(readText(home.EnvPath(h)), "user.example") {
		t.Fatalf("output %q env %q", output(), readText(home.EnvPath(h)))
	}
}

func TestFileSetAndUnsetStillPrintTheConfigPath(t *testing.T) {
	h, path := facadeHome(t, "")
	out, _ := captureFacade(t)
	ctx := context.Background()
	if err := SetConfigValue(ctx, h, "agent.max_turns", "7", false, pathCommandSources()); err != nil {
		t.Fatal(err)
	}
	if err := UnsetConfigValue(ctx, h, "agent.max_turns", pathCommandSources()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "✓ Set agent.max_turns = 7 in "+path+"\n") || !strings.Contains(out.String(), "✓ Unset agent.max_turns from "+path+"\n") {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestConfigEditRefusalWithoutAConfigFileExitsNonzero(t *testing.T) {
	h, path := facadeHome(t, "")
	registerTestBackend(t, "nofile", func() ConfigBackend { return &fakeKindBackend{name: "nofile", tooling: false} })
	t.Setenv(BackendEnv, "nofile")
	ResetCache()
	prevEditor := runEditor
	runEditor = func(string, string) error { t.Fatal("no editor may open"); return nil }
	t.Cleanup(func() { runEditor = prevEditor })
	_, errOut := captureFacade(t)
	err := EditConfig(context.Background(), h)
	if exitCode(err) != 1 || !strings.Contains(errOut.String(), "icebath config set") {
		t.Fatalf("edit returned %v stderr %q", err, errOut.String())
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("refused edit created config.yaml")
	}
}

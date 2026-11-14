package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestPersistModelSelectionWritesTheModelSection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model:\n  default: old\n  provider: openrouter\n  api_key: inline-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := PoolEnv{Store: NewStore(dir)}
	if _, err := PersistPluginModelSelection(env, "m-1", "fake-provider", optStr("https://api.example.test/v1"), optStr("chat_completions")); err != nil {
		t.Fatal(err)
	}
	tree, err := config.Load(dir, config.NewSecrets(dir).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := tree["model"].(map[string]any)
	if model["default"] != "m-1" || model["provider"] != "fake-provider" {
		t.Fatalf("model section %v", model)
	}
	if model["base_url"] != "https://api.example.test/v1" || model["api_mode"] != "chat_completions" {
		t.Fatalf("endpoint fields %v", model)
	}
	if _, held := model["api_key"]; held {
		t.Fatalf("endpoint credentials of the previous route carried over: %v", model)
	}
}

func TestNoteSetupFailureReachesTheSetupMetric(t *testing.T) {
	var seen []string
	prev := noteProviderSetupFailureFn
	noteProviderSetupFailureFn = func(failureClass string) { seen = append(seen, failureClass) }
	t.Cleanup(func() { noteProviderSetupFailureFn = prev })
	NoteSetupFailure("auth")
	if len(seen) != 1 || seen[0] != "auth" {
		t.Fatalf("seen %v", seen)
	}
}

func TestSecretSourceLabelNamesAnExternalSource(t *testing.T) {
	h := newTestHome(t)
	secretSourceCacheMu.Lock()
	secretSourceOfVar["FAKE_TOKEN"] = "bitwarden"
	secretSourceCacheMu.Unlock()
	t.Cleanup(func() {
		secretSourceCacheMu.Lock()
		delete(secretSourceOfVar, "FAKE_TOKEN")
		secretSourceCacheMu.Unlock()
	})
	if got := SecretSourceLabel(h, "FAKE_TOKEN"); got != " (from Bitwarden)" {
		t.Fatalf("label %q", got)
	}
	if got := SecretSourceLabel(h, "OTHER_TOKEN"); got != "" {
		t.Fatalf("label %q", got)
	}
}

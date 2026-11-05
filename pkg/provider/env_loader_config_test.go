package provider

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Fresh home with every known env key absent from the process, returning the home and its .env path
func envLoaderReloadHome(t *testing.T) (string, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	names := configFacadeExtraEnvKeys()
	for name := range config.OptionalEnvVars {
		names = append(names, name)
	}
	for _, name := range names {
		if _, ok := os.LookupEnv(name); ok {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	return h, home.EnvPath(h)
}

func TestEnvLoaderReloadKeepsKnownKeysWhenUserEnvIsUnreadable(t *testing.T) {
	h, path := envLoaderReloadHome(t)
	if err := os.WriteFile(path, []byte("OPENAI_API_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "from-launcher")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	if n, err := config.ReloadEnv(h); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != "from-launcher" {
		t.Fatal(got)
	}
}

func TestEnvLoaderReloadStillRemovesKeysAReadableEnvDropped(t *testing.T) {
	h, path := envLoaderReloadHome(t)
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "stale")
	t.Setenv("ANTHROPIC_API_KEY", "stale")
	if n, err := config.ReloadEnv(h); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	if _, ok := os.LookupEnv("OPENAI_API_KEY"); ok || os.Getenv("ANTHROPIC_API_KEY") != "from-file" {
		t.Fatal(os.Getenv("OPENAI_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"))
	}
}

func TestEnvLoaderLoadEnvPreservesConcatenatedTextAsValueData(t *testing.T) {
	h, path := envLoaderReloadHome(t)
	token := "0123456789:test"
	if err := os.WriteFile(path, []byte("TELEGRAM_BOT_TOKEN="+token+"ANTHROPIC_API_KEY=sk-ant-test123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := config.LoadEnv(h)
	if result["TELEGRAM_BOT_TOKEN"] != token+"ANTHROPIC_API_KEY=sk-ant-test123" {
		t.Fatal(result)
	}
	if _, ok := result["ANTHROPIC_API_KEY"]; ok {
		t.Fatal(result)
	}
}

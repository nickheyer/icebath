package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Context carrying the given secret scope with the multiplex flag set for one test
func envValueScope(t *testing.T, secrets map[string]string, multiplex bool) context.Context {
	t.Helper()
	SetMultiplexActive(multiplex)
	t.Cleanup(func() { SetMultiplexActive(false) })
	if secrets == nil {
		return t.Context()
	}
	return WithSecretScope(t.Context(), secrets, "")
}

func TestScopedValueWinsOverEnviron(t *testing.T) {
	h := configModuleHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-other-profile")
	ctx := envValueScope(t, map[string]string{"OPENAI_API_KEY": "sk-this-profile"}, true)
	if got, ok, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || !ok || got != "sk-this-profile" {
		t.Fatalf("value %q %v %v", got, ok, err)
	}
}

func TestMultiplexedScopeMissDoesNotLeakEnviron(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	config.InvalidateEnvCache()
	t.Cleanup(config.InvalidateEnvCache)
	t.Setenv("OPENAI_API_KEY", "sk-other-profile")
	ctx := envValueScope(t, map[string]string{}, true)
	if got, ok, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || ok || got != "" {
		t.Fatalf("another profile's credential leaked: %q %v %v", got, ok, err)
	}
}

func TestMultiplexOffScopeMissFallsBackToEnviron(t *testing.T) {
	h := configModuleHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-from-shell")
	ctx := envValueScope(t, map[string]string{}, false)
	if got, ok, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || !ok || got != "sk-from-shell" {
		t.Fatalf("value %q %v %v", got, ok, err)
	}
}

func TestNoScopeSingleProfileBehavesLikeLegacy(t *testing.T) {
	h := configModuleHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-legacy")
	ctx := envValueScope(t, nil, false)
	if got, ok, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY"); err != nil || !ok || got != "sk-legacy" {
		t.Fatalf("value %q %v %v", got, ok, err)
	}
}

func TestMultiplexActiveUnscopedReadFailsClosed(t *testing.T) {
	h := configModuleHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-should-not-leak")
	ctx := envValueScope(t, nil, true)
	_, _, err := config.GetEnvValue(ctx, h, "OPENAI_API_KEY")
	var unscoped *UnscopedSecretError
	if !errors.As(err, &unscoped) {
		t.Fatalf("unscoped read returned %v", err)
	}
}

func TestDotenvFallbackStillWorks(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("MY_TEST_KEY=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.InvalidateEnvCache()
	t.Cleanup(config.InvalidateEnvCache)
	t.Setenv("MY_TEST_KEY", "")
	os.Unsetenv("MY_TEST_KEY")
	ctx := envValueScope(t, nil, false)
	if got, ok, err := config.GetEnvValue(ctx, h, "MY_TEST_KEY"); err != nil || !ok || got != "from-dotenv" {
		t.Fatalf("value %q %v %v", got, ok, err)
	}
}

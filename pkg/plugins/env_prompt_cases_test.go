package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Fresh home with the named variables unset in the process and restored after the case
func envPromptHome(t *testing.T, names ...string) (string, context.Context) {
	t.Helper()
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return h, home.WithOverride(context.Background(), h)
}

func TestPromptsForMissingVarRichFormat(t *testing.T) {
	h, ctx := envPromptHome(t, "LANGFUSE_PUBLIC_KEY")
	savedTTY, savedInput := isTTY, inputLine
	t.Cleanup(func() { isTTY, inputLine = savedTTY, savedInput })
	inputLine = func(string) (string, error) { return "pk-lf-123", nil }
	manifest := map[string]any{"name": "langfuse_tracing", "requires_env": []any{map[string]any{
		"name": "LANGFUSE_PUBLIC_KEY", "description": "Public key", "url": "https://langfuse.com", "secret": false}}}
	var out bytes.Buffer
	if err := promptPluginEnvVars(ctx, h, manifest, NewConsole(&out)); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(h, ".env"))
	if err != nil || !strings.Contains(string(saved), "LANGFUSE_PUBLIC_KEY=pk-lf-123") || strings.Count(string(saved), "LANGFUSE_PUBLIC_KEY=") != 1 {
		t.Fatalf("saved env %q %v", saved, err)
	}
	if !strings.Contains(out.String(), "langfuse.com") {
		t.Fatalf("printed %q", out.String())
	}
}

func TestSecretUsesMaskedPrompt(t *testing.T) {
	h, ctx := envPromptHome(t, "SECRET_KEY")
	calls := 0
	previous := InstalledServices()
	next := Services{}
	if previous != nil {
		next = *previous
	}
	next.MaskedSecretPrompt = func(string) (string, error) { calls++; return "s3cret", nil }
	InstallServices(&next)
	t.Cleanup(func() { InstallServices(previous) })
	manifest := map[string]any{"name": "test", "requires_env": []any{map[string]any{"name": "SECRET_KEY", "secret": true}}}
	if err := promptPluginEnvVars(ctx, h, manifest, NewConsole(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("masked prompt called %d times", calls)
	}
}

func TestADeclinedDependencyConsentCarriesItsClassNotItsCopy(t *testing.T) {
	console := NewConsole(&bytes.Buffer{})
	t.Cleanup(SwapTerminalIO(func() bool { return false }, inputLine))
	consented, reason := consentPythonDeps(context.Background(), "p", []string{"dep"}, console, false)
	if consented || reason == nil || reason.Text != "dependency install skipped (non-interactive)" || reason.FailureClass != "non_interactive" {
		t.Fatalf("non-interactive %v %+v", consented, reason)
	}
	restore := SwapTerminalIO(func() bool { return true }, func(string) (string, error) { return "n", nil })
	defer restore()
	if _, reason := consentPythonDeps(context.Background(), "p", []string{"dep"}, console, false); reason == nil || reason.FailureClass != "deps_declined" {
		t.Fatalf("declined %+v", reason)
	}
}

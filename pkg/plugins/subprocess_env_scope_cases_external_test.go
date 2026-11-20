//go:build !windows

package plugins_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch home with a served profile under it, the real secret source installed and each token at its launch value
func scopeCaseHomes(t *testing.T, launchValues map[string]string) (launch, served string) {
	t.Helper()
	launch = filepath.Join(t.TempDir(), "launch")
	served = filepath.Join(launch, "profiles", "beta")
	if err := os.MkdirAll(served, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	plugins.InstallHostEnvHooksForTests()
	prev := tools.ProfileSecrets
	tools.ProfileSecrets = provider.ProfileSecretSource()
	t.Cleanup(func() { tools.ProfileSecrets = prev })
	for name, value := range launchValues {
		t.Setenv(name, value)
	}
	return launch, served
}

// Writes a file of the home, failing the case on error
func writeHomeFile(t *testing.T, h, name, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(h, name)
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// Hook script printing one variable of its environment
func printingHook(t *testing.T, name string) string {
	t.Helper()
	return writeHomeFile(t, t.TempDir(), "print.sh", "#!/bin/sh\ncat > /dev/null\nprintf '%s' \"${"+name+":-}\"\n", 0o755)
}

func TestShellHookUnderAServedProfileScopeForwardsTheServedPassthroughValue(t *testing.T) {
	_, served := scopeCaseHomes(t, map[string]string{"SKILL_TOKEN": "launch-skill"})
	writeHomeFile(t, served, ".env", "SKILL_TOKEN=served-skill\n", 0o600)
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := provider.BindProfileSecretScope(home.WithOverride(context.Background(), served), served)
	r, err := plugins.RunShellHookOnce(ctx, plugins.NewShellHookSpec("pre_tool_call", printingHook(t, "SKILL_TOKEN"), "", 60, false), map[string]any{"tool_name": "terminal"})
	if err != nil || r.Error != "" {
		t.Fatalf("hook under the served profile's scope failed: %v %q", err, r.Error)
	}
	if r.Stdout != "served-skill" {
		t.Fatalf("hook child saw SKILL_TOKEN %q, want the served profile's served-skill", r.Stdout)
	}
}

func TestShellHookOfARoutedTaskUnderMultiplexSemanticsScrubsLaunchCredentials(t *testing.T) {
	launch, _ := scopeCaseHomes(t, map[string]string{"OPENAI_API_KEY": "sk-launch-profile"})
	routed := filepath.Join(launch, "profiles", "gamma")
	if err := os.MkdirAll(routed, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := provider.BindProfileSecretScope(home.WithOverride(context.Background(), routed), routed)
	ctx = provider.WithMultiplexContext(ctx, true)
	r, err := plugins.RunShellHookOnce(ctx, plugins.NewShellHookSpec("pre_tool_call", printingHook(t, "OPENAI_API_KEY"), "", 60, false), map[string]any{"tool_name": "terminal"})
	if err != nil || r.Error != "" {
		t.Fatalf("hook of a routed task failed: %v %q", err, r.Error)
	}
	if r.Stdout != "" {
		t.Fatalf("hook child of a routed task under multiplex semantics saw the launch OPENAI_API_KEY %q", r.Stdout)
	}
}

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

// SDK build_subprocess_env answer of a host serving the profile, as a plain map
func sdkChildEnv(t *testing.T, host *plugins.PluginHost, kwargs map[string]any) map[string]any {
	t.Helper()
	out, err := host.ServeChildSDKRequestForTests("build_subprocess_env", kwargs)
	if err != nil {
		t.Fatalf("build_subprocess_env for the served host failed: %v", err)
	}
	env, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("build_subprocess_env answered %T %v", out, out)
	}
	return env
}

// Served profile under multiplexing whose env file holds a passthrough token and a plain setting, the host services installed
func sdkServedProfile(t *testing.T) string {
	t.Helper()
	_, served := scopeCaseHomes(t, map[string]string{"SVC_TOKEN": "launch-svc", "SERVED_SETTING": ""})
	os.Unsetenv("SERVED_SETTING")
	writeHomeFile(t, served, ".env", "SVC_TOKEN=served-svc\nSERVED_SETTING=served-setting\n", 0o600)
	writeHomeFile(t, served, "config.yaml", "terminal:\n  env_passthrough: [SVC_TOKEN]\n", 0o600)
	prev := plugins.InstalledServicesForTests()
	plugins.InstallServices(&plugins.Services{BindProfileSecretScope: provider.BindProfileSecretScope, GetSecret: provider.GetSecret})
	t.Cleanup(func() { plugins.InstallServices(prev) })
	return served
}

func TestSDKBuildSubprocessEnvOfAServedHostCarriesTheServedProfilesEnvironment(t *testing.T) {
	served := sdkServedProfile(t)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	host, err := plugins.NewPluginHostForTests(served)
	if err != nil {
		t.Fatal(err)
	}
	env := sdkChildEnv(t, host, map[string]any{})
	if env["SVC_TOKEN"] != "served-svc" || env["SERVED_SETTING"] != "served-setting" || env["ICEBATH_HOME"] != served {
		t.Fatalf("plugin helper env SVC_TOKEN %v SERVED_SETTING %v ICEBATH_HOME %v, want the served profile's", env["SVC_TOKEN"], env["SERVED_SETTING"], env["ICEBATH_HOME"])
	}
}

func TestSDKBuildSubprocessEnvOfAHostStartedBeforeMultiplexingStillReadsItsProfile(t *testing.T) {
	served := sdkServedProfile(t)
	host, err := plugins.NewPluginHostForTests(served)
	if err != nil {
		t.Fatal(err)
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	env := sdkChildEnv(t, host, map[string]any{})
	if env["SVC_TOKEN"] != "served-svc" {
		t.Fatalf("plugin helper env SVC_TOKEN %v, want the served profile's served-svc", env["SVC_TOKEN"])
	}
}

func TestSDKBuildSubprocessEnvStrippingTheLaunchProfileKeepsTheRoutedHostsOwnValue(t *testing.T) {
	launch, served := scopeCaseHomes(t, map[string]string{"SVC_TOKEN": "launch-svc"})
	writeHomeFile(t, launch, ".env", "SVC_TOKEN=launch-svc\n", 0o600)
	writeHomeFile(t, served, ".env", "SVC_TOKEN=served-svc\n", 0o600)
	prev := plugins.InstalledServicesForTests()
	plugins.InstallServices(&plugins.Services{BindProfileSecretScope: provider.BindProfileSecretScope, GetSecret: provider.GetSecret})
	t.Cleanup(func() { plugins.InstallServices(prev) })
	host, err := plugins.NewPluginHostForTests(served)
	if err != nil {
		t.Fatal(err)
	}
	if env := sdkChildEnv(t, host, map[string]any{"strip_launch_profile": true}); env["SVC_TOKEN"] != "served-svc" {
		t.Fatalf("launch stripped plugin helper env SVC_TOKEN %v, want the routed host's own served-svc", env["SVC_TOKEN"])
	}
}

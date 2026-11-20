package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestLaunchBodyChildEnvAfterMultiplexingBeginsReadsTheLaunchMapping(t *testing.T) {
	launch := profileEnvLaunchHome(t)
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-shell")
	tools.RegisterEnvPassthrough(nil, []string{"LAUNCH_ONLY_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	body := provider.WithLaunchBody(context.Background())
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	child, err := tools.BuildSubprocessEnv(body, &tools.Env{Home: launch}, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		t.Fatalf("a launch body's child env failed once multiplexing began: %v", err)
	}
	if child["LAUNCH_ONLY_TOKEN"] != "launch-secret" {
		t.Fatalf("child saw LAUNCH_ONLY_TOKEN %q, want the launch body mapping's launch-secret", child["LAUNCH_ONLY_TOKEN"])
	}
}

func TestRoutedTaskUnderMultiplexSemanticsDropsTheLaunchValueItsScopeLacks(t *testing.T) {
	launch := profileEnvLaunchHome(t)
	routed := filepath.Join(launch, "profiles", "gamma")
	if err := os.MkdirAll(routed, 0o700); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, routed, ".env", "OTHER_TOKEN=gamma-other\n")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	ctx := provider.WithMultiplexContext(provider.BindProfileSecretScope(context.Background(), routed), true)
	child, err := tools.BuildSubprocessEnv(ctx, &tools.Env{Home: routed}, tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if value, leaked := child["SKILL_TOKEN"]; leaked {
		t.Fatalf("a routed task under multiplex semantics forwarded the launch profile's SKILL_TOKEN %q", value)
	}
}

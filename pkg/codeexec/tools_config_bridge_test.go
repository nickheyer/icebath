package codeexec

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigBridgeForwardsTheCuaSetup(t *testing.T) {
	hooks := tools.ToolsConfigHooks
	if hooks.ComputerUseDefaultBackend() != "cua" || hooks.ComputerUseProviders == nil {
		t.Fatal("computer use listing not installed")
	}
	prevReady, prevInstall := cuaDriverInstallReady, installCuaDriver
	t.Cleanup(func() { cuaDriverInstallReady, installCuaDriver = prevReady, prevInstall })
	cuaDriverInstallReady = func(*tools.Env) (bool, error) { return true, nil }
	if ok, err := hooks.CuaDriverInstallReady(&tools.Env{}); !ok || err != nil {
		t.Fatalf("ready %v %v", ok, err)
	}
	var progress []bool
	installCuaDriver = func(_ context.Context, _ *tools.Env, _ *cliui.Console, show bool) (bool, error) {
		progress = append(progress, show)
		return true, nil
	}
	if ok, err := hooks.InstallCuaDriver(context.Background(), &tools.Env{}, cliui.Default()); !ok || err != nil || len(progress) != 1 || !progress[0] {
		t.Fatalf("install %v %v %v", ok, err, progress)
	}
}

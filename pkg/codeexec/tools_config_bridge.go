package codeexec

import (
	"context"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the computer use provider listing and cua-driver install facts the tools picker reads
func init() {
	hooks := &tools.ToolsConfigHooks
	hooks.ComputerUseProviders = discoverComputerUseProviders
	hooks.ComputerUseDefaultBackend = func() string { return defaultComputerUseBackend }
	hooks.CuaDriverInstallReady = func(env *tools.Env) (bool, error) { return cuaDriverInstallReady(env) }
	hooks.InstallCuaDriver = func(ctx context.Context, env *tools.Env, con *cliui.Console) (bool, error) {
		return installCuaDriver(ctx, env, con, true)
	}
}

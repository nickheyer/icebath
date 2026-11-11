package codeexec

import (
	"context"
	"io"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reconciles the pinned cua-driver and its host setup on the active home, progress lines written to w
func InstallCuaDriver(ctx context.Context, w io.Writer, showInstallerProgress bool) (bool, error) {
	return installCuaDriver(ctx, &tools.Env{Home: home.Dir(ctx)}, &cliui.Console{Out: w}, showInstallerProgress)
}

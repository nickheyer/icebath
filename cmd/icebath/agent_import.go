package main

import (
	"context"
	"io"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Installs the agent-importers plugin install the setup offer and icebath migrate call
func installAgentImportWiring() {
	provider.AgentImportersInstaller = func(ctx context.Context, icebathHome string, present bool, out io.Writer) error {
		ctx = home.WithOverride(ctx, icebathHome)
		if present {
			return plugins.CmdEnable(ctx, "agent-importers", nil, out)
		}
		enable := true
		return plugins.CmdInstall(ctx, plugins.InstallCommandOptions{Identifier: "agent-importers", Enable: &enable, Brief: true}, out)
	}
}

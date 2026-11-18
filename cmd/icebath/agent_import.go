package main

import (
	"context"
	"io"

	"github.com/nickheyer/icebath/pkg/agentimport"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Installs the native importer and the agent-importers plugin install the setup offer and icebath migrate call
func installAgentImportWiring() {
	provider.AgentImportersInstaller = func(ctx context.Context, icebathHome string, present bool, out io.Writer) error {
		ctx = home.WithOverride(ctx, icebathHome)
		if present {
			return plugins.CmdEnable(ctx, "agent-importers", nil, out)
		}
		enable := true
		return plugins.CmdInstall(ctx, plugins.InstallCommandOptions{Identifier: "agent-importers", Enable: &enable, Brief: true}, out)
	}
	provider.AgentImportRunner = func(ctx context.Context, icebathHome string, o provider.AgentImportOptions, console *cliui.Console) (int, error) {
		return agentimport.ImportAgent(home.WithOverride(ctx, icebathHome), agentimport.Args{Console: console, Agent: o.Agent,
			Source: o.Source, DryRun: o.DryRun, Overwrite: o.Overwrite, Yes: o.Yes, ActivateRoutines: o.ActivateRoutines,
			MigrateSecrets: o.MigrateSecrets, FromSetup: o.FromSetup, Bot: o.Bot})
	}
}

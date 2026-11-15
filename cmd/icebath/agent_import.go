package main

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/agentimport"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/spf13/cobra"
)

// Installs the native importer as the runner the setup offer and icebath migrate call
func installAgentImportWiring() {
	provider.AgentImportRunner = func(ctx context.Context, icebathHome string, o provider.AgentImportOptions, console *cliui.Console) (int, error) {
		return agentimport.ImportAgent(home.WithOverride(ctx, icebathHome), agentimport.Args{Console: console, Agent: o.Agent,
			Source: o.Source, DryRun: o.DryRun, Overwrite: o.Overwrite, Yes: o.Yes, ActivateRoutines: o.ActivateRoutines,
			MigrateSecrets: o.MigrateSecrets, FromSetup: o.FromSetup, Bot: o.Bot})
	}
}

// Console a command prints on, the process console when its output is standard output
func commandConsole(cmd *cobra.Command) *cliui.Console {
	out := cmd.OutOrStdout()
	con := cliui.Default()
	if out == os.Stdout || out == con.Out {
		return con
	}
	return &cliui.Console{In: con.In, Out: out, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}
}

// Runs icebath import-agent with the parsed flags
func importAgentGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	bots, _ := ns["bot"].([]string)
	include, _ := ns["include"].([]string)
	exclude, _ := ns["exclude"].([]string)
	return agentimport.ImportAgent(ctx, agentimport.Args{Console: commandConsole(cmd), Agent: nsString(ns, "agent"),
		Source: nsString(ns, "source"), DryRun: nsBool(ns, "dry_run"), Overwrite: nsBool(ns, "overwrite"), Yes: nsBool(ns, "yes"),
		Sync: nsBool(ns, "sync"), FromSetup: nsBool(ns, "from_setup"), Preset: nsString(ns, "preset"),
		MigrateSecrets: nsBool(ns, "migrate_secrets"), NoBackup: nsBool(ns, "no_backup"), WorkspaceTarget: nsString(ns, "workspace_target"),
		SkillConflict: nsString(ns, "skill_conflict"), ActivateRoutines: nsBool(ns, "activate_routines"), Bot: bots,
		Include: include, Exclude: exclude, OutputDir: nsString(ns, "output_dir"), JSON: nsBool(ns, "json")})
}

// Runs icebath claw with the subcommand word under claw_action
func clawGroup(ctx context.Context, cli *commands.CLI, cmd *cobra.Command, path string, args []string) (int, error) {
	ns := namespace(cli, cmd, path, args)
	include, _ := ns["include"].([]string)
	exclude, _ := ns["exclude"].([]string)
	return agentimport.Claw(ctx, agentimport.ClawArgs{Console: commandConsole(cmd), Action: nsString(ns, "claw_action"),
		Source: nsString(ns, "source"), DryRun: nsBool(ns, "dry_run"), Overwrite: nsBool(ns, "overwrite"), Yes: nsBool(ns, "yes"),
		Preset: nsString(ns, "preset"), MigrateSecrets: nsBool(ns, "migrate_secrets"), NoBackup: nsBool(ns, "no_backup"),
		WorkspaceTarget: nsString(ns, "workspace_target"), SkillConflict: nsString(ns, "skill_conflict"), FromSetup: nsBool(ns, "from_setup"),
		Include: include, Exclude: exclude, OutputDir: nsString(ns, "output_dir"), JSON: nsBool(ns, "json")})
}

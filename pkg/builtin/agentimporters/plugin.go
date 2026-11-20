// Package agentimporters is the agent-importers catalog plugin port, the import-agent and claw commands and the openclaw-migration skill
package agentimporters

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/agentimport"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	importHelp        = "Import an OpenClaw, Claude Code, Codex CLI, Cursor, Grok Bot or OpenBot setup into Icebath"
	importDescription = "One-command import of another agent's setup into Icebath. Maps CLAUDE.md/AGENTS.md instructions, " +
		"Cursor rules, permission allowlists, MCP servers, skills, and memories into their Icebath equivalents; Grok Bot and " +
		"OpenBot routines become cron jobs. Always shows a preview before making changes. API keys and credentials are never " +
		"imported — start 'icebath' and type '/setup model' for those. OpenClaw (the former 'icebath claw migrate') imports " +
		"allowlisted secrets only with --migrate-secrets."
	clawHelp        = "OpenClaw migration tools"
	clawDescription = "Migrate settings, memories, skills, and API keys from OpenClaw to Icebath " +
		"('icebath claw migrate' is the same as 'icebath import-agent openclaw')"
	skillDescription = "Guided OpenClaw → Icebath migration (memories, skills, settings; secrets only on request)."
)

func init() { plugins.RegisterPort(Plugin{}) }

// Agent-importers catalog plugin port, active once icebath plugins install agent-importers lands it in a home
type Plugin struct{}

// Manifest of the agent-importers catalog plugin at its compiled-in pin
func (Plugin) Manifest() plugins.Manifest { return plugins.CatalogPortManifest("agent-importers") }

// Registers icebath import-agent, icebath claw and the openclaw-migration skill the installed tree ships
func (Plugin) Register(ctx *plugins.Context) error {
	ctx.RegisterCLICommand("import-agent", importHelp, func(p any) { commands.UsePluginParser(p, "import-agent") }, handleImport, importDescription)
	ctx.RegisterCLICommand("claw", clawHelp, func(p any) { commands.UsePluginParser(p, "claw") }, handleClaw, clawDescription)
	skill := filepath.Join(ctx.PluginDir(), "skills", "openclaw-migration", "SKILL.md")
	_, err := ctx.RegisterSkill("openclaw-migration", skill, skillDescription, nil)
	return err
}

// Text value of a parsed flag, empty when unset
func nsString(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Boolean value of a parsed flag
func nsBool(ns map[string]any, key string) bool {
	b, _ := ns[key].(bool)
	return b
}

// Exit status of a command run as the error the host maps to the process status
func exitStatus(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return &commands.ExitError{Code: code}
	}
	return nil
}

// Runs icebath import-agent with the parsed namespace
func handleImport(args any) error {
	ns, _ := args.(map[string]any)
	bots, _ := ns["bot"].([]string)
	return exitStatus(agentimport.ImportAgent(context.Background(), agentimport.Args{Console: cliui.Default(), Agent: nsString(ns, "agent"),
		Source: nsString(ns, "source"), DryRun: nsBool(ns, "dry_run"), Overwrite: nsBool(ns, "overwrite"), Yes: nsBool(ns, "yes"),
		Sync: nsBool(ns, "sync"), FromSetup: nsBool(ns, "from_setup"), Preset: nsString(ns, "preset"),
		MigrateSecrets: nsBool(ns, "migrate_secrets"), NoBackup: nsBool(ns, "no_backup"), WorkspaceTarget: nsString(ns, "workspace_target"),
		SkillConflict: nsString(ns, "skill_conflict"), ActivateRoutines: nsBool(ns, "activate_routines"), Bot: bots}))
}

// Runs icebath claw with the subcommand word under claw_action
func handleClaw(args any) error {
	ns, _ := args.(map[string]any)
	return exitStatus(agentimport.Claw(context.Background(), agentimport.ClawArgs{Console: cliui.Default(), Action: nsString(ns, "claw_action"),
		Source: nsString(ns, "source"), DryRun: nsBool(ns, "dry_run"), Overwrite: nsBool(ns, "overwrite"), Yes: nsBool(ns, "yes"),
		Preset: nsString(ns, "preset"), MigrateSecrets: nsBool(ns, "migrate_secrets"), NoBackup: nsBool(ns, "no_backup"),
		WorkspaceTarget: nsString(ns, "workspace_target"), SkillConflict: nsString(ns, "skill_conflict")}))
}

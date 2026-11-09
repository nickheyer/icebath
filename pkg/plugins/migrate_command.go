package plugins

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// MigrateCommand dispatches icebath migrate, hermes_cli/migrate.py cmd_migrate with the xai, relay and agent subtypes
func MigrateCommand(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (code int, err error) {
	restore := provider.WithSetupOutput(w)
	defer restore()
	defer provider.RecoverSetupExit(&code)
	migrateType := ""
	if pyrepr.Truthy(ns["migrate_type"]) {
		migrateType = pyrepr.Str(ns["migrate_type"])
	}
	switch {
	case migrateType == "xai":
		return cmdMigrateXai(ctx, h, ns, w, errw)
	case migrateType == "relay":
		if err := CmdMigrateRelay(ctx, w, nsVerifyBool(ns, "all_profiles"), nsVerifyBool(ns, "no_validate")); err != nil {
			return 1, err
		}
		return 0, nil
	case migrateType == "":
		return MigrateFromAgents(ctx, h, "", nil)
	case IsImportSourceName(migrateType):
		return cmdMigrateAgent(ctx, h, ns)
	}
	fmt.Fprintln(errw, "usage: icebath migrate [<agent> | xai [--apply] [--no-backup]]")
	return 2, nil
}

// Flags of icebath migrate <agent> forwarded to icebath import-agent <agent>, as dest and flag
var agentImportFlags = [][2]string{{"dry_run", "--dry-run"}, {"overwrite", "--overwrite"}, {"yes", "--yes"},
	{"activate_routines", "--activate-routines"}, {"migrate_secrets", "--migrate-secrets"}}

// Installs the agent-importers plugin when missing, then runs its import for the named agent with the flags given
func cmdMigrateAgent(ctx context.Context, h string, ns map[string]any) (int, error) {
	var extra []string
	for _, pair := range agentImportFlags {
		if nsVerifyBool(ns, pair[0]) {
			extra = append(extra, pair[1])
		}
	}
	if pyrepr.Truthy(ns["source"]) {
		extra = append(extra, "--source", expandUserPath(pyrepr.Str(ns["source"])))
	}
	for _, name := range nsVerifyStrings(ns, "bot") {
		extra = append(extra, "--bot", name)
	}
	agent := pyrepr.Str(ns["migrate_agent"])
	if !pyrepr.Truthy(ns["migrate_agent"]) {
		agent = pyrepr.Str(ns["migrate_type"])
	}
	return MigrateFromAgents(ctx, h, agent, extra)
}

// Prints a red cross failure line on stderr and returns exit 1
func migrateFail(errw io.Writer, message string) (int, error) {
	fmt.Fprintf(errw, "  %s %s\n", cliui.Color("✗", cliui.Red), message)
	return 1, nil
}

// Runs the xAI May 15 model migration in dry run or apply mode
func cmdMigrateXai(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error) {
	apply := nsVerifyBool(ns, "apply")
	noBackup := nsVerifyBool(ns, "no_backup")
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return 1, err
	}
	issues := provider.FindRetiredXAIRefs(map[string]any(tree))
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color(fmt.Sprintf("◆ xAI Model Retirement Migration (%s)", provider.XAIRetirementDate), cliui.Cyan, cliui.Bold))
	fmt.Fprintln(w)
	if len(issues) == 0 {
		fmt.Fprintf(w, "  %s No retired xAI models in config — nothing to migrate.\n", cliui.Color("✓", cliui.Green))
		return 0, nil
	}
	fmt.Fprintf(w, "  Found %d retired xAI model reference(s):\n", len(issues))
	fmt.Fprintln(w)
	for _, issue := range issues {
		fmt.Fprintf(w, "    %s %s\n", cliui.Color("⚠", cliui.Yellow), provider.FormatRetirementIssue(issue))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "    %s Migration guide: %s\n", cliui.Color("→", cliui.Cyan), provider.XAIMigrationGuideURL)
	fmt.Fprintln(w)
	configPath := filepath.Join(h, "config.yaml")
	if !apply {
		fmt.Fprintln(w, cliui.Color("Dry-run mode — no changes written.", cliui.Dim))
		fmt.Fprintln(w, cliui.Color(fmt.Sprintf("Re-run with `icebath migrate xai --apply` to rewrite %s in-place (backup created automatically).", configPath), cliui.Dim))
		return 0, nil
	}
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return migrateFail(errw, fmt.Sprintf("Could not locate config.yaml (looked at: %s)", configPath))
	}
	result, err := provider.ApplyXAIRetirementMigration(ctx, configPath, issues, !noBackup)
	if err != nil {
		return migrateFail(errw, fmt.Sprintf("Migration failed: %s", pyrepr.ExceptionMessage(err)))
	}
	if !result.ConfigChanged {
		fmt.Fprintf(w, "  %s No changes written.\n", cliui.Color("⚠", cliui.Yellow))
		return 0, nil
	}
	if result.BackupPath != "" {
		fmt.Fprintf(w, "  %s Backup: %s\n", cliui.Color("✓", cliui.Green), result.BackupPath)
	}
	fmt.Fprintf(w, "  %s Updated %d slot(s) in %s\n", cliui.Color("✓", cliui.Green), len(result.IssuesResolved), result.FilePath)
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Run `icebath doctor` to confirm no retired xAI models remain.", cliui.Dim))
	return 0, nil
}

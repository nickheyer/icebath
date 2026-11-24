package provider

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Dispatches icebath migrate, with the xai and agent subtypes
func MigrateCommand(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (code int, err error) {
	restore := WithSetupOutput(w)
	defer restore()
	defer RecoverSetupExit(&code)
	migrateType := ""
	if anyutil.IsTrue(ns["migrate_type"]) {
		migrateType = anyutil.String(ns["migrate_type"])
	}
	switch {
	case migrateType == "xai":
		return cmdMigrateXai(ctx, h, ns, w, errw)
	case migrateType == "":
		return MigrateFromAgents(ctx, h, "", AgentImportOptions{})
	case IsImportSourceName(migrateType):
		return cmdMigrateAgent(ctx, h, ns)
	}
	fmt.Fprintln(errw, "usage: icebath migrate [<agent> | xai [--apply] [--no-backup]]")
	return 2, nil
}

// Runs the native import for the named agent with the icebath migrate <agent> flags
func cmdMigrateAgent(ctx context.Context, h string, ns map[string]any) (int, error) {
	opts := AgentImportOptions{DryRun: nsFlag(ns, "dry_run"), Overwrite: nsFlag(ns, "overwrite"), Yes: nsFlag(ns, "yes"),
		ActivateRoutines: nsFlag(ns, "activate_routines"), MigrateSecrets: nsFlag(ns, "migrate_secrets"), Bot: nsStringsOf(ns, "bot")}
	if anyutil.IsTrue(ns["source"]) {
		opts.Source = expandUserPath(anyutil.String(ns["source"]))
	}
	agent := anyutil.String(ns["migrate_agent"])
	if !anyutil.IsTrue(ns["migrate_agent"]) {
		agent = anyutil.String(ns["migrate_type"])
	}
	return MigrateFromAgents(ctx, h, agent, opts)
}

// Prints a red cross failure line on stderr and returns exit 1
func migrateFail(errw io.Writer, message string) (int, error) {
	fmt.Fprintf(errw, "  %s %s\n", cliui.Color("✗", cliui.Red), message)
	return 1, nil
}

// Runs the xAI May 15 model migration in dry run or apply mode
func cmdMigrateXai(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error) {
	apply := nsFlag(ns, "apply")
	noBackup := nsFlag(ns, "no_backup")
	stored, err := config.ReadRawConfigReadonly(ctx, h)
	if err != nil {
		return 1, err
	}
	if failed := config.FailedReadError(stored); failed != nil {
		return migrateFail(errw, fmt.Sprintf("Could not read config.yaml: %s", failed))
	}
	issues := FindRetiredXAIRefs(stored)
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color(fmt.Sprintf("◆ xAI Model Retirement Migration (%s)", XAIRetirementDate), cliui.Cyan, cliui.Bold))
	fmt.Fprintln(w)
	if len(issues) == 0 {
		fmt.Fprintf(w, "  %s No retired xAI models in config — nothing to migrate.\n", cliui.Color("✓", cliui.Green))
		return 0, nil
	}
	fmt.Fprintf(w, "  Found %d retired xAI model reference(s):\n", len(issues))
	fmt.Fprintln(w)
	for _, issue := range issues {
		fmt.Fprintf(w, "    %s %s\n", cliui.Color("⚠", cliui.Yellow), FormatRetirementIssue(issue))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "    %s Migration guide: %s\n", cliui.Color("→", cliui.Cyan), XAIMigrationGuideURL)
	fmt.Fprintln(w)
	configPath := migrateResolveConfigPath(h)
	if !apply {
		fmt.Fprintln(w, cliui.Color("Dry-run mode — no changes written.", cliui.Dim))
		fmt.Fprintln(w, cliui.Color(fmt.Sprintf("Re-run with `icebath migrate xai --apply` to rewrite %s in-place (backup created automatically).", configPath), cliui.Dim))
		return 0, nil
	}
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return migrateFail(errw, fmt.Sprintf("Could not locate config.yaml (looked at: %s)", configPath))
	}
	result, err := ApplyXAIRetirementMigration(ctx, configPath, issues, !noBackup)
	if err != nil {
		return migrateFail(errw, fmt.Sprintf("Migration failed: %s", err.Error()))
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

// Location of the active config.yaml on disk under the given home
func migrateResolveConfigPath(h string) string {
	return filepath.Join(h, "config.yaml")
}

// String list of a namespace value, a Go or decoded list
func nsStringsOf(ns map[string]any, key string) []string {
	switch v := ns[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, anyutil.String(item))
		}
		return out
	}
	return nil
}

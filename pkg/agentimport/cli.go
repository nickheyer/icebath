package agentimport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	agentOpenClaw = "openclaw"
	agentGrokBot  = "grok-bot"
	agentOpenBot  = "openbot"
)

// AgentChoices are the agent names import-agent accepts in command line order
var AgentChoices = []string{agentOpenClaw, "claude-code", "codex", "cursor", agentGrokBot, agentOpenBot}

// Args are the import-agent flags as the command line parsed them, empty meaning not passed
type Args struct {
	Console          *cliui.Console
	Agent            string
	Source           string
	DryRun           bool
	Overwrite        bool
	Yes              bool
	Sync             bool
	FromSetup        bool
	Preset           string
	MigrateSecrets   bool
	NoBackup         bool
	WorkspaceTarget  string
	SkillConflict    string
	ActivateRoutines bool
	Bot              []string
	Include          []string
	Exclude          []string
	OutputDir        string
	JSON             bool
	pastedText       *string
}

// ClawArgs are the flags of icebath claw with the subcommand word under Action
type ClawArgs struct {
	Console         *cliui.Console
	Action          string
	Source          string
	DryRun          bool
	Overwrite       bool
	Yes             bool
	Preset          string
	MigrateSecrets  bool
	NoBackup        bool
	WorkspaceTarget string
	SkillConflict   string
	FromSetup       bool
	Include         []string
	Exclude         []string
	OutputDir       string
	JSON            bool
}

// Option ids of repeatable comma separated flag values, trimmed with empty ids dropped
func splitOptionIDs(values []string) []string {
	var out []string
	for _, v := range values {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// Default dirs every importer looks in, as the help and errors list them
func dirsHint() string {
	dirs := []string{"~/.openclaw"}
	for _, a := range SupportedAgents {
		dirs = append(dirs, "~/"+AgentDefaultDirs[a])
	}
	return strings.Join(dirs[:len(dirs)-1], ", ") + " or " + dirs[len(dirs)-1]
}

// ANSI codes of the report and banner colors
const (
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiRed     = "\033[31m"
	ansiDim     = "\033[2m"
	ansiMagenta = "\033[35m"
)

// Colored text when the console output is a terminal and NO_COLOR is empty
func color(c *cliui.Console, text, code string) string {
	if c.StdoutIsTTY() && os.Getenv("NO_COLOR") == "" {
		return code + text + "\033[0m"
	}
	return text
}

// Agent named on the command line, else the one setup detected, empty with the reason printed otherwise
func resolveAgent(args *Args) (string, error) {
	c := args.Console
	if args.Agent != "" {
		return args.Agent, nil
	}
	oc, err := openClaw()
	if err != nil {
		return "", err
	}
	var detected []string
	if oc.FindDirs != nil && len(oc.FindDirs()) > 0 {
		detected = append(detected, agentOpenClaw)
	}
	detected = append(detected, DetectAgents()...)
	if DetectGrokBot() != "" {
		detected = append(detected, agentGrokBot)
	}
	if FindOpenBotUserData("") != "" {
		detected = append(detected, agentOpenBot)
	}
	if len(detected) == 0 {
		fmt.Fprintln(c.Out)
		c.PrintError(fmt.Sprintf("No supported agent setup found (%s).", dirsHint()))
		c.PrintInfo("Specify one explicitly: icebath import-agent claude-code --source /path")
		return "", nil
	}
	if len(detected) > 1 && args.Source == "" {
		fmt.Fprintln(c.Out)
		c.PrintInfo("Multiple agent setups detected: " + strings.Join(detected, ", "))
		picks := make([]string, len(detected))
		for i, a := range detected {
			picks[i] = "icebath import-agent " + a
		}
		c.PrintInfo("Pick one: " + strings.Join(picks, "   or   "))
		return "", nil
	}
	return detected[0], nil
}

// Option strings of the flags of a group that were passed
func openClawFlagsPassed(a *Args) []string {
	var out []string
	if a.Preset != "" {
		out = append(out, "--preset")
	}
	if a.MigrateSecrets {
		out = append(out, "--migrate-secrets")
	}
	if a.NoBackup {
		out = append(out, "--no-backup")
	}
	if a.WorkspaceTarget != "" {
		out = append(out, "--workspace-target")
	}
	if a.SkillConflict != "" {
		out = append(out, "--skill-conflict")
	}
	if a.Include != nil {
		out = append(out, "--include")
	}
	if a.Exclude != nil {
		out = append(out, "--exclude")
	}
	if a.OutputDir != "" {
		out = append(out, "--output-dir")
	}
	if a.JSON {
		out = append(out, "--json")
	}
	return out
}

func routineFlagsPassed(a *Args) []string {
	var out []string
	if a.ActivateRoutines {
		out = append(out, "--activate-routines")
	}
	if a.Bot != nil {
		out = append(out, "--bot")
	}
	return out
}

// Runs the registered OpenClaw migration with the import-agent flags
func runOpenClawMigrate(ctx context.Context, a OpenClawArgs) (int, error) {
	oc, err := openClaw()
	if err != nil {
		return 1, err
	}
	if oc.Migrate == nil {
		return 1, ErrOpenClawMissing
	}
	return oc.Migrate(ctx, a)
}

// ImportAgent handles icebath import-agent, preview first, then confirm and apply
func ImportAgent(ctx context.Context, args Args) (int, error) {
	if args.Console == nil {
		args.Console = cliui.Default()
	}
	c := args.Console
	if args.Sync {
		return 0, syncImportedAgents(ctx, args)
	}
	agent, err := resolveAgent(&args)
	if err != nil {
		return 1, err
	}
	if agent == "" {
		return 0, nil
	}
	if agent == agentOpenClaw {
		return runOpenClawMigrate(ctx, OpenClawArgs{Console: c, Source: args.Source, DryRun: args.DryRun, Overwrite: args.Overwrite,
			Yes: args.Yes, Preset: args.Preset, MigrateSecrets: args.MigrateSecrets, NoBackup: args.NoBackup,
			WorkspaceTarget: args.WorkspaceTarget, SkillConflict: args.SkillConflict, FromSetup: args.FromSetup,
			Include: splitOptionIDs(args.Include), Exclude: splitOptionIDs(args.Exclude), OutputDir: args.OutputDir, JSON: args.JSON})
	}
	for _, group := range []struct {
		owners []string
		label  string
		passed []string
	}{
		{[]string{agentOpenClaw}, "OpenClaw", openClawFlagsPassed(&args)},
		{[]string{agentGrokBot, agentOpenBot}, "Grok Bot and OpenBot", routineFlagsPassed(&args)},
	} {
		owned := false
		for _, o := range group.owners {
			owned = owned || o == agent
		}
		if !owned && len(group.passed) > 0 {
			fmt.Fprintln(c.Out)
			c.PrintError(fmt.Sprintf("%s only apply to %s imports; nothing was imported.", strings.Join(group.passed, ", "), group.label))
			c.PrintInfo(fmt.Sprintf("Use them with: icebath import-agent %s %s ...", group.owners[0], strings.Join(group.passed, " ")))
			return 0, nil
		}
	}
	explicitSource, overwrite, fromSetup := args.Source, args.Overwrite, args.FromSetup
	icebathHome := home.Dir(ctx)
	target, err := pypath.Resolve(icebathHome)
	if err != nil {
		target = icebathHome
	}
	botOpts := BotOptions{Overwrite: overwrite, ActivateRoutines: args.ActivateRoutines, BotNames: args.Bot}
	var makeImporter func(execute bool) (*Report, error)
	var sourceLabel, sourceDir string
	switch agent {
	case agentGrokBot:
		printBanner(c)
		args.pastedText = nil
		files, code, err := resolveExportFiles(ctx, &args)
		if err != nil || code != 0 {
			return code, err
		}
		if len(files) == 0 {
			return 0, nil
		}
		pasted := args.pastedText
		if pasted == nil && len(files) == 1 && files[0] == Pasted {
			text, err := readPastedExport(c)
			if err != nil {
				return 1, err
			}
			pasted = &text
		}
		sourceLabel = strings.Join(files, ", ")
		makeImporter = func(execute bool) (*Report, error) {
			o := botOpts
			o.Execute = execute
			return NewGrokBotImporter(ctx, files, target, o, pasted).Run()
		}
	case agentOpenBot:
		printBanner(c)
		userData := FindOpenBotUserData(explicitSource)
		if userData == "" {
			fmt.Fprintln(c.Out)
			where := ""
			if explicitSource != "" {
				where = " at " + explicitSource
			}
			c.PrintError("OpenBot's data folder (with openbot.db) was not found" + where + ".")
			c.PrintInfo("Point at it: icebath import-agent openbot --source '<OpenBot user data folder>'")
			return 1, nil
		}
		sourceLabel = userData
		makeImporter = func(execute bool) (*Report, error) {
			o := botOpts
			o.Execute = execute
			return NewOpenBotImporter(ctx, userData, target, o).Run()
		}
	default:
		if _, ok := AgentDefaultDirs[agent]; !ok {
			return 2, fmt.Errorf("argument agent: invalid choice: '%s' (choose from %s)", agent, quotedChoices())
		}
		sourceDir = explicitSource
		if sourceDir == "" {
			sourceDir = filepath.Join(userHome(), AgentDefaultDirs[agent])
		}
		printBanner(c)
		if !isDir(sourceDir) {
			fmt.Fprintln(c.Out)
			c.PrintError("Agent directory not found: " + sourceDir)
			c.PrintInfo(fmt.Sprintf("Specify a custom path: icebath import-agent %s --source /path/to/%s", agent, AgentDefaultDirs[agent]))
			return 1, nil
		}
		sourceLabel = sourceDir
		resolvedSource, err := pypath.Resolve(sourceDir)
		if err != nil {
			resolvedSource = sourceDir
		}
		sourceDir = resolvedSource
		makeImporter = func(execute bool) (*Report, error) {
			imp, err := NewAgentImporter(ctx, agent, resolvedSource, target, execute, overwrite, nil)
			if err != nil {
				return nil, err
			}
			return imp.Run()
		}
	}

	fmt.Fprintln(c.Out)
	c.PrintHeader("Import Settings")
	c.PrintInfo("Agent:       " + agent)
	c.PrintInfo("Source:      " + sourceLabel)
	c.PrintInfo("Target:      " + icebathHome)
	if overwrite {
		c.PrintInfo("Overwrite:   yes")
	} else {
		c.PrintInfo("Overwrite:   no (skip conflicts)")
	}
	if fromSetup {
		c.PrintInfo("Secrets:     never imported")
	} else {
		c.PrintInfo("Secrets:     never imported — start 'icebath' and type '/setup model' for credentials")
	}
	if err := ensureConfigFile(icebathHome); err != nil {
		return 1, err
	}

	runImport := func(execute bool, phase string) *Report {
		report, err := makeImporter(execute)
		if err != nil {
			fmt.Fprintln(c.Out)
			c.PrintError(fmt.Sprintf("Import%s failed: %s", phase, pyErrText(err)))
			slog.Debug("import-agent"+phase+" error", "error", err)
			return nil
		}
		return report
	}

	preview := runImport(false, " preview")
	if preview == nil {
		return 1, nil
	}
	if preview.Summary["imported"] == 0 && preview.Summary["conflict"] == 0 {
		fmt.Fprintln(c.Out)
		c.PrintInfo(fmt.Sprintf("Nothing to import from %s.", agent))
		printImportReport(ctx, c, preview, true, fromSetup)
		if preview.Summary["error"] > 0 {
			return 1, nil
		}
		return 0, nil
	}
	fmt.Fprintln(c.Out)
	c.PrintHeader(fmt.Sprintf("Import Preview — %d item(s) would be imported", preview.Summary["imported"]))
	c.PrintInfo("No changes have been made yet. Review the list below:")
	printImportReport(ctx, c, preview, true, fromSetup)
	if args.DryRun {
		return 0, nil
	}

	fmt.Fprintln(c.Out)
	if !args.Yes {
		if !c.StdinIsTTY() {
			c.PrintInfo("Non-interactive session — preview only.")
			again := ""
			if explicitSource != "" {
				again = " --source " + pyshlex.Quote(explicitSource)
			}
			piped := ""
			if explicitSource == "-" {
				piped = " (piping the reply in again)"
			}
			c.PrintInfo(fmt.Sprintf("To execute, re-run with: icebath import-agent %s%s --yes%s", agent, again, piped))
			return 0, nil
		}
		if !c.PromptYesNo(ctx, "Proceed with import?", true) {
			c.PrintInfo("Import cancelled.")
			return 0, nil
		}
	}
	report := runImport(true, "")
	if report == nil {
		return 1, nil
	}
	printImportReport(ctx, c, report, false, fromSetup)
	if agent == agentGrokBot || agent == agentOpenBot {
		product := "OpenBot"
		if agent == agentGrokBot {
			product = "Grok Bot"
		}
		printRoutineNextSteps(c, report, product)
	} else {
		registerForSync(ctx, c, agent, sourceDir, target, overwrite, report)
	}
	fmt.Fprintln(c.Out)
	if errs := report.Summary["error"]; errs > 0 {
		c.PrintError(fmt.Sprintf("Import finished with %d error(s); see the list above.", errs))
	} else {
		c.PrintSuccess("Import complete.")
	}
	if fromSetup {
		c.PrintInfo("API keys and credentials were NOT imported.")
	} else {
		c.PrintInfo("API keys and credentials were NOT imported — start 'icebath' and type '/setup model' " +
			"to configure providers, or add them to " + home.Display(icebathHome) + "/.env.")
	}
	if report.Summary["error"] > 0 {
		return 1, nil
	}
	return 0, nil
}

// Choices of the agent positional as argparse quotes them
func quotedChoices() string {
	q := make([]string, len(AgentChoices))
	for i, a := range AgentChoices {
		q[i] = "'" + a + "'"
	}
	return strings.Join(q, ", ")
}

// Writes config.yaml from the loaded defaults when the home has none, so the import can merge into it
func ensureConfigFile(icebathHome string) error {
	path := config.GetConfigPath(icebathHome)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tree, err := config.Load(icebathHome, nil)
	if err != nil {
		return err
	}
	return config.Save(icebathHome, tree)
}

// Prints the import banner, none inside a setup presentation whose own header already names the import
func printBanner(c *cliui.Console) {
	if c.IsFlow() {
		return
	}
	fmt.Fprintln(c.Out)
	fmt.Fprintln(c.Out, color(c, "┌─────────────────────────────────────────────────────────┐", ansiMagenta))
	fmt.Fprintln(c.Out, color(c, "│          ☤ Icebath — Import From Another Agent         │", ansiMagenta))
	fmt.Fprintln(c.Out, color(c, "└─────────────────────────────────────────────────────────┘", ansiMagenta))
}

// Says how to switch the routines over once the originals are off
func printRoutineNextSteps(c *cliui.Console, report *Report, product string) {
	var jobs, paused []Item
	for _, it := range report.Items {
		if it.Kind == "routine" && it.Status == "imported" {
			jobs = append(jobs, it)
			if p, _ := it.Details["paused"].(bool); p {
				paused = append(paused, it)
			}
		}
	}
	if len(paused) > 0 {
		fmt.Fprintln(c.Out)
		c.PrintInfo(fmt.Sprintf("%d routine(s) were created PAUSED so they don't run twice. Once you turn "+
			"the routine off in %s, resume its copy here:", len(paused), product))
		for _, it := range paused {
			c.PrintInfo(fmt.Sprintf("  icebath cron resume %s    # %s", pyStr(it.Details["job_id"]), it.Destination))
		}
	}
	if len(jobs) > 0 {
		c.PrintInfo("Results land in Icebath' cron output; to have one sent to Telegram, Discord, Slack, " +
			"...: icebath cron edit <id> --deliver <platform>")
	}
}

// Records the source in the sync manifest so a later --sync pulls its changes in
func registerForSync(ctx context.Context, c *cliui.Console, agent, sourceDir, target string, overwrite bool, report *Report) {
	if err := UpdateSyncManifest(ctx, agent, sourceDir, target, overwrite, report, true); err != nil {
		slog.Warn(fmt.Sprintf("Could not update import sync manifest: %s", pyErrText(err)))
		return
	}
	c.PrintInfo("Source registered for sync — re-run 'icebath import-agent --sync' any time to pull in changes.")
}

// Prints the per item report grouped by status with the stripped secrets and a summary
func printImportReport(ctx context.Context, c *cliui.Console, report *Report, dryRun, fromSetup bool) {
	fmt.Fprintln(c.Out)
	if dryRun {
		c.PrintHeader("Dry Run Results")
		c.PrintInfo("No files were modified. This is a preview of what would happen.")
	} else {
		c.PrintHeader("Import Results")
	}
	fmt.Fprintln(c.Out)
	type group struct{ status, code, label, summary string }
	groups := []group{
		{"imported", ansiGreen, "✓ Imported", "imported"},
		{"conflict", ansiYellow, "⚠ Conflicts (skipped — use --overwrite to force)", "conflict(s)"},
		{"skipped", ansiDim, "─ Skipped", "skipped"},
		{"error", ansiRed, "✗ Errors", "error(s)"},
	}
	if dryRun {
		groups[0].label, groups[0].summary = "✓ Would import", "would import"
	}
	userHomeDir := userHome()
	for _, g := range groups {
		var items []Item
		for _, it := range report.Items {
			if it.Status == g.status {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			continue
		}
		fmt.Fprintln(c.Out, color(c, "  "+g.label+":", g.code))
		for _, it := range items {
			var tail string
			if g.status == "imported" {
				dest := it.Destination
				if userHomeDir != "" {
					dest = strings.ReplaceAll(dest, userHomeDir, "~")
				}
				tail = "→ " + dest
			} else {
				tail = " " + it.Reason
			}
			fmt.Fprintf(c.Out, "      %s %s\n", pystr.Ljust(it.Kind, 22), tail)
		}
		fmt.Fprintln(c.Out)
	}
	if len(report.StrippedSecrets) > 0 {
		fmt.Fprintln(c.Out, color(c, "  ⚷ Secrets stripped (never imported):", ansiYellow))
		for _, name := range report.StrippedSecrets {
			fmt.Fprintf(c.Out, "      %s\n", name)
		}
		if fromSetup {
			c.PrintInfo("Re-add them deliberately in " + home.Display(home.Dir(ctx)) + "/.env.")
		} else {
			c.PrintInfo("Re-add credentials deliberately with '/setup model' in 'icebath' or in the profile's .env.")
		}
		fmt.Fprintln(c.Out)
	}
	var parts []string
	for _, g := range groups {
		if n := report.Summary[g.status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, g.summary))
		}
	}
	if len(parts) > 0 {
		c.PrintInfo("Summary: " + strings.Join(parts, ", "))
	}
}

// Claw handles icebath claw, migrate and cleanup going to the registered OpenClaw importer
func Claw(ctx context.Context, args ClawArgs) (int, error) {
	if args.Console == nil {
		args.Console = cliui.Default()
	}
	switch args.Action {
	case "migrate":
		return runOpenClawMigrate(ctx, OpenClawArgs{Console: args.Console, Source: args.Source, DryRun: args.DryRun,
			Overwrite: args.Overwrite, Yes: args.Yes, Preset: args.Preset, MigrateSecrets: args.MigrateSecrets,
			NoBackup: args.NoBackup, WorkspaceTarget: args.WorkspaceTarget, SkillConflict: args.SkillConflict, FromSetup: args.FromSetup,
			Include: splitOptionIDs(args.Include), Exclude: splitOptionIDs(args.Exclude), OutputDir: args.OutputDir, JSON: args.JSON})
	case "cleanup", "clean":
		oc, err := openClaw()
		if err != nil {
			return 1, err
		}
		if oc.Cleanup == nil {
			return 1, ErrOpenClawMissing
		}
		return oc.Cleanup(ctx, OpenClawCleanupArgs{Console: args.Console, Source: args.Source, DryRun: args.DryRun, Yes: args.Yes})
	}
	fmt.Fprintln(args.Console.Out, "Usage: icebath claw <command> [options]\n\nCommands:\n"+
		"  migrate          Migrate settings from OpenClaw to Icebath "+
		"(same as 'icebath import-agent openclaw')\n"+
		"  cleanup          Archive leftover OpenClaw directories after migration\n\n"+
		"Run 'icebath claw <command> --help' for options.")
	return 0, nil
}

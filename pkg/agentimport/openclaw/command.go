package openclaw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agentimport"
	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Current and legacy OpenClaw directory names under the user home
var openclawDirNames = []string{".openclaw", ".clawdbot", ".moltbot"}

// pgrep extended regex matching a node interpreter running an openclaw or clawd script
const openclawNodeCmdlineRe = `^(\S*/)?node(js)?\s.*(openclaw|clawd)`

// Workspace marker files and the labelled subset cleanup lists
var workspaceMarkers = []string{"todo.json", "SOUL.md", "MEMORY.md", "USER.md"}

// Installs the OpenClaw importer into pkg/agentimport
func init() {
	agentimport.RegisterOpenClaw(agentimport.OpenClawImporter{FindDirs: FindOpenClawDirs, Migrate: Migrate, Cleanup: Cleanup})
}

// Existing OpenClaw directories under the user home, current name first
func FindOpenClawDirs() []string {
	userHome, err := pypath.PathHome()
	if err != nil {
		panic(err)
	}
	var out []string
	for _, name := range openclawDirNames {
		if d := pypath.PathJoin(userHome, name); isDir(d) {
			out = append(out, d)
		}
	}
	return out
}

// Console of the args, the process console when none was handed over
func consoleOf(c *cliui.Console) *cliui.Console {
	if c == nil {
		return cliui.Default()
	}
	return c
}

// Prints a line to the console output
func outln(c *cliui.Console, text string) { fmt.Fprintln(c.Out, text) }

// Prints the magenta boxed banner of the claw subcommands
func printBanner(c *cliui.Console, title string) {
	outln(c, "")
	rule := strings.Repeat("─", 57)
	for _, line := range []string{"┌" + rule + "┐", "│          󰆧 Icebath — " + pystr.Ljust(title, 35) + "│", "└" + rule + "┘"} {
		outln(c, c.Color(line, cliui.Magenta))
	}
}

// Prints each line as info, an empty line as a bare blank line
func info(c *cliui.Console, lines ...string) {
	for _, line := range lines {
		if line == "" {
			outln(c, "")
		} else {
			c.PrintInfo(line)
		}
	}
}

// Prints a blank line, an error headline and the info lines
func errorBlock(c *cliui.Console, headline string, lines ...string) {
	outln(c, "")
	c.PrintError(headline)
	info(c, lines...)
}

// Answer of a confirmation, auto yes true, a non interactive stdin printing nonTTY and answering nil
func confirm(ctx context.Context, c *cliui.Console, autoYes bool, question string, def bool, declined string, nonTTY []string) *bool {
	yes := true
	if autoYes {
		return &yes
	}
	if nonTTY != nil && !c.StdinIsTTY() {
		info(c, nonTTY...)
		return nil
	}
	answer := c.PromptYesNo(ctx, question, def)
	if !answer {
		c.PrintInfo(declined)
	}
	return &answer
}

// Reports whether a confirmation answer is an explicit no
func isNo(answer *bool) bool { return answer != nil && !*answer }

// Prints the still running block with one bullet per process and the advice, then asks
func warnRunning(ctx context.Context, c *cliui.Console, autoYes bool, headline string, running []string, lines []string, question, declined string, nonTTY []string) *bool {
	block := make([]string, 0, len(running)+len(lines))
	for _, detail := range running {
		block = append(block, "  * "+detail)
	}
	errorBlock(c, headline, append(block, lines...)...)
	outln(c, "")
	return confirm(ctx, c, autoYes, question, false, declined, nonTTY)
}

// Output and exit status of one probe child
type probeResult struct {
	code   int
	stdout string
}

// Runs a probe child with a timeout, nil when the program is missing or the timeout expires
func posixProbe(argv []string, timeout time.Duration) (*probeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &probeResult{code: ee.ExitCode(), stdout: pystr.DecodeUTF8Replace(out.Bytes())}, nil
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &probeResult{code: 0, stdout: pystr.DecodeUTF8Replace(out.Bytes())}, nil
}

// Running OpenClaw processes and services, exact binary names and node processes running an openclaw script
func detectOpenClawProcesses() ([]string, error) {
	var found []string
	if runtime.GOOS == "windows" {
		for _, exe := range []string{"openclaw.exe", "clawd.exe"} {
			r := tools.BoundedProbeRun([]string{"tasklist", "/FI", "IMAGENAME eq " + exe}, 5*time.Second, nil, "", nil)
			if r != nil && strings.Contains(lower(pystr.DecodeUTF8Replace([]byte(r.Stdout))), exe) {
				found = append(found, "process: "+exe)
			}
		}
		psCmd := `Get-CimInstance Win32_Process -Filter "Name = 'node.exe'" | ` +
			`Where-Object { $_.CommandLine -match "openclaw|clawd" } | ` +
			`Select-Object -First 1 ProcessId`
		r := tools.BoundedProbeRun([]string{"powershell", "-NoProfile", "-Command", psCmd}, 5*time.Second, nil, "", nil)
		if r != nil {
			if pid := strip(pystr.DecodeUTF8Replace([]byte(r.Stdout))); pid != "" {
				found = append(found, "node.exe process with openclaw in command line (PID "+pid+")")
			}
		}
		return found, nil
	}
	r, err := posixProbe([]string{"systemctl", "--user", "is-active", "openclaw-gateway.service"}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if r != nil && strip(r.stdout) == "active" {
		found = append(found, "systemd service: openclaw-gateway.service")
	}
	var pids []string
	for _, probe := range [][]string{{"pgrep", "-x", "openclaw"}, {"pgrep", "-x", "openclaw-gatewa"}, {"pgrep", "-x", "clawd"}, {"pgrep", "-f", openclawNodeCmdlineRe}} {
		r, err := posixProbe(probe, 3*time.Second)
		if err != nil {
			return nil, err
		}
		if r != nil && r.code == 0 {
			pids = append(pids, pystr.SplitWhitespace(r.stdout, -1)...)
		}
	}
	if len(pids) > 0 {
		var unique []string
		seen := map[string]bool{}
		for _, p := range pids {
			if !seen[p] {
				seen[p] = true
				unique = append(unique, p)
			}
		}
		found = append(found, "openclaw process(es) (PIDs: "+strings.Join(unique, ", ")+")")
	}
	return found, nil
}

// Warns when OpenClaw still runs and reports whether the user declined, which ends the command with status 0
func warnIfOpenClawRunning(ctx context.Context, c *cliui.Console, autoYes bool) (bool, error) {
	running, err := detectOpenClawProcesses()
	if err != nil {
		return false, err
	}
	if len(running) == 0 {
		return false, nil
	}
	answer := warnRunning(ctx, c, autoYes, "OpenClaw appears to be running:", running,
		[]string{"Messaging platforms (Telegram, Discord, Slack) only allow one active session per bot token. If you continue, both OpenClaw and Icebath may try to use the same token, causing disconnects.",
			"Recommendation: stop OpenClaw before migrating."},
		"Continue anyway?", "Migration cancelled. Stop OpenClaw and try again.",
		[]string{"Non-interactive session — continuing to preview only."})
	return isNo(answer), nil
}

// Warns when the host gateway has connected platforms for this profile and reports whether the user declined
func warnIfGatewayRunning(ctx context.Context, c *cliui.Console, autoYes bool) (declined bool, err error) {
	defer catch(&err)
	liveness := gateway.ResolveGatewayLiveness(gateway.LivenessOptions{NoCache: true})
	var platforms any = pyjson.NewObject()
	if liveness.Running {
		profile := profiles.GetActiveProfileName(ctx)
		if liveness.Source == "multiplexer" && profile != "" && profile != "default" {
			platforms = gateway.ProfilePlatformsFromMultiplexer(liveness.Runtime, profile)
		} else {
			var runtimeRec any = pyjson.NewObject()
			if liveness.Runtime != nil && liveness.Runtime.Len() > 0 {
				runtimeRec = liveness.Runtime
			} else if rec := gateway.ReadRuntimeStatus(""); rec != nil && rec.Len() > 0 {
				runtimeRec = rec
			}
			platforms = or(dget(runtimeRec, "platforms", nil), pyjson.NewObject())
		}
	}
	var connected []string
	for _, e := range ditems(platforms) {
		if isDict(e.Val) && eqStr(dget(e.Val, "state", nil), "connected") {
			connected = append(connected, e.Key)
		}
	}
	if len(connected) == 0 {
		return false, nil
	}
	answer := warnRunning(ctx, c, autoYes, "Icebath gateway is running with active connections: "+strings.Join(connected, ", "), nil,
		[]string{"Migrating bot tokens while the gateway is active will cause conflicts (Telegram, Discord, and Slack only allow one active session per token).",
			"Recommendation: stop the host gateway first with 'icebath gateway stop'."},
		"Continue anyway?", "Migration cancelled. Stop the gateway and try again.", nil)
	return isNo(answer), nil
}

// Settings of one migrate run with the claw defaults filled in
type migrateOptions struct {
	agentimport.OpenClawArgs
	sourceDir   string
	icebathHome string
}

// Comma joined selection values as the dry run hint repeats them
func selectionText(values []string) string { return strings.Join(ParseSelectionValues(values), ",") }

// Runs the OpenClaw migration, preflight then preview then confirm then backup then apply, returning the exit status
func Migrate(ctx context.Context, args agentimport.OpenClawArgs) (int, error) {
	c := consoleOf(args.Console)
	opts := migrateOptions{OpenClawArgs: args}
	if opts.Preset == "" && len(opts.Include) == 0 {
		opts.Preset = defaultPresetName
	}
	if opts.SkillConflict == "" {
		opts.SkillConflict = defaultSkillConflictMode
	}
	if opts.Source != "" {
		opts.sourceDir = pypath.PathStr(opts.Source)
	} else if dirs := FindOpenClawDirs(); len(dirs) > 0 {
		opts.sourceDir = dirs[0]
	} else {
		userHome, err := pypath.PathHome()
		if err != nil {
			return 1, err
		}
		opts.sourceDir = pypath.PathJoin(userHome, ".openclaw")
	}
	if opts.JSON {
		return migrateJSON(ctx, c, opts)
	}
	printBanner(c, "OpenClaw Migration")
	if !isDir(opts.sourceDir) {
		errorBlock(c, "OpenClaw directory not found: "+opts.sourceDir,
			"Make sure your OpenClaw installation is at the expected path.",
			"You can specify a custom path: icebath import-agent openclaw --source /path/to/.openclaw")
		return 0, nil
	}
	opts.icebathHome = pypath.PathStr(home.Dir(ctx))
	outln(c, "")
	c.PrintHeader("Migration Settings")
	overwrite, secrets := "no (skip conflicts)", "no"
	if opts.Overwrite {
		overwrite = "yes"
	}
	if opts.MigrateSecrets {
		secrets = "yes (allowlisted only)"
	}
	preset := opts.Preset
	if preset == "" {
		preset = "None"
	}
	settings := []string{"Source:      " + opts.sourceDir, "Target:      " + opts.icebathHome, "Preset:      " + preset}
	if len(opts.Include) > 0 {
		settings = append(settings, "Include:     "+strings.Join(ParseSelectionValues(opts.Include), ", "))
	}
	if len(opts.Exclude) > 0 {
		settings = append(settings, "Exclude:     "+strings.Join(ParseSelectionValues(opts.Exclude), ", "))
	}
	settings = append(settings, "Overwrite:   "+overwrite, "Secrets:     "+secrets)
	if opts.SkillConflict != defaultSkillConflictMode {
		settings = append(settings, "Skill conflicts: "+opts.SkillConflict)
	}
	if opts.WorkspaceTarget != "" {
		settings = append(settings, "Workspace:   "+opts.WorkspaceTarget)
	}
	if opts.OutputDir != "" {
		settings = append(settings, "Report dir:  "+opts.OutputDir)
	}
	info(c, settings...)
	outln(c, "")
	if declined, err := warnIfOpenClawRunning(ctx, c, opts.Yes); err != nil || declined {
		return exitStatus(err)
	}
	if declined, err := warnIfGatewayRunning(ctx, c, opts.Yes); err != nil || declined {
		return exitStatus(err)
	}
	if err := ensureConfigFile(ctx, opts.icebathHome); err != nil {
		return 1, err
	}
	selected, err := ResolveSelectedOptions(opts.Include, opts.Exclude, opts.Preset)
	if err != nil {
		errorBlock(c, excText(err))
		return selectionErrorStatus, nil
	}
	run, err := loadMigrator(opts, selected)
	if err != nil {
		return 1, err
	}
	if proceed, err := previewMigration(ctx, c, run, opts); err != nil || !proceed {
		return exitStatus(err)
	}
	outln(c, "")
	if answer := confirm(ctx, c, opts.Yes, "Proceed with migration?", true, "Migration cancelled.",
		[]string{"Non-interactive session — preview only.", "To execute, re-run with: icebath import-agent openclaw --yes"}); answer != nil && *answer {
		return applyMigration(ctx, c, run, opts)
	}
	return 0, nil
}

// Exit status of an unknown preset or option id, the status the migration script returns for it
const selectionErrorStatus = 2

// Writes config.yaml from the effective defaults when the home has none yet
func ensureConfigFile(ctx context.Context, h string) error {
	if exists(config.GetConfigPath(h)) {
		return nil
	}
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	return config.SaveConfig(ctx, h, map[string]any(cfg), config.SaveOptions{})
}

// Prints the redacted report as two space indented JSON
func printReportJSON(c *cliui.Console, report *Report) error {
	redacted, err := RedactMigrationValue(report.JSON())
	if err != nil {
		return err
	}
	text, err := pyjson.Dumps(redacted, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	outln(c, text)
	return nil
}

// Prints an error as the JSON object the migration script prints for it
func printJSONError(c *cliui.Console, msg string) error {
	text, err := pyjson.Dumps(pyjson.ObjectOf("error", msg), pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	outln(c, text)
	return nil
}

// Runs one migration as the script main does, printing only JSON, applying with --yes whatever conflicts the plan has
func migrateJSON(ctx context.Context, c *cliui.Console, opts migrateOptions) (int, error) {
	selected, err := ResolveSelectedOptions(opts.Include, opts.Exclude, opts.Preset)
	if err != nil {
		return selectionErrorStatus, printJSONError(c, excText(err))
	}
	opts.icebathHome = pypath.PathStr(home.Dir(ctx))
	if isDir(opts.sourceDir) {
		if err := ensureConfigFile(ctx, opts.icebathHome); err != nil {
			return 1, err
		}
	}
	run, err := loadMigrator(opts, selected)
	if err != nil {
		return 1, err
	}
	execute := opts.Yes && !opts.DryRun
	if !execute {
		report, err := run(false)
		if err != nil {
			return 1, printJSONError(c, "Migration preview failed: "+excText(err))
		}
		return exitStatus(printReportJSON(c, report))
	}
	if !opts.NoBackup && isDir(opts.sourceDir) {
		if _, err := backup.CreatePreMigrationBackup(opts.icebathHome, backup.PreMigrationDefaultKeep); err != nil {
			return 1, printJSONError(c, "Could not create pre-migration backup: "+excText(err))
		}
	}
	report, err := run(true)
	if err != nil {
		return 1, printJSONError(c, "Migration failed: "+excText(err))
	}
	return exitStatus(printReportJSON(c, report))
}

// Exit status of a command that stopped early, 1 with the error when one was raised and 0 otherwise
func exitStatus(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Migration runner for preview and apply over the resolved selection, the workspace target and report directory resolved once
func loadMigrator(opts migrateOptions, selected map[string]bool) (func(execute bool) (*Report, error), error) {
	var err error
	wsTarget := ""
	if opts.WorkspaceTarget != "" {
		if wsTarget, err = pypath.Resolve(opts.WorkspaceTarget); err != nil {
			return nil, err
		}
	}
	outputDir := ""
	if opts.OutputDir != "" {
		if outputDir, err = pypath.Resolve(pypath.Expanduser(opts.OutputDir)); err != nil {
			return nil, err
		}
	}
	return func(execute bool) (*Report, error) {
		sourceRoot, err := pypath.Resolve(opts.sourceDir)
		if err != nil {
			return nil, err
		}
		targetRoot, err := pypath.Resolve(opts.icebathHome)
		if err != nil {
			return nil, err
		}
		m, err := NewMigrator(MigratorOptions{
			SourceRoot: sourceRoot, TargetRoot: targetRoot, Execute: execute, WorkspaceTarget: wsTarget,
			Overwrite: opts.Overwrite, MigrateSecrets: opts.MigrateSecrets, OutputDir: outputDir, SelectedOptions: selected,
			PresetName: opts.Preset, SkillConflictMode: opts.SkillConflict,
		})
		if err != nil {
			return nil, err
		}
		return m.Migrate()
	}, nil
}

// Previews the plan and reports whether the run should go on to apply
func previewMigration(ctx context.Context, c *cliui.Console, run func(bool) (*Report, error), opts migrateOptions) (bool, error) {
	report, err := run(false)
	if err != nil {
		errorBlock(c, "Migration preview failed: "+excText(err))
		return false, nil
	}
	count, conflicts := report.Summary.Get(StatusMigrated), report.Summary.Get(StatusConflict)
	outln(c, "")
	if count == 0 && conflicts == 0 {
		c.PrintInfo("Nothing to migrate from OpenClaw.")
	} else {
		what := fmt.Sprintf("%d conflict(s), nothing would be imported", conflicts)
		if count > 0 {
			what = fmt.Sprintf("%d item(s) would be imported", count)
		}
		c.PrintHeader("Migration Preview — " + what)
		c.PrintInfo("No changes have been made yet. Review the list below:")
	}
	if err := printMigrationReport(c, report, true, opts.FromSetup); err != nil {
		return false, err
	}
	if opts.DryRun {
		hint := "  icebath import-agent openclaw"
		if report.Preset != nil {
			hint += " --preset " + *report.Preset
		}
		if len(opts.Include) > 0 {
			hint += " --include " + selectionText(opts.Include)
		}
		if len(opts.Exclude) > 0 {
			hint += " --exclude " + selectionText(opts.Exclude)
		}
		info(c, "", "To execute the migration, run without --dry-run:", hint)
	}
	if opts.DryRun || (count == 0 && conflicts == 0) {
		return false, nil
	}
	if conflicts > 0 && !opts.Overwrite {
		errorBlock(c, fmt.Sprintf("Plan has %d conflict(s). Refusing to apply.", conflicts),
			"Each conflict is an item whose target already exists in "+home.Display(home.Dir(ctx))+"/. "+
				"Re-run with --overwrite to replace conflicting targets (item-level backups are written to the migration report directory).",
			"Or re-run with --dry-run to review the full plan.")
		return false, nil
	}
	return true, nil
}

// Takes the pre-migration backup unless disabled, applies the migration and prints the result
func applyMigration(ctx context.Context, c *cliui.Console, run func(bool) (*Report, error), opts migrateOptions) (int, error) {
	backupArchive := ""
	if !opts.NoBackup {
		var size int64
		archive, err := backup.CreatePreMigrationBackup(opts.icebathHome, backup.PreMigrationDefaultKeep)
		if err == nil && archive != "" {
			var st os.FileInfo
			if st, err = os.Stat(archive); err == nil {
				size = st.Size()
			}
		}
		if err != nil {
			errorBlock(c, "Could not create pre-migration backup: "+excText(err),
				"Re-run with --no-backup to skip, or free up disk space under the Icebath home.")
			return 0, nil
		}
		backupArchive = archive
		outln(c, "")
		if archive != "" {
			c.PrintSuccess(fmt.Sprintf("Pre-migration backup: %s (%s)", archive, tools.SizefmtBytes(size)))
			c.PrintInfo("Restore with: icebath import " + pypath.PathName(archive))
		} else {
			c.PrintWarning("Pre-migration backup was not created (nothing to back up, the write failed, or it was incomplete); " +
				"if incomplete, a partial pre-migration-*.incomplete.zip may exist in the backups dir.")
		}
	}
	report, err := run(true)
	if err != nil {
		errorBlock(c, "Migration failed: "+excText(err))
		if backupArchive != "" {
			info(c, "A pre-migration backup is available at: "+backupArchive, "Restore with: icebath import "+pypath.PathName(backupArchive))
		}
		return 0, nil
	}
	return exitStatus(printMigrationReport(c, report, false, opts.FromSetup))
}

// Report row group printed after the migrated items
type reasonGroup struct {
	status, heading, color string
}

var reportReasonGroups = []reasonGroup{
	{StatusConflict, "  ⚠ Conflicts (skipped — use --overwrite to force):", cliui.Yellow},
	{StatusSkipped, "  ─ Skipped:", cliui.Dim},
	{StatusError, "  ✗ Errors:", cliui.Red},
}

// Prints rows indented, the first shown of them followed by a count of the rest
func printRows(c *cliui.Console, rows []string, shown int) {
	for _, row := range rows[:min(shown, len(rows))] {
		outln(c, "      "+row)
	}
	if len(rows) > shown {
		outln(c, fmt.Sprintf("      ... and %d more", len(rows)-shown))
	}
}

// Prints the preview or apply report, then the unredacted report JSON when MIGRATION_JSON_OUTPUT is set
func printMigrationReport(c *cliui.Console, report *Report, dryRun, compactSkipped bool) error {
	migrated := report.Summary.Get(StatusMigrated)
	outln(c, "")
	if dryRun {
		c.PrintHeader("Dry Run Results")
		c.PrintInfo("No files were modified. This is a preview of what would happen.")
	} else {
		c.PrintHeader("Migration Results")
	}
	outln(c, "")
	migratedHeading := "  ✓ Migrated:"
	if dryRun {
		migratedHeading = "  ✓ Would migrate:"
	}
	userHome, err := pypath.PathHome()
	if err != nil {
		panic(err)
	}
	groups := append([]reasonGroup{{StatusMigrated, migratedHeading, cliui.Green}}, reportReasonGroups...)
	for _, g := range groups {
		if g.status == StatusSkipped && compactSkipped {
			n := 0
			for _, item := range report.Items {
				if item.Status == StatusSkipped {
					n++
				}
			}
			printSkippedCount(c, n, report)
			continue
		}
		var rows []string
		for _, item := range report.Items {
			if item.Status != g.status {
				continue
			}
			kind := item.Kind
			switch {
			case g.status != StatusMigrated:
				rows = append(rows, pystr.Ljust(kind, 22)+"  "+item.Reason)
			case item.Destination != nil && *item.Destination != "":
				rows = append(rows, pystr.Ljust(kind, 22)+" → "+strings.ReplaceAll(*item.Destination, userHome, "~"))
			default:
				rows = append(rows, kind)
			}
		}
		if len(rows) > 0 {
			outln(c, c.Color(g.heading, g.color))
			printRows(c, rows, len(rows))
			outln(c, "")
		}
	}
	label := "migrated"
	if dryRun {
		label = "would migrate"
	}
	counts := []struct {
		n     int
		label string
	}{{migrated, label}, {report.Summary.Get(StatusConflict), "conflict(s)"}, {report.Summary.Get(StatusSkipped), "skipped"}, {report.Summary.Get(StatusError), "error(s)"}}
	var parts []string
	for _, cnt := range counts {
		if cnt.n != 0 {
			parts = append(parts, fmt.Sprintf("%d %s", cnt.n, cnt.label))
		}
	}
	if len(parts) > 0 {
		c.PrintInfo("Summary: " + strings.Join(parts, ", "))
	} else {
		c.PrintInfo("Nothing to migrate.")
	}
	if report.OutputDirText() != "" {
		c.PrintInfo("Full report saved to: " + report.OutputDirText())
	}
	if !dryRun && migrated != 0 {
		printNextSteps(c, report.Items)
	}
	if os.Getenv("MIGRATION_JSON_OUTPUT") == "" {
		return nil
	}
	text, err := pyjson.Dumps(report.JSON(), pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	outln(c, text)
	return nil
}

// Prints the one line skipped count used inside setup
func printSkippedCount(c *cliui.Console, count int, report *Report) {
	if count == 0 {
		return
	}
	where := ""
	if report.OutputDirText() != "" {
		where = " (each is listed in the report)"
	}
	outln(c, c.Color(fmt.Sprintf("  ─ Skipped: %d item(s) with nothing to bring over%s", count, where), cliui.Dim))
	outln(c, "")
}

// Prints what still needs doing after an apply that migrated something
func printNextSteps(c *cliui.Console, items []ItemResult) {
	outln(c, "")
	c.PrintSuccess("Migration complete!")
	keysSkipped := false
	for _, i := range items {
		if i.Kind == "provider-keys" && i.Status == StatusSkipped {
			keysSkipped = true
		}
	}
	if !keysSkipped {
		return
	}
	outln(c, "")
	for _, line := range []string{"  ⚠ API keys were NOT migrated (secrets migration is disabled by default).",
		"  Your OPENROUTER_API_KEY and other provider keys must be added manually."} {
		outln(c, c.Color(line, cliui.Yellow))
	}
	info(c, "", "To migrate API keys, re-run with:", "  icebath import-agent openclaw --migrate-secrets", "", "Or add your key manually:",
		"  icebath config set OPENROUTER_API_KEY sk-or-v1-...")
}

// Description of one found state path
type stateFile struct {
	desc string
}

// State files of an OpenClaw directory root and of its workspace like subdirectories
func scanWorkspaceState(sourceDir string) []stateFile {
	type candidate struct{ path, scope string }
	var candidates []candidate
	for _, name := range []string{"todo.json", "sessions", "logs"} {
		candidates = append(candidates, candidate{pypath.PathJoin(sourceDir, name), "Root"})
	}
	if entries, err := os.ReadDir(sourceDir); err == nil {
		for _, e := range entries {
			child := pypath.PathJoin(sourceDir, e.Name())
			if !isDir(child) || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			for _, name := range []string{"todo.json", "sessions", "logs", "memory"} {
				candidates = append(candidates, candidate{pypath.PathJoin(child, name), "Workspace"})
			}
		}
	}
	var out []stateFile
	for _, cand := range candidates {
		if !exists(cand.path) {
			continue
		}
		kind := "file"
		if isDir(cand.path) {
			kind = "directory"
		}
		rel, _ := pypath.PathRelativeTo(cand.path, sourceDir)
		out = append(out, stateFile{cand.scope + " " + kind + ": " + pypath.PathAsPosix(rel)})
	}
	return out
}

// Archive name for an OpenClaw directory, .pre-migration then date stamped then numbered, renamed unless a dry run
func archiveDirectory(sourceDir string, dryRun bool) (string, error) {
	base := pypath.PathName(sourceDir) + ".pre-migration"
	stamped := base + "-" + time.Now().Format("20060102")
	parent := pypath.PathParent(sourceDir)
	archivePath := ""
	for n := 0; archivePath == ""; n++ {
		name := base
		switch {
		case n == 1:
			name = stamped
		case n > 1:
			name = fmt.Sprintf("%s-%d", stamped, n)
		}
		if candidate := pypath.PathJoin(parent, name); !exists(candidate) {
			archivePath = candidate
		}
	}
	if !dryRun {
		if err := os.Rename(sourceDir, archivePath); err != nil {
			return "", err
		}
	}
	return archivePath, nil
}

// Labels of the workspace items present in a workspace directory
func workspaceItems(ws string) []string {
	var out []string
	if exists(pypath.PathJoin(ws, "todo.json")) {
		out = append(out, "todo.json")
	}
	if isDir(pypath.PathJoin(ws, "sessions")) {
		out = append(out, "sessions/")
	}
	if exists(pypath.PathJoin(ws, "SOUL.md")) {
		out = append(out, "SOUL.md")
	}
	if exists(pypath.PathJoin(ws, "MEMORY.md")) {
		out = append(out, "MEMORY.md")
	}
	return out
}

// Prints the first five workspace directories and first eight state files of one OpenClaw directory
func describeOpenClawDir(c *cliui.Console, sourceDir string) {
	outln(c, "")
	c.PrintHeader("Found: " + sourceDir)
	stateFiles := scanWorkspaceState(sourceDir)
	var workspaceDirs []string
	if entries, err := os.ReadDir(sourceDir); err == nil {
		for _, e := range entries {
			d := pypath.PathJoin(sourceDir, e.Name())
			if !isDir(d) || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			for _, marker := range workspaceMarkers {
				if exists(pypath.PathJoin(d, marker)) {
					workspaceDirs = append(workspaceDirs, d)
					break
				}
			}
		}
	}
	if len(workspaceDirs) > 0 {
		c.PrintInfo(fmt.Sprintf("Workspace directories: %d", len(workspaceDirs)))
		rows := make([]string, len(workspaceDirs))
		for i, ws := range workspaceDirs {
			items := strings.Join(workspaceItems(ws), ", ")
			if items == "" {
				items = "empty"
			}
			rows[i] = pypath.PathName(ws) + "/  (" + items + ")"
		}
		printRows(c, rows, 5)
	}
	if len(stateFiles) > 0 {
		outln(c, "")
		outln(c, c.Color(fmt.Sprintf("  %d state file(s) found:", len(stateFiles)), cliui.Yellow))
		rows := make([]string, len(stateFiles))
		for i, sf := range stateFiles {
			rows[i] = sf.desc
		}
		printRows(c, rows, 8)
	}
	outln(c, "")
}

// Renames leftover OpenClaw directories to .pre-migration, returning the exit status
func Cleanup(ctx context.Context, args agentimport.OpenClawCleanupArgs) (int, error) {
	c := consoleOf(args.Console)
	printBanner(c, "OpenClaw Cleanup")
	var dirs []string
	if args.Source != "" {
		dirs = []string{pypath.PathStr(args.Source)}
	} else {
		dirs = FindOpenClawDirs()
	}
	if len(dirs) == 0 {
		outln(c, "")
		c.PrintSuccess("No OpenClaw directories found. Nothing to clean up.")
		return 0, nil
	}
	running, err := detectOpenClawProcesses()
	if err != nil {
		return 1, err
	}
	if len(running) > 0 {
		answer := warnRunning(ctx, c, args.Yes, "OpenClaw appears to be still running:", running,
			[]string{"Archiving .openclaw/ while the service is active may cause it to immediately recreate an empty skeleton directory, destroying your config.",
				"Stop OpenClaw first: systemctl --user stop openclaw-gateway.service"},
			"Proceed anyway?", "Aborted. Stop OpenClaw first, then re-run: icebath claw cleanup",
			[]string{"Non-interactive session — aborting. Stop OpenClaw and re-run."})
		if answer == nil || !*answer {
			return 0, nil
		}
	}
	total := 0
	for _, sourceDir := range dirs {
		describeOpenClawDir(c, sourceDir)
		if args.DryRun {
			archivePath, _ := archiveDirectory(sourceDir, true)
			c.PrintInfo("Would archive: " + sourceDir + " → " + archivePath)
			continue
		}
		answer := confirm(ctx, c, args.Yes, "Archive "+sourceDir+"?", true, "Skipped.",
			[]string{"Non-interactive session — would archive: " + sourceDir, "To execute, re-run with: icebath claw cleanup --yes"})
		if answer == nil || !*answer {
			continue
		}
		archivePath, err := archiveDirectory(sourceDir, false)
		if err != nil {
			c.PrintError("Could not archive: " + osErrText(err))
			c.PrintInfo("Try manually: mv " + sourceDir + " " + sourceDir + ".pre-migration")
			continue
		}
		c.PrintSuccess("Archived: " + sourceDir + " → " + archivePath)
		total++
	}
	outln(c, "")
	n := total
	if args.DryRun {
		n = len(dirs)
	}
	word := "directories"
	if n == 1 {
		word = "directory"
	}
	switch {
	case args.DryRun:
		info(c, fmt.Sprintf("Dry run complete. %d %s would be archived.", len(dirs), word), "Run without --dry-run to archive them.")
	case total > 0:
		c.PrintSuccess(fmt.Sprintf("Cleaned up %d OpenClaw %s.", total, word))
		c.PrintInfo("Directories were renamed, not deleted. You can undo by renaming them back.")
	default:
		c.PrintInfo("No directories were archived.")
	}
	return 0, nil
}

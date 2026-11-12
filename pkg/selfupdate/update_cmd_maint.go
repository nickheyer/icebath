package selfupdate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pytoml"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	preUpdateSnapshotKeep        = 1
	preUpdateSnapshotMaxFileSize = int64(1) << 30
)

// Installer one liner per platform, keyed by whether the host is Windows
var reinstallOneLiner = map[bool]string{
	true:  "iex (irm https://hermes-agent.nousresearch.com/install.ps1)",
	false: "curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash",
}

var backupModeAliases = map[string]string{
	"off": "off", "false": "off", "none": "off", "disabled": "off",
	"full": "full", "zip": "full", "true": "full",
	"quick": "quick",
}

var (
	maintCreateQuickSnapshot  = backup.CreateQuickSnapshot
	maintSqliteRuntimeStatus  = postUpdateSqliteRuntimeStatus
	maintKillStaleDashboards  = killStaleDashboardsGuarded
	maintSyncSkills           = skills.SyncSkills
	maintConfigMigration      = checkAndApplyConfigMigration
	maintCheckoutVersion      = checkoutVersion
	maintNoticesAndSelfHeals  = printPostUpdateNoticesAndSelfHeals
	maintSyncProfiles         = syncProfilesAfterUpdate
	maintRepairWindowsHomeACL = home.RepairWindowsHomeACL
)

// Historical updater hook that never completes inside the pre swap process
func prepareUpdatedCheckout(ctx context.Context, projectRoot string, desktop bool) error {
	return pm.StopForRelaunchIncomplete(ctx)
}

// Shared wording of the partial completion banner for a vulnerable post update SQLite
func sqlitePartialCompletionLines(sqliteVersion string) []string {
	return []string{
		fmt.Sprintf("⚠ Update partially complete — your Python's SQLite (%s) has a known "+
			"corruption bug. Icebath works, but sessions could be damaged.", sqliteVersion),
		fmt.Sprintf("  Fix: run the installer again (%s) "+
			"which installs a safe Python, then run `icebath doctor` to confirm.", reinstallOneLiner[sysPlatform() == "win32"]),
	}
}

// The updates section of config.yaml
func loadUpdatesCfg(ctx context.Context) (map[string]any, error) { return updatesConfig(ctx) }

// Historical updater hook, module graph surgery cannot complete an update
func purgeStaleHermesModules(ctx context.Context) error { return pm.StopForRelaunchIncomplete(ctx) }

// Historical updater hook, dependency activation belongs to the next process
func reloadUpdatedRuntimeModules(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Curator heads up printed only while enabled and never run
func printCuratorFirstRunNotice(ctx context.Context, w io.Writer) {
	if !memory.CuratorIsEnabled(ctx) {
		return
	}
	curatorState, err := memory.LoadCuratorState(ctx)
	if err != nil {
		return
	}
	if pyrepr.Truthy(curatorState.GetOr("last_run_at", nil)) {
		return
	}
	days := max(1, memory.CuratorIntervalHours(ctx)/24)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "ℹ Skill curator")
	fmt.Fprintf(w, "  Background skill maintenance is enabled. First pass is deferred "+
		"~%dd after installation; only agent-created skills are in "+
		"scope and nothing is ever auto-deleted (archive is recoverable).\n", days)
	fmt.Fprintln(w, "  Preview now:  icebath curator run --dry-run")
	fmt.Fprintln(w, "  Pause it:     icebath curator pause")
	fmt.Fprintln(w, "  Docs:         https://hermes-agent.nousresearch.com/docs/user-guide/features/curator")
}

// Whether a single row probe returns a row
func maintHasRow(ctx context.Context, conn state.SQLiteConn, query string) (bool, error) {
	var one any
	err := conn.QueryRowContext(ctx, query).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Advertises the opt in FTS storage rebuild when state.db still needs one
func printFTSOptimizeAvailableNotice(ctx context.Context, w io.Writer) {
	mode := "advise"
	if cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil); err == nil {
		var sessions map[string]any
		known := true
		switch section := cfg["sessions"].(type) {
		case map[string]any:
			sessions = section
		case config.Tree:
			sessions = section
		default:
			known = !pyrepr.Truthy(section)
		}
		if known {
			mode = "advise"
			if value, ok := sessions["fts_optimize_notice"]; ok {
				mode = pyrepr.Str(value)
			}
			mode = strings.ToLower(pystr.Strip(mode))
		}
	}
	if mode == "off" {
		return
	}
	dbPath := filepath.Join(home.Dir(ctx), "state.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		return
	}
	sizeGB := float64(info.Size()) / (1024 * 1024 * 1024)
	if sizeGB < 0.5 {
		return
	}
	db, err := state.NewSessionDB(ctx, dbPath, true)
	if err != nil {
		return
	}
	var drift []string
	legacyInline, interrupted := false, false
	err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		if drift, err = state.FTSLayoutDrift(ctx, conn); err != nil {
			return err
		}
		if legacyInline, err = state.DBHasLegacyInlineFTS(ctx, conn); err != nil {
			return err
		}
		for _, query := range []string{
			"SELECT 1 FROM state_meta WHERE key = 'fts_rebuild_high_water' LIMIT 1",
			"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name LIKE 'fts\\_v22\\_trash\\_%' ESCAPE '\\' LIMIT 1",
			"SELECT 1 FROM state_meta WHERE key IN ('fts_cjk_rebuild_high_water', 'fts_cjk_stale') LIMIT 1",
		} {
			if interrupted, err = maintHasRow(ctx, conn, query); err != nil || interrupted {
				return err
			}
		}
		return nil
	})
	_ = db.Close()
	if err != nil {
		return
	}
	if len(drift) == 0 && !interrupted {
		return
	}
	if interrupted {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "◆ Session database optimization incomplete")
		fmt.Fprintln(w, "  A previous `icebath sessions optimize-storage` run was "+
			"interrupted. Search still works; re-run the command to resume "+
			"and finish reclaiming disk:")
		fmt.Fprintln(w, "    icebath sessions optimize-storage")
		return
	}
	fmt.Fprintln(w)
	if legacyInline {
		estReclaim := sizeGB * 0.6
		if mode == "require" {
			fmt.Fprintln(w, "◆ Session database upgrade required")
			fmt.Fprintf(w, "  Your search index uses the OLD storage layout and should be "+
				"upgraded. The new layout typically frees ~60%% of state.db "+
				"(≈%.1f GB of your current %.1f GB) and is "+
				"required for continued optimal operation.\n", estReclaim, sizeGB)
		} else {
			fmt.Fprintln(w, "◆ Reclaim ~60% of your session database disk")
			fmt.Fprintf(w, "  Your search index uses the old storage layout. Upgrading it "+
				"typically frees ~60%% of state.db — about %.1f GB "+
				"of your current %.1f GB.\n", estReclaim, sizeGB)
		}
	} else {
		if mode == "require" {
			fmt.Fprintln(w, "◆ Search index rebuild required by your notice policy")
		} else {
			fmt.Fprintln(w, "◆ Search index update available")
		}
		fmt.Fprintln(w, "  Your search index is on the compact layout from an earlier "+
			"optimization, but a later release changed it. Running the "+
			"command again rebuilds it in the current layout.")
	}
	fmt.Fprintln(w, "  Run when convenient:  icebath sessions optimize-storage")
	fmt.Fprintln(w, "  It runs in the foreground with a progress bar, is safe to "+
		"interrupt/re-run, and never changes your conversations.")
}

// Prints the latest background curator run summary once and stamps it shown
func printCuratorRecentRunNotice(ctx context.Context, w io.Writer) {
	curatorState, err := memory.LoadCuratorState(ctx)
	if err != nil {
		return
	}
	lastRunAt := curatorState.GetOr("last_run_at", nil)
	if !pyrepr.Truthy(lastRunAt) {
		return
	}
	if pyrepr.Equal(curatorState.GetOr("last_run_summary_shown_at", nil), lastRunAt) {
		return
	}
	summary := curatorState.GetOr("last_run_summary", nil)
	if !pyrepr.Truthy(summary) {
		return
	}
	if text, ok := summary.(string); ok && strings.Contains(text, "\n") {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "ℹ Skill curator — last run %s\n", formatTimeAgo(lastRunAt))
		for _, line := range pystr.Splitlines(text, false) {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  (This message shows once per curator run. View anytime: icebath curator status)")
	}
	curatorState.Set("last_run_summary_shown_at", lastRunAt)
	memory.SaveCuratorState(ctx, curatorState)
}

// Renders an ISO timestamp as a minutes, hours or days ago phrase
func formatTimeAgo(isoTS any) string {
	text, ok := isoTS.(string)
	if !ok {
		return "recently"
	}
	ts, _, err := pydatetime.ParseISOFormat(text)
	if err != nil {
		return "recently"
	}
	secs := int64(time.Since(ts) / time.Second)
	switch {
	case secs < 60:
		return "just now"
	case secs < 3600:
		return fmt.Sprintf("%dm ago", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%dh ago", secs/3600)
	}
	return fmt.Sprintf("%dd ago", secs/86400)
}

// Historical updater hook, scans run only in a fresh completion process
func reloadProcessScanModules(ctx context.Context) error { return pm.StopForRelaunchIncomplete(ctx) }

// Historical updater hook that never continues a pre PM update after the swap
func finishDashboardUpdateCleanup(ctx context.Context, nodeFailures []string, alreadyRestartedUnits map[string]bool) error {
	return pm.StopForRelaunch(ctx)
}

// Runs the dashboard stop and restart, a panic of the stop returned as its failure
func killStaleDashboardsGuarded(ctx context.Context, w io.Writer, alreadyRestartedUnits map[string]bool, scopeHome string) (result gateway.DashboardKillResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			if failure, ok := r.(error); ok {
				err = failure
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return gateway.KillStaleDashboardProcesses(ctx, w, "the running backend no longer matches the updated frontend", true, alreadyRestartedUnits, scopeHome), nil
}

// Refreshes managed dashboards or stops stale manual ones, the pids it stopped and could not bring back
func refreshDashboardAfterUpdate(ctx context.Context, w io.Writer, alreadyRestartedUnits map[string]bool) map[int]bool {
	unrecovered := map[int]bool{}
	result, err := maintKillStaleDashboards(ctx, w, alreadyRestartedUnits, home.Dir(ctx))
	if err != nil {
		updateLog.WarnContext(ctx, fmt.Sprintf("Post-update dashboard cleanup failed: %s", launchErrorText(err)))
		recordUpdateStep(ctx, "dashboard_cleanup", false, pm.ErrorClass(err)+": "+launchErrorText(err))
		fmt.Fprintln(w)
		fmt.Fprintf(w, "⚠ Could not refresh running dashboard/serve process(es): %s\n", launchErrorText(err))
		fmt.Fprintln(w, "  If one is still running, restart it so it serves the updated code:")
		fmt.Fprintln(w, "    icebath dashboard --port <port>   (or: systemctl --user restart icebath-dashboard)")
		return unrecovered
	}
	for _, pid := range result.Unrecovered {
		unrecovered[pid] = true
	}
	if len(unrecovered) == 0 {
		return unrecovered
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "⚠ A web dashboard/serve process was stopped during update and could not be auto-restarted.")
	fmt.Fprintln(w, "  Re-launch it when you want the web UI back:")
	fmt.Fprintln(w, "    icebath dashboard --port <port>")
	return unrecovered
}

// Exported form of the post update dashboard refresh for the dashboard cases outside this package
func RefreshDashboardAfterUpdate(ctx context.Context, w io.Writer, alreadyRestartedUnits map[string]bool) map[int]bool {
	return refreshDashboardAfterUpdate(ctx, w, alreadyRestartedUnits)
}

// Prints the outcome with branch and sha plus the receipt line a dashboard action id asks for
func printUpdateCompletion(ctx context.Context, w io.Writer, message string) {
	fmt.Fprintf(w, "%s%s\n", message, branchHeadSuffix(ctx, nil, ""))
	actionID := os.Getenv("ICEBATH_ACTION_ID")
	if len(actionID) != 32 || strings.Trim(actionID, "0123456789abcdef") != "" {
		return
	}
	fmt.Fprintf(w, "=== icebath-update completed %s ===\n", actionID)
}

// Version field of the checkout's pyproject.toml, nil on any failure
func readProjectVersion(ctx context.Context) *string {
	raw, err := os.ReadFile(filepath.Join(fleetProjectRoot(), "pyproject.toml"))
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return nil
	}
	document, err := pytoml.Loads(text)
	if err != nil {
		return nil
	}
	project, _ := document["project"].(map[string]any)
	if version := project["version"]; pyrepr.Truthy(version) {
		shown := pyrepr.Str(version)
		return &shown
	}
	return nil
}

// Runtime identity of the checkout as the source stamp publishes it, nil without a commit
func checkoutVersion(ctx context.Context) *string {
	info := updaterecord.GitVersionInfo(fleetProjectRoot(), true)
	if info.Commit == nil || *info.Commit == "" {
		return nil
	}
	return &info.DerivedVersion
}

// Completion line naming both versions when known and changed
func updateCompleteMessage(ctx context.Context, preVersion *string) string {
	shown := func(version string) string {
		for _, r := range version {
			if pystr.IsDigit(r) {
				return "v" + version
			}
			break
		}
		return version
	}
	post := maintCheckoutVersion(ctx)
	havePost := post != nil && *post != ""
	if preVersion != nil && *preVersion != "" && havePost && *preVersion != *post {
		return fmt.Sprintf("✓ Update complete! (%s → %s)", shown(*preVersion), shown(*post))
	}
	if havePost {
		return fmt.Sprintf("✓ Update complete! (%s)", shown(*post))
	}
	return "✓ Update complete!"
}

// Whether the interpreter used after the update has a safe SQLite, with the probe result
func postUpdateSqliteRuntimeStatus(ctx context.Context) (bool, *state.SQLiteRuntimeInfo) {
	exe, _ := os.Executable()
	info, _ := state.ProbeSQLiteRuntime(exe, 30.0)
	return info != nil && !info.WALResetVulnerable(), info
}

// Prints a success completion only after probing the next runtime, false when success is withheld
func printVerifiedUpdateCompletion(ctx context.Context, w io.Writer, message string) bool {
	if !strings.HasPrefix(message, "✓") {
		printUpdateCompletion(ctx, w, message)
		return false
	}
	ok, info := maintSqliteRuntimeStatus(ctx)
	if info == nil {
		updateLog.DebugContext(ctx, "Post-update SQLite runtime probe unavailable; not blocking")
	}
	if info == nil || ok {
		printUpdateCompletion(ctx, w, message)
		return true
	}
	fmt.Fprintln(w)
	for _, line := range sqlitePartialCompletionLines(info.SQLiteVersionString) {
		fmt.Fprintln(w, line)
	}
	return false
}

// Historical updater hook, old soft build results cannot establish completion
func printUpdateSummary(ctx context.Context, nodeFailures []any, desktopBuildOk bool, preUpdateVersion *string) (bool, error) {
	return false, pm.StopForRelaunch(ctx)
}

// Integrity check and auto restore of one home's state.db from its newest valid snapshot, never failing
func verifyAndRestoreOneStateDB(ctx context.Context, w io.Writer, h, label string) {
	defer func() {
		if r := recover(); r != nil {
			updateLog.DebugContext(ctx, fmt.Sprintf("Post-update state.db guard (%s) failed: %v", label, r))
		}
	}()
	statePath := filepath.Join(h, "state.db")
	if !statExists(statePath) {
		return
	}
	ok := backup.VerifySQLiteIntegrity(statePath, backup.IntegrityOptions{})
	if ok.Valid {
		updateLog.DebugContext(ctx, fmt.Sprintf("Post-update state.db integrity OK (%s): %s", label, ok.Message))
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "⚠ state.db is corrupted after update (%s): %s\n", label, ok.Message)
	snapRoot := backup.QuickSnapshotRoot(h)
	if !statExists(snapRoot) {
		fmt.Fprintln(w, "  ⚠ No pre-update snapshot for this home")
		return
	}
	entries, err := os.ReadDir(snapRoot)
	if err != nil {
		updateLog.DebugContext(ctx, fmt.Sprintf("Post-update state.db guard (%s) failed: %s", label, launchErrorText(err)))
		return
	}
	var dirs []string
	for _, entry := range entries {
		if info, err := os.Stat(filepath.Join(snapRoot, entry.Name())); err == nil && info.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, name := range dirs {
		snapState := filepath.Join(snapRoot, name, "state.db")
		if !statExists(snapState) || !backup.VerifySQLiteIntegrity(snapState, backup.IntegrityOptions{}).Valid {
			continue
		}
		restored, err := restoreStateDBFromSnapshot(ctx, w, statePath, snapState)
		switch {
		case err != nil && pypath.IsOSError(err):
			fmt.Fprintf(w, "  ✗ Auto-restore file copy failed: %s\n", launchErrorText(err))
		case err != nil:
			updateLog.DebugContext(ctx, fmt.Sprintf("Post-update state.db guard (%s) failed: %s", label, launchErrorText(err)))
		case restored:
			fmt.Fprintf(w, "  ✓ Auto-restored from snapshot %s (%s)\n", name, label)
		default:
			fmt.Fprintln(w, "  ✗ Auto-restore did not complete; see the restore diagnostic above.")
		}
		return
	}
	fmt.Fprintln(w, "  ⚠ No valid pre-update snapshot found for this home")
}

// Integrity guard for the root state.db and every sibling profile's
func verifyAndRestoreStateDBsPostUpdate(ctx context.Context, w io.Writer) error {
	h := home.Dir(ctx)
	verifyAndRestoreOneStateDB(ctx, w, h, "default home")
	return bestEffort(ctx, "Sibling-profile state.db guard sweep failed: %s", func() error {
		for _, sibling := range backup.SiblingProfileHomes(h) {
			verifyAndRestoreOneStateDB(ctx, w, sibling[1], "profile "+sibling[0])
		}
		return nil
	})
}

// Drops the cached live plugin catalog under the active home and every sibling profile's
func invalidateLivePluginCatalogCaches(ctx context.Context) {
	h := home.Dir(ctx)
	plugins.InvalidateLiveCacheForHome(h)
	defer func() { _ = recover() }()
	for _, sibling := range backup.SiblingProfileHomes(h) {
		plugins.InvalidateLiveCacheForHome(sibling[1])
	}
}

// Names of a sync result list, a typed list read the same as a decoded one
func maintNames(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, len(list))
		for i, item := range list {
			out[i] = pyrepr.Str(item)
		}
		return out
	}
	return nil
}

// Runs the bundled skills sync and prints its summary
func printBundledSkillsSyncReport(ctx context.Context, w io.Writer) error {
	result, err := maintSyncSkills(ctx, true)
	if err != nil {
		return err
	}
	copied, updated := maintNames(result["copied"]), maintNames(result["updated"])
	if len(copied) > 0 {
		fmt.Fprintf(w, "  + %d new: %s\n", len(copied), strings.Join(copied, ", "))
	}
	if len(updated) > 0 {
		fmt.Fprintf(w, "  ↑ %d updated: %s\n", len(updated), strings.Join(updated, ", "))
	}
	if modified := maintNames(result["user_modified"]); len(modified) > 0 {
		fmt.Fprintf(w, "  ~ %d user-modified (kept)\n", len(modified))
		fmt.Fprintln(w, "    → see them: icebath skills list-modified  (diff/reset to resume updates)")
	}
	if cleaned := maintNames(result["cleaned"]); len(cleaned) > 0 {
		fmt.Fprintf(w, "  − %d removed from manifest\n", len(cleaned))
	}
	if relocated := maintNames(result["relocated"]); len(relocated) > 0 {
		fmt.Fprintf(w, "  → %d moved to new upstream paths: %s\n", len(relocated), strings.Join(relocated, ", "))
	}
	if len(copied) == 0 && len(updated) == 0 {
		fmt.Fprintln(w, "  ✓ Skills are up to date")
	}
	return nil
}

// Puts /usr/local/bin on PATH for root non login shells of an FHS install whose shell cannot find the command
func ensureFHSPathGuard(ctx context.Context, w io.Writer) error {
	if sysPlatform() != "linux" || os.Geteuid() != 0 {
		return nil
	}
	if _, err := os.Lstat("/usr/local/bin/icebath"); err != nil {
		return nil
	}
	userHome := os.Getenv("HOME")
	if userHome == "" {
		userHome = "/root"
	}
	term, ok := os.LookupEnv("TERM")
	if !ok {
		term = "dumb"
	}
	probe, err := pysubprocess.RunWith(ctx, []string{"env", "-i", "HOME=" + userHome, "TERM=" + term, "bash", "-i", "-c", "command -v icebath"},
		pysubprocess.Options{Timeout: 10 * time.Second})
	if err != nil {
		var expired *pysubprocess.TimeoutExpired
		var missing *exec.Error
		if errors.As(err, &expired) || errors.As(err, &missing) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if probe.Returncode == 0 {
		return nil
	}
	pathLine := `export PATH="/usr/local/bin:$PATH"`
	pathComment := "# Icebath Agent — ensure /usr/local/bin is on PATH (RHEL non-login shells)"
	wroteAny := false
	for _, candidate := range []string{".bashrc", ".bash_profile"} {
		cfg := filepath.Join(userHome, candidate)
		if info, err := os.Stat(cfg); err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(cfg)
		if err != nil {
			continue
		}
		guarded := false
		for _, line := range pystr.Splitlines(pystr.DecodeUTF8SigReplace(raw), false) {
			if strings.Contains(line, "/usr/local/bin") && strings.Contains(line, "PATH") && !strings.HasPrefix(pystr.Lstrip(line), "#") {
				guarded = true
				break
			}
		}
		if guarded {
			continue
		}
		file, err := os.OpenFile(cfg, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err == nil {
			_, err = file.WriteString("\n" + pathComment + "\n" + pathLine + "\n")
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			fmt.Fprintf(w, "  ⚠ Could not update %s: %s\n", cfg, launchErrorText(err))
			continue
		}
		fmt.Fprintf(w, "  ✓ Added /usr/local/bin to PATH in %s\n", cfg)
		wroteAny = true
	}
	if wroteAny {
		fmt.Fprintln(w, "    (reload your shell or run 'source ~/.bashrc' to pick it up)")
	}
	return nil
}

// Historical export, launcher policy belongs to the launcher owner
func ensureACPLauncher(ctx context.Context) error {
	_, err := pm.ExposeCLI(ctx, fleetProjectRoot(), true)
	return err
}

// Backup mode off, quick or full, the flags winning over the config and quick the default
func resolvePreUpdateBackupMode(ctx context.Context, args map[string]any) string {
	if pyrepr.Truthy(args["no_backup"]) {
		return "off"
	}
	if pyrepr.Truthy(args["backup"]) {
		return "full"
	}
	var raw any = "quick"
	cfg, err := loadUpdatesCfg(ctx)
	if err != nil {
		updateLog.DebugContext(ctx, fmt.Sprintf("Could not load config for pre-update backup: %s", launchErrorText(err)))
	} else if value, ok := cfg["pre_update_backup"]; ok {
		raw = value
	}
	if flag, ok := raw.(bool); ok {
		if flag {
			return "full"
		}
		return "off"
	}
	mode, ok := backupModeAliases[strings.ToLower(pystr.Strip(pyrepr.Str(raw)))]
	if !ok {
		updateLog.WarnContext(ctx, fmt.Sprintf("Unknown updates.pre_update_backup value %s — using 'quick'", pyrepr.Repr(raw)))
		return "quick"
	}
	return mode
}

// Verifies the live state.db after the snapshot and says which copy is still valid when it fails
func verifyStateDBAfterSnapshot(ctx context.Context, w io.Writer, snapshotID string) {
	h := home.Dir(ctx)
	source := filepath.Join(h, "state.db")
	if !statExists(source) {
		return
	}
	limit := preUpdateSnapshotMaxFileSize
	integrity := backup.VerifySQLiteIntegrity(source, backup.IntegrityOptions{MaxBytes: &limit})
	if integrity.Valid {
		return
	}
	fmt.Fprintf(w, "  ⚠ state.db integrity check FAILED after snapshot: %s\n", integrity.Message)
	snapState := filepath.Join(backup.QuickSnapshotRoot(h), snapshotID, "state.db")
	switch {
	case !statExists(snapState):
		fmt.Fprintln(w, "  ⚠ Snapshot does not contain state.db (was skipped or too large).")
	case backup.VerifySQLiteIntegrity(snapState, backup.IntegrityOptions{}).Valid:
		fmt.Fprintln(w, "  ✓ Snapshot copy is valid — continuing update.")
		fmt.Fprintln(w, "    If state.db is lost after update it will be auto-restored.")
	default:
		fmt.Fprintln(w, "  ✗ Snapshot copy ALSO failed integrity — the source was already corrupted before the backup.")
	}
	fmt.Fprintln(w)
}

// Quick snapshot of the root home plus every sibling profile, the root snapshot id
func runQuickSnapshots(ctx context.Context, w io.Writer) (string, error) {
	label, keep, limit := "pre-update", preUpdateSnapshotKeep, preUpdateSnapshotMaxFileSize
	h := home.Dir(ctx)
	snapshotID, err := maintCreateQuickSnapshot(&label, h, &keep, &limit)
	if err != nil {
		return "", err
	}
	if snapshotID != "" {
		verifyStateDBAfterSnapshot(ctx, w, snapshotID)
		fmt.Fprintf(w, "◆ Pre-update snapshot: %s\n", snapshotID)
	}
	err = bestEffort(ctx, "Sibling profile snapshots failed: %s", func() error {
		siblings, err := backup.CreatePreUpdateSnapshotsAllProfiles(h, &keep, &limit)
		if err != nil || siblings == nil || siblings.Len() == 0 {
			return err
		}
		names := append([]string{}, siblings.Keys...)
		sort.Strings(names)
		pairs := make([]string, len(names))
		for i, name := range names {
			pairs[i] = name + "=" + pyrepr.Str(siblings.GetOr(name, nil))
		}
		fmt.Fprintln(w, "◆ Sibling profile snapshot(s): "+strings.Join(names, ", "))
		recordUpdateStep(ctx, "sibling_profile_snapshots", true, strings.Join(pairs, ", "))
		lastSiblingSnapshots = siblings
		return nil
	})
	return snapshotID, err
}

// Zips the home under backups, printing where it went and never failing the update
func runFullBackup(ctx context.Context, w io.Writer) {
	var keepRaw any = 5
	if cfg, err := loadUpdatesCfg(ctx); err == nil {
		if value, ok := cfg["backup_keep"]; ok {
			keepRaw = value
		}
	}
	fmt.Fprintln(w, "◆ Creating pre-update backup...")
	started := time.Now()
	h := home.Dir(ctx)
	outPath := ""
	keep, err := maintInt(keepRaw)
	if err == nil {
		outPath, err = backup.CreatePreUpdateBackup(h, keep)
	}
	if err != nil {
		fmt.Fprintf(w, "  ⚠ Backup failed: %s\n", launchErrorText(err))
		fmt.Fprintln(w, "  Continuing with update.")
		fmt.Fprintln(w)
		return
	}
	elapsed := time.Since(started).Seconds()
	if outPath == "" {
		fmt.Fprintln(w, "  ⚠ Backup skipped (no files found or write failed); continuing update.")
		fmt.Fprintln(w)
		return
	}
	var size int64
	if info, err := os.Stat(outPath); err == nil {
		size = info.Size()
	}
	display := pypath.PathStr(outPath)
	if rel, ok := pypath.PathRelativeTo(outPath, h); ok {
		display = home.Display(h) + "/" + rel
	}
	fmt.Fprintf(w, "  Saved:    %s (%s, %.1fs)\n", display, tools.SizefmtBytes(size), elapsed)
	fmt.Fprintf(w, "  Restore:  icebath import %s\n", pypath.PathStr(outPath))
	fmt.Fprintln(w, "  Disable:  set updates.pre_update_backup: quick (or off) in config.yaml")
	fmt.Fprintln(w)
}

// Python int of a config number
func maintInt(v any) (int, error) {
	if n, ok := pyIntValue(v); ok {
		return int(n), nil
	}
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		return int(t), nil
	case string:
		n, err := strconv.ParseInt(strings.ReplaceAll(pystr.Strip(t), "_", ""), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(t))
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// Runs the pre update backup, the quick snapshot id or nil when off or failed
func runPreUpdateBackup(ctx context.Context, w io.Writer, args map[string]any) *string {
	mode := resolvePreUpdateBackupMode(ctx, args)
	if mode == "off" {
		if pyrepr.Truthy(args["no_backup"]) {
			fmt.Fprintln(w, "◆ Pre-update backup: skipped (--no-backup)")
			fmt.Fprintln(w)
		}
		return nil
	}
	snapshotID, err := runQuickSnapshots(ctx, w)
	detail := ""
	if err != nil {
		updateLog.WarnContext(ctx, fmt.Sprintf("Pre-update snapshot failed: %s", launchErrorText(err)))
		detail = " (" + launchErrorText(err) + ")"
		snapshotID = ""
	}
	if snapshotID == "" {
		fmt.Fprintf(w, "  ⚠ Pre-update snapshot FAILED — no recovery point was saved%s.\n", detail)
		fmt.Fprintln(w, "  Continuing with update (set updates.pre_update_backup: off to silence this).")
		fmt.Fprintln(w)
	}
	var id *string
	if snapshotID != "" {
		id = &snapshotID
	}
	if mode != "full" {
		if id != nil {
			fmt.Fprintln(w)
		}
		return id
	}
	runFullBackup(ctx, w)
	return id
}

// Text of a failure, its class name when the text is empty
func maintReason(err error) string {
	if reason := launchErrorText(err); reason != "" {
		return reason
	}
	return pm.ErrorClass(err)
}

// Clears stale bytecode, stamps the fingerprint and refreshes the bootstrap scripts, each failure a follow up
func sweepBytecodeAfterUpdate(ctx context.Context, w io.Writer, branch string) {
	removed := clearBytecodeCache(fleetProjectRoot())
	if removed > 0 {
		plural := "ies"
		if removed == 1 {
			plural = "y"
		}
		fmt.Fprintf(w, "  ✓ Cleared %d stale __pycache__ director%s\n", removed, plural)
	}
	recordBytecodeFingerprint()
	if err := refreshBootstrapCacheScripts(ctx, w, branch); err != nil {
		updaterecord.RecordFollowup(ctx, "bootstrap_scripts", maintReason(err), "the next `icebath update` refreshes them")
	}
}

func profileSkillSyncStatus(r map[string]any) string {
	if len(r) > 0 && pyrepr.Truthy(r["skipped_opt_out"]) {
		return "opted out (--no-skills)"
	}
	if len(r) == 0 {
		return "sync failed"
	}
	var parts []string
	for _, item := range [][2]string{{"copied", "+%d new"}, {"updated", "↑%d updated"}, {"user_modified", "~%d user-modified"}} {
		if count := len(maintNames(r[item[0]])); count > 0 {
			parts = append(parts, fmt.Sprintf(item[1], count))
		}
	}
	if len(parts) == 0 {
		return "up to date"
	}
	return strings.Join(parts, ", ")
}

// Per profile syncs of bundled skills, the seeded env file and the Honcho profiles, each failure swallowed
func syncProfilesAfterUpdate(ctx context.Context, w io.Writer) error {
	if all, err := manage.ListProfiles(ctx, false); isUpdateExit(err) {
		return err
	} else if err == nil && len(all) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "→ Syncing bundled skills to all profiles...")
		for _, profile := range all {
			fmt.Fprintf(w, "  %s: %s\n", profile.Name, profileSkillSyncStatus(manage.SeedProfileSkills(ctx, profile.Path, true)))
		}
	}
	if backfilled, err := manage.BackfillProfileEnvs(true); isUpdateExit(err) {
		return err
	} else if err == nil && len(backfilled) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "→ Seeded .env for %d profile(s) (empty placeholder): %s\n", len(backfilled), strings.Join(backfilled, ", "))
	}
	module, err := memory.ImportProviderModule(ctx, "honcho", "cli")
	if err == nil {
		var synced any
		if synced, err = module.Call(ctx, "sync_honcho_profiles_quiet", nil, nil); err == nil && pyrepr.Truthy(synced) {
			fmt.Fprintf(w, "\n-> Honcho: synced %s profile(s)\n", pyrepr.Str(synced))
		}
	}
	if isUpdateExit(err) {
		return err
	}
	return nil
}

// Reconciles an installed cua-driver package, never a user selected external binary
func refreshCuaDriverAfterUpdate(ctx context.Context, w io.Writer) error {
	cfg, err := loadUpdatesCfg(ctx)
	if err != nil {
		return err
	}
	if refresh, ok := cfg["refresh_cua_driver"]; ok && !pyrepr.Truthy(refresh) {
		return nil
	}
	if pystr.Strip(os.Getenv("ICEBATH_CUA_DRIVER_CMD")) != "" {
		return nil
	}
	installed, err := cuaRefreshInstalled(ctx, "cua-driver", true)
	if err != nil || installed == nil {
		return err
	}
	if sysPlatform() == "win32" {
		fmt.Fprintln(w, "\n→ Windows cua-driver refresh deferred (autostart registration requires UAC).")
		fmt.Fprintln(w, "  Run `icebath computer-use install --upgrade` in an interactive terminal.")
		return nil
	}
	fmt.Fprintln(w, "\n→ Preparing pinned cua-driver (Computer Use)...")
	if sysPlatform() == "darwin" {
		_, err = installCuaDriver(ctx, w, false)
		return err
	}
	return pmEnsureFn(ctx, "cua-driver")
}

// Installs the optional default tools this install lacks, a failed download warning without failing
func installDefaultToolsAfterUpdate(ctx context.Context, w io.Writer) error {
	sealed, err := pm.Sealed(ctx)
	if err != nil {
		return err
	}
	if sealed || !pm.LazyInstallsAllowedFor(ctx) {
		return nil
	}
	defaults, err := pm.DefaultPackages(ctx, pm.NewLockfile(pm.LockfilePath()).Names(), "", nil)
	if err != nil {
		return err
	}
	for _, name := range defaults {
		installed, err := pm.Installed(ctx, name, false)
		if err != nil {
			return err
		}
		if installed != nil {
			continue
		}
		fmt.Fprintf(w, "\n→ Installing %s (default tool; opt out with `icebath pm install --without %s`)...\n", name, name)
		if _, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: true}); err != nil {
			var install *pm.InstallError
			if !errors.As(err, &install) && !pypath.IsOSError(err) {
				return err
			}
			fmt.Fprintf(w, "  ⚠ %s was not installed: %s\n", name, launchErrorText(err))
			fmt.Fprintf(w, "    Retry with: icebath pm install %s\n", name)
		}
	}
	return nil
}

// Surfaces a rollback store at or above its cap that the user may not know is on
func printCheckpointFootprintNotice(ctx context.Context, w io.Writer) error {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	if notice := tools.CheckpointFootprintNotice(&tools.Env{Home: h, Config: cfg}); notice != "" {
		fmt.Fprintf(w, "\n\033[1;33mℹ  %s\033[0m\n", notice)
	}
	return nil
}

// Notices and self heals that run after the summary, each failure logged and swallowed
func printPostUpdateNoticesAndSelfHeals(ctx context.Context, w io.Writer) error {
	quiet := func(step func(context.Context, io.Writer)) func() error {
		return func() error { step(ctx, w); return nil }
	}
	steps := []struct {
		message string
		run     func() error
	}{
		{"FTS optimize notice failed: %s", quiet(printFTSOptimizeAvailableNotice)},
		{"Curator first-run notice failed: %s", quiet(printCuratorFirstRunNotice)},
		{"Curator recent-run notice failed: %s", quiet(printCuratorRecentRunNotice)},
		{"FHS PATH guard check failed: %s", func() error { return ensureFHSPathGuard(ctx, w) }},
		{"CLI launcher exposure failed: %s", func() error { return ensureACPLauncher(ctx) }},
		{"Windows bin launcher migration failed: %s", func() error {
			migrateWindowsBinPath(fleetProjectRoot())
			return nil
		}},
		{"cua-driver refresh failed: %s", func() error { return refreshCuaDriverAfterUpdate(ctx, w) }},
		{"Default PM tool install failed: %s", func() error { return installDefaultToolsAfterUpdate(ctx, w) }},
		{"Checkpoint footprint notice failed: %s", func() error { return printCheckpointFootprintNotice(ctx, w) }},
		{"Relay exporter migration failed: %s", func() error { return migrateRelayExporterEnv(ctx, w) }},
	}
	for _, step := range steps {
		if err := bestEffort(ctx, step.message, step.run); err != nil {
			return err
		}
	}
	return nil
}

func migrateRelayExporterEnv(ctx context.Context, w io.Writer) error {
	return plugins.RunRelayMigrationAfterUpdate(ctx, w)
}

// Post build housekeeping and the completion line, the SQLite runtime verdict of the next process
func RunPostUpdateMaintenance(ctx context.Context, o PostUpdateMaintenance) (bool, error) {
	var w io.Writer = os.Stdout
	if o.Stdout != nil {
		w = o.Stdout
	}
	owedStep := func(name string, run func() error) {
		err := run()
		if err == nil {
			return
		}
		reason := maintReason(err)
		updaterecord.RecordFollowup(ctx, name, reason, "")
		if o.Followups != nil {
			*o.Followups = append(*o.Followups, [2]string{name, reason})
		}
	}
	root := fleetProjectRoot()
	owedStep("windows_home_acl", func() error { return maintRepairWindowsHomeACL(ctx, nil) })
	if sysPlatform() == "darwin" && o.HadDesktopAppBeforeUpdate {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  ℹ macOS: if Icebath re-prompts for permissions you already "+
			"granted (toggle shows ON), the stored grant is stale — run "+
			"`tccutil reset ScreenCapture ai.icebath` (repeat "+
			"per affected service), toggle it ON in System Settings, then "+
			"fully quit & relaunch once.")
	}
	if _, err := ensureMacosTCCAnchor(ctx, root); err != nil {
		updateLog.DebugContext(ctx, "macOS TCC anchor refresh skipped", "error", launchErrorText(err))
	}
	if err := bestEffort(ctx, "Post-update state.db integrity check failed: %s", func() error {
		return verifyAndRestoreStateDBsPostUpdate(ctx, w)
	}); err != nil {
		return false, err
	}
	kw := noPromptGitKwargs()
	fetched, err := tools.FetchFullCommitGraph(ctx, root, nil, pysubprocess.Options{Env: kw.Env, Attr: kw.Attr})
	switch {
	case err == nil && fetched:
		fmt.Fprintln(w, "  ✓ Fetched release history (commits only) for version identity")
	case err != nil && (pypath.IsOSError(err) || isSubprocessError(err)):
		fmt.Fprintf(w, "  ⚠ Could not refresh release history (%s); the version label may be stale or unknown until the next update\n", maintLastLine(err))
	case err != nil:
		return false, err
	}
	if err := bestEffort(ctx, "Commit-graph refresh failed to start: %s", func() error {
		_, err := tools.SpawnCommitGraphRefresh(ctx, root)
		return err
	}); err != nil {
		return false, err
	}
	if err := bestEffort(ctx, "Model catalog seed during update failed: %s", func() error {
		if provider.SeedCatalogCacheFromCheckout(ctx, root) {
			fmt.Fprintln(w, "  ✓ Model catalog cache refreshed from checkout")
		}
		return nil
	}); err != nil {
		return false, err
	}
	invalidateLivePluginCatalogCaches(ctx)
	if err := bestEffort(ctx, "Skills sync during update failed: %s", func() error {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "→ Syncing bundled skills...")
		return printBundledSkillsSyncReport(ctx, w)
	}); err != nil {
		return false, err
	}
	owedStep("profile_sync", func() error { return maintSyncProfiles(ctx, w) })
	owedStep("config_migration", func() error {
		return maintConfigMigration(ctx, w, o.AssumeYes, o.GatewayMode, o.PreUpdateSnapshotID)
	})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	fmt.Fprintln(w)
	message := ""
	if o.CompletionMessage != nil {
		message = *o.CompletionMessage
	}
	if message == "" {
		message = updateCompleteMessage(ctx, o.PreUpdateVersion)
	}
	complete := printVerifiedUpdateCompletion(ctx, w, message)
	for _, line := range append(consumeRewrittenNotice(), recordedStandaloneWarningLines(ctx)...) {
		fmt.Fprintln(w, line)
	}
	if err := bestEffort(ctx, "Post-update notices failed: %s", func() error { return maintNoticesAndSelfHeals(ctx, w) }); err != nil {
		return false, err
	}
	given := "✓"
	if o.CompletionMessage != nil && *o.CompletionMessage != "" {
		given = *o.CompletionMessage
	}
	if !complete && strings.HasPrefix(given, "✓") {
		updaterecord.RecordFollowup(ctx, "sqlite_runtime", "the selected Python links an unsafe SQLite runtime", "run the installer again")
	}
	return complete, nil
}

// Last line of a failed git run's stderr, else of its message, else its class name
func maintLastLine(err error) string {
	text := launchErrorText(err)
	if called, ok := asCalledProcessError(err); ok {
		if called.Stderr != "" {
			text = called.Stderr
		}
	}
	lines := pystr.Splitlines(pystr.Strip(text), false)
	if len(lines) == 0 {
		return pm.ErrorClass(err)
	}
	return lines[len(lines)-1]
}

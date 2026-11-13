package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Removes flag and its value from parts, false when the value is missing
func takeFlag(parts []string, flag string) ([]string, string, bool, bool) {
	for i, p := range parts {
		if p != flag {
			continue
		}
		if i+1 >= len(parts) {
			return parts, "", false, false
		}
		rest := append(append([]string(nil), parts[:i]...), parts[i+2:]...)
		return rest, parts[i+1], true, true
	}
	return parts, "", false, true
}

// First paths joined by commas with a more suffix past the limit
func (c *CLI) summarizePaths(paths []string, limit int) string {
	more := ""
	if len(paths) > limit {
		more = c.commandsT("shared.more_suffix", map[string]any{"count": len(paths) - limit})
	}
	return strings.Join(paths[:min(limit, len(paths))], ", ") + more
}

// Text cut to the limit with an ellipsis when truncated
func ellipsize(text string, limit int) string {
	if pystr.Len(text) > limit {
		return pystr.Slice(text, 0, limit) + "..."
	}
	return pystr.Slice(text, 0, limit)
}

// String items of a result list
func resultStrings(v any) []string {
	var out []string
	switch items := v.(type) {
	case []string:
		return items
	case []any:
		for _, item := range items {
			out = append(out, pyrepr.Str(item))
		}
	}
	return out
}

// Prints the kept and not removed path lines of a restore result
func (c *CLI) rollbackKept(result map[string]any) {
	if skipped := resultStrings(result["skipped_user_edits"]); len(skipped) > 0 {
		printLines(catalogLines(c.gatewayT("rollback.kept_user_edits", map[string]any{"files": c.summarizePaths(skipped, 5)}))...)
	}
	if oversize := resultStrings(result["skipped_oversize"]); len(oversize) > 0 {
		printLines("  " + c.gatewayT("rollback.kept_oversize", map[string]any{"files": c.summarizePaths(oversize, 5)}))
	}
	if failed := resultStrings(result["failed_deletes"]); len(failed) > 0 {
		printLines("  " + c.gatewayT("rollback.failed_deletes", map[string]any{"files": c.summarizePaths(failed, 5)}))
	}
}

// Working directory the checkpoint and diff commands read, TERMINAL_CWD before the process cwd
func terminalCwd() string {
	if cwd, ok := os.LookupEnv("TERMINAL_CWD"); ok {
		return cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

// Handles /rollback, listing, diffing or restoring checkpoints, a restore also undoing the last chat turn
func (c *CLI) handleRollbackCommand(command string) {
	if connection := gatewayConnectionFor(c); connection != nil {
		gatewayRollback(c, connection, command)
		return
	}
	c.checkpointManager()
}

// Prints that no agent session exists, a gateway agent holding no manager raising on its enabled check as main does
func (c *CLI) checkpointManager() {
	if c.agentRef() == nil {
		printLines("  " + c.commandsT("shared.no_active_agent_session", nil))
		return
	}
	panic(fmt.Errorf("'NoneType' object has no attribute 'enabled'"))
}

var diffModes = map[string]string{
	"staged": "staged", "--staged": "staged", "cached": "staged", "--cached": "staged",
	"all": "all", "--all": "all", "head": "all", "session": "session",
}

var diffLabels = map[string]string{"working": "diff.label_working", "staged": "diff.label_staged", "all": "diff.label_all"}

// Handles /diff, git changes in the cwd or everything changed since the checkpoint baseline
func (c *CLI) handleDiffCommand(command string) {
	statOnly, mode := false, "working"
	var paths []string
	for _, arg := range shlexArgs(command) {
		low := pystr.Lower(arg)
		if low == "--stat" || low == "stat" {
			statOnly = true
		} else if m, ok := diffModes[low]; ok {
			mode = m
		} else {
			paths = append(paths, arg)
		}
	}
	cwd := terminalCwd()
	if mode == "session" {
		c.printSessionDiffFor()
		return
	}
	result := tools.CollectWorkingDiff(cwd, mode, paths)
	if !result.Success {
		printLines("  " + firstNonEmpty(result.Error, c.commandsT("shared.could_not_generate_diff", nil)))
		return
	}
	if result.Empty || (result.Stat == "" && result.Diff == "" && len(result.Untracked) == 0) {
		printLines("  " + c.gatewayT("diff.no_changes", nil))
		return
	}
	if result.Stat != "" {
		printLines("\n  " + c.commandsT(diffLabels[mode], nil) + ":")
		c.printDiffText(result.Stat)
	}
	if len(result.Untracked) > 0 && (mode == "working" || mode == "all") {
		printLines("\n  " + c.commandsT("diff.untracked", nil))
		for _, rel := range result.Untracked[:min(20, len(result.Untracked))] {
			printLines("    + " + rel)
		}
		if len(result.Untracked) > 20 {
			printLines("    " + c.commandsT("diff.and_more", map[string]any{"count": len(result.Untracked) - 20}))
		}
	}
	if result.Diff != "" && !statOnly {
		c.printDiffBody(result.Diff, c.commandsT("diff.stat_hint", nil), 400, 0)
	}
}

// Prints a diff capped at limit lines of total with a pointer to the stat form
func (c *CLI) printDiffBody(diff, statHint string, limit, total int) {
	printLines("")
	lines := pystr.Splitlines(diff, false)
	total = max(total, len(lines))
	shown := lines[:min(limit, len(lines))]
	if total > len(shown) {
		c.printDiffText(strings.Join(shown, "\n"))
		printLines("\n  " + c.commandsT("diff.more_lines", map[string]any{"count": total - len(shown), "hint": statHint}))
		return
	}
	c.printDiffText(diff)
}

// Prints why the session diff is unavailable off the gateway, no local agent holding a checkpoint baseline
func (c *CLI) printSessionDiffFor() {
	c.checkpointManager()
}

// Renders a session diff result, stat then the body unless stat only
func (c *CLI) printSessionDiff(result map[string]any, statOnly bool) {
	if !cliTruthy(result["success"]) {
		printLines("  " + firstNonEmpty(cliStr(result["error"], ""), c.commandsT("shared.could_not_generate_diff", nil)))
		return
	}
	stat, diff := cliStr(result["stat"], ""), cliStr(result["diff"], "")
	if cliTruthy(result["empty"]) || (stat == "" && diff == "") {
		printLines("  " + c.commandsT("diff.no_session_changes", nil))
		return
	}
	if stat != "" {
		c.printDiffText("\n" + stat)
	}
	total := 0
	if n, ok := pyrepr.Int(result["diff_lines"]); ok && n != nil {
		total = int(n.Int64())
	}
	if (diff != "" || cliTruthy(result["diff_lines"])) && !statOnly {
		c.printDiffBody(diff, c.commandsT("diff.stat_hint_session", nil), 400, total)
	}
}

// Renders diff or stat text with its ANSI colors through the console
func (c *CLI) printDiffText(text string) {
	if c.console != nil {
		c.consolePrintRenderable(&pyrich.Text{Plain: text})
		return
	}
	fmt.Fprintln(console().Out, text)
}

// Handles /snapshot with list, create, restore and prune subcommands
func (c *CLI) handleSnapshotCommand(command string) {
	parts := strings.Fields(command)
	sub := "list"
	if len(parts) > 1 {
		sub = pystr.Lower(parts[1])
	}
	switch sub {
	case "list", "ls":
		c.snapshotList()
	case "create":
		c.snapshotCreate(parts)
	case "restore", "rewind":
		c.restoreSnapshotCommand(parts)
	case "prune":
		c.snapshotPrune(parts)
	default:
		printLines("  "+c.commandsT("snapshot.unknown_subcommand", map[string]any{"subcommand": sub}), "  "+c.commandsT("snapshot.usage", nil))
	}
}

// Size of a snapshot as bytes, kilobytes or megabytes
func snapshotSize(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return pyFormat(float64(size)/1024, ".0f") + " KB"
	}
	return pyFormat(float64(size)/1024/1024, ".1f") + " MB"
}

// Python format spec applied to a float
func pyFormat(f float64, spec string) string {
	text, err := pyrepr.Format(f, spec)
	if err != nil {
		panic(err)
	}
	return text
}

// Lists quick state snapshots in a table
func (c *CLI) snapshotList() {
	h := home.Dir(c.ctx)
	snaps, err := backup.ListQuickSnapshots(20, h)
	if err != nil {
		panic(err)
	}
	if len(snaps) == 0 {
		printLines("  "+c.commandsT("snapshot.none_yet", nil), "  "+c.commandsT("snapshot.create_hint", nil))
		return
	}
	printLines("  " + c.commandsT("snapshot.list_header", map[string]any{"dir": home.Display(h)}) + "\n")
	printLines(fmt.Sprintf("  %3s  %-35s %5s %10s %s", c.commandsT("snapshot.col_num", nil), c.commandsT("snapshot.col_id", nil),
		c.commandsT("snapshot.col_files", nil), c.commandsT("snapshot.col_size", nil), c.commandsT("snapshot.col_label", nil)),
		"  "+strings.Repeat("─", 3)+"  "+strings.Repeat("─", 35)+" "+strings.Repeat("─", 5)+" "+strings.Repeat("─", 10)+" "+strings.Repeat("─", 20))
	for i, raw := range snaps {
		s := plainMap(raw)
		size := int64(0)
		if n, ok := pyrepr.Int(mapValueOr(s, "total_size", 0)); ok && n != nil {
			size = n.Int64()
		}
		label := cliStr(s["label"], "")
		printLines(fmt.Sprintf("  %3d  %s %5s %10s %s", i+1, pystr.Ljust(pyrepr.Str(s["id"]), 35), pyrepr.Str(mapValueOr(s, "file_count", 0)), snapshotSize(size), label))
	}
}

// Creates a quick snapshot with an optional label
func (c *CLI) snapshotCreate(parts []string) {
	var label *string
	if len(parts) > 2 {
		joined := strings.Join(parts[2:], " ")
		label = &joined
	}
	id, err := backup.CreateQuickSnapshot(label, home.Dir(c.ctx), nil, nil)
	if err != nil {
		panic(err)
	}
	if id != "" {
		printLines("  " + c.commandsT("snapshot.created", map[string]any{"snapshot_id": id}))
		return
	}
	printLines("  " + c.commandsT("snapshot.nothing_to_snapshot", nil))
}

// Prunes quick snapshots down to the newest N, twenty by default
func (c *CLI) snapshotPrune(parts []string) {
	keep := 20
	if len(parts) > 2 {
		if !isDecimal(parts[2]) {
			printLines("  " + c.commandsT("snapshot.usage_prune", nil))
			return
		}
		keep, _ = parseQueueIndex(parts[2])
	}
	deleted, err := backup.PruneQuickSnapshots(keep, home.Dir(c.ctx))
	if err != nil {
		panic(err)
	}
	printLines("  " + c.commandsT("snapshot.pruned", map[string]any{"deleted": deleted, "keep": keep}))
}

// Reports a non empty string of decimal digits as str.isdecimal does
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if _, ok := decimalValue(r); !ok {
			return false
		}
	}
	return true
}

// Restores a quick snapshot by number or id, closing this process's store first
func (c *CLI) restoreSnapshotCommand(parts []string) {
	h := home.Dir(c.ctx)
	if len(parts) < 3 {
		printLines("  " + c.commandsT("snapshot.usage_restore", nil))
		snaps, err := backup.ListQuickSnapshots(1, h)
		if err != nil {
			panic(err)
		}
		if len(snaps) > 0 {
			printLines("  " + c.commandsT("snapshot.most_recent", map[string]any{"snapshot_id": plainMap(snaps[0])["id"]}))
		}
		return
	}
	snapID := parts[2]
	if idx, ok := parsePyInt(snapID); ok {
		snaps, err := backup.ListQuickSnapshots(20, h)
		if err != nil {
			panic(err)
		}
		if idx < 1 || idx > len(snaps) {
			printLines("  " + c.commandsT("snapshot.invalid_number", map[string]any{"max": len(snaps)}))
			return
		}
		snapID = pyrepr.Str(plainMap(snaps[idx-1])["id"])
	}
	store := c.gatewayStore()
	chat := store != nil && c.gatewaySessionOpen
	onGateway := chat && strings.TrimSpace(os.Getenv("ICEBATH_TUI_GATEWAY_URL")) == ""
	var restored bool
	var err error
	gatewayTarget := false
	if onGateway {
		dir, derr := backup.QuickSnapshotDir(snapID, h)
		gatewayTarget, err = dir != "", derr
	}
	switch {
	case err != nil:
	case gatewayTarget:
		restored, err = c.restoreGatewaySnapshot(store, snapID)
	default:
		if c.sessionDB != nil && !chat {
			c.sessionDB.Close()
			c.sessionDB = nil
		}
		restored, err = backup.RestoreQuickSnapshot(c.ctx, snapID, h)
	}
	if err != nil {
		cliLogger.Warn(fmt.Sprintf("Snapshot restore failed: %v", err))
		printLines("  "+c.commandsT("snapshot.restore_incomplete", map[string]any{"snapshot_id": snapID}), "  "+err.Error())
		return
	}
	if restored {
		printLines("  " + c.commandsT("snapshot.restored", map[string]any{"snapshot_id": snapID}))
		if !onGateway {
			printLines("  " + c.commandsT("snapshot.restart_recommended", nil))
		}
		return
	}
	all, err := backup.ListQuickSnapshots(1000000, h)
	if err != nil {
		panic(err)
	}
	for _, s := range all {
		if pyrepr.Str(plainMap(s)["id"]) == snapID {
			printLines("  " + c.commandsT("snapshot.restore_incomplete", map[string]any{"snapshot_id": snapID}))
			return
		}
	}
	printLines("  " + c.commandsT("snapshot.not_found", map[string]any{"snapshot_id": snapID}))
}

// Plain map of a decoded JSON row
func plainMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if m, ok := pyjson.Plain(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Handles /export, a profile archived to a shareable tar.gz
func (c *CLI) handleExportCommand(command string) {
	fields := strings.Fields(command)
	parts, output, _, ok := takeFlag(fields[1:], "-o")
	if !ok {
		printLines("  " + c.commandsT("export.usage", nil))
		return
	}
	name := profiles.GetActiveProfileName(c.ctx)
	if len(parts) > 0 {
		name = parts[0]
	}
	if name == "" {
		name = "default"
	}
	result, err := func() (string, error) {
		target := output
		if target == "" {
			path, err := manage.GetProfileExportPath(name, "")
			if err != nil {
				return "", err
			}
			target = path
		}
		return manage.ExportProfile(name, target, nil)
	}()
	if err != nil {
		printLines("  " + c.commandsT("shared.error", map[string]any{"error": err.Error()}))
		return
	}
	printLines("  "+c.commandsT("export.exported", map[string]any{"name": name, "path": result}), "  "+c.commandsT("export.share_hint", nil))
}

// Handles /import, a shared profile archive imported as a new profile with its wrapper alias
func (c *CLI) handleImportCommand(command string) {
	fields := strings.Fields(command)
	parts, name, _, ok := takeFlag(fields[1:], "--name")
	if !ok || len(parts) == 0 {
		printLines("  " + c.commandsT("import.usage", nil))
		return
	}
	profileDir, err := manage.ImportProfile(strings.Join(parts, " "), name)
	if err != nil {
		printLines("  " + c.commandsT("shared.error", map[string]any{"error": err.Error()}))
		return
	}
	imported := filepath.Base(profileDir)
	printLines("  " + c.commandsT("import.imported", map[string]any{"name": imported, "path": profileDir}))
	func() {
		defer func() { _ = recover() }()
		collision, err := profiles.CheckAliasCollision(imported)
		if err != nil || collision != "" {
			return
		}
		if wrapper, err := profiles.CreateWrapperScript(imported, ""); err == nil && wrapper != "" {
			printLines("  " + c.commandsT("import.wrapper_created", map[string]any{"path": wrapper}))
		}
	}()
	printLines("  " + c.commandsT("import.use_it", map[string]any{"name": imported}))
}

// Prints the lines and keeps the CLI session
func handoffKeep(lines ...string) bool {
	cp(lines...)
	return true
}

// Handles /handoff, transferring this session to a gateway platform, false only once the transfer completed
func (c *CLI) handleHandoffCommand(cmdOriginal string) bool {
	platformName := pystr.Lower(commandArg(cmdOriginal, false))
	if platformName == "" {
		return handoffKeep("  "+c.commandsT("handoff.usage", nil), "  "+c.commandsT("handoff.usage_detail_1", nil),
			"  "+c.commandsT("handoff.usage_detail_2", nil))
	}
	homeChannel := c.handoffValidateTarget(platformName)
	if homeChannel == nil {
		return true
	}
	sessionTitle, ok := c.handoffPrepareSession()
	if !ok {
		return true
	}
	requested, err := c.handoffSessionDB().RequestHandoff(c.ctx, c.sessionID, platformName)
	if err != nil {
		panic(err)
	}
	if !requested {
		return handoffKeep("  " + c.commandsT("handoff.already_in_flight", nil))
	}
	cp("  "+c.commandsT("handoff.queued", map[string]any{"title": sessionTitle, "platform": platformName, "home": homeChannel.Name}),
		"  "+c.commandsT("handoff.waiting", nil))
	return c.handoffWait(platformName, sessionTitle)
}

// Home channel of the destination platform, nil after printing why it is unknown, disabled or homeless
func (c *CLI) handoffValidateTarget(platformName string) *gateway.HomeChannel {
	platform, err := gateway.ParsePlatform(c.ctx, platformName)
	if err != nil {
		cp("  " + c.commandsT("handoff.unknown_platform", map[string]any{"platform": platformName}))
		return nil
	}
	cfg, err := gateway.LoadGatewayConfig(c.ctx)
	if err != nil {
		cp("  " + c.commandsT("handoff.config_load_failed", map[string]any{"error": err.Error()}))
		return nil
	}
	if pcfg := cfg.Platforms[platform]; pcfg == nil || !pcfg.Enabled {
		relayFronts := false
		func() {
			defer func() { _ = recover() }()
			if relay := cfg.Platforms[gateway.PlatformRelay]; relay != nil && relay.Enabled {
				for _, identity := range gateway.RelayPlatformIdentities() {
					if identity[0] == platformName {
						relayFronts = true
					}
				}
			}
		}()
		if !relayFronts {
			cp("  " + c.commandsT("handoff.platform_not_enabled", map[string]any{"platform": platformName}))
			return nil
		}
	}
	homeChannel := cfg.GetHomeChannel(platform)
	if homeChannel == nil || homeChannel.ChatID == "" {
		cp("  "+c.commandsT("handoff.no_home_channel", map[string]any{"platform": platformName}), "  "+c.commandsT("handoff.set_home_hint", nil))
		return nil
	}
	return homeChannel
}

// Session store verbs a /handoff calls, met by the gateway store and by a registry database
type handoffStore interface {
	GetSession(ctx context.Context, sessionID string) (map[string]any, error)
	SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error)
	RequestHandoff(ctx context.Context, sessionID, platform string) (bool, error)
	GetHandoffState(ctx context.Context, sessionID string) *pyjson.Object
	FailHandoff(ctx context.Context, sessionID, reason string, onlyStates []string) (bool, error)
}

var (
	_ handoffStore = (*gatewaycli.GatewaySessionStore)(nil)
	_ handoffStore = registryHandoffStore{}
)

// Registry database answering get_session with the plain mapping the gateway store returns
type registryHandoffStore struct{ *state.SessionDB }

// Session row as a plain mapping, nil when absent
func (r registryHandoffStore) GetSession(ctx context.Context, sessionID string) (map[string]any, error) {
	row, err := r.SessionDB.GetSession(ctx, sessionID)
	if err != nil || row == nil {
		return nil, err
	}
	plain := make(map[string]any, row.Len())
	for _, key := range row.Keys {
		plain[key], _ = row.Get(key)
	}
	return plain, nil
}

// Seconds clock and sleep of the handoff poll, the wall clock in production
var (
	handoffTime  = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	handoffSleep = time.Sleep
)

const (
	handoffPendingTimeout = 60.0
	handoffRunningTimeout = 900.0
	handoffHeartbeatEvery = 30.0
)

// Store a /handoff runs against, the registry database it acquired or the live gateway store, nil when neither
func (c *CLI) handoffSessionDB() handoffStore {
	if c.handoffDB != nil {
		return c.handoffDB
	}
	if c.sessionDB != nil && c.sessionDB.Bool() {
		return c.sessionDB
	}
	return nil
}

// Refuses mid turn and makes sure a session row exists, the display title or false after printing why not
func (c *CLI) handoffPrepareSession() (string, bool) {
	if c.agentRunning {
		cp("  " + c.commandsT("shared.agent_busy", map[string]any{"command": "/handoff"}))
		return "", false
	}
	if c.handoffSessionDB() == nil {
		if db, err := state.Acquire(c.ctx, ""); err == nil {
			c.handoffDB = registryHandoffStore{db}
		}
	}
	db := c.handoffSessionDB()
	if db == nil {
		cp(c.dbUnavailableLine())
		return "", false
	}
	meta, err := db.GetSession(c.ctx, c.sessionID)
	if err == nil && len(meta) == 0 {
		_, err = db.SetSessionTitle(c.ctx, c.sessionID, "handoff-"+pystr.Slice(c.sessionID, 0, 8))
	}
	if err != nil {
		cp("  " + c.commandsT("handoff.session_row_failed", map[string]any{"error": err.Error()}))
		return "", false
	}
	sessionTitle := ""
	if meta, err := db.GetSession(c.ctx, c.sessionID); err == nil {
		if title, _ := meta["title"].(string); title != "" {
			sessionTitle = title
		}
	}
	if sessionTitle == "" {
		sessionTitle = pystr.Slice(c.sessionID, 0, 8)
	}
	return sessionTitle, true
}

// Polls the handoff row, pending for a minute then failed for a retry, running for fifteen minutes and never failed
func (c *CLI) handoffWait(platformName, sessionTitle string) bool {
	db := c.handoffSessionDB()
	pendingDeadline := handoffTime() + handoffPendingTimeout
	var runningDeadline, nextHeartbeat float64
	running := false
	lastState := "pending"
	for {
		stateRow := db.GetHandoffState(c.ctx, c.sessionID)
		current := "pending"
		if stateRow != nil {
			if value, _ := stateRow.Get("state"); pyrepr.Truthy(value) {
				current = pyrepr.Str(value)
			}
		}
		if current != lastState {
			if current == "running" {
				cp("  " + c.commandsT("handoff.picked_up", nil))
				runningDeadline = handoffTime() + handoffRunningTimeout
				nextHeartbeat = handoffTime() + handoffHeartbeatEvery
				running = true
			}
			lastState = current
		}
		if current == "completed" {
			cp("", "  "+c.commandsT("handoff.complete", map[string]any{"platform": platformName}),
				"  "+c.commandsT("handoff.resume_hint", map[string]any{"title": sessionTitle}), "")
			markSessionHandedOff(c.sessionID)
			c.setShouldExit(false)
			return false
		}
		if current == "failed" {
			var reason any = c.commandsT("handoff.unknown_error", nil)
			if value, _ := stateRow.Get("error"); pyrepr.Truthy(value) {
				reason = value
			}
			return handoffKeep("  "+c.commandsT("handoff.failed", map[string]any{"error": reason}),
				"  "+c.commandsT("handoff.session_intact_retry", nil))
		}
		now := handoffTime()
		if current == "pending" {
			if now >= pendingDeadline {
				break
			}
		} else {
			if running && now >= nextHeartbeat {
				cp("  " + c.commandsT("handoff.still_transferring", nil))
				nextHeartbeat = now + handoffHeartbeatEvery
			}
			if running && now >= runningDeadline {
				return handoffKeep("  "+c.commandsT("handoff.taking_long", nil),
					"  "+c.commandsT("handoff.check_platform", map[string]any{"platform": platformName}),
					"  "+c.commandsT("handoff.no_longer_waiting", nil), "  "+c.commandsT("handoff.retry_when_settled", nil))
			}
		}
		handoffSleep(500 * time.Millisecond)
	}
	_, _ = db.FailHandoff(c.ctx, c.sessionID, "timed out waiting for gateway", []string{"pending"})
	return handoffKeep("  "+c.commandsT("handoff.timed_out", nil), "  "+c.commandsT("handoff.session_intact", nil))
}

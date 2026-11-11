package kanban

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Kanban section of config.yaml, empty when the config cannot be loaded or the section is falsy
func (c *kanbanCLI) kanbanConfig() map[string]any {
	cfg, err := loadKanbanConfigTree(c.ctx, false)
	if err != nil {
		return map[string]any{}
	}
	section, ok := cfg["kanban"].(map[string]any)
	if !ok || len(section) == 0 {
		return map[string]any{}
	}
	return section
}

// Runs tick every interval seconds with a floor of 0.1 until Ctrl-C
func (c *kanbanCLI) pollLoop(interval float64, tick func() error) (int, error) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	for {
		if err := tick(); err != nil {
			return 0, err
		}
		select {
		case <-sigs:
			fmt.Fprintln(c.w, "\n(stopped)")
			return 0, nil
		case <-c.ctx.Done():
			fmt.Fprintln(c.w, "\n(stopped)")
			return 0, nil
		case <-time.After(time.Duration(max(0.1, interval) * float64(time.Second))):
		}
	}
}

// Follows one task's event stream, printing each new event once
func (c *kanbanCLI) cmdTail() (int, error) {
	taskID := nsStr(c.ns, "task_id")
	var lastID int64
	fmt.Fprintf(c.w, "Tailing events for %s. Ctrl-C to stop.\n", taskID)
	return c.pollLoop(nsFloat(c.ns, "interval"), func() error {
		var events []*Event
		if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			var err error
			events, err = ListEvents(c.ctx, conn, taskID)
			return err
		}); err != nil {
			return err
		}
		for _, e := range events {
			if e.ID > lastID {
				pl := ""
				if pyrepr.Truthy(e.Payload) {
					pl = " " + pyrepr.Str(e.Payload)
				}
				fmt.Fprintf(c.w, "[%s] %s%s\n", fmtTS(&e.CreatedAt), e.Kind, pl)
				lastID = e.ID
			}
		}
		return nil
	})
}

// Optional positive integer of a config value, nil for anything below one or unparseable
func positiveIntOpt(value any) *int {
	n, err := pyint.Of(value)
	if err != nil || n.Sign() < 1 {
		return nil
	}
	v := int(n.Int64())
	if !n.IsInt64() {
		v = int(^uint(0) >> 1)
	}
	return &v
}

// Dispatch knobs from the kanban config, the CLI --max winning over kanban.max_spawn
func (c *kanbanCLI) dispatchSettings() (defaultAssignee *string, perProfile, maxInProgress, maxSpawn *int) {
	cliMax := nsOptIntInt(c.ns, "max")
	cfg, err := loadKanbanConfigTree(c.ctx, false)
	if err != nil {
		return nil, nil, nil, cliMax
	}
	section, present := cfg["kanban"]
	if !present {
		section = map[string]any{}
	}
	kanbanCfg, ok := section.(map[string]any)
	if !ok {
		return nil, nil, nil, cliMax
	}
	raw := kanbanCfg["default_assignee"]
	if !pyrepr.Truthy(raw) {
		raw = ""
	}
	text, ok := raw.(string)
	if !ok {
		return nil, nil, nil, cliMax
	}
	if text = strings.TrimSpace(text); text != "" {
		defaultAssignee = &text
	}
	perProfile = positiveIntOpt(kanbanCfg["max_in_progress_per_profile"])
	maxInProgress = ResolveMaxInProgress(positiveIntOpt(kanbanCfg["max_in_progress"]))
	maxSpawn = cliMax
	if maxSpawn == nil {
		maxSpawn = positiveIntOpt(kanbanCfg["max_spawn"])
	}
	return defaultAssignee, perProfile, maxInProgress, maxSpawn
}

// Failure limit of the namespace, the dispatcher default when absent
func (c *kanbanCLI) failureLimit() int64 {
	if n := nsOptInt(c.ns, "failure_limit"); n != nil {
		return *n
	}
	return DefaultFailureLimit
}

// Runs one dispatcher pass and prints its outcome as text or JSON
func (c *kanbanCLI) cmdDispatch() (int, error) {
	defaultAssignee, perProfile, maxInProgress, maxSpawn := c.dispatchSettings()
	dryRun := nsBool(c.ns, "dry_run")
	var res *DispatchResult
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		o := DefaultDispatchOptions()
		o.DryRun, o.MaxSpawn, o.MaxInProgress, o.FailureLimit = dryRun, maxSpawn, maxInProgress, c.failureLimit()
		o.DefaultAssignee, o.MaxInProgressPerProfile = defaultAssignee, perProfile
		var err error
		res, err = DispatchOnce(c.ctx, conn, o)
		return err
	}); err != nil {
		return 0, err
	}
	if nsBool(c.ns, "json") {
		out := pyjson.NewObject()
		out.Set("reclaimed", res.Reclaimed)
		out.Set("crashed", stringsAny(res.Crashed))
		out.Set("timed_out", stringsAny(res.TimedOut))
		out.Set("stale", stringsAny(res.Stale))
		out.Set("auto_blocked", stringsAny(res.AutoBlocked))
		out.Set("promoted", res.Promoted)
		out.Set("reaped_terminal_workers", stringsAny(res.ReapedTerminalWorkers))
		spawned := []any{}
		for _, s := range res.Spawned {
			spawned = append(spawned, pyjson.ObjectOf("task_id", s.TaskID, "assignee", s.Assignee, "workspace", s.WorkspacePath))
		}
		out.Set("spawned", spawned)
		out.Set("skipped_unassigned", stringsAny(res.SkippedUnassigned))
		out.Set("skipped_nonspawnable", stringsAny(res.SkippedNonspawnable))
		capped := []any{}
		for _, s := range res.SkippedPerProfileCapped {
			capped = append(capped, pyjson.ObjectOf("task_id", s.TaskID, "assignee", s.Assignee, "current", s.Running))
		}
		out.Set("skipped_per_profile_capped", capped)
		out.Set("auto_assigned_default", stringsAny(res.AutoAssignedDefault))
		guarded := []any{}
		for _, g := range res.RespawnGuarded {
			guarded = append(guarded, pyjson.ObjectOf("task_id", g.TaskID, "reason", g.Reason))
		}
		out.Set("respawn_guarded", guarded)
		out.Set("rate_limited", stringsAny(res.RateLimited))
		out.Set("skipped_locked", res.SkippedLocked)
		out.Set("memory_pressure", optStrValue(res.MemoryPressure))
		return 0, printJSON(c.w, out, true)
	}
	fmt.Fprintf(c.w, "Reclaimed:    %d\n", res.Reclaimed)
	if len(res.ReapedTerminalWorkers) > 0 {
		fmt.Fprintf(c.w, "Reaped workers of finished tasks: %s\n", strings.Join(res.ReapedTerminalWorkers, ", "))
	}
	for _, group := range []struct {
		label string
		items []string
	}{{"Crashed:     ", res.Crashed}, {"Timed out:   ", res.TimedOut}, {"Stale:       ", res.Stale}, {"Auto-blocked:", res.AutoBlocked}} {
		fmt.Fprintf(c.w, "%s %d\n", group.label, len(group.items))
		if len(group.items) > 0 {
			fmt.Fprintf(c.w, "  %s\n", strings.Join(group.items, ", "))
		}
	}
	fmt.Fprintf(c.w, "Promoted:     %d\n", res.Promoted)
	fmt.Fprintf(c.w, "Spawned:      %d\n", len(res.Spawned))
	tag := ""
	if dryRun {
		tag = " (dry)"
	}
	for _, s := range res.Spawned {
		ws := s.WorkspacePath
		if ws == "" {
			ws = "-"
		}
		fmt.Fprintf(c.w, "  - %s  ->  %s  @ %s%s\n", s.TaskID, s.Assignee, ws, tag)
	}
	if len(res.AutoAssignedDefault) > 0 {
		var who any
		if defaultAssignee != nil {
			who = *defaultAssignee
		}
		fmt.Fprintf(c.w, "Auto-assigned to kanban.default_assignee=%s: %s\n", pyrepr.Repr(who), strings.Join(res.AutoAssignedDefault, ", "))
	}
	if len(res.SkippedUnassigned) > 0 {
		fmt.Fprintf(c.w, "Skipped (unassigned): %s\n", strings.Join(res.SkippedUnassigned, ", "))
	}
	for _, s := range res.SkippedPerProfileCapped {
		fmt.Fprintf(c.w, "Deferred (%s at per-profile cap, %d running): %s\n", s.Assignee, s.Running, s.TaskID)
	}
	if len(res.SkippedNonspawnable) > 0 {
		fmt.Fprintf(c.w, "Skipped (non-spawnable assignee — terminal lane, OK): %s\n", strings.Join(res.SkippedNonspawnable, ", "))
	}
	for _, g := range res.RespawnGuarded {
		fmt.Fprintf(c.w, "Guarded (%s): %s\n", g.Reason, g.TaskID)
	}
	if len(res.RateLimited) > 0 {
		fmt.Fprintf(c.w, "Rate-limited (released to ready, no failure counted): %s\n", strings.Join(res.RateLimited, ", "))
	}
	if res.SkippedLocked {
		fmt.Fprintln(c.w, "Skipped: another dispatcher holds this board's lock (no writes this tick)")
	}
	if res.MemoryPressure != nil && *res.MemoryPressure != "" {
		fmt.Fprintf(c.w, "Memory pressure %s: new workers restricted this tick\n", *res.MemoryPressure)
	}
	return 0, nil
}

const daemonDeprecated = "icebath kanban daemon: DEPRECATED — the dispatcher now runs\ninside the gateway. To use " +
	"kanban:\n\n    icebath gateway start       # starts the gateway + embedded dispatcher\n\nReady " +
	"tasks will be picked up on the next dispatcher tick\n(default: every 60 seconds). Configure " +
	"via config.yaml:\n\n    kanban:\n      dispatch_in_gateway: true      # default\n      " +
	"dispatch_interval_seconds: 60\n      failure_limit: 2              # consecutive non-success " +
	"attempts before auto-block\n\nRunning both the gateway AND this standalone daemon will\nrace " +
	"for claims. If you truly need the old standalone\ndaemon (no gateway available), rerun with " +
	"--force."

const daemonHealthWindow = 6

// Standalone dispatcher loop behind --force, the deprecation notice and exit 2 otherwise
func (c *kanbanCLI) cmdDaemon() (int, error) {
	if !nsBool(c.ns, "force") {
		return cliErr(c.errw, daemonDeprecated, 2), nil
	}
	if _, err := InitDB(c.ctx, "", nil); err != nil {
		return 0, err
	}
	pidfile := nsStr(c.ns, "pidfile")
	if pidfile != "" {
		err := os.MkdirAll(filepath.Dir(pidfile), 0o777)
		if err == nil {
			err = os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o666)
		}
		if err != nil {
			fmt.Fprintf(c.errw, "warning: could not write pidfile %s: %s\n", pidfile, pyrepr.ExceptionMessage(err))
		}
	}
	verbose := nsBool(c.ns, "verbose")
	interval := nsFloat(c.ns, "interval")
	fmt.Fprintf(c.errw, "Kanban dispatcher running STANDALONE via --force (interval=%ss, pid=%d). Ctrl-C to stop. NOTE: if a gateway is also running with dispatch_in_gateway=true (default), you have two dispatchers racing for claims.\n",
		pyrepr.Float(interval), os.Getpid())
	badTicks, lastWarnAt := 0, int64(0)
	readyQueueNonempty := func() bool {
		found := false
		_ = ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			var err error
			found, err = HasSpawnableReady(c.ctx, conn)
			return err
		})
		return found
	}
	onTick := func(res *DispatchResult) {
		readyPending := len(res.SkippedUnassigned) > 0 || readyQueueNonempty()
		if readyPending && len(res.Spawned) == 0 {
			badTicks++
		} else {
			badTicks = 0
		}
		if badTicks >= daemonHealthWindow {
			now := time.Now().Unix()
			if now-lastWarnAt >= 300 {
				held := DescribeSuppression([]*DispatchResult{res})
				if held != "" {
					held = " Last tick held back: " + held + "."
				}
				fmt.Fprintf(c.errw, "[%s] WARN dispatcher stuck: ready queue non-empty for %d consecutive ticks but 0 workers spawned successfully.%s Check profile health (venv, PATH, credentials) and `icebath kanban list --status ready` / `icebath kanban list --status blocked` for recent spawn_failed tasks.\n",
					fmtTS(&now), badTicks, held)
				lastWarnAt = now
			}
		}
		if !verbose {
			return
		}
		if res.Reclaimed != 0 || len(res.Crashed) > 0 || len(res.TimedOut) > 0 || res.Promoted != 0 ||
			len(res.Spawned) > 0 || len(res.AutoBlocked) > 0 || len(res.Stale) > 0 {
			now := time.Now().Unix()
			fmt.Fprintf(c.w, "[%s] reclaimed=%d crashed=%d timed_out=%d stale=%d promoted=%d spawned=%d auto_blocked=%d\n",
				fmtTS(&now), res.Reclaimed, len(res.Crashed), len(res.TimedOut), len(res.Stale), res.Promoted, len(res.Spawned), len(res.AutoBlocked))
		}
	}
	RunDaemon(c.ctx, DaemonOptions{
		Interval: interval, MaxSpawn: nsOptIntInt(c.ns, "max"), FailureLimit: c.failureLimit(),
		Stop: c.ctx.Done(), OnTick: onTick, Stderr: c.errw,
	})
	if pidfile != "" {
		_ = os.Remove(pidfile)
	}
	fmt.Fprintln(c.w, "(dispatcher stopped)")
	return 0, nil
}

// Live streams task events of the board, filtered by kind, assignee and tenant
func (c *kanbanCLI) cmdWatch() (int, error) {
	var kinds map[string]bool
	if raw := nsStr(c.ns, "kinds"); raw != "" {
		kinds = map[string]bool{}
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kinds[k] = true
			}
		}
		if len(kinds) == 0 {
			kinds = nil
		}
	}
	assignee, tenant := nsStr(c.ns, "assignee"), nsStr(c.ns, "tenant")
	fmt.Fprintf(c.w, "Watching kanban events (initial board '%s'). Ctrl-C to stop.\n", GetCurrentBoard(c.ctx))
	var cursor int64
	if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		row, err := QueryOne(c.ctx, conn, "SELECT COALESCE(MAX(id), 0) AS m FROM task_events")
		if err != nil {
			return err
		}
		n, err := pyint.Of(row.Get("m"))
		if err != nil {
			return err
		}
		cursor = n.Int64()
		return nil
	}); err != nil {
		return 0, err
	}
	return c.pollLoop(nsFloat(c.ns, "interval"), func() error {
		var rows []*Row
		if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			var err error
			rows, err = QueryRows(c.ctx, conn, "SELECT e.id, e.task_id, e.kind, e.payload, e.created_at,        t.assignee, "+
				"t.tenant FROM task_events e LEFT JOIN tasks t ON t.id = e.task_id WHERE e.id > ? "+
				"ORDER BY e.id ASC LIMIT 200", cursor)
			return err
		}); err != nil {
			return err
		}
		for _, r := range rows {
			id, err := pyint.Of(r.Get("id"))
			if err != nil {
				return err
			}
			cursor = max(cursor, id.Int64())
			kind := pyrepr.Str(r.Get("kind"))
			if (kinds != nil && !kinds[kind]) || (assignee != "" && r.Get("assignee") != assignee) || (tenant != "" && r.Get("tenant") != tenant) {
				continue
			}
			var decoded any
			if text, ok := r.Get("payload").(string); ok && text != "" {
				if v, err := pyjson.Loads(text); err == nil {
					decoded = v
				}
			}
			pl := ""
			if pyrepr.Truthy(decoded) {
				pl = " " + pyrepr.Str(decoded)
			}
			who := "-"
			if a := r.Get("assignee"); pyrepr.Truthy(a) {
				who = pyrepr.Str(a)
			}
			fmt.Fprintf(c.w, "[%s] %-10s %-18s (@%s)%s\n", fmtTS(toEpoch(r.Get("created_at"))), pyrepr.Str(r.Get("task_id")), kind, who, pl)
		}
		return nil
	})
}

// Removes archived tasks' workspaces, old events and old worker logs
func (c *kanbanCLI) cmdGC() (int, error) {
	eventDays, logDays := int64(30), int64(30)
	if n := nsOptInt(c.ns, "event_retention_days"); n != nil {
		eventDays = *n
	}
	if n := nsOptInt(c.ns, "log_retention_days"); n != nil {
		logDays = *n
	}
	if eventDays < 0 || logDays < 0 {
		return cliErr(c.errw, "kanban gc: retention days must be >= 0 (0 disables that sweep)", 2), nil
	}
	scratchRoot, err := WorkspacesRoot(c.ctx, nil)
	if err != nil {
		return 0, err
	}
	removedWS := 0
	isDir := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.IsDir()
	}
	err = ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		rows, err := QueryRows(c.ctx, conn, "SELECT id, workspace_kind, workspace_path, branch_name FROM tasks WHERE status = 'archived'")
		if err != nil {
			return err
		}
		for _, row := range rows {
			id := pyrepr.Str(row.Get("id"))
			wsPath, _ := row.Get("workspace_path").(string)
			if row.Get("workspace_kind") == "worktree" {
				if wsPath != "" && isDir(wsPath) {
					deferred, err := deferSharedWorktreeCleanup(c.ctx, conn, id, wsPath)
					if err != nil {
						return err
					}
					if deferred {
						continue
					}
					cleanupWorktreeWorkspace(id, wsPath, cellOptStr(row.Get("branch_name")))
					if !isDir(wsPath) {
						removedWS++
					}
				}
				continue
			}
			if row.Get("workspace_kind") != "scratch" {
				continue
			}
			path := wsPath
			if path == "" {
				path = filepath.Join(scratchRoot, id)
			}
			if !isDir(path) {
				continue
			}
			if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
				continue
			}
			managed, err := isManagedScratchPath(path)
			if err != nil {
				return err
			}
			if !managed {
				continue
			}
			deferred, err := deferSharedWorkspaceCleanup(c.ctx, conn, id, path)
			if err != nil {
				return err
			}
			if deferred {
				continue
			}
			_ = os.RemoveAll(path)
			if _, err := os.Lstat(path); err != nil {
				removedWS++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var removedEvents int64
	if eventDays != 0 {
		if err := ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			var err error
			removedEvents, err = GCEvents(c.ctx, conn, eventDays*24*3600)
			return err
		}); err != nil {
			return 0, err
		}
	}
	removedLogs := 0
	if logDays != 0 {
		if removedLogs, err = GCWorkerLogs(c.ctx, logDays*24*3600, nil); err != nil {
			return 0, err
		}
	}
	fmt.Fprintf(c.w, "GC complete: %d workspace(s), %d event row(s), %d log file(s) removed\n", removedWS, removedEvents, removedLogs)
	return 0, nil
}

// Integrity check with the narrow index REINDEX repair, exit 1 while still corrupt
func (c *kanbanCLI) cmdRepair() (int, error) {
	report, err := RepairDB(c.ctx, "", nil)
	if err != nil {
		return cliErr(c.errw, "kanban repair: "+pyrepr.ExceptionMessage(err), 1), nil
	}
	if nsBool(c.ns, "json") {
		var backup any
		if report.BackupPath != "" {
			backup = report.BackupPath
		}
		out := pyjson.ObjectOf("status", report.Status, "db_path", report.DBPath, "messages", stringsAny(report.Messages),
			"post_repair_messages", stringsAny(report.PostRepairMessages), "backup_path", backup, "reindexed", stringsAny(report.Reindexed))
		if err := printJSON(c.w, out, true); err != nil {
			return 0, err
		}
		switch report.Status {
		case "ok", "repaired", "missing":
			return 0, nil
		}
		return 1, nil
	}
	switch report.Status {
	case "missing":
		fmt.Fprintf(c.w, "No kanban DB at %s — nothing to repair.\n", report.DBPath)
		return 0, nil
	case "ok":
		fmt.Fprintf(c.w, "%s: integrity_check ok — no repair needed.\n", report.DBPath)
		return 0, nil
	case "repaired":
		fmt.Fprintf(c.w, "%s: repaired.\n", report.DBPath)
		fmt.Fprintf(c.w, "  reindexed: %s\n", strings.Join(report.Reindexed, ", "))
		if report.BackupPath != "" {
			fmt.Fprintf(c.w, "  pre-repair backup: %s\n", report.BackupPath)
		}
		fmt.Fprintln(c.w, "  integrity_check now ok.")
		return 0, nil
	}
	errLine := func(line string) { fmt.Fprintln(c.errw, line) }
	errLine(report.DBPath + ": CORRUPT.")
	for _, line := range firstN(report.Messages, 10) {
		errLine("  " + line)
	}
	if len(report.Reindexed) > 0 {
		errLine("  REINDEX (" + strings.Join(report.Reindexed, ", ") + ") attempted but integrity_check is still failing:")
		for _, line := range firstN(report.PostRepairMessages, 10) {
			errLine("    " + line)
		}
	} else {
		errLine("  Not an index-only failure — automatic REINDEX repair does not apply (fail-closed).")
	}
	if report.BackupPath != "" {
		errLine("  corrupt copy quarantined at: " + report.BackupPath)
	}
	errLine("  Recover manually (copy kanban.db aside FIRST, then run " +
		"`sqlite3 <copy> \".recover\"` into a fresh file — never against " +
		"the live path, a WAL-reset-vulnerable sqlite3 CLI can corrupt it " +
		"further) or move the file aside to start a new board.")
	return 1, nil
}

// First n items of a list
func firstN(items []string, n int) []string {
	if len(items) > n {
		return items[:n]
	}
	return items
}

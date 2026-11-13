package kanban

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var respawnBlockerRE = regexp2.MustCompile(`\b(quota|rate[\s_\-]?limit|429|403|`+
	`auth|authenticat(?:e|es|ed|ing|ion)|authoriz(?:e|es|ed|ing|ation)|`+
	`authoris(?:e|es|ed|ing|ation)|authz|`+
	`unauthorized|forbidden|billing|subscription|`+
	`access[\s_]denied|permission[\s_]denied|`+
	`invalid[\s_]api[\s_]key)\b`, regexp2.IgnoreCase|regexp2.Unicode)

var respawnGuardPRURLRE = regexp2.MustCompile(`https?://github\.com/[^/\s]+/[^/\s]+/pull/\d+`, regexp2.IgnoreCase|regexp2.Unicode)

const (
	respawnGuardSuccessWindow     = 3600
	respawnGuardPRWindow          = 86400
	MemoryGuardMBPerWorker        = 512
	DerivedMaxInProgressFloor     = 2
	DerivedMaxInProgressCeiling   = 8
	defaultDispatchDaemonInterval = 60.0
)

// GatewayDispatchProbeSet is the gateway memory sampling and pressure classification the dispatcher reads
type GatewayDispatchProbeSet struct {
	SampleMemory     func() *pyjson.Object
	ClassifyPressure func(availableKiB, totalKiB any) string
	Liveness         func(ctx context.Context, profileDir string) (pid any, probeError bool)
}

// Gateway memory and liveness probes, installed by pkg/gateway at init
var GatewayDispatchProbes GatewayDispatchProbeSet

// Installed gateway dispatch probes, panicking when pkg/gateway never assigned them
func gatewayDispatch() GatewayDispatchProbeSet {
	p := GatewayDispatchProbes
	if p.SampleMemory == nil || p.ClassifyPressure == nil || p.Liveness == nil {
		panic("kanban.GatewayDispatchProbes is not installed: pkg/gateway assigns it at init")
	}
	return p
}

// Reason a ready or review task must not be respawned now, nil when it may be
func CheckRespawnGuard(ctx context.Context, conn state.SQLiteConn, taskID, lane string) (*string, error) {
	row, err := QueryOne(ctx, conn, "SELECT last_failure_error FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	reason := func(s string) (*string, error) { return &s, nil }
	now := time.Now().Unix()
	rlCooldown := resolveRateLimitCooldownSeconds()
	latestRun, err := QueryOne(ctx, conn, "SELECT outcome, ended_at, metadata FROM task_runs "+
		"WHERE task_id = ? AND ended_at IS NOT NULL ORDER BY ended_at DESC LIMIT 1", taskID)
	if err != nil {
		return nil, err
	}
	if latestRun != nil && latestRun.Get("outcome") == "spawn_failed" {
		infra, _ := jsonDict(latestRun.Get("metadata")).Get("infrastructure")
		if rlCooldown > 0 && pyrepr.Truthy(infra) {
			if endedAt := toEpoch(latestRun.Get("ended_at")); endedAt != nil && now-*endedAt < rlCooldown {
				return reason("infrastructure_cooldown")
			}
		}
	}
	if latestRun != nil && latestRun.Get("outcome") == "rate_limited" {
		if rlCooldown <= 0 {
			return nil, nil
		}
		if endedAt := toEpoch(latestRun.Get("ended_at")); endedAt != nil && now-*endedAt < rlCooldown {
			return reason("rate_limit_cooldown")
		}
		return nil, nil
	}
	errText, _ := LossyText(row.Get("last_failure_error")).(string)
	var latestOutcome any
	if latestRun != nil {
		latestOutcome = latestRun.Get("outcome")
	}
	if errText != "" && latestOutcome != "crashed" {
		if m, _ := respawnBlockerRE.MatchString(errText); m {
			return reason("blocker_auth")
		}
	}
	if lane == "review" {
		return nil, nil
	}
	successes, err := rowsSince(ctx, conn, "task_runs", taskID, now-respawnGuardSuccessWindow, "AND outcome = 'completed'", "", false, nil)
	if err != nil {
		return nil, err
	}
	if len(successes) > 0 {
		success := successes[0].t
		for _, s := range successes[1:] {
			success = max(success, s.t)
		}
		requeues, err := rowsSince(ctx, conn, "task_events", taskID, success, "AND kind IN ('status', 'promoted', 'unblocked', 'reclaimed')", "", false, nil)
		if err != nil {
			return nil, err
		}
		if len(requeues) == 0 {
			return reason("recent_success")
		}
	}
	comments, err := rowsSince(ctx, conn, "task_comments", taskID, now-respawnGuardPRWindow, "", ", body", false, &now)
	if err != nil {
		return nil, err
	}
	var prTimes []int64
	for _, c := range comments {
		body, _ := LossyText(c.row.Get("body")).(string)
		if m, _ := respawnGuardPRURLRE.MatchString(body); m {
			prTimes = append(prTimes, c.t)
		}
	}
	if len(prTimes) > 0 {
		handoffs, err := rowsSince(ctx, conn, "task_events", taskID, slices.Max(prTimes),
			"AND kind IN ('assigned', 'changes_requested', 'review_reopened')", ", kind, payload", true, nil)
		if err != nil {
			return nil, err
		}
		for _, e := range handoffs {
			kind, _ := e.row.Get("kind").(string)
			if isHandoffEvent(kind, e.row.Get("payload")) {
				return nil, nil
			}
		}
		return reason("active_pr")
	}
	return nil, nil
}

var guardTimeCols = map[string]string{"task_runs": "ended_at", "task_events": "created_at", "task_comments": "created_at"}

// timedRow is one guard row with its parsed epoch
type timedRow struct {
	t   int64
	row *Row
}

// Rows of a task table timed at or after since, after it when strict, unreadable stamps dropped unless substituted
func rowsSince(ctx context.Context, conn state.SQLiteConn, table, taskID string, since int64, where, cols string, strict bool, unreadable *int64) ([]timedRow, error) {
	col := guardTimeCols[table]
	op := ">="
	if strict {
		op = ">"
	}
	rows, err := QueryRows(ctx, conn, fmt.Sprintf("SELECT %s%s FROM %s WHERE task_id = ? %s AND %s %s ?", col, cols, table, where, col, op), taskID, since)
	if err != nil {
		return nil, err
	}
	var out []timedRow
	for _, row := range rows {
		t := toEpoch(row.Get(col))
		if t == nil {
			t = unreadable
		}
		if t != nil && ((strict && *t > since) || (!strict && *t >= since)) {
			out = append(out, timedRow{*t, row})
		}
	}
	return out, nil
}

// Whether an event hands the card to a different profile, an assigned event needing a recorded from that differs
func isHandoffEvent(kind string, payloadValue any) bool {
	if kind != "assigned" {
		return true
	}
	data, ok := jsonOr(payloadValue, pyjson.NewObject()).(*pyjson.Object)
	if !ok {
		return false
	}
	if src, _ := data.Get("source"); src == "kanban.default_assignee" {
		return false
	}
	to, _ := data.Get("assignee")
	from, hasFrom := data.Get("from")
	return pyrepr.Truthy(to) && hasFrom && !pyjson.Equal(from, to)
}

// Profile predicate the dispatcher spawns for, narrowed to kanban.dispatch_profiles when that key is set
func dispatchProfileExistsFn(ctx context.Context) func(string) bool {
	allowlist := dispatchProfileAllowlist(ctx)
	if allowlist == nil {
		return profileExistsFn
	}
	return func(name string) bool {
		canon, err := profiles.NormalizeProfileName(name)
		if err != nil {
			return false
		}
		return allowlist[canon] && profileExistsFn(name)
	}
}

// Claim allowlist of this home from kanban.dispatch_profiles, nil when the key is absent and empty when fail closed
func dispatchProfileAllowlist(ctx context.Context) map[string]bool {
	cfg, err := config.LoadUserEffectiveFailClosed(home.ConfigPath(home.Dir(ctx)), os.LookupEnv)
	if err != nil {
		log.Warn(fmt.Sprintf("kanban: could not read kanban.dispatch_profiles (%s: %s) — this home claims no cards until the config is readable",
			pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err)))
		return map[string]bool{}
	}
	var section any = map[string]any{}
	if cfg != nil {
		if v, ok := cfg["kanban"]; ok {
			section = v
		}
	}
	kanbanCfg, ok := section.(map[string]any)
	if !ok {
		return nil
	}
	raw, present := kanbanCfg["dispatch_profiles"]
	if !present {
		return nil
	}
	if s, isStr := raw.(string); raw == nil || (isStr && strings.TrimSpace(s) == "") {
		log.Warn("kanban: kanban.dispatch_profiles is present but empty — this home claims no cards; omit the key to allow any existing profile")
		return map[string]bool{}
	}
	var names []string
	if list, isList := raw.([]any); isList {
		for _, n := range list {
			names = append(names, pyrepr.Str(n))
		}
	} else {
		names = strings.Split(pyrepr.Str(raw), ",")
	}
	allowed := map[string]bool{}
	for _, n := range names {
		if canon, err := profiles.NormalizeProfileName(n); err == nil {
			allowed[canon] = true
		}
	}
	return allowed
}

// Human readable kanban.dispatch_profiles resolution of this home for diagnostics
func DispatchProfileAllowlistSummary(ctx context.Context) string {
	allowlist := dispatchProfileAllowlist(ctx)
	if allowlist == nil {
		return "any"
	}
	if len(allowlist) > 0 {
		names := make([]string, 0, len(allowlist))
		for n := range allowlist {
			names = append(names, n)
		}
		sort.Strings(names)
		return strings.Join(names, ", ")
	}
	return "none (fail-closed: kanban.dispatch_profiles is present but names no valid " +
		"profile, or the config could not be read — omit the key to allow any)"
}

// Whether an unclaimed assigned task of the status maps to a profile this home may spawn
func hasSpawnable(ctx context.Context, conn state.SQLiteConn, status string) (bool, error) {
	rows, err := QueryRows(ctx, conn, "SELECT DISTINCT assignee FROM tasks WHERE status = ? AND assignee IS NOT NULL AND claim_lock IS NULL", status)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	exists := dispatchProfileExistsFn(ctx)
	for _, row := range rows {
		if exists(pyrepr.Str(row.Get("assignee"))) {
			return true, nil
		}
	}
	return false, nil
}

// True when a ready assigned unclaimed task maps to a real profile
func HasSpawnableReady(ctx context.Context, conn state.SQLiteConn) (bool, error) {
	return hasSpawnable(ctx, conn, "ready")
}

// True when a review assigned unclaimed task maps to a real profile
func HasSpawnableReview(ctx context.Context, conn state.SQLiteConn) (bool, error) {
	return hasSpawnable(ctx, conn, "review")
}

// Loaded config of the current home, the shared cached tree when readonly
func loadKanbanConfigTree(ctx context.Context, readonly bool) (config.Tree, error) {
	if readonly {
		return config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	}
	return config.LoadContext(ctx, home.Dir(ctx), os.LookupEnv)
}

// Kanban section of the loaded config, false when the config or the section is not a mapping
func kanbanSection(ctx context.Context, readonly bool) (map[string]any, bool) {
	cfg, err := loadKanbanConfigTree(ctx, readonly)
	if err != nil {
		return nil, false
	}
	section, present := cfg["kanban"]
	if !present {
		return map[string]any{}, true
	}
	m, ok := section.(map[string]any)
	return m, ok
}

// Whether review tasks dispatch automatically, true unless kanban.review_dispatch is falsy
func ReviewDispatchEnabled(ctx context.Context) bool {
	section, ok := kanbanSection(ctx, false)
	if !ok {
		return true
	}
	v, present := section["review_dispatch"]
	if !present {
		return true
	}
	return pyrepr.Truthy(v)
}

// System memory snapshot in KiB values, empty when unknown
func systemMemorySample() *pyjson.Object {
	if s := gatewayDispatch().SampleMemory(); s != nil {
		return s
	}
	return pyjson.NewObject()
}

// Memory derived default for kanban.max_in_progress, nil when total memory is unknown
func DeriveDefaultMaxInProgress(sample *pyjson.Object) *int {
	if sample == nil {
		sample = systemMemorySample()
	}
	raw, _ := sample.Get("mem_total_kib")
	if _, isBool := raw.(bool); isBool {
		return nil
	}
	total, ok := pyint.Instance(raw)
	if !ok || total.Sign() <= 0 {
		return nil
	}
	workers := total.Int64() / 1024 / MemoryGuardMBPerWorker
	n := int(max(DerivedMaxInProgressFloor, min(workers, DerivedMaxInProgressCeiling)))
	return &n
}

// Effective global concurrency cap, the configured value else the memory derived default
func ResolveMaxInProgress(configured *int) *int {
	if configured != nil {
		return configured
	}
	return DeriveDefaultMaxInProgress(nil)
}

// Positive kanban.max_in_progress from config, nil when unset or invalid
func ConfiguredMaxInProgress(ctx context.Context) *int {
	section, ok := kanbanSection(ctx, true)
	if !ok {
		return nil
	}
	raw := section["max_in_progress"]
	if raw == nil {
		return nil
	}
	n, err := pyint.Of(raw)
	if err != nil || !n.IsInt64() || n.Int64() < 1 {
		return nil
	}
	v := int(n.Int64())
	return &v
}

// Number of running tasks, zero on any error
func CountRunningTasks(ctx context.Context, conn state.SQLiteConn) int {
	row, err := QueryOne(ctx, conn, "SELECT COUNT(*) AS n FROM tasks WHERE status = 'running'")
	if err != nil || row == nil {
		return 0
	}
	n, err := pyint.Of(row.Get("n"))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

// Running tasks across every board except the given one, matched by resolved database path
func CountRunningTasksOtherBoards(ctx context.Context, board *string) int {
	var currentPath *string
	if p, err := KanbanDBPath(ctx, board); err == nil {
		if resolved, err := pypath.Resolve(pypath.Expanduser(p)); err == nil {
			currentPath = &resolved
		}
	}
	boards, err := ListBoards(false)
	if err != nil {
		return 0
	}
	total := 0
	for _, meta := range boards {
		slug := DefaultBoard
		if v, _ := meta.Get("slug"); pyrepr.Truthy(v) {
			slug = pyrepr.Str(v)
		}
		path, err := KanbanDBPath(ctx, BoardArg(slug))
		if err != nil {
			continue
		}
		path = pypath.Expanduser(path)
		resolved, err := pypath.Resolve(path)
		if err != nil {
			continue
		}
		if currentPath != nil && resolved == *currentPath {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		other, err := Connect(ctx, "", BoardArg(slug))
		if err != nil {
			continue
		}
		total += CountRunningTasks(ctx, other)
		_ = other.Close()
	}
	return total
}

// Memory pressure level ok, elevated, critical or unknown, unknown imposing no restriction
func memoryPressureLevel(sample *pyjson.Object) (level string) {
	if sample == nil {
		sample = systemMemorySample()
	}
	if sample.Len() == 0 {
		return "unknown"
	}
	defer func() {
		if recover() != nil {
			level = "unknown"
		}
	}()
	avail, _ := sample.Get("mem_available_kib")
	total, _ := sample.Get("mem_total_kib")
	return gatewayDispatch().ClassifyPressure(avail, total)
}

// SpawnFunc starts a worker for a claimed task in its workspace and returns its pid, nil when none is known
type SpawnFunc func(ctx context.Context, task *Task, workspace string, board *string) (*int64, error)

// DispatchOptions are the keyword arguments of one dispatcher tick
type DispatchOptions struct {
	SpawnFn                 SpawnFunc
	TTLSeconds              *int64
	DryRun                  bool
	MaxSpawn                *int
	MaxInProgress           *int
	FailureLimit            int64
	StaleTimeoutSeconds     int64
	Board                   *string
	DefaultAssignee         *string
	MaxInProgressPerProfile *int
	ReconcileOrphans        bool
}

// Dispatch options at the dispatch_once defaults
func DefaultDispatchOptions() DispatchOptions {
	return DispatchOptions{FailureLimit: DefaultFailureLimit, ReconcileOrphans: true}
}

// One dispatcher tick under the board single writer lock, the loser returning a skipped locked result
func DispatchOnce(ctx context.Context, conn state.SQLiteConn, o DispatchOptions) (*DispatchResult, error) {
	dbPath, err := KanbanDBPath(ctx, o.Board)
	if err != nil {
		result, err := dispatchOnceLocked(ctx, conn, o)
		if err != nil {
			return nil, err
		}
		fireDispatchTickHook(ctx, result, o.Board, o.DryRun)
		return result, nil
	}
	held, release := dispatchTickLock(dbPath)
	var result *DispatchResult
	if !held {
		result = &DispatchResult{SkippedLocked: true}
	} else {
		result, err = dispatchOnceLocked(ctx, conn, o)
		if err != nil {
			release()
			return nil, err
		}
		maybeCheckpointWAL(ctx, conn, dbPath)
	}
	release()
	fireDispatchTickHook(ctx, result, o.Board, o.DryRun)
	return result, nil
}

// Calls the spawn function with the board the task was claimed from
func callSpawnFn(ctx context.Context, spawnFn SpawnFunc, task *Task, workspace string, board *string) (*int64, error) {
	return spawnFn(ctx, task, workspace, board)
}

// laneParams are the per tick settings every lane row dispatch shares
type laneParams struct {
	dryRun            bool
	ttlSeconds        *int64
	board             *string
	failureLimit      int64
	spawnFn           SpawnFunc
	perProfileCap     *int
	perProfileRunning map[string]int
}

// Guards, claims, resolves the workspace and spawns one lane row, true when a spawn slot was consumed
func dispatchLaneTask(ctx context.Context, conn state.SQLiteConn, row *Row, assignee string, result *DispatchResult, lane string, p *laneParams) (bool, error) {
	taskID := pyrepr.Str(row.Get("id"))
	if !dispatchProfileExistsFn(ctx)(assignee) {
		result.SkippedNonspawnable = append(result.SkippedNonspawnable, taskID)
		if !p.dryRun {
			err := WriteTxn(ctx, conn, false, func() error {
				last, err := QueryOne(ctx, conn, "SELECT kind, payload FROM task_events WHERE task_id = ? "+
					"ORDER BY created_at DESC, id DESC LIMIT 1", taskID)
				if err != nil {
					return err
				}
				want, err := jsonOrNull(payload("assignee", assignee))
				if err != nil {
					return err
				}
				if last == nil || last.Get("kind") != "skipped_nonspawnable" || !pyjson.Equal(last.Get("payload"), want) {
					return appendEvent(ctx, conn, taskID, "skipped_nonspawnable", payload("assignee", assignee), nil)
				}
				return nil
			})
			if err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if p.perProfileCap != nil {
		if current := p.perProfileRunning[assignee]; current >= *p.perProfileCap {
			result.SkippedPerProfileCapped = append(result.SkippedPerProfileCapped, CappedTask{TaskID: taskID, Assignee: assignee, Running: current})
			return false, nil
		}
	}
	guard, err := CheckRespawnGuard(ctx, conn, taskID, lane)
	if err != nil {
		return false, err
	}
	if guard != nil {
		result.RespawnGuarded = append(result.RespawnGuarded, GuardedTask{TaskID: taskID, Reason: *guard})
		if !p.dryRun {
			if err := WriteTxn(ctx, conn, false, func() error {
				return appendEvent(ctx, conn, taskID, "respawn_guarded", payload("reason", *guard), nil)
			}); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	countSpawn := func(name string) {
		if p.perProfileCap != nil && name != "" {
			p.perProfileRunning[name]++
		}
	}
	if p.dryRun {
		result.Spawned = append(result.Spawned, SpawnedTask{TaskID: taskID, Assignee: assignee})
		countSpawn(assignee)
		return true, nil
	}
	claim := ClaimTask
	if lane == "review" {
		claim = ClaimReviewTask
	}
	claimed, err := claim(ctx, conn, taskID, p.ttlSeconds, nil)
	if err != nil || claimed == nil {
		return false, err
	}
	var workspace, resolvedBranch string
	if claimed.WorkspaceKind == "worktree" {
		workspace, resolvedBranch, err = resolveWorktreeWorkspace(ctx, claimed, p.board)
	} else {
		workspace, err = ResolveWorkspace(ctx, claimed, p.board)
	}
	if err != nil {
		blocked, ferr := recordTaskFailure(ctx, conn, claimed.ID, "workspace: "+pyrepr.ExceptionMessage(err), RecordFailureOptions{
			Outcome: "spawn_failed", FailureLimit: &p.failureLimit, ReleaseClaim: true, EndRun: true,
		})
		if ferr != nil {
			return false, ferr
		}
		if blocked {
			result.AutoBlocked = append(result.AutoBlocked, claimed.ID)
		}
		return false, nil
	}
	if err := SetWorkspacePath(ctx, conn, claimed.ID, workspace); err != nil {
		return false, err
	}
	if claimed.WorkspaceKind == "worktree" {
		branch := resolvedBranch
		if branch == "" && claimed.BranchName != nil {
			branch = strings.TrimSpace(*claimed.BranchName)
		}
		if branch == "" {
			branch = "wt/" + claimed.ID
		}
		if err := SetBranchName(ctx, conn, claimed.ID, branch); err != nil {
			return false, err
		}
	}
	maybeEmitScratchTip(ctx, conn, claimed.ID, &claimed.WorkspaceKind)
	if lane == "review" {
		skills := []string{}
		for _, s := range append(append([]string{}, claimed.Skills...), "sdlc-review") {
			if !slices.Contains(skills, s) {
				skills = append(skills, s)
			}
		}
		claimed.Skills, claimed.SkillsSet = skills, true
	}
	spawnFn := p.spawnFn
	if spawnFn == nil {
		spawnFn = defaultSpawn
	}
	spawnErr := func() error {
		pid, err := callSpawnFn(ctx, spawnFn, claimed, workspace, p.board)
		if err != nil {
			return err
		}
		if pid != nil && *pid != 0 {
			if err := setWorkerPid(ctx, conn, claimed.ID, int(*pid), claimed.CurrentRunID, claimed.ClaimLock); err != nil {
				return err
			}
		}
		boardName := ""
		if p.board != nil {
			boardName = *p.board
		}
		fireWorkerSpawnedHook(ctx, conn, claimed, workspace, pid, boardName)
		assigned := ""
		if claimed.Assignee != nil {
			assigned = *claimed.Assignee
		}
		result.Spawned = append(result.Spawned, SpawnedTask{TaskID: claimed.ID, Assignee: assigned, WorkspacePath: workspace})
		countSpawn(assigned)
		return nil
	}()
	if spawnErr == nil {
		return true, nil
	}
	var scope *tools.RestartSafeScopeUnavailable
	infrastructure := errors.As(spawnErr, &scope)
	if infrastructure {
		log.Warn(fmt.Sprintf("kanban dispatcher: spawn of %s deferred, host cannot place the worker: %s", claimed.ID, pyrepr.ExceptionMessage(spawnErr)))
	}
	blocked, err := recordTaskFailure(ctx, conn, claimed.ID, pyrepr.ExceptionMessage(spawnErr), RecordFailureOptions{
		Outcome: "spawn_failed", FailureLimit: &p.failureLimit, ReleaseClaim: true, EndRun: true, Infrastructure: infrastructure,
	})
	if err != nil {
		return false, err
	}
	if blocked {
		result.AutoBlocked = append(result.AutoBlocked, claimed.ID)
	}
	return false, nil
}

// Persists kanban.default_assignee on an unassigned ready row, false when the write failed
func applyDefaultAssignee(ctx context.Context, conn state.SQLiteConn, taskID, assignee string, dryRun bool) bool {
	if dryRun {
		return true
	}
	err := WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET assignee = ? WHERE id = ? AND (assignee IS NULL OR assignee = '')", assignee, taskID); err != nil {
			return err
		}
		return appendEvent(ctx, conn, taskID, "assigned", payload("assignee", assignee, "source", "kanban.default_assignee"), nil)
	})
	if err != nil {
		log.Debug(fmt.Sprintf("kanban dispatch: failed to apply default_assignee=%s to task %s", pyrepr.Repr(assignee), taskID), "exc", err)
		return false
	}
	return true
}

// Reclaims stale, orphaned, crashed and timed out running tasks, then promotes
func runReclaimPhase(ctx context.Context, conn state.SQLiteConn, result *DispatchResult, staleTimeoutSeconds, failureLimit int64, reconcileOrphans bool, board *string) error {
	reapWorkerZombies()
	var err error
	if result.ReapedTerminalWorkers, err = ReapTerminalWorkers(ctx, conn, nil); err != nil {
		return err
	}
	if result.Reclaimed, err = ReleaseStaleClaims(ctx, conn, nil, &failureLimit); err != nil {
		return err
	}
	if reconcileOrphans {
		if result.ReconciledOrphans, err = ReconcileOrphanedRunning(ctx, conn); err != nil {
			return err
		}
	}
	if result.Stale, err = DetectStaleRunning(ctx, conn, staleTimeoutSeconds, nil); err != nil {
		return err
	}
	crash, err := detectCrashedWorkersFull(ctx, conn, board)
	if err != nil {
		return err
	}
	result.Crashed = crash.Crashed
	result.AutoBlocked = append(result.AutoBlocked, crash.AutoBlocked...)
	result.RateLimited = append(result.RateLimited, crash.RateLimited...)
	if result.TimedOut, err = EnforceMaxRuntime(ctx, conn, nil); err != nil {
		return err
	}
	result.Promoted, err = RecomputeReady(ctx, conn, &failureLimit)
	return err
}

// Whether this tick may spawn and its budget, nil budget meaning uncapped
func tickSpawnBudget(ctx context.Context, conn state.SQLiteConn, result *DispatchResult, maxSpawn, maxInProgress *int, board *string) (bool, *int) {
	runningCount := 0
	var budget *int
	if maxSpawn != nil || maxInProgress != nil {
		runningCount = CountRunningTasks(ctx, conn)
	}
	if maxSpawn != nil {
		if runningCount >= *maxSpawn {
			return false, nil
		}
		b := *maxSpawn - runningCount
		budget = &b
	}
	if maxInProgress != nil {
		totalRunning := runningCount + CountRunningTasksOtherBoards(ctx, board)
		if totalRunning >= *maxInProgress {
			return false, nil
		}
		remaining := *maxInProgress - totalRunning
		if budget == nil || *budget > remaining {
			budget = &remaining
		}
	}
	pressure := memoryPressureLevel(nil)
	if pressure == "critical" {
		result.MemoryPressure = &pressure
		log.Warn("kanban dispatch: system memory pressure is critical; spawning no new workers this tick (deferred, not dropped)")
		return false, nil
	}
	if pressure == "elevated" {
		result.MemoryPressure = &pressure
		if budget == nil || *budget > 1 {
			log.Warn("kanban dispatch: system memory pressure is elevated; limiting to at most 1 new worker this tick")
			one := 1
			budget = &one
		}
	}
	return true, budget
}

// Unclaimed rows of one lane in dispatch order
func laneRows(ctx context.Context, conn state.SQLiteConn, status string) ([]*Row, error) {
	return QueryRows(ctx, conn, "SELECT id, assignee FROM tasks WHERE status = '"+status+"' AND claim_lock IS NULL ORDER BY priority DESC, created_at ASC")
}

// Whether a review row would pass the review lane gates this tick and so may hold a reserved slot
func anySpawnableReview(ctx context.Context, conn state.SQLiteConn, reviewRows []*Row, perProfileCap *int, perProfileRunning map[string]int) (bool, error) {
	if len(reviewRows) == 0 {
		return false, nil
	}
	exists := dispatchProfileExistsFn(ctx)
	for _, row := range reviewRows {
		assignee, _ := row.Get("assignee").(string)
		if assignee == "" || !exists(assignee) {
			continue
		}
		if perProfileCap != nil && perProfileRunning[assignee] >= *perProfileCap {
			continue
		}
		guard, err := CheckRespawnGuard(ctx, conn, pyrepr.Str(row.Get("id")), "review")
		if err != nil {
			return false, err
		}
		if guard == nil {
			return true, nil
		}
	}
	return false, nil
}

// Configured default assignee when it names a profile this home may claim, nil otherwise
func resolveDefaultAssignee(ctx context.Context, defaultAssignee *string) *string {
	name := ""
	if defaultAssignee != nil {
		name = strings.TrimSpace(*defaultAssignee)
	}
	if name == "" {
		return nil
	}
	if !dispatchProfileExistsFn(ctx)(name) {
		return nil
	}
	return &name
}

// One dispatcher tick reclaiming, promoting and spawning every eligible ready and review row within budget
func dispatchOnceLocked(ctx context.Context, conn state.SQLiteConn, o DispatchOptions) (*DispatchResult, error) {
	result := &DispatchResult{}
	if err := runReclaimPhase(ctx, conn, result, o.StaleTimeoutSeconds, o.FailureLimit, o.ReconcileOrphans, o.Board); err != nil {
		return nil, err
	}
	maySpawn, spawnBudget := tickSpawnBudget(ctx, conn, result, o.MaxSpawn, o.MaxInProgress, o.Board)
	if !maySpawn {
		return result, nil
	}
	readyRows, err := laneRows(ctx, conn, "ready")
	if err != nil {
		return nil, err
	}
	var reviewRows []*Row
	if ReviewDispatchEnabled(ctx) {
		if reviewRows, err = laneRows(ctx, conn, "review"); err != nil {
			return nil, err
		}
	}
	var perProfileCap *int
	if o.MaxInProgressPerProfile != nil && *o.MaxInProgressPerProfile > 0 {
		perProfileCap = o.MaxInProgressPerProfile
	}
	perProfileRunning := map[string]int{}
	if perProfileCap != nil {
		rows, err := QueryRows(ctx, conn, "SELECT assignee, COUNT(*) AS n FROM tasks WHERE status = 'running' AND assignee IS NOT NULL GROUP BY assignee")
		if err != nil {
			return nil, err
		}
		for _, prow := range rows {
			n, _ := pyint.Of(prow.Get("n"))
			perProfileRunning[pyrepr.Str(prow.Get("assignee"))] = int(n.Int64())
		}
	}
	readyBudget := spawnBudget
	if spawnBudget != nil && *spawnBudget > 0 {
		reserve, err := anySpawnableReview(ctx, conn, reviewRows, perProfileCap, perProfileRunning)
		if err != nil {
			return nil, err
		}
		if reserve {
			b := max(*spawnBudget-1, 0)
			readyBudget = &b
		}
	}
	params := &laneParams{
		dryRun: o.DryRun, ttlSeconds: o.TTLSeconds, board: o.Board, failureLimit: o.FailureLimit, spawnFn: o.SpawnFn,
		perProfileCap: perProfileCap, perProfileRunning: perProfileRunning,
	}
	defaultAssignee := resolveDefaultAssignee(ctx, o.DefaultAssignee)
	spawned := 0
	for _, row := range readyRows {
		if readyBudget != nil && spawned >= *readyBudget {
			break
		}
		rowAssignee, _ := row.Get("assignee").(string)
		if rowAssignee == "" {
			id := pyrepr.Str(row.Get("id"))
			if defaultAssignee == nil || !applyDefaultAssignee(ctx, conn, id, *defaultAssignee, o.DryRun) {
				result.SkippedUnassigned = append(result.SkippedUnassigned, id)
				continue
			}
			rowAssignee = *defaultAssignee
			result.AutoAssignedDefault = append(result.AutoAssignedDefault, id)
		}
		ok, err := dispatchLaneTask(ctx, conn, row, rowAssignee, result, "ready", params)
		if err != nil {
			return nil, err
		}
		if ok {
			spawned++
		}
	}
	for _, row := range reviewRows {
		if spawnBudget != nil && spawned >= *spawnBudget {
			break
		}
		assignee, _ := row.Get("assignee").(string)
		if assignee == "" {
			result.SkippedUnassigned = append(result.SkippedUnassigned, pyrepr.Str(row.Get("id")))
			continue
		}
		ok, err := dispatchLaneTask(ctx, conn, row, assignee, result, "review", params)
		if err != nil {
			return nil, err
		}
		if ok {
			spawned++
		}
	}
	return result, nil
}

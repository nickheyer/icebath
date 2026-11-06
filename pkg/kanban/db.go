package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var log = slog.Default().With("logger", "hermes_cli.kanban_db")

// Python ValueError raised by board and task validation
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Python PermissionError raised when a fenced context touches durable kanban state
type PermissionError struct{ Msg string }

func (e *PermissionError) Error() string { return e.Msg }

// Every value tasks.status can hold
var ValidStatuses = DefaultStatuses

// Statuses a task may be created in besides the computed default
var ValidInitialStatuses = map[string]bool{"running": true, "blocked": true}

// Reasons a task can be blocked
var ValidBlockKinds = map[string]bool{"dependency": true, "needs_input": true, "capability": true, "transient": true}

// Unblock loop count after which a recurring block escalates
const BlockRecurrenceLimit = 2

// Workspace kinds a task may carry
var ValidWorkspaceKinds = map[string]bool{"scratch": true, "worktree": true, "dir": true}

// Attachment size cap shared by the dashboard, tools and CLI
const KanbanAttachmentMaxBytes = 25 * 1024 * 1024

const isWindows = runtime.GOOS == "windows"

// Bytes decoded as UTF-8 with U+FFFD for undecodable sequences, anything else passed through
func LossyText(value any) any {
	switch v := value.(type) {
	case []byte:
		return pystr.DecodeUTF8Replace(v)
	case string:
		if !utf8.ValidString(v) {
			return pystr.DecodeUTF8Replace([]byte(v))
		}
	}
	return value
}

// One result row with its column names, the sqlite3.Row a board connection yields
type Row struct {
	Cols []string
	Vals []any
}

// Value of a column and whether the row carries it
func (r *Row) Lookup(col string) (any, bool) {
	if r == nil {
		return nil, false
	}
	for i, c := range r.Cols {
		if c == col {
			return r.Vals[i], true
		}
	}
	return nil, false
}

// Value of a column, nil when absent
func (r *Row) Get(col string) any {
	v, _ := r.Lookup(col)
	return v
}

// Rows of a query with TEXT cells decoded the way the lossy text factory does
func QueryRows(ctx context.Context, conn state.SQLiteConn, query string, args ...any) ([]*Row, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []*Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if s, ok := v.(string); ok {
				vals[i] = LossyText(s)
			}
		}
		out = append(out, &Row{Cols: cols, Vals: vals})
	}
	return out, rows.Err()
}

// First row of a query, nil when it returns none
func QueryOne(ctx context.Context, conn state.SQLiteConn, query string, args ...any) (*Row, error) {
	rows, err := QueryRows(ctx, conn, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Column value tolerant of the column being absent from the row
func rowGet(row *Row, col string, def any) any {
	if v, ok := row.Lookup(col); ok {
		return v
	}
	return def
}

// Decoded JSON text column, def for an empty value or any decode failure
func jsonOr(value any, def any) any {
	if !pyrepr.Truthy(value) {
		return def
	}
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return def
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return def
	}
	return parsed
}

// Decoded JSON object column, an empty object for anything else
func jsonDict(value any) *pyjson.Object {
	if o, ok := jsonOr(value, nil).(*pyjson.Object); ok {
		return o
	}
	return pyjson.NewObject()
}

// Integer env override where absent, empty, non integer or below minimum yields def
func envInt(name string, def, minimum int64) int64 {
	raw := pystr.Strip(os.Getenv(pyEnvName(name)))
	if raw == "" {
		return def
	}
	parsed, ok := pyint.Parse(raw)
	if !ok {
		return def
	}
	n := int64(math.MaxInt64)
	if parsed.IsInt64() {
		n = parsed.Int64()
	} else if parsed.Sign() < 0 {
		return def
	}
	if n >= minimum {
		return n
	}
	return def
}

// Env name with the hermes prefix renamed to the icebath one
func pyEnvName(name string) string {
	if rest, ok := strings.CutPrefix(name, "HERMES_"); ok {
		return "ICEBATH_" + rest
	}
	return name
}

// Stripped stdout of git -C cwd args, nil on any failure or empty output
func gitOut(cwd string, timeout time.Duration, args ...string) *string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil
	}
	out := pystr.Strip(pystr.DecodeUTF8Replace([]byte(stdout.String())))
	if out == "" {
		return nil
	}
	return &out
}

// Valid reasoning effort or none case insensitively, nil for empty, a ValueError for anything else
func NormalizeReasoningEffort(effort *string) (*string, error) {
	raw := ""
	if effort != nil {
		raw = *effort
	}
	value := pystr.Lower(pystr.Strip(raw))
	if value == "" {
		return nil, nil
	}
	if value == "none" || slices.Contains(config.ValidReasoningEfforts, value) {
		return &value, nil
	}
	allowed := strings.Join(append([]string{"none"}, config.ValidReasoningEfforts...), ", ")
	shown := "None"
	if effort != nil {
		shown = pyrepr.StrRepr(*effort)
	}
	return nil, &ValueError{fmt.Sprintf("reasoning_effort must be one of %s, got %s", allowed, shown)}
}

var knownToolsetNames = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	for _, name := range tools.Default.ToolsetNames(context.Background(), "") {
		out[pystr.Casefold(name)] = true
	}
	return out
})

// PermissionError when a delegate child context would mutate the board at path, the lineage board when empty
func assertNotDelegatedChildMutation(ctx context.Context, path string) error {
	if path == "" {
		path = KanbanHome()
	}
	if tools.KanbanPathIsFenced(ctx, path) {
		return &PermissionError{"delegate_task child contexts cannot mutate Kanban tasks or boards"}
	}
	return nil
}

// Fires a lifecycle hook after the write commits, swallowing observer failures
func fireKanbanLifecycleHook(ctx context.Context, event, taskID string, fields map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			log.Debug(fmt.Sprintf("kanban lifecycle hook %s failed: %v", event, r))
		}
	}()
	kwargs := map[string]any{"task_id": taskID, "profile_name": hookProfileName(ctx)}
	for k, v := range fields {
		kwargs[k] = v
	}
	plugins.InvokeHook(ctx, event, kwargs)
}

// Lifecycle hook for a task transition with the assignee of the possibly missing row
func fireTaskHook(ctx context.Context, event string, task *Task, taskID string, runID *int64, fields map[string]any) {
	all := map[string]any{"board": GetCurrentBoard(ctx), "assignee": nil, "run_id": optIntValue(runID)}
	if task != nil {
		all["assignee"] = optStrValue(task.Assignee)
	}
	for k, v := range fields {
		all[k] = v
	}
	fireKanbanLifecycleHook(ctx, event, taskID, all)
}

// Active profile for hook payloads, default when it cannot be resolved
func hookProfileName(ctx context.Context) (name string) {
	defer func() {
		if recover() != nil {
			name = "default"
		}
	}()
	return profiles.GetActiveProfileName(ctx)
}

// Reports whether any observer subscribes to the event, inspection failure counting as unconsumed
func kanbanObserverConsumed(ctx context.Context, event string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return plugins.HasHook(ctx, event)
}

// Fires the worker spawned hook after the pid is durably persisted, swallowing failures
func fireWorkerSpawnedHook(ctx context.Context, conn state.SQLiteConn, task *Task, workspacePath string, pid *int64, board string) {
	if !kanbanObserverConsumed(ctx, "on_kanban_worker_spawned") {
		return
	}
	if board == "" {
		board = GetCurrentBoard(ctx)
	}
	runID, err := currentRunID(ctx, conn, task.ID)
	if err != nil {
		log.Debug(fmt.Sprintf("kanban worker spawned hook failed: %v", err))
		return
	}
	var workerPID any
	if pid != nil && *pid != 0 {
		workerPID = *pid
	}
	fireKanbanLifecycleHook(ctx, "on_kanban_worker_spawned", task.ID, map[string]any{
		"board": board, "assignee": optStrValue(task.Assignee), "run_id": optIntValue(runID),
		"worker_pid": workerPID, "workspace_path": workspacePath,
	})
}

// Fires on_kanban_task_updated after a non lifecycle mutation commits, naming the changed fields only
func NotifyTaskUpdated(ctx context.Context, conn state.SQLiteConn, taskID string, changedFields []string, board string) {
	if !kanbanObserverConsumed(ctx, "on_kanban_task_updated") {
		return
	}
	row, err := QueryOne(ctx, conn, "SELECT assignee, current_run_id FROM tasks WHERE id = ?", taskID)
	if err != nil {
		log.Debug(fmt.Sprintf("kanban task updated hook failed: %v", err))
		return
	}
	if board == "" {
		board = GetCurrentBoard(ctx)
	}
	fields := make([]any, len(changedFields))
	for i, f := range changedFields {
		fields[i] = f
	}
	fireKanbanLifecycleHook(ctx, "on_kanban_task_updated", taskID, map[string]any{
		"board": board, "assignee": row.Get("assignee"), "run_id": row.Get("current_run_id"), "changed_fields": fields,
	})
}

// Dispatch result fields whose truthiness makes a tick count as active
var tickActivityFields = []string{
	"spawned", "reclaimed", "promoted", "reconciled_orphans", "reaped_terminal_workers", "crashed", "stale",
	"timed_out", "auto_blocked", "rate_limited", "auto_assigned_default",
	"respawn_guarded", "skipped_per_profile_capped", "skipped_unassigned",
	"skipped_nonspawnable",
}

// Fires on_kanban_dispatch_tick after the tick lock is released with an ok, idle or skipped_locked outcome
func fireDispatchTickHook(ctx context.Context, result *DispatchResult, board *string, dryRun bool) {
	if !kanbanObserverConsumed(ctx, "on_kanban_dispatch_tick") {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Debug(fmt.Sprintf("kanban dispatch tick hook failed: %v", r))
		}
	}()
	profileName := hookProfileName(ctx)
	var boardValue any
	if board != nil {
		boardValue = *board
	} else {
		boardValue = GetCurrentBoard(ctx)
	}
	outcome := "ok"
	if result.SkippedLocked {
		outcome = "skipped_locked"
	} else if !slices.ContainsFunc(tickActivityFields, result.Activity) {
		outcome = "idle"
	}
	plugins.InvokeHook(ctx, "on_kanban_dispatch_tick", map[string]any{
		"board": boardValue, "profile_name": profileName, "dry_run": dryRun, "outcome": outcome, "result": result,
	})
}

const (
	DefaultClaimTTLSeconds               = 15 * 60
	DefaultClaimHeartbeatMaxStaleSeconds = 60 * 60
	ReclaimDeferGraceSeconds             = 120
	DefaultCrashGraceSeconds             = 30
	KanbanRateLimitExitCode              = 75
	KanbanTerminalProviderExitCode       = 78
)

// Explicit ttl, then ICEBATH_KANBAN_CLAIM_TTL_SECONDS, then the default
func resolveClaimTTLSeconds(ttlSeconds *int64) int64 {
	if ttlSeconds != nil {
		return max(1, *ttlSeconds)
	}
	return envInt("HERMES_KANBAN_CLAIM_TTL_SECONDS", DefaultClaimTTLSeconds, 1)
}

// ICEBATH_KANBAN_CRASH_GRACE_SECONDS where 0 means immediate, else the default
func resolveCrashGraceSeconds() int64 {
	return envInt("HERMES_KANBAN_CRASH_GRACE_SECONDS", DefaultCrashGraceSeconds, 0)
}

// ICEBATH_KANBAN_RATE_LIMIT_COOLDOWN_SECONDS where 0 means next tick, else the default
func resolveRateLimitCooldownSeconds() int64 {
	return envInt("HERMES_KANBAN_RATE_LIMIT_COOLDOWN_SECONDS", DefaultRateLimitCooldownSeconds, 0)
}

// Hook payload value of an optional string
func optStrValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Hook payload value of an optional integer
func optIntValue(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Ledger owner liveness probes from gateway status, installed by pkg/gateway at init
var AsyncDelegationOwnerProbes struct {
	PidExists        func(pid int) bool
	ProcessStartTime func(pid int) (int64, bool)
}

// RetirementWork is the backend retirement fence async dispatch reserves its work under
type RetirementWork interface {
	Work() (bool, func())
	Acquire() bool
	Release()
}

// Backend retirement fence of hermes_cli backend_retirement, installed by pkg/provider at init
var AsyncDelegationRetirement RetirementWork

const (
	asyncDefaultMaxChildren        = 3
	asyncMaxRetainedCompleted      = 50
	asyncDurableRetentionSeconds   = 7 * 24 * 60 * 60
	asyncMaxDurablePending         = 1000
	asyncMaxDeliveryAttempts       = 8
	asyncMaxCompletionReplayAgeS   = 48 * 3600.0
	asyncClaimLeaseS               = 300.0
	OrphanSweepIntervalS           = 30.0
	asyncOrphanStaleS              = 60.0
	asyncFailureSurfaceWindowS     = 24 * 3600.0
	asyncDelegationLedgerDBLabel   = "state.db (async_delegation)"
	asyncDelegationBusyTimeoutMS   = 10_000
	asyncDelegationExecutorPrefix  = "async-delegate"
	asyncDelegationRejectedClosing = "This session was closed (exit or /new): no new background work."
)

// Monitor cadence and stall grace, swapped by tests
var (
	asyncStaleCheckInterval = 30 * time.Second
	asyncStallGraceSeconds  = 120.0
	asyncStaleIdleSeconds   = 450.0
	asyncStaleInToolSeconds = 1200.0
	asyncEnsureStaleMonitor = ensureStaleMonitor
	asyncTimeNow            = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	asyncMonotonicStart     = time.Now()
	asyncMonotonic          = func() float64 { return time.Since(asyncMonotonicStart).Seconds() }
)

var (
	asyncLiveStates     = setOf("running", "stalling", "finalizing")
	asyncActiveStates   = setOf("running", "stalling")
	asyncRoutingKeys    = []string{"scope_id", "user_id", "user_name"}
	asyncStallMetaKeys  = []string{"stalled_after_quiet_seconds", "stall_threshold_seconds", "stall_phase", "stall_grace_seconds"}
	asyncStallFieldMap  = [][2]string{{"_stall_quiet_seconds", "stalled_after_quiet_seconds"}, {"_stall_threshold_seconds", "stall_threshold_seconds"}, {"_stall_in_tool", "stall_in_tool"}}
	asyncFencedFields   = []string{"session_key", "parent_session_id"}
	asyncFailedStates   = setOf("error", "failed", "failure", "timeout", "stalled", "unknown", "interrupted")
	asyncDBMu           sync.Mutex
	asyncExecutorMu     sync.Mutex
	asyncExecutor       *DaemonPool
	asyncExecutorMax    int
	asyncRecordsMu      sync.Mutex
	asyncRecords        = map[string]*asyncRecord{}
	asyncRecordOrder    []string
	asyncRefused        = map[any]map[asyncSelector]bool{}
	asyncRefusedBy      = map[asyncSelector]map[any]bool{}
	asyncOrphanMu       sync.Mutex
	asyncOffered        = map[[2]string]bool{}
	asyncStartupReplay  = map[string]bool{}
	asyncLastOrphanTick = map[string]float64{}
	asyncReofferedAt    = map[[2]string]float64{}
	asyncHeldMu         sync.Mutex
	asyncHeldCopies     = map[string]int{}
	asyncMonitorMu      sync.Mutex
	asyncMonitorStop    chan struct{}
	asyncMonitorDone    chan struct{}
)

type asyncSelector struct{ field, value string }

// One live or recently finished delegation unit, public fields in m and the dispatcher's context beside it
type asyncRecord struct {
	m           map[string]any
	interruptFn func() error
	progressFn  func() (any, bool, error)
	ctx         context.Context
	env         *Env
}

func asyncLog() interface {
	Info(string, ...any)
	Warn(string, ...any)
	Error(string, ...any)
	Debug(string, ...any)
} {
	return toolsLogger(nil).With("logger", "tools.async_delegation")
}

func asyncProbes() (func(int) bool, func(int) (int64, bool)) {
	p := AsyncDelegationOwnerProbes
	if p.PidExists == nil || p.ProcessStartTime == nil {
		panic("tools.AsyncDelegationOwnerProbes is not installed: pkg/gateway assigns it at init")
	}
	return p.PidExists, p.ProcessStartTime
}

func asyncRetirement() RetirementWork {
	if AsyncDelegationRetirement == nil {
		panic("tools.AsyncDelegationRetirement is not installed: pkg/provider assigns it at init")
	}
	return AsyncDelegationRetirement
}

// Durable ledger path, state.db of the bound profile home
func asyncDBPath(env *Env) string { return filepath.Join(envHome(env), "state.db") }

// Opens the owner's ledger connection with owner only files, refusing a worker and a removed profile
func asyncConnect(ctx context.Context, env *Env) (state.SQLiteConn, error) {
	if isWorkerProcess() {
		return nil, NewWorkerPersistenceError("worker_delegation_ledger_unavailable")
	}
	path := asyncDBPath(env)
	if _, err := home.Mkdir(WithEnvHome(ctx, env), filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := state.SecureStateDBFiles(path, true); err != nil {
		return nil, err
	}
	conn, err := state.OpenDB(ctx, path, asyncDelegationLedgerDBLabel, state.WithBusyTimeoutMS(asyncDelegationBusyTimeoutMS),
		state.WithWAL(false), state.WithInitialize(asyncInitializeSchema))
	if err != nil {
		return nil, err
	}
	if err := state.SecureStateDBFiles(path, false); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Keeps SessionDB's journal mode and reconciles the canonical schema that owns async_delegations
func asyncInitializeSchema(ctx context.Context, conn state.SQLiteConn) error {
	state.ApplyDurabilityBarriers(ctx, conn)
	return state.ReconcileStateSchema(ctx, conn)
}

// Runs body in one ledger transaction under the process wide ledger lock
func asyncTransaction(env *Env, body func(ctx context.Context, conn state.SQLiteConn) error) error {
	asyncDBMu.Lock()
	defer asyncDBMu.Unlock()
	ctx := context.Background()
	conn, err := asyncConnect(ctx, env)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, false, func(c state.SQLiteConn) error { return body(ctx, c) })
}

// Runs body in one ledger transaction, exported for delegation lifecycle callers
func AsyncTransaction(env *Env, body func(ctx context.Context, conn state.SQLiteConn) error) error {
	return asyncTransaction(env, body)
}

// Every row of a query as raw column values
func asyncQueryAll(ctx context.Context, conn state.SQLiteConn, query string, args ...any) ([][]any, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
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
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// Rows one statement changed
func asyncExec(ctx context.Context, conn state.SQLiteConn, query string, args ...any) (int64, error) {
	res, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// json.dumps of a ledger value
func asyncDumps(v any) (string, error) { return pyjson.Dumps(v) }

// json.loads of a ledger payload into plain maps, slices and ints
func asyncLoads(raw string) (any, error) {
	v, err := pyjson.Loads(raw)
	if err != nil {
		return nil, err
	}
	return asyncNative(v), nil
}

// Ordered JSON values as plain Go maps and slices with int counts
func asyncNative(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		if t == nil {
			return nil
		}
		m := make(map[string]any, len(t.Keys))
		for _, k := range t.Keys {
			m[k] = asyncNative(t.Vals[k])
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = asyncNative(val)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = asyncNative(val)
		}
		return out
	case int64:
		return int(t)
	case *big.Int:
		if t.IsInt64() {
			return int(t.Int64())
		}
		return t
	}
	return v
}

// Loaded JSON object, an empty map for anything else
func asyncObject(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	v, err := asyncLoads(raw)
	if err != nil {
		return map[string]any{}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func asyncText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	}
	return pyrepr.Str(v)
}

// Python str of a value or the empty string when falsy
func asyncStrOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return asyncText(v)
}

func asyncFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Python isinstance int over a JSON or row value, bool included
func asyncInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Python equality of two JSON values with ints and floats compared by value
func asyncSame(a, b any) bool {
	fa, oka := asyncFloat(a)
	fb, okb := asyncFloat(b)
	if oka && okb {
		return fa == fb
	}
	return reflect.DeepEqual(a, b)
}

func asyncRound(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.RoundToEven(v*p) / p
}

func asyncUUIDHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

func asyncMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func asyncCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Snapshot of the session routing origin so a replayed completion can rebuild its SessionSource
func captureRoutingOrigin(env *Env) map[string]any {
	out := map[string]any{}
	for _, k := range asyncRoutingKeys {
		if v := sessionEnvValue(env, "ICEBATH_SESSION_"+strings.ToUpper(k)); v != "" {
			out[k] = v
		}
	}
	return out
}

// Writes the running row of a fresh dispatch, then bounds the terminal history
func persistDispatch(env *Env, record map[string]any) error {
	remote, err := workerLedger()
	if err != nil {
		return err
	}
	if remote != nil {
		_, err := remote.Dispatch(record)
		return err
	}
	now := asyncTimeNow()
	_, startTime := asyncProbes()
	var ownerStartedAt any
	if st, ok := startTime(os.Getpid()); ok {
		ownerStartedAt = st
	}
	taskPayload := pyjson.NewObject()
	for _, key := range append([]string{"goal", "goals", "context", "toolsets", "role", "model", "is_batch", "task_indexes", "task_transcripts"}, asyncRoutingKeys...) {
		if v, ok := record[key]; ok {
			taskPayload.Set(key, v)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		taskPayload.Set("owner_cwd", cwd)
	}
	taskJSON, err := asyncDumps(taskPayload)
	if err != nil {
		return err
	}
	var parent any
	if p, ok := record["parent_session_id"]; ok {
		parent = p
	}
	err = asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := asyncExec(ctx, conn, `INSERT OR REPLACE INTO async_delegations
               (delegation_id, origin_session, origin_ui_session_id,
                parent_session_id, state, dispatched_at, updated_at,
                delivery_state, delivery_attempts, owner_pid,
                owner_started_at, task_json, origin_session_id)
               VALUES (?, ?, ?, ?, 'running', ?, ?, 'pending', 0, ?, ?, ?, ?)`,
			record["delegation_id"], asyncText(record["session_key"]), asyncText(record["origin_ui_session_id"]),
			parent, record["dispatched_at"], now, os.Getpid(), ownerStartedAt, taskJSON, asyncText(record["origin_session_id"]))
		return err
	})
	if err != nil {
		return err
	}
	return pruneDurableRecords(env)
}

// Writes the running row of a fresh dispatch, exported for delegation lifecycle callers
func PersistDispatch(env *Env, record map[string]any) error { return persistDispatch(env, record) }

// Bounds terminal ledger history, deleting delivered rows first
func pruneDurableRecords(env *Env) error {
	if err := requireLedgerOwner(); err != nil {
		return err
	}
	cutoff := asyncTimeNow() - asyncDurableRetentionSeconds
	return asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		if _, err := asyncExec(ctx, conn, "DELETE FROM async_delegations WHERE delivery_state='delivered' AND updated_at < ?", cutoff); err != nil {
			return err
		}
		var terminal int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM async_delegations WHERE state NOT IN ('running','finalizing')").Scan(&terminal); err != nil {
			return err
		}
		if terminal > asyncMaxRetainedCompleted {
			if _, err := asyncExec(ctx, conn, `DELETE FROM async_delegations WHERE delegation_id IN (
                     SELECT delegation_id FROM async_delegations
                     WHERE state NOT IN ('running','finalizing')
                     ORDER BY CASE delivery_state WHEN 'delivered' THEN 0 ELSE 1 END,
                              updated_at ASC LIMIT ?
                   )`, terminal-asyncMaxRetainedCompleted); err != nil {
				return err
			}
		}
		var pending int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM async_delegations
               WHERE state NOT IN ('running','finalizing') AND delivery_state='pending'`).Scan(&pending); err != nil {
			return err
		}
		if pending > asyncMaxDurablePending {
			if _, err := asyncExec(ctx, conn, `DELETE FROM async_delegations WHERE delegation_id IN (
                     SELECT delegation_id FROM async_delegations
                     WHERE state NOT IN ('running','finalizing') AND delivery_state='pending'
                     ORDER BY updated_at ASC LIMIT ?
                   )`, pending-asyncMaxDurablePending); err != nil {
				return err
			}
		}
		return nil
	})
}

// Writes the terminal event and result with forced redaction, leaving delivery pending
func persistCompletion(env *Env, event, result map[string]any) error {
	event = asyncMap(RedactCompletionEvent(env, event, true))
	result = asyncMap(RedactChildResult(env, result, nil, true))
	remote, err := workerLedger()
	if err != nil {
		return err
	}
	if remote != nil {
		_, err := remote.Complete(event, result)
		return err
	}
	now := asyncTimeNow()
	eventJSON, err := asyncDumps(event)
	if err != nil {
		return err
	}
	resultJSON, err := asyncDumps(result)
	if err != nil {
		return err
	}
	status := any("completed")
	if v, ok := event["status"]; ok {
		status = v
	}
	completedAt := any(now)
	if v, ok := event["completed_at"]; ok {
		completedAt = v
	}
	return asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := asyncExec(ctx, conn, `UPDATE async_delegations SET state=?, completed_at=?, updated_at=?,
               event_json=?, result_json=?, delivery_state='pending'
               WHERE delegation_id=?`, status, completedAt, now, eventJSON, resultJSON, event["delegation_id"])
		return err
	})
}

// Durably records one finished child of a running multi child unit so a crash loses only unfinished children
func RecordUnitChild(env *Env, delegationID string, entry map[string]any) error {
	entry = asyncMap(RedactChildResult(env, entry, nil, true))
	remote, err := workerLedger()
	if err != nil {
		return err
	}
	if remote != nil {
		_, err := remote.Child(delegationID, entry)
		return err
	}
	err = asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := asyncQueryAll(ctx, conn, "SELECT result_json FROM async_delegations WHERE delegation_id=? AND state='running'", delegationID)
		if err != nil || len(rows) == 0 {
			return err
		}
		raw := asyncText(rows[0][0])
		if raw == "" {
			raw = "{}"
		}
		loaded, err := asyncLoads(raw)
		if err != nil {
			return err
		}
		if !pyrepr.Truthy(loaded) {
			loaded = map[string]any{}
		}
		partial := asyncMap(RedactChildResult(env, loaded, nil, true))
		var results []any
		prior, _ := partial["results"].([]any)
		for _, r := range prior {
			if !asyncSame(asyncMap(r)["task_index"], entry["task_index"]) {
				results = append(results, r)
			}
		}
		results = append(results, entry)
		payload, err := asyncDumps(map[string]any{"results": results, "partial": true})
		if err != nil {
			return err
		}
		_, err = asyncExec(ctx, conn, "UPDATE async_delegations SET result_json=?, updated_at=? WHERE delegation_id=? AND state='running'",
			payload, asyncTimeNow(), delegationID)
		return err
	})
	if err != nil {
		var wpe *WorkerPersistenceError
		if errors.As(err, &wpe) {
			return err
		}
		asyncLog().Warn(fmt.Sprintf("Async delegation %s: could not record finished child %s", delegationID, pyrepr.Str(entry["task_index"])), "error", err)
	}
	return nil
}

// Per task results of an abandoned unit, recorded children as they finished and the rest unknown, nil when none
func recoveredResults(task map[string]any, resultJSON, errText string) []any {
	if resultJSON == "" {
		resultJSON = "{}"
	}
	loaded, err := asyncLoads(resultJSON)
	if err != nil {
		return nil
	}
	partial := asyncMap(loaded)
	if !(pyrepr.Truthy(task["is_batch"]) && pyrepr.Truthy(partial["partial"]) && pyrepr.Truthy(partial["results"])) {
		return nil
	}
	recorded := map[int]map[string]any{}
	list, _ := partial["results"].([]any)
	for _, r := range list {
		rm := asyncMap(r)
		if i, ok := asyncInt(rm["task_index"]); ok {
			recorded[i] = rm
		}
	}
	var indexes []int
	if raw, ok := task["task_indexes"].([]any); ok && len(raw) > 0 {
		for _, v := range raw {
			i, _ := asyncInt(v)
			indexes = append(indexes, i)
		}
	} else {
		goals, _ := task["goals"].([]any)
		for i := range goals {
			indexes = append(indexes, i)
		}
	}
	out := make([]any, 0, len(indexes))
	for _, i := range indexes {
		if r := recorded[i]; len(r) > 0 {
			out = append(out, r)
			continue
		}
		out = append(out, map[string]any{"task_index": i, "status": "unknown", "summary": nil, "error": errText})
	}
	return out
}

// Whether a recorded owner pid still names the same live process
func asyncOwnerAlive(pid, started any) bool {
	pidExists, startTime := asyncProbes()
	p, ok := asyncInt(pid)
	if !ok || p == 0 || !pidExists(p) {
		return false
	}
	if started == nil {
		return true
	}
	recorded, _ := asyncInt(started)
	current, _ := startTime(p)
	return StartTimeFingerprintsMatch(int64(recorded), current)
}

// Classifies rows whose owner process disappeared as outcome unknown, replaying recorded unit children
func RecoverAbandonedDelegations(env *Env) (int, error) {
	if err := requireLedgerOwner(); err != nil {
		return 0, err
	}
	asyncProbes()
	now, recovered := asyncTimeNow(), 0
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := asyncQueryAll(ctx, conn, `SELECT delegation_id, origin_session, origin_ui_session_id,
                      parent_session_id, dispatched_at, owner_pid,
                      owner_started_at, task_json, origin_session_id, result_json, state
               FROM async_delegations WHERE state IN ('running','finalizing')`)
		if err != nil {
			return err
		}
		for _, row := range rows {
			delegationID, sessionKey, originUI, parentID, dispatchedAt := asyncText(row[0]), row[1], row[2], row[3], row[4]
			if asyncOwnerAlive(row[5], row[6]) {
				continue
			}
			taskRaw := asyncText(row[7])
			if taskRaw == "" {
				taskRaw = "{}"
			}
			loaded, err := asyncLoads(taskRaw)
			if err != nil {
				return err
			}
			task := asyncMap(loaded)
			errText := "Delegation owner exited before recording a terminal result; outcome unknown."
			recoveredRes := recoveredResults(task, asyncText(row[9]), errText)
			if len(recoveredRes) > 0 {
				done := 0
				for _, r := range recoveredRes {
					if asyncMap(r)["status"] != "unknown" {
						done++
					}
				}
				errText = fmt.Sprintf("Delegation owner exited before the unit finished; %d/%d child "+
					"results were recorded and are included below, the rest are unknown.", done, len(recoveredRes))
			}
			transcripts := map[string]any{}
			if t, ok := task["task_transcripts"].(map[string]any); ok && len(t) > 0 {
				transcripts = t
			}
			diagnostics := map[string]any{"last_known_status": row[10], "task_transcripts": transcripts}
			paths := map[string]string{}
			for k, v := range transcripts {
				paths[k] = asyncText(v)
			}
			if tails := TranscriptTails(paths); len(tails) > 0 {
				diagnostics["transcript_tails"] = tails
			}
			if hint := GitStateHint(asyncText(task["owner_cwd"])); hint != "" {
				diagnostics["git_state_hint"] = hint
			}
			goal := any("")
			if v, ok := task["goal"]; ok {
				goal = v
			}
			event := map[string]any{
				"type": "async_delegation", "delegation_id": delegationID, "session_key": sessionKey,
				"origin_ui_session_id": originUI, "origin_session_id": asyncStrOr(row[8]),
				"parent_session_id": parentID, "goal": goal, "goals": task["goals"],
				"context": task["context"], "toolsets": task["toolsets"], "role": task["role"],
				"model": task["model"], "is_batch": pyrepr.Truthy(task["is_batch"]),
				"status": "unknown", "summary": nil, "error": errText,
			}
			result := map[string]any{"status": "unknown", "summary": nil, "error": errText}
			for k, v := range diagnostics {
				event[k] = v
				result[k] = v
			}
			if len(recoveredRes) > 0 {
				event["results"] = recoveredRes
				result["results"] = recoveredRes
			}
			event["dispatched_at"] = dispatchedAt
			event["completed_at"] = now
			for _, k := range asyncRoutingKeys {
				if pyrepr.Truthy(task[k]) {
					event[k] = task[k]
				}
			}
			event = asyncMap(RedactCompletionEvent(env, event, true))
			result = asyncMap(RedactChildResult(env, result, nil, true))
			eventJSON, err := asyncDumps(event)
			if err != nil {
				return err
			}
			resultJSON, err := asyncDumps(result)
			if err != nil {
				return err
			}
			if _, err := asyncExec(ctx, conn, `UPDATE async_delegations SET state='unknown', completed_at=?,
                   updated_at=?, event_json=?, result_json=?, delivery_state='pending'
                   WHERE delegation_id=?`, now, now, eventJSON, resultJSON, delegationID); err != nil {
				return err
			}
			recovered++
		}
		return nil
	})
	return recovered, err
}

// Enqueues durable pending completions of a previous process stamped restored, never creating the ledger
func RestoreUndeliveredCompletions(env *Env, q *EventQueue) (int, error) {
	if isWorkerProcess() {
		return 0, nil
	}
	if _, err := os.Stat(asyncDBPath(env)); err != nil {
		return 0, nil
	}
	if _, err := RecoverAbandonedDelegations(env); err != nil {
		return 0, err
	}
	now, restored := asyncTimeNow(), 0
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := asyncQueryAll(ctx, conn, `SELECT delegation_id, event_json, completed_at, dispatched_at
               FROM async_delegations
               WHERE state != 'running' AND delivery_state='pending' AND event_json IS NOT NULL
               ORDER BY completed_at, delegation_id`)
		if err != nil {
			return err
		}
		restored, err = replayPending(ctx, env, conn, rows, q, now, true)
		return err
	})
	return restored, err
}

// Puts each pending row on q stamped restored, terminally dropping rows past the replay age
func replayPending(ctx context.Context, env *Env, conn state.SQLiteConn, rows [][]any, q *EventQueue, now float64, startup bool) (int, error) {
	homeKey, restored := home.Key(envHome(env)), 0
	for _, row := range rows {
		delegationID, payload := asyncText(row[0]), asyncText(row[1])
		ageBasis, ok := asyncFloat(row[2])
		if !ok || ageBasis == 0 {
			ageBasis, ok = asyncFloat(row[3])
		}
		if ok && ageBasis != 0 && now-ageBasis > asyncMaxCompletionReplayAgeS {
			if _, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_state='dropped',
                          delivery_claim=NULL, delivery_claimed_at=NULL,
                          updated_at=?
                   WHERE delegation_id=? AND delivery_state='pending'`, now, delegationID); err != nil {
				return restored, err
			}
			asyncLog().Warn(fmt.Sprintf("Async delegation %s: pending completion is %.1fh old "+
				"(cap %.1fh); terminally dropping the replay (result remains queryable).",
				delegationID, (now-ageBasis)/3600.0, asyncMaxCompletionReplayAgeS/3600.0))
			continue
		}
		asyncOrphanMu.Lock()
		seen := asyncOffered[[2]string{homeKey, delegationID}]
		asyncOrphanMu.Unlock()
		if seen {
			continue
		}
		loaded, err := asyncLoads(payload)
		if err != nil {
			return restored, err
		}
		evt, isMap := loaded.(map[string]any)
		if !isMap {
			asyncLog().Warn(fmt.Sprintf("Async delegation %s: stored completion is not a JSON object; leaving it pending", delegationID))
			continue
		}
		evt = asyncMap(RedactCompletionEvent(env, evt, true))
		asyncOrphanMu.Lock()
		if startup {
			asyncStartupReplay[delegationID] = true
		}
		evt["restored"] = true
		if asyncStartupReplay[delegationID] {
			evt["startup_replay"] = true
		}
		asyncOrphanMu.Unlock()
		q.Put(evt)
		asyncOrphanMu.Lock()
		asyncOffered[[2]string{homeKey, delegationID}] = true
		asyncOrphanMu.Unlock()
		restored++
	}
	return restored, nil
}

// Delegation id an event is the final result copy of, empty for other events and interim notices
func CompletionCopyID(evt map[string]any) string {
	if evt == nil || evt["type"] != "async_delegation" || IsInterimDelegationEvent(evt) {
		return ""
	}
	return asyncStrOr(evt["delegation_id"])
}

// Records that a consumer holds one copy of a delegation off every queue
func MarkCopyHeld(delegationID string) {
	if delegationID == "" {
		return
	}
	asyncHeldMu.Lock()
	asyncHeldCopies[delegationID]++
	asyncHeldMu.Unlock()
}

// Ends one hold on a delegation copy
func UnmarkCopyHeld(delegationID string) {
	if delegationID == "" {
		return
	}
	asyncHeldMu.Lock()
	asyncHeldCopies[delegationID]--
	if asyncHeldCopies[delegationID] <= 0 {
		delete(asyncHeldCopies, delegationID)
	}
	asyncHeldMu.Unlock()
}

// Delegation ids with a final result copy queued in any queue or held off queue, read under each queue mutex
func queuedCompletionIDs(queues ...*EventQueue) map[string]bool {
	ids := map[string]bool{}
	seen := map[*EventQueue]bool{}
	for _, q := range queues {
		if q == nil || seen[q] {
			continue
		}
		seen[q] = true
		q.mu.Lock()
		snapshot := append([]map[string]any{}, q.items...)
		asyncHeldMu.Lock()
		for id := range asyncHeldCopies {
			ids[id] = true
		}
		asyncHeldMu.Unlock()
		q.mu.Unlock()
		for _, evt := range snapshot {
			ids[CompletionCopyID(evt)] = true
		}
	}
	delete(ids, "")
	return ids
}

// Whether this process's copy of a row may still be delivered, a row another live process owns is left to it
func copyMayBeLive(delegationID string, pid any, updatedAt float64, live map[string]bool, homeKey string, offered map[string]bool, now float64) bool {
	p, ok := asyncInt(pid)
	if !(offered[delegationID] || (ok && p == os.Getpid())) {
		return true
	}
	if live[delegationID] || updatedAt >= now-asyncClaimLeaseS {
		return true
	}
	asyncOrphanMu.Lock()
	defer asyncOrphanMu.Unlock()
	at, ok := asyncReofferedAt[[2]string{homeKey, delegationID}]
	return ok && at >= now-asyncClaimLeaseS
}

// Starts each lost copy's re-offer lease, warning on a row's first re-offer and debug after that
func noteReoffers(homeKey string, lost []string, now float64) {
	asyncOrphanMu.Lock()
	first := false
	for _, d := range lost {
		if _, ok := asyncReofferedAt[[2]string{homeKey, d}]; !ok {
			first = true
		}
	}
	for key, at := range asyncReofferedAt {
		if now-at > asyncMaxCompletionReplayAgeS {
			delete(asyncReofferedAt, key)
		}
	}
	for _, d := range lost {
		asyncReofferedAt[[2]string{homeKey, d}] = now
		delete(asyncOffered, [2]string{homeKey, d})
	}
	asyncOrphanMu.Unlock()
	msg := fmt.Sprintf("Re-offering %d pending async completion(s) whose in-memory copy was lost while this process ran: %s",
		len(lost), strings.Join(lost, ", "))
	if first {
		asyncLog().Warn(msg)
	} else {
		asyncLog().Debug(msg)
	}
}

// Re-offers this home's pending completions that no live in memory copy will deliver
func SweepOrphanedCompletions(env *Env, q *EventQueue, now *float64) (int, error) {
	asyncProbes()
	if _, err := os.Stat(asyncDBPath(env)); err != nil {
		return 0, nil
	}
	if _, err := RecoverAbandonedDelegations(env); err != nil {
		return 0, err
	}
	at := asyncTimeNow()
	if now != nil {
		at = *now
	}
	homeKey := home.Key(envHome(env))
	live := queuedCompletionIDs(q, DefaultProcessRegistry().CompletionQueue)
	offered := map[string]bool{}
	asyncOrphanMu.Lock()
	for key := range asyncOffered {
		if key[0] == homeKey {
			offered[key[1]] = true
		}
	}
	asyncOrphanMu.Unlock()
	restored := 0
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := asyncQueryAll(ctx, conn, `SELECT delegation_id, event_json, completed_at, dispatched_at,
                      owner_pid, owner_started_at, delivery_attempts, updated_at
               FROM async_delegations
               WHERE state NOT IN ('running','finalizing') AND delivery_state='pending'
                 AND event_json IS NOT NULL AND updated_at < ?
                 AND (delivery_claim IS NULL OR delivery_claimed_at < ?)
               ORDER BY completed_at, delegation_id`, at-asyncOrphanStaleS, at-asyncClaimLeaseS)
		if err != nil {
			return err
		}
		var orphans [][]any
		var lost []string
		for _, row := range rows {
			delegationID := asyncText(row[0])
			updatedAt, _ := asyncFloat(row[7])
			copyLost := asyncOwnerAlive(row[4], row[5]) || offered[delegationID]
			if copyLost && copyMayBeLive(delegationID, row[4], updatedAt, live, homeKey, offered, at) {
				continue
			}
			if attempts, _ := asyncInt(row[6]); attempts >= asyncMaxDeliveryAttempts {
				if _, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_state='dropped',
                              delivery_claim=NULL, delivery_claimed_at=NULL, updated_at=?
                       WHERE delegation_id=? AND delivery_state='pending'`, at, delegationID); err != nil {
					return err
				}
				asyncLog().Warn(fmt.Sprintf("Async delegation %s exhausted its %d delivery attempts; "+
					"marking terminally dropped (result remains queryable).", delegationID, asyncMaxDeliveryAttempts))
				continue
			}
			orphans = append(orphans, []any{row[0], row[1], row[2], row[3]})
			if copyLost {
				lost = append(lost, delegationID)
			}
		}
		if len(lost) > 0 {
			noteReoffers(homeKey, lost, at)
		}
		restored, err = replayPending(ctx, env, conn, orphans, q, at, false)
		return err
	})
	return restored, err
}

// Orphan sweep the throttle runs, replaced by tests counting sweeps per home
var asyncSweepOrphanedFn = SweepOrphanedCompletions

// Runs the orphan sweep at most once per interval per home on the monotonic clock, never failing the loop
func MaybeSweepOrphanedCompletions(env *Env, q *EventQueue, now *float64) int {
	homeKey := home.Key(envHome(env))
	at := asyncMonotonic()
	if now != nil {
		at = *now
	}
	asyncOrphanMu.Lock()
	if last, ok := asyncLastOrphanTick[homeKey]; ok && at-last < OrphanSweepIntervalS {
		asyncOrphanMu.Unlock()
		return 0
	}
	asyncLastOrphanTick[homeKey] = at
	asyncOrphanMu.Unlock()
	n, err := func() (n int, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return asyncSweepOrphanedFn(env, q, nil)
	}()
	if err != nil {
		asyncLog().Debug("Orphaned async delegation sweep failed", "error", err)
		return 0
	}
	return n
}

// Runs one ledger UPDATE, true when exactly one row changed
func updateDelivery(env *Env, query string, args ...any) (bool, error) {
	changed := false
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		n, err := asyncExec(ctx, conn, query, args...)
		changed = n == 1
		return err
	})
	return changed, err
}

// Acknowledges a durable completion as delivered
func MarkCompletionDelivered(env *Env, delegationID string) (bool, error) {
	if err := requireLedgerOwner(); err != nil {
		return false, err
	}
	now := asyncTimeNow()
	return updateDelivery(env, `UPDATE async_delegations SET delivery_state='delivered', delivered_at=?, updated_at=?
           WHERE delegation_id=? AND delivery_state!='delivered'`, now, now, delegationID)
}

func asyncRemoteBool(v any, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(v), nil
}

// Claims one pending completion across competing consumers and processes
func ClaimCompletionDelivery(env *Env, delegationID, claimID string) (bool, error) {
	remote, err := workerLedger()
	if err != nil {
		return false, err
	}
	if remote != nil {
		return asyncRemoteBool(remote.Claim(delegationID, claimID))
	}
	now := asyncTimeNow()
	claimed := false
	err = asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := asyncQueryAll(ctx, conn, "SELECT delivery_state FROM async_delegations WHERE delegation_id=?", delegationID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			claimed = true
			return nil
		}
		n, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_claim=?, delivery_claimed_at=?,
                      delivery_attempts=delivery_attempts+1, updated_at=?
               WHERE delegation_id=? AND delivery_state='pending'
                 AND (delivery_claim IS NULL OR delivery_claimed_at < ?)`,
			claimID, now, now, delegationID, now-asyncClaimLeaseS)
		claimed = n == 1
		return err
	})
	return claimed, err
}

// Reports an early per task notice of a running batch, never the durable completion
func IsInterimDelegationEvent(evt map[string]any) bool {
	return evt["type"] == "async_delegation" && pyrepr.Truthy(evt["task_failure_notice"])
}

// Claims a durable delegation event, an empty token when none is needed and false when another consumer holds it
func ClaimEventDelivery(env *Env, evt map[string]any, consumer string) (string, bool, error) {
	if IsInterimDelegationEvent(evt) {
		return "", true, nil
	}
	delegationID := ""
	if evt["type"] == "async_delegation" {
		delegationID = asyncStrOr(evt["delegation_id"])
	}
	if delegationID == "" {
		return "", true, nil
	}
	claimID := fmt.Sprintf("%s:%d:%s", consumer, os.Getpid(), asyncUUIDHex())
	ok, err := ClaimCompletionDelivery(env, delegationID, claimID)
	if err != nil || !ok {
		return "", false, err
	}
	return claimID, true, nil
}

// Releases a failed claim for retry, converging to dropped once the attempt budget is spent
func ReleaseCompletionDelivery(env *Env, delegationID, claimID string) (bool, error) {
	remote, err := workerLedger()
	if err != nil {
		return false, err
	}
	if remote != nil {
		return asyncRemoteBool(remote.Release(delegationID, claimID))
	}
	now := asyncTimeNow()
	released := false
	err = asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		capped, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_state='dropped',
                      delivery_claim=NULL, delivery_claimed_at=NULL, updated_at=?
               WHERE delegation_id=? AND delivery_state='pending'
                 AND delivery_claim=? AND delivery_attempts>=?`, now, delegationID, claimID, asyncMaxDeliveryAttempts)
		if err != nil {
			return err
		}
		if capped == 1 {
			asyncLog().Warn(fmt.Sprintf("Async delegation %s exhausted its %d delivery attempts; "+
				"marking terminally dropped (result remains queryable).", delegationID, asyncMaxDeliveryAttempts))
			released = true
			return nil
		}
		n, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_claim=NULL,
                      delivery_claimed_at=NULL, updated_at=?
               WHERE delegation_id=? AND delivery_state='pending'
                 AND delivery_claim=?`, now, delegationID, claimID)
		released = n == 1
		return err
	})
	return released, err
}

// Returns an unadmitted completion to pending without spending a delivery attempt
func DeferCompletionDelivery(env *Env, delegationID, claimID string) (bool, error) {
	remote, err := workerLedger()
	if err != nil {
		return false, err
	}
	if remote != nil {
		return asyncRemoteBool(remote.Defer(delegationID, claimID))
	}
	return updateDelivery(env, `UPDATE async_delegations SET delivery_claim=NULL,
                  delivery_claimed_at=NULL, delivery_attempts=MAX(0, delivery_attempts-1),
                  updated_at=?
           WHERE delegation_id=? AND delivery_state='pending' AND delivery_claim=?`, asyncTimeNow(), delegationID, claimID)
}

// Terminally drops a claimed completion whose target session is permanently gone
func DropCompletionDelivery(env *Env, delegationID, claimID string) (bool, error) {
	remote, err := workerLedger()
	if err != nil {
		return false, err
	}
	if remote != nil {
		return asyncRemoteBool(remote.Drop(delegationID, claimID))
	}
	return updateDelivery(env, `UPDATE async_delegations SET delivery_state='dropped',
                  updated_at=?, delivery_claim=NULL,
                  delivery_claimed_at=NULL
           WHERE delegation_id=? AND delivery_state='pending'
             AND delivery_claim=?`, asyncTimeNow(), delegationID, claimID)
}

// Acknowledges acceptance for the consumer holding the claim
func CompleteCompletionDelivery(env *Env, delegationID, claimID string) (bool, error) {
	remote, err := workerLedger()
	if err != nil {
		return false, err
	}
	if remote != nil {
		return asyncRemoteBool(remote.Ack(delegationID, claimID))
	}
	now := asyncTimeNow()
	return updateDelivery(env, `UPDATE async_delegations SET delivery_state='delivered',
                  delivered_at=?, updated_at=?, delivery_claim=NULL,
                  delivery_claimed_at=NULL
           WHERE delegation_id=? AND delivery_state='pending'
             AND delivery_claim=?`, now, now, delegationID, claimID)
}

// Reopens an acknowledged completion whose turn could not confirm the owner and offers it on q again
func RequeueAdmittedCompletion(env *Env, delegationID string, q *EventQueue) (bool, error) {
	now := asyncTimeNow()
	requeued := false
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		exhausted, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_state='dropped', updated_at=?
               WHERE delegation_id=? AND delivery_state='delivered' AND delivery_attempts>=?`, now, delegationID, asyncMaxDeliveryAttempts)
		if err != nil {
			return err
		}
		if exhausted == 1 {
			asyncLog().Warn(fmt.Sprintf("Async delegation %s exhausted its %d delivery attempts; "+
				"marking terminally dropped (result remains queryable).", delegationID, asyncMaxDeliveryAttempts))
			return nil
		}
		reopened, err := asyncExec(ctx, conn, `UPDATE async_delegations SET delivery_state='pending',
                      delivered_at=NULL, updated_at=?
               WHERE delegation_id=? AND delivery_state='delivered' AND event_json IS NOT NULL`, now, delegationID)
		if err != nil || reopened != 1 {
			return err
		}
		rows, err := asyncQueryAll(ctx, conn, `SELECT delegation_id, event_json, completed_at, dispatched_at
               FROM async_delegations WHERE delegation_id=?`, delegationID)
		if err != nil {
			return err
		}
		asyncOrphanMu.Lock()
		delete(asyncStartupReplay, delegationID)
		asyncOrphanMu.Unlock()
		n, err := replayPending(ctx, env, conn, rows, q, now, false)
		requeued = n == 1
		return err
	})
	return requeued, err
}

// Acknowledges the claimed delivery of a delegation event
func CompleteEventDelivery(env *Env, evt map[string]any, claimID string) error {
	return eventDelivery(env, CompleteCompletionDelivery, evt, claimID)
}

// Releases a failed claim for a consumer that discards its copy, handing the row back to the orphan sweep
func ReleaseEventDelivery(env *Env, evt map[string]any, claimID string) error {
	err := eventDelivery(env, ReleaseCompletionDelivery, evt, claimID)
	ReturnCompletionOffer(evt)
	return err
}

// Hands a discarded offered completion back to the orphan sweep in every home
func ReturnCompletionOffer(evt map[string]any) {
	delegationID := ""
	if evt["type"] == "async_delegation" {
		delegationID = asyncStrOr(evt["delegation_id"])
	}
	if delegationID == "" || IsInterimDelegationEvent(evt) {
		return
	}
	asyncOrphanMu.Lock()
	for key := range asyncOffered {
		if key[1] == delegationID {
			delete(asyncOffered, key)
		}
	}
	asyncOrphanMu.Unlock()
}

func eventDelivery(env *Env, fn func(env *Env, delegationID, claimID string) (bool, error), evt map[string]any, claimID string) error {
	if claimID != "" && evt["type"] == "async_delegation" {
		_, err := fn(env, asyncStrOr(evt["delegation_id"]), claimID)
		return err
	}
	return nil
}

// Durable row of a delegation with its result redacted, nil when unknown
func GetDurableDelegation(env *Env, delegationID string) (map[string]any, error) {
	remote, err := workerLedger()
	if err != nil {
		return nil, err
	}
	if remote != nil {
		v, err := remote.Read(delegationID)
		if err != nil {
			return nil, err
		}
		saved, _ := asyncNative(v).(map[string]any)
		if saved != nil && saved["result"] != nil {
			out := asyncCopyMap(saved)
			out["result"] = RedactChildResult(env, saved["result"], nil, true)
			return out, nil
		}
		return saved, nil
	}
	var row []any
	err = asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := asyncQueryAll(ctx, conn, `SELECT origin_session, state, dispatched_at, completed_at,
                      result_json, delivery_state, delivery_attempts,
                      origin_session_id
               FROM async_delegations WHERE delegation_id=?`, delegationID)
		if len(rows) > 0 {
			row = rows[0]
		}
		return err
	})
	if err != nil || row == nil {
		return nil, err
	}
	var result any
	if raw := asyncText(row[4]); raw != "" {
		loaded, err := asyncLoads(raw)
		if err != nil {
			return nil, err
		}
		result = RedactChildResult(env, loaded, nil, true)
	}
	return map[string]any{
		"delegation_id": delegationID, "origin_session": row[0], "state": row[1], "dispatched_at": row[2],
		"completed_at": row[3], "result": result, "delivery_state": row[5],
		"delivery_attempts": asyncNative(row[6]), "origin_session_id": asyncStrOr(row[7]),
	}, nil
}

// Recently failed async delegation tasks a session owns from the durable rows, newest first
func FailedDelegationsForSession(env *Env, originUISessionID, parentSessionID string, limit int, now *float64) ([]map[string]any, error) {
	var cols []string
	var args []any
	for _, sel := range [][2]string{{"origin_ui_session_id", originUISessionID}, {"parent_session_id", parentSessionID}} {
		if sel[1] != "" {
			cols = append(cols, sel[0]+"=?")
			args = append(args, sel[1])
		}
	}
	if len(cols) == 0 {
		return []map[string]any{}, nil
	}
	at := asyncTimeNow()
	if now != nil {
		at = *now
	}
	args = append(args, at-asyncFailureSurfaceWindowS, limit)
	var rows [][]any
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		var err error
		rows, err = asyncQueryAll(ctx, conn, `SELECT delegation_id, state, dispatched_at, completed_at, task_json, result_json FROM async_delegations
                WHERE (`+strings.Join(cols, " OR ")+`) AND state NOT IN ('running','finalizing') AND completed_at >= ?
                ORDER BY completed_at DESC LIMIT ?`, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	failed := []map[string]any{}
	for _, row := range rows {
		state := row[1]
		task := asyncObject(asyncText(row[4]))
		result := asyncMap(RedactChildResult(env, asyncObject(asyncText(row[5])), nil, true))
		goals := []any{task["goal"]}
		if g, ok := task["goals"].([]any); ok && len(g) > 0 {
			goals = g
		} else if !pyrepr.Truthy(task["goal"]) {
			goals = []any{""}
		}
		goalFor := map[int]any{}
		if idx, ok := task["task_indexes"].([]any); ok && len(idx) > 0 {
			for i, v := range idx {
				if i < len(goals) {
					n, _ := asyncInt(v)
					goalFor[n] = goals[i]
				}
			}
		} else {
			for i, g := range goals {
				goalFor[i] = g
			}
		}
		var tasks []any
		if list, ok := result["results"].([]any); ok {
			tasks = list
		} else if !pyrepr.Truthy(task["is_batch"]) {
			tasks = []any{result}
		}
		if len(tasks) == 0 && asyncFailedStates[strings.ToLower(asyncText(state))] {
			tasks = []any{map[string]any{"task_index": 0, "error": result["error"]}}
		}
		for _, e := range tasks {
			entry := asyncMap(e)
			statusRaw := entry["status"]
			if !pyrepr.Truthy(statusRaw) {
				statusRaw = state
			}
			status := strings.ToLower(asyncStrOr(statusRaw))
			if !asyncFailedStates[status] {
				continue
			}
			index, ok := asyncInt(entry["task_index"])
			if !ok {
				index = 0
			}
			errVal := entry["error"]
			if !pyrepr.Truthy(errVal) {
				errVal = result["error"]
			}
			goal, ok := goalFor[index]
			if !ok {
				goal = goals[0]
			}
			var errOut any
			if pyrepr.Truthy(errVal) {
				errOut = asyncText(errVal)
			}
			failed = append(failed, map[string]any{
				"delegation_id": asyncText(row[0]), "task_index": index, "status": status,
				"goal": asyncStrOr(goal), "error": errOut, "dispatched_at": row[2], "completed_at": row[3],
			})
		}
	}
	if len(failed) > limit {
		failed = failed[:limit]
	}
	return failed, nil
}

// Shared daemon executor, created lazily and grown in place but never shrunk
func asyncGetExecutor(maxWorkers int) *DaemonPool {
	asyncExecutorMu.Lock()
	defer asyncExecutorMu.Unlock()
	if asyncExecutor == nil {
		asyncExecutor = NewDaemonPool(maxWorkers, asyncDelegationExecutorPrefix)
		asyncExecutorMax = maxWorkers
	} else if maxWorkers > asyncExecutorMax {
		asyncExecutor.mu.Lock()
		asyncExecutor.maxWorkers = maxWorkers
		asyncExecutor.mu.Unlock()
		asyncExecutorMax = maxWorkers
	}
	return asyncExecutor
}

func asyncRecordList() []*asyncRecord {
	out := make([]*asyncRecord, 0, len(asyncRecordOrder))
	for _, id := range asyncRecordOrder {
		if r := asyncRecords[id]; r != nil {
			out = append(out, r)
		}
	}
	return out
}

// SwapAsyncRecords installs plain delegation records in id order the way hermes cases patch the records table, returning the restore
func SwapAsyncRecords(ids []string, records map[string]map[string]any) (restore func()) {
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	savedRecords, savedOrder := asyncRecords, asyncRecordOrder
	asyncRecords, asyncRecordOrder = map[string]*asyncRecord{}, append([]string(nil), ids...)
	for _, id := range ids {
		asyncRecords[id] = &asyncRecord{m: records[id]}
	}
	return func() {
		asyncRecordsMu.Lock()
		defer asyncRecordsMu.Unlock()
		asyncRecords, asyncRecordOrder = savedRecords, savedOrder
	}
}

func asyncDeleteRecordLocked(id string) {
	if _, ok := asyncRecords[id]; !ok {
		return
	}
	delete(asyncRecords, id)
	for i, v := range asyncRecordOrder {
		if v == id {
			asyncRecordOrder = append(asyncRecordOrder[:i], asyncRecordOrder[i+1:]...)
			break
		}
	}
}

func (r *asyncRecord) status() string { return asyncText(r.m["status"]) }

// Live async delegation units, one per completion message
func AsyncDelegationActiveCount() int {
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	n := 0
	for _, r := range asyncRecordList() {
		if asyncLiveStates[r.status()] {
			n++
		}
	}
	return n
}

// Running child subagents, a batch of N counting N
func AsyncDelegationActiveTaskCount() int {
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	n := 0
	for _, r := range asyncRecordList() {
		if s := r.status(); s != "running" && s != "finalizing" {
			continue
		}
		goals, _ := r.m["goals"].([]string)
		if pyrepr.Truthy(r.m["is_batch"]) && len(goals) > 0 {
			if idx, ok := r.m["task_indexes"].([]int); ok && len(idx) > 0 {
				n += len(idx)
			} else {
				n += len(goals)
			}
			continue
		}
		n++
	}
	return n
}

// Records in statuses a session owns through any non empty selector
func sessionRecords(statuses map[string]bool, sessionKey, originUISessionID, parentSessionID string) []*asyncRecord {
	var sels [][2]string
	for _, s := range [][2]string{{"origin_ui_session_id", originUISessionID}, {"session_key", sessionKey}, {"parent_session_id", parentSessionID}} {
		if s[1] != "" {
			sels = append(sels, s)
		}
	}
	if len(sels) == 0 {
		return nil
	}
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	var out []*asyncRecord
	for _, r := range asyncRecordList() {
		if !statuses[r.status()] {
			continue
		}
		for _, s := range sels {
			if asyncStrOr(r.m[s[0]]) == s[1] {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// Whether a session still owns any running, stalling or finalizing delegation
func HasLiveForSession(sessionKey, originUISessionID, parentSessionID string) bool {
	return len(sessionRecords(asyncLiveStates, sessionKey, originUISessionID, parentSessionID)) > 0
}

// Number of live async delegation batches a parent session spawned
func LiveDelegationBatches(parentSessionID string) int {
	return len(sessionRecords(asyncLiveStates, "", "", parentSessionID))
}

func newDelegationID() string { return "deleg_" + asyncUUIDHex()[:8] }

// NewDelegationID is a fresh async delegation id
func NewDelegationID() string { return newDelegationID() }

// Drops the oldest completed records beyond the retention cap, caller holds the records lock
func pruneCompletedLocked() {
	type entry struct {
		id  string
		key float64
	}
	var completed []entry
	for _, r := range asyncRecordList() {
		if asyncLiveStates[r.status()] {
			continue
		}
		key, ok := asyncFloat(r.m["completed_at"])
		if !ok || key == 0 {
			key, _ = asyncFloat(r.m["dispatched_at"])
		}
		completed = append(completed, entry{asyncText(r.m["delegation_id"]), key})
	}
	sort.SliceStable(completed, func(i, j int) bool { return completed[i].key < completed[j].key })
	for _, e := range completed[:max(0, len(completed)-asyncMaxRetainedCompleted)] {
		asyncDeleteRecordLocked(e.id)
	}
}

// Raw session id of the originating api_server request, empty on push platforms
func currentOriginSessionID(env *Env) string {
	if sessionEnvValue(env, "ICEBATH_SESSION_PLATFORM") != "api_server" {
		return ""
	}
	return sessionEnvValue(env, "ICEBATH_SESSION_CHAT_ID")
}

// Raw session id of the originating api_server request, empty on push platforms
func CurrentOriginSessionID(env *Env) string { return currentOriginSessionID(env) }

func singleCrash(errText string, duration float64) map[string]any {
	return map[string]any{"status": "error", "summary": nil, "error": errText, "api_calls": 0, "duration_seconds": duration}
}

func batchCrash(errText string, duration float64) map[string]any {
	return map[string]any{"results": []any{}, "error": errText, "total_duration_seconds": duration}
}

// Batch status, completed unless every child errored or was interrupted
func batchStatus(combined map[string]any) string {
	results, _ := combined["results"].([]any)
	if len(results) == 0 {
		return "completed"
	}
	for _, r := range results {
		if s := asyncMap(r)["status"]; s == "completed" || s == "success" {
			return "completed"
		}
	}
	return "error"
}

// AsyncDispatch is one single or batch unit handed to the async delegation executor
type AsyncDispatch struct {
	DelegationID      string
	Goal              string
	Goals             []string
	IsBatch           bool
	Context           *string
	Toolsets          []string
	Role              string
	Model             *string
	SessionKey        string
	ParentSessionID   *string
	Runner            func(ctx context.Context, env *Env) (map[string]any, error)
	OriginUISessionID string
	OriginSessionID   string
	InterruptFn       func() error
	MaxAsyncChildren  int
	ProgressFn        func() (any, bool, error)
	CapacityError     string
	SlotKey           string
	TaskIndexes       []int
	TaskTranscripts   map[string]string
}

// Python exception class name of a runner failure
func asyncExceptionName(err error) string {
	var named interface{ PyTypeName() string }
	if errors.As(err, &named) {
		return named.PyTypeName()
	}
	return "RuntimeError"
}

// Dispatch under a backend retirement work reservation, rejected while the backend retires
func asyncDispatch(ctx context.Context, env *Env, d AsyncDispatch) (map[string]any, error) {
	admitted, release := asyncRetirement().Work()
	defer release()
	if !admitted {
		return map[string]any{"status": "rejected", "error": "backend is retiring; reconnect to continue"}, nil
	}
	return asyncDispatchAdmitted(ctx, env, d)
}

// Executor the dispatch submits to, replaced by tests pinning a pool so a unit stays queued
var asyncGetExecutorFn = asyncGetExecutor

// Shared dispatch core, the capacity check and record insert under one lock hold so the cap is never exceeded
func asyncDispatchAdmitted(ctx context.Context, env *Env, d AsyncDispatch) (map[string]any, error) {
	if _, err := workerLedger(); err != nil {
		return nil, err
	}
	label := ""
	classify := func(r map[string]any) string {
		if s := asyncStrOr(r["status"]); s != "" {
			return s
		}
		return "completed"
	}
	crash := singleCrash
	if d.IsBatch {
		label = " batch"
		classify = batchStatus
		crash = batchCrash
	}
	dispatchedAt := asyncTimeNow()
	m := map[string]any{"delegation_id": d.DelegationID, "goal": d.Goal}
	if d.IsBatch {
		m["goals"] = append([]string{}, d.Goals...)
	}
	m["context"] = nil
	if d.Context != nil {
		m["context"] = *d.Context
	}
	m["toolsets"] = nil
	if len(d.Toolsets) > 0 {
		m["toolsets"] = append([]string{}, d.Toolsets...)
	}
	m["role"] = d.Role
	m["model"] = nil
	if d.Model != nil {
		m["model"] = *d.Model
	}
	m["session_key"] = d.SessionKey
	m["origin_ui_session_id"] = d.OriginUISessionID
	m["origin_session_id"] = d.OriginSessionID
	m["parent_session_id"] = nil
	if d.ParentSessionID != nil {
		m["parent_session_id"] = *d.ParentSessionID
	}
	for k, v := range captureRoutingOrigin(env) {
		m[k] = v
	}
	m["status"] = "running"
	m["dispatched_at"] = dispatchedAt
	m["completed_at"] = nil
	if d.IsBatch {
		m["is_batch"] = true
	}
	m["slot_key"] = d.DelegationID
	if d.SlotKey != "" {
		m["slot_key"] = d.SlotKey
	}
	if len(d.TaskTranscripts) > 0 {
		tt := map[string]any{}
		for k, v := range d.TaskTranscripts {
			tt[k] = v
		}
		m["task_transcripts"] = tt
	}
	if d.TaskIndexes != nil {
		m["task_indexes"] = append([]int{}, d.TaskIndexes...)
	}
	m["_progress_token"] = nil
	m["_progress_ts"] = dispatchedAt
	m["_interrupted_at"] = nil
	rec := &asyncRecord{m: m, interruptFn: d.InterruptFn, progressFn: d.ProgressFn, ctx: ctx, env: env}
	asyncRecordsMu.Lock()
	for _, field := range asyncFencedFields {
		if v, ok := m[field].(string); ok && len(asyncRefusedBy[asyncSelector{field, v}]) > 0 {
			asyncRecordsMu.Unlock()
			return map[string]any{"status": "rejected", "error": asyncDelegationRejectedClosing}, nil
		}
	}
	slots := map[string]bool{}
	for _, r := range asyncRecordList() {
		if asyncActiveStates[r.status()] {
			key := asyncStrOr(r.m["slot_key"])
			if key == "" {
				key = asyncText(r.m["delegation_id"])
			}
			slots[key] = true
		}
	}
	if !slots[asyncText(m["slot_key"])] && len(slots) >= d.MaxAsyncChildren {
		asyncRecordsMu.Unlock()
		return map[string]any{"status": "rejected", "error": d.CapacityError}, nil
	}
	if _, exists := asyncRecords[d.DelegationID]; !exists {
		asyncRecordOrder = append(asyncRecordOrder, d.DelegationID)
	}
	asyncRecords[d.DelegationID] = rec
	liveUnits := 0
	for _, r := range asyncRecordList() {
		if asyncLiveStates[r.status()] {
			liveUnits++
		}
	}
	asyncRecordsMu.Unlock()
	if err := persistDispatch(env, m); err != nil {
		return nil, err
	}
	executor := asyncGetExecutorFn(max(d.MaxAsyncChildren, liveUnits))
	delegationID := d.DelegationID
	worker := func(wctx context.Context, wenv *Env) (any, error) {
		result := map[string]any{}
		status := "error"
		asyncRecordsMu.Lock()
		if r := asyncRecords[delegationID]; r != nil {
			r.m["_started"] = true
			r.m["_progress_ts"] = asyncTimeNow()
		}
		asyncRecordsMu.Unlock()
		defer func() { asyncFinalize(wenv, delegationID, result, nil, status) }()
		out, err := func() (out map[string]any, err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%v", p)
				}
			}()
			return d.Runner(wctx, wenv)
		}()
		if err != nil {
			asyncLog().Error(fmt.Sprintf("Async delegation%s %s crashed", label, delegationID), "error", err)
			result = crash(fmt.Sprintf("%s: %s", asyncExceptionName(err), err.Error()), asyncRound(asyncTimeNow()-dispatchedAt, 2))
			return nil, nil
		}
		if out == nil {
			out = map[string]any{}
		}
		result = out
		status = classify(out)
		return nil, nil
	}
	retire := asyncRetirement()
	retire.Acquire()
	run := PropagateContextToThread(ctx, env, worker)
	future, err := executor.Submit(context.WithoutCancel(ctx), func(context.Context) (any, error) { return run() })
	if err != nil {
		retire.Release()
		asyncRecordsMu.Lock()
		asyncDeleteRecordLocked(delegationID)
		asyncRecordsMu.Unlock()
		remote, rerr := workerLedger()
		if rerr != nil {
			return nil, rerr
		}
		if remote != nil {
			if _, uerr := remote.Unscheduled(delegationID); uerr != nil {
				return nil, uerr
			}
		} else if derr := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
			_, err := asyncExec(ctx, conn, "DELETE FROM async_delegations WHERE delegation_id=?", delegationID)
			return err
		}); derr != nil {
			return nil, derr
		}
		return map[string]any{"status": "rejected", "error": fmt.Sprintf("Failed to schedule async delegation%s: %s", label, err)}, nil
	}
	go func() {
		<-future.Done()
		retire.Release()
	}()
	if d.ProgressFn != nil {
		asyncEnsureStaleMonitor()
	}
	return map[string]any{"status": "dispatched", "delegation_id": delegationID}, nil
}

// Spawns one background child on the daemon executor and returns its handle immediately
func DispatchAsyncDelegationUnit(ctx context.Context, env *Env, d AsyncDispatch) (map[string]any, error) {
	d.DelegationID = newDelegationID()
	d.IsBatch, d.Goals, d.TaskIndexes, d.TaskTranscripts, d.SlotKey = false, nil, nil, nil, ""
	if d.MaxAsyncChildren == 0 {
		d.MaxAsyncChildren = asyncDefaultMaxChildren
	}
	d.CapacityError = fmt.Sprintf("Async delegation capacity reached (%d running). Wait for one to finish "+
		"(its result will re-enter the chat), or run this task synchronously (background=false). "+
		"Raise delegation.max_concurrent_children in config.yaml to allow more concurrent background subagents.", d.MaxAsyncChildren)
	handle, err := asyncDispatch(ctx, env, d)
	if err == nil && handle["status"] == "dispatched" {
		key := d.SessionKey
		if key == "" {
			key = "<cli>"
		}
		asyncLog().Info(fmt.Sprintf("Dispatched async delegation %s (session_key=%s): %s", d.DelegationID, key, pystrPrefix(d.Goal, 80)))
	}
	return handle, err
}

// One single background unit a caller outside pkg/tools dispatches
type AsyncDelegationRequest struct {
	Goal, Context     string
	Toolsets          []string
	Role              string
	Model             *string
	SessionKey        string
	ParentSessionID   *string
	Runner            func() *pyjson.Object
	OriginUISessionID string
	OriginSessionID   string
	MaxAsyncChildren  int
}

// Dispatches a single background unit, status dispatched with its delegation_id, else rejected or error with the reason
func DispatchAsyncDelegation(env *Env, req AsyncDelegationRequest) map[string]any {
	d := AsyncDispatch{
		Goal: req.Goal, Toolsets: req.Toolsets, Role: req.Role, Model: req.Model, SessionKey: req.SessionKey,
		ParentSessionID: req.ParentSessionID, OriginUISessionID: req.OriginUISessionID,
		OriginSessionID: req.OriginSessionID, MaxAsyncChildren: req.MaxAsyncChildren,
	}
	if req.Context != "" {
		c := req.Context
		d.Context = &c
	}
	if req.Runner != nil {
		runner := req.Runner
		d.Runner = func(context.Context, *Env) (map[string]any, error) {
			out := runner()
			if out == nil {
				return nil, nil
			}
			m, _ := asyncNative(out).(map[string]any)
			return m, nil
		}
	}
	handle, err := DispatchAsyncDelegationUnit(context.Background(), env, d)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	return handle
}

// Dispatches a fan out unit as one background unit with one completion event carrying per task results
func DispatchAsyncDelegationBatch(ctx context.Context, env *Env, d AsyncDispatch) (map[string]any, error) {
	if d.DelegationID == "" {
		d.DelegationID = newDelegationID()
	}
	if d.MaxAsyncChildren == 0 {
		d.MaxAsyncChildren = asyncDefaultMaxChildren
	}
	d.IsBatch = true
	if d.Goals == nil {
		d.Goals = []string{}
	}
	unitGoals := d.Goals
	if d.TaskIndexes != nil {
		unitGoals = make([]string, 0, len(d.TaskIndexes))
		for _, i := range d.TaskIndexes {
			if i < 0 {
				i += len(d.Goals)
			}
			if i < 0 || i >= len(d.Goals) {
				return nil, fmt.Errorf("IndexError: list index out of range")
			}
			unitGoals = append(unitGoals, d.Goals[i])
		}
	}
	n := len(unitGoals)
	if n == 1 {
		d.Goal = unitGoals[0]
	} else {
		parts := make([]string, n)
		for i, g := range unitGoals {
			parts[i] = pystrPrefix(g, 40)
		}
		d.Goal = fmt.Sprintf("%d parallel subagents: ", n) + strings.Join(parts, "; ")
	}
	d.CapacityError = fmt.Sprintf("Async delegation capacity reached (%d running). Wait for one to finish "+
		"(its result will re-enter the chat), or raise delegation.max_concurrent_children in "+
		"config.yaml to allow more concurrent background units.", d.MaxAsyncChildren)
	handle, err := asyncDispatch(ctx, env, d)
	if err == nil && handle["status"] == "dispatched" {
		key := d.SessionKey
		if key == "" {
			key = "<cli>"
		}
		asyncLog().Info(fmt.Sprintf("Dispatched async delegation batch %s (%d task(s), session_key=%s)", d.DelegationID, n, key))
	}
	return handle, err
}

// First n code points of s like a Python slice
func pystrPrefix(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Claims terminal delivery once, pushes the completion event, then marks the record with status
func asyncFinalize(env *Env, delegationID string, result map[string]any, resultFn func(snapshot map[string]any) map[string]any, status string) {
	asyncRecordsMu.Lock()
	rec := asyncRecords[delegationID]
	if rec == nil || !asyncActiveStates[rec.status()] {
		asyncRecordsMu.Unlock()
		return
	}
	rec.m["status"] = "finalizing"
	rec.m["completed_at"] = asyncTimeNow()
	rec.interruptFn = nil
	rec.progressFn = nil
	snapshot := asyncCopyMap(rec.m)
	asyncRecordsMu.Unlock()
	if resultFn != nil {
		result = resultFn(snapshot)
	}
	pushCompletionEvent(env, snapshot, result, status)
	asyncRecordsMu.Lock()
	if r := asyncRecords[delegationID]; r != nil {
		r.m["status"] = status
		r.ctx, r.env = nil, nil
	}
	pruneCompletedLocked()
	asyncRecordsMu.Unlock()
}

// Terminal durable write of a completion, replaced by tests standing in for a locked ledger
var asyncPersistCompletionFn = persistCompletion

// Pushes the async_delegation completion event onto the shared completion queue and its durable row
func pushCompletionEvent(env *Env, record, result map[string]any, status string) {
	result = asyncMap(RedactChildResult(env, result, nil, false))
	isBatch := pyrepr.Truthy(record["is_batch"])
	label := ""
	if isBatch {
		label = " batch"
	}
	now := asyncTimeNow()
	dispatchedAt, ok := asyncFloat(record["dispatched_at"])
	if !ok || dispatchedAt == 0 {
		dispatchedAt = now
	}
	completedAt, ok := asyncFloat(record["completed_at"])
	if !ok || completedAt == 0 {
		completedAt = now
	}
	evt := map[string]any{
		"type": "async_delegation", "delegation_id": record["delegation_id"],
		"session_key": asyncValueOr(record, "session_key", ""), "origin_ui_session_id": asyncValueOr(record, "origin_ui_session_id", ""),
		"origin_session_id": asyncValueOr(record, "origin_session_id", ""), "parent_session_id": record["parent_session_id"],
		"goal": asyncValueOr(record, "goal", ""), "context": record["context"], "toolsets": record["toolsets"], "role": record["role"],
		"status": status,
	}
	if isBatch {
		evt["goals"] = record["goals"]
		evt["model"] = record["model"]
		results := result["results"]
		if !pyrepr.Truthy(results) {
			results = []any{}
		}
		evt["is_batch"] = true
		evt["results"] = results
		evt["live_transcripts"] = result["live_transcripts"]
		evt["error"] = result["error"]
		evt["total_duration_seconds"] = result["total_duration_seconds"]
		if result["group"] != nil {
			evt["group"] = result["group"]
		}
	} else {
		evt["model"] = result["model"]
		if !pyrepr.Truthy(evt["model"]) {
			evt["model"] = record["model"]
		}
		evt["summary"] = result["summary"]
		evt["error"] = result["error"]
		if result["api_calls"] != nil {
			evt["api_calls"] = result["api_calls"]
		}
		if v, ok := result["duration_seconds"]; ok {
			evt["duration_seconds"] = v
		} else {
			evt["duration_seconds"] = asyncRound(completedAt-dispatchedAt, 2)
		}
		evt["exit_reason"] = result["exit_reason"]
	}
	evt["dispatched_at"] = dispatchedAt
	evt["completed_at"] = completedAt
	for _, k := range asyncRoutingKeys {
		if pyrepr.Truthy(record[k]) {
			evt[k] = record[k]
		}
	}
	for _, k := range asyncStallMetaKeys {
		if v, ok := result[k]; ok {
			evt[k] = v
		}
	}
	if err := asyncPersistCompletionFn(env, asyncCopyMap(evt), result); err != nil {
		asyncLog().Error(fmt.Sprintf("Async delegation%s %s: durable completion write failed; delivering in-memory "+
			"only (a restart may report this unit as unknown): %s", label, asyncText(record["delegation_id"]), err))
	}
	DefaultProcessRegistry().CompletionQueue.Put(evt)
}

// Pushes the completion event and its durable row, exported for delegation lifecycle callers
func PushCompletionEvent(env *Env, record, result map[string]any, status string) {
	pushCompletionEvent(env, record, result, status)
}

func asyncValueOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Surfaces one failed child of a still running batch now, in process only so it honours the redaction opt out
func PushTaskFailureNotice(env *Env, delegationID string, entry map[string]any, nTasks int) {
	entry = asyncMap(RedactChildResult(env, entry, nil, false))
	asyncRecordsMu.Lock()
	rec := asyncRecords[delegationID]
	if rec == nil || !asyncActiveStates[rec.status()] {
		asyncRecordsMu.Unlock()
		return
	}
	snap := asyncCopyMap(rec.m)
	asyncRecordsMu.Unlock()
	now := asyncTimeNow()
	dispatchedAt := snap["dispatched_at"]
	if !pyrepr.Truthy(dispatchedAt) {
		dispatchedAt = now
	}
	evt := map[string]any{
		"type": "async_delegation", "task_failure_notice": true, "is_batch": true, "n_tasks": nTasks,
		"delegation_id": delegationID, "results": []any{entry},
		"session_key": asyncValueOr(snap, "session_key", ""), "origin_ui_session_id": asyncValueOr(snap, "origin_ui_session_id", ""),
		"origin_session_id": asyncValueOr(snap, "origin_session_id", ""), "parent_session_id": snap["parent_session_id"],
		"goal": asyncValueOr(snap, "goal", ""), "goals": snap["goals"], "context": snap["context"],
		"toolsets": snap["toolsets"], "role": snap["role"], "model": snap["model"],
		"status": "running", "dispatched_at": dispatchedAt, "completed_at": now,
	}
	for _, k := range asyncRoutingKeys {
		if pyrepr.Truthy(snap[k]) {
			evt[k] = snap[k]
		}
	}
	DefaultProcessRegistry().CompletionQueue.Put(evt)
}

// Starts the one stale monitor goroutine unless it is already running
func ensureStaleMonitor() {
	asyncMonitorMu.Lock()
	defer asyncMonitorMu.Unlock()
	if asyncMonitorDone != nil {
		select {
		case <-asyncMonitorDone:
		default:
			return
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	asyncMonitorStop, asyncMonitorDone = stop, done
	go func() {
		defer close(done)
		staleMonitorLoop(stop)
	}()
}

type asyncStall struct {
	id      string
	quiet   float64
	inTool  bool
	limitAt float64
}

// One monitor pass over the records under the records lock, newly stalling ids, ids past grace and whether any remain
func sweepStaleLocked(now float64) (stalled []asyncStall, expired []string, anyMonitorable bool) {
	for _, r := range asyncRecordList() {
		status := r.status()
		if status == "stalling" {
			anyMonitorable = true
			interruptedAt, ok := asyncFloat(r.m["_interrupted_at"])
			if !ok || interruptedAt == 0 {
				interruptedAt = now
			}
			if now-interruptedAt >= asyncStallGraceSeconds {
				expired = append(expired, asyncText(r.m["delegation_id"]))
			}
			continue
		}
		if status != "running" || r.progressFn == nil {
			continue
		}
		anyMonitorable = true
		if !pyrepr.Truthy(r.m["_started"]) {
			continue
		}
		token, inTool, err := asyncSampleProgress(r.progressFn)
		if err != nil {
			token, inTool = r.m["_progress_token"], false
		}
		if !reflect.DeepEqual(token, r.m["_progress_token"]) {
			r.m["_progress_token"] = token
			r.m["_progress_ts"] = now
			continue
		}
		ts, ok := asyncFloat(r.m["_progress_ts"])
		if !ok || ts == 0 {
			ts = now
		}
		quietFor := now - ts
		limit := asyncStaleIdleSeconds
		if inTool {
			limit = asyncStaleInToolSeconds
		}
		if quietFor >= limit {
			r.m["status"] = "stalling"
			r.m["_interrupted_at"] = now
			r.m["_stall_quiet_seconds"] = asyncRound(quietFor, 2)
			r.m["_stall_threshold_seconds"] = limit
			r.m["_stall_in_tool"] = inTool
			stalled = append(stalled, asyncStall{id: asyncText(r.m["delegation_id"]), quiet: quietFor, inTool: inTool})
		}
	}
	return stalled, expired, anyMonitorable
}

// Samples a progress function, a panic counting as an unreadable child
func asyncSampleProgress(fn func() (any, bool, error)) (token any, inTool bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	return fn()
}

// Calls an interrupt function, true on success and a debug log otherwise
func callInterrupt(fn func() error, msg string) bool {
	if fn == nil {
		return false
	}
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%v", p)
			}
		}()
		return fn()
	}()
	if err != nil {
		asyncLog().Debug(fmt.Sprintf("%s: %s", msg, err))
		return false
	}
	return true
}

// Interrupts frozen delegations and force finalizes a stalling one that never returns within the grace window
func staleMonitorLoop(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(asyncStaleCheckInterval):
		}
		now := asyncTimeNow()
		asyncRecordsMu.Lock()
		stalled, expired, anyMonitorable := sweepStaleLocked(now)
		asyncRecordsMu.Unlock()
		for _, s := range stalled {
			asyncLog().Warn(fmt.Sprintf("Async delegation %s made no progress for %.0fs (in_tool=%s) — interrupting; grace window %.0fs",
				s.id, s.quiet, pyrepr.Str(s.inTool), asyncStallGraceSeconds))
			asyncRecordsMu.Lock()
			var fn func() error
			if r := asyncRecords[s.id]; r != nil {
				fn = r.interruptFn
			}
			asyncRecordsMu.Unlock()
			callInterrupt(fn, fmt.Sprintf("Async delegation %s stall interrupt failed", s.id))
		}
		for _, id := range expired {
			asyncRecordsMu.Lock()
			var env *Env
			if r := asyncRecords[id]; r != nil {
				env = r.env
			}
			asyncRecordsMu.Unlock()
			did := id
			asyncFinalize(env, did, nil, func(rec map[string]any) map[string]any { return stalledResult(did, rec) }, "stalled")
		}
		if !anyMonitorable {
			return
		}
	}
}

// User facing wording of a force finalized stall naming the task, its silence and what to do
func stalledErrorText(rec map[string]any) string {
	goal := strings.Join(strings.Fields(asyncStrOr(rec["goal"])), " ")
	label := "The background task"
	if goal != "" {
		label = fmt.Sprintf("Background task \"%s\"", pystrPrefix(goal, 120))
	}
	quiet, _ := asyncFloat(rec["_stall_quiet_seconds"])
	silence := ""
	if quiet >= 60 {
		silence = fmt.Sprintf(" after %d min of no progress", int(math.RoundToEven(quiet/60)))
	}
	return label + " stopped responding" + silence + " and was cancelled. Nothing else was affected; " +
		"ask me to run it again if you still need it."
}

// Synthetic terminal result of a stalling delegation whose runner never returned
func stalledResult(delegationID string, rec map[string]any) map[string]any {
	completedAt, ok := asyncFloat(rec["completed_at"])
	if !ok || completedAt == 0 {
		completedAt = asyncTimeNow()
	}
	dispatchedAt, ok := asyncFloat(rec["dispatched_at"])
	if !ok || dispatchedAt == 0 {
		dispatchedAt = completedAt
	}
	duration := asyncRound(completedAt-dispatchedAt, 2)
	errText := stalledErrorText(rec)
	asyncLog().Error(fmt.Sprintf("Async delegation %s force-finalized as stalled after %.0fs", delegationID, duration))
	var phase any
	if v, ok := rec["_stall_in_tool"]; ok && v != nil {
		phase = "idle"
		if pyrepr.Truthy(v) {
			phase = "in_tool"
		}
	}
	meta := map[string]any{
		"stalled_after_quiet_seconds": rec["_stall_quiet_seconds"], "stall_threshold_seconds": rec["_stall_threshold_seconds"],
		"stall_phase": phase, "stall_grace_seconds": asyncStallGraceSeconds,
	}
	var out map[string]any
	if pyrepr.Truthy(rec["is_batch"]) {
		out = batchCrash(errText, duration)
	} else {
		out = singleCrash(errText, duration)
		out["status"] = "stalled"
		out["exit_reason"] = "stalled"
	}
	for k, v := range meta {
		out[k] = v
	}
	return out
}

// Per child activity parsed from a progress token, nil for a token that is not a sequence
func childrenActivityFromToken(token any, now float64) []any {
	rv := reflect.ValueOf(token)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil
	}
	out := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		part := reflect.ValueOf(rv.Index(i).Interface())
		if !part.IsValid() || (part.Kind() != reflect.Slice && part.Kind() != reflect.Array) || part.Len() < 2 {
			out = append(out, nil)
			continue
		}
		entry := map[string]any{"api_calls": part.Index(0).Interface(), "current_tool": part.Index(1).Interface()}
		if part.Len() >= 3 {
			third := part.Index(2).Interface()
			if _, isBool := third.(bool); !isBool {
				if ts, ok := asyncFloat(third); ok {
					entry["seconds_since_activity"] = asyncRound(math.Max(0, now-ts), 1)
				}
			}
		}
		out = append(out, entry)
	}
	return out
}

// Snapshot of running and recently completed delegations with live progress fields and stall context
func ListAsyncDelegations() []map[string]any {
	now := asyncTimeNow()
	samplers := map[string]func() (any, bool, error){}
	asyncRecordsMu.Lock()
	var items []map[string]any
	for _, r := range asyncRecordList() {
		item := map[string]any{}
		for k, v := range r.m {
			if !strings.HasPrefix(k, "_") {
				item[k] = v
			}
		}
		status := r.status()
		if asyncActiveStates[status] {
			if ts, ok := asyncFloat(r.m["_progress_ts"]); ok && ts != 0 {
				item["seconds_since_progress"] = asyncRound(now-ts, 1)
			}
			if r.progressFn != nil {
				samplers[asyncText(r.m["delegation_id"])] = r.progressFn
			}
		}
		if status == "stalling" || status == "stalled" {
			for _, pair := range asyncStallFieldMap {
				if v, ok := r.m[pair[0]]; ok && v != nil {
					item[pair[1]] = v
				}
			}
		}
		items = append(items, item)
	}
	asyncRecordsMu.Unlock()
	for _, item := range items {
		fn := samplers[asyncText(item["delegation_id"])]
		if fn == nil {
			continue
		}
		token, inTool, err := asyncSampleProgress(fn)
		if err != nil {
			continue
		}
		if activity := childrenActivityFromToken(token, now); activity != nil {
			item["children_activity"] = activity
		}
		item["in_tool"] = inTool
	}
	if items == nil {
		items = []map[string]any{}
	}
	return items
}

// Interrupts each record and logs once, returning how many interrupts succeeded
func interruptRecords(targets []*asyncRecord, caller, reason, msg string) int {
	count := 0
	for _, r := range targets {
		asyncRecordsMu.Lock()
		fn, id := r.interruptFn, asyncText(r.m["delegation_id"])
		asyncRecordsMu.Unlock()
		if callInterrupt(fn, fmt.Sprintf("%s: %s interrupt failed", caller, id)) {
			count++
		}
	}
	if count > 0 {
		asyncLog().Info(fmt.Sprintf(msg, count, reason))
	}
	return count
}

// Signals every running async delegation to stop, the child still emits its completion event
func InterruptAllAsyncDelegations(reason string) int {
	asyncRecordsMu.Lock()
	var targets []*asyncRecord
	for _, r := range asyncRecordList() {
		if asyncActiveStates[r.status()] {
			targets = append(targets, r)
		}
	}
	asyncRecordsMu.Unlock()
	return interruptRecords(targets, "interrupt_all", reason, "Interrupted %d async delegation(s) (%s)")
}

// Signals the running async delegations one ending session owns to stop
func InterruptAsyncDelegationsForSession(sessionKey, originUISessionID, parentSessionID, reason string) int {
	targets := sessionRecords(asyncActiveStates, sessionKey, originUISessionID, parentSessionID)
	return interruptRecords(targets, "interrupt_for_session", reason, "Interrupted %d async delegation(s) for ending session (%s)")
}

// Refuses new dispatches for one closing session until its refusal is lifted
func RefuseAsyncDelegationsForSession(token any, sessionKey string, parentSessionIDs []string) {
	fence := map[asyncSelector]bool{}
	if sessionKey != "" {
		fence[asyncSelector{"session_key", sessionKey}] = true
	}
	for _, p := range parentSessionIDs {
		if p != "" {
			fence[asyncSelector{"parent_session_id", p}] = true
		}
	}
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	unfenceLocked(token)
	asyncRefused[token] = fence
	for sel := range fence {
		if asyncRefusedBy[sel] == nil {
			asyncRefusedBy[sel] = map[any]bool{}
		}
		asyncRefusedBy[sel][token] = true
	}
}

// Lifts one session's dispatch refusal
func AllowAsyncDelegationsForSession(token any) {
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	unfenceLocked(token)
}

// Drops a token's refusal, caller holds the records lock
func unfenceLocked(token any) {
	fence := asyncRefused[token]
	delete(asyncRefused, token)
	for sel := range fence {
		tokens := asyncRefusedBy[sel]
		delete(tokens, token)
		if len(tokens) == 0 {
			delete(asyncRefusedBy, sel)
		}
	}
}

type asyncDelegationLedger struct{}

func (asyncDelegationLedger) RestoreUndelivered(q *EventQueue) (int, error) {
	return RestoreUndeliveredCompletions(nil, q)
}
func (asyncDelegationLedger) CopyID(evt map[string]any) string { return CompletionCopyID(evt) }
func (asyncDelegationLedger) MarkCopyHeld(id string)           { MarkCopyHeld(id) }
func (asyncDelegationLedger) UnmarkCopyHeld(id string)         { UnmarkCopyHeld(id) }

func init() { AsyncDelegationLedger = asyncDelegationLedger{} }

// Installs one in memory delegation record for a case of another package and hands back the removal
func SetAsyncDelegationRecordForTests(delegationID string, record map[string]any) func() {
	asyncRecordsMu.Lock()
	asyncDeleteRecordLocked(delegationID)
	asyncRecords[delegationID] = &asyncRecord{m: record}
	asyncRecordOrder = append(asyncRecordOrder, delegationID)
	asyncRecordsMu.Unlock()
	return func() {
		asyncRecordsMu.Lock()
		asyncDeleteRecordLocked(delegationID)
		asyncRecordsMu.Unlock()
	}
}

// Gives a delegation record a case of another package installed the stop signal its child would hold
func SetAsyncDelegationInterruptForTests(delegationID string, interrupt func() error) {
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	if r := asyncRecords[delegationID]; r != nil {
		r.interruptFn = interrupt
	}
}

// Clears every registry, fence, offer and hold and tears down the executor and stale monitor
func ResetAsyncDelegationForTests() {
	asyncExecutorMu.Lock()
	if asyncExecutor != nil {
		asyncExecutor.Shutdown(false, false)
	}
	asyncExecutor, asyncExecutorMax = nil, 0
	asyncExecutorMu.Unlock()
	asyncMonitorMu.Lock()
	stop, done := asyncMonitorStop, asyncMonitorDone
	asyncMonitorStop, asyncMonitorDone = nil, nil
	asyncMonitorMu.Unlock()
	if stop != nil {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	asyncRecordsMu.Lock()
	asyncRecords, asyncRecordOrder = map[string]*asyncRecord{}, nil
	asyncRefused, asyncRefusedBy = map[any]map[asyncSelector]bool{}, map[asyncSelector]map[any]bool{}
	asyncRecordsMu.Unlock()
	asyncOrphanMu.Lock()
	asyncOffered, asyncStartupReplay = map[[2]string]bool{}, map[string]bool{}
	asyncLastOrphanTick, asyncReofferedAt = map[string]float64{}, map[[2]string]float64{}
	asyncOrphanMu.Unlock()
	asyncHeldMu.Lock()
	asyncHeldCopies = map[string]int{}
	asyncHeldMu.Unlock()
}

//go:build !icebath_core && !icebath_noextras

package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Closed once this worker's turns ended, stopping the submitter watch
var kanbanTurnsDone = make(chan struct{})

// Item of the decoded kanban context, KeyError when absent
func kanbanContextItem(context *jsonutil.Object, key string) (any, error) {
	v, ok := context.Get(key)
	if !ok {
		return nil, &PyError{Kind: "KeyError", Msg: anyutil.Quote(key)}
	}
	return v, nil
}

// Decoded kanban context of a policy's kanban_json text, nil for a chat turn, refusing a policy whose source is not kanban
func decodeKanbanWorkerContext(text, source any) (*jsonutil.Object, error) {
	raw, _ := text.(string)
	if raw == "" {
		raw = "null"
	}
	decoded, err := jsonutil.Decode(raw)
	if err != nil || decoded == nil {
		return nil, err
	}
	if !anyutil.Equal(source, "kanban") {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_kanban_policy"}
	}
	kctx, ok := decoded.(*jsonutil.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("the kanban worker context is %s, expected a mapping", errchain.Kind(decoded))}
	}
	return kctx, nil
}

// Worker protocol env a kanban attempt's worker child starts with, nil for a chat turn; the claim lifetime comes from the board's claimed event
func kanbanWorkerSpawnEnv(ctx context.Context, policy *LocalSessionPolicy) (map[string]string, error) {
	if policy == nil || policy.KanbanJSON == nil {
		return nil, nil
	}
	kctx, err := decodeKanbanWorkerContext(*policy.KanbanJSON, policy.Source)
	if err != nil || kctx == nil {
		return nil, err
	}
	item := func(key string) any {
		if err != nil {
			return nil
		}
		var v any
		v, err = kanbanContextItem(kctx, key)
		return v
	}
	text := func(key string) string { return anyutil.String(item(key)) }
	env := map[string]string{"ICEBATH_KANBAN_TASK": text("task_id"), "ICEBATH_KANBAN_BOARD": text("board"),
		"ICEBATH_KANBAN_DB": text("db"), "ICEBATH_KANBAN_RUN_ID": text("run_id"),
		"ICEBATH_KANBAN_CLAIM_LOCK": text("claim_lock"), "ICEBATH_KANBAN_WORKSPACE": text("workspace"),
		"ICEBATH_KANBAN_WORKSPACES_ROOT": text("workspaces_root"), "ICEBATH_PROFILE": text("profile"),
		"ICEBATH_SESSION_SOURCE": "kanban"}
	for _, pair := range [][2]string{{"ICEBATH_KANBAN_BRANCH", "branch"}, {"ICEBATH_TENANT", "tenant"}} {
		if v := item(pair[1]); anyutil.IsTrue(v) {
			env[pair[0]] = anyutil.String(v)
		}
	}
	if anyutil.IsTrue(item("goal_mode")) {
		env["ICEBATH_KANBAN_GOAL_MODE"] = "1"
	}
	maxRuntime := item("max_runtime_seconds")
	runID, _ := item("run_id").(int64)
	if err != nil {
		return nil, err
	}
	var budget *int64
	if n, isInt := maxRuntime.(int64); isInt {
		budget = &n
	}
	for key, value := range kanban.WorkerTerminalTimeouts(ctx, budget) {
		env[key] = value
	}
	ttl, known, err := kanbanClaimTTL(ctx, env["ICEBATH_KANBAN_DB"], env["ICEBATH_KANBAN_TASK"], runID)
	if err != nil {
		return nil, err
	}
	if known {
		env["ICEBATH_KANBAN_CLAIM_TTL_SECONDS"] = strconv.FormatInt(ttl, 10)
	}
	return env, nil
}

// Seconds the run's claim was granted for, from its latest claimed event, false when the event carries no expiry
func kanbanClaimTTL(ctx context.Context, dbPath, taskID string, runID int64) (int64, bool, error) {
	var claimedPayload sql.NullString
	var claimedAt any
	found := false
	err := kanban.ConnectClosing(ctx, dbPath, nil, func(conn state.SQLiteConn) error {
		row := conn.QueryRowContext(ctx, "SELECT payload, created_at FROM task_events WHERE task_id=? AND run_id=? AND kind='claimed' ORDER BY id DESC LIMIT 1", taskID, runID)
		switch scanErr := row.Scan(&claimedPayload, &claimedAt); scanErr {
		case nil:
			found = true
		case sql.ErrNoRows:
		default:
			return scanErr
		}
		return nil
	})
	if err != nil || !found {
		return 0, false, err
	}
	decoded, err := jsonutil.Decode(claimedPayload.String)
	if err != nil {
		return 0, false, err
	}
	obj, ok := decoded.(*jsonutil.Object)
	if !ok {
		return 0, false, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("the kanban claimed event is %s, expected a mapping", errchain.Kind(decoded))}
	}
	expires, isInt := obj.ValueOr("expires", nil).(int64)
	if !isInt || !anyutil.IsTrue(claimedAt) {
		return 0, false, nil
	}
	created, err := claimInt(claimedAt)
	if err != nil {
		return 0, false, err
	}
	return max(1, expires-created), true, nil
}

// Binds the adopted worker to its Kanban claim before tool discovery, refusing a stale claim and following the submitter; its worker protocol env came with its spawn
func bindKanbanWorkerContext(ctx context.Context, frame *jsonutil.Object) error {
	policy, _ := frame.ValueOr("policy", nil).(*jsonutil.Object)
	kctx, err := decodeKanbanWorkerContext(policy.ValueOr("kanban_json", nil), policy.ValueOr("source", nil))
	if err != nil || kctx == nil {
		return err
	}
	item := func(key string) any {
		if err != nil {
			return nil
		}
		var v any
		v, err = kanbanContextItem(kctx, key)
		return v
	}
	dbPath, taskID, claimLock := anyutil.String(item("db")), anyutil.String(item("task_id")), item("claim_lock")
	runID, _ := item("run_id").(int64)
	workspace := anyutil.String(item("workspace"))
	if err != nil {
		return err
	}
	err = kanban.ConnectClosing(ctx, dbPath, nil, func(conn state.SQLiteConn) error {
		return kanban.WriteTxn(ctx, conn, false, func() error {
			task, err := kanban.GetTask(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if task == nil || task.Status != "running" || task.CurrentRunID == nil || *task.CurrentRunID != runID ||
				task.ClaimLock == nil || !anyutil.Equal(*task.ClaimLock, claimLock) {
				return runtimeRefusal("stale_kanban_claim")
			}
			fingerprint := kanban.UnverifiedWorkerFingerprint
			if fp := kanban.ProcessFingerprint(os.Getpid()); fp != nil && *fp != "" {
				fingerprint = *fp
			}
			run := runID
			return kanban.AppendTaskEvent(ctx, conn, task.ID, "worker_bound", map[string]any{"pid": int64(os.Getpid()),
				"claim_lock": claimLock, "started_at": fingerprint}, &run)
		})
	})
	if err != nil {
		return err
	}
	watchSubmitter(dbPath, runID)
	return os.Chdir(workspace)
}

// int() of a task event creation time
func claimInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	case []byte:
		return claimInt(string(n))
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, nil
		}
		return 0, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("the kanban claim number %v is not a whole number", n)}
	}
	return 0, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("the kanban claim number is %s, expected a whole number", errchain.Kind(v))}
}

// Ends this attempt hard once the dispatcher's spawned worker for the run is gone, re-reading the run row each second
func watchSubmitter(dbPath string, runID int64) {
	go func() {
		resolved, err := filepath.Abs(dbPath)
		if err != nil {
			resolved = dbPath
		}
		conn, err := sql.Open("sqlite", "file:"+resolved+"?mode=ro")
		if err != nil {
			return
		}
		defer conn.Close()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-kanbanTurnsDone:
				return
			case <-ticker.C:
			}
			var pid sql.NullInt64
			var started any
			if err := conn.QueryRow("SELECT worker_pid, worker_started_at FROM task_runs WHERE id=?", runID).Scan(&pid, &started); err != nil {
				continue
			}
			if pid.Valid && pid.Int64 != 0 && int(pid.Int64) != os.Getpid() && !kanban.WorkerAlive(pid.Int64, started) {
				os.Exit(1)
			}
		}
	}()
}

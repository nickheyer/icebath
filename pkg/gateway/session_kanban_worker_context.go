package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

// Closed once this worker's turns ended, stopping the submitter watch
var kanbanTurnsDone = make(chan struct{})

// Item of the decoded kanban context, KeyError when absent
func kanbanContextItem(context *pyjson.Object, key string) (any, error) {
	v, ok := context.Get(key)
	if !ok {
		return nil, &PyError{Kind: "KeyError", Msg: pyrepr.Repr(key)}
	}
	return v, nil
}

// Binds the adopted worker to its Kanban claim before tool discovery, refusing a stale claim and following the submitter
func BindWorkerContext(ctx context.Context, frame *pyjson.Object) error {
	policy, _ := frame.GetOr("policy", nil).(*pyjson.Object)
	text, _ := policy.GetOr("kanban_json", nil).(string)
	if text == "" {
		text = "null"
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return err
	}
	if decoded == nil {
		return nil
	}
	if !pyjson.Equal(policy.GetOr("source", nil), "kanban") {
		return &PyError{Kind: "ValueError", Msg: "invalid_kanban_policy"}
	}
	kctx, ok := decoded.(*pyjson.Object)
	if !ok {
		return &PyError{Kind: "TypeError", Msg: fmt.Sprintf("%s indices must be integers or slices, not str", pyTypeName(decoded))}
	}
	item := func(key string) any {
		if err != nil {
			return nil
		}
		var v any
		v, err = kanbanContextItem(kctx, key)
		return v
	}
	text2 := func(key string) string { return pyrepr.Str(item(key)) }
	env := map[string]string{"ICEBATH_KANBAN_TASK": text2("task_id"), "ICEBATH_KANBAN_BOARD": text2("board"),
		"ICEBATH_KANBAN_DB": text2("db"), "ICEBATH_KANBAN_RUN_ID": text2("run_id"),
		"ICEBATH_KANBAN_CLAIM_LOCK": text2("claim_lock"), "ICEBATH_KANBAN_WORKSPACE": text2("workspace"),
		"ICEBATH_KANBAN_WORKSPACES_ROOT": text2("workspaces_root"), "ICEBATH_PROFILE": text2("profile"),
		"ICEBATH_SESSION_SOURCE": "kanban"}
	for _, pair := range [][2]string{{"ICEBATH_KANBAN_BRANCH", "branch"}, {"ICEBATH_TENANT", "tenant"}} {
		if v := item(pair[1]); pyrepr.Truthy(v) {
			env[pair[0]] = pyrepr.Str(v)
		}
	}
	if pyrepr.Truthy(item("goal_mode")) {
		env["ICEBATH_KANBAN_GOAL_MODE"] = "1"
	}
	maxRuntime := item("max_runtime_seconds")
	if err != nil {
		return err
	}
	var budget *int64
	if n, isInt := maxRuntime.(int64); isInt {
		budget = &n
	}
	for _, key := range []string{"TERMINAL_TIMEOUT", "TERMINAL_MAX_FOREGROUND_TIMEOUT"} {
		var current *string
		if v, has := os.LookupEnv(key); has {
			current = &v
		}
		if value := kanban.WorkerTerminalTimeoutEnv(budget, current); value != nil && *value != "" {
			env[key] = *value
		}
	}
	dbPath, taskID, claimLock := text2("db"), text2("task_id"), item("claim_lock")
	runID, _ := item("run_id").(int64)
	workspace := text2("workspace")
	if err != nil {
		return err
	}
	var claimedPayload, claimedAt any
	err = kanban.ConnectClosing(ctx, dbPath, nil, func(conn state.SQLiteConn) error {
		return kanban.WriteTxn(ctx, conn, false, func() error {
			task, err := kanban.GetTask(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if task == nil || task.Status != "running" || task.CurrentRunID == nil || *task.CurrentRunID != runID ||
				task.ClaimLock == nil || !pyjson.Equal(*task.ClaimLock, claimLock) {
				return runtimeRefusal("stale_kanban_claim")
			}
			fingerprint := kanban.UnverifiedWorkerFingerprint
			if fp := kanban.ProcessFingerprint(os.Getpid()); fp != nil && *fp != "" {
				fingerprint = *fp
			}
			run := runID
			if err := kanban.AppendTaskEvent(ctx, conn, task.ID, "worker_bound", map[string]any{"pid": int64(os.Getpid()),
				"claim_lock": claimLock, "started_at": fingerprint}, &run); err != nil {
				return err
			}
			row := conn.QueryRowContext(ctx, "SELECT payload, created_at FROM task_events WHERE task_id=? AND run_id=? AND kind='claimed' ORDER BY id DESC LIMIT 1", task.ID, runID)
			var payload sql.NullString
			var created any
			switch scanErr := row.Scan(&payload, &created); scanErr {
			case nil:
				claimedPayload, claimedAt = payload, created
			case sql.ErrNoRows:
			default:
				return scanErr
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if claimedPayload != nil {
		text := claimedPayload.(sql.NullString)
		decoded, err := pyjson.Loads(text.String)
		if err != nil {
			return err
		}
		obj, ok := decoded.(*pyjson.Object)
		if !ok {
			return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(decoded))}
		}
		expires, isInt := obj.GetOr("expires", nil).(int64)
		if isInt && pyrepr.Truthy(claimedAt) {
			created, err := claimInt(claimedAt)
			if err != nil {
				return err
			}
			env["ICEBATH_KANBAN_CLAIM_TTL_SECONDS"] = strconv.FormatInt(max(1, expires-created), 10)
		}
	}
	watchSubmitter(dbPath, runID)
	for k, v := range env {
		os.Setenv(k, v)
	}
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
		return 0, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(n))}
	}
	return 0, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))}
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

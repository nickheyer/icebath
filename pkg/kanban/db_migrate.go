package kanban

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

type columnSpec struct{ name, ddl string }

var baseTaskColumns = []columnSpec{
	{"body", "body TEXT"},
	{"assignee", "assignee TEXT"},
	{"priority", "priority INTEGER DEFAULT 0"},
	{"created_by", "created_by TEXT"},
	{"started_at", "started_at INTEGER"},
	{"completed_at", "completed_at INTEGER"},
	{"workspace_kind", "workspace_kind TEXT NOT NULL DEFAULT 'scratch'"},
	{"workspace_path", "workspace_path TEXT"},
	{"claim_lock", "claim_lock TEXT"},
	{"claim_expires", "claim_expires INTEGER"},
}

var earlyTaskColumns = []columnSpec{
	{"tenant", "tenant TEXT"},
	{"result", "result TEXT"},
	{"branch_name", "branch_name TEXT"},
	{"project_id", "project_id TEXT"},
	{"idempotency_key", "idempotency_key TEXT"},
}

var renamedTaskColumns = []struct{ name, ddl, legacy, copySQL string }{
	{"consecutive_failures", "consecutive_failures INTEGER NOT NULL DEFAULT 0",
		"spawn_failures", "UPDATE tasks SET consecutive_failures = COALESCE(spawn_failures, 0)"},
	{"worker_pid", "worker_pid INTEGER", "", ""},
	{"last_failure_error", "last_failure_error TEXT",
		"last_spawn_error", "UPDATE tasks SET last_failure_error = last_spawn_error"},
}

var laterTaskColumns = []columnSpec{
	{"max_runtime_seconds", "max_runtime_seconds INTEGER"},
	{"last_heartbeat_at", "last_heartbeat_at INTEGER"},
	{"current_run_id", "current_run_id INTEGER"},
	{"workflow_template_id", "workflow_template_id TEXT"},
	{"current_step_key", "current_step_key TEXT"},
	{"skills", "skills TEXT"},
	{"max_retries", "max_retries INTEGER"},
	{"model_override", "model_override TEXT"},
	{"provider_override", "provider_override TEXT"},
	{"reasoning_effort", "reasoning_effort TEXT"},
	{"goal_mode", "goal_mode INTEGER NOT NULL DEFAULT 0"},
	{"goal_max_turns", "goal_max_turns INTEGER"},
	{"completion_contract", "completion_contract TEXT"},
	{"session_id", "session_id TEXT"},
	{"block_kind", "block_kind TEXT"},
	{"block_recurrences", "block_recurrences INTEGER NOT NULL DEFAULT 0"},
	{"worker_started_at", "worker_started_at INTEGER"},
}

var notifySubColumns = []columnSpec{
	{"last_ping_event_id", "last_ping_event_id INTEGER NOT NULL DEFAULT 0"},
	{"notifier_profile", "notifier_profile TEXT"},
	{"delivery_mode", "delivery_mode TEXT NOT NULL DEFAULT 'notify'"},
	{"chat_type", "chat_type TEXT"},
	{"user_id_alt", "user_id_alt TEXT"},
	{"delivery_metadata", "delivery_metadata TEXT"},
}

var taskRunColumns = []columnSpec{{"worker_started_at", "worker_started_at INTEGER"}}

// Column names of a table
func columnNames(ctx context.Context, conn state.SQLiteConn, table string) (map[string]bool, error) {
	rows, err := QueryRows(ctx, conn, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if s, ok := r.Get("name").(string); ok {
			out[s] = true
		}
	}
	return out, nil
}

// Reports whether a table exists
func tableExists(ctx context.Context, conn state.SQLiteConn, table string) (bool, error) {
	row, err := QueryOne(ctx, conn, fmt.Sprintf("SELECT name FROM sqlite_master WHERE type='table' AND name='%s'", table))
	return row != nil, err
}

// Adds every missing column from a spec list
func addMissingColumns(ctx context.Context, conn state.SQLiteConn, table string, have map[string]bool, specs []columnSpec) error {
	for _, c := range specs {
		if !have[c.name] {
			if _, err := state.AddColumnIfMissing(ctx, conn, table, c.name, c.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// Adds columns introduced after v1 to legacy databases, renames legacy event kinds and rebuilds drifted tables
func migrateAddOptionalColumns(ctx context.Context, conn state.SQLiteConn) error {
	cols, err := columnNames(ctx, conn, "tasks")
	if err != nil {
		return err
	}
	if err := addMissingColumns(ctx, conn, "tasks", cols, append(append([]columnSpec{}, baseTaskColumns...), earlyTaskColumns...)); err != nil {
		return err
	}
	if cols, err = columnNames(ctx, conn, "tasks"); err != nil {
		return err
	}
	for _, c := range renamedTaskColumns {
		if cols[c.name] {
			continue
		}
		added, err := state.AddColumnIfMissing(ctx, conn, "tasks", c.name, c.ddl)
		if err != nil {
			return err
		}
		if added && c.legacy != "" && cols[c.legacy] {
			if _, err := conn.ExecContext(ctx, c.copySQL); err != nil {
				return err
			}
		}
	}
	if err := addMissingColumns(ctx, conn, "tasks", cols, laterTaskColumns); err != nil {
		return err
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS idx_tasks_assignee_status ON tasks(assignee, status)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_tenant ON tasks(tenant)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_idempotency ON tasks(idempotency_key)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_session_id ON tasks(session_id)",
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	eventCols, err := columnNames(ctx, conn, "task_events")
	if err != nil {
		return err
	}
	if err := addMissingColumns(ctx, conn, "task_events", eventCols, []columnSpec{{"run_id", "run_id INTEGER"}}); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_events_run ON task_events(run_id, id)"); err != nil {
		return err
	}
	if ok, err := tableExists(ctx, conn, "kanban_notify_subs"); err != nil {
		return err
	} else if ok {
		notifyCols, err := columnNames(ctx, conn, "kanban_notify_subs")
		if err != nil {
			return err
		}
		for _, c := range notifySubColumns {
			if notifyCols[c.name] {
				continue
			}
			if _, err := state.AddColumnIfMissing(ctx, conn, "kanban_notify_subs", c.name, c.ddl); err != nil {
				return err
			}
			if c.name == "delivery_mode" {
				if _, err := conn.ExecContext(ctx, "UPDATE kanban_notify_subs SET delivery_mode = 'notify+wake' WHERE platform != 'tui'"); err != nil {
					return err
				}
			}
		}
	}
	if ok, err := tableExists(ctx, conn, "task_runs"); err != nil {
		return err
	} else if ok {
		runCols, err := columnNames(ctx, conn, "task_runs")
		if err != nil {
			return err
		}
		if err := addMissingColumns(ctx, conn, "task_runs", runCols, taskRunColumns); err != nil {
			return err
		}
		if err := backfillLegacyInflightRuns(ctx, conn); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"ready", "promoted"}, {"priority", "reprioritized"}, {"spawn_auto_blocked", "gave_up"}} {
		if _, err := conn.ExecContext(ctx, "UPDATE task_events SET kind = ? WHERE kind = ?", pair[1], pair[0]); err != nil {
			return err
		}
	}
	return rebuildDriftedTables(ctx, conn)
}

// Synthesizes a task_runs row for every task running from before runs existed, the CAS guarding against a racing claim
func backfillLegacyInflightRuns(ctx context.Context, conn state.SQLiteConn) error {
	return WriteTxn(ctx, conn, false, func() error {
		inflight, err := QueryRows(ctx, conn, "SELECT id, assignee, claim_lock, claim_expires, worker_pid, "+
			"       max_runtime_seconds, last_heartbeat_at, started_at "+
			"FROM tasks "+
			"WHERE status = 'running' AND current_run_id IS NULL")
		if err != nil {
			return err
		}
		for _, row := range inflight {
			started := row.Get("started_at")
			if !truthy(started) {
				started = time.Now().Unix()
			}
			res, err := conn.ExecContext(ctx, `
                INSERT INTO task_runs (
                    task_id, profile, status,
                    claim_lock, claim_expires, worker_pid,
                    max_runtime_seconds, last_heartbeat_at,
                    started_at
                ) VALUES (?, ?, 'running', ?, ?, ?, ?, ?, ?)
                `, row.Get("id"), row.Get("assignee"), row.Get("claim_lock"), row.Get("claim_expires"),
				row.Get("worker_pid"), row.Get("max_runtime_seconds"), row.Get("last_heartbeat_at"), started)
			if err != nil {
				return err
			}
			runID, err := res.LastInsertId()
			if err != nil {
				return err
			}
			upd, err := conn.ExecContext(ctx, "UPDATE tasks SET current_run_id = ? WHERE id = ? AND current_run_id IS NULL", runID, row.Get("id"))
			if err != nil {
				return err
			}
			if n, err := upd.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET status = 'reclaimed', "+
					"    outcome = 'reclaimed', ended_at = ? WHERE id = ?", time.Now().Unix(), runID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

type rebuildSpec struct {
	table     string
	createSQL string
	indexSQLs []string
}

var rebuildSpecs = []rebuildSpec{
	{"task_events",
		"CREATE TABLE task_events (" +
			" id INTEGER PRIMARY KEY AUTOINCREMENT," +
			" task_id TEXT NOT NULL, run_id INTEGER, kind TEXT NOT NULL," +
			" payload TEXT, created_at INTEGER NOT NULL)",
		[]string{
			"CREATE INDEX idx_events_task ON task_events(task_id, created_at)",
			"CREATE INDEX idx_events_run ON task_events(run_id, id)",
		}},
	{"task_comments",
		"CREATE TABLE task_comments (" +
			" id INTEGER PRIMARY KEY AUTOINCREMENT," +
			" task_id TEXT NOT NULL, author TEXT NOT NULL, body TEXT NOT NULL," +
			" created_at INTEGER NOT NULL)",
		[]string{"CREATE INDEX idx_comments_task ON task_comments(task_id, created_at)"}},
	{"task_runs",
		"CREATE TABLE task_runs (" +
			" id INTEGER PRIMARY KEY AUTOINCREMENT," +
			" task_id TEXT NOT NULL, profile TEXT, step_key TEXT," +
			" status TEXT NOT NULL, claim_lock TEXT, claim_expires INTEGER," +
			" worker_pid INTEGER, worker_started_at INTEGER, max_runtime_seconds INTEGER," +
			" last_heartbeat_at INTEGER, started_at INTEGER NOT NULL," +
			" ended_at INTEGER, outcome TEXT, summary TEXT, metadata TEXT," +
			" error TEXT)",
		[]string{
			"CREATE INDEX idx_runs_task ON task_runs(task_id, started_at)",
			"CREATE INDEX idx_runs_status ON task_runs(status)",
		}},
	{"kanban_notify_subs",
		"CREATE TABLE kanban_notify_subs (" +
			" task_id TEXT NOT NULL, platform TEXT NOT NULL, chat_id TEXT NOT NULL," +
			" thread_id TEXT NOT NULL DEFAULT '', user_id TEXT, user_id_alt TEXT," +
			" chat_type TEXT," +
			" notifier_profile TEXT, delivery_mode TEXT NOT NULL DEFAULT 'notify'," +
			" delivery_metadata TEXT, created_at INTEGER NOT NULL," +
			" last_event_id INTEGER NOT NULL DEFAULT 0," +
			" last_ping_event_id INTEGER NOT NULL DEFAULT 0," +
			" PRIMARY KEY (task_id, platform, chat_id, thread_id))",
		[]string{"CREATE INDEX idx_notify_task ON kanban_notify_subs(task_id)"}},
}

// Reports whether a table still carries the legacy pre AUTOINCREMENT shape
func tableHasDrifted(ctx context.Context, conn state.SQLiteConn, table string) (bool, error) {
	info, err := QueryRows(ctx, conn, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil || len(info) == 0 {
		return false, err
	}
	find := func(name string) *Row {
		for _, c := range info {
			if c.Get("name") == name {
				return c
			}
		}
		return nil
	}
	colType := func(c *Row) string {
		s, _ := c.Get("type").(string)
		return pystr.Upper(s)
	}
	if table == "kanban_notify_subs" {
		lei := find("last_event_id")
		return lei != nil && colType(lei) != "INTEGER", nil
	}
	id := find("id")
	if id == nil {
		return false, nil
	}
	return !(colType(id) == "INTEGER" && truthy(id.Get("pk"))), nil
}

// Rebuilds every drifted kanban table in one transaction with its indexes, legacy ids reassigned and cursors reset
func rebuildDriftedTables(ctx context.Context, conn state.SQLiteConn) error {
	var drifted []rebuildSpec
	for _, spec := range rebuildSpecs {
		d, err := tableHasDrifted(ctx, conn, spec.table)
		if err != nil {
			return err
		}
		if d {
			drifted = append(drifted, spec)
		}
	}
	if len(drifted) == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	err := func() error {
		for _, spec := range drifted {
			table := spec.table
			info, err := QueryRows(ctx, conn, fmt.Sprintf("PRAGMA table_info(%s)", table))
			if err != nil {
				return err
			}
			log.Info(fmt.Sprintf("kanban migration: rebuilding %s to match current schema", table))
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s RENAME TO %s_legacy", table, table)); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, spec.createSQL); err != nil {
				return err
			}
			newCols, err := columnNames(ctx, conn, table)
			if err != nil {
				return err
			}
			drop, extraCols, extraSelect := "id", "", ""
			if table == "kanban_notify_subs" {
				drop, extraCols, extraSelect = "last_event_id", ", last_event_id", ", COALESCE(CAST(last_event_id AS INTEGER), 0)"
			}
			var kept []string
			for _, c := range info {
				name, _ := c.Get("name").(string)
				if newCols[name] && name != drop {
					kept = append(kept, name)
				}
			}
			colsCSV := strings.Join(kept, ", ")
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s%s) SELECT %s%s FROM %s_legacy",
				table, colsCSV, extraCols, colsCSV, extraSelect, table)); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE %s_legacy", table)); err != nil {
				return err
			}
			for _, idx := range spec.indexSQLs {
				if _, err := conn.ExecContext(ctx, idx); err != nil {
					return err
				}
			}
		}
		_, err := conn.ExecContext(ctx, "COMMIT")
		return err
	}()
	if err != nil {
		if rerr := execSuppressOperational(ctx, conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	return nil
}

// Python truthiness of a SQLite cell value
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []byte:
		return len(x) > 0
	case bool:
		return x
	}
	return true
}

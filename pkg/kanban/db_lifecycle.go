package kanban

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Archives a task, a running task having its host local worker terminated after the commit
func ArchiveTask(ctx context.Context, conn state.SQLiteConn, taskID string, signalFn SignalFunc) (bool, error) {
	archived, wasRunning := false, false
	var prevPid, prevStarted any
	var prevLock *string
	var runID *int64
	err := WriteTxn(ctx, conn, false, func() error {
		row, err := QueryOne(ctx, conn, "SELECT status, claim_lock, worker_pid, worker_started_at FROM tasks WHERE id = ?", taskID)
		if err != nil || row == nil {
			return err
		}
		wasRunning = row.Get("status") == "running"
		prevPid, prevLock, prevStarted = row.Get("worker_pid"), cellOptStr(row.Get("claim_lock")), row.Get("worker_started_at")
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'archived', "+
			"    claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL "+
			"WHERE id = ? AND status != 'archived'", taskID)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		summary := "task archived with run still active"
		if runID, err = endRun(ctx, conn, taskID, "reclaimed", &summary, nil, nil, "reclaimed"); err != nil {
			return err
		}
		archived = true
		return appendEvent(ctx, conn, taskID, "archived", nil, runID)
	})
	if err != nil || !archived {
		return false, err
	}
	if wasRunning {
		termination := terminateReclaimedWorkerFn(prevPid, prevLock, signalFn, prevStarted)
		if err := WriteTxn(ctx, conn, false, func() error {
			return appendEvent(ctx, conn, taskID, "archive_worker_termination", termination, runID)
		}); err != nil {
			return false, err
		}
	}
	if _, err := RecomputeReady(ctx, conn, nil); err != nil {
		return false, err
	}
	cleanupWorkspace(ctx, conn, taskID)
	return true, nil
}

// Deletes every row referencing the task across the relation tables
func deleteTaskRelations(ctx context.Context, conn state.SQLiteConn, taskID string) error {
	if _, err := conn.ExecContext(ctx, "DELETE FROM task_links WHERE parent_id = ? OR child_id = ?", taskID, taskID); err != nil {
		return err
	}
	for _, table := range []string{"task_comments", "task_events", "task_runs", "kanban_notify_subs"} {
		if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE task_id = ?", taskID); err != nil {
			return err
		}
	}
	return nil
}

// Hard deletes an archived task with its related rows, anything else refused
func DeleteArchivedTask(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	deleted := false
	err := WriteTxn(ctx, conn, false, func() error {
		status, err := taskStatusOf(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if status == nil || *status != "archived" {
			return nil
		}
		if err := deleteTaskRelations(ctx, conn, taskID); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", taskID)
		if err != nil {
			return err
		}
		deleted = rowsAffected(res) == 1
		return nil
	})
	return deleted && err == nil, err
}

// Hard deletes a task and its related rows in one transaction, false when not found
func DeleteTask(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	deleted := false
	err := WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", taskID)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		deleted = true
		return deleteTaskRelations(ctx, conn, taskID)
	})
	if err != nil || !deleted {
		return false, err
	}
	if _, err := RecomputeReady(ctx, conn, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Parks a task in scheduled until an unblock re gates it
func ScheduleTask(ctx context.Context, conn state.SQLiteConn, taskID string, reason *string, expectedRunID *int64) (bool, error) {
	scheduled := false
	err := WriteTxn(ctx, conn, false, func() error {
		params := []any{taskID}
		sql := `
            UPDATE tasks
               SET status       = 'scheduled',
                   claim_lock   = NULL,
                   claim_expires= NULL,
                   worker_pid   = NULL
             WHERE id = ?
               AND status IN ('todo', 'ready', 'running', 'blocked')
        `
		if expectedRunID != nil {
			if err := runIDBindable(expectedRunID); err != nil {
				return err
			}
			sql += " AND current_run_id = ?"
			params = append(params, *expectedRunID)
		}
		res, err := conn.ExecContext(ctx, sql, params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		runID, err := endOrSynthesizeRunFn(ctx, conn, taskID, "scheduled", "scheduled", reason, nil, truthyStr(reason), runProfile{})
		if err != nil {
			return err
		}
		scheduled = true
		return appendEvent(ctx, conn, taskID, "scheduled", payload("reason", optStrValue(reason)), runID)
	})
	return scheduled && err == nil, err
}

// Everything a worker reads about its task as capped markdown sections
func BuildWorkerContext(ctx context.Context, conn state.SQLiteConn, taskID string) (string, error) {
	task, err := GetTask(ctx, conn, taskID)
	if err != nil {
		return "", err
	}
	if task == nil {
		return "", &ValueError{fmt.Sprintf("unknown task %s", taskID)}
	}
	now := time.Now().Unix()
	var lines []string
	ctxHeader(&lines, task)
	attachments, err := ListAttachments(ctx, conn, taskID)
	if err != nil {
		return "", err
	}
	ctxAttachments(&lines, attachments)
	if err := ctxPriorAttempts(ctx, &lines, conn, taskID, now); err != nil {
		return "", err
	}
	if err := ctxParentResults(ctx, &lines, conn, taskID, now); err != nil {
		return "", err
	}
	if err := ctxRoleHistory(ctx, &lines, conn, task, now); err != nil {
		return "", err
	}
	comments, err := ListComments(ctx, conn, taskID)
	if err != nil {
		return "", err
	}
	ctxComments(&lines, comments, now)
	return pystr.Rstrip(strings.Join(lines, "\n")) + "\n", nil
}

// Per status and per assignee counts with the oldest ready age
func BoardStats(ctx context.Context, conn state.SQLiteConn) (*pyjson.Object, error) {
	byStatus := pyjson.NewObject()
	rows, err := QueryRows(ctx, conn, "SELECT status, COUNT(*) AS n FROM tasks WHERE status != 'archived' GROUP BY status")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		byStatus.Set(cellStr(row.Get("status")), cellInt(row.Get("n")))
	}
	byAssignee, err := countsByAssignee(ctx, conn)
	if err != nil {
		return nil, err
	}
	oldest, err := QueryOne(ctx, conn, "SELECT MIN(created_at) AS ts FROM tasks WHERE status = 'ready'")
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var oldestReadyAge any
	if oldest != nil && oldest.Get("ts") != nil {
		ts, err := pyIntOf(oldest.Get("ts"))
		if err != nil {
			return nil, err
		}
		oldestReadyAge = now - ts
	}
	return payload("by_status", byStatus, "by_assignee", byAssignee, "oldest_ready_age_seconds", oldestReadyAge, "now", now), nil
}

// Status counts per assignee over non archived tasks
func countsByAssignee(ctx context.Context, conn state.SQLiteConn) (*pyjson.Object, error) {
	counts := pyjson.NewObject()
	rows, err := QueryRows(ctx, conn, "SELECT assignee, status, COUNT(*) AS n FROM tasks "+
		"WHERE status != 'archived' AND assignee IS NOT NULL "+
		"GROUP BY assignee, status")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		assignee := cellStr(row.Get("assignee"))
		inner, ok := counts.Get(assignee)
		if !ok {
			inner = pyjson.NewObject()
			counts.Set(assignee, inner)
		}
		inner.(*pyjson.Object).Set(cellStr(row.Get("status")), cellInt(row.Get("n")))
	}
	return counts, nil
}

// Age metrics of one task in seconds, nil where the stamp is absent
func TaskAge(task *Task) *pyjson.Object {
	now := time.Now().Unix()
	var createdAge, startedAge, timeToComplete any
	if task.CreatedAt != nil {
		createdAge = now - *task.CreatedAt
	}
	if task.StartedAt != nil {
		startedAge = now - *task.StartedAt
	}
	if task.CompletedAt != nil {
		base := task.CreatedAt
		if task.StartedAt != nil && *task.StartedAt != 0 {
			base = task.StartedAt
		}
		if base != nil {
			timeToComplete = *task.CompletedAt - *base
		}
	}
	return payload("created_age_seconds", createdAge, "started_age_seconds", startedAge, "time_to_complete_seconds", timeToComplete)
}

// Garbage collection window, a negative one refused because it would select everything
func retentionSeconds(olderThanSeconds int64) (int64, error) {
	if olderThanSeconds < 0 {
		return 0, &ValueError{fmt.Sprintf("older_than_seconds must be >= 0, got %s: a negative retention selects everything.", pyrepr.Repr(olderThanSeconds))}
	}
	return olderThanSeconds, nil
}

const defaultRetentionSeconds = 30 * 24 * 3600

// Prunes old events of done and archived tasks, decomposition identity kept, returning the count deleted
func GCEvents(ctx context.Context, conn state.SQLiteConn, olderThanSeconds int64) (int64, error) {
	retention, err := retentionSeconds(olderThanSeconds)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Unix() - retention
	var deleted int64
	err = WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "DELETE FROM task_events WHERE created_at < ? AND kind != 'decomposed' AND task_id IN "+
			"(SELECT id FROM tasks WHERE status IN ('done', 'archived'))", cutoff)
		if err != nil {
			return err
		}
		deleted = rowsAffected(res)
		return nil
	})
	return deleted, err
}

// Deletes worker log files older than the window on one board, returning the count
func GCWorkerLogs(ctx context.Context, olderThanSeconds int64, board *string) (int, error) {
	retention, err := retentionSeconds(olderThanSeconds)
	if err != nil {
		return 0, err
	}
	logDir, err := WorkerLogsDir(ctx, board)
	if err != nil {
		return 0, err
	}
	if !pathExists(logDir) {
		return 0, nil
	}
	return pyfs.UnlinkFilesOlderThan(logDir, "*", float64(retention), nil), nil
}

// Worker log path of a task on a board, which may not exist yet
func WorkerLogPath(ctx context.Context, taskID string, board *string) (string, error) {
	dir, err := WorkerLogsDir(ctx, board)
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(dir, taskID+".log"), nil
}

// Worker log text or its last tailBytes, nil when the file is missing or unreadable
func ReadWorkerLog(ctx context.Context, taskID string, tailBytes *int64, board *string) (*string, error) {
	path, err := WorkerLogPath(ctx, taskID, board)
	if err != nil {
		return nil, err
	}
	if !pathExists(path) {
		return nil, nil
	}
	if tailBytes == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil
		}
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(pystr.DecodeUTF8Replace(data))
		return &text, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	size := st.Size()
	offset := int64(0)
	if size > *tailBytes {
		offset = size - *tailBytes
		r := bufio.NewReader(io.NewSectionReader(f, offset, size-offset))
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, nil
		}
		consumed := offset + int64(len(line))
		if !bytes.HasSuffix(line, []byte("\n")) && consumed >= size {
			consumed = offset
		}
		offset = consumed
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil
	}
	text := pystr.DecodeUTF8Replace(data)
	return &text, nil
}

// Profiles with a config.yaml plus the implicit default, sorted
func ListProfilesOnDisk() []string {
	defaultRoot := home.Root()
	profilesDir := pypath.PathJoin(defaultRoot, "profiles")
	names := map[string]bool{}
	if pathExists(defaultRoot) {
		names["default"] = true
	}
	if isDir(profilesDir) {
		if entries, err := os.ReadDir(profilesDir); err == nil {
			for _, e := range entries {
				dir := pypath.PathJoin(profilesDir, e.Name())
				if isDir(dir) && isFile(pypath.PathJoin(dir, "config.yaml")) {
					names[e.Name()] = true
				}
			}
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Every on disk profile or task assignee with its presence and status counts
func KnownAssignees(ctx context.Context, conn state.SQLiteConn) ([]*pyjson.Object, error) {
	onDisk := map[string]bool{}
	for _, n := range ListProfilesOnDisk() {
		onDisk[n] = true
	}
	counts, err := countsByAssignee(ctx, conn)
	if err != nil {
		return nil, err
	}
	all := map[string]bool{}
	for n := range onDisk {
		all[n] = true
	}
	for _, k := range counts.Keys {
		all[k] = true
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*pyjson.Object, 0, len(names))
	for _, n := range names {
		c, ok := counts.Get(n)
		if !ok {
			c = pyjson.NewObject()
		}
		out = append(out, payload("name", n, "on_disk", onDisk[n], "counts", c))
	}
	return out, nil
}

// Runs of a task in start order, closed only without active, filtered by one status or outcome state
func ListRuns(ctx context.Context, conn state.SQLiteConn, taskID string, includeActive bool, stateType, stateName *string) ([]*Run, error) {
	if (stateType == nil) != (stateName == nil) {
		return nil, &ValueError{"state_type and state_name must both be set or both omitted"}
	}
	if stateType != nil && *stateType != "status" && *stateType != "outcome" {
		return nil, &ValueError{"state_type must be 'status' or 'outcome'"}
	}
	q := "SELECT * FROM task_runs WHERE task_id = ?"
	params := []any{taskID}
	if !includeActive {
		q += " AND ended_at IS NOT NULL"
	}
	if stateType != nil {
		q += " AND " + *stateType + " = ?"
		params = append(params, *stateName)
	}
	q += " ORDER BY started_at ASC, id ASC"
	rows, err := QueryRows(ctx, conn, q, params...)
	if err != nil {
		return nil, err
	}
	return runsFromRows(rows)
}

// Runs decoded from rows
func runsFromRows(rows []*Row) ([]*Run, error) {
	out := make([]*Run, 0, len(rows))
	for _, r := range rows {
		run, err := RunFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

// One run by id, nil when absent
func GetRun(ctx context.Context, conn state.SQLiteConn, runID int64) (*Run, error) {
	row, err := QueryOne(ctx, conn, "SELECT * FROM task_runs WHERE id = ?", runID)
	if err != nil || row == nil {
		return nil, err
	}
	return RunFromRow(row)
}

// Most recent run of a task regardless of outcome, nil when none
func LatestRun(ctx context.Context, conn state.SQLiteConn, taskID string) (*Run, error) {
	row, err := QueryOne(ctx, conn, "SELECT * FROM task_runs WHERE task_id = ? ORDER BY started_at DESC, id DESC LIMIT 1", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	return RunFromRow(row)
}

// Newest non empty run summary of a task, nil when none
func LatestSummary(ctx context.Context, conn state.SQLiteConn, taskID string) (*string, error) {
	row, err := QueryOne(ctx, conn, "SELECT summary FROM task_runs "+
		"WHERE task_id = ? AND summary IS NOT NULL AND summary != '' "+
		"ORDER BY COALESCE(ended_at, started_at) DESC, id DESC LIMIT 1", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	return cellOptStr(row.Get("summary")), nil
}

// Newest non empty run summary per task in one window query, tasks without one omitted
func LatestSummaries(ctx context.Context, conn state.SQLiteConn, taskIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	params := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		params[i] = id
	}
	rows, err := QueryRows(ctx, conn, `
        SELECT task_id, summary FROM (
            SELECT task_id, summary,
                   ROW_NUMBER() OVER (
                       PARTITION BY task_id
                       ORDER BY COALESCE(ended_at, started_at) DESC, id DESC
                   ) AS rn
              FROM task_runs
             WHERE task_id IN (`+sqlMarks(len(taskIDs))+`)
               AND summary IS NOT NULL AND summary != ''
        ) WHERE rn = 1
        `, params...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[cellStr(r.Get("task_id"))] = cellStr(r.Get("summary"))
	}
	return out, nil
}

// Started at of the run each task current_run_id points at, tasks without an active run omitted
func CurrentRunStartedAts(ctx context.Context, conn state.SQLiteConn, taskIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	params := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		params[i] = id
	}
	rows, err := QueryRows(ctx, conn, "SELECT t.id AS task_id, r.started_at AS started_at FROM tasks t "+
		"JOIN task_runs r ON r.id = t.current_run_id "+
		"WHERE t.id IN ("+sqlMarks(len(taskIDs))+")", params...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[cellStr(r.Get("task_id"))] = cellInt(r.Get("started_at"))
	}
	return out, nil
}

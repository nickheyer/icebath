package gateway

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	MaxActivePolicyEvents        = 64
	maxActiveRetryReceipts       = 64
	maxThreadTranscriptEvents    = 24
	policyTranscriptSchema       = 4
	policyDeleteActiveEventsSQL  = "DELETE FROM hosted_room_policy_events WHERE room_id=? AND discussion_event_id=?"
	policyMaxSQLiteLimitArgument = math.MaxInt64 - 1
)

var (
	policyTerminalKinds = map[string]bool{"turn.settled": true, "turn.failed": true, "turn.cancelled": true, "turn.deferred": true}
	policySchemaDDL     = []string{
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_capacity (
        singleton INTEGER PRIMARY KEY CHECK(singleton=1), max_active_events INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_cursors (
        room_id TEXT PRIMARY KEY, through_seq INTEGER NOT NULL DEFAULT 0,
        stopped_through_seq INTEGER NOT NULL DEFAULT 0, updated_at REAL NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_threads (
        room_id TEXT NOT NULL, thread_id TEXT NOT NULL, discussion_event_id TEXT NOT NULL,
        latest_user_seq INTEGER NOT NULL, completed INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY(room_id, thread_id))`,
		`CREATE INDEX IF NOT EXISTS idx_hosted_room_policy_pending
        ON hosted_room_policy_threads(room_id, completed, latest_user_seq, thread_id)`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_events (
        room_id TEXT NOT NULL, thread_id TEXT NOT NULL, discussion_event_id TEXT NOT NULL,
        seq INTEGER NOT NULL, event_json TEXT NOT NULL, PRIMARY KEY(room_id, seq))`,
		`CREATE INDEX IF NOT EXISTS idx_hosted_room_policy_events_active
        ON hosted_room_policy_events(room_id, discussion_event_id, seq)`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_watermarks (
        room_id TEXT NOT NULL, thread_id TEXT NOT NULL, member_id TEXT NOT NULL,
        seen_through_seq INTEGER NOT NULL, PRIMARY KEY(room_id, thread_id, member_id))`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_publications (
        room_id TEXT NOT NULL, task_id TEXT NOT NULL, kind TEXT NOT NULL,
        execution_generation INTEGER NOT NULL DEFAULT 0, seq INTEGER NOT NULL,
        PRIMARY KEY(room_id, task_id, kind, execution_generation))`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_transcript (
        room_id TEXT NOT NULL, thread_id TEXT NOT NULL, seq INTEGER NOT NULL,
        kind TEXT NOT NULL, settled_seq INTEGER, PRIMARY KEY(room_id, thread_id, seq))`,
		`CREATE TABLE IF NOT EXISTS hosted_room_policy_transcript_state (
        room_id TEXT PRIMARY KEY, schema_version INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_hosted_room_user_thread
       ON hosted_room_events(room_id, json_extract(payload_json, '$.thread_id'), seq)
       WHERE kind='message.user'`,
	}
	policyTranscriptEventsSQL = `WITH transcript_events(seq) AS (
        SELECT seq FROM hosted_room_policy_transcript WHERE room_id=? AND thread_id=?
        UNION ALL
        SELECT settled_seq FROM hosted_room_policy_transcript
         WHERE room_id=? AND thread_id=? AND settled_seq IS NOT NULL)
    SELECT ` + strings.Join(mapStrings(strings.Split(hostedEventColumns, ", "), func(c string) string { return "events." + c }), ", ") + `
    FROM transcript_events
    JOIN hosted_room_events AS events ON events.room_id=? AND events.seq=transcript_events.seq
    ORDER BY events.seq LIMIT ?`
)

// Bounded active policy input at one durable room log cursor, watermarks keyed by thread and member
type PolicySnapshot struct {
	ThroughSeq        int64
	StoppedThroughSeq int64
	Events            []map[string]any
	Watermarks        map[[2]string]int64
}

// Python str of a mapping value, empty for a falsy one
func policyText(mapping map[string]any, key string) string {
	if !pyrepr.Truthy(mapping[key]) {
		return ""
	}
	return pyrepr.Str(mapping[key])
}

// Python int of a mapping value, zero for a falsy one
func policyInt(mapping map[string]any, key string) (int64, error) {
	if !pyrepr.Truthy(mapping[key]) {
		return 0, nil
	}
	return pyint.Int64(mapping[key])
}

// Refuses a room the store does not hold
func policyRequireRoom(ctx context.Context, conn state.SQLiteConn, roomID string) error {
	row, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_rooms WHERE room_id=?", roomID)
	if err == nil && row == nil {
		return roomNotFound("hosted room not found")
	}
	return err
}

// Committed member message of a discussion, readable after its active projection compacted
func policySettledMessage(ctx context.Context, conn state.SQLiteConn, roomID, discussionEventID string, messageEventID any) (map[string]any, error) {
	row, err := state.QueryRowObject(ctx, conn, "SELECT "+hostedEventColumns+" FROM hosted_room_events WHERE room_id=? AND event_id=?", roomID, messageEventID)
	if err != nil || row == nil {
		return nil, err
	}
	event, err := hostedEventFromRow(row, false)
	if err != nil {
		return nil, err
	}
	payload, _ := event["payload"].(map[string]any)
	if event["kind"] != "message.member" || policyText(payload, "discussion_event_id") != discussionEventID {
		return nil, nil
	}
	return event, nil
}

// Incremental room policy index that never compacts visible history
type HostedRoomPolicyCheckpoint struct {
	DBPath                    string
	MaxActiveEvents           int64
	maxTranscriptPolicyEvents int64
}

// Checkpoint over dbPath whose replay capacity never drops below a capacity stored earlier
func NewHostedRoomPolicyCheckpoint(ctx context.Context, dbPath string, maxActiveEvents any) (*HostedRoomPolicyCheckpoint, error) {
	requested, isInt := hostedStrictInt(maxActiveEvents)
	if _, isBool := maxActiveEvents.(bool); isBool || !isInt || requested < 1 {
		if n, ok := roomInt(maxActiveEvents); !(ok && n.Sign() > 0 && !n.IsInt64()) || isBool {
			return nil, roomPlainValueError("max_active_events must be a positive integer")
		}
		requested = math.MaxInt64
	}
	c := &HostedRoomPolicyCheckpoint{DBPath: dbPath}
	capacity := min(int64(policyMaxSQLiteLimitArgument), max(MaxActivePolicyEvents, requested))
	if err := c.transaction(ctx, false, func(conn state.SQLiteConn) error {
		for _, ddl := range policySchemaDDL {
			if _, err := conn.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_policy_capacity(singleton, max_active_events)
                VALUES (1, ?) ON CONFLICT(singleton) DO UPDATE SET
                max_active_events=MAX(max_active_events, excluded.max_active_events)`, capacity); err != nil {
			return err
		}
		return conn.QueryRowContext(ctx, "SELECT max_active_events FROM hosted_room_policy_capacity WHERE singleton=1").Scan(&c.MaxActiveEvents)
	}); err != nil {
		return nil, err
	}
	c.maxTranscriptPolicyEvents = policyMaxSQLiteLimitArgument
	if c.MaxActiveEvents <= (policyMaxSQLiteLimitArgument/maxThreadTranscriptEvents)-maxActiveRetryReceipts-2 {
		c.maxTranscriptPolicyEvents = maxThreadTranscriptEvents * (c.MaxActiveEvents + maxActiveRetryReceipts + 2)
	}
	return c, nil
}

// Room policy connection with the WAL and busy timeout of the shared store
func (c *HostedRoomPolicyCheckpoint) connect(ctx context.Context) (state.SQLiteConn, error) {
	return state.OpenDB(ctx, c.DBPath, "shared-state.db (room policy checkpoint)", state.WithBusyTimeoutMS(10_000))
}

// Runs body in one transaction of a fresh checkpoint connection
func (c *HostedRoomPolicyCheckpoint) transaction(ctx context.Context, immediate bool, body func(conn state.SQLiteConn) error) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, immediate, body)
}

// Indexes one event of the active discussion projection
func policyStoreActiveEvent(ctx context.Context, conn state.SQLiteConn, event map[string]any, threadID, discussionEventID string) error {
	encoded, err := roomCompactJSON(event, true)
	if err != nil {
		return err
	}
	seq, err := pyint.Int64(event["seq"])
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO hosted_room_policy_events(
                   room_id, thread_id, discussion_event_id, seq, event_json
               ) VALUES (?, ?, ?, ?, ?)`, event["room_id"], threadID, discussionEventID, seq, encoded)
	return err
}

// References one event in the thread transcript, trimming it to its message bound
func policyStoreTranscriptEvent(ctx context.Context, conn state.SQLiteConn, event map[string]any, threadID string, settledSeq *int64) error {
	seq, err := pyint.Int64(event["seq"])
	if err != nil {
		return err
	}
	var settled any
	if settledSeq != nil {
		settled = *settledSeq
	}
	kind := pyrepr.Str(event["kind"])
	if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_policy_transcript(
                   room_id, thread_id, seq, kind, settled_seq
               ) VALUES (?, ?, ?, ?, ?)
               ON CONFLICT(room_id, thread_id, seq) DO UPDATE SET
                   settled_seq=COALESCE(excluded.settled_seq, hosted_room_policy_transcript.settled_seq)`, event["room_id"], threadID, seq, kind, settled); err != nil {
		return err
	}
	if kind != "message.user" && kind != "message.member" {
		return nil
	}
	cutoff, err := state.QueryRowObject(ctx, conn, `SELECT seq FROM hosted_room_policy_transcript
                   WHERE room_id=? AND thread_id=? AND kind IN ('message.user', 'message.member')
                   ORDER BY seq DESC LIMIT 1 OFFSET ?`, event["room_id"], threadID, maxThreadTranscriptEvents-1)
	if err != nil || cutoff == nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "DELETE FROM hosted_room_policy_transcript WHERE room_id=? AND thread_id=? AND seq<?", event["room_id"], threadID, hrInt(cutoff, "seq"))
	return err
}

// Thread transcript merged with the active projection in sequence order
func (c *HostedRoomPolicyCheckpoint) discussionEvents(ctx context.Context, conn state.SQLiteConn, roomID, threadID, discussionEventID, boundError string) ([]map[string]any, error) {
	bySeq := map[int64]map[string]any{}
	var active []*pyjson.Object
	for _, pass := range []struct {
		receipts int
		bound    int64
	}{{0, c.MaxActiveEvents}, {1, maxActiveRetryReceipts}} {
		rows, err := state.QueryObjects(ctx, conn, `SELECT event_json FROM hosted_room_policy_events
                   WHERE room_id=? AND discussion_event_id=?
                     AND (json_extract(event_json, '$.kind') = 'turn.deferred') = ?
                   ORDER BY seq LIMIT ?`, roomID, discussionEventID, pass.receipts, pass.bound+1)
		if err != nil {
			return nil, err
		}
		if int64(len(rows)) > pass.bound {
			return nil, errors.New(boundError)
		}
		active = append(active, rows...)
	}
	rows, err := state.QueryObjects(ctx, conn, policyTranscriptEventsSQL, roomID, threadID, roomID, threadID, roomID, c.maxTranscriptPolicyEvents+1)
	if err != nil {
		return nil, err
	}
	if int64(len(rows)) > c.maxTranscriptPolicyEvents {
		return nil, errors.New("thread policy transcript exceeded its bound")
	}
	for _, row := range rows {
		event, err := hostedEventFromRow(row, false)
		if err != nil {
			return nil, err
		}
		bySeq[event["seq"].(int64)] = event
	}
	for _, row := range active {
		decoded, err := roomLoads(hrStr(row, "event_json"))
		if err != nil {
			return nil, err
		}
		event, _ := decoded.(map[string]any)
		seq, err := pyint.Int64(event["seq"])
		if err != nil {
			return nil, err
		}
		bySeq[seq] = event
	}
	return policyOrdered(bySeq), nil
}

// Events of a sequence keyed map in ascending order
func policyOrdered(bySeq map[int64]map[string]any) []map[string]any {
	seqs := make([]int64, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]map[string]any, len(seqs))
	for i, seq := range seqs {
		out[i] = bySeq[seq]
	}
	return out
}

// Opens or replaces a thread's active discussion at a user message
func policyApplyUserMessage(ctx context.Context, conn state.SQLiteConn, event, payload map[string]any) error {
	threadID, eventID := policyText(payload, "thread_id"), policyText(event, "event_id")
	if threadID == "" || eventID == "" {
		return nil
	}
	seq, err := pyint.Int64(event["seq"])
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_policy_threads(
                   room_id, thread_id, discussion_event_id, latest_user_seq, completed
               ) VALUES (?, ?, ?, ?, 0)
               ON CONFLICT(room_id, thread_id) DO UPDATE SET
                   discussion_event_id=excluded.discussion_event_id,
                   latest_user_seq=excluded.latest_user_seq, completed=0`, pyrepr.Str(event["room_id"]), threadID, eventID, seq); err != nil {
		return err
	}
	if err := policyStoreActiveEvent(ctx, conn, event, threadID, eventID); err != nil {
		return err
	}
	return policyStoreTranscriptEvent(ctx, conn, event, threadID, nil)
}

// Indexes member messages and terminal outcomes of a known discussion
func policyApplyDiscussionEvent(ctx context.Context, conn state.SQLiteConn, event, payload map[string]any) error {
	roomID, kind := pyrepr.Str(event["room_id"]), policyText(event, "kind")
	seq, err := pyint.Int64(event["seq"])
	if err != nil {
		return err
	}
	threadID, discussionEventID := policyText(payload, "thread_id"), policyText(payload, "discussion_event_id")
	known, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_room_policy_events WHERE room_id=? AND discussion_event_id=? LIMIT 1", roomID, discussionEventID)
	if err != nil {
		return err
	}
	if known != nil {
		if err := policyStoreActiveEvent(ctx, conn, event, threadID, discussionEventID); err != nil {
			return err
		}
	}
	if !policyTerminalKinds[kind] {
		return nil
	}
	taskID := policyText(payload, "task_id")
	var generation int64
	if kind == "turn.deferred" {
		if generation, err = policyInt(payload, "execution_generation"); err != nil {
			return err
		}
	}
	if taskID != "" {
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO hosted_room_policy_publications(
                       room_id, task_id, kind, execution_generation, seq
                   ) VALUES (?, ?, ?, ?, ?)`, roomID, taskID, kind, generation, seq); err != nil {
			return err
		}
	}
	memberID := policyText(payload, "member_id")
	seenThrough, err := policyInt(payload, "seen_through_seq")
	if err != nil {
		return err
	}
	if kind == "turn.settled" && pyrepr.Truthy(payload["message_event_id"]) {
		committed, err := policySettledMessage(ctx, conn, roomID, discussionEventID, payload["message_event_id"])
		if err != nil {
			return err
		}
		if committed != nil {
			seenThrough = max(seenThrough, committed["seq"].(int64))
			if err := policyStoreTranscriptEvent(ctx, conn, committed, threadID, &seq); err != nil {
				return err
			}
		}
	} else if err := policyStoreTranscriptEvent(ctx, conn, event, threadID, nil); err != nil {
		return err
	}
	if memberID != "" && seenThrough > 0 {
		_, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_policy_watermarks(
                       room_id, thread_id, member_id, seen_through_seq
                   ) VALUES (?, ?, ?, ?)
                   ON CONFLICT(room_id, thread_id, member_id) DO UPDATE SET
                       seen_through_seq=MAX(hosted_room_policy_watermarks.seen_through_seq, excluded.seen_through_seq)`, roomID, threadID, memberID, seenThrough)
		return err
	}
	return nil
}

// Drops a discussion the room activity closed
func policyApplyRoomActivity(ctx context.Context, conn state.SQLiteConn, event, payload map[string]any) error {
	roomID := pyrepr.Str(event["room_id"])
	if _, err := conn.ExecContext(ctx, policyDeleteActiveEventsSQL, roomID, policyText(payload, "discussion_event_id")); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_policy_threads WHERE room_id=? AND thread_id=?", roomID, policyText(payload, "thread_id"))
	return err
}

// Advances the stop fence of the room cursor
func policyApplyStopRequested(ctx context.Context, conn state.SQLiteConn, event, _ map[string]any) error {
	seq, err := pyint.Int64(event["seq"])
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `UPDATE hosted_room_policy_cursors
               SET stopped_through_seq=MAX(stopped_through_seq, ?) WHERE room_id=?`, seq, pyrepr.Str(event["room_id"]))
	return err
}

// Projects one event through the handler of its kind
func policyApplyEvent(ctx context.Context, conn state.SQLiteConn, event map[string]any) error {
	kind := policyText(event, "kind")
	var handler func(context.Context, state.SQLiteConn, map[string]any, map[string]any) error
	switch {
	case kind == "message.user":
		handler = policyApplyUserMessage
	case kind == "message.member" || policyTerminalKinds[kind]:
		handler = policyApplyDiscussionEvent
	case kind == "room.activity":
		handler = policyApplyRoomActivity
	case kind == "room.stop_requested":
		handler = policyApplyStopRequested
	default:
		return nil
	}
	payload, ok := event["payload"].(map[string]any)
	if !ok {
		payload = map[string]any{}
	}
	return handler(ctx, conn, event, payload)
}

// Creates the room cursor and rebuilds derived caches of an older transcript schema
func policyEnsureCursorAndTranscript(ctx context.Context, conn state.SQLiteConn, roomID string) (int64, error) {
	if err := policyRequireRoom(ctx, conn, roomID); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO hosted_room_policy_cursors(
                   room_id, through_seq, stopped_through_seq, updated_at
               ) VALUES (?, 0, 0, 0)`, roomID); err != nil {
		return 0, err
	}
	var cursor int64
	if err := conn.QueryRowContext(ctx, "SELECT through_seq FROM hosted_room_policy_cursors WHERE room_id=?", roomID).Scan(&cursor); err != nil {
		return 0, err
	}
	transcript, err := state.QueryRowObject(ctx, conn, "SELECT schema_version FROM hosted_room_policy_transcript_state WHERE room_id=?", roomID)
	if err != nil {
		return 0, err
	}
	if transcript == nil || hrInt(transcript, "schema_version") < policyTranscriptSchema {
		for _, table := range []string{"hosted_room_policy_events", "hosted_room_policy_threads", "hosted_room_policy_watermarks", "hosted_room_policy_transcript"} {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE room_id=?", roomID); err != nil {
				return 0, err
			}
		}
		if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_policy_cursors SET through_seq=0, stopped_through_seq=0 WHERE room_id=?", roomID); err != nil {
			return 0, err
		}
		cursor = 0
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_policy_transcript_state(room_id, schema_version)
                   VALUES (?, ?)
                   ON CONFLICT(room_id) DO UPDATE SET schema_version=excluded.schema_version`, roomID, policyTranscriptSchema); err != nil {
			return 0, err
		}
	}
	return cursor, nil
}

// Materializes each unseen room event exactly once behind the durable cursor
func (c *HostedRoomPolicyCheckpoint) Sync(ctx context.Context, roomID string, latestSeq int64) (int64, error) {
	var cursor int64
	if err := c.transaction(ctx, true, func(conn state.SQLiteConn) (err error) {
		cursor, err = policyEnsureCursorAndTranscript(ctx, conn, roomID)
		return err
	}); err != nil {
		return 0, err
	}
	if cursor > latestSeq {
		conn, err := c.connect(ctx)
		if err != nil {
			return 0, err
		}
		room, err := state.QueryRowObject(ctx, conn, "SELECT next_seq FROM hosted_rooms WHERE room_id=?", roomID)
		conn.Close()
		if err != nil {
			return 0, err
		}
		if room == nil {
			return 0, roomNotFound("hosted room not found")
		}
		if cursor >= hrInt(room, "next_seq") {
			return 0, errors.New("room policy cursor is ahead of the durable log")
		}
	}
	for cursor < latestSeq {
		page, err := ReadEvents(ctx, c.DBPath, roomID, cursor, int64(MaxRoomLogLimit), false)
		if err != nil {
			return 0, err
		}
		var rows []map[string]any
		for _, item := range page["events"].([]any) {
			if event, ok := item.(map[string]any); ok {
				rows = append(rows, event)
			}
		}
		nextCursor := cursor
		if value, ok := page["cursor"].(int64); ok && value != 0 {
			nextCursor = value
		}
		if len(rows) == 0 || nextCursor <= cursor {
			return 0, errors.New("hosted room policy cursor did not advance")
		}
		reloaded := false
		if err := c.transaction(ctx, true, func(conn state.SQLiteConn) error {
			if err := policyRequireRoom(ctx, conn, roomID); err != nil {
				return err
			}
			current, err := state.QueryRowObject(ctx, conn, "SELECT through_seq FROM hosted_room_policy_cursors WHERE room_id=?", roomID)
			if err != nil {
				return err
			}
			if current == nil {
				return errors.New("room policy cursor disappeared during replay")
			}
			if through := hrInt(current, "through_seq"); through != cursor {
				cursor, reloaded = through, true
				return nil
			}
			for _, event := range rows {
				if err := policyApplyEvent(ctx, conn, event); err != nil {
					return err
				}
			}
			updatedAt := 0.0
			if last := rows[len(rows)-1]; pyrepr.Truthy(last["created_at"]) {
				if updatedAt, err = pyint.Float(last["created_at"]); err != nil {
					return err
				}
			}
			return roomFencedUpdate(ctx, conn, "UPDATE hosted_room_policy_cursors SET through_seq=?, updated_at=? WHERE room_id=?",
				[]any{nextCursor, updatedAt, roomID}, errors.New("room policy cursor disappeared during replay"))
		}); err != nil {
			return 0, err
		}
		if !reloaded {
			cursor = nextCursor
		}
	}
	return cursor, nil
}

// Oldest active discussion of the room and its watermark set after a sync
func (c *HostedRoomPolicyCheckpoint) Snapshot(ctx context.Context, roomID string, latestSeq int64) (PolicySnapshot, error) {
	if _, err := c.Sync(ctx, roomID, latestSeq); err != nil {
		return PolicySnapshot{}, err
	}
	var out PolicySnapshot
	err := c.transaction(ctx, false, func(conn state.SQLiteConn) error {
		cursor, err := state.QueryRowObject(ctx, conn, "SELECT through_seq, stopped_through_seq FROM hosted_room_policy_cursors WHERE room_id=?", roomID)
		if err != nil {
			return err
		}
		if cursor == nil {
			return roomNotFound("hosted room checkpoint not found")
		}
		out = PolicySnapshot{ThroughSeq: hrInt(cursor, "through_seq"), StoppedThroughSeq: hrInt(cursor, "stopped_through_seq"), Events: []map[string]any{}, Watermarks: map[[2]string]int64{}}
		thread, err := state.QueryRowObject(ctx, conn, `SELECT thread_id, discussion_event_id FROM hosted_room_policy_threads
                   WHERE room_id=? AND completed=0 AND latest_user_seq>?
                   ORDER BY latest_user_seq, thread_id LIMIT 1`, roomID, out.StoppedThroughSeq)
		if err != nil || thread == nil {
			return err
		}
		threadID := pyrepr.Str(thread.Vals["thread_id"])
		if out.Events, err = c.discussionEvents(ctx, conn, roomID, threadID, pyrepr.Str(thread.Vals["discussion_event_id"]), "active room policy projection exceeded its bound"); err != nil {
			return err
		}
		rows, err := state.QueryObjects(ctx, conn, `SELECT member_id, seen_through_seq FROM hosted_room_policy_watermarks
                   WHERE room_id=? AND thread_id=?`, roomID, threadID)
		for _, row := range rows {
			out.Watermarks[[2]string{threadID, pyrepr.Str(row.Vals["member_id"])}] = hrInt(row, "seen_through_seq")
		}
		return err
	})
	return out, err
}

// Whether one exact driver outcome is already in the room log
func (c *HostedRoomPolicyCheckpoint) PublicationExists(ctx context.Context, roomID, taskID, status string, executionGeneration int64) (bool, error) {
	query, args := `SELECT 1 FROM hosted_room_policy_publications
                     WHERE room_id=? AND task_id=? AND kind IN ('turn.settled', 'turn.failed', 'turn.cancelled')`, []any{roomID, taskID}
	if status == "deferred" {
		query, args = `SELECT 1 FROM hosted_room_policy_publications
                     WHERE room_id=? AND task_id=? AND kind=? AND execution_generation=?`, []any{roomID, taskID, "turn." + status, executionGeneration}
	}
	exists := false
	err := c.transaction(ctx, false, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(ctx, conn, query, args...)
		exists = row != nil
		return err
	})
	return exists, err
}

// One bounded discussion projection for terminal reconstruction of a task
func (c *HostedRoomPolicyCheckpoint) EventsForTask(ctx context.Context, roomID string, sourceEventSeq int64, inputContext any, taskID string) ([]map[string]any, error) {
	var out []map[string]any
	err := c.transaction(ctx, false, func(conn state.SQLiteConn) error {
		published, err := policyPublishedTaskEvents(ctx, conn, roomID, taskID)
		if err != nil {
			return err
		}
		if inputContext != nil {
			validated, err := ValidateTaskInput(inputContext)
			if err != nil {
				return err
			}
			seqSet := map[int64]bool{sourceEventSeq: true}
			for _, item := range validated["event_seqs"].([]any) {
				seq, _ := hostedStrictInt(item)
				seqSet[seq] = true
			}
			args := []any{roomID}
			for seq := range seqSet {
				args = append(args, seq)
			}
			rows, err := state.QueryObjects(ctx, conn, "SELECT * FROM hosted_room_events WHERE room_id=? AND seq IN ("+
				strings.TrimSuffix(strings.Repeat("?,", len(seqSet)), ",")+") ORDER BY seq", args...)
			if err != nil {
				return err
			}
			if len(rows) != len(seqSet) {
				return errors.New("admitted task input event is missing")
			}
			events := map[int64]map[string]any{}
			for _, row := range rows {
				if events[hrInt(row, "seq")], err = hostedEventFromRow(row, false); err != nil {
					return err
				}
			}
			source := events[sourceEventSeq]
			threadID := policyText(source["payload"].(map[string]any), "thread_id")
			if source["kind"] != "message.user" || threadID == "" {
				return errors.New("admitted task source is not a user discussion")
			}
			latest, err := state.QueryRowObject(ctx, conn, `SELECT * FROM hosted_room_events
                    WHERE room_id=? AND kind='message.user' AND json_extract(payload_json, '$.thread_id')=?
                    ORDER BY seq DESC LIMIT 1`, roomID, threadID)
			if err != nil {
				return err
			}
			if latest != nil {
				if events[hrInt(latest, "seq")], err = hostedEventFromRow(latest, false); err != nil {
					return err
				}
			}
			for _, event := range published {
				events[event["seq"].(int64)] = event
			}
			out = policyOrdered(events)
			return nil
		}
		row, err := state.QueryRowObject(ctx, conn, "SELECT "+hostedEventColumns+" FROM hosted_room_events WHERE room_id=? AND seq=?", roomID, sourceEventSeq)
		if err != nil {
			return err
		}
		if row == nil || hrStr(row, "kind") != "message.user" {
			out = published
			return nil
		}
		source, err := hostedEventFromRow(row, false)
		if err != nil {
			return err
		}
		events, err := c.discussionEvents(ctx, conn, roomID, policyText(source["payload"].(map[string]any), "thread_id"), pyrepr.Str(source["event_id"]), "task policy projection exceeded its bound")
		if err != nil {
			return err
		}
		bySeq := map[int64]map[string]any{}
		for _, event := range append(events, published...) {
			seq, err := pyint.Int64(event["seq"])
			if err != nil {
				return err
			}
			bySeq[seq] = event
		}
		bySeq[sourceEventSeq] = source
		out = policyOrdered(bySeq)
		return nil
	})
	return out, err
}

// Drops completed projections an interrupted sync left behind
func (c *HostedRoomPolicyCheckpoint) CompactCompleted(ctx context.Context, roomID string) error {
	return c.transaction(ctx, false, func(conn state.SQLiteConn) error {
		rows, err := state.QueryObjects(ctx, conn, "SELECT discussion_event_id FROM hosted_room_policy_threads WHERE room_id=? AND completed=1", roomID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if _, err := conn.ExecContext(ctx, policyDeleteActiveEventsSQL, roomID, pyrepr.Str(row.Vals["discussion_event_id"])); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, "DELETE FROM hosted_room_policy_threads WHERE room_id=? AND completed=1", roomID)
		return err
	})
}

// Published message and terminal receipts of one discussion task, refusing a changed identity
func policyPublishedTaskEvents(ctx context.Context, conn state.SQLiteConn, roomID, taskID string) ([]map[string]any, error) {
	if taskID == "" {
		return []map[string]any{}, nil
	}
	digest := strings.TrimPrefix(taskID, "dtask:")
	rows, err := state.QueryObjects(ctx, conn, `SELECT * FROM hosted_room_events
               WHERE room_id=? AND event_id IN (?, ?) ORDER BY seq`, roomID, "dmessage:"+digest, "dterminal:"+digest)
	if err != nil {
		return nil, err
	}
	events := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		event, err := hostedEventFromRow(row, false)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	for _, event := range events {
		if payload, _ := event["payload"].(map[string]any); payload["task_id"] != taskID {
			return nil, errors.New("published task receipt identity changed")
		}
	}
	return events, nil
}

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// Turn lease lifetime a transcript writer renews with when the caller names none
const DefaultTurnLeaseTTL = 300 * time.Second

// Admission checks one transcript write runs inside its transaction
type writeGuards struct {
	compressionLockHolder string
	turnLeaseHolder       string
	turnLeaseTTL          time.Duration
	// Refuses while another holder owns a live turn lease
	rejectActiveTurnLease bool
	// Refuses while another holder owns a live compression lock
	rejectActiveCompressionLock bool
	// Lets a write land on a parent compression already closed
	allowClosedCompressionParent bool
}

// Seconds of a duration as the store stamps them
func seconds(d time.Duration) float64 { return float64(d) / float64(time.Second) }

// Holder and expiry of one lock or lease row
type leaseRow struct {
	holder  string
	expires float64
}

// Lock or lease row of a single row query, nil when none exists
func readLeaseRow(ctx context.Context, c Conn, query string, key string) (*leaseRow, error) {
	var holder sql.NullString
	var expires any
	err := c.QueryRowContext(ctx, query, key).Scan(&holder, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	at, _ := cellFloat(expires)
	return &leaseRow{holder: holder.String, expires: at}, nil
}

// Reports a lock or lease row whose holder is expired or a provably dead local process
func (s *Store) staleHolder(row *leaseRow, now float64) bool {
	return row.expires <= now || s.eng.holderProcessGone(row.holder)
}

// Reports whether the session row is ended by compression
func endedByCompression(ctx context.Context, c Conn, sessionID string) (bool, error) {
	var ended any
	var reason sql.NullString
	err := c.QueryRowContext(ctx, endedRowSQL, sessionID).Scan(&ended, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && ended != nil && reason.Valid && reason.String == "compression", err
}

// Conversation key a turn lease serializes on, the compression root of the session read on c
func sessionTurnLeaseKeyIn(ctx context.Context, c Conn, sessionID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	read := func(id string) (Session, bool, error) {
		var row Session
		err := c.QueryRowContext(ctx, "SELECT id, parent_session_id, source, model_config, end_reason FROM sessions WHERE id = ?", id).
			Scan(lenient{&row.ID}, lenient{&row.ParentSessionID}, lenient{&row.Source}, lenient{&row.ModelConfig}, lenient{&row.EndReason})
		if errors.Is(err, sql.ErrNoRows) {
			return row, false, nil
		}
		return row, err == nil, err
	}
	current, ok, err := read(sessionID)
	if err != nil || !ok {
		return sessionID, err
	}
	seen := map[string]bool{sessionID: true}
	for {
		parentID := current.ParentSessionID.V
		if parentID == "" || seen[parentID] || isExplicitForkChildRow(current, true) {
			break
		}
		parent, ok, err := read(parentID)
		if err != nil {
			return "", err
		}
		if !ok || parent.EndReason.V != "compression" {
			break
		}
		seen[parentID] = true
		current = parent
	}
	if current.ID == "" {
		return sessionID, nil
	}
	return current.ID, nil
}

// Transcript write admission checks every writer runs inside its transaction
func (s *Store) checkTranscriptWriteGuards(ctx context.Context, c Conn, sessionID string, g writeGuards) error {
	if g.rejectActiveCompressionLock {
		lock, err := readLeaseRow(ctx, c, compressionLockRowSQL, sessionID)
		if err != nil {
			return err
		}
		if lock != nil {
			if s.staleHolder(lock, nowEpoch()) {
				if _, err := c.ExecContext(ctx, deleteCompressionLockSQL, sessionID, lock.holder); err != nil {
					return err
				}
			} else if lock.holder != g.compressionLockHolder {
				return &CompressionBusyError{msg: "Session " + quotedText(sessionID) + " is being compressed by another writer", Live: true}
			}
		}
	}
	if g.turnLeaseHolder != "" || g.rejectActiveTurnLease {
		conversationID, err := sessionTurnLeaseKeyIn(ctx, c, sessionID)
		if err != nil {
			return err
		}
		lease, err := readLeaseRow(ctx, c, turnLeaseRowSQL, conversationID)
		if err != nil {
			return err
		}
		now := nowEpoch()
		if g.turnLeaseHolder != "" {
			if lease == nil || lease.holder != g.turnLeaseHolder {
				return &TurnLeaseLostError{msg: "Session turn lease lost; refusing transcript write for " + quotedText(sessionID)}
			}
			if lease.expires <= now {
				ttl := g.turnLeaseTTL
				if ttl == 0 {
					ttl = DefaultTurnLeaseTTL
				}
				if _, err := c.ExecContext(ctx, "UPDATE session_turn_leases SET expires_at = ? "+
					"WHERE conversation_id = ? AND holder = ?", now+max(0.1, seconds(ttl)), conversationID, g.turnLeaseHolder); err != nil {
					return err
				}
			}
		} else if lease != nil {
			if !s.staleHolder(lease, now) {
				return &TurnLeaseLostError{msg: "Session has an active turn lease; refusing transcript mutation for " + quotedText(sessionID)}
			}
			if _, err := c.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id = ? AND holder = ?",
				conversationID, lease.holder); err != nil {
				return err
			}
		}
	}
	closed, err := endedByCompression(ctx, c, sessionID)
	if err != nil {
		return err
	}
	if closed && !g.allowClosedCompressionParent {
		return &CompressionClosedError{SessionID: sessionID}
	}
	return nil
}

// Fencing a transcript append presents
type AppendOptions struct {
	// Compression lock the writer holds, empty when it holds none
	CompressionLockHolder string
	// Turn lease the writer must still own, empty when it writes outside a turn
	TurnLeaseHolder string
	// Lifetime an expired but still owned turn lease is renewed with, the default when zero
	TurnLeaseTTL time.Duration
	// Rows per transaction of a large copy, zero writing the whole batch in one
	ChunkRows int
}

// Appends one message and bumps the session counters, returning its row id
func (s *Store) AppendMessage(ctx context.Context, sessionID string, m Message, o AppendOptions) (int64, error) {
	msg := m
	toolCalls := parseToolCalls(msg.ToolCalls)
	timestamp := coerceTimestamp(msg.Timestamp, nowEpoch())
	if err := stampMessageUID(&msg); err != nil {
		return 0, err
	}
	if err := stampToolCallUIDs(&msg, toolCalls, map[string]string{}); err != nil {
		return 0, err
	}
	row, err := s.messageRowParams(sessionID, msg.Role, &msg, toolCalls, timestamp, true)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.Write(ctx, TranscriptWritePatience, func(c Conn) error {
		if err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{compressionLockHolder: o.CompressionLockHolder,
			turnLeaseHolder: o.TurnLeaseHolder, turnLeaseTTL: o.TurnLeaseTTL}); err != nil {
			return err
		}
		res, err := c.ExecContext(ctx, insertMessageSQL, row.insertArgs()...)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return bumpSessionCounters(ctx, c, sessionID, 1, toolCallsCount(toolCalls), true)
	})
	return id, err
}

// Appends messages in one transaction, all rows or none, stamping row ids and identities on them, returning the inserted count
func (s *Store) AppendMessages(ctx context.Context, sessionID string, messages []*Message, o AppendOptions) (int, error) {
	if len(messages) == 0 {
		return 0, nil
	}
	if o.ChunkRows > 0 && len(messages) > o.ChunkRows {
		index := map[string]string{}
		for _, m := range messages {
			if err := stampToolCallUIDs(m, parseToolCalls(m.ToolCalls), index); err != nil {
				return 0, err
			}
		}
		total := 0
		chunked := o
		chunked.ChunkRows = 0
		for start := 0; start < len(messages); start += o.ChunkRows {
			n, err := s.AppendMessages(ctx, sessionID, messages[start:min(start+o.ChunkRows, len(messages))], chunked)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	inserted := 0
	err := s.transcriptWrite(ctx, TranscriptWritePatience, messages, func(c Conn) error {
		var err error
		inserted, err = s.AppendMessagesIn(ctx, c, sessionID, messages, o)
		return err
	})
	return inserted, err
}

// Batch append inside a transaction the caller owns
func (s *Store) AppendMessagesIn(ctx context.Context, c Conn, sessionID string, messages []*Message, o AppendOptions) (int, error) {
	if err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{compressionLockHolder: o.CompressionLockHolder,
		turnLeaseHolder: o.TurnLeaseHolder, turnLeaseTTL: o.TurnLeaseTTL}); err != nil {
		return 0, err
	}
	fresh, err := s.resolveAndRepairBatch(ctx, c, sessionID, messages)
	if err != nil {
		return 0, err
	}
	inserted, toolCalls, err := s.insertMessageRows(ctx, c, sessionID, fresh, true)
	if err != nil {
		return 0, err
	}
	return inserted, bumpSessionCounters(ctx, c, sessionID, inserted, toolCalls, false)
}

// Row state of one caller owned message a rolled back attempt must put back
type rowState struct {
	rowID     int64
	snapshot  string
	timestamp sql.Null[float64]
}

// Runs a write whose callback stamps row state on the caller's messages, restoring that state before each attempt and after a failure
func (s *Store) transcriptWrite(ctx context.Context, patience time.Duration, messages []*Message, fn func(c Conn) error) error {
	saved := make([]rowState, len(messages))
	for i, m := range messages {
		saved[i] = rowState{m.RowID, m.Snapshot, m.Timestamp}
	}
	restore := func() {
		for i, m := range messages {
			m.Canonical = nil
			m.RowID, m.Snapshot, m.Timestamp = saved[i].rowID, saved[i].snapshot, saved[i].timestamp
		}
	}
	err := s.Write(ctx, patience, func(c Conn) error {
		restore()
		return fn(c)
	})
	if err != nil {
		restore()
	}
	return err
}

// Adopts the durable parent row's timestamp onto carried rows that have none, first match winning per identical row
func (s *Store) carryParentTimestamps(ctx context.Context, c Conn, parentSessionID string, messages []*Message) {
	var candidates []*Message
	for _, m := range messages {
		if m == nil {
			continue
		}
		if _, ok := trustedMessageTimestamp(m.Timestamp); !ok {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return
	}
	rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name FROM messages "+
		"WHERE session_id = ? AND active = 1 ORDER BY id", parentSessionID))
	if err != nil {
		return
	}
	donors := map[string][]any{}
	for _, r := range rows {
		key := contentKeyText(r.roleText(), r.content, nil, r.toolCallID, r.toolCalls, r.toolName)
		donors[key] = append(donors[key], r.timestamp)
	}
	for _, m := range candidates {
		content, err := m.Content.encode()
		if err != nil {
			return
		}
		var calls any
		if parsed := parseToolCalls(m.ToolCalls); jsonTruthy(parsed) {
			text, err := spacedJSON(parsed)
			if err != nil {
				return
			}
			calls = text
		}
		key := contentKeyText(m.role(), content, nil, nullText(m.ToolCallID), calls, scrubbedText(m.ToolName))
		queue := donors[key]
		if len(queue) == 0 {
			continue
		}
		donors[key] = queue[1:]
		if ts, ok := floatCell(queue[0]); ok {
			m.Timestamp = sql.Null[float64]{V: ts, Valid: true}
		}
	}
}

// Timestamp of a message when it carries a trusted one
func trustedMessageTimestamp(ts sql.Null[float64]) (float64, bool) {
	if !ts.Valid {
		return 0, false
	}
	return trustedEpoch(ts.V, "message timestamp")
}

// Inserts messages as fresh active rows in the caller's transaction, returning the inserted and tool call counts
func (s *Store) insertMessageRows(ctx context.Context, c Conn, sessionID string, messages []*Message, pruneCheckpoints bool) (int, int, error) {
	now := nowEpoch()
	inserted, toolCallsTotal := 0, 0
	index := map[string]string{}
	for _, m := range messages {
		role := m.role()
		toolCalls := parseToolCalls(m.ToolCalls)
		timestamp := coerceTimestamp(m.Timestamp, now)
		if err := stampMessageUID(m); err != nil {
			return 0, 0, err
		}
		if err := stampToolCallUIDs(m, toolCalls, index); err != nil {
			return 0, 0, err
		}
		row, err := s.messageRowParams(sessionID, role, m, toolCalls, timestamp, role == "assistant")
		if err != nil {
			return 0, 0, err
		}
		res, err := c.ExecContext(ctx, insertMessageSQL, row.insertArgs()...)
		if err != nil {
			return 0, 0, err
		}
		m.Timestamp = sql.Null[float64]{V: timestamp, Valid: true}
		if id, err := res.LastInsertId(); err == nil && id != 0 {
			m.RowID = id
		}
		inserted++
		toolCallsTotal += toolCallsCount(toolCalls)
		now = max(now, timestamp) + 1e-6
	}
	if pruneCheckpoints {
		if err := s.pruneShadowedCheckpoints(ctx, c, sessionID, messages); err != nil {
			return 0, 0, err
		}
	}
	return inserted, toolCallsTotal, stampInsertedRowSnapshots(ctx, c, sessionID, messages)
}

// Stamps the stored row digest on freshly inserted messages with one read per chunk
func stampInsertedRowSnapshots(ctx context.Context, c Conn, sessionID string, messages []*Message) error {
	byID := map[int64]*Message{}
	var ids []int64
	for _, m := range messages {
		if m.RowID > 0 {
			if _, seen := byID[m.RowID]; !seen {
				ids = append(ids, m.RowID)
			}
			byID[m.RowID] = m
		}
	}
	for _, chunk := range chunks(ids, sqlInChunk) {
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND id IN ("+bindMarks(len(chunk))+")",
			append([]any{sessionID}, int64Args(chunk)...)...))
		if err != nil {
			return err
		}
		for _, r := range rows {
			byID[r.id].Snapshot = rowSnapshot(r)
		}
	}
	return nil
}

// Bound arguments of row ids
func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// Reports a reasoning item that is a native compaction checkpoint
func isCheckpointItem(raw json.RawMessage) bool {
	members, ok := objectMembers(raw)
	if !ok {
		return false
	}
	v, _ := memberValue(members, "type")
	return len(v) > 0 && v[0] == '"' && jsonString(v) == "compaction"
}

// Reports reasoning items that hold a compaction checkpoint
func holdsCheckpoint(raw json.RawMessage) bool {
	items, ok := arrayElements(raw)
	if !ok {
		return false
	}
	for _, item := range items {
		if isCheckpointItem(item) {
			return true
		}
	}
	return false
}

// Keeps only the newest checkpoint among the inserted messages, rewriting older active assistant rows
func (s *Store) pruneShadowedCheckpoints(ctx context.Context, c Conn, sessionID string, live []*Message) error {
	for i := len(live) - 1; i >= 0; i-- {
		m := live[i]
		if m == nil || m.Role != "assistant" || !holdsCheckpoint(m.CodexReasoningItems) {
			continue
		}
		if m.RowID > 0 {
			_, err := dropShadowedCheckpointRows(ctx, c, sessionID, m.RowID)
			return err
		}
		return nil
	}
	return nil
}

// Rewrites older active assistant rows so only the carrier row keeps a compaction checkpoint, returning rows rewritten
func dropShadowedCheckpointRows(ctx context.Context, c Conn, sessionID string, carrierRowID int64) (int, error) {
	type shadowed struct {
		id  int64
		raw any
	}
	rows, err := c.QueryContext(ctx, shadowedCheckpointRowsSQL, sessionID, carrierRowID)
	if err != nil {
		return 0, err
	}
	var found []shadowed
	for rows.Next() {
		var r shadowed
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, r)
	}
	if err := rowsDone(rows); err != nil {
		return 0, err
	}
	rewritten := 0
	for _, r := range found {
		text, _ := r.raw.(string)
		if !json.Valid([]byte(text)) {
			slog.Warn("Ignoring malformed codex_reasoning_items on message row")
			continue
		}
		items, ok := arrayElements(json.RawMessage(text))
		if !ok || !holdsCheckpoint(json.RawMessage(text)) {
			continue
		}
		var kept []json.RawMessage
		for _, item := range items {
			if !isCheckpointItem(item) {
				kept = append(kept, item)
			}
		}
		var value any
		if len(kept) > 0 {
			enc, err := encodeArray(kept)
			if err != nil {
				return 0, err
			}
			value = enc
		}
		if _, err := c.ExecContext(ctx, setCodexReasoningSQL, value, r.id); err != nil {
			return 0, err
		}
		rewritten++
	}
	return rewritten, nil
}

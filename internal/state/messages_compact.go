package state

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Commit of an in-place compaction, which rows it summarizes and what rides the same transaction
type CompactOptions struct {
	// Merged into model_config in the same transaction, nil leaving the column alone
	ModelConfigPatch []ConfigMember
	// Highest active row id at compression start, rows above it surviving as a concurrent tail, nil archiving everything
	Watermark *int64
	// Compression lock the commit must still own, empty when it holds none
	LockHolder string
	// The last N compacted messages are the verbatim carried tail
	TailCount int
	// Durable originals carried forward verbatim outside a contiguous suffix
	CarriedMessages []*Message
	// Rows the compressor actually held, nil keeping the watermark path
	CoveredIDs []int64
	// Held entries without a row id, matched inside the transaction
	UnresolvedHeld []Held
	// Held messages re-verified against the live rows before any row is archived, nil on the lease path
	HeldForCAS []*Message
}

// Soft archives the active rows and inserts the compacted messages as fresh active rows atomically, returning the new active count
func (s *Store) ArchiveAndCompact(ctx context.Context, sessionID string, compacted []*Message, o CompactOptions) (int, error) {
	inserted := 0
	err := s.transcriptWrite(ctx, WritePatience, compacted, func(c Conn) error {
		var err error
		inserted, err = s.ArchiveAndCompactIn(ctx, c, sessionID, compacted, o)
		return err
	})
	return inserted, err
}

// Ids and tool call count of the rows a query over id and tool_calls selects
func tailRows(ctx context.Context, c Conn, query string, args ...any) ([]int64, int, error) {
	rows, err := queryMsgRows(c.QueryContext(ctx, query, args...))
	if err != nil {
		return nil, 0, err
	}
	ids := make([]int64, len(rows))
	calls := 0
	for i, r := range rows {
		ids[i] = r.id
		calls += toolCallsLen(r.toolCalls, 0)
	}
	return ids, calls, nil
}

// Clones rows as fresh live rows with new ids and display order, into another session when one is given
func (s *Store) cloneMessageRows(ctx context.Context, c Conn, ids []int64, sessionID string) error {
	names, err := s.messageColumnNames(ctx, c)
	if err != nil {
		return err
	}
	skip := []string{"id", "active", "compacted", "display_order"}
	if sessionID != "" {
		skip = append(skip, "session_id")
	}
	var cols []string
	for _, n := range names {
		if !containsString(skip, n) {
			cols = append(cols, n)
		}
	}
	list := strings.Join(cols, ", ")
	target, value := "", ""
	args := int64Args(ids)
	if sessionID != "" {
		target, value = "session_id, ", "?, "
		args = append([]any{sessionID}, args...)
	}
	_, err = c.ExecContext(ctx, "INSERT INTO messages ("+list+", "+target+"active, compacted) "+
		"SELECT "+list+", "+value+"1, 0 FROM messages "+
		"WHERE id IN ("+bindMarks(len(ids))+") ORDER BY id", args...)
	return err
}

// Marks rows removed from display and model history, the flags a superseded duplicate takes
func rewindRows(ctx context.Context, c Conn, sessionID string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.ExecContext(ctx, "UPDATE messages SET active = 0, compacted = 0 "+
		"WHERE session_id = ? AND id IN ("+bindMarks(len(ids))+")", append([]any{sessionID}, int64Args(ids)...)...)
	return err
}

// Ids appended in order without repeats
func appendUnique(dst []int64, ids ...int64) []int64 {
	for _, id := range ids {
		if !slices.Contains(dst, id) {
			dst = append(dst, id)
		}
	}
	return dst
}

// Writes the session counters after a compaction, with the patched model_config when one rides along
func setCompactedCounters(ctx context.Context, c Conn, sessionID string, inserted, toolCalls int, patch bool, patched any) error {
	query, args := setCountersSQL, []any{inserted, toolCalls}
	if patch {
		query += ", model_config = ?"
		args = append(args, patched)
	}
	_, err := c.ExecContext(ctx, query+" WHERE id = ?", append(args, sessionID)...)
	return err
}

// Archives the covered rows as summarized and clones every other active row after the new set
func (s *Store) archiveNamedRows(ctx context.Context, c Conn, sessionID string, compacted []*Message, covered []int64,
	mergedAway map[int64]bool, o CompactOptions, patch bool, patched any) (int, error) {
	active, err := queryInt64s(ctx, c, activeIDsSQL, sessionID)
	if err != nil {
		return 0, err
	}
	carriedIDs, err := s.resolveCarriedRowIDs(ctx, c, sessionID, o.CarriedMessages)
	if err != nil {
		return 0, err
	}
	coveredSet := map[int64]bool{}
	for _, id := range append(append([]int64{}, covered...), carriedIDs...) {
		coveredSet[id] = true
	}
	var unseen, coveredActive []int64
	for _, id := range active {
		if coveredSet[id] {
			coveredActive = append(coveredActive, id)
		} else {
			unseen = append(unseen, id)
		}
	}
	carriedUnseen, weak, err := s.carriedTailIDs(ctx, c, unseen, compacted)
	if err != nil {
		return 0, err
	}
	var cloneIDs []int64
	for _, id := range unseen {
		if _, carried := carriedUnseen[id]; !carried {
			cloneIDs = append(cloneIDs, id)
		}
	}
	tailBelow := o.TailCount - carriedTailSlots(carriedUnseen, len(compacted), o.TailCount)
	rewind := appendUnique(nil, carriedIDs...)
	if tailBelow > 0 {
		rewind = appendUnique(rewind, tailOriginals(coveredActive, tailBelow, mergedAway)...)
	}
	for _, id := range unseen {
		if !weak[id] {
			rewind = appendUnique(rewind, id)
		}
	}
	if err := rewindRows(ctx, c, sessionID, rewind); err != nil {
		return 0, err
	}
	if _, err := c.ExecContext(ctx, archiveActiveSQL, sessionID); err != nil {
		return 0, err
	}
	s.carryParentTimestamps(ctx, c, sessionID, compacted)
	inserted, toolCalls, err := s.insertMessageRows(ctx, c, sessionID, compacted, true)
	if err != nil {
		return 0, err
	}
	if len(cloneIDs) > 0 {
		_, unseenCalls, err := tailRows(ctx, c, "SELECT id, tool_calls FROM messages WHERE id IN ("+bindMarks(len(cloneIDs))+") ORDER BY id", int64Args(cloneIDs)...)
		if err != nil {
			return 0, err
		}
		if err := s.cloneMessageRows(ctx, c, cloneIDs, ""); err != nil {
			return 0, err
		}
		inserted += len(cloneIDs)
		toolCalls += unseenCalls
	}
	if err := reconcileDisplayOrders(ctx, c, sessionID); err != nil {
		return 0, err
	}
	return inserted, setCompactedCounters(ctx, c, sessionID, inserted, toolCalls, patch, patched)
}

// Compaction commit inside a transaction the caller owns
func (s *Store) ArchiveAndCompactIn(ctx context.Context, c Conn, sessionID string, compacted []*Message, o CompactOptions) (int, error) {
	if o.LockHolder != "" {
		lock, err := readLeaseRow(ctx, c, compressionLockRowSQL, sessionID)
		if err != nil {
			return 0, err
		}
		if lock == nil || lock.holder != o.LockHolder || lock.expires <= nowEpoch() {
			return 0, &CompressionBusyError{Live: true, msg: "Compression lease for " + quotedText(sessionID) +
				" lost before commit; refusing to publish a stale compaction"}
		}
	}
	patch := o.ModelConfigPatch != nil
	var patched any
	if patch {
		merged, err := mergeModelConfigIn(ctx, c, sessionID, o.ModelConfigPatch)
		if err != nil {
			return 0, err
		}
		if merged.Valid {
			patched = merged.String
		}
	}
	if len(o.HeldForCAS) > 0 {
		mismatches, err := s.HeldRowMismatchesIn(ctx, c, sessionID, o.HeldForCAS)
		if err != nil {
			return 0, err
		}
		if len(mismatches) > 0 {
			return 0, &HeldRowRewrittenError{SessionID: sessionID}
		}
	}
	covered, mergedAway, proved, err := s.provedCoverage(ctx, c, sessionID, o.CoveredIDs, o.UnresolvedHeld, o.Watermark)
	if err != nil {
		return 0, err
	}
	if proved {
		return s.archiveNamedRows(ctx, c, sessionID, compacted, covered, mergedAway, o, patch, patched)
	}
	var tailIDs []int64
	if o.Watermark != nil {
		if tailIDs, err = queryInt64s(ctx, c, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND id > ? ORDER BY id", sessionID, *o.Watermark); err != nil {
			return 0, err
		}
	}
	carriedTail, weak, err := s.carriedTailIDs(ctx, c, tailIDs, compacted)
	if err != nil {
		return 0, err
	}
	var cloneIDs []int64
	for _, id := range tailIDs {
		if _, carried := carriedTail[id]; !carried {
			cloneIDs = append(cloneIDs, id)
		}
	}
	tailCalls := 0
	if len(cloneIDs) > 0 {
		if _, tailCalls, err = tailRows(ctx, c, "SELECT id, tool_calls FROM messages WHERE id IN ("+bindMarks(len(cloneIDs))+") ORDER BY id", int64Args(cloneIDs)...); err != nil {
			return 0, err
		}
	}
	tailBelow := o.TailCount - carriedTailSlots(carriedTail, len(compacted), o.TailCount)
	carriedIDs, err := s.resolveCarriedRowIDs(ctx, c, sessionID, o.CarriedMessages)
	if err != nil {
		return 0, err
	}
	rewind := appendUnique(nil, carriedIDs...)
	if tailBelow > 0 {
		query, args := "SELECT id FROM messages WHERE session_id = ? AND active = 1", []any{sessionID}
		if o.Watermark != nil {
			query += " AND id <= ?"
			args = append(args, *o.Watermark)
		}
		below, err := queryInt64s(ctx, c, query+" ORDER BY id DESC LIMIT ?",
			append(args, tailBelow+uncountedMergedRows(compacted[max(len(compacted)-o.TailCount, 0):]))...)
		if err != nil {
			return 0, err
		}
		rewind = appendUnique(rewind, below...)
	}
	for _, id := range tailIDs {
		if !weak[id] {
			rewind = appendUnique(rewind, id)
		}
	}
	if len(rewind) > 0 {
		if err := rewindRows(ctx, c, sessionID, rewind); err != nil {
			return 0, err
		}
		if _, err := c.ExecContext(ctx, archiveActiveSQL+" AND id NOT IN ("+bindMarks(len(rewind))+")",
			append([]any{sessionID}, int64Args(rewind)...)...); err != nil {
			return 0, err
		}
	} else if _, err := c.ExecContext(ctx, archiveActiveSQL, sessionID); err != nil {
		return 0, err
	}
	s.carryParentTimestamps(ctx, c, sessionID, compacted)
	inserted, toolCalls, err := s.insertMessageRows(ctx, c, sessionID, compacted, true)
	if err != nil {
		return 0, err
	}
	if len(cloneIDs) > 0 {
		if err := s.cloneMessageRows(ctx, c, cloneIDs, ""); err != nil {
			return 0, err
		}
		inserted += len(cloneIDs)
		toolCalls += tailCalls
	}
	if err := reconcileDisplayOrders(ctx, c, sessionID); err != nil {
		return 0, err
	}
	return inserted, setCompactedCounters(ctx, c, sessionID, inserted, toolCalls, patch, patched)
}

// Ids among held messages whose active row no longer holds the payload the message holds
func (s *Store) RewrittenHeldRowIDs(ctx context.Context, sessionID string, held []*Message) ([]int64, error) {
	var out []int64
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = s.HeldRowMismatchesIn(ctx, c, sessionID, held)
		return err
	})
	return out, err
}

// Held row staleness compare on a connection the caller already holds
func (s *Store) HeldRowMismatchesIn(ctx context.Context, c Conn, sessionID string, held []*Message) ([]int64, error) {
	var exact []*Message
	byID := map[int64]*Message{}
	byUID := map[string]*Message{}
	var ids []int64
	var uids []string
	for _, m := range held {
		if m == nil || !m.Persisted || (m.Role != "user" && m.Role != "assistant" && m.Role != "tool") {
			continue
		}
		exact = append(exact, m)
		switch {
		case m.RowID > 0:
			if _, seen := byID[m.RowID]; !seen {
				ids = append(ids, m.RowID)
			}
			byID[m.RowID] = m
		case m.Snapshot != "" && m.UID != "":
			if _, seen := byUID[m.UID]; !seen {
				uids = append(uids, m.UID)
			}
			byUID[m.UID] = m
		}
	}
	if sessionID == "" || (len(byID) == 0 && len(byUID) == 0) {
		return nil, nil
	}
	stored := map[int64]*msgRow{}
	for _, chunk := range chunks(ids, sqlInChunk) {
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND id IN ("+bindMarks(len(chunk))+")",
			append([]any{sessionID}, int64Args(chunk)...)...))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			stored[r.id] = r
		}
	}
	byUIDRow := map[string]*msgRow{}
	for _, uid := range uids {
		row, err := queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND message_uid = ? "+
			"ORDER BY id DESC LIMIT 1", sessionID, byUID[uid].Role, uid))
		if err != nil {
			return nil, err
		}
		if row != nil {
			byUIDRow[uid] = row
		}
	}
	var mismatches []int64
	for _, m := range exact {
		row := byUIDRow[m.UID]
		if m.RowID > 0 {
			row = stored[m.RowID]
		}
		if row == nil || !cellTruthy(row.active) {
			continue
		}
		if m.Snapshot != "" {
			if rowSnapshot(row) != m.Snapshot {
				mismatches = append(mismatches, row.id)
			}
			continue
		}
		heldIdentity, err := liveRowIdentity(m)
		if err != nil {
			return nil, err
		}
		storedIdentity, err := storedRowIdentity(row)
		if err != nil {
			return nil, err
		}
		if heldIdentity != storedIdentity {
			mismatches = append(mismatches, row.id)
		}
	}
	return mismatches, nil
}

// One message a prune edited, as it was persisted and as the prune rewrote it
type PruneChange struct {
	Original    *Message
	Replacement *Message
}

// Fencing and extras of an in-place prune commit
type PruneOptions struct {
	// Merged into model_config in the same transaction, nil leaving the column alone
	ModelConfigPatch []ConfigMember
	// Turn lease the writer must still own, empty outside a turn
	TurnLeaseHolder string
	TurnLeaseTTL    time.Duration
}

// Columns a prune never moves, who said the message, when, and in which row
var pruneFixedColumns = []string{"session_id", "role", "tool_call_id", "timestamp", "active", "display_identity"}

// Call ids of a tool calls value, a call without a string id reading as empty
func callIDsOf(parsed json.RawMessage) []string {
	elements, ok := arrayElements(parsed)
	if !ok {
		return nil
	}
	ids := make([]string, len(elements))
	for i, e := range elements {
		if members, isObject := objectMembers(e); isObject {
			v, _ := memberValue(members, "id")
			ids[i] = jsonString(v)
		}
	}
	return ids
}

// Names the one live row of a pruned message and proves it still holds what the pruner pruned
func (s *Store) resolvePrunedRow(ctx context.Context, c Conn, sessionID string, original *Message) (*msgRow, error) {
	role := original.Role
	shown := role
	if shown == "" {
		shown = "None"
	}
	if (role != "assistant" && role != "tool") || len(original.AbsorbedRowIDs) > 0 {
		return nil, &PruneRowError{msg: fmt.Sprintf("a %s message is not rewritten in place", shown)}
	}
	var ids []int64
	var err error
	wanted := callIDsOf(parseToolCalls(original.ToolCalls))
	switch {
	case original.RowID > 0:
		ids = []int64{original.RowID}
	case role == "tool" && original.ToolCallID.V != "":
		ids, err = queryInt64s(ctx, c, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND role = 'tool' AND tool_call_id = ?",
			sessionID, original.ToolCallID.V)
	case role == "assistant" && len(wanted) > 0:
		var rows []*msgRow
		rows, err = queryMsgRows(c.QueryContext(ctx, "SELECT id, tool_calls FROM messages WHERE session_id = ? AND active = 1 AND role = 'assistant' "+
			"AND tool_calls IS NOT NULL", sessionID))
		for _, r := range rows {
			if text, ok := r.toolCalls.(string); ok && slices.Equal(callIDsOf(parseStoredJSON(text)), wanted) {
				ids = append(ids, r.id)
			}
		}
	default:
		ids, err = matchingActiveIDs(ctx, c, sessionID, original)
	}
	if err != nil {
		return nil, err
	}
	if len(ids) != 1 {
		return nil, &PruneRowError{msg: fmt.Sprintf("%d live rows match a pruned %s message", len(ids), role)}
	}
	stale := &PruneRowError{msg: fmt.Sprintf("row %d no longer holds the pruned %s message", ids[0], role), Stale: true}
	row, err := queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE id = ? AND session_id = ? AND active = 1", ids[0], sessionID))
	if err != nil {
		return nil, err
	}
	if row == nil || row.roleText() != role {
		return nil, stale
	}
	if original.Snapshot != "" {
		if rowSnapshot(row) != original.Snapshot {
			return nil, stale
		}
		return row, nil
	}
	held, err := rowIdentity(role, original.Content, nullText(original.ToolCallID), parseToolCalls(original.ToolCalls))
	if err != nil {
		return nil, err
	}
	stored, err := storedRowIdentity(row)
	if err != nil {
		return nil, err
	}
	if held != stored {
		return nil, stale
	}
	return row, nil
}

// One planned in-place row rewrite of a prune
type pruneUpdate struct {
	rowID       int64
	columns     []string
	values      []any
	identity    any
	order       int64
	replacement *Message
}

// Proactive prune commit that rewrites only the changed rows in place, each keeping one archived twin of its earlier self, returning rows rewritten
func (s *Store) RewritePrunedRows(ctx context.Context, sessionID string, changes []PruneChange, o PruneOptions) (int, error) {
	if _, err := s.ensureDisplayOrder(ctx, sessionID); err != nil {
		return 0, err
	}
	replacements := make([]*Message, len(changes))
	for i, ch := range changes {
		replacements[i] = ch.Replacement
	}
	rewritten := 0
	err := s.transcriptWrite(ctx, WritePatience, replacements, func(c Conn) error {
		rewritten = 0
		if o.TurnLeaseHolder != "" {
			if err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{turnLeaseHolder: o.TurnLeaseHolder, turnLeaseTTL: o.TurnLeaseTTL}); err != nil {
				return err
			}
		}
		patch := o.ModelConfigPatch != nil
		var patched any
		if patch {
			merged, err := mergeModelConfigIn(ctx, c, sessionID, o.ModelConfigPatch)
			if err != nil {
				return err
			}
			if merged.Valid {
				patched = merged.String
			}
		}
		var updates []pruneUpdate
		for _, ch := range changes {
			row, err := s.resolvePrunedRow(ctx, c, sessionID, ch.Original)
			if err != nil {
				return err
			}
			role := row.roleText()
			if ch.Replacement.Role != role {
				return &PruneRowError{msg: "a prune replacement changed the message role"}
			}
			ts, _ := floatCell(row.timestamp)
			before, err := s.messageRowParams(sessionID, role, ch.Original, parseToolCalls(ch.Original.ToolCalls), ts, role == "assistant")
			if err != nil {
				return err
			}
			after, err := s.messageRowParams(sessionID, role, ch.Replacement, parseToolCalls(ch.Replacement.ToolCalls), ts, role == "assistant")
			if err != nil {
				return err
			}
			u := pruneUpdate{rowID: row.id, identity: row.displayIdentity, order: row.id, replacement: ch.Replacement}
			if order, ok := cellInt(row.displayOrder); ok && row.displayOrder != nil {
				u.order = order
			}
			for _, col := range messageWriteColumns {
				if !containsString(pruneFixedColumns, col) && !cellsEqual(before.col(col), after.col(col)) {
					u.columns = append(u.columns, col)
					u.values = append(u.values, after.col(col))
				}
			}
			if len(u.columns) > 0 {
				updates = append(updates, u)
			}
		}
		names, err := s.messageColumnNames(ctx, c)
		if err != nil {
			return err
		}
		var twin []string
		for _, n := range names {
			if !containsString([]string{"id", "active", "compacted", "display_order"}, n) {
				twin = append(twin, n)
			}
		}
		twinColumns := strings.Join(twin, ", ")
		for _, u := range updates {
			if err := s.applyPruneUpdate(ctx, c, sessionID, twinColumns, u); err != nil {
				return err
			}
		}
		if patch {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET model_config = ? WHERE id = ?", patched, sessionID); err != nil {
				return err
			}
		}
		rewritten = len(updates)
		return nil
	})
	return rewritten, err
}

// Writes one planned prune rewrite, its archived twin, the changed columns and the display slots the update trigger cleared
func (s *Store) applyPruneUpdate(ctx context.Context, c Conn, sessionID, twinColumns string, u pruneUpdate) error {
	res, err := c.ExecContext(ctx, "INSERT INTO messages ("+twinColumns+", active, compacted) "+
		"SELECT "+twinColumns+", 0, 1 FROM messages WHERE id = ?", u.rowID)
	if err != nil {
		return err
	}
	twinID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	type peer struct {
		id    int64
		order int64
	}
	peers := []peer{{twinID, u.order}}
	if u.identity != nil {
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, display_order FROM messages WHERE session_id = ? AND id <> ? "+
			"AND display_identity = ? AND (active = 1 OR compacted = 1)", sessionID, u.rowID, u.identity))
		if err != nil {
			return err
		}
		peers = peers[:0]
		for _, r := range rows {
			order := u.order
			if v, ok := cellInt(r.displayOrder); ok && r.displayOrder != nil {
				order = v
			}
			peers = append(peers, peer{r.id, order})
		}
	}
	assignments := make([]string, len(u.columns))
	for i, col := range u.columns {
		assignments[i] = col + " = ?"
	}
	if _, err := c.ExecContext(ctx, "UPDATE messages SET "+strings.Join(assignments, ", ")+" WHERE id = ?", append(append([]any{}, u.values...), u.rowID)...); err != nil {
		return err
	}
	for _, p := range peers {
		if _, err := c.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ?", u.identity, p.order, p.id); err != nil {
			return err
		}
	}
	live, err := queryMsgRow(c.QueryContext(ctx, "SELECT role, content, timestamp, tool_call_id, tool_calls, tool_name, display_kind, "+
		"display_metadata FROM messages WHERE id = ?", u.rowID))
	if err != nil {
		return err
	}
	if live == nil {
		return ErrSessionRowGone
	}
	if _, err := c.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ?",
		displayIdentity(displayKeyOf(live)), u.order, u.rowID); err != nil {
		return err
	}
	final, err := queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE id = ?", u.rowID))
	if err != nil {
		return err
	}
	if final == nil {
		return ErrSessionRowGone
	}
	u.replacement.Snapshot = rowSnapshot(final)
	return nil
}

package state

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Insert statement columns in bind order so a row rewrite diffs bind values by name
var messageRowColumns = func() []string {
	inner := strings.SplitN(strings.SplitN(hermesInsertMessageSQL, "(", 2)[1], ")", 2)[0]
	var out []string
	for _, column := range strings.Split(inner, ",") {
		out = append(out, strings.TrimSpace(column))
	}
	return out
}()

// Columns a prune never moves since speaker, time and row stay put
var pruneFixedColumns = map[string]bool{"session_id": true, "role": true, "tool_call_id": true, "timestamp": true, "active": true, "display_identity": true}

// One persisted message the prune edited and the dict that replaces it
type PruneChange struct {
	Original    map[string]any
	Replacement map[string]any
}

// Keyword arguments of rewrite_pruned_rows, a nil TurnLeaseTTLSeconds meaning 300 seconds
type PruneRewriteOptions struct {
	ModelConfigPatch    *pyjson.Object
	TurnLeaseHolder     *string
	TurnLeaseTTLSeconds *float64
}

type pruneUpdate struct {
	rowID       int64
	columns     []string
	values      []any
	identity    any
	order       int64
	replacement map[string]any
}

// Ids among held dicts whose active row no longer holds the payload the dict holds
func (d *SessionDB) RewrittenHeldRowIDs(ctx context.Context, sessionID string, held []map[string]any) ([]int64, error) {
	var out []int64
	err := d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		var err error
		out, err = d.heldRowMismatches(ctx, conn, sessionID, held)
		return err
	})
	return out, err
}

type heldRowRef struct {
	message map[string]any
	rowID   int64
	exact   bool
}

// Held row compare and swap check on a held connection, by row id or by message uid with a digest
func (d *SessionDB) heldRowMismatches(ctx context.Context, conn SQLiteConn, sessionID string, held []map[string]any) ([]int64, error) {
	var refs []heldRowRef
	byID := map[int64]map[string]any{}
	var idOrder []int64
	for _, message := range held {
		if message == nil || !pyTruthy(message[DBPersistedMarker]) {
			continue
		}
		switch message["role"] {
		case "user", "assistant", "tool":
		default:
			continue
		}
		rowID, exact := namedRowID(message["_row_id"])
		refs = append(refs, heldRowRef{message, rowID, exact})
		if exact {
			if _, seen := byID[rowID]; !seen {
				idOrder = append(idOrder, rowID)
			}
			byID[rowID] = message
		}
	}
	byUID := map[string]map[string]any{}
	var uidOrder []string
	for _, ref := range refs {
		if _, isStr := ref.message[DBRowSnapshot].(string); ref.exact || !isStr {
			continue
		}
		if uid, ok := MessageUIDOrNone(ref.message); ok {
			if _, seen := byUID[uid]; !seen {
				uidOrder = append(uidOrder, uid)
			}
			byUID[uid] = ref.message
		}
	}
	if sessionID == "" || (len(byID) == 0 && len(byUID) == 0) {
		return []int64{}, nil
	}
	stored := map[int64]map[string]any{}
	for _, chunk := range idChunks(idOrder, sqlInChunk) {
		rows, err := connRows(ctx, conn, "SELECT * FROM messages WHERE session_id = ? AND id IN ("+placeholders(len(chunk))+")", append([]any{sessionID}, int64sToAny(chunk)...)...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id, _ := pyIntID(row["id"])
			stored[id] = row
		}
	}
	byUIDRow := map[string]map[string]any{}
	for _, uid := range uidOrder {
		row, err := connRow(ctx, conn, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND message_uid = ? ORDER BY id DESC LIMIT 1", sessionID, byUID[uid]["role"], uid)
		if err != nil {
			return nil, err
		}
		if row != nil {
			byUIDRow[uid] = row
			id, _ := pyIntID(row["id"])
			stored[id] = row
		}
	}
	mismatches := []int64{}
	for _, ref := range refs {
		var row map[string]any
		if ref.exact {
			row = stored[ref.rowID]
		} else {
			uid, _ := MessageUIDOrNone(ref.message)
			row = byUIDRow[uid]
		}
		if row == nil || !pyTruthy(row["active"]) {
			continue
		}
		rowID, _ := pyIntID(row["id"])
		if snapshot, ok := ref.message[DBRowSnapshot].(string); ok {
			if TranscriptRowSnapshot(row) != snapshot {
				mismatches = append(mismatches, rowID)
			}
			continue
		}
		heldIdentity, err := messageIdentity(ref.message)
		if err != nil {
			return nil, err
		}
		storedIdentity, err := storedRowIdentity(row)
		if err != nil {
			return nil, err
		}
		if !pyrepr.Equal(heldIdentity, storedIdentity) {
			mismatches = append(mismatches, rowID)
		}
	}
	return mismatches, nil
}

// Rewrites each pruned row in place behind an archived twin, returning the rows rewritten
func (d *SessionDB) RewritePrunedRows(ctx context.Context, sessionID string, changes []PruneChange, o PruneRewriteOptions) (int, error) {
	if _, err := d.ensureDisplayOrder(ctx, sessionID); err != nil {
		return 0, err
	}
	replacements := make([]map[string]any, len(changes))
	for i, change := range changes {
		replacements[i] = change.Replacement
	}
	var count int
	err := d.executeTranscriptWrite(ctx, func(conn SQLiteConn) error {
		n, err := d.rewritePrunedRowsOnConn(ctx, conn, sessionID, changes, o)
		count = n
		return err
	}, replacements, nil)
	return count, err
}

// Tool call ids of a stored or loaded tool_calls value, None for a call that is not a dict
func pruneCallIDs(toolCalls any) ([]any, error) {
	parsed := parseToolCalls(toolCalls)
	if !pyTruthy(parsed) {
		return []any{}, nil
	}
	calls, err := seqIter(parsed)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(calls))
	for i, call := range calls {
		if m, ok := dictValue(call); ok {
			out[i] = m["id"]
		}
	}
	return out, nil
}

// Exact live row a pruned dict names, refusing ambiguity and rows another writer moved on
func (d *SessionDB) resolvePrunedRow(ctx context.Context, conn SQLiteConn, sessionID string, original map[string]any) (map[string]any, error) {
	role := original["role"]
	if (role != "assistant" && role != "tool") || pyTruthy(original[AbsorbedRowIDs]) {
		return nil, &PruneRowUnresolvedError{Msg: "a " + pyStr(role) + " message is not rewritten in place"}
	}
	var ids []int64
	_, isBool := original["_row_id"].(bool)
	rowID, isInt := pyIntID(original["_row_id"])
	wanted, err := pruneCallIDs(original["tool_calls"])
	if err != nil {
		return nil, err
	}
	switch {
	case isInt && !isBool && rowID > 0:
		ids = []int64{rowID}
	case role == "tool" && pyTruthy(original["tool_call_id"]):
		rows, err := connRows(ctx, conn, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND role = 'tool' AND tool_call_id = ?",
			sessionID, original["tool_call_id"])
		if err != nil {
			return nil, err
		}
		ids = rowIDList(rows)
	case role == "assistant" && len(wanted) > 0:
		rows, err := connRows(ctx, conn, "SELECT id, tool_calls FROM messages WHERE session_id = ? AND active = 1 AND role = 'assistant' "+
			"AND tool_calls IS NOT NULL", sessionID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			got, err := pruneCallIDs(row["tool_calls"])
			if err != nil {
				return nil, err
			}
			if pyrepr.Equal(got, wanted) {
				id, _ := pyIntID(row["id"])
				ids = append(ids, id)
			}
		}
	default:
		if ids, err = matchingActiveIDs(ctx, conn, sessionID, original); err != nil {
			return nil, err
		}
	}
	if len(ids) != 1 {
		return nil, &PruneRowUnresolvedError{Msg: strconv.Itoa(len(ids)) + " live rows match a pruned " + pyStr(role) + " message"}
	}
	stale := &PruneRowStaleError{Msg: "row " + strconv.FormatInt(ids[0], 10) + " no longer holds the pruned " + pyStr(role) + " message"}
	row, err := connRow(ctx, conn, "SELECT * FROM messages WHERE id = ? AND session_id = ? AND active = 1", ids[0], sessionID)
	if err != nil {
		return nil, err
	}
	if row == nil || row["role"] != role {
		return nil, stale
	}
	if snapshot, ok := original[DBRowSnapshot].(string); ok {
		if TranscriptRowSnapshot(row) != snapshot {
			return nil, stale
		}
		return row, nil
	}
	mine, err := rowIdentity(role, original["content"], original["tool_call_id"], parseToolCalls(original["tool_calls"]))
	if err != nil {
		return nil, err
	}
	theirs, err := rowIdentity(row["role"], decodeMessageContent(row["content"]), row["tool_call_id"], parseToolCalls(row["tool_calls"]))
	if err != nil {
		return nil, err
	}
	if !pyrepr.Equal(mine, theirs) {
		return nil, stale
	}
	return row, nil
}

// Body of the in place prune commit on the held write connection
func (d *SessionDB) rewritePrunedRowsOnConn(ctx context.Context, conn SQLiteConn, sessionID string, changes []PruneChange, o PruneRewriteOptions) (int, error) {
	if o.TurnLeaseHolder != nil && *o.TurnLeaseHolder != "" {
		if err := d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{turnLeaseHolder: o.TurnLeaseHolder,
			turnLeaseTTLSeconds: o.TurnLeaseTTLSeconds}); err != nil {
			return 0, err
		}
	}
	patch := o.ModelConfigPatch != nil
	var patched any
	if patch {
		value, _, err := mergeModelConfigJSON(ctx, conn, sessionID, o.ModelConfigPatch, true)
		if err != nil {
			return 0, err
		}
		patched = value
	}
	var updates []pruneUpdate
	for _, change := range changes {
		row, err := d.resolvePrunedRow(ctx, conn, sessionID, change.Original)
		if err != nil {
			return 0, err
		}
		role, _ := row["role"].(string)
		if change.Replacement["role"] != role {
			return 0, &PruneRowUnresolvedError{Msg: "a prune replacement changed the message role"}
		}
		timestamp, err := pyFloatOf(row["timestamp"])
		if err != nil {
			return 0, err
		}
		before, err := messageRowParams(sessionID, role, change.Original, parseToolCalls(change.Original["tool_calls"]), timestamp, role == "assistant")
		if err != nil {
			return 0, err
		}
		after, err := messageRowParams(sessionID, role, change.Replacement, parseToolCalls(change.Replacement["tool_calls"]), timestamp, role == "assistant")
		if err != nil {
			return 0, err
		}
		var columns []string
		var values []any
		for i, column := range messageRowColumns {
			if i < len(before) && i < len(after) && !pyrepr.Equal(before[i], after[i]) && !pruneFixedColumns[column] {
				columns = append(columns, column)
				values = append(values, after[i])
			}
		}
		if len(columns) == 0 {
			continue
		}
		id, _ := pyIntID(row["id"])
		order := id
		if row["display_order"] != nil {
			order, _ = pyIntID(row["display_order"])
		}
		updates = append(updates, pruneUpdate{id, columns, values, row["display_identity"], order, change.Replacement})
	}
	names, err := d.messageColumnNames(ctx, conn)
	if err != nil {
		return 0, err
	}
	var twin []string
	for _, c := range names {
		if c != "id" && c != "active" && c != "compacted" && c != "display_order" {
			twin = append(twin, c)
		}
	}
	twinColumns := strings.Join(twin, ", ")
	for _, u := range updates {
		res, err := conn.ExecContext(ctx, "INSERT INTO messages ("+twinColumns+", active, compacted) SELECT "+twinColumns+", 0, 1 FROM messages WHERE id = ?", u.rowID)
		if err != nil {
			return 0, err
		}
		twinID, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		peers := []map[string]any{{"id": twinID, "display_order": u.order}}
		if u.identity != nil {
			if peers, err = connRows(ctx, conn, "SELECT id, display_order FROM messages WHERE session_id = ? AND id <> ? "+
				"AND display_identity = ? AND (active = 1 OR compacted = 1)", sessionID, u.rowID, u.identity); err != nil {
				return 0, err
			}
		}
		sets := make([]string, len(u.columns))
		for i, column := range u.columns {
			sets[i] = column + " = ?"
		}
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(append([]any{}, u.values...), u.rowID)...); err != nil {
			return 0, err
		}
		for _, peer := range peers {
			slot := peer["display_order"]
			if slot == nil {
				slot = u.order
			}
			if _, err := conn.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ?", u.identity, slot, peer["id"]); err != nil {
				return 0, err
			}
		}
		live, err := connRow(ctx, conn, "SELECT role, content, timestamp, tool_call_id, tool_calls, tool_name, display_kind, "+
			"display_metadata FROM messages WHERE id = ?", u.rowID)
		if err != nil {
			return 0, err
		}
		key, err := displayDedupeKey(live)
		if err != nil {
			return 0, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ?", displayIdentityOf(key), u.order, u.rowID); err != nil {
			return 0, err
		}
		final, err := connRow(ctx, conn, "SELECT * FROM messages WHERE id = ?", u.rowID)
		if err != nil {
			return 0, err
		}
		u.replacement[DBRowSnapshot] = TranscriptRowSnapshot(final)
	}
	if patch {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET model_config = ? WHERE id = ?", patched, sessionID); err != nil {
			return 0, err
		}
	}
	return len(updates), nil
}

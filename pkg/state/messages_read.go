package state

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var displayIndexMissingSQL = "SELECT 1 FROM messages WHERE session_id = ?" + displayActiveClause +
	" AND (display_order IS NULL OR display_identity IS NULL) LIMIT 1"

// Message table column names read once per store instance
func (d *SessionDB) messageColumnNames(ctx context.Context, conn SQLiteConn) ([]string, error) {
	d.messageColumnsMu.Lock()
	defer d.messageColumnsMu.Unlock()
	if len(d.messageColumnsCache) > 0 {
		return d.messageColumnsCache, nil
	}
	rows, err := queryObjects(ctx, conn, "PRAGMA table_info(messages)")
	if err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		cols = append(cols, pyStr(rowValue(r, "name")))
	}
	d.messageColumnsCache = cols
	return cols, nil
}

// Python slice items[offset:][:limit] with negative bounds counted from the end
func pyPage[T any](items []T, offset int, limit *int) []T {
	n := len(items)
	if offset < 0 {
		offset = max(0, n+offset)
	}
	items = items[min(offset, n):]
	if limit != nil {
		end := *limit
		if end < 0 {
			end = max(0, len(items)+end)
		}
		items = items[:min(end, len(items))]
	}
	return items
}

// Page taken from the newest end when latest, returned in ascending order
func pyLatestPage[T any](items []T, offset int, limit *int, latest bool) []T {
	if !latest {
		return pyPage(items, offset, limit)
	}
	rev := slices.Clone(items)
	slices.Reverse(rev)
	rev = slices.Clone(pyPage(rev, offset, limit))
	slices.Reverse(rev)
	return rev
}

// Restamps display order and identity for every visible row of a session in id order batches
func (d *SessionDB) reconcileDisplayOrders(ctx context.Context, conn SQLiteConn, sessionID string) error {
	firstID := map[string]any{}
	uidIdentity := map[string]string{}
	var lastID int64
	for {
		rows, err := queryObjects(ctx, conn,
			"SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name, "+
				"display_kind, display_metadata, display_order, display_identity, message_uid, _compressed_summary "+
				"FROM messages INDEXED BY idx_messages_session_id "+
				"WHERE session_id = ? AND id > ? AND (active = 1 OR compacted = 1) "+
				"ORDER BY id LIMIT 1000", sessionID, lastID)
		if err != nil {
			return err
		}
		batchStart := lastID
		type update struct {
			order    any
			identity []byte
			id       int64
		}
		var updates []update
		for _, r := range rows {
			row := objMap(r)
			lastID, _ = pyIntID(row["id"])
			key, err := displayDedupeKey(row)
			if err != nil {
				return err
			}
			identity := foldSummaryCarrier(row, string(displayIdentityOf(key)), uidIdentity)
			stored, _ := row["display_identity"].([]byte)
			var kept any
			if stored != nil && bytes.Equal(stored, []byte(identity)) {
				kept = row["display_order"]
			}
			order, ok := firstID[identity]
			if !ok {
				order = kept
				if kept == nil {
					order = lastID
				}
				firstID[identity] = order
			}
			if !pyValueEqual(order, row["display_order"]) || stored == nil || !bytes.Equal(stored, []byte(identity)) {
				updates = append(updates, update{order, []byte(identity), lastID})
			}
		}
		if lastID == batchStart {
			return nil
		}
		for _, u := range updates {
			if _, err := conn.ExecContext(ctx, "UPDATE messages SET display_order = ?, display_identity = ? WHERE id = ?", u.order, u.identity, u.id); err != nil {
				return err
			}
		}
	}
}

// Whether the display index of a session is complete, rebuilding it when the store is writable
func (d *SessionDB) ensureDisplayOrder(ctx context.Context, sessionID string) (bool, error) {
	var cols []string
	if err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		cols, err = d.messageColumnNames(ctx, conn)
		return err
	}); err != nil {
		return false, err
	}
	if !slices.Contains(cols, "display_order") || !slices.Contains(cols, "display_identity") {
		return false, nil
	}
	missing, err := d.readOne(ctx, displayIndexMissingSQL, sessionID)
	if err != nil {
		return false, err
	}
	if missing == nil {
		return true, nil
	}
	if d.isReadOnly {
		return false, nil
	}
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, displayIndexMissingSQL, sessionID)
		if err != nil || len(rows) == 0 {
			return err
		}
		return d.reconcileDisplayOrders(ctx, conn, sessionID)
	}, nil)
	return err == nil, err
}

// Display page of a session without the display index, newest live copy of each identity chosen
func (d *SessionDB) legacyDisplayPage(ctx context.Context, sessionID, activeClause string, limit *int, offset int, latest bool) ([]*pyjson.Object, error) {
	var out []*pyjson.Object
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		idx, err := queryObjects(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?", "idx_messages_session_id")
		if err != nil {
			return err
		}
		hint := "NOT INDEXED"
		if len(idx) > 0 {
			hint = "INDEXED BY idx_messages_session_id"
		}
		rows, err := queryObjects(ctx, conn, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name, active, "+
			"display_kind, display_metadata FROM messages "+hint+" WHERE session_id = ?"+activeClause+" ORDER BY id ASC", sessionID)
		if err != nil {
			return err
		}
		type rep struct{ active, id int64 }
		reps := map[string]rep{}
		var order []string
		for _, r := range rows {
			if isModelOnlyRow(r) {
				continue
			}
			key, err := displayDedupeKey(objMap(r))
			if err != nil {
				return err
			}
			identity := string(displayIdentityOf(key))
			active, _ := pyIntID(rowValue(r, "active"))
			id, _ := pyIntID(rowValue(r, "id"))
			cur, ok := reps[identity]
			if !ok {
				order = append(order, identity)
			}
			if !ok || active > cur.active || (active == cur.active && id > cur.id) {
				reps[identity] = rep{active, id}
			}
		}
		order = pyLatestPage(order, offset, limit, latest)
		selectedIDs := make([]int64, len(order))
		for i, identity := range order {
			selectedIDs[i] = reps[identity].id
		}
		selected := map[int64]*pyjson.Object{}
		for start := 0; start < len(selectedIDs); start += 900 {
			chunk := selectedIDs[start:min(start+900, len(selectedIDs))]
			args := []any{sessionID}
			for _, id := range chunk {
				args = append(args, id)
			}
			got, err := queryObjects(ctx, conn, "SELECT * FROM messages WHERE session_id = ?"+activeClause+
				" AND id IN ("+placeholders(len(chunk))+")", args...)
			if err != nil {
				return err
			}
			for _, r := range got {
				id, _ := pyIntID(rowValue(r, "id"))
				selected[id] = r
			}
		}
		out = nil
		for _, id := range selectedIDs {
			if r, ok := selected[id]; ok {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// One display page through the display index, live then newest copy per display order
func displayRowsFromConn(ctx context.Context, conn SQLiteConn, sessionID string, limit *int, offset int, latest bool) ([]*pyjson.Object, error) {
	direction := "ASC"
	if latest {
		direction = "DESC"
	}
	lim := -1
	if limit != nil {
		lim = *limit
	}
	return queryObjects(ctx, conn, `WITH page AS (
                   SELECT display_order FROM messages
                   WHERE session_id = ? AND (active = 1 OR compacted = 1)`+DisplayVisibleSQL+notALaterToolCopySQL("messages")+`
                   GROUP BY display_order ORDER BY display_order `+direction+`
                   LIMIT ? OFFSET ?
               )
               SELECT chosen.* FROM page
               JOIN messages AS chosen ON chosen.id = (
                   SELECT candidate.id FROM messages AS candidate
                   WHERE candidate.session_id = ?
                     AND candidate.display_order = page.display_order
                     AND (candidate.active = 1 OR candidate.compacted = 1)`+DisplayVisibleSQL+notALaterToolCopySQL("candidate")+`
                   ORDER BY candidate.active DESC, candidate.id DESC LIMIT 1
               )
               ORDER BY page.display_order ASC`, sessionID, lim, offset, sessionID)
}

// Display messages of a session on conn, false when the session is gone or its index incomplete
func (d *SessionDB) displayMessagesFromConn(ctx context.Context, conn SQLiteConn, sessionID string) ([]*pyjson.Object, bool, error) {
	exists, err := queryObjects(ctx, conn, "SELECT 1 FROM sessions WHERE id = ? LIMIT 1", sessionID)
	if err != nil || len(exists) == 0 {
		return nil, false, err
	}
	missing, err := queryObjects(ctx, conn, displayIndexMissingSQL, sessionID)
	if err != nil || len(missing) > 0 {
		return nil, false, err
	}
	rows, err := displayRowsFromConn(ctx, conn, sessionID, nil, 0, false)
	if err != nil {
		return nil, false, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, r := range rows {
		m, err := d.rowToMessageDict(r, "verified delete", true)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	return out, true, nil
}

// Paging and visibility of a message read, AfterID nil standing for None
type GetMessagesOptions struct {
	IncludeInactive, IncludeCompacted bool
	Limit                             *int
	Offset                            int
	Latest                            bool
	AfterID                           *int64
	IncludeAncestors                  bool
}

// Message dicts of a session page under the requested visibility, deduped for display reads
func (d *SessionDB) GetMessages(ctx context.Context, sessionID string, o GetMessagesOptions) ([]*pyjson.Object, error) {
	if o.AfterID != nil && (o.Latest || o.Offset != 0) {
		return nil, pyValueError("after_id is incompatible with latest/offset paging")
	}
	if o.AfterID != nil && o.IncludeCompacted {
		return nil, pyValueError("after_id is incompatible with include_compacted (deduped display reads use offset paging)")
	}
	if o.AfterID != nil && o.IncludeAncestors {
		return nil, pyValueError("after_id is incompatible with include_ancestors (merged-lineage reads use offset paging)")
	}
	clause := activeClause(o.IncludeInactive, o.IncludeCompacted)
	sessionIDs := []string{sessionID}
	if o.IncludeAncestors {
		var err error
		if sessionIDs, err = d.resumeLineageIDs(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	var rows []*pyjson.Object
	var err error
	display := o.IncludeCompacted && !o.IncludeInactive
	switch {
	case len(sessionIDs) > 1:
		if rows, err = d.readAll(ctx, "SELECT * FROM messages WHERE session_id IN ("+placeholders(len(sessionIDs))+")"+clause+" ORDER BY id ASC", stringArgs(sessionIDs)...); err != nil {
			return nil, err
		}
		if display {
			if rows, err = d.withRemovedIdentities(ctx, rows, sessionIDs); err != nil {
				return nil, err
			}
		}
		if rows, err = dedupeDisplayGenerations(rows, display); err != nil {
			return nil, err
		}
		rows = pyLatestPage(rows, o.Offset, o.Limit, o.Latest)
	case display:
		ready, err := d.ensureDisplayOrder(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if ready {
			err = d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
				var err error
				rows, err = displayRowsFromConn(ctx, conn, sessionID, o.Limit, o.Offset, o.Latest)
				return err
			})
		} else {
			rows, err = d.legacyDisplayPage(ctx, sessionID, clause, o.Limit, o.Offset, o.Latest)
		}
		if err != nil {
			return nil, err
		}
	case o.IncludeCompacted:
		if rows, err = d.legacyDisplayPage(ctx, sessionID, clause, o.Limit, o.Offset, o.Latest); err != nil {
			return nil, err
		}
	default:
		query := "SELECT * FROM messages WHERE session_id = ?" + clause
		args := []any{sessionID}
		if o.AfterID != nil {
			query += " AND id > ?"
			args = append(args, *o.AfterID)
		}
		if o.Latest {
			query += " ORDER BY id DESC"
		} else {
			query += " ORDER BY id ASC"
		}
		if o.Limit != nil || o.Offset != 0 {
			lim := -1
			if o.Limit != nil {
				lim = *o.Limit
			}
			query += " LIMIT ? OFFSET ?"
			args = append(args, lim, o.Offset)
		}
		if rows, err = d.readAll(ctx, query, args...); err != nil {
			return nil, err
		}
		if o.Latest {
			slices.Reverse(rows)
		}
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, r := range rows {
		m, err := d.rowToMessageDict(r, "get_messages", true)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// A ValueError carrying hermes' message
type stateValueError struct{ msg string }

func (e *stateValueError) Error() string      { return e.msg }
func (e *stateValueError) PyTypeName() string { return "ValueError" }

func pyValueError(msg string) error { return &stateValueError{msg} }

// Conversation rows of the sessions, removed row identities merged in for display reads
func (d *SessionDB) fetchConversationRows(ctx context.Context, sessionIDs []string, clause string, withSessionID, withCompacted bool) ([]*pyjson.Object, error) {
	head := ""
	if withSessionID || withCompacted {
		head = "session_id, "
	}
	if withCompacted {
		head += "compacted, display_order, "
		clause = displayActiveClause
	}
	rows, err := d.readAll(ctx, "SELECT "+head+conversationRowColumns+" FROM messages WHERE session_id IN ("+
		placeholders(len(sessionIDs))+")"+clause+" ORDER BY id", stringArgs(sessionIDs)...)
	if err != nil || !withCompacted {
		return rows, err
	}
	return d.withRemovedIdentities(ctx, rows, sessionIDs)
}

// Rows merged in id order with the identities of removed rows, content kept only for unindexed ones
func (d *SessionDB) withRemovedIdentities(ctx context.Context, rows []*pyjson.Object, sessionIDs []string) ([]*pyjson.Object, error) {
	removed, err := d.readAll(ctx, "SELECT session_id, id, role, timestamp, tool_call_id, tool_calls, tool_name, display_kind, display_metadata, "+
		"message_uid, absorbed_message_uids, display_identity, active, compacted, "+
		"CASE WHEN display_identity IS NULL THEN content END AS content "+
		"FROM messages WHERE session_id IN ("+placeholders(len(sessionIDs))+") AND active = 0 AND compacted = 0 "+
		"ORDER BY id", stringArgs(sessionIDs)...)
	if err != nil {
		return nil, err
	}
	out := append(slices.Clone(rows), removed...)
	if len(removed) > 0 {
		slices.SortStableFunc(out, func(a, b *pyjson.Object) int {
			x, _ := pyIntID(rowValue(a, "id"))
			y, _ := pyIntID(rowValue(b, "id"))
			return int(x - y)
		})
	}
	return out, nil
}

// Visibility and repair switches of a conversation read
type ConversationOptions struct {
	IncludeAncestors, IncludeInactive, RepairAlternation, IncludeRowIDs, IncludeCompacted bool
}

// Provider shaped conversation of a session, optionally merged across its lineage and repaired
func (d *SessionDB) GetMessagesAsConversation(ctx context.Context, sessionID string, o ConversationOptions) ([]map[string]any, error) {
	display := o.IncludeCompacted && !o.IncludeInactive
	ids := []string{sessionID}
	if o.IncludeAncestors {
		var err error
		if ids, err = d.resumeLineageIDs(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	rows, err := d.fetchConversationRows(ctx, ids, activeClause(o.IncludeInactive, o.IncludeCompacted), false, display)
	if err != nil {
		return nil, err
	}
	if o.IncludeCompacted {
		if rows, err = dedupeDisplayGenerations(rows, display); err != nil {
			return nil, err
		}
	}
	return d.rowsToConversation(rows, sessionID, o.IncludeAncestors, o.RepairAlternation, o.IncludeRowIDs, o.RepairAlternation)
}

// Revision tuple of a transcript, its lineage ids, active count, newest id and summed revisions
type TranscriptRevision struct {
	SessionIDs             []string
	Count, MaxID, Revision int64
}

// Revision of the active transcript, changing whenever a row lands, leaves or is rewritten
func (d *SessionDB) ConversationRevision(ctx context.Context, sessionID string, includeAncestors bool) (TranscriptRevision, error) {
	ids := []string{sessionID}
	if includeAncestors {
		var err error
		if ids, err = d.resumeLineageIDs(ctx, sessionID); err != nil {
			return TranscriptRevision{}, err
		}
	}
	marks := placeholders(len(ids))
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n, COALESCE(MAX(id), 0) AS max_id, (SELECT COALESCE(SUM(transcript_revision), 0) FROM sessions"+
		" WHERE id IN ("+marks+")) AS revision FROM messages WHERE session_id IN ("+marks+") AND active = 1", append(stringArgs(ids), stringArgs(ids)...)...)
	if err != nil {
		return TranscriptRevision{}, err
	}
	rev := TranscriptRevision{SessionIDs: ids}
	for _, f := range []struct {
		key string
		dst *int64
	}{{"n", &rev.Count}, {"max_id", &rev.MaxID}, {"revision", &rev.Revision}} {
		n, err := pyIntOf(rowValue(row, f.key))
		if err != nil {
			return TranscriptRevision{}, err
		}
		*f.dst = n.Int64()
	}
	return rev, nil
}

// Comparable content of a replayed user turn, its live part when it carries a summary handoff
func canonicalReplayedUserContent(msg map[string]any) (any, bool) {
	if msg["role"] != "user" {
		return nil, false
	}
	handoff, live := SplitUserOriginatedTurn(msg)
	if handoff != nil && live != nil {
		return live["content"], true
	}
	return msg["content"], false
}

// Whether content is None, an empty string or an empty list
func emptyReplayContent(content any) bool {
	if content == nil || content == "" {
		return true
	}
	items, ok := listItems(content)
	return ok && len(items) == 0
}

// Exact clone key of a replayed user turn from its timestamp and canonical JSON, false when unkeyable
func exactReplayedUserCloneKey(timestamp, content any) (string, bool) {
	if timestamp == nil || emptyReplayContent(content) {
		return "", false
	}
	text, err := pyjson.Dumps(content, pyjson.EnsureASCII(false), pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return "", false
	}
	ts := pyReprValue(timestamp)
	switch x := timestamp.(type) {
	case int64:
		ts = strconv.FormatFloat(float64(x), 'g', -1, 64)
	case int:
		ts = strconv.FormatFloat(float64(x), 'g', -1, 64)
	case float64:
		ts = strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ts + "\x00" + text, true
}

// Index of an earlier copy of a replayed user turn before the last answered assistant turn
func findDuplicateReplayedUserMessage(messages []map[string]any, msg map[string]any) (int, bool, bool) {
	if msg["role"] != "user" {
		return 0, false, false
	}
	content, preferCurrent := canonicalReplayedUserContent(msg)
	if emptyReplayContent(content) {
		return 0, false, false
	}
	_, isStr := content.(string)
	for i := len(messages) - 1; i >= 0; i-- {
		prev := messages[i]
		switch prev["role"] {
		case "user":
			prevContent, prevComposite := canonicalReplayedUserContent(prev)
			if pyValueEqual(prevContent, content) && (preferCurrent || prevComposite || isStr) {
				return i, preferCurrent, true
			}
		case "assistant":
			if seqTruthy(prev["content"]) || seqTruthy(prev["tool_calls"]) {
				return 0, false, false
			}
		}
	}
	return 0, false, false
}

// Whether two message dicts are the same object
func sameMessage(a, b map[string]any) bool {
	return a != nil && b != nil && reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// Drops or skips a replayed user copy, reporting whether msg is skipped and its exact clone key
func dedupeReplayedUser(messages *[]map[string]any, msg map[string]any, clones map[string]map[string]any) (bool, string, bool) {
	content, _ := canonicalReplayedUserContent(msg)
	key, keyed := exactReplayedUserCloneKey(msg["timestamp"], content)
	index, preferCurrent, found := 0, false, false
	if keyed {
		if previous, ok := clones[key]; ok {
			for i, candidate := range *messages {
				if sameMessage(candidate, previous) {
					index, preferCurrent, found = i, true, true
					break
				}
			}
		}
	}
	if !found {
		index, preferCurrent, found = findDuplicateReplayedUserMessage(*messages, msg)
	}
	if !found {
		return false, key, keyed
	}
	if preferCurrent {
		*messages = slices.Delete(*messages, index, index+1)
	}
	return !preferCurrent, key, keyed
}

// Conversation dicts restored from stored rows with identity, sidecars and tool uid pairing
func (d *SessionDB) rowsToConversation(rows []*pyjson.Object, sessionID string, includeAncestors, repairAlternation, includeRowIDs, includeSummaryMarkers bool) ([]map[string]any, error) {
	stampSnapshot := repairAlternation && !includeRowIDs
	var messages []map[string]any
	clones := map[string]map[string]any{}
	toolUIDIndex := map[string]string{}
	var owners []map[string]any
	for _, r := range rows {
		row := objMap(r)
		role := row["role"]
		roleText, _ := role.(string)
		msg := map[string]any{"role": role, "content": loadedViewContent(roleText, decodeMessageContent(row["content"])), DBPersistedMarker: true}
		if stampSnapshot {
			msg[DBRowSnapshot] = TranscriptRowSnapshot(row)
		}
		if includeRowIDs && row["id"] != nil {
			msg["_row_id"] = row["id"]
		}
		restoreIdentityColumns(row, msg)
		for _, col := range []string{"api_content", "display_kind"} {
			if pyTruthy(row[col]) {
				msg[col] = row[col]
			}
		}
		if pyTruthy(row["display_metadata"]) {
			if decoded := decodeDisplayMetadata(row["display_metadata"]); decoded != nil {
				msg["display_metadata"] = decoded
				LiftDataUse(msg)
			}
		}
		if includeSummaryMarkers && pyTruthy(row["_compressed_summary"]) {
			msg["_compressed_summary"] = true
		}
		for _, col := range []string{"timestamp", "tool_call_id", "tool_name", "effect_disposition"} {
			if pyTruthy(row[col]) {
				msg[col] = row[col]
			}
		}
		if pyTruthy(row["tool_calls"]) {
			calls, err := jsonOr(row["tool_calls"], []any{}, "Failed to deserialize tool_calls in conversation replay, falling back to []")
			if err != nil {
				return nil, err
			}
			msg["tool_calls"] = calls
		}
		if pyTruthy(row["platform_message_id"]) {
			msg["message_id"] = row["platform_message_id"]
		}
		if pyTruthy(row["observed"]) {
			msg["observed"] = true
		}
		switch role {
		case "assistant":
			for _, col := range []string{"finish_reason", "reasoning"} {
				if pyTruthy(row[col]) {
					msg[col] = row[col]
				}
			}
			if row["reasoning_content"] != nil {
				msg["reasoning_content"] = row["reasoning_content"]
			}
			for _, col := range []string{"reasoning_details", "codex_reasoning_items", "codex_message_items"} {
				if pyTruthy(row[col]) {
					v, err := jsonOr(row[col], nil, fmt.Sprintf("Failed to deserialize %s, falling back to None", col))
					if err != nil {
						return nil, err
					}
					msg[col] = v
				}
			}
			if seqTruthy(msg["tool_calls"]) {
				owners = append(owners, msg)
			}
		case "user":
			clear(toolUIDIndex)
			owners = owners[:0]
		case "tool":
			if !pyTruthy(row["tool_call_uid"]) && pyTruthy(row["tool_call_id"]) {
				for _, owner := range owners {
					IndexToolCallUIDs(toolUIDIndex, owner)
				}
				owners = owners[:0]
				if uid, ok := ResolveToolCallUID(toolUIDIndex, row["tool_call_id"]); ok && uid != "" {
					msg[ToolCallUID] = uid
				}
			}
		}
		if includeAncestors {
			skip, key, keyed := dedupeReplayedUser(&messages, msg, clones)
			if skip {
				continue
			}
			if keyed {
				clones[key] = msg
			}
		}
		messages = append(messages, msg)
	}
	items := make([]any, len(messages))
	for i, m := range messages {
		items[i] = m
	}
	stripped, err := stripBackgroundReviewHarness(items)
	if err != nil {
		return nil, err
	}
	stripped = stripStaleToolCallMarkers(stripped)
	messages = make([]map[string]any, 0, len(stripped))
	for _, item := range stripped {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("TypeError: restored message is %T, not a dict", item)
		}
		messages = append(messages, m)
	}
	if repairAlternation && len(messages) > 0 {
		repaired, err := RepairMessageSequence(&messages)
		if err != nil {
			return nil, err
		}
		if repaired > 0 {
			slog.Info(fmt.Sprintf("Repaired %d message-alternation violation(s) while restoring session %s — durable transcript kept them, see repair_message_sequence", repaired, sessionID))
		}
	}
	return messages, nil
}

// Compression chain from the session through its live tip, root first
func walkCompressionChain(sessionID string, step func(string) (*pyjson.Object, error)) ([]string, error) {
	current := sessionID
	var chain []string
	if current != "" {
		chain = []string{current}
	}
	seen := map[string]bool{current: current != ""}
	for range chainCap {
		row, err := step(current)
		if err != nil {
			return nil, err
		}
		child := pyStr(rowValue(row, "id"))
		if row == nil || rowValue(row, "id") == nil || child == "" || seen[child] {
			return chain, nil
		}
		seen[child] = true
		current = child
		chain = append(chain, child)
	}
	return chain, nil
}

// Compression continuation chain of a session, root first through the tip
func (d *SessionDB) GetCompressionChain(ctx context.Context, sessionID string) ([]string, error) {
	return walkCompressionChain(sessionID, func(current string) (*pyjson.Object, error) {
		return d.readOne(ctx, chainStepSQL, current)
	})
}

// Live tip of a compression chain, the input id when no continuation exists
func (d *SessionDB) GetCompressionTip(ctx context.Context, sessionID string) (string, error) {
	chain, err := d.GetCompressionChain(ctx, sessionID)
	if err != nil || len(chain) == 0 {
		return sessionID, err
	}
	return chain[len(chain)-1], nil
}

// Compression tip walked on an open connection
func compressionTipOn(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	chain, err := walkCompressionChain(sessionID, func(current string) (*pyjson.Object, error) {
		rows, err := queryObjects(ctx, conn, chainStepSQL, current)
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		return rows[0], nil
	})
	if err != nil {
		return "", err
	}
	return chain[len(chain)-1], nil
}

// Deepest descendant with messages along the newest plain continuation children, the input when a lookup fails
func (d *SessionDB) ResolveResumeSessionID(sessionID string) (string, error) {
	ctx := context.Background()
	if sessionID == "" {
		return sessionID, nil
	}
	if tip, err := d.GetCompressionTip(ctx, sessionID); err == nil && tip != "" {
		sessionID = tip
	}
	result := sessionID
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		current := sessionID
		seen := map[string]bool{current: true}
		best := ""
		for range 32 {
			has, err := queryObjects(ctx, conn, "SELECT 1 FROM messages WHERE session_id = ? LIMIT 1", current)
			if err != nil {
				result = sessionID
				return nil
			}
			if len(has) > 0 {
				best = current
			}
			child, err := queryObjects(ctx, conn, "SELECT id FROM sessions AS child WHERE child.parent_session_id = ? "+
				"  AND "+sqlJSONExtract("child.model_config", "$._branched_from")+" IS NULL "+
				"  AND "+sqlJSONExtract("child.model_config", "$._delegate_from")+" IS NULL "+
				"  AND "+sqlJSONExtract("child.model_config", "$._reset_from")+" IS NULL "+
				"  AND NOT "+legacyResetChildSQL("child", resetEndReasonsSQL)+" "+
				"  AND COALESCE(child.source, '') != 'tool' "+
				"ORDER BY child.started_at DESC, child.id DESC LIMIT 1", current)
			if err != nil {
				result = sessionID
				return nil
			}
			if len(child) == 0 || !pyTruthy(rowValue(child[0], "id")) || seen[pyStr(rowValue(child[0], "id"))] {
				break
			}
			current = pyStr(rowValue(child[0], "id"))
			seen[current] = true
		}
		if best != "" {
			result = best
		}
		return nil
	})
	return result, err
}

// Model history of the tip and display history of the whole lineage for a resume
func (d *SessionDB) GetResumeConversations(ctx context.Context, sessionID string) ([]map[string]any, []map[string]any, error) {
	ids, err := d.resumeLineageIDs(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := d.fetchConversationRows(ctx, ids, "", true, true)
	if err != nil {
		return nil, nil, err
	}
	var tip []*pyjson.Object
	for _, r := range rows {
		if rowValue(r, "session_id") == sessionID && pyTruthy(rowValue(r, "active")) && rowValue(r, "role") != "session_meta" {
			tip = append(tip, r)
		}
	}
	model, err := d.rowsToConversation(tip, sessionID, false, true, true, true)
	if err != nil {
		return nil, nil, err
	}
	deduped, err := dedupeDisplayGenerations(rows, true)
	if err != nil {
		return nil, nil, err
	}
	display, err := d.rowsToConversation(deduped, sessionID, true, false, true, false)
	return model, display, err
}

// Sessions and visibility predicate a resume count covers
func (d *SessionDB) resumeCountScope(ctx context.Context, sessionID string, tipOnly bool) ([]string, string, error) {
	if tipOnly {
		return []string{sessionID}, "active = 1", nil
	}
	ids, err := d.resumeLineageIDs(ctx, sessionID)
	return ids, "(active = 1 OR compacted = 1)", err
}

// Messages a resume of the session would reload
func (d *SessionDB) GetResumeMessageCount(ctx context.Context, sessionID string, tipOnly bool) (int64, error) {
	ids, clause, err := d.resumeCountScope(ctx, sessionID, tipOnly)
	if err != nil {
		return 0, err
	}
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n FROM messages WHERE session_id IN ("+placeholders(len(ids))+") AND "+clause, stringArgs(ids)...)
	if err != nil {
		return 0, err
	}
	n, err := pyIntOf(rowValue(row, "n"))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// Bounded count of the messages a resume reloads, refusing a transcript past the cap
func (d *SessionDB) AssertResumeSafe(ctx context.Context, sessionID string, maxMessages *int, tipOnly bool) (int, error) {
	limit := 0
	if maxMessages == nil {
		limit = ResolvedMaxResumeMessages(ctx)
	} else {
		limit = *maxMessages
	}
	if limit < 0 {
		return 0, pyValueError("max_messages must be non-negative")
	}
	if limit == 0 {
		return 0, nil
	}
	ids, clause, err := d.resumeCountScope(ctx, sessionID, tipOnly)
	if err != nil {
		return 0, err
	}
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n FROM ("+
		"SELECT 1 FROM messages WHERE session_id IN ("+placeholders(len(ids))+") "+
		"AND "+clause+" LIMIT ?)", append(stringArgs(ids), limit+1)...)
	if err != nil {
		return 0, err
	}
	n, err := pyIntOf(rowValue(row, "n"))
	if err != nil {
		return 0, err
	}
	count := int(n.Int64())
	if count > limit {
		scope := "across its lineage"
		if tipOnly {
			scope = "in its tip segment"
		}
		return 0, &SessionResumeTooLargeError{MessageCount: count, Limit: limit, Scope: scope}
	}
	return count, nil
}

// Display messages the ancestors contribute ahead of the tip, row ids stripped
func (d *SessionDB) GetAncestorDisplayPrefix(ctx context.Context, sessionID string) ([]map[string]any, error) {
	ids, err := d.resumeLineageIDs(ctx, sessionID)
	if err != nil || len(ids) <= 1 {
		return []map[string]any{}, err
	}
	fetched, err := d.fetchConversationRows(ctx, ids, "", true, true)
	if err != nil {
		return nil, err
	}
	rows, err := dedupeDisplayGenerations(fetched, true)
	if err != nil {
		return nil, err
	}
	ancestor := map[int64]bool{}
	for _, r := range rows {
		if rowValue(r, "session_id") != sessionID && rowValue(r, "id") != nil {
			n, err := pyIntOf(rowValue(r, "id"))
			if err != nil {
				return nil, err
			}
			ancestor[n.Int64()] = true
		}
	}
	if len(ancestor) == 0 {
		return []map[string]any{}, nil
	}
	lineage, err := d.rowsToConversation(rows, sessionID, true, false, true, false)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range lineage {
		if id, ok := plainRowInt(m["_row_id"]); ok && ancestor[id] {
			c := make(map[string]any, len(m))
			for k, v := range m {
				if k != "_row_id" {
					c[k] = v
				}
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// Lineage root of a session id, the id itself when it has no recorded chain
func (d *SessionDB) GetConversationRoot(sessionID string) (string, error) {
	chain, err := d.sessionLineageRootToTip(context.Background(), sessionID)
	if err != nil {
		return "", err
	}
	if len(chain) > 0 && chain[0] != "" {
		return chain[0], nil
	}
	return sessionID, nil
}

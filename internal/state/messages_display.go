package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sort"
)

// Filter dropping alias rows that are a later copy of an archived tool result already painted in an earlier slot
func notALaterToolCopySQL(alias string) string {
	return " AND NOT (" + alias + ".role = 'tool' AND " + alias + ".message_uid IS NOT NULL AND EXISTS (" +
		"SELECT 1 FROM messages AS original WHERE original.session_id = " + alias + ".session_id " +
		"AND original.timestamp = " + alias + ".timestamp AND original.message_uid = " + alias + ".message_uid " +
		"AND original.role = 'tool' AND original.active = 0 AND original.compacted = 1 " +
		"AND +original.display_order < " + alias + ".display_order AND " +
		"COALESCE(" + JSONExtractSQL("original.display_metadata", "$."+ModelOnlyDisplayMetadataKey) + ", 0) = 0))"
}

// Owner of a tool result copy, its session, identity and timestamp
type toolCopyOwner struct {
	session string
	uid     string
	ts      float64
}

// Ids of the tool rows a display read skips because an archived copy holds an earlier slot
func laterToolCopyIDs(rows []*msgRow) map[int64]bool {
	skip := map[int64]bool{}
	var tools []*msgRow
	for _, r := range rows {
		if r.roleText() == "tool" {
			tools = append(tools, r)
		}
	}
	if len(tools) == 0 || !tools[0].has["compacted"] {
		return skip
	}
	var candidates []*msgRow
	for _, r := range tools {
		if cellTruthy(r.active) || cellTruthy(r.compacted) {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 || !candidates[0].has["display_order"] {
		return skip
	}
	owner := func(r *msgRow) toolCopyOwner {
		ts, _ := floatCell(r.timestamp)
		return toolCopyOwner{r.session(), textCell(r.messageUID), ts}
	}
	firstSlot := map[toolCopyOwner]int64{}
	for _, r := range candidates {
		slot, ok := cellInt(r.displayOrder)
		if textCell(r.messageUID) == "" || r.displayOrder == nil || !ok || cellTruthy(r.active) || isModelOnlyRow(r) {
			continue
		}
		if cur, seen := firstSlot[owner(r)]; !seen || slot < cur {
			firstSlot[owner(r)] = slot
		}
	}
	for _, r := range candidates {
		slot, ok := cellInt(r.displayOrder)
		if textCell(r.messageUID) == "" || r.displayOrder == nil || !ok {
			continue
		}
		if first, seen := firstSlot[owner(r)]; seen && first < slot {
			skip[r.id] = true
		}
	}
	return skip
}

// Display identity both display reads give a row scanned in id order, a summary carrier taking its message's first key
func foldSummaryCarrier(r *msgRow, key string, firstByUID map[string]string) string {
	uid := textCell(r.messageUID)
	if uid == "" {
		return key
	}
	if cellTruthy(r.compressedSummary) {
		if first, ok := firstByUID[uid]; ok {
			return first
		}
		return key
	}
	if _, ok := firstByUID[uid]; !ok {
		firstByUID[uid] = key
	}
	return key
}

// Reports a row removed from display, neither live nor compaction archived
func rowRemoved(r *msgRow) bool { return !cellTruthy(r.active) && !cellTruthy(r.compacted) }

// Collapses compaction generations so each logical message appears once, preferring the live row and then the newest
func dedupeDisplayGenerations(rows []*msgRow, dropRemoved bool) []*msgRow {
	seen := map[string]*msgRow{}
	firstID := map[string]int64{}
	uidKey := map[string]string{}
	removed := map[string]bool{}
	identities := map[string][]string{}
	var order []string
	removedTS := map[float64]bool{}
	later := laterToolCopyIDs(rows)
	if dropRemoved {
		for _, r := range rows {
			if rowRemoved(r) {
				ts, _ := floatCell(r.timestamp)
				removedTS[ts] = true
			}
		}
		dropRemoved = len(removedTS) > 0
	}
	for _, r := range rows {
		var copies []string
		key, haveKey := "", false
		if dropRemoved {
			gone := rowRemoved(r)
			var stored []byte
			if gone {
				stored, _ = r.displayIdentity.([]byte)
			}
			hashed := string(stored)
			if len(stored) == 0 {
				key, haveKey = displayKeyOf(r), true
				ts, _ := floatCell(r.timestamp)
				if gone || removedTS[ts] {
					hashed = string(displayIdentity(key))
				}
			}
			if hashed != "" {
				copies = append(copies, "h:"+hashed)
			}
			for _, uid := range append([]string{textCell(r.messageUID)}, uidListCell(r.absorbedMessageUIDs)...) {
				if uid != "" {
					copies = append(copies, "u:"+uid)
				}
			}
			for _, c := range copies {
				removed[c] = gone
			}
			if gone {
				continue
			}
		}
		if isModelOnlyRow(r) || later[r.id] {
			continue
		}
		if !haveKey {
			key = displayKeyOf(r)
		}
		key = foldSummaryCarrier(r, key, uidKey)
		cur, ok := seen[key]
		if !ok {
			order = append(order, key)
		}
		if !ok || rowOutranks(r, cur) {
			seen[key] = r
			identities[key] = copies
		}
		if first, has := firstID[key]; !has || r.id < first {
			firstID[key] = r.id
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return firstID[order[i]] < firstID[order[j]] })
	out := make([]*msgRow, 0, len(order))
	for _, key := range order {
		hidden := false
		if dropRemoved {
			for _, c := range identities[key] {
				hidden = hidden || removed[c]
			}
		}
		if !hidden {
			out = append(out, seen[key])
		}
	}
	return out
}

// Reports whether row a outranks b as the representative of one display group, live first and then newest
func rowOutranks(a, b *msgRow) bool {
	aa, _ := cellInt(a.active)
	ba, _ := cellInt(b.active)
	if aa != ba {
		return aa > ba
	}
	return a.id > b.id
}

// Uid list text decoded quietly, empty text reading as none
func uidListCell(v any) []string {
	text := textCell(v)
	if text == "" {
		return nil
	}
	return DecodeUIDList(text)
}

// Folds split display generations of a session back together by the recomputed display key, writing only on drift
func reconcileDisplayOrders(ctx context.Context, c Conn, sessionID string) error {
	firstID := map[string]int64{}
	uidIdentity := map[string]string{}
	var lastID int64
	type update struct {
		order    int64
		identity []byte
		id       int64
	}
	for {
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name, "+
			"display_kind, display_metadata, display_order, display_identity, message_uid, _compressed_summary "+
			"FROM messages INDEXED BY idx_messages_session_id "+
			"WHERE session_id = ? AND id > ? AND (active = 1 OR compacted = 1) "+
			"ORDER BY id LIMIT 1000", sessionID, lastID))
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		var updates []update
		for _, r := range rows {
			lastID = r.id
			identity := foldSummaryCarrier(r, string(displayIdentity(displayKeyOf(r))), uidIdentity)
			stored, _ := r.displayIdentity.([]byte)
			sameIdentity := r.displayIdentity != nil && bytes.Equal(stored, []byte(identity))
			storedOrder, hasOrder := cellInt(r.displayOrder)
			hasOrder = hasOrder && r.displayOrder != nil
			slot := lastID
			if sameIdentity && hasOrder {
				slot = storedOrder
			}
			order, known := firstID[identity]
			if !known {
				order = slot
				firstID[identity] = order
			}
			if !hasOrder || order != storedOrder || !sameIdentity {
				updates = append(updates, update{order, []byte(identity), lastID})
			}
		}
		for _, u := range updates {
			if _, err := c.ExecContext(ctx, "UPDATE messages SET display_order = ?, display_identity = ? WHERE id = ?", u.order, u.identity, u.id); err != nil {
				return err
			}
		}
	}
}

// Column names of the messages table, read once per store
func (s *Store) messageColumnNames(ctx context.Context, c Conn) ([]string, error) {
	st := s.tables.state
	st.columnsMu.Lock()
	defer st.columnsMu.Unlock()
	if len(st.messageColumns) > 0 {
		return st.messageColumns, nil
	}
	cols, err := tableInfo(ctx, c, "messages")
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.name
	}
	st.messageColumns = names
	return names, nil
}

// Backfills the display index of one legacy session once, false when the store cannot carry or persist it
func (s *Store) ensureDisplayOrder(ctx context.Context, sessionID string) (bool, error) {
	var cols []string
	missing := false
	err := s.Read(ctx, func(c Conn) error {
		var err error
		if cols, err = s.messageColumnNames(ctx, c); err != nil {
			return err
		}
		if !containsString(cols, "display_order") || !containsString(cols, "display_identity") {
			return nil
		}
		missing, err = displayIndexMissing(ctx, c, sessionID)
		return err
	})
	if err != nil {
		return false, err
	}
	if !containsString(cols, "display_order") || !containsString(cols, "display_identity") {
		return false, nil
	}
	if !missing {
		return true, nil
	}
	if s.readOnly {
		return false, nil
	}
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		missing, err := displayIndexMissing(ctx, c, sessionID)
		if err != nil || !missing {
			return err
		}
		return reconcileDisplayOrders(ctx, c, sessionID)
	})
	return err == nil, err
}

// Reports whether any display row of the session still lacks either half of its display index
func displayIndexMissing(ctx context.Context, c Conn, sessionID string) (bool, error) {
	var one int
	err := c.QueryRowContext(ctx, displayIndexMissingSQL, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// One page of rows, the newest first when latest is set and the result chronological
func pageRows[T any](rows []T, offset int, limit *int, latest bool) []T {
	clip := func(in []T) []T {
		in = in[min(max(offset, 0), len(in)):]
		if limit == nil {
			return in
		}
		n := *limit
		if n < 0 {
			n = max(len(in)+n, 0)
		}
		return in[:min(n, len(in))]
	}
	if !latest {
		return clip(rows)
	}
	reversed := make([]T, len(rows))
	for i, r := range rows {
		reversed[len(rows)-1-i] = r
	}
	page := clip(reversed)
	out := make([]T, len(page))
	for i, r := range page {
		out[len(page)-1-i] = r
	}
	return out
}

// Display page of a legacy read-only store, keeping only identities and representative ids while scanning
func (s *Store) legacyDisplayPage(ctx context.Context, sessionID, activeClause string, limit *int, offset int, latest bool) ([]*msgRow, error) {
	var out []*msgRow
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
			return err
		}
		defer func() { _, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }()
		indexed, err := queryStrings(ctx, c, "SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?", "idx_messages_session_id")
		if err != nil {
			return err
		}
		hint := "NOT INDEXED"
		if len(indexed) > 0 {
			hint = "INDEXED BY idx_messages_session_id"
		}
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name, active, "+
			"display_kind, display_metadata FROM messages "+hint+" "+
			"WHERE session_id = ?"+activeClause+" ORDER BY id ASC", sessionID))
		if err != nil {
			return err
		}
		type representative struct{ active, id int64 }
		best := map[string]representative{}
		var identities []string
		for _, r := range rows {
			if isModelOnlyRow(r) {
				continue
			}
			identity := string(displayIdentity(displayKeyOf(r)))
			active, _ := cellInt(r.active)
			cur, ok := best[identity]
			if !ok {
				identities = append(identities, identity)
			}
			if !ok || active > cur.active || (active == cur.active && r.id > cur.id) {
				best[identity] = representative{active, r.id}
			}
		}
		rows = nil
		var selected []int64
		for _, identity := range pageRows(identities, offset, limit, latest) {
			selected = append(selected, best[identity].id)
		}
		byID := map[int64]*msgRow{}
		for _, chunk := range chunks(selected, sqlInChunk) {
			found, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ?"+activeClause+" "+
				"AND id IN ("+bindMarks(len(chunk))+")", append([]any{sessionID}, int64Args(chunk)...)...))
			if err != nil {
				return err
			}
			for _, r := range found {
				byID[r.id] = r
			}
		}
		for _, id := range selected {
			if r, ok := byID[id]; ok {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// One display history projection, a page of display slots each shown as its live or newest row
func displayRowsIn(ctx context.Context, c Conn, sessionID string, limit *int, offset int, latest bool) ([]*msgRow, error) {
	direction := "ASC"
	if latest {
		direction = "DESC"
	}
	bound := -1
	if limit != nil {
		bound = *limit
	}
	return queryMsgRows(c.QueryContext(ctx, `WITH page AS (
                   SELECT display_order FROM messages
                   WHERE session_id = ? AND (active = 1 OR compacted = 1)`+displayVisibleSQL+notALaterToolCopySQL("messages")+`
                   GROUP BY display_order ORDER BY display_order `+direction+`
                   LIMIT ? OFFSET ?
               )
               SELECT chosen.* FROM page
               JOIN messages AS chosen ON chosen.id = (
                   SELECT candidate.id FROM messages AS candidate
                   WHERE candidate.session_id = ?
                     AND candidate.display_order = page.display_order
                     AND (candidate.active = 1 OR candidate.compacted = 1)`+displayVisibleSQL+notALaterToolCopySQL("candidate")+`
                   ORDER BY candidate.active DESC, candidate.id DESC LIMIT 1
               )
               ORDER BY page.display_order ASC`, sessionID, bound, offset, sessionID))
}

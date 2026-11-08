package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Rounds half to even like the Python round builtin
func pyRound(f float64) int64 {
	return int64(math.RoundToEven(f))
}

// Text free timing evidence of a session export built from message timestamps only
func exportTimings(messages []*pyjson.Object, sessionID string) *pyjson.Object {
	type stamped struct {
		msg *pyjson.Object
		ts  float64
	}
	timestamped := []stamped{}
	roleCounts := pyjson.NewObject()
	toolCallsEmitted := int64(0)
	for _, msg := range messages {
		if ts, ok := pydatetime.CoerceEpoch(rowValue(msg, "timestamp"), sessionID, ""); ok {
			timestamped = append(timestamped, stamped{msg, ts})
		}
		role := "unknown"
		if v := rowValue(msg, "role"); pyTruthy(v) {
			role = pyStr(v)
		}
		current, _ := roleCounts.Get(role)
		n, _ := exactInt(current)
		roleCounts.Set(role, n+1)
		if tc := rowValue(msg, "tool_calls"); pyTruthy(tc) {
			if list, ok := tc.([]any); ok {
				toolCallsEmitted += int64(len(list))
			} else {
				toolCallsEmitted++
			}
		}
	}
	intervals := []any{}
	var largest any
	for i := 1; i < len(timestamped); i++ {
		prev, next := timestamped[i-1], timestamped[i]
		gap := max(int64(0), pyRound((next.ts-prev.ts)*1000))
		intervals = append(intervals, pyjson.ObjectOf("from_message_id", rowValue(prev.msg, "id"), "to_message_id", rowValue(next.msg, "id"),
			"from_role", rowValue(prev.msg, "role"), "to_role", rowValue(next.msg, "role"), "gap_ms", gap))
		if largest == nil || gap > largest.(int64) {
			largest = gap
		}
	}
	iso := func(ts float64) any {
		sec, frac := math.Modf(ts)
		return pydatetime.ISOFormatUTC(time.Unix(int64(sec), int64(frac*1e9)).UTC())
	}
	var first, last, wall, reason any
	available := len(timestamped) > 0
	if available {
		first, last = iso(timestamped[0].ts), iso(timestamped[len(timestamped)-1].ts)
		wall = max(int64(0), pyRound((timestamped[len(timestamped)-1].ts-timestamped[0].ts)*1000))
	} else {
		reason = "no_timestamped_messages"
	}
	toolResults, _ := roleCounts.Get("tool")
	if toolResults == nil {
		toolResults = int64(0)
	}
	return pyjson.ObjectOf(
		"source", "message_timestamps",
		"available", available,
		"complete", false,
		"unavailable_reason", reason,
		"message_timestamps", pyjson.ObjectOf("available", int64(len(timestamped)), "missing", int64(len(messages)-len(timestamped))),
		"first_message_at", first,
		"last_message_at", last,
		"wall_clock_ms", wall,
		"largest_gap_ms", largest,
		"role_counts", roleCounts,
		"tool_result_count", toolResults,
		"tool_calls_emitted", toolCallsEmitted,
		"intervals", intervals,
	)
}

// Session id of an earlier import of the same foreign snapshot on the caller connection, empty when none
func findForeignImportOnConn(ctx context.Context, conn SQLiteConn, origin *pyjson.Object) (string, error) {
	tool, err := receiptItem(origin, "tool")
	if err != nil {
		return "", err
	}
	rows, err := queryObjects(ctx, conn, "SELECT id, origin_json FROM sessions WHERE source = ? AND origin_json IS NOT NULL", tool)
	if err != nil {
		return "", err
	}
	path, err := receiptItem(origin, "path")
	if err != nil && pyTruthy(rowValue(origin, "foreign_session_id")) {
		err = nil
	}
	for _, row := range rows {
		decoded := jsonValueOrText(rowValue(row, "origin_json"))
		stored, _ := decoded.(*pyjson.Object)
		if stored == nil {
			stored = pyjson.NewObject()
		}
		importedRaw, _ := stored.Get("imported_from")
		imported, _ := importedRaw.(*pyjson.Object)
		if imported == nil {
			imported = pyjson.NewObject()
		}
		if !pyjson.Equal(rowValue(imported, "tool"), tool) {
			continue
		}
		foreignID, _ := origin.Get("foreign_session_id")
		if pyTruthy(foreignID) {
			if pyjson.Equal(foreignID, rowValue(imported, "foreign_session_id")) {
				return pyStr(rowValue(row, "id")), nil
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if pyjson.Equal(rowValue(imported, "path"), path) {
			return pyStr(rowValue(row, "id")), nil
		}
	}
	return "", nil
}

// Session id of an earlier import of the foreign snapshot, empty when none
func (d *SessionDB) FindForeignImport(ctx context.Context, origin *pyjson.Object) (string, error) {
	var found string
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		found, err = findForeignImportOnConn(ctx, conn, origin)
		return err
	})
	return found, err
}

// Adopts or mints a foreign snapshot in one transaction with its provenance, a duplicate click returning the first
func (d *SessionDB) ImportForeignHistory(ctx context.Context, origin *pyjson.Object, messages []any, title, cwd string, profile any) (*pyjson.Object, error) {
	sessionID := NewSessionIDAt(time.Now(), 12)
	tool, err := receiptItem(origin, "tool")
	if err != nil {
		return nil, err
	}
	normalized, errs, err := d.validateImportPayload([]any{pyjson.ObjectOf("id", sessionID, "source", tool, "title", title, "cwd", cwd, "messages", messages)})
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, pyValueError(pyStr(rowValue(errs[0].(*pyjson.Object), "error")))
	}
	var result *pyjson.Object
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		existing, err := findForeignImportOnConn(ctx, conn, origin)
		if err != nil {
			return err
		}
		if existing != "" {
			result = pyjson.ObjectOf("session_id", existing, "already_imported", true)
			return nil
		}
		item := normalized[0]
		session := rowGet(item, "session").(*pyjson.Object)
		taken, err := connRow(ctx, conn, "SELECT 1 FROM sessions WHERE title = ?", title)
		if err != nil {
			return err
		}
		if taken != nil {
			runes := []rune(sessionID)
			session.Set("title", fmt.Sprintf("%s (%s)", title, string(runes[max(0, len(runes)-12):])))
		}
		if err := d.importSessionRow(ctx, conn, session, rowGet(item, "messages"), sessionID); err != nil {
			return err
		}
		provenance, err := pyDumps(pyjson.ObjectOf("imported_from", origin), pyJSONDefault)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET origin_json = ?, profile_name = ? WHERE id = ?", provenance, profile, sessionID); err != nil {
			return err
		}
		result = pyjson.ObjectOf("session_id", sessionID, "already_imported", false)
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Distinct non empty session cwds with usage counts and last activity for repo discovery
func (d *SessionDB) DistinctSessionCwds(ctx context.Context, includeArchived bool) ([]*pyjson.Object, error) {
	where := "cwd IS NOT NULL AND TRIM(cwd) != ''"
	if !includeArchived {
		where += " AND archived = 0"
	}
	rows, err := d.readRows(ctx, "SELECT cwd AS cwd, COUNT(*) AS sessions, MAX(COALESCE(ended_at, started_at, 0)) AS last_active "+
		"FROM sessions WHERE "+where+" GROUP BY cwd")
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, r := range rows {
		sessions, _ := exactInt(pyOr(rowValue(r, "sessions"), int64(0)))
		lastActive, err := pyFloatOf(pyOr(rowValue(r, "last_active"), int64(0)))
		if err != nil {
			return nil, err
		}
		out = append(out, pyjson.ObjectOf("cwd", rowValue(r, "cwd"), "sessions", sessions, "last_active", lastActive))
	}
	return out, nil
}

// Cron job and execution ids of a cron run session, nil for any other session
func (d *SessionDB) CronRunIdentity(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	if job, ok := LegacyCronRunJob(sessionID); ok {
		return pyjson.ObjectOf("cron_job_id", job, "cron_execution_id", nil), nil
	}
	rows, err := d.readRows(ctx, "SELECT request_id, "+cronPolicyJobSQL+" AS policy_job FROM session_admissions"+
		" WHERE principal_id = ? AND target_session_id = ? ORDER BY seq LIMIT 1", CronPrincipalID, sessionID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	requestID := rowValue(rows[0], "request_id")
	job, execution, ok := CronRunFromRequest(requestID, nil)
	if !ok {
		if policyJob, isStr := rowValue(rows[0], "policy_job").(string); isStr {
			job, execution, ok = CronRunFromRequest(requestID, &policyJob)
		}
	}
	if !ok {
		return nil, nil
	}
	return pyjson.ObjectOf("cron_job_id", job, "cron_execution_id", execution), nil
}

// Titled sessions whose first user turn was a skill invocation or an auto load scaffold, newest first
func (d *SessionDB) ListSkillScaffoldedSessions(ctx context.Context, limit int) ([]*pyjson.Object, error) {
	return d.readRows(ctx, `
                SELECT s.id, s.title, m.content
                FROM sessions s
                JOIN messages m ON m.id = (
                    SELECT m2.id FROM messages m2
                    WHERE m2.session_id = s.id AND m2.role = 'user'
                      AND m2.content IS NOT NULL
                    ORDER BY m2.timestamp, m2.id LIMIT 1
                )
                WHERE s.title IS NOT NULL AND (m.content LIKE ? OR m.content LIKE ?)
                ORDER BY s.started_at DESC
                LIMIT ?
                `, SkillScaffoldSQLLike, AutoLoadScaffoldSQLLike, int64(limit))
}

// Session dict with its messages and timings attached
func (d *SessionDB) withMessages(ctx context.Context, session *pyjson.Object, includeCompacted, includeInactive bool) (*pyjson.Object, error) {
	sessionID := pyStr(rowValue(session, "id"))
	messages, err := d.GetMessages(ctx, sessionID, GetMessagesOptions{IncludeInactive: includeInactive, IncludeCompacted: includeCompacted})
	if err != nil {
		return nil, err
	}
	out := session.Copy()
	out.Set("messages", anyObjectList(messages))
	out.Set("timings", exportTimings(messages, sessionID))
	return out, nil
}

// One session with every message as a dict, compacted turns and archived rows on request, nil when unknown
func (d *SessionDB) ExportSession(ctx context.Context, sessionID string, includeCompacted, includeInactive bool) (*pyjson.Object, error) {
	session, err := d.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return nil, err
	}
	return d.withMessages(ctx, session, includeCompacted, includeInactive)
}

// A compression lineage exported as one logical session dict with its segments, nil when unknown
func (d *SessionDB) ExportSessionLineage(ctx context.Context, sessionID string, includeCompacted, includeInactive bool) (*pyjson.Object, error) {
	lineage, err := d.GetCompressionLineage(ctx, sessionID)
	if err != nil || len(lineage) == 0 {
		return nil, err
	}
	segments := []any{}
	messages := []any{}
	var last *pyjson.Object
	ids := []any{}
	for _, sid := range lineage {
		segment, err := d.ExportSession(ctx, sid, includeCompacted, includeInactive)
		if err != nil {
			return nil, err
		}
		if segment == nil {
			continue
		}
		segments = append(segments, segment)
		last = segment
		ids = append(ids, rowValue(segment, "id"))
		if list, ok := rowValue(segment, "messages").([]any); ok {
			messages = append(messages, list...)
		}
	}
	if last == nil {
		return nil, nil
	}
	out := last.Copy()
	out.Set("segments", segments)
	out.Set("lineage_session_ids", ids)
	out.Set("message_count", int64(len(messages)))
	out.Set("messages", messages)
	out.Set("timings", exportTimings(objectsOf(messages), sessionID))
	return out, nil
}

// Row objects among a JSON list value
func objectsOf(values []any) []*pyjson.Object {
	out := make([]*pyjson.Object, 0, len(values))
	for _, v := range values {
		if obj, ok := v.(*pyjson.Object); ok {
			out = append(out, obj)
		}
	}
	return out
}

// Every session with its messages, one batched read per 900 sessions outside the display projection
func (d *SessionDB) ExportAll(ctx context.Context, source string, includeCompacted, includeInactive bool) ([]*pyjson.Object, error) {
	var sources []string
	if source != "" {
		sources = []string{source}
	}
	sessions, err := d.SearchSessions(ctx, SearchSessionsOptions{Sources: sources, Limit: 100000})
	if err != nil {
		return nil, err
	}
	if includeCompacted {
		out := make([]*pyjson.Object, 0, len(sessions))
		for _, session := range sessions {
			exported, err := d.withMessages(ctx, session, true, includeInactive)
			if err != nil {
				return nil, err
			}
			out = append(out, exported)
		}
		return out, nil
	}
	bySession := map[string][]any{}
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sid := pyStr(rowValue(session, "id"))
		bySession[sid] = []any{}
		ids = append(ids, sid)
	}
	clause := activeClause(includeInactive, false)
	for start := 0; start < len(ids); start += 900 {
		chunk := ids[start:min(start+900, len(ids))]
		rows, err := d.readAll(ctx, "SELECT * FROM messages WHERE session_id IN ("+placeholders(len(chunk))+") "+clause+" ORDER BY session_id, id", stringArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			msg, err := d.rowToMessageDict(row, "get_messages", true)
			if err != nil {
				return nil, err
			}
			sid := pyStr(rowValue(row, "session_id"))
			bySession[sid] = append(bySession[sid], msg)
		}
	}
	out := make([]*pyjson.Object, 0, len(sessions))
	for _, session := range sessions {
		sid := pyStr(rowValue(session, "id"))
		exported := session.Copy()
		exported.Set("messages", bySession[sid])
		exported.Set("timings", exportTimings(objectsOf(bySession[sid]), sid))
		out = append(out, exported)
	}
	return out, nil
}

// Adopts a session's whole compression lineage from a donor store, archiving the donor rows after a complete adoption
func (d *SessionDB) AdoptSessionLineageFrom(ctx context.Context, donor *SessionDB, sessionID string, retireDonor bool) (*pyjson.Object, error) {
	payload, err := donor.ExportSessionLineage(ctx, sessionID, false, true)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return pyjson.ObjectOf("ok", false, "adopted", false, "donor_retired", false,
			"error", fmt.Sprintf("session %s not found in donor store", pyRepr(sessionID))), nil
	}
	segments, ok := rowValue(payload, "segments").([]any)
	if !ok || len(segments) == 0 {
		segments = []any{payload}
	}
	donorAhead := false
	for _, raw := range segments {
		seg := raw.(*pyjson.Object)
		segID, _ := rowValue(seg, "id").(string)
		if segID == "" {
			continue
		}
		local, err := d.GetSession(ctx, segID)
		if err != nil {
			return nil, err
		}
		if local == nil {
			continue
		}
		donorMessages, _ := rowValue(seg, "messages").([]any)
		localMessages, err := d.GetMessages(ctx, segID, GetMessagesOptions{IncludeInactive: true})
		if err != nil {
			return nil, err
		}
		if len(donorMessages) > len(localMessages) {
			donorAhead = true
			slog.Warn(fmt.Sprintf("adoption divergence: donor segment %s has %d messages, local copy has %d — donor will NOT be retired",
				segID, len(donorMessages), len(localMessages)))
		}
	}
	copies := make([]any, 0, len(segments))
	for _, raw := range segments {
		copies = append(copies, raw.(*pyjson.Object).Copy())
	}
	result, err := d.ImportSessions(ctx, copies)
	if err != nil {
		return nil, err
	}
	imported, _ := exactInt(pyOr(rowValue(result, "imported"), int64(0)))
	skipped, _ := exactInt(pyOr(rowValue(result, "skipped"), int64(0)))
	adopted := pyTruthy(rowValue(result, "ok")) && int(imported+skipped) == len(segments)
	if !adopted {
		slog.Warn(fmt.Sprintf("adoption of %s did not complete: imported=%d skipped=%d of %d segment(s); errors=%s",
			sessionID, imported, skipped, len(segments), pyReprValue(rowValue(result, "errors"))))
	}
	donorRetired := false
	if adopted && retireDonor && !donorAhead {
		donorRetired = true
		for _, raw := range segments {
			segID, _ := rowValue(raw.(*pyjson.Object), "id").(string)
			if segID == "" {
				continue
			}
			if !d.retireDonorSegment(ctx, donor, segID) {
				donorRetired = false
			}
		}
	}
	out := result.Copy()
	out.Set("adopted", adopted)
	out.Set("donor_retired", donorRetired)
	return out, nil
}

// Archives one adopted donor segment after re reading both stores, false when skipped or failed
func (d *SessionDB) retireDonorSegment(ctx context.Context, donor *SessionDB, segID string) (retired bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption: %v", segID, r))
			retired = false
		}
	}()
	donorMessages, err := donor.GetMessages(ctx, segID, GetMessagesOptions{IncludeInactive: true})
	if err != nil {
		slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption: %v", segID, err))
		return false
	}
	localMessages, err := d.GetMessages(ctx, segID, GetMessagesOptions{IncludeInactive: true})
	if err != nil {
		slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption: %v", segID, err))
		return false
	}
	if len(donorMessages) > len(localMessages) {
		slog.Warn(fmt.Sprintf("adoption divergence at retire time: donor segment %s grew to %d messages (local %d) — leaving donor unretired",
			segID, len(donorMessages), len(localMessages)))
		return false
	}
	if err := donor.ReopenSession(ctx, segID); err != nil {
		slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption: %v", segID, err))
		return false
	}
	if err := donor.EndSession(segID, "adopted_by_profile"); err != nil {
		slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption: %v", segID, err))
		return false
	}
	if _, err := donor.SetSessionArchived(ctx, segID, true, false); err != nil {
		slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption: %v", segID, err))
		return false
	}
	return true
}

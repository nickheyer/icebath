package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

type ExportOptions struct {
	IncludeCompacted bool
	IncludeInactive  bool
}

type SessionExport struct {
	Session           Session
	Messages          []MessageRow
	Timings           ExportTimings
	Segments          []SessionExport
	LineageSessionIDs []string
	columns           []configMember
}

type ExportTimings struct {
	Source            string            `json:"source"`
	Available         bool              `json:"available"`
	Complete          bool              `json:"complete"`
	UnavailableReason *string           `json:"unavailable_reason"`
	MessageTimestamps TimestampCoverage `json:"message_timestamps"`
	FirstMessageAt    *string           `json:"first_message_at"`
	LastMessageAt     *string           `json:"last_message_at"`
	WallClockMS       *int64            `json:"wall_clock_ms"`
	LargestGapMS      *int64            `json:"largest_gap_ms"`
	RoleCounts        RoleCounts        `json:"role_counts"`
	ToolResultCount   int               `json:"tool_result_count"`
	ToolCallsEmitted  int               `json:"tool_calls_emitted"`
	Intervals         []TimingInterval  `json:"intervals"`
}

type TimestampCoverage struct {
	Available int `json:"available"`
	Missing   int `json:"missing"`
}

type RoleCount struct {
	Role  string
	Count int
}

type RoleCounts []RoleCount

type TimingInterval struct {
	FromMessageID int64  `json:"from_message_id"`
	ToMessageID   int64  `json:"to_message_id"`
	FromRole      string `json:"from_role"`
	ToRole        string `json:"to_role"`
	GapMS         int64  `json:"gap_ms"`
}

func (r RoleCounts) MarshalJSON() ([]byte, error) {
	members := make([]configMember, 0, len(r))
	for _, rc := range r {
		members = append(members, configMember{rc.Role, rawInt(int64(rc.Count))})
	}
	return rawObject(members), nil
}

func (r RoleCounts) Count(role string) int {
	for _, rc := range r {
		if rc.Role == role {
			return rc.Count
		}
	}
	return 0
}

func timestampISO(ts float64) string {
	sec, frac := math.Modf(ts)
	us := math.RoundToEven(frac * 1e6)
	if us >= 1e6 {
		sec, us = sec+1, us-1e6
	} else if us < 0 {
		sec, us = sec-1, us+1e6
	}
	return isoUTC(time.Unix(int64(sec), int64(us)*1000))
}

func exportEpoch(ts sql.Null[float64], sessionID string) (float64, bool) {
	if !ts.Valid {
		return 0, false
	}
	if math.IsNaN(ts.V) || ts.V < 0 || ts.V > 4200000000 {
		suffix := ""
		if sessionID != "" {
			suffix = " on session " + sessionID
		}
		slog.Warn(fmt.Sprintf("Ignoring corrupt timestamp %s%s", shortestFloatText(ts.V), suffix))
		return 0, false
	}
	return ts.V, true
}

func ExportTimingsOf(messages []MessageRow, sessionID string) ExportTimings {
	type stamped struct {
		msg *MessageRow
		ts  float64
	}
	var timestamped []stamped
	for i := range messages {
		if ts, ok := exportEpoch(messages[i].Timestamp, sessionID); ok {
			timestamped = append(timestamped, stamped{&messages[i], ts})
		}
	}
	var counts RoleCounts
	emitted := 0
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "unknown"
		}
		found := false
		for i := range counts {
			if counts[i].Role == role {
				counts[i].Count++
				found = true
				break
			}
		}
		if !found {
			counts = append(counts, RoleCount{role, 1})
		}
		if jsonTruthy(m.ToolCalls) {
			if elements, ok := arrayElements(m.ToolCalls); ok {
				emitted += len(elements)
			} else {
				emitted++
			}
		}
	}
	if counts == nil {
		counts = RoleCounts{}
	}
	intervals := make([]TimingInterval, 0, max(len(timestamped)-1, 0))
	var largest *int64
	for i := 1; i < len(timestamped); i++ {
		prev, next := timestamped[i-1], timestamped[i]
		gap := max(0, int64(math.RoundToEven((next.ts-prev.ts)*1000)))
		intervals = append(intervals, TimingInterval{FromMessageID: prev.msg.ID, ToMessageID: next.msg.ID,
			FromRole: prev.msg.Role, ToRole: next.msg.Role, GapMS: gap})
		if largest == nil || gap > *largest {
			largest = &gap
		}
	}
	out := ExportTimings{Source: "message_timestamps", Available: len(timestamped) > 0,
		MessageTimestamps: TimestampCoverage{Available: len(timestamped), Missing: len(messages) - len(timestamped)},
		LargestGapMS:      largest, RoleCounts: counts, ToolResultCount: counts.Count("tool"), ToolCallsEmitted: emitted,
		Intervals: intervals}
	if len(timestamped) == 0 {
		reason := "no_timestamped_messages"
		out.UnavailableReason = &reason
		return out
	}
	first, last := timestamped[0].ts, timestamped[len(timestamped)-1].ts
	firstAt, lastAt := timestampISO(first), timestampISO(last)
	wall := max(0, int64(math.RoundToEven((last-first)*1000)))
	out.FirstMessageAt, out.LastMessageAt, out.WallClockMS = &firstAt, &lastAt, &wall
	return out
}

func exportCellJSON(v any) json.RawMessage {
	switch t := v.(type) {
	case nil:
		return json.RawMessage("null")
	case []byte:
		v = strings.ToValidUTF8(string(t), "�")
	case string:
		v = strings.ToValidUTF8(t, "�")
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return json.RawMessage("null")
		}
	}
	enc, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{Tight: true})
	if err != nil {
		return json.RawMessage("null")
	}
	return enc
}

func exportNullText(v sql.Null[string]) json.RawMessage {
	if !v.Valid {
		return json.RawMessage("null")
	}
	return exportCellJSON(v.V)
}

func (r MessageRow) MarshalJSON() ([]byte, error) {
	content, err := r.Content.MarshalJSON()
	if err != nil {
		return nil, err
	}
	toolCalls := json.RawMessage("null")
	if r.ToolCalls != nil {
		toolCalls = r.ToolCalls
	}
	timestamp := json.RawMessage("null")
	if r.Timestamp.Valid {
		timestamp = exportCellJSON(r.Timestamp.V)
	}
	tokenCount := json.RawMessage("null")
	if r.TokenCount.Valid {
		tokenCount = rawInt(r.TokenCount.V)
	}
	displayMetadata := json.RawMessage("null")
	if r.DisplayMetadata != nil {
		displayMetadata = r.DisplayMetadata
	}
	members := []configMember{{"id", rawInt(r.ID)}, {"session_id", exportCellJSON(r.SessionID)}, {"role", exportCellJSON(r.Role)},
		{"content", content}, {"tool_call_id", exportNullText(r.ToolCallID)}, {"tool_calls", toolCalls},
		{"tool_name", exportNullText(r.ToolName)}, {"effect_disposition", exportNullText(r.EffectDisposition)},
		{"timestamp", timestamp}, {"token_count", tokenCount}, {"finish_reason", exportNullText(r.FinishReason)},
		{"reasoning", exportNullText(r.Reasoning)}, {"reasoning_content", exportNullText(r.ReasoningContent)},
		{"reasoning_details", exportNullText(r.ReasoningDetails)}, {"codex_reasoning_items", exportNullText(r.CodexReasoningItems)},
		{"codex_message_items", exportNullText(r.CodexMessageItems)}, {"platform_message_id", exportNullText(r.PlatformMessageID)},
		{"observed", rawInt(int64(boolInt(r.Observed)))}, {"active", rawInt(int64(boolInt(r.Active)))},
		{"compacted", rawInt(int64(boolInt(r.Compacted)))}, {"api_content", exportNullText(r.APIContent)},
		{"display_kind", exportNullText(r.DisplayKind)}, {"display_metadata", displayMetadata},
		{"message_uid", exportNullText(r.MessageUID)}, {"absorbed_message_uids", exportNullText(r.AbsorbedMessageUIDs)},
		{"tool_call_uids", exportNullText(r.ToolCallUIDs)}, {"tool_call_uid", exportNullText(r.ToolCallUID)}}
	if r.CompressedSummary {
		members = append(members, configMember{"_compressed_summary", json.RawMessage("true")})
	}
	return rawObject(members), nil
}

func exportMessagesJSON(messages []MessageRow) (json.RawMessage, error) {
	elements := make([]json.RawMessage, len(messages))
	for i, m := range messages {
		enc, err := m.MarshalJSON()
		if err != nil {
			return nil, err
		}
		elements[i] = enc
	}
	return rawArray(elements), nil
}

func (e SessionExport) MarshalJSON() ([]byte, error) {
	members := append([]configMember{}, e.columns...)
	messages, err := exportMessagesJSON(e.Messages)
	if err != nil {
		return nil, err
	}
	timings, err := json.Marshal(e.Timings)
	if err != nil {
		return nil, err
	}
	members = setConfigMember(members, "messages", messages)
	members = setConfigMember(members, "timings", timings)
	if e.Segments != nil {
		segments := make([]json.RawMessage, len(e.Segments))
		for i, seg := range e.Segments {
			if segments[i], err = seg.MarshalJSON(); err != nil {
				return nil, err
			}
		}
		members = setConfigMember(members, "segments", rawArray(segments))
		members = setConfigMember(members, "lineage_session_ids", rawTexts(e.LineageSessionIDs))
	}
	return rawObject(members), nil
}

type sessionRecord struct {
	session Session
	columns []configMember
}

func querySessionRecords(rows *sql.Rows, err error) ([]sessionRecord, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []sessionRecord
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		var r sessionRecord
		for i, col := range cols {
			if d := r.session.dest(col); d != nil {
				if err := (lenient{dst: d}).Scan(cells[i]); err != nil {
					return nil, err
				}
			}
			r.columns = setConfigMember(r.columns, col, exportCellJSON(cells[i]))
		}
		for _, column := range []string{"system_prompt", "tool_names"} {
			resolved, ok := memberValue(r.columns, "_"+column+"_resolved")
			if !ok {
				continue
			}
			r.columns = dropMember(r.columns, "_"+column+"_resolved")
			if _, has := memberValue(r.columns, column); has {
				r.columns = setConfigMember(r.columns, column, resolved)
			}
		}
		r.session.liftMarkers()
		if r.session.ResetFrom != "" {
			r.columns = setConfigMember(r.columns, "_reset_from", exportCellJSON(r.session.ResetFrom))
		}
		if r.session.BranchedFrom != "" {
			r.columns = setConfigMember(r.columns, "_branched_from", exportCellJSON(r.session.BranchedFrom))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) readSessionRecords(ctx context.Context, query string, args ...any) ([]sessionRecord, error) {
	var out []sessionRecord
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = querySessionRecords(c.QueryContext(ctx, query, args...))
		return err
	})
	return out, err
}

func (s *Store) withMessages(ctx context.Context, r sessionRecord, o ExportOptions) (SessionExport, error) {
	messages, err := s.GetMessages(ctx, r.session.ID, MessageQuery{IncludeInactive: o.IncludeInactive, IncludeCompacted: o.IncludeCompacted})
	if err != nil {
		return SessionExport{}, err
	}
	return SessionExport{Session: r.session, Messages: messages, Timings: ExportTimingsOf(messages, r.session.ID), columns: r.columns}, nil
}

func (s *Store) ExportSession(ctx context.Context, sessionID string, o ExportOptions) (SessionExport, bool, error) {
	s.tokenBarrier(ctx, true)
	records, err := s.readSessionRecords(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		"COALESCE(tp.prompt, s.tool_names) AS _tool_names_resolved "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
		"LEFT JOIN system_prompts tp ON tp.hash = s.tool_names WHERE s.id = ?", sessionID)
	if err != nil || len(records) == 0 {
		return SessionExport{}, false, err
	}
	out, err := s.withMessages(ctx, records[0], o)
	return out, err == nil, err
}

func (s *Store) ExportSessionLineage(ctx context.Context, sessionID string, o ExportOptions) (SessionExport, bool, error) {
	lineage, err := s.CompressionLineage(ctx, sessionID)
	if err != nil || len(lineage) == 0 {
		return SessionExport{}, false, err
	}
	var segments []SessionExport
	for _, sid := range lineage {
		seg, ok, err := s.ExportSession(ctx, sid, o)
		if err != nil {
			return SessionExport{}, false, err
		}
		if ok {
			segments = append(segments, seg)
		}
	}
	if len(segments) == 0 {
		return SessionExport{}, false, nil
	}
	var messages []MessageRow
	ids := make([]string, len(segments))
	for i, seg := range segments {
		messages = append(messages, seg.Messages...)
		ids[i] = seg.Session.ID
	}
	last := segments[len(segments)-1]
	out := SessionExport{Session: last.Session, Messages: messages, Timings: ExportTimingsOf(messages, sessionID),
		Segments: segments, LineageSessionIDs: ids, columns: append([]configMember{}, last.columns...)}
	out.Session.MessageCount = sql.Null[int64]{V: int64(len(messages)), Valid: true}
	out.columns = setConfigMember(out.columns, "message_count", rawInt(int64(len(messages))))
	return out, true, nil
}

func (s *Store) ExportAll(ctx context.Context, source string, o ExportOptions) ([]SessionExport, error) {
	var where []string
	var args []any
	if source != "" {
		clause, bound := sourceMatchClause([]string{source}, nil)
		where, args = append(where, clause), append(args, bound...)
	}
	records, err := s.readSessionRecords(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		SessionLastActiveSQL("s")+" AS last_active "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
		whereSQL(where, " ")+" "+
		"ORDER BY last_active DESC, s.started_at DESC, s.id DESC LIMIT ? OFFSET ?", append(args, 100000, 0)...)
	if err != nil {
		return nil, err
	}
	out := make([]SessionExport, 0, len(records))
	if o.IncludeCompacted {
		for _, r := range records {
			e, err := s.withMessages(ctx, r, ExportOptions{IncludeCompacted: true, IncludeInactive: o.IncludeInactive})
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	}
	bySession := map[string][]MessageRow{}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		if _, seen := bySession[r.session.ID]; !seen {
			ids = append(ids, r.session.ID)
			bySession[r.session.ID] = []MessageRow{}
		}
	}
	clause := activeClause(o.IncludeInactive, false)
	for _, chunk := range chunks(ids, sqlInChunk) {
		rows, err := s.readMsgRows(ctx, "SELECT * FROM messages WHERE session_id IN ("+bindMarks(len(chunk))+") "+
			clause+" ORDER BY session_id, id", stringArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		for _, r := range rowsToMessageRows(rows, "get_messages") {
			bySession[r.SessionID] = append(bySession[r.SessionID], r)
		}
	}
	for _, r := range records {
		messages := bySession[r.session.ID]
		out = append(out, SessionExport{Session: r.session, Messages: messages, Timings: ExportTimingsOf(messages, r.session.ID), columns: r.columns})
	}
	return out, nil
}

type AdoptionResult struct {
	ImportResult
	Adopted      bool
	DonorRetired bool
	Error        string
}

func (s *Store) AdoptSessionLineageFrom(ctx context.Context, donor *Store, sessionID string, retireDonor bool) (AdoptionResult, error) {
	payload, ok, err := donor.ExportSessionLineage(ctx, sessionID, ExportOptions{IncludeInactive: true})
	if err != nil {
		return AdoptionResult{}, err
	}
	if !ok {
		return AdoptionResult{Error: "session '" + sessionID + "' not found in donor store"}, nil
	}
	segments := payload.Segments
	if len(segments) == 0 {
		segments = []SessionExport{payload}
	}
	donorAhead := false
	for _, seg := range segments {
		segID := seg.Session.ID
		if segID == "" {
			continue
		}
		_, present, err := s.GetSession(ctx, segID)
		if err != nil {
			return AdoptionResult{}, err
		}
		if !present {
			continue
		}
		local, err := s.GetMessages(ctx, segID, MessageQuery{IncludeInactive: true})
		if err != nil {
			return AdoptionResult{}, err
		}
		if len(seg.Messages) > len(local) {
			donorAhead = true
			slog.Warn(fmt.Sprintf("adoption divergence: donor segment %s has %d messages, local copy has %d — donor will NOT be retired",
				segID, len(seg.Messages), len(local)))
		}
	}
	rows := make([]json.RawMessage, len(segments))
	for i, seg := range segments {
		if rows[i], err = seg.MarshalJSON(); err != nil {
			return AdoptionResult{}, err
		}
	}
	result, err := s.ImportSessions(ctx, rows)
	if err != nil {
		return AdoptionResult{}, err
	}
	adopted := result.OK && result.Imported+result.Skipped == len(segments)
	if !adopted {
		slog.Warn(fmt.Sprintf("adoption of %s did not complete: imported=%d skipped=%d of %d segment(s); errors=%s",
			sessionID, result.Imported, result.Skipped, len(segments), importErrorsText(result)))
	}
	retired := false
	if adopted && retireDonor && !donorAhead {
		retired = true
		for _, seg := range segments {
			if seg.Session.ID == "" {
				continue
			}
			if !s.retireDonorSegment(ctx, donor, seg.Session.ID) {
				retired = false
				break
			}
		}
	}
	return AdoptionResult{ImportResult: result, Adopted: adopted, DonorRetired: retired}, nil
}

func importErrorsText(r ImportResult) string {
	if len(r.Refusals) > 0 {
		enc, _ := json.Marshal(r.Refusals)
		return string(enc)
	}
	enc, _ := json.Marshal(r.Problems)
	return string(enc)
}

func (s *Store) retireDonorSegment(ctx context.Context, donor *Store, segID string) bool {
	retire := func() (bool, error) {
		donorNow, err := donor.GetMessages(ctx, segID, MessageQuery{IncludeInactive: true})
		if err != nil {
			return false, err
		}
		localNow, err := s.GetMessages(ctx, segID, MessageQuery{IncludeInactive: true})
		if err != nil {
			return false, err
		}
		if len(donorNow) > len(localNow) {
			slog.Warn(fmt.Sprintf("adoption divergence at retire time: donor segment %s grew to %d messages (local %d) — leaving donor unretired",
				segID, len(donorNow), len(localNow)))
			return false, nil
		}
		if err := donor.ReopenSession(ctx, segID); err != nil {
			return false, err
		}
		if err := donor.EndSession(ctx, segID, "adopted_by_profile"); err != nil {
			return false, err
		}
		if _, err := donor.SetSessionArchived(ctx, segID, true, false); err != nil {
			return false, err
		}
		return true, nil
	}
	ok, err := retire()
	if err != nil {
		slog.Warn(fmt.Sprintf("failed to retire donor segment %s after adoption", segID), "error", err)
		return false
	}
	return ok
}

type SessionCwd struct {
	Cwd        string
	Sessions   int64
	LastActive float64
}

func (s *Store) DistinctSessionCwds(ctx context.Context, includeArchived bool) ([]SessionCwd, error) {
	where := "cwd IS NOT NULL AND TRIM(cwd) != ''"
	if !includeArchived {
		where += " AND archived = 0"
	}
	var out []SessionCwd
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, "SELECT cwd AS cwd, COUNT(*) AS sessions, MAX(COALESCE(ended_at, started_at, 0)) AS last_active "+
			"FROM sessions WHERE "+where+" GROUP BY cwd")
		if err != nil {
			return err
		}
		for rows.Next() {
			var cwd, sessions, lastActive any
			if err := rows.Scan(&cwd, &sessions, &lastActive); err != nil {
				rows.Close()
				return err
			}
			text, _ := cellText(cwd)
			n, _ := cellInt(sessions)
			f, _ := cellFloat(lastActive)
			out = append(out, SessionCwd{Cwd: text, Sessions: n, LastActive: f})
		}
		return rowsDone(rows)
	})
	return out, err
}

const cronPolicyJobSQL = "(SELECT CASE WHEN json_valid(r) THEN json_extract(r, '$.cron_job.id') END FROM" +
	" (SELECT CASE WHEN json_valid(value) THEN json_extract(value, '$.policy.request_json') END AS r" +
	" FROM state_meta WHERE key = '" + localPolicyPrefix + "' || target_session_id))"

var cronAdmissionSQL = "principal_id = '" + CronPrincipalID + "' AND request_id >= :request_lo AND request_id < :request_hi" +
	" AND ((instr(substr(request_id, " + strconv.Itoa(len(CronRequestPrefix)+1) + "), ':') = length(:request_lo) - " + strconv.Itoa(len(CronRequestPrefix)) +
	" AND instr(substr(request_id, length(:request_lo) + 1), ':') = 0)" +
	" OR " + cronPolicyJobSQL + " = :job_id)"

var cronRunIDsSQL = "SELECT id FROM sessions WHERE source = 'cron' AND id >= :prefix AND id < :prefix_hi" +
	" AND substr(id, length(:prefix) + 1) GLOB '[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]_[0-9][0-9][0-9][0-9][0-9][0-9]'" +
	" UNION ALL SELECT target_session_id FROM session_admissions WHERE " + cronAdmissionSQL

var cronExecutionSQL = "(SELECT substr(request_id, length(:request_lo) + 1) FROM session_admissions" +
	" WHERE target_session_id = s.id AND " + cronAdmissionSQL + " LIMIT 1)"

type CronRunListing struct {
	SessionListing
	CronJobID       string
	CronExecutionID sql.Null[string]
}

func (s *Store) ListCronJobRuns(ctx context.Context, jobID string, limit, offset int) ([]CronRunListing, error) {
	prefix, prefixHi, _ := PrefixRange("cron_" + jobID + "_")
	requestLo, requestHi := CronRequestRange(jobID)
	query := `
            SELECT s.*, ` + cronExecutionSQL + ` AS cron_execution_id,
                COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved,
                ` + previewRawSQL("s.id") + `,
                ` + SessionLastActiveSQL("s") + ` AS last_active
            FROM sessions s
            LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash
            WHERE s.source = 'cron' AND s.id IN (` + cronRunIDsSQL + `)
            ORDER BY s.started_at DESC, s.id DESC
            LIMIT :limit OFFSET :offset
        `
	var out []CronRunListing
	err := s.Read(ctx, func(c Conn) error {
		rows, err := c.QueryContext(ctx, query, sql.Named("prefix", prefix), sql.Named("prefix_hi", prefixHi),
			sql.Named("request_lo", requestLo), sql.Named("request_hi", requestHi), sql.Named("job_id", jobID),
			sql.Named("limit", limit), sql.Named("offset", offset))
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanRecords(rows, func(r *CronRunListing, col string) any {
			if col == "cron_execution_id" {
				return &r.CronExecutionID
			}
			return r.SessionListing.dest(col)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].liftMarkers()
		out[i].Preview = shapePreview(out[i].Preview)
		out[i].LineageRootID = out[i].lineageRoot.String
		out[i].CronJobID = jobID
	}
	return out, nil
}

type CronRun struct {
	JobID       string
	ExecutionID sql.Null[string]
}

func (s *Store) CronRunIdentity(ctx context.Context, sessionID string) (CronRun, bool, error) {
	if job, ok := LegacyCronRunJob(sessionID); ok {
		return CronRun{JobID: job}, true, nil
	}
	var requestID, policyJob any
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT request_id, "+cronPolicyJobSQL+" AS policy_job FROM session_admissions"+
			" WHERE principal_id = ? AND target_session_id = ? ORDER BY seq LIMIT 1", CronPrincipalID, sessionID).Scan(&requestID, &policyJob)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil || !found {
		return CronRun{}, false, err
	}
	request, _ := cellText(requestID)
	job, execution, ok := CronRunFromRequest(request, "")
	if !ok {
		if policy, isText := policyJob.(string); isText {
			job, execution, ok = CronRunFromRequest(request, policy)
		}
	}
	if !ok {
		return CronRun{}, false, nil
	}
	return CronRun{JobID: job, ExecutionID: sql.Null[string]{V: execution, Valid: true}}, true, nil
}

type ScaffoldedSession struct {
	ID      string
	Title   string
	Content string
}

func (s *Store) ListSkillScaffoldedSessions(ctx context.Context, limit int) ([]ScaffoldedSession, error) {
	var out []ScaffoldedSession
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, `
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
                `, skillInvocationPrefix+"%", autoLoadPrefix+"%", limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, title, content any
			if err := rows.Scan(&id, &title, &content); err != nil {
				rows.Close()
				return err
			}
			var row ScaffoldedSession
			row.ID, _ = cellText(id)
			row.Title, _ = cellText(title)
			row.Content, _ = cellText(content)
			out = append(out, row)
		}
		return rowsDone(rows)
	})
	return out, err
}

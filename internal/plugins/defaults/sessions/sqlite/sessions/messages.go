package sessions

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const messageColumns = `id, role, COALESCE(content, ''), COALESCE(tool_calls, ''), COALESCE(tool_call_id, ''), COALESCE(tool_name, ''), timestamp,
	COALESCE(token_count, 0), COALESCE(finish_reason, ''), COALESCE(reasoning, ''), failed, payload, active, compacted, summary, COALESCE(api_content, ''),
	COALESCE(display_kind, ''), model_only, message_uid, COALESCE(absorbed_message_uids, ''), COALESCE(platform_message_id, '')`

var roles = map[sessionsv1.Role]string{
	sessionsv1.Role_ROLE_USER:      "user",
	sessionsv1.Role_ROLE_ASSISTANT: "assistant",
	sessionsv1.Role_ROLE_TOOL:      "tool",
	sessionsv1.Role_ROLE_SYSTEM:    "system",
}

func role(text string) sessionsv1.Role {
	for value, name := range roles {
		if name == text {
			return value
		}
	}
	return sessionsv1.Role_ROLE_UNSPECIFIED
}

// Tool call as stored, in the shape providers send it
type storedCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func scanMessage(row scanner) (*sessionsv1.Message, error) {
	m := &sessionsv1.Message{}
	var roleText, calls, absorbed string
	var at float64
	var payload []byte
	var failed, compacted, summary, modelOnly int
	var active int
	if err := row.Scan(&m.Id, &roleText, &m.Text, &calls, &m.ToolCallId, &m.ToolName, &at, &m.Tokens, &m.FinishReason, &m.Reasoning, &failed, &payload,
		&active, &compacted, &summary, &m.Sent, &m.Display, &modelOnly, &m.Uid, &absorbed, &m.PlatformId); err != nil {
		return nil, err
	}
	m.Removed = active == 0 && compacted == 0
	m.Role, m.Time = role(roleText), timestamppb.New(fromSeconds(at))
	m.Failed, m.Compacted, m.Summary, m.ModelOnly = failed != 0, compacted != 0, summary != 0, modelOnly != 0
	if calls != "" {
		var stored []storedCall
		if err := json.Unmarshal([]byte(calls), &stored); err == nil {
			for _, call := range stored {
				m.ToolCalls = append(m.ToolCalls, &sessionsv1.ToolCall{Id: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
			}
		}
	}
	if absorbed != "" {
		json.Unmarshal([]byte(absorbed), &m.Absorbed)
	}
	if len(payload) > 0 {
		var extra sessionsv1.Message
		if err := proto.Unmarshal(payload, &extra); err != nil {
			return nil, err
		}
		m.Carriers, m.Media, m.Shown = extra.GetCarriers(), extra.GetMedia(), extra.GetShown()
	}
	return m, nil
}

// Visibility of the messages a view reads
func visible(view sessions.View) string {
	switch view {
	case sessions.Display:
		return " AND (active = 1 OR compacted = 1) AND model_only = 0"
	case sessions.All:
		return ""
	}
	return " AND active = 1"
}

func (s *store) Messages(ctx context.Context, id string, view sessions.View) ([]*sessionsv1.Message, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return messages(ctx, db, id, view)
}

func messages(ctx context.Context, q querier, id string, view sessions.View) ([]*sessionsv1.Message, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE session_id = ?`+visible(view)+` ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionsv1.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) Append(ctx context.Context, id string, guard sessions.Guard, list ...*sessionsv1.Message) error {
	if len(list) == 0 {
		return nil
	}
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		var reason sql.NullString
		var child sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT end_reason, (SELECT id FROM sessions c WHERE c.parent_session_id = sessions.id AND `+
			configKey("c", "_branched_from")+` IS NULL AND `+configKey("c", "_delegate_from")+` IS NULL LIMIT 1) FROM sessions WHERE id = ?`, id).
			Scan(&reason, &child)
		if errors.Is(err, sql.ErrNoRows) {
			return sessions.ErrNotFound
		}
		if err != nil {
			return err
		}
		if reason.String == sessions.EndCompression && child.Valid {
			return fmt.Errorf("The session %s was summarized into a new session, so nothing more is saved to it. Go on in the new session.", id)
		}
		if err := checkGuard(ctx, tx, id, guard); err != nil {
			return err
		}
		calls := 0
		for _, m := range list {
			if err := insert(ctx, tx, id, m); err != nil {
				return err
			}
			calls += len(m.GetToolCalls())
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET message_count = message_count + ?, tool_call_count = tool_call_count + ?,
			last_activity_at = MAX(COALESCE(last_activity_at, 0), ?) WHERE id = ?`, len(list), calls, now(), id)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

// Refuses a transcript write the guard does not allow: the session's turn taken over by another holder, or compression of it under way
// by another writer
func checkGuard(ctx context.Context, tx *sql.Tx, id string, guard sessions.Guard) error {
	var holder string
	var expires float64
	err := tx.QueryRowContext(ctx, `SELECT holder, expires_at FROM compression_locks WHERE session_id = ?`, id).Scan(&holder, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case expires < now() || holderDead(holder):
		if _, err := tx.ExecContext(ctx, `DELETE FROM compression_locks WHERE session_id = ? AND holder = ?`, id, holder); err != nil {
			return err
		}
	case holder != guard.Compression:
		return fmt.Errorf("Another icebath is summarizing the session %s right now, so this was not saved to it.", id)
	}
	if guard.Turn == "" {
		return nil
	}
	err = tx.QueryRowContext(ctx, `SELECT holder, expires_at FROM session_turn_leases WHERE session_id = ?`, id).Scan(&holder, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && holder != guard.Turn {
		return fmt.Errorf("Another icebath took over the session %s, so this reply was not saved to it.", id)
	}
	if err != nil {
		return err
	}
	if expires <= now() {
		_, err = tx.ExecContext(ctx, `UPDATE session_turn_leases SET expires_at = ? WHERE session_id = ? AND holder = ?`, now()+300, id, holder)
	}
	return err
}

// Stores one message of the session, giving it its id, time and uid
func insert(ctx context.Context, q querier, id string, m *sessionsv1.Message) error {
	var calls any
	if len(m.GetToolCalls()) > 0 {
		stored := make([]storedCall, len(m.GetToolCalls()))
		for i, call := range m.GetToolCalls() {
			stored[i].ID, stored[i].Type = call.GetId(), "function"
			stored[i].Function.Name, stored[i].Function.Arguments = call.GetName(), call.GetArguments()
		}
		data, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		calls = string(data)
	}
	var payload []byte
	if len(m.GetCarriers()) > 0 || len(m.GetMedia()) > 0 || m.GetShown() != "" {
		var err error
		if payload, err = proto.Marshal(&sessionsv1.Message{Carriers: m.GetCarriers(), Media: m.GetMedia(), Shown: m.GetShown()}); err != nil {
			return err
		}
	}
	var absorbed any
	if len(m.GetAbsorbed()) > 0 {
		data, err := json.Marshal(m.GetAbsorbed())
		if err != nil {
			return err
		}
		absorbed = string(data)
	}
	at := now()
	if m.GetTime() != nil {
		at = seconds(m.GetTime().AsTime())
	}
	if m.Uid == "" {
		m.Uid = strs.Random(16)
	}
	content := strings.ToValidUTF8(m.GetText(), "�")
	result, err := q.ExecContext(ctx, `INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, tool_name, timestamp, token_count,
		finish_reason, reasoning, failed, payload, active, compacted, summary, api_content, display_kind, model_only, message_uid, absorbed_message_uids,
		platform_message_id) VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, 0), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''),
		NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''))`,
		id, roles[m.GetRole()], content, calls, m.GetToolCallId(), m.GetToolName(), at, m.GetTokens(), m.GetFinishReason(), m.GetReasoning(),
		boolInt(m.GetFailed()), payload, boolInt(!m.GetCompacted() && !m.GetRemoved()), boolInt(m.GetCompacted()), boolInt(m.GetSummary()), m.GetSent(), m.GetDisplay(),
		boolInt(m.GetModelOnly()), m.GetUid(), absorbed, m.GetPlatformId())
	if err != nil {
		return err
	}
	if m.Id, err = result.LastInsertId(); err != nil {
		return err
	}
	m.Time = timestamppb.New(fromSeconds(at))
	return nil
}

func (s *store) Rewind(ctx context.Context, id string, message int64) (int, error) {
	count := 0
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		if busy, err := guarded(ctx, tx, []string{id}); err != nil {
			return err
		} else if len(busy) > 0 {
			return errors.New("A reply is running in this session. Stop it first.")
		}
		var roleText, uid string
		var active, compacted int
		err := tx.QueryRowContext(ctx, `SELECT role, active, compacted, message_uid FROM messages WHERE id = ? AND session_id = ?`, message, id).
			Scan(&roleText, &active, &compacted, &uid)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("The session %s has no message %d to go back to.", id, message)
		}
		if err != nil {
			return err
		}
		if roleText != "user" {
			return fmt.Errorf("The chat can go back only to a message you sent, and message %d is not one.", message)
		}
		// The message as the person saw it, the first stored under its uid, and the copy compaction left of it for the model
		origin := message
		var first sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(id) FROM messages WHERE session_id = ? AND message_uid = ? AND compacted = 1`, id, uid).
			Scan(&first); err != nil {
			return err
		}
		if first.Valid && first.Int64 < origin {
			origin = first.Int64
		}
		if active == 0 {
			if compacted == 0 {
				return errors.New("That message is no longer part of the session, so the chat can't go back to it.")
			}
			var live int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND message_uid = ? AND active = 1 ORDER BY id LIMIT 1`, id, uid).
				Scan(&live)
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("This message was summarized when the session was compressed, so the chat can't be rewound to it.")
			}
			if err != nil {
				return err
			}
			message = live
		}
		result, err := tx.ExecContext(ctx, `UPDATE messages SET active = 0 WHERE session_id = ? AND id >= ? AND active = 1`, id, message)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		count = int(n)
		// What the person still saw from the target on, the messages compaction folded away, goes with the copies of them
		if origin < message {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET compacted = 0 WHERE session_id = ? AND id >= ? AND compacted = 1 AND active = 0`,
				id, origin); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET rewind_count = rewind_count + 1 WHERE id = ?`, id); err != nil {
			return err
		}
		return recount(ctx, tx, id)
	})
	if err == nil {
		s.changed(id)
	}
	return count, err
}

func (s *store) Around(ctx context.Context, id string, message int64, window, bookend int, keep []sessionsv1.Role) (sessions.Window, error) {
	db, err := s.read(ctx)
	if err != nil {
		return sessions.Window{}, err
	}
	var found int
	err = db.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE id = ? AND session_id = ?`, message, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return sessions.Window{}, nil
	}
	if err != nil {
		return sessions.Window{}, err
	}
	window = max(window, 0)
	before, err := query(ctx, db, `SELECT `+messageColumns+` FROM messages WHERE session_id = ? AND id <= ? ORDER BY id DESC LIMIT ?`, id, message, window+1)
	if err != nil {
		return sessions.Window{}, err
	}
	after, err := query(ctx, db, `SELECT `+messageColumns+` FROM messages WHERE session_id = ? AND id > ? ORDER BY id LIMIT ?`, id, message, window)
	if err != nil {
		return sessions.Window{}, err
	}
	reverse(before)
	all := append(before, after...)
	out := sessions.Window{Before: max(len(before)-1, 0), After: len(after)}
	for _, m := range all {
		if keep == nil || m.GetId() == message || slices.Contains(keep, m.GetRole()) {
			out.Messages = append(out.Messages, m)
		}
	}
	if bookend <= 0 || len(all) == 0 {
		return out, nil
	}
	clause, args := "", []any{}
	if keep != nil {
		names := make([]string, len(keep))
		for i, r := range keep {
			names[i] = roles[r]
		}
		clause, args = " AND role IN ("+marks(len(names))+")", strings2any(names)
	}
	if out.Start, err = query(ctx, db, `SELECT `+messageColumns+` FROM messages WHERE session_id = ? AND id < ?`+clause+
		` AND length(content) > 0 ORDER BY id LIMIT ?`, append(append([]any{id, all[0].GetId()}, args...), bookend)...); err != nil {
		return out, err
	}
	if out.End, err = query(ctx, db, `SELECT `+messageColumns+` FROM messages WHERE session_id = ? AND id > ?`+clause+
		` AND length(content) > 0 ORDER BY id DESC LIMIT ?`, append(append([]any{id, all[len(all)-1].GetId()}, args...), bookend)...); err != nil {
		return out, err
	}
	reverse(out.End)
	return out, nil
}

func reverse(list []*sessionsv1.Message) {
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
}

func query(ctx context.Context, q querier, text string, args ...any) ([]*sessionsv1.Message, error) {
	rows, err := q.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionsv1.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) Prompts(ctx context.Context, id string, limit int) ([]*sessionsv1.Prompt, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.timestamp, COALESCE(m.content, ''), length(COALESCE(m.payload, '')) > 0,
		COALESCE((SELECT MAX(p.id) FROM messages p WHERE p.session_id = m.session_id AND p.active = 1 AND p.id < m.id), 0)
		FROM messages m WHERE m.session_id = ? AND m.role = 'user' AND m.active = 1 AND m.summary = 0
		AND (m.display_kind IS NULL OR m.display_kind = '' OR m.display_kind = 'steer') ORDER BY m.id DESC LIMIT ?`, id, limit*2+5)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionsv1.Prompt
	for rows.Next() && len(out) < limit {
		var p sessionsv1.Prompt
		var at float64
		var content string
		if err := rows.Scan(&p.Id, &at, &content, &p.Media, &p.Through); err != nil {
			return nil, err
		}
		if sessions.IsSummary(content) {
			continue
		}
		p.Text = content
		if described, ok := sessions.DescribeSkill(content, " — "); ok && described != "" {
			content = described
		}
		p.Preview = strings.Join(strings.Fields(content), " ")
		if runes := []rune(p.Preview); len(runes) > 80 {
			p.Preview = string(runes[:77]) + "..."
		}
		p.Time = timestamppb.New(fromSeconds(at))
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *store) Export(ctx context.Context, request *sessionsv1.ExportRequest) ([]*sessionsv1.Transcript, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	filter := request.GetFilter()
	where, args := s.where(filter, true)
	if !filter.GetIncludeHidden() && len(filter.GetIds()) == 0 {
		where = append(where, "s.hidden = 0")
	}
	rows, err := db.QueryContext(ctx, `SELECT s.id FROM sessions s`+whereSQL(where)+` ORDER BY s.started_at`, args...)
	if err != nil {
		return nil, err
	}
	ids, err := collect(rows)
	if err != nil {
		return nil, err
	}
	if request.GetLineage() {
		var all []string
		for _, id := range ids {
			chain, err := chainOf(ctx, db, id)
			if err != nil {
				return nil, err
			}
			for _, member := range chain {
				if !slices.Contains(all, member) {
					all = append(all, member)
				}
			}
		}
		ids = all
	}
	var out []*sessionsv1.Transcript
	for _, id := range ids {
		session, err := get(ctx, db, id)
		if err != nil {
			return nil, err
		}
		list, err := messages(ctx, db, id, sessions.All)
		if err != nil {
			return nil, err
		}
		prompt, err := systemPrompt(ctx, db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, &sessionsv1.Transcript{Session: session, Messages: list, SystemPrompt: prompt})
	}
	return out, nil
}

// Compression chain the session is in, oldest first
func chainOf(ctx context.Context, q querier, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE chain(id, depth) AS (
			SELECT ?, 0
			UNION SELECT p.id, chain.depth - 1 FROM chain JOIN sessions c ON c.id = chain.id JOIN sessions p ON p.id = c.parent_session_id WHERE `+continuation+`
			UNION SELECT c.id, chain.depth + 1 FROM chain JOIN sessions p ON p.id = chain.id JOIN sessions c ON c.parent_session_id = p.id WHERE `+continuation+`
		) SELECT id FROM chain GROUP BY id ORDER BY MIN(depth)`, id)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

func (s *store) Import(ctx context.Context, transcripts []*sessionsv1.Transcript) (*sessionsv1.ImportResponse, error) {
	out := &sessionsv1.ImportResponse{}
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		out.Ids, out.Skipped = nil, nil
		for _, transcript := range orderParents(transcripts) {
			session := transcript.GetSession()
			if session.GetId() == "" {
				return errors.New("An imported session has no id, so nothing was imported.")
			}
			var found int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, session.GetId()).Scan(&found)
			if err == nil {
				out.Skipped = append(out.Skipped, session.GetId())
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := importSession(ctx, tx, transcript); err != nil {
				return fmt.Errorf("The session %s could not be imported: %w", session.GetId(), err)
			}
			out.Ids = append(out.Ids, session.GetId())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.changed(out.GetIds()...)
	return out, nil
}

// Transcripts with every parent before its children
func orderParents(transcripts []*sessionsv1.Transcript) []*sessionsv1.Transcript {
	var out []*sessionsv1.Transcript
	placed := map[string]bool{}
	ids := map[string]bool{}
	for _, t := range transcripts {
		ids[t.GetSession().GetId()] = true
	}
	for len(out) < len(transcripts) {
		progress := false
		for _, t := range transcripts {
			id, parent := t.GetSession().GetId(), t.GetSession().GetParentId()
			if placed[id] || parent != "" && ids[parent] && !placed[parent] {
				continue
			}
			out, placed[id], progress = append(out, t), true, true
		}
		if !progress {
			for _, t := range transcripts {
				if !placed[t.GetSession().GetId()] {
					out, placed[t.GetSession().GetId()] = append(out, t), true
				}
			}
		}
	}
	return out
}

func importSession(ctx context.Context, tx *sql.Tx, t *sessionsv1.Transcript) error {
	s := t.GetSession()
	var parent any
	if s.GetParentId() != "" {
		var found int
		if tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, s.GetParentId()).Scan(&found) == nil {
			parent = s.GetParentId()
		}
	}
	started := now()
	if s.GetCreated() != nil {
		started = seconds(s.GetCreated().AsTime())
	}
	var ended, active any
	if s.GetEnded() != nil {
		ended = seconds(s.GetEnded().AsTime())
	}
	if s.GetUpdated() != nil {
		active = seconds(s.GetUpdated().AsTime())
	}
	usage, peer := s.GetUsage(), s.GetPeer()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, profile, source, created_source, user_id, session_key, chat_id, chat_type, thread_id,
		display_name, origin_json, model, model_config, parent_session_id, started_at, ended_at, end_reason, api_call_count, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, reasoning_tokens, cwd, git_branch, git_repo_root, billing_provider, billing_base_url, estimated_cost_usd,
		actual_cost_usd, cost_status, cost_source, pricing_version, last_activity_at, rewind_count, archived, pinned, hidden, yolo)
		VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
		NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?,
		NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`,
		s.GetId(), cmp.Or(s.GetProfile(), "default"), cmp.Or(s.GetSource(), "import"), s.GetCreatedSource(), peer.GetUserId(), peer.GetSessionKey(),
		peer.GetChatId(), peer.GetChatType(), peer.GetThreadId(), peer.GetDisplayName(), peer.GetOrigin(), s.GetModel(), s.GetModelConfig(), parent, started,
		ended, s.GetEndReason(), usage.GetApiCalls(), usage.GetInput(), usage.GetOutput(), usage.GetCacheRead(), usage.GetCacheWrite(), usage.GetReasoning(),
		s.GetDirectory(), s.GetGitBranch(), s.GetGitRoot(), s.GetProvider(), s.GetBaseUrl(), usage.GetEstimatedCost(), usage.GetActualCost(),
		usage.GetCostStatus(), usage.GetCostSource(), usage.GetPricingVersion(), active, s.GetRewinds(), boolInt(s.GetArchived()), boolInt(s.GetPinned()),
		boolInt(s.GetHidden()), boolInt(s.GetYolo())); err != nil {
		return err
	}
	for _, m := range t.GetMessages() {
		copied := proto.Clone(m).(*sessionsv1.Message)
		copied.Id = 0
		if err := insert(ctx, tx, s.GetId(), copied); err != nil {
			return err
		}
	}
	if err := recount(ctx, tx, s.GetId()); err != nil {
		return err
	}
	if t.GetSystemPrompt() != "" {
		if err := storePrompt(ctx, tx, s.GetId(), t.GetSystemPrompt(), nil); err != nil {
			return err
		}
	}
	if title := s.GetTitle(); title != "" {
		stored, err := setTitle(ctx, tx, s.GetId(), title, cmp.Or(s.GetTitleSource(), sessions.TitleUser))
		if err != nil && stored == "" {
			next, nextErr := nextTitle(ctx, tx, s.GetProfile(), title)
			if nextErr != nil {
				return nextErr
			}
			if _, err := setTitle(ctx, tx, s.GetId(), next, cmp.Or(s.GetTitleSource(), sessions.TitleUser)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *store) Locate(ctx context.Context, message int64) (string, *sessionsv1.Message, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", nil, err
	}
	var session string
	if err := db.QueryRowContext(ctx, `SELECT session_id FROM messages WHERE id = ?`, message).Scan(&session); errors.Is(err, sql.ErrNoRows) {
		return "", nil, sessions.ErrNotFound
	} else if err != nil {
		return "", nil, err
	}
	found, err := scanMessage(db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, message))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, sessions.ErrNotFound
	}
	return session, found, err
}

package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

const DeleteReasonProfileRepair = "profile_repair"

var messageMoveSkip = []string{"id", "display_order"}

type SessionLedgeredError struct {
	SessionID string
}

func (e *SessionLedgeredError) Error() string {
	return e.SessionID + " has gateway admission/worker history; not moved"
}

func SessionKeyProfile(sessionKey sql.Null[string]) (string, bool) {
	if !sessionKey.Valid {
		return "", false
	}
	parts := strings.Split(sessionKey.V, ":")
	if len(parts) < 3 || parts[0] != "agent" || parts[1] == "" {
		return "", false
	}
	return ProfileFromSessionKeyNamespace(parts[1]), true
}

func storedProfile(v sql.Null[string]) string { return strings.TrimSpace(v.V) }

func tableColumnNames(ctx context.Context, c Conn, table string) ([]string, error) {
	rows, err := c.QueryContext(ctx, "PRAGMA table_info('"+table+"')")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var cid, name, kind, notNull, dflt, pk any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return nil, err
		}
		text, _ := cellText(name)
		names = append(names, text)
	}
	return names, rowsDone(rows)
}

type MislabelledSession struct {
	ID          string
	SessionKey  string
	ProfileName string
	KeyProfile  string
}

type ForeignSession struct {
	ID              string
	SessionKey      string
	KeyProfile      string
	MessageCount    int64
	ParentSessionID sql.Null[string]
	Ledgered        bool
}

type CrossedParent struct {
	ID              string
	KeyProfile      string
	ParentSessionID sql.Null[string]
	ParentProfile   string
}

type CrossedProfileSessions struct {
	Mislabelled    []MislabelledSession
	Foreign        []ForeignSession
	CrossedParents []CrossedParent
}

func (s *Store) FindCrossedProfileSessions(ctx context.Context, owner string) (CrossedProfileSessions, error) {
	type keyedRow struct {
		id, sessionKey, profileName, parentID, parentKey sql.Null[string]
		messageCount                                     sql.Null[int64]
	}
	var rows []keyedRow
	err := s.Read(ctx, func(c Conn) error {
		rows = nil
		r, err := c.QueryContext(ctx, "SELECT s.id, s.session_key, s.profile_name, s.parent_session_id, s.message_count, "+
			"       p.session_key AS parent_session_key "+
			"FROM sessions s LEFT JOIN sessions p ON p.id = s.parent_session_id "+
			"WHERE s.session_key LIKE 'agent:%' ORDER BY s.started_at, s.id")
		if err != nil {
			return err
		}
		for r.Next() {
			var row keyedRow
			if err := r.Scan(lenient{&row.id}, lenient{&row.sessionKey}, lenient{&row.profileName}, lenient{&row.parentID},
				lenient{&row.messageCount}, lenient{&row.parentKey}); err != nil {
				_ = r.Close()
				return err
			}
			rows = append(rows, row)
		}
		return rowsDone(r)
	})
	if err != nil {
		return CrossedProfileSessions{}, err
	}
	ledgered, err := s.ledgeredSessionIDs(ctx)
	if err != nil {
		return CrossedProfileSessions{}, err
	}
	var found CrossedProfileSessions
	for _, row := range rows {
		keyProfile, ok := SessionKeyProfile(row.sessionKey)
		if !ok {
			continue
		}
		if label := storedProfile(row.profileName); label != "" && label != keyProfile {
			found.Mislabelled = append(found.Mislabelled, MislabelledSession{ID: row.id.V, SessionKey: row.sessionKey.V,
				ProfileName: label, KeyProfile: keyProfile})
		}
		if keyProfile != owner {
			found.Foreign = append(found.Foreign, ForeignSession{ID: row.id.V, SessionKey: row.sessionKey.V, KeyProfile: keyProfile,
				MessageCount: row.messageCount.V, ParentSessionID: row.parentID, Ledgered: ledgered[row.id.V]})
		}
		if parentProfile, ok := SessionKeyProfile(row.parentKey); ok && parentProfile != keyProfile {
			found.CrossedParents = append(found.CrossedParents, CrossedParent{ID: row.id.V, KeyProfile: keyProfile,
				ParentSessionID: row.parentID, ParentProfile: parentProfile})
		}
	}
	return found, nil
}

func ledgeredSessionIDsIn(ctx context.Context, c Conn) (map[string]bool, error) {
	tables, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type='table' "+
		"AND name IN ('session_admissions', 'worker_executions')")
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, q := range []struct{ table, query string }{
		{"session_admissions", "SELECT DISTINCT target_session_id FROM session_admissions"},
		{"worker_executions", "SELECT DISTINCT session_id FROM worker_executions"},
	} {
		if !containsString(tables, q.table) {
			continue
		}
		found, err := queryStrings(ctx, c, q.query)
		if err != nil {
			return nil, err
		}
		for _, id := range found {
			ids[id] = true
		}
	}
	return ids, nil
}

func (s *Store) ledgeredSessionIDs(ctx context.Context) (map[string]bool, error) {
	var ids map[string]bool
	err := s.Read(ctx, func(c Conn) error {
		var err error
		ids, err = ledgeredSessionIDsIn(ctx, c)
		return err
	})
	return ids, err
}

type GatewayRoutingRow struct {
	Scope      string
	SessionKey string
	EntryJSON  string
	UpdatedAt  float64
}

func (s *Store) ListGatewayRoutingRows(ctx context.Context) ([]GatewayRoutingRow, error) {
	var out []GatewayRoutingRow
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, "SELECT scope, session_key, entry_json, updated_at FROM gateway_routing "+
			"ORDER BY scope, session_key")
		if err != nil {
			return err
		}
		for rows.Next() {
			var r GatewayRoutingRow
			if err := rows.Scan(lenient{&r.Scope}, lenient{&r.SessionKey}, lenient{&r.EntryJSON}, lenient{&r.UpdatedAt}); err != nil {
				_ = rows.Close()
				return err
			}
			out = append(out, r)
		}
		return rowsDone(rows)
	})
	return out, err
}

type TopicRelabel struct {
	ChatID     string
	ThreadID   string
	SessionKey string
	KeyProfile string
}

func (s *Store) FindProfileLessTelegramTopicRows(ctx context.Context) ([]TopicRelabel, error) {
	var rows []TopicRelabel
	err := s.Read(ctx, func(c Conn) error {
		rows = nil
		existing, err := queryStrings(ctx, c, "SELECT name FROM sqlite_master WHERE type='table' AND name = 'telegram_dm_topic_bindings'")
		if err != nil || len(existing) == 0 {
			return err
		}
		columns, err := tableColumnNames(ctx, c, "telegram_dm_topic_bindings")
		if err != nil || !containsString(columns, "profile_name") {
			return err
		}
		r, err := c.QueryContext(ctx, "SELECT chat_id, thread_id, session_key FROM telegram_dm_topic_bindings "+
			"WHERE profile_name = 'default' ORDER BY chat_id, thread_id")
		if err != nil {
			return err
		}
		for r.Next() {
			var row TopicRelabel
			if err := r.Scan(lenient{&row.ChatID}, lenient{&row.ThreadID}, lenient{&row.SessionKey}); err != nil {
				_ = r.Close()
				return err
			}
			rows = append(rows, row)
		}
		return rowsDone(r)
	})
	if err != nil {
		return nil, err
	}
	var out []TopicRelabel
	for _, row := range rows {
		keyProfile, ok := SessionKeyProfile(sql.Null[string]{V: row.SessionKey, Valid: true})
		if ok && keyProfile != home.DefaultProfile {
			row.KeyProfile = keyProfile
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *Store) CountMessagesAll(ctx context.Context, sessionID string) (int64, error) {
	n, _, err := s.readInt(ctx, "SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID)
	return n, err
}

func (s *Store) RelabelSessionsToKeyProfile(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var changed int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		changed = 0
		for _, chunk := range chunks(ids, sqlInChunk) {
			type keyed struct{ id, key sql.Null[string] }
			var found []keyed
			rows, err := c.QueryContext(ctx, "SELECT id, session_key FROM sessions WHERE id IN ("+bindMarks(len(chunk))+")", stringArgs(chunk)...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var k keyed
				if err := rows.Scan(lenient{&k.id}, lenient{&k.key}); err != nil {
					_ = rows.Close()
					return err
				}
				found = append(found, k)
			}
			if err := rowsDone(rows); err != nil {
				return err
			}
			for _, k := range found {
				target, ok := SessionKeyProfile(k.key)
				if !ok {
					continue
				}
				res, err := c.ExecContext(ctx, "UPDATE sessions SET profile_name = ? WHERE id = ? AND profile_name IS NOT ?", target, k.id.V, target)
				if err != nil {
					return err
				}
				changed += affectedRows(res)
			}
		}
		return nil
	})
	return changed, err
}

func (s *Store) SeverCrossedParents(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var severed int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		severed = 0
		for _, chunk := range chunks(ids, sqlInChunk) {
			type pair struct{ id, key, parentKey sql.Null[string] }
			var found []pair
			rows, err := c.QueryContext(ctx, "SELECT s.id, s.session_key, p.session_key FROM sessions s "+
				"JOIN sessions p ON p.id = s.parent_session_id "+
				"WHERE s.id IN ("+bindMarks(len(chunk))+")", stringArgs(chunk)...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var p pair
				if err := rows.Scan(lenient{&p.id}, lenient{&p.key}, lenient{&p.parentKey}); err != nil {
					_ = rows.Close()
					return err
				}
				found = append(found, p)
			}
			if err := rowsDone(rows); err != nil {
				return err
			}
			for _, p := range found {
				mine, okMine := SessionKeyProfile(p.key)
				theirs, okTheirs := SessionKeyProfile(p.parentKey)
				if okMine && okTheirs && mine != theirs {
					res, err := c.ExecContext(ctx, "UPDATE sessions SET parent_session_id = NULL WHERE id = ?", p.id.V)
					if err != nil {
						return err
					}
					severed += affectedRows(res)
				}
			}
		}
		return nil
	})
	return severed, err
}

type rowValues struct {
	columns []string
	values  []any
}

func (r rowValues) value(col string) (any, bool) {
	for i, c := range r.columns {
		if c == col {
			return r.values[i], true
		}
	}
	return nil, false
}

func (r rowValues) with(col string, v any) rowValues {
	out := rowValues{columns: append([]string{}, r.columns...), values: append([]any{}, r.values...)}
	for i, c := range out.columns {
		if c == col {
			out.values[i] = v
			return out
		}
	}
	out.columns, out.values = append(out.columns, col), append(out.values, v)
	return out
}

func scanRowValues(rows *sql.Rows, err error) ([]rowValues, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []rowValues
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, rowValues{columns: cols, values: values})
	}
	return out, rows.Err()
}

type MovedSession struct {
	ID              string
	ParentSessionID sql.Null[string]
	session         rowValues
	systemPrompt    sql.Null[string]
	toolPin         sql.Null[string]
	messages        []rowValues
	usage           []rowValues
}

func (m MovedSession) MessageCount() int { return len(m.messages) }

func (s *Store) ExportSessionForMove(ctx context.Context, sessionID string) (MovedSession, bool, error) {
	var out MovedSession
	found := false
	err := s.Read(ctx, func(c Conn) error {
		found = false
		sessions, err := scanRowValues(c.QueryContext(ctx, "SELECT * FROM sessions WHERE id = ?", sessionID))
		if err != nil || len(sessions) == 0 {
			return err
		}
		stored := func(column string) (sql.Null[string], error) {
			v, _ := sessions[0].value(column)
			if !cellTruthy(v) {
				return sql.Null[string]{}, nil
			}
			var prompt sql.Null[string]
			err := c.QueryRowContext(ctx, "SELECT prompt FROM system_prompts WHERE hash = ?", v).Scan(lenient{&prompt})
			if errors.Is(err, sql.ErrNoRows) {
				return sql.Null[string]{}, nil
			}
			return prompt, err
		}
		out = MovedSession{session: sessions[0]}
		if out.systemPrompt, err = stored("system_prompt_hash"); err != nil {
			return err
		}
		if out.toolPin, err = stored("tool_names"); err != nil {
			return err
		}
		if out.messages, err = scanRowValues(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? ORDER BY id", sessionID)); err != nil {
			return err
		}
		if out.usage, err = scanRowValues(c.QueryContext(ctx, "SELECT * FROM session_model_usage WHERE session_id = ?", sessionID)); err != nil {
			return err
		}
		id, _ := sessions[0].value("id")
		out.ID, _ = cellText(id)
		parent, _ := sessions[0].value("parent_session_id")
		out.ParentSessionID.V, out.ParentSessionID.Valid = cellText(parent)
		found = true
		return nil
	})
	if err != nil || !found {
		return MovedSession{}, false, err
	}
	ledgered, err := s.ledgeredSessionIDs(ctx)
	if err != nil {
		return MovedSession{}, false, err
	}
	if ledgered[sessionID] {
		return MovedSession{}, false, &SessionLedgeredError{SessionID: sessionID}
	}
	return out, true, nil
}

func insertMovedRow(ctx context.Context, c Conn, table string, values rowValues, skip []string) error {
	declared, err := tableColumnNames(ctx, c, table)
	if err != nil {
		return err
	}
	var columns []string
	var args []any
	for _, col := range declared {
		v, ok := values.value(col)
		if !ok || containsString(skip, col) {
			continue
		}
		columns = append(columns, col)
		args = append(args, v)
	}
	_, err = c.ExecContext(ctx, "INSERT INTO "+table+" ("+strings.Join(columns, ", ")+") VALUES ("+bindMarks(len(columns))+")", args...)
	return err
}

type MoveOutcome string

const (
	MovePresent  MoveOutcome = "present"
	MoveImported MoveOutcome = "imported"
)

func (s *Store) ImportMovedSession(ctx context.Context, payload MovedSession, profileName string) (MoveOutcome, error) {
	sessionID := payload.ID
	var outcome MoveOutcome
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		exists, err := sessionExists(ctx, c, sessionID)
		if err != nil {
			return err
		}
		if exists {
			outcome = MovePresent
			return nil
		}
		session := payload.session.with("profile_name", profileName)
		if parent, _ := session.value("parent_session_id"); cellTruthy(parent) {
			present, err := sessionExists(ctx, c, mustText(parent))
			if err != nil {
				return err
			}
			if !present {
				session = session.with("parent_session_id", nil)
			}
		}
		if title, _ := session.value("title"); title != nil {
			var one int
			err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE title = ?", title).Scan(&one)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				suffix := " (" + sessionID[max(0, len(sessionID)-12):] + ")"
				runes := []rune(mustText(title))
				session = session.with("title", string(runes[:max(0, min(len(runes), MaxTitleLength-len([]rune(suffix))))])+suffix)
			}
		}
		var prompt *string
		if payload.systemPrompt.Valid {
			prompt = &payload.systemPrompt.V
		}
		promptHash, err := storeSystemPrompt(ctx, c, prompt)
		if err != nil {
			return err
		}
		session = session.with("system_prompt_hash", nullStringValue(promptHash))
		toolNames, _ := session.value("tool_names")
		if payload.toolPin.Valid || len(mustText(toolNames)) == 64 {
			var pin *string
			if payload.toolPin.Valid {
				pin = &payload.toolPin.V
			}
			pinHash, err := storeSystemPrompt(ctx, c, pin)
			if err != nil {
				return err
			}
			session = session.with("tool_names", nullStringValue(pinHash))
		}
		if err := insertMovedRow(ctx, c, "sessions", session, nil); err != nil {
			return err
		}
		for _, m := range payload.messages {
			if err := insertMovedRow(ctx, c, "messages", m.with("session_id", sessionID), messageMoveSkip); err != nil {
				return err
			}
		}
		for _, u := range payload.usage {
			if err := insertMovedRow(ctx, c, "session_model_usage", u.with("session_id", sessionID), nil); err != nil {
				return err
			}
		}
		outcome = MoveImported
		return nil
	})
	return outcome, err
}

func affectedRows(res sql.Result) int64 {
	n, _ := res.RowsAffected()
	return n
}

func mustText(v any) string {
	text, _ := cellText(v)
	return text
}

func nullStringValue(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func (s *Store) DeleteMovedSession(ctx context.Context, sessionID string) (bool, error) {
	deleted := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		deleted = false
		exists, err := sessionExists(ctx, c, sessionID)
		if err != nil || !exists {
			return err
		}
		for _, statement := range []string{
			"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?",
			"DELETE FROM messages WHERE session_id = ?",
			"DELETE FROM session_model_usage WHERE session_id = ?",
			"DELETE FROM sessions WHERE id = ?",
		} {
			if _, err := c.ExecContext(ctx, statement, sessionID); err != nil {
				return err
			}
		}
		if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if deleted {
		s.emitSessionDeleted(ctx, []string{sessionID}, DeleteReasonProfileRepair, sessionID, "")
	}
	return deleted, nil
}

func sessionKeyNamespace(profile string) string {
	switch profile {
	case "", home.DefaultProfile:
		return "agent:main"
	case "main":
		return "agent:main~"
	}
	return "agent:" + profile
}

func (s *Store) RekeyLegacyMainSessions(ctx context.Context, ids []string, profile string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	newNS := sessionKeyNamespace(profile) + ":"
	var changed int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		changed = 0
		for _, chunk := range chunks(ids, sqlInChunk) {
			res, err := c.ExecContext(ctx, "UPDATE sessions SET session_key = ? || substr(session_key, 12), profile_name = ? "+
				"WHERE id IN ("+bindMarks(len(chunk))+") AND substr(session_key, 1, 11) = 'agent:main:'",
				append([]any{newNS, profile}, stringArgs(chunk)...)...)
			if err != nil {
				return err
			}
			changed += affectedRows(res)
		}
		return nil
	})
	return changed, err
}

type RoutingRowKey struct {
	Scope      string
	SessionKey string
}

func (s *Store) DeleteGatewayRoutingRows(ctx context.Context, keys []RoutingRowKey) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	var removed int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		removed = 0
		for _, k := range keys {
			res, err := c.ExecContext(ctx, "DELETE FROM gateway_routing WHERE scope = ? AND session_key = ?", k.Scope, k.SessionKey)
			if err != nil {
				return err
			}
			removed += affectedRows(res)
		}
		return nil
	})
	return removed, err
}

func (s *Store) InsertGatewayRoutingRowsIfAbsent(ctx context.Context, rows []GatewayRoutingRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var inserted int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		inserted = 0
		for _, r := range rows {
			res, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO gateway_routing (scope, session_key, entry_json, updated_at) "+
				"VALUES (?, ?, ?, ?)", r.Scope, r.SessionKey, r.EntryJSON, r.UpdatedAt)
			if err != nil {
				return err
			}
			inserted += affectedRows(res)
		}
		return nil
	})
	return inserted, err
}

type TopicRelabelCounts struct {
	BindingsRelabelled        int64
	BindingsDuplicatesRemoved int64
	ModeRowsRelabelled        int64
}

func (s *Store) RelabelTelegramTopicRows(ctx context.Context, rows []TopicRelabel) (TopicRelabelCounts, error) {
	var counts TopicRelabelCounts
	if len(rows) == 0 {
		return counts, nil
	}
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		counts = TopicRelabelCounts{}
		modeColumns, err := tableColumnNames(ctx, c, "telegram_dm_topic_mode")
		if err != nil {
			return err
		}
		modeHasProfile := containsString(modeColumns, "profile_name")
		for _, row := range rows {
			target, ok := SessionKeyProfile(sql.Null[string]{V: row.SessionKey, Valid: true})
			if !ok || target == home.DefaultProfile {
				continue
			}
			var one int
			err := c.QueryRowContext(ctx, "SELECT 1 FROM telegram_dm_topic_bindings WHERE profile_name = ? AND chat_id = ? "+
				"AND thread_id = ?", target, row.ChatID, row.ThreadID).Scan(&one)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				res, err := c.ExecContext(ctx, "DELETE FROM telegram_dm_topic_bindings WHERE profile_name = 'default' "+
					"AND chat_id = ? AND thread_id = ?", row.ChatID, row.ThreadID)
				if err != nil {
					return err
				}
				counts.BindingsDuplicatesRemoved += affectedRows(res)
			} else {
				res, err := c.ExecContext(ctx, "UPDATE telegram_dm_topic_bindings SET profile_name = ? WHERE profile_name = 'default' "+
					"AND chat_id = ? AND thread_id = ?", target, row.ChatID, row.ThreadID)
				if err != nil {
					return err
				}
				counts.BindingsRelabelled += affectedRows(res)
			}
			if !modeHasProfile {
				continue
			}
			err = c.QueryRowContext(ctx, "SELECT 1 FROM telegram_dm_topic_mode WHERE profile_name = ? AND chat_id = ?",
				target, row.ChatID).Scan(&one)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if errors.Is(err, sql.ErrNoRows) {
				res, err := c.ExecContext(ctx, "UPDATE telegram_dm_topic_mode SET profile_name = ? WHERE profile_name = 'default' "+
					"AND chat_id = ?", target, row.ChatID)
				if err != nil {
					return err
				}
				counts.ModeRowsRelabelled += affectedRows(res)
			}
		}
		return nil
	})
	return counts, err
}

func (s *Store) KeyProfilesForChat(ctx context.Context, platform, chatID string) ([]string, error) {
	var keys []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		keys, err = queryStrings(ctx, c, "SELECT DISTINCT session_key FROM sessions WHERE source = ? AND chat_id = ? "+
			"AND session_key LIKE 'agent:%'", platform, chatID)
		return err
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, key := range keys {
		if p, ok := SessionKeyProfile(sql.Null[string]{V: key, Valid: true}); ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

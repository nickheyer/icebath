package state

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Gateway session key namespace builder, the agent prefix a profile's keys carry, wired by the host
var SessionKeyNamespace func(profile string) string

// Profile encoded in an agent gateway key, default for main, false for a keyless or foreign key
func SessionKeyProfile(sessionKey any) (string, bool) {
	key, ok := sessionKey.(string)
	if !ok {
		return "", false
	}
	parts := strings.Split(key, ":")
	if len(parts) < 3 || parts[0] != "agent" || parts[1] == "" {
		return "", false
	}
	return ProfileFromSessionKeyNamespace(parts[1]), true
}

// Stored profile_name as a comparable label, NULL or blank being unowned rather than a crossing
func storedProfile(value any) (string, bool) {
	name := ""
	if value != nil {
		name = pystr.Strip(pyStr(value))
	}
	return name, name != ""
}

// Column names of a table in declaration order
func tableColumns(ctx context.Context, conn SQLiteConn, table string) ([]string, error) {
	rows, err := queryObjects(ctx, conn, "PRAGMA table_info('"+table+"')")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, pyStr(rowValue(row, "name")))
	}
	return out, nil
}

// Columns a moved message never carries verbatim, the id being reassigned and display_order recomputed
var messageMoveSkip = map[string]bool{"id": true, "display_order": true}

// A session with gateway admission or worker history, never movable between stores
type SessionLedgeredError struct{ SessionID string }

func (e *SessionLedgeredError) Error() string {
	return e.SessionID + " has gateway admission/worker history; not moved"
}

func (e *SessionLedgeredError) PyTypeName() string { return "SessionLedgeredError" }

// Crossed profile identity findings of one store, a row possibly in more than one list
type CrossedProfileSessions struct {
	Mislabelled, Foreign, CrossedParents []*pyjson.Object
}

// Keyed session rows whose identity disagrees with itself or with the owner of this store
func (d *SessionDB) FindCrossedProfileSessions(ctx context.Context, owner string) (*CrossedProfileSessions, error) {
	rows, err := d.readAll(ctx, "SELECT s.id, s.session_key, s.profile_name, s.parent_session_id, s.message_count, "+
		"       p.session_key AS parent_session_key "+
		"FROM sessions s LEFT JOIN sessions p ON p.id = s.parent_session_id "+
		"WHERE s.session_key LIKE 'agent:%' ORDER BY s.started_at, s.id")
	if err != nil {
		return nil, err
	}
	ledgered, err := d.ledgeredSessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	found := &CrossedProfileSessions{Mislabelled: []*pyjson.Object{}, Foreign: []*pyjson.Object{}, CrossedParents: []*pyjson.Object{}}
	for _, row := range rows {
		keyProfile, ok := SessionKeyProfile(rowValue(row, "session_key"))
		if !ok {
			continue
		}
		id := rowValue(row, "id")
		if label, labelled := storedProfile(rowValue(row, "profile_name")); labelled && label != keyProfile {
			found.Mislabelled = append(found.Mislabelled, pyjson.ObjectOf("id", id, "session_key", rowValue(row, "session_key"),
				"profile_name", label, "key_profile", keyProfile))
		}
		if keyProfile != owner {
			count, _ := exactInt(rowValue(row, "message_count"))
			found.Foreign = append(found.Foreign, pyjson.ObjectOf("id", id, "session_key", rowValue(row, "session_key"),
				"key_profile", keyProfile, "message_count", count, "parent_session_id", rowValue(row, "parent_session_id"),
				"ledgered", ledgered[pyStr(id)]))
		}
		if parentProfile, ok := SessionKeyProfile(rowValue(row, "parent_session_key")); ok && parentProfile != keyProfile {
			found.CrossedParents = append(found.CrossedParents, pyjson.ObjectOf("id", id, "key_profile", keyProfile,
				"parent_session_id", rowValue(row, "parent_session_id"), "parent_profile", parentProfile))
		}
	}
	return found, nil
}

// Sessions the gateway's durable ledger references, none for a store whose schema predates the ledger
func (d *SessionDB) ledgeredSessionIDs(ctx context.Context) (map[string]bool, error) {
	ids := map[string]bool{}
	err := d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		ids = map[string]bool{}
		tables, err := queryObjects(ctx, conn, "SELECT name FROM sqlite_master WHERE type='table' "+
			"AND name IN ('session_admissions', 'worker_executions')")
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, row := range tables {
			present[pyStr(rowValue(row, "name"))] = true
		}
		for table, query := range map[string]string{
			"session_admissions": "SELECT DISTINCT target_session_id FROM session_admissions",
			"worker_executions":  "SELECT DISTINCT session_id FROM worker_executions",
		} {
			if !present[table] {
				continue
			}
			rows, err := queryObjects(ctx, conn, query)
			if err != nil {
				return err
			}
			for _, row := range rows {
				ids[pyStr(rowValue(row, row.Keys[0]))] = true
			}
		}
		return nil
	})
	return ids, err
}

// Every gateway routing row ordered by scope and key
func (d *SessionDB) ListGatewayRoutingRows(ctx context.Context) ([]*pyjson.Object, error) {
	return d.readAll(ctx, "SELECT scope, session_key, entry_json, updated_at FROM gateway_routing ORDER BY scope, session_key")
}

// Telegram topic bindings labelled default whose session key names a named profile
func (d *SessionDB) FindProfileLessTelegramTopicRows(ctx context.Context) ([]*pyjson.Object, error) {
	var rows []*pyjson.Object
	err := d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		rows = nil
		existing, err := queryObjects(ctx, conn, "SELECT name FROM sqlite_master WHERE type='table' AND name = 'telegram_dm_topic_bindings'")
		if err != nil || len(existing) == 0 {
			return err
		}
		columns, err := tableColumns(ctx, conn, "telegram_dm_topic_bindings")
		if err != nil || !containsStr(columns, "profile_name") {
			return err
		}
		rows, err = queryObjects(ctx, conn, "SELECT chat_id, thread_id, session_key FROM telegram_dm_topic_bindings "+
			"WHERE profile_name = 'default' ORDER BY chat_id, thread_id")
		return err
	})
	if err != nil {
		return nil, err
	}
	out := []*pyjson.Object{}
	for _, row := range rows {
		if keyProfile, ok := SessionKeyProfile(rowValue(row, "session_key")); ok && keyProfile != "default" {
			labelled := row.Copy()
			labelled.Set("key_profile", keyProfile)
			out = append(out, labelled)
		}
	}
	return out, nil
}

// Message rows of a session across every generation
func (d *SessionDB) CountMessagesAll(ctx context.Context, sessionID string) (int, error) {
	row, err := d.readOne(ctx, "SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID)
	if err != nil || row == nil {
		return 0, err
	}
	n, _ := exactInt(row.Vals[row.Keys[0]])
	return int(n), nil
}

// Sets profile_name to the profile of each row's own key, the target re-derived inside the transaction
func (d *SessionDB) RelabelSessionsToKeyProfile(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var changed int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		changed = 0
		for _, chunk := range idChunks(ids, sqlInChunk) {
			rows, err := queryObjects(ctx, conn, "SELECT id, session_key FROM sessions WHERE id IN ("+placeholders(len(chunk))+")", stringsToAny(chunk)...)
			if err != nil {
				return err
			}
			for _, row := range rows {
				target, ok := SessionKeyProfile(rowValue(row, "session_key"))
				if !ok {
					continue
				}
				n, err := execRowcount(ctx, conn, "UPDATE sessions SET profile_name = ? WHERE id = ? AND profile_name IS NOT ?",
					target, rowValue(row, "id"), target)
				if err != nil {
					return err
				}
				changed += n
			}
		}
		return nil
	}, nil)
	return changed, err
}

// Detaches children from a parent keyed under another profile while the crossing still holds
func (d *SessionDB) SeverCrossedParents(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var severed int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		severed = 0
		for _, chunk := range idChunks(ids, sqlInChunk) {
			rows, err := queryObjects(ctx, conn, "SELECT s.id, s.session_key, p.session_key AS parent_key FROM sessions s "+
				"JOIN sessions p ON p.id = s.parent_session_id "+
				"WHERE s.id IN ("+placeholders(len(chunk))+")", stringsToAny(chunk)...)
			if err != nil {
				return err
			}
			for _, row := range rows {
				mine, okMine := SessionKeyProfile(rowValue(row, "session_key"))
				theirs, okTheirs := SessionKeyProfile(rowValue(row, "parent_key"))
				if okMine && okTheirs && mine != theirs {
					n, err := execRowcount(ctx, conn, "UPDATE sessions SET parent_session_id = NULL WHERE id = ?", rowValue(row, "id"))
					if err != nil {
						return err
					}
					severed += n
				}
			}
		}
		return nil
	}, nil)
	return severed, err
}

// Everything a target store needs to hold a session as its own, every message generation included
type SessionMovePayload struct {
	Session      *pyjson.Object
	SystemPrompt any
	ToolPin      any
	Messages     []*pyjson.Object
	Usage        []*pyjson.Object
}

// Move payload of a session, nil when absent, refused for a row the gateway ledger references
func (d *SessionDB) ExportSessionForMove(ctx context.Context, sessionID string) (*SessionMovePayload, error) {
	var payload *SessionMovePayload
	err := d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		payload = nil
		sessions, err := queryObjects(ctx, conn, "SELECT * FROM sessions WHERE id = ?", sessionID)
		if err != nil || len(sessions) == 0 {
			return err
		}
		session := sessions[0]
		stored := func(hash any) (any, error) {
			if !pyTruthy(hash) {
				return nil, nil
			}
			row, err := connRow(ctx, conn, "SELECT prompt FROM system_prompts WHERE hash = ?", hash)
			if err != nil || row == nil {
				return nil, err
			}
			return row["prompt"], nil
		}
		prompt, err := stored(rowValue(session, "system_prompt_hash"))
		if err != nil {
			return err
		}
		toolPin, err := stored(rowValue(session, "tool_names"))
		if err != nil {
			return err
		}
		messages, err := queryObjects(ctx, conn, "SELECT * FROM messages WHERE session_id = ? ORDER BY id", sessionID)
		if err != nil {
			return err
		}
		usage, err := queryObjects(ctx, conn, "SELECT * FROM session_model_usage WHERE session_id = ?", sessionID)
		if err != nil {
			return err
		}
		payload = &SessionMovePayload{Session: session, SystemPrompt: prompt, ToolPin: toolPin, Messages: messages, Usage: usage}
		return nil
	})
	if err != nil || payload == nil {
		return nil, err
	}
	ledgered, err := d.ledgeredSessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	if ledgered[sessionID] {
		return nil, &SessionLedgeredError{SessionID: sessionID}
	}
	return payload, nil
}

// Inserts a moved session into this store as profile's own, present when the id already exists else imported
func (d *SessionDB) ImportMovedSession(ctx context.Context, payload *SessionMovePayload, profileName string) (string, error) {
	session := payload.Session.Copy()
	sessionID := pyStr(rowValue(session, "id"))
	outcome := ""
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		exists, err := fetchOne(ctx, conn, "SELECT 1 FROM sessions WHERE id = ?", sessionID)
		if err != nil {
			return err
		}
		if exists != nil {
			outcome = "present"
			return nil
		}
		session.Set("profile_name", profileName)
		if parentID := rowValue(session, "parent_session_id"); pyTruthy(parentID) {
			parent, err := fetchOne(ctx, conn, "SELECT 1 FROM sessions WHERE id = ?", parentID)
			if err != nil {
				return err
			}
			if parent == nil {
				session.Set("parent_session_id", nil)
			}
		}
		if title := rowValue(session, "title"); title != nil {
			taken, err := fetchOne(ctx, conn, "SELECT 1 FROM sessions WHERE title = ?", title)
			if err != nil {
				return err
			}
			if taken != nil {
				suffix := " (" + pystr.Slice(sessionID, -12, pystr.Len(sessionID)) + ")"
				session.Set("title", pystr.Slice(pyStr(title), 0, MaxTitleLength-pystr.Len(suffix))+suffix)
			}
		}
		hash, err := storeSystemPrompt(ctx, conn, payload.SystemPrompt)
		if err != nil {
			return err
		}
		session.Set("system_prompt_hash", hash)
		toolNames, _ := rowValue(session, "tool_names").(string)
		if payload.ToolPin != nil || pystr.Len(toolNames) == 64 {
			pin, err := storeSystemPrompt(ctx, conn, payload.ToolPin)
			if err != nil {
				return err
			}
			session.Set("tool_names", pin)
		}
		if err := insertRow(ctx, conn, "sessions", session, nil); err != nil {
			return err
		}
		for _, message := range payload.Messages {
			moved := message.Copy()
			moved.Set("session_id", sessionID)
			if err := insertRow(ctx, conn, "messages", moved, messageMoveSkip); err != nil {
				return err
			}
		}
		for _, usage := range payload.Usage {
			moved := usage.Copy()
			moved.Set("session_id", sessionID)
			if err := insertRow(ctx, conn, "session_model_usage", moved, nil); err != nil {
				return err
			}
		}
		outcome = "imported"
		return nil
	}, nil)
	return outcome, err
}

// Inserts the values the table declares, skipping the named columns and any the target schema lacks
func insertRow(ctx context.Context, conn SQLiteConn, table string, values *pyjson.Object, skip map[string]bool) error {
	declared, err := tableColumns(ctx, conn, table)
	if err != nil {
		return err
	}
	columns := []string{}
	args := []any{}
	for _, column := range declared {
		if v, ok := values.Get(column); ok && !skip[column] {
			columns = append(columns, column)
			args = append(args, v)
		}
	}
	_, err = conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(columns, ", "), placeholders(len(columns))), args...)
	return err
}

// Removes a session this store no longer owns after the target confirmed it, children left behind detached
func (d *SessionDB) DeleteMovedSession(ctx context.Context, sessionID string) (bool, error) {
	deleted := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		deleted = false
		exists, err := fetchOne(ctx, conn, "SELECT 1 FROM sessions WHERE id = ?", sessionID)
		if err != nil || exists == nil {
			return err
		}
		for _, query := range []string{
			"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?",
			"DELETE FROM messages WHERE session_id = ?",
			"DELETE FROM session_model_usage WHERE session_id = ?",
			"DELETE FROM sessions WHERE id = ?",
		} {
			if _, err := conn.ExecContext(ctx, query, sessionID); err != nil {
				return err
			}
		}
		if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
			return err
		}
		deleted = true
		return nil
	}, nil)
	if err != nil {
		return false, err
	}
	if deleted {
		d.EmitSessionDeleted(ctx, []string{sessionID}, "profile_repair", "", "")
	}
	return deleted, nil
}

// Rewrites legacy agent main keys and labels onto a profile for rows a standalone gateway wrote
func (d *SessionDB) RekeyLegacyMainSessions(ctx context.Context, ids []string, profile string) (int64, error) {
	requireHook(SessionKeyNamespace != nil, "SessionKeyNamespace", "gateway session _session_key_namespace")
	if len(ids) == 0 {
		return 0, nil
	}
	newNS := SessionKeyNamespace(profile) + ":"
	var changed int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		changed = 0
		for _, chunk := range idChunks(ids, sqlInChunk) {
			n, err := execRowcount(ctx, conn, "UPDATE sessions SET session_key = ? || substr(session_key, 12), profile_name = ? "+
				"WHERE id IN ("+placeholders(len(chunk))+") AND substr(session_key, 1, 11) = 'agent:main:'",
				append([]any{newNS, profile}, stringsToAny(chunk)...)...)
			if err != nil {
				return err
			}
			changed += n
		}
		return nil
	}, nil)
	return changed, err
}

// Deletes the routing rows named by scope and session key, returning how many went
func (d *SessionDB) DeleteGatewayRoutingRows(ctx context.Context, rows [][2]string) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var removed int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed = 0
		for _, row := range rows {
			n, err := execRowcount(ctx, conn, "DELETE FROM gateway_routing WHERE scope = ? AND session_key = ?", row[0], row[1])
			if err != nil {
				return err
			}
			removed += n
		}
		return nil
	}, nil)
	return removed, err
}

// One routing index row another store held for this one
type GatewayRoutingRow struct {
	Scope, SessionKey, EntryJSON string
	UpdatedAt                    float64
}

// Adopts routing rows another store held for this one, an existing key keeping the row the gateway loads
func (d *SessionDB) InsertGatewayRoutingRowsIfAbsent(ctx context.Context, rows []GatewayRoutingRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var inserted int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		inserted = 0
		for _, row := range rows {
			n, err := execRowcount(ctx, conn, "INSERT OR IGNORE INTO gateway_routing (scope, session_key, entry_json, updated_at) "+
				"VALUES (?, ?, ?, ?)", row.Scope, row.SessionKey, row.EntryJSON, row.UpdatedAt)
			if err != nil {
				return err
			}
			inserted += n
		}
		return nil
	}, nil)
	return inserted, err
}

// Stamps the key's profile onto default labelled topic bindings and lone mode rows, duplicates removed
func (d *SessionDB) RelabelTelegramTopicRows(ctx context.Context, rows []*pyjson.Object) (*pyjson.Object, error) {
	counts := pyjson.ObjectOf("bindings_relabelled", int64(0), "bindings_duplicates_removed", int64(0), "mode_rows_relabelled", int64(0))
	if len(rows) == 0 {
		return counts, nil
	}
	bump := func(key string, n int64) {
		current, _ := counts.Get(key)
		value, _ := exactInt(current)
		counts.Set(key, value+n)
	}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		counts = pyjson.ObjectOf("bindings_relabelled", int64(0), "bindings_duplicates_removed", int64(0), "mode_rows_relabelled", int64(0))
		modeColumns, err := tableColumns(ctx, conn, "telegram_dm_topic_mode")
		if err != nil {
			return err
		}
		modeHasProfile := containsStr(modeColumns, "profile_name")
		for _, row := range rows {
			target, ok := SessionKeyProfile(rowValue(row, "session_key"))
			if !ok || target == "default" {
				continue
			}
			chatID, threadID := rowValue(row, "chat_id"), rowValue(row, "thread_id")
			collides, err := fetchOne(ctx, conn, "SELECT 1 FROM telegram_dm_topic_bindings WHERE profile_name = ? AND chat_id = ? "+
				"AND thread_id = ?", target, chatID, threadID)
			if err != nil {
				return err
			}
			if collides != nil {
				n, err := execRowcount(ctx, conn, "DELETE FROM telegram_dm_topic_bindings WHERE profile_name = 'default' "+
					"AND chat_id = ? AND thread_id = ?", chatID, threadID)
				if err != nil {
					return err
				}
				bump("bindings_duplicates_removed", n)
			} else {
				n, err := execRowcount(ctx, conn, "UPDATE telegram_dm_topic_bindings SET profile_name = ? WHERE profile_name = 'default' "+
					"AND chat_id = ? AND thread_id = ?", target, chatID, threadID)
				if err != nil {
					return err
				}
				bump("bindings_relabelled", n)
			}
			if !modeHasProfile {
				continue
			}
			mode, err := fetchOne(ctx, conn, "SELECT 1 FROM telegram_dm_topic_mode WHERE profile_name = ? AND chat_id = ?", target, chatID)
			if err != nil {
				return err
			}
			if mode == nil {
				n, err := execRowcount(ctx, conn, "UPDATE telegram_dm_topic_mode SET profile_name = ? WHERE profile_name = 'default' "+
					"AND chat_id = ?", target, chatID)
				if err != nil {
					return err
				}
				bump("mode_rows_relabelled", n)
			}
		}
		return nil
	}, nil)
	return counts, err
}

// Profiles whose keyed sessions hold the platform chat in this store
func (d *SessionDB) KeyProfilesForChat(ctx context.Context, platform, chatID string) (map[string]bool, error) {
	rows, err := d.readAll(ctx, "SELECT DISTINCT session_key FROM sessions WHERE source = ? AND chat_id = ? "+
		"AND session_key LIKE 'agent:%'", platform, chatID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, row := range rows {
		if profile, ok := SessionKeyProfile(rowValue(row, "session_key")); ok {
			out[profile] = true
		}
	}
	return out, nil
}

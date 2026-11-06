package state

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Value of a row column, nil when the column is absent
func rowValue(row *pyjson.Object, key string) any {
	if row == nil {
		return nil
	}
	v, _ := row.Get(key)
	return v
}

// Python str of a row column after the or empty fallback
func rowStrOr(row *pyjson.Object, key string) string {
	v := rowValue(row, key)
	if !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

// Most recent live session id of a platform chat, empty when none or when several participants make it ambiguous
func (d *SessionDB) FindSessionByOrigin(ctx context.Context, platform, chatID string, threadID, userID *string) (string, error) {
	if platform == "" || chatID == "" {
		return "", nil
	}
	query := `
            SELECT id, user_id, started_at FROM sessions
            WHERE LOWER(source) = LOWER(?)
              AND session_key IS NOT NULL
              AND chat_id = ?
              AND ended_at IS NULL
        `
	params := []any{platform, chatID}
	if threadID != nil {
		query += " AND COALESCE(thread_id, '') = ?"
		params = append(params, *threadID)
	}
	rows, err := d.readAll(ctx, query+" ORDER BY started_at DESC", params...)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	if userID != nil && *userID != "" {
		for _, r := range rows {
			if rowStrOr(r, "user_id") == *userID {
				return pyStr(rowValue(r, "id")), nil
			}
		}
		if len(rows) > 1 {
			return "", nil
		}
	} else {
		users := map[string]bool{}
		for _, r := range rows {
			if u := pyStrip(rowStrOr(r, "user_id")); u != "" {
				users[u] = true
			}
		}
		if len(users) > 1 {
			return "", nil
		}
	}
	return pyStr(rowValue(rows[0], "id")), nil
}

// Hermes find_session_by_origin as a package function over a store
func FindSessionByOrigin(ctx context.Context, db *SessionDB, platform, chatID string, threadID, userID *string) (string, error) {
	return db.FindSessionByOrigin(ctx, platform, chatID, threadID, userID)
}

const sameKeyNamespaceSQL = "p.session_key IS NULL OR sessions.session_key IS NULL" +
	" OR substr(p.session_key, 1, instr(substr(p.session_key, 7), ':') + 6)" +
	"  = substr(sessions.session_key, 1, instr(substr(sessions.session_key, 7), ':') + 6)"

// Upsert tail of the session row insert keeping every routing column an earlier writer set
var upsertKeepExistingSQL = func() string {
	cols := []string{"session_key", "chat_id", "chat_type", "thread_id", "parent_session_id", "cwd", "profile_name",
		"transport_profile", "git_repo_root", "origin_json", "display_name"}
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = "                       " + col + " = COALESCE(sessions." + col + ", excluded." + col + ")"
	}
	return strings.Join(parts, ",\n") + ",\n" + "                       created_source = CASE WHEN " +
		unsetCreatedSQL("sessions.created_source") +
		" THEN COALESCE(NULLIF(excluded.created_source, 'local'), sessions.created_source)" +
		" ELSE sessions.created_source END"
}()

// Column assignment that fills a NULL child column from its parent row
func inheritColSQL(col, extra string) string {
	pad := strings.Repeat(" ", 30+len(col))
	return col + " = COALESCE(sessions." + col + ",\n" + pad + "(SELECT p." + col + " FROM sessions p\n" +
		pad + "  WHERE p.id = sessions.parent_session_id" + extra + "))"
}

var inheritSep = ",\n" + strings.Repeat(" ", 27)

var inheritParentMetaSQL = "UPDATE sessions\n                       SET " + strings.Join([]string{
	inheritColSQL("cwd", ""), inheritColSQL("git_repo_root", ""), inheritColSQL("git_branch", ""),
	inheritColSQL("profile_name", "\n"+strings.Repeat(" ", 46)+"AND ("+sameKeyNamespaceSQL+")"),
}, inheritSep) + "\n                     WHERE id = ? AND parent_session_id IS NOT NULL"

var forkEdgeExclusionSQL = func() string {
	var b strings.Builder
	for _, marker := range []string{"_delegate_from", "_branched_from"} {
		b.WriteString("\n                       AND COALESCE(" + sqlJSONExtract("model_config", "$."+marker) + ", '')" +
			"\n                           != parent_session_id")
	}
	return b.String()
}()

var inheritParentRoutingSQL = func() string {
	cols := []string{"user_id", "session_key", "chat_id", "chat_type", "thread_id", "display_name", "origin_json", "transport_profile"}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = inheritColSQL(c, "")
	}
	return "UPDATE sessions\n                       SET " + strings.Join(parts, inheritSep) +
		"\n                     WHERE id = ? AND parent_session_id IS NOT NULL\n" +
		"                       AND EXISTS (\n" +
		"                           SELECT 1 FROM sessions p\n" +
		"                           WHERE p.id = sessions.parent_session_id\n" +
		"                             AND p.end_reason = 'compression'\n" +
		"                       )" + forkEdgeExclusionSQL
}()

// Profile owning this store from its path alone, empty outside the profile tree
func (d *SessionDB) ownProfileName() string {
	root := realpathLax(home.Root())
	parent := filepath.Dir(realpathLax(d.Path()))
	if parent == root {
		return "default"
	}
	if filepath.Dir(parent) == filepath.Join(root, "profiles") && home.ProfileIDRe.MatchString(filepath.Base(parent)) {
		return filepath.Base(parent)
	}
	return ""
}

// Fills a child's cwd, git and profile from its parent and routing columns only for compression forks
func inheritParentSessionMetadata(ctx context.Context, conn SQLiteConn, sessionID string) error {
	if _, err := conn.ExecContext(ctx, inheritParentMetaSQL, sessionID); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, inheritParentRoutingSQL, sessionID)
	return err
}

// Upserts a session row without overwriting what an earlier writer set
func (d *SessionDB) insertSessionRow(ctx context.Context, n NewSession) error {
	patience := sessionTranscriptWritePatienceS
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		return d.insertSessionRowInTransaction(ctx, conn, n)
	}, &patience)
}

// Creates or upserts a session record
func (d *SessionDB) CreateSession(n NewSession) error {
	return d.insertSessionRow(context.Background(), n)
}

// Ensures a session row exists, an empty source meaning unknown
func (d *SessionDB) EnsureSession(n NewSession) error {
	if n.Source == "" {
		n.Source = "unknown"
	}
	return d.insertSessionRow(context.Background(), n)
}

// Session row by id with resolved prompt columns after the queued token deltas land, nil when absent
func (d *SessionDB) GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	d.FlushForRead(ctx)
	row, err := d.readOne(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		"COALESCE(tp.prompt, s.tool_names) AS _tool_names_resolved "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
		"LEFT JOIN system_prompts tp ON tp.hash = s.tool_names WHERE s.id = ?", sessionID)
	if err != nil || row == nil {
		return nil, err
	}
	return sessionRowDict(row), nil
}

// Tolerant model_config decode, JSON text or an object copied, anything else empty
func parseModelConfigValue(raw any) *pyjson.Object {
	if text, ok := raw.(string); ok && pyStrip(text) != "" {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return pyjson.NewObject()
		}
		raw = parsed
	}
	out := pyjson.NewObject()
	if obj, ok := raw.(*pyjson.Object); ok {
		for _, k := range obj.Keys {
			out.Set(k, obj.Vals[k])
		}
	}
	return out
}

// Whether a session is a copied user facing branch
func (d *SessionDB) isExplicitBranchSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	row, err := d.readOne(ctx, "SELECT model_config FROM sessions WHERE id = ?", sessionID)
	if err != nil || row == nil {
		return false, err
	}
	branched, _ := parseModelConfigValue(rowValue(row, "model_config")).Get("_branched_from")
	return pyTruthy(branched), nil
}

// Raw parent walk from the root to a session, capped at 100 hops
func (d *SessionDB) sessionLineageRootToTip(ctx context.Context, sessionID string) ([]string, error) {
	if sessionID == "" {
		return []string{sessionID}, nil
	}
	var chain []string
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		chain = nil
		current := sessionID
		for current != "" && !slices.Contains(chain, current) && len(chain) < 100 {
			chain = append(chain, current)
			rows, err := queryObjects(ctx, conn, "SELECT parent_session_id FROM sessions WHERE id = ?", current)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			parent, _ := rowValue(rows[0], "parent_session_id").(string)
			current = parent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Reverse(chain)
	if len(chain) == 0 {
		return []string{sessionID}, nil
	}
	return chain, nil
}

// Stores the assembled system prompt snapshot through the content addressed prompt table, nil clearing it
func (d *SessionDB) UpdateSystemPrompt(ctx context.Context, sessionID string, systemPrompt *string) error {
	var prompt any
	if systemPrompt != nil {
		prompt = *systemPrompt
	}
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		hash, err := storeSystemPrompt(ctx, conn, prompt)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET system_prompt_hash = ?, system_prompt = NULL WHERE id = ?", hash, sessionID); err != nil {
			return err
		}
		return deleteUnreferencedSystemPrompts(ctx, conn)
	}, nil)
}

// Persists the JSON tools pin in the content addressed prompt table, the column holding its hash, nil clearing it
func (d *SessionDB) UpdateSessionToolNames(sessionID string, pin any) error {
	var payload any
	if pin != nil {
		text, err := pyjson.Dumps(pin)
		if err != nil {
			return err
		}
		payload = text
	}
	ctx := context.Background()
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		hash, err := storeSystemPrompt(ctx, conn, payload)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET tool_names = ? WHERE id = ?", hash, sessionID); err != nil {
			return err
		}
		return deleteUnreferencedSystemPrompts(ctx, conn)
	}, nil)
}

// Stamps durable mid turn activity without ever moving the activity clock backwards
func (d *SessionDB) TouchSessionActivity(sessionID string, at *float64, description string, provenance ActivityProvenance) error {
	if sessionID == "" {
		return nil
	}
	when := now()
	if at != nil {
		when = *at
	}
	patience := sessionActivityWritePatienceS
	return d.writeSQL(context.Background(),
		"UPDATE sessions SET last_activity_at = ?, "+
			"last_activity_description = ?, last_activity_provenance = ? "+
			"WHERE id = ? AND (last_activity_at IS NULL OR last_activity_at < ?)",
		[]any{when, BoundActivityDescription(description), string(NormalizeActivityProvenance(string(provenance))), sessionID, when},
		false, &patience)
}

// Clears the activity description and provenance after a turn, skipping the write when already clear
func (d *SessionDB) ClearSessionActivityLabels(sessionID string) error {
	if sessionID == "" {
		return nil
	}
	row, err := d.readOne(context.Background(),
		"SELECT last_activity_description, last_activity_provenance FROM sessions WHERE id = ?", sessionID)
	if err != nil {
		row = nil
	}
	if row != nil {
		desc, prov := rowValue(row, "last_activity_description"), rowValue(row, "last_activity_provenance")
		if !pyTruthy(desc) && (!pyTruthy(prov) || prov == string(ActivityProvenanceUnknown)) {
			return nil
		}
	}
	patience := sessionActivityWritePatienceS
	return d.writeSQL(context.Background(),
		"UPDATE sessions SET last_activity_description = ?, last_activity_provenance = ? WHERE id = ?",
		[]any{"", string(ActivityProvenanceUnknown), sessionID}, false, &patience)
}

// Merges the patch into the stored model_config, nil values deleting keys, returning the JSON text or nil when empty
func mergeModelConfigJSON(ctx context.Context, conn SQLiteConn, sessionID string, patch *pyjson.Object, raiseOnMissing bool) (any, bool, error) {
	row, err := connRow(ctx, conn, "SELECT model_config FROM sessions WHERE id = ?", sessionID)
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		if raiseOnMissing {
			return nil, true, pyValueError("Session not found: " + sessionID)
		}
		return nil, true, nil
	}
	config := parseModelConfigValue(row["model_config"])
	for _, key := range patch.Keys {
		if value := patch.Vals[key]; value == nil {
			config.Delete(key)
		} else {
			config.Set(key, value)
		}
	}
	if config.Len() == 0 {
		return nil, false, nil
	}
	text, err := pyjson.Dumps(config)
	if err != nil {
		return nil, false, err
	}
	return text, false, nil
}

// Marks a session ended keeping the first end reason, advancing the generation only when this call ended it
func (d *SessionDB) EndSession(sessionID, endReason string) error {
	ctx := context.Background()
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		_, err := d.endAndBump(ctx, conn, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
			[]any{now(), endReason, sessionID}, sessionID, endReason)
		return err
	}, nil)
}

// Runs an end stamp update and bumps the conversation generation only for a boundary it wrote
func (d *SessionDB) endAndBump(ctx context.Context, conn SQLiteConn, query string, params []any, sessionID, reason string) (int64, error) {
	res, err := conn.ExecContext(ctx, query, params...)
	if err != nil {
		return 0, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed > 0 {
		if err := bumpConversationGeneration(ctx, conn, sessionID, reason); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

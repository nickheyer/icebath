package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Row of the sessions table
type Session struct {
	ID               string
	Source           string
	UserID           string
	Model            string
	ModelConfig      map[string]any
	SystemPrompt     string
	ParentSessionID  string
	StartedAt        float64
	EndedAt          float64
	EndReason        string
	MessageCount     int
	ToolCallCount    int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	Cwd              string
	GitBranch        string
	GitRepoRoot      string
	BillingProvider  string
	BillingBaseURL   string
	BillingMode      string
	EstimatedCostUSD float64
	ActualCostUSD    float64
	CostStatus       string
	CostSource       string
	PricingVersion   string
	Title            string
	TitleSource      string
	LastActivityAt   float64
	LastActivity     string
	APICallCount     int
	ProfileName      string
	RewindCount      int
	Archived         bool
	AutoArchived     bool
	Pinned           bool
	Hidden           bool
	LastReadAt       float64
	ToolNames        []string
	TranscriptRev    int64
	// Head of the first user message, filled by listings
	Preview string
	// Freshest activity known, filled by listings
	LastActive float64
}

// Keyword arguments of the session row upsert, empty strings and a nil map are the Python None keywords
type NewSession struct {
	ID               string
	Source           string
	UserID           string
	Model            string
	ModelConfig      map[string]any
	SystemPrompt     string
	ParentSessionID  string
	Cwd              string
	GitRepoRoot      string
	ProfileName      string
	SessionKey       string
	ChatID           string
	ChatType         string
	ThreadID         string
	TransportProfile string
	OriginJSON       string
	DisplayName      string
}

// Reason a compression continuation ends its parent with
const EndReasonCompression = "compression"

const sessionColumns = `s.id, s.source, s.user_id, s.model, s.model_config, COALESCE(sp.prompt, ''), s.parent_session_id,
	s.started_at, s.ended_at, s.end_reason, s.message_count, s.tool_call_count, s.input_tokens, s.output_tokens,
	s.cache_read_tokens, s.cache_write_tokens, s.reasoning_tokens, s.cwd, s.git_branch, s.git_repo_root,
	s.billing_provider, s.billing_base_url, s.billing_mode, s.estimated_cost_usd, s.actual_cost_usd, s.cost_status,
	s.cost_source, s.pricing_version, s.title, s.title_source, s.last_activity_at, s.last_activity_description,
	s.api_call_count, s.profile_name, s.rewind_count, s.archived, s.auto_archived, s.pinned, s.hidden, s.last_read_at,
	s.tool_names, s.transcript_revision`

const sessionFrom = ` FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash`

// Row scanner shared by every session query
type rowScanner interface {
	Scan(dest ...any) error
}

// Session decoded from a row selected with sessionColumns
func scanSession(r rowScanner) (*Session, error) {
	var s Session
	var userID, model, modelConfig, parent, endReason, cwd, branch, root, bprov, burl, bmode, cstatus, csource, pver, title, tsource, activity, profile, toolNames sql.NullString
	var endedAt, estCost, actCost, lastActivity, lastRead sql.NullFloat64
	var archived, autoArchived, pinned, hidden int
	err := r.Scan(&s.ID, &s.Source, &userID, &model, &modelConfig, &s.SystemPrompt, &parent, &s.StartedAt, &endedAt, &endReason,
		&s.MessageCount, &s.ToolCallCount, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens,
		&s.ReasoningTokens, &cwd, &branch, &root, &bprov, &burl, &bmode, &estCost, &actCost, &cstatus, &csource, &pver,
		&title, &tsource, &lastActivity, &activity, &s.APICallCount, &profile, &s.RewindCount, &archived, &autoArchived,
		&pinned, &hidden, &lastRead, &toolNames, &s.TranscriptRev)
	if err != nil {
		return nil, err
	}
	s.UserID, s.Model, s.ParentSessionID, s.EndReason = userID.String, model.String, parent.String, endReason.String
	s.Cwd, s.GitBranch, s.GitRepoRoot = cwd.String, branch.String, root.String
	s.BillingProvider, s.BillingBaseURL, s.BillingMode = bprov.String, burl.String, bmode.String
	s.CostStatus, s.CostSource, s.PricingVersion = cstatus.String, csource.String, pver.String
	s.Title, s.TitleSource, s.LastActivity, s.ProfileName = title.String, tsource.String, activity.String, profile.String
	s.EndedAt, s.EstimatedCostUSD, s.ActualCostUSD = endedAt.Float64, estCost.Float64, actCost.Float64
	s.LastActivityAt, s.LastReadAt = lastActivity.Float64, lastRead.Float64
	s.Archived, s.AutoArchived, s.Pinned, s.Hidden = archived != 0, autoArchived != 0, pinned != 0, hidden != 0
	if modelConfig.Valid && modelConfig.String != "" {
		_ = json.Unmarshal([]byte(modelConfig.String), &s.ModelConfig)
	}
	if toolNames.Valid && toolNames.String != "" {
		_ = json.Unmarshal([]byte(toolNames.String), &s.ToolNames)
	}
	return &s, nil
}

// Stores a prompt by hash and returns the hash, empty for an empty prompt
func storePrompt(tx *Tx, prompt string) (string, error) {
	if prompt == "" {
		return "", nil
	}
	h := promptHash(prompt)
	_, err := tx.Exec("INSERT OR IGNORE INTO system_prompts (hash, prompt) VALUES (?, ?)", h, prompt)
	return h, err
}

// Drops prompts no session references
func deleteUnreferencedPrompts(tx *Tx) error {
	_, err := tx.Exec(deleteUnreferencedSystemPromptsSQL)
	return err
}

// Nullable text bound as NULL when empty
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// JSON text of a map, NULL when empty
func jsonOrNull(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}

// Full session id matching an id or a unique prefix, empty when none or ambiguous
func (d *DB) ResolveSessionID(idOrPrefix string) (string, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return "", nil
	}
	var ids []string
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT id FROM sessions WHERE id = ? OR id LIKE ? ESCAPE '\\' ORDER BY (id = ?) DESC, started_at DESC LIMIT 2",
			idOrPrefix, EscapeLike(idOrPrefix)+"%", idOrPrefix)
		if err != nil {
			return err
		}
		defer rows.Close()
		ids = nil
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	if ids[0] == idOrPrefix || len(ids) == 1 {
		return ids[0], nil
	}
	return "", nil
}

// Marks a session ended, the first reason winning
func (d *DB) EndSession(id, reason string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL", now(), reason, id)
		return err
	})
}

// Clears the end of a session so it can be resumed, dropping an automatic archive
func (d *DB) ReopenSession(id string) error {
	return d.write(func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", id); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE sessions SET archived = 0, auto_archived = 0 WHERE id = ? AND auto_archived = 1", id)
		return err
	})
}

// Stores the system prompt a session sends
func (d *DB) UpdateSystemPrompt(id, prompt string) error {
	return d.write(func(tx *Tx) error {
		hash, err := storePrompt(tx, prompt)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE sessions SET system_prompt_hash = ? WHERE id = ?", nullStr(hash), id); err != nil {
			return err
		}
		return deleteUnreferencedPrompts(tx)
	})
}

// Pins the tool names a session sends
func (d *DB) UpdateSessionToolNames(id string, names []string) error {
	var encoded any
	if names != nil {
		b, err := json.Marshal(names)
		if err != nil {
			return err
		}
		encoded = string(b)
	}
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET tool_names = ? WHERE id = ?", encoded, id)
		return err
	})
}

// Records the model and billing route a session runs on
func (d *DB) UpdateSessionModel(id, model, provider, baseURL string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET model = ?, billing_provider = ?, billing_base_url = ? WHERE id = ?",
			nullStr(model), nullStr(provider), nullStr(baseURL), id)
		return err
	})
}

// Merges keys into the model config of a session, a nil value removing the key
func (d *DB) PatchModelConfig(id string, patch map[string]any) error {
	return d.write(func(tx *Tx) error {
		return patchModelConfig(tx, id, patch)
	})
}

// Merges keys into the model config of a session inside a transaction
func patchModelConfig(tx *Tx, id string, patch map[string]any) error {
	var raw sql.NullString
	if err := tx.QueryRow("SELECT model_config FROM sessions WHERE id = ?", id).Scan(&raw); err != nil {
		return err
	}
	cfg := map[string]any{}
	if raw.Valid && raw.String != "" {
		_ = json.Unmarshal([]byte(raw.String), &cfg)
	}
	for k, v := range patch {
		if v == nil {
			delete(cfg, k)
		} else {
			cfg[k] = v
		}
	}
	_, err := tx.Exec("UPDATE sessions SET model_config = ? WHERE id = ?", jsonOrNull(cfg), id)
	return err
}

// Records the working directory and git facts of a session
func (d *DB) UpdateSessionCwd(id, cwd, repoRoot, branch string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET cwd = ?, git_repo_root = ?, git_branch = ? WHERE id = ?",
			nullStr(cwd), nullStr(repoRoot), nullStr(branch), id)
		return err
	})
}

// Stamps the latest activity of a session with a short description
func (d *DB) TouchActivity(id, description string) error {
	return d.retryWrite(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET last_activity_at = ?, last_activity_description = ? WHERE id = ?", now(), nullStr(description), id)
		return err
	}, sessionActivityWritePatienceS)
}

// Sets the archived flag of a session
func (d *DB) SetArchived(id string, archived bool) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET archived = ?, auto_archived = 0 WHERE id = ?", boolInt(archived), id)
		return err
	})
}

// Sets the pinned flag of a session
func (d *DB) SetPinned(id string, pinned bool) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET pinned = ? WHERE id = ?", boolInt(pinned), id)
		return err
	})
}

// Sets the hidden flag of a session
func (d *DB) SetHidden(id string, hidden bool) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET hidden = ? WHERE id = ?", boolInt(hidden), id)
		return err
	})
}

// Stamps when the person last read a session
func (d *DB) MarkRead(id string) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE sessions SET last_read_at = ? WHERE id = ?", now(), id)
		return err
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Filters of a session listing
type ListFilter struct {
	Limit           int
	Offset          int
	Source          string
	ExcludeSources  []string
	Cwd             string
	CwdPrefix       string
	Search          string
	IncludeArchived bool
	IncludeHidden   bool
	IncludeChildren bool
	IncludeEmpty    bool
	OnlyPinned      bool
}

const previewSQL = `COALESCE((SELECT substr(replace(replace(m.content, char(10), ' '), char(13), ' '), 1, 63) FROM messages m
	WHERE m.session_id = tip.id AND m.role = 'user' AND m.content IS NOT NULL AND (m.active = 1 OR m.compacted = 1)
	AND COALESCE(m.display_kind, '') <> 'hidden' AND substr(m.content, 1, 1) <> char(0)
	ORDER BY m.timestamp, m.id LIMIT 1), '')`

const lastActiveSQL = `COALESCE(tip.last_activity_at, (SELECT MAX(m2.timestamp) FROM messages m2 WHERE m2.session_id = tip.id), tip.started_at)`

// Sessions ordered by freshest activity, compression continuations collapsed onto their tip
func (d *DB) ListSessions(f ListFilter) ([]*Session, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	where := []string{"1=1"}
	var params []any
	if !f.IncludeArchived {
		where = append(where, "tip.archived = 0")
	}
	if !f.IncludeHidden {
		where = append(where, "tip.hidden = 0")
	}
	if !f.IncludeChildren {
		where = append(where, "COALESCE(json_extract(tip.model_config, '$._delegate_from'), '') = ''")
	}
	if !f.IncludeEmpty {
		where = append(where, "tip.message_count > 0")
	}
	if f.OnlyPinned {
		where = append(where, "tip.pinned = 1")
	}
	where = append(where, "NOT EXISTS (SELECT 1 FROM sessions child WHERE child.parent_session_id = tip.id AND tip.end_reason = 'compression' AND COALESCE(json_extract(child.model_config, '$._branched_from'), '') = '' AND COALESCE(json_extract(child.model_config, '$._delegate_from'), '') = '')")
	if f.Source != "" {
		where = append(where, "tip.source = ?")
		params = append(params, f.Source)
	}
	if len(f.ExcludeSources) > 0 {
		where = append(where, "tip.source NOT IN ("+placeholders(len(f.ExcludeSources))+")")
		params = append(params, args(f.ExcludeSources)...)
	}
	if f.Cwd != "" {
		where = append(where, "tip.cwd = ?")
		params = append(params, f.Cwd)
	}
	if f.CwdPrefix != "" {
		where = append(where, "(tip.cwd = ? OR tip.cwd LIKE ? ESCAPE '\\')")
		params = append(params, f.CwdPrefix, EscapeLike(strings.TrimRight(f.CwdPrefix, "/"))+"/%")
	}
	if f.Search != "" {
		needle := "%" + EscapeLike(f.Search) + "%"
		where = append(where, "(tip.id LIKE ? ESCAPE '\\' OR tip.title LIKE ? ESCAPE '\\' OR "+previewSQL+" LIKE ? ESCAPE '\\')")
		params = append(params, needle, needle, needle)
	}
	query := `SELECT ` + strings.ReplaceAll(sessionColumns, "s.", "tip.") + `, ` + previewSQL + `, ` + lastActiveSQL + `
		FROM sessions tip LEFT JOIN system_prompts sp ON sp.hash = tip.system_prompt_hash
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ` + lastActiveSQL + ` DESC, tip.started_at DESC, tip.id DESC LIMIT ? OFFSET ?`
	params = append(params, f.Limit, f.Offset)
	var out []*Session
	err := d.read(func(q querier) error {
		rows, err := q.Query(query, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			s, err := scanSessionWithExtras(rows)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// Session plus the preview and activity columns of a listing row
func scanSessionWithExtras(rows *sql.Rows) (*Session, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	dest := make([]any, len(cols))
	holders := make([]any, len(cols))
	for i := range dest {
		dest[i] = &holders[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	base := len(cols) - 2
	s, err := scanSession(valuesScanner(holders[:base]))
	if err != nil {
		return nil, err
	}
	if p, ok := holders[base].(string); ok {
		s.Preview = strings.TrimSpace(p)
	}
	switch v := holders[base+1].(type) {
	case float64:
		s.LastActive = v
	case int64:
		s.LastActive = float64(v)
	}
	return s, nil
}

// Scanner over already fetched column values
type valuesScanner []any

func (v valuesScanner) Scan(dest ...any) error {
	if len(dest) != len(v) {
		return fmt.Errorf("scan: %d destinations for %d values", len(dest), len(v))
	}
	for i, d := range dest {
		if err := assignValue(d, v[i]); err != nil {
			return err
		}
	}
	return nil
}

// Stores a driver value into a scan destination
func assignValue(dest, value any) error {
	switch p := dest.(type) {
	case *string:
		switch v := value.(type) {
		case nil:
			*p = ""
		case string:
			*p = v
		case []byte:
			*p = string(v)
		default:
			*p = fmt.Sprint(v)
		}
	case *sql.NullString:
		switch v := value.(type) {
		case nil:
			*p = sql.NullString{}
		case string:
			*p = sql.NullString{String: v, Valid: true}
		case []byte:
			*p = sql.NullString{String: string(v), Valid: true}
		default:
			*p = sql.NullString{String: fmt.Sprint(v), Valid: true}
		}
	case *sql.NullFloat64:
		switch v := value.(type) {
		case nil:
			*p = sql.NullFloat64{}
		case float64:
			*p = sql.NullFloat64{Float64: v, Valid: true}
		case int64:
			*p = sql.NullFloat64{Float64: float64(v), Valid: true}
		}
	case *float64:
		switch v := value.(type) {
		case float64:
			*p = v
		case int64:
			*p = float64(v)
		case nil:
			*p = 0
		}
	case *int:
		switch v := value.(type) {
		case int64:
			*p = int(v)
		case float64:
			*p = int(v)
		case nil:
			*p = 0
		}
	case *int64:
		switch v := value.(type) {
		case int64:
			*p = v
		case float64:
			*p = int64(v)
		case nil:
			*p = 0
		}
	default:
		return fmt.Errorf("scan: unsupported destination %T", dest)
	}
	return nil
}

// Number of sessions, every row or only those of a source
func (d *DB) SessionCount(source string) (int, error) {
	var n int
	err := d.read(func(q querier) error {
		if source == "" {
			return q.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n)
		}
		return q.QueryRow("SELECT COUNT(*) FROM sessions WHERE source = ?", source).Scan(&n)
	})
	return n, err
}

// Compression chain of a session from its root to its tip
func (d *DB) CompressionChain(id string) ([]string, error) {
	root, err := d.ConversationRoot(id)
	if err != nil {
		return nil, err
	}
	chain := []string{root}
	cur := root
	seen := map[string]bool{root: true}
	for {
		var child string
		err := d.read(func(q querier) error {
			return q.QueryRow(`SELECT child.id FROM sessions child JOIN sessions parent ON parent.id = child.parent_session_id
				WHERE parent.id = ? AND parent.end_reason = 'compression'
				AND COALESCE(json_extract(child.model_config, '$._branched_from'), '') = ''
				AND COALESCE(json_extract(child.model_config, '$._delegate_from'), '') = ''
				ORDER BY child.started_at DESC LIMIT 1`, cur).Scan(&child)
		})
		if errors.Is(err, sql.ErrNoRows) {
			return chain, nil
		}
		if err != nil {
			return nil, err
		}
		if seen[child] {
			return chain, nil
		}
		seen[child] = true
		chain = append(chain, child)
		cur = child
	}
}

// Freshest continuation of a session, the session itself when it was never compressed away
func (d *DB) CompressionTip(id string) (string, error) {
	chain, err := d.CompressionChain(id)
	if err != nil {
		return "", err
	}
	return chain[len(chain)-1], nil
}

// Oldest ancestor of a session along compression continuations
func (d *DB) ConversationRoot(id string) (string, error) {
	cur := id
	seen := map[string]bool{id: true}
	for {
		var parent string
		err := d.read(func(q querier) error {
			return q.QueryRow(`SELECT parent.id FROM sessions child JOIN sessions parent ON parent.id = child.parent_session_id
				WHERE child.id = ? AND parent.end_reason = 'compression'
				AND COALESCE(json_extract(child.model_config, '$._branched_from'), '') = ''
				AND COALESCE(json_extract(child.model_config, '$._delegate_from'), '') = ''`, cur).Scan(&parent)
		})
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil
		}
		if err != nil {
			return "", err
		}
		if seen[parent] {
			return cur, nil
		}
		seen[parent] = true
		cur = parent
	}
}

// Deletes a session with its messages, orphaning children and removing delegate children
func (d *DB) DeleteSession(id string) error {
	return d.write(func(tx *Tx) error {
		ids := []string{id}
		rows, err := tx.Query("SELECT id FROM sessions WHERE parent_session_id = ? AND json_extract(model_config, '$._delegate_from') IS NOT NULL", id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var child string
			if err := rows.Scan(&child); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, child)
		}
		rows.Close()
		ph := placeholders(len(ids))
		if _, err := tx.Exec("UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN ("+ph+")", args(ids)...); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM compression_locks WHERE session_id IN ("+ph+")", args(ids)...); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM session_turn_leases WHERE conversation_id IN ("+ph+")", args(ids)...); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM messages WHERE session_id IN ("+ph+")", args(ids)...); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM sessions WHERE id IN ("+ph+")", args(ids)...); err != nil {
			return err
		}
		return deleteUnreferencedPrompts(tx)
	})
}

// Deletes ended sessions idle longer than the retention, returning how many went
func (d *DB) PruneSessions(olderThan time.Duration, source string) (int, error) {
	cutoff := now() - olderThan.Seconds()
	count := 0
	err := d.write(func(tx *Tx) error {
		where := `ended_at IS NOT NULL AND pinned = 0 AND COALESCE(last_activity_at,
			(SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id), started_at) < ?`
		params := []any{cutoff}
		if source != "" {
			where += " AND source = ?"
			params = append(params, source)
		}
		rows, err := tx.Query("SELECT id FROM sessions WHERE "+where, params...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		count = len(ids)
		for start := 0; start < len(ids); start += 500 {
			end := min(start+500, len(ids))
			chunk := ids[start:end]
			ph := placeholders(len(chunk))
			for _, stmt := range []string{
				"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
				"DELETE FROM messages WHERE session_id IN (" + ph + ")",
				"DELETE FROM compression_locks WHERE session_id IN (" + ph + ")",
				"DELETE FROM session_turn_leases WHERE conversation_id IN (" + ph + ")",
				"DELETE FROM sessions WHERE id IN (" + ph + ")",
			} {
				if _, err := tx.Exec(stmt, args(chunk)...); err != nil {
					return err
				}
			}
		}
		return deleteUnreferencedPrompts(tx)
	})
	return count, err
}

// Deletes ended sessions that never received a message
func (d *DB) DeleteEmptySessions() (int, error) {
	count := 0
	err := d.write(func(tx *Tx) error {
		res, err := tx.Exec(`DELETE FROM sessions WHERE message_count = 0 AND ended_at IS NOT NULL AND pinned = 0
			AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = sessions.id)
			AND NOT EXISTS (SELECT 1 FROM sessions c WHERE c.parent_session_id = sessions.id)`)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		count = int(n)
		return deleteUnreferencedPrompts(tx)
	})
	return count, err
}

// Archives sessions idle longer than the window, pinned ones excepted, returning how many
func (d *DB) ArchiveStaleSessions(idle time.Duration) (int, error) {
	cutoff := now() - idle.Seconds()
	count := 0
	err := d.write(func(tx *Tx) error {
		res, err := tx.Exec(`UPDATE sessions SET archived = 1, auto_archived = 1 WHERE archived = 0 AND pinned = 0
			AND COALESCE(last_activity_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id), started_at) < ?`, cutoff)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		count = int(n)
		return nil
	})
	return count, err
}

// Reclaims free pages, blocking writers while it runs
func (d *DB) Vacuum() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return &SQLiteError{Class: SQLiteProgrammingError, Msg: "Cannot operate on a closed database."}
	}
	_, err := d.conn.ExecContext(d.openCtx, "VACUUM")
	return err
}

// Share of the file that is free pages
func (d *DB) FreelistRatio() (float64, error) {
	var pages, free int64
	err := d.read(func(q querier) error {
		if err := q.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			return err
		}
		return q.QueryRow("PRAGMA freelist_count").Scan(&free)
	})
	if err != nil || pages == 0 {
		return 0, err
	}
	return float64(free) / float64(pages), nil
}

// Typed view of a session dict, nil for a missing row
func sessionFromRow(row *pyjson.Object) *Session {
	if row == nil {
		return nil
	}
	str := func(k string) string {
		if v, ok := rowValue(row, k).(string); ok {
			return v
		}
		return ""
	}
	f64 := func(k string) float64 {
		f, _ := pyFloatOf(orZero(rowValue(row, k)))
		return f
	}
	i64 := func(k string) int64 {
		n, err := pyIntOf(orZero(rowValue(row, k)))
		if err != nil {
			return 0
		}
		return n.Int64()
	}
	s := &Session{ID: str("id"), Source: str("source"), UserID: str("user_id"), Model: str("model"),
		SystemPrompt: str("system_prompt"), ParentSessionID: str("parent_session_id"), StartedAt: f64("started_at"),
		EndedAt: f64("ended_at"), EndReason: str("end_reason"), MessageCount: int(i64("message_count")),
		ToolCallCount: int(i64("tool_call_count")), InputTokens: i64("input_tokens"), OutputTokens: i64("output_tokens"),
		CacheReadTokens: i64("cache_read_tokens"), CacheWriteTokens: i64("cache_write_tokens"),
		ReasoningTokens: i64("reasoning_tokens"), Cwd: str("cwd"), GitBranch: str("git_branch"),
		GitRepoRoot: str("git_repo_root"), BillingProvider: str("billing_provider"), BillingBaseURL: str("billing_base_url"),
		BillingMode: str("billing_mode"), EstimatedCostUSD: f64("estimated_cost_usd"), ActualCostUSD: f64("actual_cost_usd"),
		CostStatus: str("cost_status"), CostSource: str("cost_source"), PricingVersion: str("pricing_version"),
		Title: str("title"), TitleSource: str("title_source"), LastActivityAt: f64("last_activity_at"),
		LastActivity: str("last_activity_description"), APICallCount: int(i64("api_call_count")),
		ProfileName: str("profile_name"), RewindCount: int(i64("rewind_count")), Archived: i64("archived") != 0,
		AutoArchived: i64("auto_archived") != 0, Pinned: i64("pinned") != 0, Hidden: i64("hidden") != 0,
		LastReadAt: f64("last_read_at"), TranscriptRev: i64("transcript_revision")}
	if cfg := parseModelConfigValue(rowValue(row, "model_config")); cfg.Len() > 0 {
		if b, err := cfg.MarshalJSON(); err == nil {
			_ = json.Unmarshal(b, &s.ModelConfig)
		}
	}
	if names := str("tool_names"); names != "" {
		_ = json.Unmarshal([]byte(names), &s.ToolNames)
	}
	return s
}

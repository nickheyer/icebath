package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Meta key prefix of a deletion fence a retired session leaves behind
const retiredSessionPrefix = "gateway.retired_session.v1."

// Error of a create that would recreate a deleted session beside its tombstone
var ErrSessionRetired = errors.New("session was deleted; its id cannot be reused")

// Fields a session upsert writes, nil pointers leaving the column NULL
type NewSession struct {
	ID     string
	Source string
	Model  *string
	// Serialized to JSON, nil or empty leaving the column NULL
	ModelConfig      map[string]any
	SystemPrompt     *string
	UserID           *string
	SessionKey       *string
	ChatID           *string
	ChatType         *string
	ThreadID         *string
	ParentSessionID  *string
	Cwd              *string
	ProfileName      *string
	GitRepoRoot      *string
	OriginJSON       *string
	DisplayName      *string
	TransportProfile *string
}

// Upsert tail keeping every routing and metadata column an earlier writer set
var upsertKeepExistingSQL = func() string {
	var parts []string
	for _, col := range []string{"session_key", "chat_id", "chat_type", "thread_id", "parent_session_id", "cwd", "profile_name",
		"transport_profile", "git_repo_root", "origin_json", "display_name"} {
		parts = append(parts, "                       "+col+" = COALESCE(sessions."+col+", excluded."+col+")")
	}
	return strings.Join(parts, ",\n") + ",\n" +
		"                       created_source = CASE WHEN " + unsetCreatedSQL("sessions.created_source") +
		" THEN COALESCE(NULLIF(excluded.created_source, 'local'), sessions.created_source) ELSE sessions.created_source END"
}()

// Predicate true when a created_source column holds no creation provenance
func unsetCreatedSQL(col string) string {
	return "COALESCE(" + col + ", '') IN ('', 'unknown', 'local')"
}

// Keep existing constructor upsert of one session row
var insertSessionSQL = `INSERT INTO sessions (
           id, source, created_source, user_id, session_key, chat_id, chat_type, thread_id,
           model, model_config, system_prompt, system_prompt_hash,
           parent_session_id, cwd, profile_name, transport_profile, git_repo_root,
           origin_json, display_name, started_at
        )
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(id) DO UPDATE SET
               source = CASE
                   WHEN sessions.source = 'unknown'
                   THEN COALESCE(excluded.source, 'unknown')
                   ELSE sessions.source
               END,
               model = COALESCE(sessions.model, excluded.model),
               model_config = CASE
                   WHEN excluded.model_config IS NOT NULL
                        AND json_type(
                            sessions.model_config, '$._reset_from'
                        ) IS NOT NULL
                        AND json_remove(
                            sessions.model_config, '$._reset_from'
                        ) = '{}'
                   THEN json_set(
                       excluded.model_config,
                       '$._reset_from',
                       json_extract(
                           sessions.model_config, '$._reset_from'
                       )
                   )
                   ELSE COALESCE(
                       sessions.model_config, excluded.model_config
                   )
               END,
               system_prompt_hash = COALESCE(
                   sessions.system_prompt_hash,
                   excluded.system_prompt_hash
               ),
               system_prompt = CASE
                   WHEN sessions.system_prompt_hash IS NULL
                        AND excluded.system_prompt_hash IS NOT NULL
                   THEN NULL
                   ELSE sessions.system_prompt
               END,
` + upsertKeepExistingSQL

// Parent to child profile fence, keyless rows inheriting freely and keyed rows agreeing on the namespace slot
const sameKeyNamespaceSQL = "p.session_key IS NULL OR sessions.session_key IS NULL" +
	" OR substr(p.session_key, 1, instr(substr(p.session_key, 7), ':') + 6)" +
	"  = substr(sessions.session_key, 1, instr(substr(sessions.session_key, 7), ':') + 6)"

// NULL filling assignment of col from the parent row, extra narrowing the parent lookup
func inheritColSQL(col, extra string) string {
	pad := strings.Repeat(" ", 30+len(col))
	return col + " = COALESCE(sessions." + col + ",\n" + pad + "(SELECT p." + col + " FROM sessions p\n" + pad +
		"  WHERE p.id = sessions.parent_session_id" + extra + "))"
}

// Inheritance statements, working metadata from any parent and routing columns only across a compression edge
var (
	inheritSep           = ",\n" + strings.Repeat(" ", 27)
	inheritParentMetaSQL = "UPDATE sessions\n                       SET " + strings.Join([]string{
		inheritColSQL("cwd", ""), inheritColSQL("git_repo_root", ""), inheritColSQL("git_branch", ""),
		inheritColSQL("profile_name", "\n"+strings.Repeat(" ", 46)+"AND ("+sameKeyNamespaceSQL+")"),
	}, inheritSep) + "\n                     WHERE id = ? AND parent_session_id IS NOT NULL"
	inheritParentRoutingSQL = func() string {
		var cols []string
		for _, c := range []string{"user_id", "session_key", "chat_id", "chat_type", "thread_id", "display_name", "origin_json", "transport_profile"} {
			cols = append(cols, inheritColSQL(c, ""))
		}
		fork := ""
		for _, marker := range []string{"_delegate_from", "_branched_from"} {
			fork += "\n                       AND COALESCE(" + jsonExtractSQL("model_config", "$."+marker) + ", '')" +
				"\n                           != parent_session_id"
		}
		return "UPDATE sessions\n                       SET " + strings.Join(cols, inheritSep) +
			"\n                     WHERE id = ? AND parent_session_id IS NOT NULL\n" +
			"                       AND EXISTS (\n                           SELECT 1 FROM sessions p\n" +
			"                           WHERE p.id = sessions.parent_session_id\n" +
			"                             AND p.end_reason = 'compression'\n                       )" + fork
	}()
)

// Profile id grammar a named profile directory matches
var profileDirName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Profile owning this store from its path alone, empty outside the profile tree
func (s *Store) ownProfileName() string {
	root, err := home.DefaultRoot()
	if err != nil {
		return ""
	}
	root = resolvePath(root)
	parent := filepath.Dir(resolvePath(s.path))
	if normCase(parent) == normCase(root) {
		return home.DefaultProfile
	}
	if normCase(filepath.Dir(parent)) == normCase(filepath.Join(root, "profiles")) && profileDirName.MatchString(filepath.Base(parent)) {
		return filepath.Base(parent)
	}
	return ""
}

// Bound value of an optional text field, NULL for nil
func nullable(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// JSON text of a model config, NULL for an empty one
func modelConfigJSON(cfg map[string]any) (any, error) {
	if len(cfg) == 0 {
		return nil, nil
	}
	enc, err := fileutil.EncodeJSON(cfg, fileutil.JSONStyle{Sorted: true, ASCII: true})
	if err != nil {
		return nil, err
	}
	return string(enc), nil
}

// Seconds since the epoch the store stamps rows with
func nowEpoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Upserts a session row without overwriting what an earlier writer set, inside the caller's transaction
func (s *Store) insertSessionRow(ctx context.Context, c Conn, n NewSession) error {
	var exists, retired int
	err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", n.ID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		err = c.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", retiredSessionPrefix+n.ID).Scan(&retired)
		if err == nil {
			return ErrSessionRetired
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	} else if err != nil {
		return err
	}
	profile := nullable(n.ProfileName)
	if n.ProfileName == nil || strings.TrimSpace(*n.ProfileName) == "" {
		profile = nil
		if own := s.ownProfileName(); own != "" {
			profile = own
		}
	}
	hash, err := storeSystemPrompt(ctx, c, n.SystemPrompt)
	if err != nil {
		return err
	}
	cfg, err := modelConfigJSON(n.ModelConfig)
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, insertSessionSQL, n.ID, n.Source, n.Source, nullable(n.UserID), nullable(n.SessionKey),
		nullable(n.ChatID), nullable(n.ChatType), nullable(n.ThreadID), nullable(n.Model), cfg, hash,
		nullable(n.ParentSessionID), nullable(n.Cwd), profile, nullable(n.TransportProfile), nullable(n.GitRepoRoot),
		nullable(n.OriginJSON), nullable(n.DisplayName), nowEpoch()); err != nil {
		return err
	}
	if hash.Valid {
		if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
			return err
		}
	}
	if n.ParentSessionID != nil && *n.ParentSessionID != "" {
		if err := inheritParentSessionMetadata(ctx, c, n.ID); err != nil {
			return err
		}
	}
	return stampCompressionAncestors(ctx, c, n.ID)
}

// NULL fills a child's working metadata from its parent and its routing columns only across a compression edge
func inheritParentSessionMetadata(ctx context.Context, c Conn, sessionID string) error {
	if _, err := c.ExecContext(ctx, inheritParentMetaSQL, sessionID); err != nil {
		return err
	}
	_, err := c.ExecContext(ctx, inheritParentRoutingSQL, sessionID)
	return err
}

// Creates or upserts a session row on the transcript budget
func (s *Store) CreateSession(ctx context.Context, n NewSession) error {
	return s.Write(ctx, TranscriptWritePatience, func(c Conn) error { return s.insertSessionRow(ctx, c, n) })
}

// Upserts a session row inside a transaction an authority already owns
func (s *Store) InsertSessionRowIn(ctx context.Context, c Conn, n NewSession) error {
	return s.insertSessionRow(ctx, c, n)
}

// Ensures a row exists, an empty source standing in as unknown
func (s *Store) EnsureSession(ctx context.Context, n NewSession) error {
	if n.Source == "" {
		n.Source = "unknown"
	}
	return s.CreateSession(ctx, n)
}

// Mirrors the gateway entry's expiry_finalized flag so it survives a lost sessions.json
func (s *Store) SetExpiryFinalized(ctx context.Context, sessionID string, finalized bool) error {
	if sessionID == "" {
		return nil
	}
	return s.WriteSQL(ctx, WritePatience, "UPDATE sessions SET expiry_finalized = ? WHERE id = ?", boolInt(finalized), sessionID)
}

// One for true, zero for false
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Most recent live session of a platform chat and thread, an exact sender winning, ambiguous senders yielding none
func (s *Store) FindSessionByOrigin(ctx context.Context, platform, chatID string, threadID, userID *string) (string, bool, error) {
	if platform == "" || chatID == "" {
		return "", false, nil
	}
	query := `
            SELECT id, user_id, started_at FROM sessions
            WHERE LOWER(source) = LOWER(?)
              AND session_key IS NOT NULL
              AND chat_id = ?
              AND ended_at IS NULL
        `
	args := []any{platform, chatID}
	if threadID != nil {
		query += " AND COALESCE(thread_id, '') = ?"
		args = append(args, *threadID)
	}
	type peer struct{ id, user string }
	var rows []peer
	err := s.Read(ctx, func(c Conn) error {
		rows = rows[:0]
		r, err := c.QueryContext(ctx, query+" ORDER BY started_at DESC", args...)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id string
			var user sql.NullString
			var started any
			if err := r.Scan(&id, &user, &started); err != nil {
				return err
			}
			rows = append(rows, peer{id, user.String})
		}
		return r.Err()
	})
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	if userID != nil && *userID != "" {
		for _, r := range rows {
			if r.user == *userID {
				return r.id, true, nil
			}
		}
		if len(rows) > 1 {
			return "", false, nil
		}
	} else {
		users := map[string]bool{}
		for _, r := range rows {
			if u := strings.TrimSpace(r.user); u != "" {
				users[u] = true
			}
		}
		if len(users) > 1 {
			return "", false, nil
		}
	}
	return rows[0].id, true, nil
}

// Session with its prompt and tool pin resolved, false when no row has the id
func (s *Store) GetSession(ctx context.Context, sessionID string) (Session, bool, error) {
	s.tokenBarrier(ctx, true)
	return s.getSessionRow(ctx, sessionID)
}

// Session lookup without the token barrier
func (s *Store) getSessionRow(ctx context.Context, sessionID string) (Session, bool, error) {
	var found []Session
	err := s.Read(ctx, func(c Conn) error {
		var err error
		found, err = querySessions(c.QueryContext(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
			"COALESCE(tp.prompt, s.tool_names) AS _tool_names_resolved "+
			"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
			"LEFT JOIN system_prompts tp ON tp.hash = s.tool_names WHERE s.id = ?", sessionID))
		return err
	})
	if err != nil || len(found) == 0 {
		return Session{}, false, err
	}
	return found[0], true, nil
}

// Picker columns of one session row
type SessionListRow struct {
	ID           string
	Source       string
	Title        sql.Null[string]
	StartedAt    float64
	EndedAt      sql.Null[float64]
	MessageCount sql.Null[int64]
	Model        sql.Null[string]
	Hidden       int64
	Archived     int64
}

// Picker columns of one session without the prompt joins or the token barrier, false when absent
func (s *Store) GetSessionListRow(ctx context.Context, sessionID string) (SessionListRow, bool, error) {
	var r SessionListRow
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT id, source, title, started_at, ended_at, message_count, model, hidden, archived "+
			"FROM sessions WHERE id = ?", sessionID).Scan(&r.ID, &r.Source, &r.Title, &r.StartedAt, &r.EndedAt, &r.MessageCount, &r.Model, &r.Hidden, &r.Archived)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return r, found, err
}

// Most recently used main loop route of a session as one coherent tuple
type ModelRoute struct {
	Model           string
	BillingProvider string
	BillingBaseURL  string
	BillingMode     string
	APICallCount    int64
}

// Route the session used most recently, false when its usage names none
func (s *Store) GetRecentSessionModelRoute(ctx context.Context, sessionID string) (ModelRoute, bool, error) {
	s.tokenBarrier(ctx, true)
	var r ModelRoute
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, `SELECT model, billing_provider, billing_base_url, billing_mode,
                      api_call_count
                 FROM session_model_usage
                WHERE session_id = ?
                  AND task = ''
                  AND model <> 'unknown'
                  AND billing_provider <> ''
                ORDER BY last_seen DESC, rowid DESC
                LIMIT 1`, sessionID).Scan(&r.Model, &r.BillingProvider, &r.BillingBaseURL, &r.BillingMode, &r.APICallCount)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return r, found, err
}

// Exact id, else the one unambiguous prefix match, false otherwise
func (s *Store) ResolveSessionID(ctx context.Context, idOrPrefix string) (string, bool, error) {
	if exact, ok, err := s.GetSession(ctx, idOrPrefix); err != nil || ok {
		return exact.ID, ok, err
	}
	var matches []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		matches, err = queryStrings(ctx, c, `SELECT id FROM sessions WHERE id LIKE ? ESCAPE '\' ORDER BY started_at DESC LIMIT 2`, EscapeLike(idOrPrefix)+"%")
		return err
	})
	if err != nil || len(matches) != 1 {
		return "", false, err
	}
	return matches[0], true, nil
}

// Stamps this store's own profile onto rows that predate durable ownership, returning rows stamped
func (s *Store) BackfillNullSessionProfiles(ctx context.Context, profileName string) (int64, error) {
	stamp := strings.TrimSpace(profileName)
	if stamp == "" {
		return 0, nil
	}
	return s.WriteRowcount(ctx, WritePatience, `UPDATE sessions
               SET profile_name = ?
             WHERE profile_name IS NULL OR TRIM(profile_name) = ''`, stamp)
}

// Promotes model_config.cwd into the cwd column of ACP rows lacking one, returning rows changed
func (s *Store) BackfillACPSessionCwd(ctx context.Context) (int64, error) {
	return s.WriteRowcount(ctx, WritePatience, `UPDATE sessions
                  SET cwd = json_extract(model_config, '$.cwd')
                WHERE source = 'acp'
                  AND COALESCE(cwd, '') = ''
                  AND json_valid(model_config)
                  AND COALESCE(json_extract(model_config, '$.cwd'), '') != ''`)
}

// Persists the authoritative cwd and claims a git metadata generation, false when the row is gone
func (s *Store) UpdateSessionCwd(ctx context.Context, sessionID, cwd, gitBranch, gitRepoRoot string, replaceGitMeta bool) (int64, bool, error) {
	if sessionID == "" || cwd == "" {
		return 0, false, nil
	}
	branch, root := strings.TrimSpace(gitBranch), strings.TrimSpace(gitRepoRoot)
	var generation int64
	found := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		found = false
		var current sql.NullString
		err := c.QueryRowContext(ctx, "SELECT cwd FROM sessions WHERE id = ?", sessionID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		sets := []string{"cwd = ?", "git_metadata_generation = COALESCE(git_metadata_generation, 0) + 1"}
		args := []any{cwd}
		if !current.Valid || current.String != cwd || replaceGitMeta {
			sets = append(sets, "git_branch = ?", "git_repo_root = ?")
			args = append(args, emptyNull(branch), emptyNull(root))
		} else {
			for _, f := range [][2]string{{"git_branch", branch}, {"git_repo_root", root}} {
				if f[1] != "" {
					sets = append(sets, f[0]+" = ?")
					args = append(args, f[1])
				}
			}
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(args, sessionID)...); err != nil {
			return err
		}
		err = c.QueryRowContext(ctx, "SELECT git_metadata_generation FROM sessions WHERE id = ?", sessionID).Scan(&generation)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return generation, found, err
}

// NULL for empty text, the text otherwise
func emptyNull(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Publishes async git enrichment only while its cwd claim is current, true when exactly one row took it
func (s *Store) PublishSessionGitMetadata(ctx context.Context, sessionID, cwd string, generation int64, gitBranch, gitRepoRoot string) (bool, error) {
	if sessionID == "" || cwd == "" || generation < 1 {
		return false, nil
	}
	var sets []string
	var args []any
	for _, f := range [][2]string{{"git_branch", strings.TrimSpace(gitBranch)}, {"git_repo_root", strings.TrimSpace(gitRepoRoot)}} {
		if f[1] != "" {
			sets = append(sets, f[0]+" = ?")
			args = append(args, f[1])
		}
	}
	if len(sets) == 0 {
		return false, nil
	}
	n, err := s.WriteRowcount(ctx, WritePatience, "UPDATE sessions SET "+strings.Join(sets, ", ")+
		" WHERE id = ? AND cwd = ? AND git_metadata_generation = ?", append(args, sessionID, cwd, generation)...)
	return n == 1, err
}

// Backfills git roots for cwds that have none, never replacing a recorded root
func (s *Store) BackfillRepoRoots(ctx context.Context, cwdToRoot map[string]string) error {
	var rows [][]any
	for cwd, root := range cwdToRoot {
		if root != "" && cwd != "" {
			rows = append(rows, []any{root, cwd})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return s.WriteMany(ctx, WritePatience, "UPDATE sessions SET git_repo_root = ? WHERE cwd = ? AND COALESCE(git_repo_root, '') = ''", rows)
}

// Where a durable activity stamp came from
type ActivityProvenance string

// Known activity provenances, the compression writers and the unknown default
const (
	ProvenanceUnknown                  ActivityProvenance = "unknown"
	ProvenanceAgentCompression         ActivityProvenance = "agent.compression"
	ProvenanceAgentCompressionTimeout  ActivityProvenance = "agent.compression_timeout"
	ProvenanceAgentCompressionCooldown ActivityProvenance = "agent.compression_cooldown"
	ProvenanceAgentCompressionTurnhold ActivityProvenance = "agent.compression_turnhold"
)

// Characters an activity description keeps
const activityDescriptionMax = 120

// Known provenance of p, unknown when unset or unrecognized
func NormalizeActivityProvenance(p string) ActivityProvenance {
	switch v := ActivityProvenance(strings.TrimSpace(p)); v {
	case ProvenanceUnknown, ProvenanceAgentCompression, ProvenanceAgentCompressionTimeout, ProvenanceAgentCompressionCooldown,
		ProvenanceAgentCompressionTurnhold:
		return v
	}
	return ProvenanceUnknown
}

// Activity text clamped to the shared description budget with an ellipsis
func BoundActivityDescription(description string) string {
	text := strings.TrimSpace(description)
	if utf8.RuneCountInString(text) <= activityDescriptionMax {
		return text
	}
	return string([]rune(text)[:activityDescriptionMax-1]) + "…"
}

// Stamps durable mid-turn activity on the activity budget, never moving last_activity_at backwards
func (s *Store) TouchSessionActivity(ctx context.Context, sessionID string, at time.Time, description string, provenance ActivityProvenance) error {
	if sessionID == "" {
		return nil
	}
	when := nowEpoch()
	if !at.IsZero() {
		when = float64(at.UnixNano()) / 1e9
	}
	return s.WriteSQL(ctx, ActivityWritePatience, "UPDATE sessions SET last_activity_at = ?, "+
		"last_activity_description = ?, last_activity_provenance = ? "+
		"WHERE id = ? AND (last_activity_at IS NULL OR last_activity_at < ?)",
		when, BoundActivityDescription(description), string(NormalizeActivityProvenance(string(provenance))), sessionID, when)
}

// Clears the activity labels after a turn on the activity budget, skipping the write when they are already clear
func (s *Store) ClearSessionActivityLabels(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	var desc, prov sql.NullString
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT last_activity_description, last_activity_provenance FROM sessions WHERE id = ?", sessionID).Scan(&desc, &prov)
		found = err == nil
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil && familyOf(err) == notSQLite {
		return err
	}
	if err == nil && found && desc.String == "" && (prov.String == "" || prov.String == string(ProvenanceUnknown)) {
		return nil
	}
	return s.WriteSQL(ctx, ActivityWritePatience, "UPDATE sessions SET last_activity_description = ?, last_activity_provenance = ? WHERE id = ?",
		"", string(ProvenanceUnknown), sessionID)
}

// Replaces model_config and fills model when given, behind the token write barrier
func (s *Store) UpdateSessionMeta(ctx context.Context, sessionID, modelConfigJSON string, model *string) error {
	s.tokenBarrier(ctx, false)
	return s.WriteSQL(ctx, WritePatience, "UPDATE sessions SET model_config = ?, model = COALESCE(?, model) WHERE id = ?",
		modelConfigJSON, nullable(model), sessionID)
}

// Stores the assembled system prompt snapshot, collecting prompts no row references any more
func (s *Store) UpdateSystemPrompt(ctx context.Context, sessionID string, systemPrompt *string) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		hash, err := storeSystemPrompt(ctx, c, systemPrompt)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET system_prompt_hash = ?, system_prompt = NULL WHERE id = ?", hash, sessionID); err != nil {
			return err
		}
		return deleteUnreferencedSystemPrompts(ctx, c)
	})
}

// Stores the tools pin in the content addressed prompt table so a rebuilt agent sends the same bytes
func (s *Store) UpdateSessionToolNames(ctx context.Context, sessionID string, pin any) error {
	var payload *string
	if pin != nil {
		text, err := pinJSON(pin)
		if err != nil {
			return err
		}
		payload = &text
	}
	return s.Write(ctx, WritePatience, func(c Conn) error {
		hash, err := storeSystemPrompt(ctx, c, payload)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET tool_names = ? WHERE id = ?", hash, sessionID); err != nil {
			return err
		}
		return deleteUnreferencedSystemPrompts(ctx, c)
	})
}

// JSON text of a tools pin, raw JSON re-encoded in the spaced layout with its order kept
func pinJSON(pin any) (string, error) {
	if raw, ok := pin.(json.RawMessage); ok {
		return pythonJSONText(raw)
	}
	enc, err := fileutil.EncodeJSON(pin, fileutil.JSONStyle{ASCII: true})
	if err != nil {
		return "", fmt.Errorf("tools pin is not JSON serializable: %w", err)
	}
	return string(enc), nil
}

// Barrier against queued token deltas, readers waiting only as long as the read path allows
func (s *Store) tokenBarrier(ctx context.Context, forRead bool) {
	s.flushQueuedTokenDeltas(ctx, forRead)
}

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
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const columns = `s.id, s.profile, s.source, COALESCE(s.created_source, ''), COALESCE(s.user_id, ''), COALESCE(s.session_key, ''), COALESCE(s.chat_id, ''),
	COALESCE(s.chat_type, ''), COALESCE(s.thread_id, ''), COALESCE(s.display_name, ''), COALESCE(s.origin_json, ''), COALESCE(s.model, ''),
	COALESCE(s.model_config, ''), COALESCE(s.parent_session_id, ''), s.started_at, s.ended_at, COALESCE(s.end_reason, ''), s.message_count,
	s.tool_call_count, s.api_call_count, s.input_tokens, s.output_tokens, s.cache_read_tokens, s.cache_write_tokens, s.reasoning_tokens,
	COALESCE(s.cwd, ''), COALESCE(s.git_branch, ''), COALESCE(s.git_repo_root, ''), COALESCE(s.billing_provider, ''), COALESCE(s.billing_base_url, ''),
	COALESCE(s.estimated_cost_usd, 0), COALESCE(s.actual_cost_usd, 0), COALESCE(s.cost_status, ''), COALESCE(s.cost_source, ''),
	COALESCE(s.pricing_version, ''), COALESCE(s.title, ''), COALESCE(s.title_source, ''), COALESCE(s.last_activity_at, s.started_at),
	COALESCE(s.last_activity_description, ''), s.rewind_count, s.archived, s.auto_archived, s.pinned, s.hidden, s.yolo, s.last_read_at`

// Start of the first message the user sent in the session the alias names, long enough to describe a skill turn from
func previewSQL(alias string) string {
	return `COALESCE((SELECT substr(m.content, 1, 400) FROM messages m WHERE m.session_id = ` + alias + `.id AND m.role = 'user' AND m.content IS NOT NULL
		AND (m.active = 1 OR m.compacted = 1) AND COALESCE(m.display_kind, '') <> 'hidden' AND m.summary = 0 ORDER BY m.id LIMIT 1), '')`
}

// Value of a JSON key of the model settings of the session the alias names, NULL for a row without it
func configKey(alias, key string) string {
	return "json_extract(CASE WHEN json_valid(" + alias + ".model_config) THEN " + alias + ".model_config ELSE '{}' END, '$." + key + "')"
}

// Session continued into a child by compression, which listings show as its newest continuation instead
func continuedSQL(alias string) string {
	return `(` + alias + `.end_reason = 'compression' AND EXISTS (SELECT 1 FROM sessions c WHERE c.parent_session_id = ` + alias + `.id AND ` +
		configKey("c", "_branched_from") + ` IS NULL AND ` + configKey("c", "_delegate_from") + ` IS NULL))`
}

// Child that continues a parent the compression way, over aliases c and p
const continuation = `p.end_reason = 'compression' AND json_extract(CASE WHEN json_valid(c.model_config) THEN c.model_config ELSE '{}' END, '$._branched_from') IS NULL
	AND json_extract(CASE WHEN json_valid(c.model_config) THEN c.model_config ELSE '{}' END, '$._delegate_from') IS NULL`

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner, extra ...any) (*sessionsv1.Session, error) {
	s := &sessionsv1.Session{Peer: &sessionsv1.Peer{}, Usage: &sessionsv1.Usage{}}
	var started, active float64
	var ended, read sql.NullFloat64
	var archived, autoArchived, pinned, hidden, yolo int
	dest := []any{&s.Id, &s.Profile, &s.Source, &s.CreatedSource, &s.Peer.UserId, &s.Peer.SessionKey, &s.Peer.ChatId, &s.Peer.ChatType, &s.Peer.ThreadId,
		&s.Peer.DisplayName, &s.Peer.Origin, &s.Model, &s.ModelConfig, &s.ParentId, &started, &ended, &s.EndReason, &s.Messages, &s.ToolCalls,
		&s.Usage.ApiCalls, &s.Usage.Input, &s.Usage.Output, &s.Usage.CacheRead, &s.Usage.CacheWrite, &s.Usage.Reasoning, &s.Directory, &s.GitBranch,
		&s.GitRoot, &s.Provider, &s.BaseUrl, &s.Usage.EstimatedCost, &s.Usage.ActualCost, &s.Usage.CostStatus, &s.Usage.CostSource,
		&s.Usage.PricingVersion, &s.Title, &s.TitleSource, &active, &s.Activity, &s.Rewinds, &archived, &autoArchived, &pinned, &hidden, &yolo, &read}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	s.Created, s.Updated = timestamppb.New(fromSeconds(started)), timestamppb.New(fromSeconds(active))
	if ended.Valid {
		s.Ended = timestamppb.New(fromSeconds(ended.Float64))
	}
	if read.Valid {
		s.Read = timestamppb.New(fromSeconds(read.Float64))
	}
	s.Archived, s.AutoArchived, s.Pinned, s.Hidden, s.Yolo = archived != 0, autoArchived != 0, pinned != 0, hidden != 0, yolo != 0
	if s.ModelConfig != "" {
		var markers struct {
			Branched  string `json:"_branched_from"`
			Delegated string `json:"_delegate_from"`
		}
		if json.Unmarshal([]byte(s.ModelConfig), &markers) == nil {
			s.BranchedFrom, s.DelegatedFrom = markers.Branched, markers.Delegated
		}
	}
	return s, nil
}

// Preview of a listed session: what the user typed first, a skill turn as its command, cut at 60 characters
func preview(raw string) string {
	if sessions.IsSummary(raw) {
		return ""
	}
	if described, ok := sessions.DescribeSkill(raw, " "); ok {
		raw = described
	}
	text := strings.Join(strings.Fields(raw), " ")
	if runes := []rune(text); len(runes) > 60 {
		return string(runes[:60]) + "..."
	}
	return text
}

func (s *store) Create(ctx context.Context, session *sessionsv1.Session) (*sessionsv1.Session, error) {
	id := session.GetId()
	if id == "" {
		id = newID(time.Now())
	}
	started := now()
	if session.GetCreated() != nil {
		started = seconds(session.GetCreated().AsTime())
	}
	title, err := sessions.SanitizeTitle(session.GetTitle())
	if err != nil {
		return nil, err
	}
	peer := session.GetPeer()
	err = s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, profile, source, created_source, user_id, session_key, chat_id, chat_type, thread_id,
			display_name, origin_json, model, model_config, parent_session_id, started_at, cwd, git_branch, git_repo_root, billing_provider, billing_base_url, yolo)
			VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
			NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`,
			id, session.GetProfile(), cmp.Or(session.GetSource(), "tui"), session.GetCreatedSource(), peer.GetUserId(), peer.GetSessionKey(), peer.GetChatId(),
			peer.GetChatType(), peer.GetThreadId(), peer.GetDisplayName(), peer.GetOrigin(), session.GetModel(), session.GetModelConfig(), session.GetParentId(),
			started, session.GetDirectory(), session.GetGitBranch(), session.GetGitRoot(), session.GetProvider(), session.GetBaseUrl(), boolInt(session.GetYolo())); err != nil {
			return err
		}
		if title == "" {
			return nil
		}
		source := cmp.Or(session.GetTitleSource(), sessions.TitleUser)
		_, err := setTitle(ctx, tx, id, title, source)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.changed(id)
	return s.Get(ctx, id)
}

// Session id carrying when it was made: date, time and six random hex digits
func newID(at time.Time) string {
	return at.Format("20060102_150405") + "_" + strs.Random(3)
}

func (s *store) Get(ctx context.Context, id string) (*sessionsv1.Session, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return get(ctx, db, id)
}

func get(ctx context.Context, q querier, id string) (*sessionsv1.Session, error) {
	session, err := scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM sessions s WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sessions.ErrNotFound
	}
	return session, err
}

func (s *store) Resolve(ctx context.Context, profile, ref string) (*sessionsv1.Session, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	if ref = strings.TrimSpace(ref); ref == "" {
		return nil, sessions.ErrNotFound
	}
	var ids []string
	rows, err := db.QueryContext(ctx, `SELECT id FROM sessions WHERE id = ? OR id LIKE ? ESCAPE '\' ORDER BY (id = ?) DESC, started_at DESC LIMIT 2`,
		ref, escapeLike(ref)+"%", ref)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) > 0 && (ids[0] == ref || len(ids) == 1) {
		return get(ctx, db, ids[0])
	}
	if len(ids) > 1 {
		return nil, sessions.ErrNotFound
	}
	return titled(ctx, db, profile, ref)
}

func (s *store) Titled(ctx context.Context, profile, title string) (*sessionsv1.Session, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return titled(ctx, db, profile, strings.TrimSpace(title))
}

// Session of the profile a title names, the latest numbered continuation of it first, save for a bot's chat, whose title is its own; any
// profile's when none is given
func titled(ctx context.Context, db querier, profile, title string) (*sessionsv1.Session, error) {
	if title == "" {
		return nil, sessions.ErrNotFound
	}
	scope, args := "", []any{}
	if profile != "" {
		scope, args = " AND profile = ?", []any{profile}
	}
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE title = ?`+scope+` ORDER BY started_at DESC LIMIT 1`, append([]any{title}, args...)...).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if id == "" || title != sessions.BotChat {
		var numbered string
		err := db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE title LIKE ? ESCAPE '\'`+scope+` ORDER BY started_at DESC LIMIT 1`,
			append([]any{escapeLike(title) + " #%"}, args...)...).Scan(&numbered)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if numbered != "" {
			id = numbered
		}
	}
	if id == "" {
		return nil, sessions.ErrNotFound
	}
	return get(ctx, db, id)
}

// Conditions and arguments of a filter over the sessions alias s
func (s *store) where(f *sessionsv1.Filter, upkeep bool) ([]string, []any) {
	var where []string
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if len(f.GetIds()) > 0 {
		add("s.id IN ("+marks(len(f.GetIds()))+")", strings2any(f.GetIds())...)
	}
	if !upkeep {
		if !f.GetIncludeArchived() && !f.GetOnlyArchived() {
			add("s.archived = 0")
		}
		if !f.GetIncludeHidden() {
			add("s.hidden = 0")
		}
		if !f.GetIncludeChildren() && !s.config.GetBool(sessionsconfig.ShowSubagents) {
			add(configKey("s", "_delegate_from") + " IS NULL")
		}
		if !f.GetIncludeEmpty() {
			add("(s.message_count > 0 OR s.title IS NOT NULL OR s.pinned = 1)")
		}
		if len(f.GetIds()) == 0 {
			add("NOT " + continuedSQL("s"))
		}
	}
	if f.GetOnlyPinned() {
		add("s.pinned = 1")
	}
	if f.GetOnlyArchived() {
		add("s.archived = 1")
	}
	if f.GetSource() != "" {
		add("(s.source = ? OR s.created_source = ?)", f.GetSource(), f.GetSource())
	}
	if len(f.GetExcludeSources()) > 0 {
		add("s.source NOT IN ("+marks(len(f.GetExcludeSources()))+")", strings2any(f.GetExcludeSources())...)
	}
	if f.GetProfile() != "" {
		add("s.profile = ?", f.GetProfile())
	}
	if dir := strings.TrimRight(f.GetDirectory(), `/\`); dir != "" {
		add(`(s.cwd = ? OR s.cwd LIKE ? ESCAPE '\')`, dir, escapeLike(dir)+"/%")
	}
	if words := f.GetSearch(); words != "" {
		needle := "%" + escapeLike(words) + "%"
		add(`(s.id LIKE ? ESCAPE '\' OR s.title LIKE ? ESCAPE '\' OR `+previewSQL("s")+` LIKE ? ESCAPE '\')`, needle, needle, needle)
	}
	last := "COALESCE(s.last_activity_at, s.started_at)"
	if t := f.GetActiveBefore(); t != nil {
		add(last+" < ?", seconds(t.AsTime()))
	}
	if t := f.GetActiveAfter(); t != nil {
		add(last+" >= ?", seconds(t.AsTime()))
	}
	if t := f.GetStartedBefore(); t != nil {
		add("s.started_at < ?", seconds(t.AsTime()))
	}
	if t := f.GetStartedAfter(); t != nil {
		add("s.started_at >= ?", seconds(t.AsTime()))
	}
	like := func(column, value string) {
		if value != "" {
			add("LOWER(COALESCE("+column+`, '')) LIKE ? ESCAPE '\'`, "%"+escapeLike(strings.ToLower(value))+"%")
		}
	}
	like("s.title", f.GetTitle())
	like("s.model", f.GetModel())
	like("s.git_branch", f.GetBranch())
	equal := func(column, value string) {
		if value != "" {
			add(column+" = ?", value)
		}
	}
	equal("s.end_reason", f.GetEndReason())
	equal("s.user_id", f.GetUserId())
	equal("s.chat_id", f.GetChatId())
	equal("s.chat_type", f.GetChatType())
	if f.GetProvider() != "" {
		add("LOWER(COALESCE(s.billing_provider, '')) = ?", strings.ToLower(f.GetProvider()))
	}
	bound := func(expression string, low, high any) {
		if low != nil {
			add(expression+" >= ?", low)
		}
		if high != nil {
			add(expression+" <= ?", high)
		}
	}
	optional := func(value *int64) any {
		if value == nil {
			return nil
		}
		return *value
	}
	money := func(value *float64) any {
		if value == nil {
			return nil
		}
		return *value
	}
	bound("s.message_count", optional(f.MinMessages), optional(f.MaxMessages))
	bound("(s.input_tokens + s.output_tokens)", optional(f.MinTokens), optional(f.MaxTokens))
	bound("COALESCE(s.actual_cost_usd, s.estimated_cost_usd, 0)", money(f.MinCost), money(f.MaxCost))
	bound("s.tool_call_count", optional(f.MinToolCalls), optional(f.MaxToolCalls))
	return where, args
}

func (s *store) List(ctx context.Context, f *sessionsv1.Filter) ([]*sessionsv1.Session, bool, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, false, err
	}
	where, args := s.where(f, false)
	order := "COALESCE(s.last_activity_at, s.started_at) DESC, s.started_at DESC, s.id DESC"
	if f.GetByStart() {
		order = "s.started_at DESC, s.id DESC"
	}
	query := `SELECT ` + columns + `, ` + previewSQL("s") + ` FROM sessions s` + whereSQL(where) + ` ORDER BY ` + order
	limit := int(f.GetLimit())
	if limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, limit+1, f.GetOffset())
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var list []*sessionsv1.Session
	for rows.Next() {
		var raw string
		session, err := scan(rows, &raw)
		if err != nil {
			return nil, false, err
		}
		session.Preview = preview(raw)
		list = append(list, session)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := limit > 0 && len(list) > limit
	if more {
		list = list[:limit]
	}
	if workspace := strings.ToLower(strings.TrimSpace(f.GetWorkspace())); workspace != "" {
		kept := list[:0]
		for _, session := range list {
			if key := cmp.Or(session.GetGitRoot(), session.GetDirectory()); key != "" {
				trimmed := strings.TrimRight(key, `/\`)
				base := strings.ToLower(trimmed[strings.LastIndexAny(trimmed, `/\`)+1:])
				if strings.Contains(strings.ToLower(key), workspace) || base == workspace {
					kept = append(kept, session)
				}
			}
		}
		list = kept
	}
	if f.GetStatus() {
		for _, session := range list {
			if session.Status, err = status(ctx, db, session.GetId()); err != nil {
				return nil, false, err
			}
		}
	}
	return list, more, nil
}

// How the session's last turn went, read from its last message: an error, cut short, complete, or no messages at all
func status(ctx context.Context, q querier, id string) (string, error) {
	var role, finish, calls sql.NullString
	err := q.QueryRowContext(ctx, `SELECT role, finish_reason, tool_calls FROM messages WHERE session_id = ? AND active = 1 ORDER BY id DESC LIMIT 1`, id).
		Scan(&role, &finish, &calls)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "empty", nil
	case err != nil:
		return "", err
	case finish.String == "error" || finish.String == "agent_error" || finish.String == "content_filter":
		return "error", nil
	case role.String == "user" || role.String == "tool" || role.String == "assistant" && calls.String != "" && calls.String != "[]":
		return "interrupted", nil
	}
	return "complete", nil
}

func (s *store) SetTitle(ctx context.Context, id, title, source string) (string, error) {
	var stored string
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		stored, err = setTitle(ctx, tx, id, title, source)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return stored, err
}

// Sets the title in the transaction at the authority of its source. A title held by another session is taken from it when that one is an
// ancestor this session continues, or an ended session that never held a message; otherwise it is refused
func setTitle(ctx context.Context, tx *sql.Tx, id, title, source string) (string, error) {
	title, err := sessions.SanitizeTitle(title)
	if err != nil {
		return "", err
	}
	var current, currentSource sql.NullString
	var hidden bool
	err = tx.QueryRowContext(ctx, `SELECT title, title_source, hidden <> 0 FROM sessions WHERE id = ?`, id).Scan(&current, &currentSource, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sessions.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if current.String == sessions.BotChat && hidden && title != sessions.BotChat {
		if source == sessions.TitleUser {
			return "", errors.New("This is the bot's canonical Bot Chat — its name is its identity, and renaming it would orphan the conversation. " +
				"To start fresh, create a new bot instead.")
		}
		return "", nil
	}
	if source != sessions.TitleUser && current.Valid && sessions.TitleRank(currentSource.String) >= sessions.TitleRank(source) {
		return "", nil
	}
	if title == "" {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET title = NULL, title_source = NULL WHERE id = ?`, id)
		return "", err
	}
	var holder string
	var ghost, retired bool
	err = tx.QueryRowContext(ctx, `SELECT id, message_count = 0 AND ended_at IS NOT NULL AND archived = 0 AND hidden = 0
		AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = sessions.id), archived <> 0 AND hidden <> 0 FROM sessions
		WHERE title = ? AND id <> ? AND profile = (SELECT profile FROM sessions WHERE id = ?)`, title, id, id).Scan(&holder, &ghost, &retired)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", err
	default:
		ancestor, err := continues(ctx, tx, id, holder)
		if err != nil {
			return "", err
		}
		// A bot chat retired, archived and hidden, gives its title up to the bot's new one
		if !ancestor && !ghost && !(title == sessions.BotChat && retired) {
			return "", fmt.Errorf("Title '%s' is already in use by session %s", title, holder)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET title = NULL, title_source = NULL WHERE id = ?`, holder); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`, title, source, id); err != nil {
		return "", err
	}
	return title, nil
}

// Whether the session continues the ancestor along compression, the ancestor its title passed down from
func continues(ctx context.Context, q querier, id, ancestor string) (bool, error) {
	var found int
	err := q.QueryRowContext(ctx, `WITH RECURSIVE up(id) AS (
			SELECT ? UNION SELECT p.id FROM up JOIN sessions c ON c.id = up.id JOIN sessions p ON p.id = c.parent_session_id WHERE `+continuation+`
		) SELECT 1 FROM up WHERE id = ? AND id <> ? LIMIT 1`, id, ancestor, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *store) NextTitle(ctx context.Context, profile, base string) (string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	return nextTitle(ctx, db, profile, base)
}

// Next free title of the lineage of the base among the profile's sessions, whose titles are their own
func nextTitle(ctx context.Context, q querier, profile, base string) (string, error) {
	if head, _, ok := sessions.NumberedTitle(base); ok {
		base = head
	}
	rows, err := q.QueryContext(ctx, `SELECT title FROM sessions WHERE profile = ? AND (title = ? OR title LIKE ? ESCAPE '\')`, profile, base,
		escapeLike(base)+" #%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	highest, any := 1, false
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return "", err
		}
		any = true
		if _, n, ok := sessions.NumberedTitle(title); ok && n > highest {
			highest = n
		}
	}
	if err := rows.Err(); err != nil || !any {
		return base, err
	}
	return fmt.Sprintf("%s #%d", base, highest+1), nil
}

func (s *store) Delete(ctx context.Context, id string) ([]string, error) {
	var removed []string
	owner := ""
	err := s.write(ctx, func(tx *sql.Tx) error {
		removed = nil
		session, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		owner = session.GetProfile()
		scope, err := lineage(ctx, tx, id)
		if err != nil {
			return err
		}
		children, err := delegated(ctx, tx, scope)
		if err != nil {
			return err
		}
		targets := append(scope, children...)
		if busy, err := guarded(ctx, tx, targets); err != nil {
			return err
		} else if len(busy) > 0 {
			return fmt.Errorf("Cannot delete active session: session '%s' (or a delegate child) has an active turn lease or compression lock", id)
		}
		if err := deleteRows(ctx, tx, targets); err != nil {
			return err
		}
		removed = targets
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.changed(removed...)
	s.registry.Deleted(ctx, s.config.In(owner), removed, "delete", id)
	return removed, nil
}

// Deletes the sessions and their messages, leaving the sessions branched from them without a parent, and the prompts no session holds now
func deleteRows(ctx context.Context, tx *sql.Tx, ids []string) error {
	for _, chunk := range chunks(ids) {
		marks, args := marks(len(chunk)), strings2any(chunk)
		for _, query := range []string{
			"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + marks + ")",
			"DELETE FROM messages WHERE session_id IN (" + marks + ")",
			"DELETE FROM session_turn_leases WHERE session_id IN (" + marks + ")",
			"DELETE FROM compression_locks WHERE session_id IN (" + marks + ")",
			"DELETE FROM sessions WHERE id IN (" + marks + ")",
		} {
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM system_prompts WHERE hash NOT IN (SELECT system_prompt_hash FROM sessions WHERE system_prompt_hash IS NOT NULL)`)
	return err
}

// The session and every session of its compression chain, both ways
func lineage(ctx context.Context, q querier, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE chain(id) AS (
			SELECT ?
			UNION SELECT p.id FROM chain JOIN sessions c ON c.id = chain.id JOIN sessions p ON p.id = c.parent_session_id WHERE `+continuation+`
			UNION SELECT c.id FROM chain JOIN sessions p ON p.id = chain.id JOIN sessions c ON c.parent_session_id = p.id WHERE `+continuation+`
		) SELECT id FROM chain`, id)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// Sessions the agents of the sessions delegated to, and theirs in turn
func delegated(ctx context.Context, q querier, ids []string) ([]string, error) {
	var out []string
	frontier := ids
	for len(frontier) > 0 {
		rows, err := q.QueryContext(ctx, `SELECT id FROM sessions WHERE `+configKey("sessions", "_delegate_from")+` IN (`+marks(len(frontier))+`)`,
			strings2any(frontier)...)
		if err != nil {
			return nil, err
		}
		next, err := collect(rows)
		if err != nil {
			return nil, err
		}
		frontier = next[:0]
		for _, id := range next {
			if !slices.Contains(out, id) && !slices.Contains(ids, id) {
				out = append(out, id)
				frontier = append(frontier, id)
			}
		}
	}
	return out, nil
}

// Sessions of the ids a live turn lease or compression lock holds
func guarded(ctx context.Context, q querier, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT session_id FROM session_turn_leases WHERE expires_at > ? AND session_id IN (`+marks(len(ids))+`)
		UNION SELECT session_id FROM compression_locks WHERE expires_at > ? AND session_id IN (`+marks(len(ids))+`)`,
		append(append(append([]any{now()}, strings2any(ids)...), now()), strings2any(ids)...)...)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

func (s *store) Mark(ctx context.Context, mark *sessionsv1.MarkRequest) (*sessionsv1.Session, error) {
	id := mark.GetId()
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := get(ctx, tx, id); err != nil {
			return err
		}
		// Marks hold for the whole compression lineage, so a session reads the same whichever of its parts is listed
		scope, err := lineage(ctx, tx, id)
		if err != nil {
			return err
		}
		set := func(assignments string, args ...any) error {
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET `+assignments+` WHERE id IN (`+marks(len(scope))+`)`, append(args, strings2any(scope)...)...)
			return err
		}
		if mark.Pinned != nil {
			if err := set(`pinned = ?`, boolInt(mark.GetPinned())); err != nil {
				return err
			}
			// Pinning brings a hidden session back into the lists, save a bot's chat, which stays hidden
			var botChat bool
			if err := tx.QueryRowContext(ctx, `SELECT hidden <> 0 AND COALESCE(title, '') = ? FROM sessions WHERE id = ?`, sessions.BotChat, id).Scan(&botChat); err != nil {
				return err
			}
			if mark.GetPinned() && !botChat && mark.Hidden == nil {
				if err := set(`hidden = 0`); err != nil {
					return err
				}
			}
		}
		if mark.Archived != nil {
			if err := set(`archived = ?, auto_archived = 0`, boolInt(mark.GetArchived())); err != nil {
				return err
			}
		}
		if mark.Hidden != nil {
			if err := set(`hidden = ?`, boolInt(mark.GetHidden())); err != nil {
				return err
			}
		}
		if mark.GetRead() {
			if err := set(`last_read_at = ?`, now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.changed(id)
	return s.Get(ctx, id)
}

func (s *store) Update(ctx context.Context, id string, change sessions.Change) error {
	var sets []string
	var args []any
	set := func(column string, value *string) {
		if value != nil {
			sets = append(sets, column+" = NULLIF(?, '')")
			args = append(args, *value)
		}
	}
	set("model", change.Model)
	set("billing_provider", change.Provider)
	set("billing_base_url", change.BaseURL)
	set("cwd", change.Directory)
	set("git_branch", change.GitBranch)
	set("git_repo_root", change.GitRoot)
	set("source", change.Source)
	if change.Activity != nil {
		sets = append(sets, "last_activity_description = NULLIF(?, '')", "last_activity_at = MAX(COALESCE(last_activity_at, 0), ?)")
		args = append(args, *change.Activity, now())
	}
	if change.Yolo != nil {
		sets = append(sets, "yolo = ?")
		args = append(args, boolInt(*change.Yolo))
	}
	if peer := change.Peer; peer != nil {
		sets = append(sets, "user_id = COALESCE(NULLIF(?, ''), user_id)", "session_key = COALESCE(NULLIF(?, ''), session_key)",
			"chat_id = COALESCE(NULLIF(?, ''), chat_id)", "chat_type = COALESCE(NULLIF(?, ''), chat_type)", "thread_id = COALESCE(NULLIF(?, ''), thread_id)",
			"display_name = COALESCE(NULLIF(?, ''), display_name)", "origin_json = COALESCE(NULLIF(?, ''), origin_json)")
		args = append(args, peer.GetUserId(), peer.GetSessionKey(), peer.GetChatId(), peer.GetChatType(), peer.GetThreadId(), peer.GetDisplayName(),
			peer.GetOrigin())
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if len(sets) > 0 {
			result, err := tx.ExecContext(ctx, `UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n == 0 {
				return sessions.ErrNotFound
			}
		}
		if len(change.ModelConfig) == 0 {
			return nil
		}
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT model_config FROM sessions WHERE id = ?`, id).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
			return sessions.ErrNotFound
		} else if err != nil {
			return err
		}
		merged := map[string]any{}
		if raw.Valid {
			json.Unmarshal([]byte(raw.String), &merged)
		}
		for key, value := range change.ModelConfig {
			if value == nil {
				delete(merged, key)
			} else {
				merged[key] = value
			}
		}
		var text any
		if len(merged) > 0 {
			data, err := json.Marshal(merged)
			if err != nil {
				return err
			}
			text = string(data)
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET model_config = ? WHERE id = ?`, text, id)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

func (s *store) Move(ctx context.Context, id, profile string) error {
	err := s.change(ctx, id, `UPDATE sessions SET profile = ? WHERE id = ?`, profile, id)
	if err == nil {
		s.changed(id)
	}
	return err
}

func (s *store) Clear(ctx context.Context, profile string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE profile = ?`, profile)
		if err != nil {
			return err
		}
		ids, err := collect(rows)
		if err != nil {
			return err
		}
		if err := deleteRows(ctx, tx, ids); err != nil {
			return err
		}
		return purgeIdentity(ctx, tx, profile)
	})
	if err == nil {
		s.changed()
	}
	return s.failed(err)
}

func (s *store) End(ctx context.Context, id, reason string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL`, now(), reason, id)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

func (s *store) Reopen(ctx context.Context, id string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ? AND ended_at IS NOT NULL
			AND COALESCE(end_reason, '') <> 'compression'`, id)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

func (s *store) Branch(ctx context.Context, branch *sessionsv1.BranchRequest) (*sessionsv1.Session, error) {
	child := newID(time.Now())
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		parent, err := get(ctx, tx, branch.GetId())
		if err != nil {
			return err
		}
		if busy, err := guarded(ctx, tx, []string{parent.GetId()}); err != nil {
			return err
		} else if len(busy) > 0 {
			return errors.New("A reply is running in this session. Stop it first, then branch.")
		}
		config, err := json.Marshal(map[string]string{"_branched_from": parent.GetId()})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, profile, source, created_source, model, model_config, system_prompt_hash, tool_names,
			parent_session_id, started_at, cwd, git_branch, git_repo_root, billing_provider, billing_base_url, yolo)
			SELECT ?, profile, source, created_source, model, ?, system_prompt_hash, tool_names, id, ?, cwd, git_branch, git_repo_root, billing_provider,
			billing_base_url, yolo FROM sessions WHERE id = ?`, child, string(config), now(), parent.GetId()); err != nil {
			return err
		}
		before := branch.GetBefore()
		if before == 0 {
			before = 1 << 62
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, tool_name, timestamp, token_count,
			finish_reason, reasoning, failed, payload, active, compacted, summary, api_content, display_kind, model_only, message_uid, absorbed_message_uids,
			platform_message_id, observed)
			SELECT ?, role, content, tool_calls, tool_call_id, tool_name, timestamp, token_count, finish_reason, reasoning, failed, payload, active, compacted,
			summary, api_content, display_kind, model_only, message_uid, absorbed_message_uids, NULL, observed
			FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1) AND id < ? ORDER BY id`, child, parent.GetId(), before); err != nil {
			return err
		}
		if err := recount(ctx, tx, child); err != nil {
			return err
		}
		if title := strings.TrimSpace(branch.GetTitle()); title != "" {
			_, err := setTitle(ctx, tx, child, title, sessions.TitleUser)
			return err
		}
		next, err := nextTitle(ctx, tx, parent.GetProfile(), cmp.Or(parent.GetTitle(), "branch"))
		if err != nil {
			return err
		}
		_, err = setTitle(ctx, tx, child, next, sessions.TitleDerived)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.changed(child)
	return s.Get(ctx, child)
}

// Counts the session's active messages and tool calls again into its record
func recount(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET
		message_count = (SELECT COUNT(*) FROM messages WHERE session_id = ?1 AND active = 1),
		tool_call_count = COALESCE((SELECT SUM(json_array_length(tool_calls)) FROM messages WHERE session_id = ?1 AND active = 1 AND json_valid(tool_calls)), 0)
		WHERE id = ?1`, id)
	return err
}

func (s *store) RecordUsage(ctx context.Context, id, model, task string, usage *sessionsv1.Usage) error {
	err := s.writeWithin(ctx, transcriptPatience, func(tx *sql.Tx) error {
		var provider, base sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT billing_provider, billing_base_url FROM sessions WHERE id = ?`, id).Scan(&provider, &base); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sessions.ErrNotFound
			}
			return err
		}
		at := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_model_usage (session_id, model, billing_provider, billing_base_url, task, api_call_count,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, estimated_cost_usd, actual_cost_usd, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (session_id, model, billing_provider, billing_base_url, task) DO UPDATE SET
			api_call_count = api_call_count + excluded.api_call_count, input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens, cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
			cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens, reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
			estimated_cost_usd = estimated_cost_usd + excluded.estimated_cost_usd, actual_cost_usd = actual_cost_usd + excluded.actual_cost_usd,
			last_seen = excluded.last_seen`,
			id, cmp.Or(model, "unknown"), provider.String, base.String, task, usage.GetApiCalls(), usage.GetInput(), usage.GetOutput(), usage.GetCacheRead(),
			usage.GetCacheWrite(), usage.GetReasoning(), usage.GetEstimatedCost(), usage.GetActualCost(), at, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET api_call_count = api_call_count + ?, input_tokens = input_tokens + ?,
			output_tokens = output_tokens + ?, cache_read_tokens = cache_read_tokens + ?, cache_write_tokens = cache_write_tokens + ?,
			reasoning_tokens = reasoning_tokens + ?, estimated_cost_usd = COALESCE(estimated_cost_usd, 0) + ?, actual_cost_usd = COALESCE(actual_cost_usd, 0) + ?,
			cost_status = COALESCE(NULLIF(?, ''), cost_status), cost_source = COALESCE(NULLIF(?, ''), cost_source),
			pricing_version = COALESCE(NULLIF(?, ''), pricing_version) WHERE id = ?`,
			usage.GetApiCalls(), usage.GetInput(), usage.GetOutput(), usage.GetCacheRead(), usage.GetCacheWrite(), usage.GetReasoning(), usage.GetEstimatedCost(),
			usage.GetActualCost(), usage.GetCostStatus(), usage.GetCostSource(), usage.GetPricingVersion(), id)
		return err
	})
	if err == nil {
		s.changed(id)
	}
	return err
}

// Runs one statement that must change the session, which is not found otherwise
func (s *store) change(ctx context.Context, id, query string, args ...any) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return sessions.ErrNotFound
		}
		return nil
	})
}

func whereSQL(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func strings2any(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func collect(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// Groups of ids small enough for one IN list
func chunks(ids []string) [][]string {
	var out [][]string
	for len(ids) > 0 {
		n := min(len(ids), 900)
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

func boolInt(on bool) int {
	if on {
		return 1
	}
	return 0
}

// Text as a LIKE pattern matches it literally, with backslash escapes
func escapeLike(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
}

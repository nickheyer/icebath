package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Page size the session listings and searches use when a caller names none
const DefaultListLimit = 20

// Sources human pickers never list, kanban workers, tool integrations and one-shot runs
func InternalListingSources() []string { return []string{"kanban", "tool", "oneshot"} }

// Lifecycle statuses a picker shows, classified from the final message row only
const (
	SessionStatusComplete    = "complete"
	SessionStatusInterrupted = "interrupted"
	SessionStatusError       = "error"
	SessionStatusEmpty       = "empty"
)

// Status from the last message, an error finish first, then a pending call or a trailing user or tool row
func ClassifySessionStatus(role string, hasToolCalls bool, finishReason string) string {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "error", "agent_error", "content_filter":
		return SessionStatusError
	}
	r := strings.ToLower(strings.TrimSpace(role))
	if r == "user" || r == "tool" || (r == "assistant" && hasToolCalls) {
		return SessionStatusInterrupted
	}
	return SessionStatusComplete
}

// Filters every session listing and count shares, so a paired count lines up with the listed rows
type SessionFilter struct {
	Source         string
	Sources        []string
	SessionKey     string
	ExcludeSources []string
	CwdPrefix      string
	// Rows with at least this many live messages, zero listing every row
	MinMessageCount int
	ArchivedOnly    bool
	IncludeArchived bool
	// Re-admits subagent runs to a listing that hides children
	IncludeSubagents bool
	// Creator surfaces a source local row must carry to match
	LocalCreatedSources []string
}

// Listing form of the rich session list
type SessionListOptions struct {
	SessionFilter
	// Rows per page as SQLite reads a LIMIT, zero listing none and a negative value listing all
	Limit  int
	Offset int
	// Lists compression continuations and subagent runs as rows of their own
	IncludeChildren bool
	// Keeps each compression root as itself instead of showing its live tip
	KeepCompressionRoots bool
	// Ranks by the freshest activity across each compression chain
	OrderByLastActive bool
	// Matches a row whose own id or any forward chain id contains the text
	IDQuery string
	// Matches titles and chain ids, punctuation insensitive
	SearchQuery string
	// Omits the prompt storage columns
	CompactRows bool
	// Back-fills pinned rows the page missed, archived ones included
	IncludePinned bool
	IncludeHidden bool
}

// WHERE clause text of clauses with a lead, empty when there are none
func whereSQL(clauses []string, lead string) string {
	if len(clauses) == 0 {
		return ""
	}
	return lead + "WHERE " + strings.Join(clauses, " AND ")
}

// Path prefix predicate, the raw prefix or anything below it with LIKE wildcards escaped
func cwdPrefixClause(cwdPrefix string) (string, []any) {
	prefix := strings.TrimRight(cwdPrefix, `/\`)
	if prefix == "" {
		prefix = cwdPrefix
	}
	esc := EscapeLike(prefix)
	return `(s.cwd = ? OR s.cwd LIKE ? ESCAPE '\' OR s.cwd LIKE ? ESCAPE '\')`, []any{prefix, esc + "/%", esc + `\\%`}
}

// Workspace predicate, the git root equal to key or a rootless row whose cwd is at or under it
func workspaceKeyClause(key string) (string, []any) {
	prefix := strings.TrimRight(key, `/\`)
	if prefix == "" {
		prefix = key
	}
	cwd, args := cwdPrefixClause(prefix)
	return "(s.git_repo_root = ? OR (COALESCE(s.git_repo_root, '') = '' AND " + cwd + "))", append([]any{prefix}, args...)
}

// Shared sessions WHERE clauses in the reference order, children hiding subagents and continuations unless re-admitted
func sessionFilterWhere(excludeChildren bool, f SessionFilter) ([]string, []any) {
	var where []string
	var args []any
	if excludeChildren && f.IncludeSubagents {
		where = append(where, "("+listableChildSQL+" OR "+delegateFromSQL("s.model_config")+" IS NOT NULL)")
	} else if excludeChildren {
		where = append(where, listableChildSQL, delegateFromSQL("s.model_config")+" IS NULL")
	}
	include := f.Sources
	if f.Source != "" {
		include = []string{f.Source}
	}
	if clause, bound := sourceMatchClause(include, f.LocalCreatedSources); len(bound) > 0 {
		where, args = append(where, clause), append(args, bound...)
	}
	if f.SessionKey != "" {
		where, args = append(where, "s.session_key = ?"), append(args, f.SessionKey)
	}
	if len(f.ExcludeSources) > 0 {
		where, args = append(where, "s.source NOT IN ("+bindMarks(len(f.ExcludeSources))+")"), append(args, stringArgs(f.ExcludeSources)...)
	}
	if f.CwdPrefix != "" {
		clause, bound := cwdPrefixClause(f.CwdPrefix)
		where, args = append(where, clause), append(args, bound...)
	}
	if f.MinMessageCount > 0 {
		where, args = append(where, "s.message_count >= ?"), append(args, f.MinMessageCount)
	}
	if f.ArchivedOnly {
		where = append(where, "s.archived = 1")
	} else if !f.IncludeArchived {
		where = append(where, "s.archived = 0")
	}
	return where, args
}

// Columns a compact row omits, the prompt storage internals
var sessionCompactExcluded = []string{"system_prompt", "system_prompt_hash", "git_metadata_generation"}

// Prefixed select list of every declared sessions column a compact row keeps
func compactSessionCols() (string, error) {
	tables, err := declaredSchema()
	if err != nil {
		return "", err
	}
	for _, t := range tables {
		if t.name != "sessions" {
			continue
		}
		var cols []string
		for _, c := range t.columns {
			if !containsString(sessionCompactExcluded, c.name) {
				cols = append(cols, "s."+c.name)
			}
		}
		return strings.Join(cols, ", "), nil
	}
	return "", errors.New("declared schema has no sessions table")
}

// Chain search predicates admitting a row whose own id or forward chain matches, titles too for a search
func chainSearchWhere(where, idNeedle, searchNeedle string) (string, []any) {
	like := func(n string) string { return "%" + EscapeLike(n) + "%" }
	var clauses []string
	var args []any
	if idNeedle != "" {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM chain cq        WHERE cq.root_id = s.id"+
			`          AND LOWER(cq.cur_id) LIKE ? ESCAPE '\')`)
		args = append(args, like(idNeedle))
	}
	if searchNeedle != "" {
		compact := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				return r
			}
			return -1
		}, searchNeedle)
		clause := "EXISTS (SELECT 1 FROM chain cq JOIN sessions cs ON cs.id = cq.cur_id" +
			` WHERE cq.root_id = s.id AND (LOWER(COALESCE(cs.title, '')) LIKE ? ESCAPE '\'` +
			` OR LOWER(cq.cur_id) LIKE ? ESCAPE '\'`
		args = append(args, like(searchNeedle), like(searchNeedle))
		if compact != "" {
			clause += " OR REPLACE(REPLACE(REPLACE(REPLACE(LOWER(COALESCE(cs.title, ''))," +
				` '-', ''), '_', ''), '.', ''), ' ', '') LIKE ? ESCAPE '\'`
			args = append(args, like(compact))
		}
		clauses = append(clauses, clause+"))")
	}
	if len(clauses) == 0 {
		return where, args
	}
	combined := strings.Join(clauses, " AND ")
	if where != "" {
		return where + " AND " + combined, args
	}
	return "WHERE " + combined, args
}

// Lists sessions with previews and recency in one query, compression roots shown as their live tips
func (s *Store) ListSessionsRich(ctx context.Context, o SessionListOptions) ([]SessionListing, error) {
	s.tokenBarrier(ctx, true)
	limit := o.Limit
	where, args := sessionFilterWhere(!o.IncludeChildren, o.SessionFilter)
	if !o.IncludeHidden && !o.ArchivedOnly {
		where = append(where, "s.hidden = 0")
	}
	var heads map[string]localHead
	if !o.IncludeChildren {
		var err error
		if heads, err = s.excludeSupersededSegments(ctx, &where, &args); err != nil {
			return nil, err
		}
	}
	cols := "s.*"
	if o.CompactRows {
		var err error
		if cols, err = compactSessionCols(); err != nil {
			return nil, err
		}
	}
	head := "SELECT " + cols
	if !o.CompactRows {
		head += ", COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved"
	}
	head += ",\n                    " + previewRawSQL("s.id") + ",\n                    "
	promptJoin := ""
	if !o.CompactRows {
		promptJoin = "LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash"
	}
	fromSessions := "FROM sessions s\n                " + promptJoin
	ws := whereSQL(where, "")
	var query string
	var params []any
	if o.OrderByLastActive {
		idNeedle := strings.ToLower(strings.TrimFunc(o.IDQuery, previewSpace))
		searchNeedle := strings.ToLower(strings.TrimFunc(o.SearchQuery, previewSpace))
		seed := ws
		if idNeedle == "" && searchNeedle == "" {
			seed = whereSQL(append(append([]string{}, where...), "EXISTS (SELECT 1 FROM sessions _seed_c WHERE _seed_c.parent_session_id = s.id)"), "")
		}
		outer, idArgs := chainSearchWhere(ws, idNeedle, searchNeedle)
		query = `
                WITH RECURSIVE chain(root_id, cur_id) AS (
                    SELECT s.id, s.id FROM sessions s ` + seed + `
                    UNION ALL
                    SELECT c.root_id, child.id
                    FROM chain c
                    JOIN sessions parent ON parent.id = c.cur_id
                    JOIN sessions child ON child.parent_session_id = c.cur_id
                    WHERE parent.end_reason = 'compression'
                      AND ` + JSONExtractSQL("child.model_config", "$._branched_from") + ` IS NULL
                      AND ` + JSONExtractSQL("child.model_config", "$._delegate_from") + ` IS NULL
                      AND NOT (` + ResetChildSQL("child") + `)
                      AND COALESCE(child.source, '') != 'tool'
                ),
                chain_max AS (
                    SELECT
                        root_id,
                        MAX(` + sqlSessionLastActiveByID("cur_id") + `) AS effective_last_active
                    FROM chain
                    GROUP BY root_id
                ),
                page AS (
                    SELECT s.id AS page_id,
                        COALESCE(cm.effective_last_active, ` + SessionLastActiveSQL("s") + `,
                                 ` + sqlInWindow("s.started_at") + `) AS effective_last_active
                    FROM sessions s
                    LEFT JOIN chain_max cm ON cm.root_id = s.id
                    ` + outer + `
                    ORDER BY effective_last_active DESC, s.started_at DESC, s.id DESC
                    LIMIT ? OFFSET ?
                )
                ` + head + SessionLastActiveSQL("s") + ` AS last_active,
                    page.effective_last_active AS _effective_last_active
                FROM page
                JOIN sessions s ON s.id = page.page_id
                ` + promptJoin + `
                ORDER BY _effective_last_active DESC, s.started_at DESC, s.id DESC
            `
		params = append(append(append(append([]any{}, args...), args...), idArgs...), limit, o.Offset)
	} else {
		query = `
                ` + head + SessionLastActiveSQL("s") + ` AS last_active
                ` + fromSessions + `
                ` + ws + `
                ORDER BY s.started_at DESC
                LIMIT ? OFFSET ?
            `
		params = append(append([]any{}, args...), limit, o.Offset)
	}
	rows, err := s.readListings(ctx, query, params)
	if err != nil {
		return nil, err
	}
	if o.IncludePinned {
		seen := map[string]bool{}
		for _, r := range rows {
			seen[r.ID] = true
		}
		pf := o.SessionFilter
		pf.ArchivedOnly, pf.IncludeArchived = false, true
		pw, pa := sessionFilterWhere(!o.IncludeChildren, pf)
		if !o.IncludeHidden && !o.ArchivedOnly {
			pw = append(pw, "s.hidden = 0")
		}
		if len(heads) > 0 {
			if _, err := s.excludeSupersededSegments(ctx, &pw, &pa); err != nil {
				return nil, err
			}
		}
		pw = append(pw, "s.pinned = 1")
		pinned, err := s.readListings(ctx, `
                `+head+SessionLastActiveSQL("s")+` AS last_active
                `+fromSessions+`
                `+whereSQL(pw, "")+`
                ORDER BY s.started_at DESC
            `, pa)
		if err != nil {
			return nil, err
		}
		for _, r := range pinned {
			if !seen[r.ID] {
				seen[r.ID] = true
				rows = append(rows, r)
			}
		}
	}
	if !o.KeepCompressionRoots && !o.IncludeChildren {
		if rows, err = s.projectCompressionTips(ctx, rows, o.CompactRows); err != nil {
			return nil, err
		}
	}
	if len(heads) > 0 {
		annotateLocalLineages(rows, heads)
	}
	for i := range rows {
		rows[i].Unread = sessionUnread(rows[i].LastReadAt, rows[i].LastActive, rows[i].StartedAt)
	}
	return rows, nil
}

// Listing rows of one read query
func (s *Store) readListings(ctx context.Context, query string, args []any) ([]SessionListing, error) {
	var out []SessionListing
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = queryListings(c.QueryContext(ctx, query, args...))
		return err
	})
	return out, err
}

// Replaces each compression root's surfaced fields with its live tip's, keeping the root start for stable order
func (s *Store) projectCompressionTips(ctx context.Context, rows []SessionListing, compact bool) ([]SessionListing, error) {
	chains := map[string][]string{}
	var tips []string
	for _, r := range rows {
		if r.EndReason.V != "compression" {
			continue
		}
		chain, err := s.CompressionChain(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		if len(chain) > 0 && chain[len(chain)-1] != r.ID {
			chains[r.ID] = chain
			tips = append(tips, chain[len(chain)-1])
		}
	}
	if len(chains) == 0 {
		return rows, nil
	}
	tipRows, err := s.sessionRichRowsBatch(ctx, tips, compact)
	if err != nil {
		return nil, err
	}
	out := make([]SessionListing, 0, len(rows))
	for _, r := range rows {
		chain := chains[r.ID]
		if chain == nil {
			out = append(out, r)
			continue
		}
		tip, ok := tipRows[chain[len(chain)-1]]
		if !ok {
			out = append(out, r)
			continue
		}
		merged := r
		merged.ID, merged.EndedAt, merged.EndReason = tip.ID, tip.EndedAt, tip.EndReason
		merged.MessageCount, merged.ToolCallCount, merged.Title = tip.MessageCount, tip.ToolCallCount, tip.Title
		merged.LastActive, merged.Preview, merged.Model = tip.LastActive, tip.Preview, tip.Model
		merged.Cwd, merged.GitBranch, merged.GitRepoRoot = tip.Cwd, tip.GitBranch, tip.GitRepoRoot
		if !compact {
			merged.SystemPrompt = tip.SystemPrompt
		}
		if !merged.Title.Valid {
			merged.Title = r.Title
		}
		merged.LineageRootID = r.ID
		merged.LineageIDs = chain
		merged.ContinuationKind = "compression"
		out = append(out, merged)
	}
	return out, nil
}

// Enriched listing rows of many sessions keyed by id, missing ids absent, behind the token barrier
func (s *Store) sessionRichRowsBatch(ctx context.Context, ids []string, compact bool) (map[string]SessionListing, error) {
	var want []string
	for _, id := range ids {
		if id != "" {
			want = append(want, id)
		}
	}
	out := map[string]SessionListing{}
	if len(want) == 0 {
		return out, nil
	}
	s.tokenBarrier(ctx, false)
	cols, prompt, join := "s.*", ", COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved", "LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash"
	if compact {
		var err error
		if cols, err = compactSessionCols(); err != nil {
			return nil, err
		}
		prompt, join = "", ""
	}
	for _, chunk := range chunks(want, sqlInChunk) {
		rows, err := s.readListings(ctx, `
            SELECT `+cols+prompt+`,
                `+previewRawSQL("s.id")+`,
                `+SessionLastActiveSQL("s")+` AS last_active
            FROM sessions s
            `+join+`
            WHERE s.id IN (`+bindMarks(len(chunk))+`)
        `, stringArgs(chunk))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ID] = r
		}
	}
	return out, nil
}

// One session with the listing enrichment, false when absent
func (s *Store) GetSessionRichRow(ctx context.Context, sessionID string, compact bool) (SessionListing, bool, error) {
	rows, err := s.sessionRichRowsBatch(ctx, []string{sessionID}, compact)
	r, ok := rows[sessionID]
	return r, ok, err
}

// Bounds of the latency bounded recent browse
type RecentBrowseOptions struct {
	// Rows to return, at least one
	Limit          int
	ExcludeSources []string
	// Wall clock budget of the query, past which it is interrupted
	Timeout time.Duration
	// Recent rows preselected before lineage resolution, zero meaning the default
	CandidateLimit int
	// Total lineage rows walked, zero meaning the default
	LineageLimit int
}

// Recent browse that ran past its deadline
type BrowseTimeoutError struct {
	Timeout time.Duration
	cause   error
}

// Deadline that passed
func (e *BrowseTimeoutError) Error() string {
	return fmt.Sprintf("recent-session browse exceeded %gs deadline", e.Timeout.Seconds())
}

// Interruption behind the timeout
func (e *BrowseTimeoutError) Unwrap() error { return e.cause }

// Latency bounded recent conversation browse over a small indexed candidate set, deep lineages omitted rather than walked
func (s *Store) ListRecentSessionsBounded(ctx context.Context, o RecentBrowseOptions) ([]SessionListing, error) {
	limit := max(1, o.Limit)
	timeout := max(0, o.Timeout)
	candidate := o.CandidateLimit
	if candidate == 0 {
		candidate = max(128, limit*8)
	}
	candidate = max(limit, min(candidate, 2048))
	lineage := o.LineageLimit
	if lineage == 0 {
		lineage = min(8192, candidate*8)
	}
	lineage = max(candidate, min(lineage, 8192))
	clauses := []string{"s.archived = 0", "s.hidden = 0", delegateFromSQL("s.model_config") + " IS NULL"}
	var args []any
	if len(o.ExcludeSources) > 0 {
		clauses = append(clauses, "s.source NOT IN ("+bindMarks(len(o.ExcludeSources))+")")
		args = append(args, stringArgs(o.ExcludeSources)...)
	}
	edge := `
            parent.end_reason = 'compression'
            AND child.parent_session_id = parent.id
            AND json_extract(
                COALESCE(child.model_config, '{}'), '$._branched_from'
            ) IS NULL
            AND ` + delegateFromSQL("child.model_config") + ` IS NULL
            AND COALESCE(child.source, '') != 'tool'
        `
	query := `
            WITH RECURSIVE
            recent_candidates(id) AS (
                SELECT s.id
                FROM sessions s
                WHERE ` + strings.Join(clauses, " AND ") + `
                ORDER BY COALESCE(s.last_activity_at, s.started_at) DESC,
                         s.started_at DESC, s.id DESC
                LIMIT ?
            ),
            ancestors(candidate_id, cur_id) AS (
                SELECT id, id FROM recent_candidates
                UNION
                SELECT a.candidate_id, parent.id
                FROM ancestors a
                JOIN sessions child ON child.id = a.cur_id
                JOIN sessions parent ON ` + edge + `
                LIMIT ?
            ),
            candidate_roots(root_id) AS (
                SELECT DISTINCT a.cur_id
                FROM ancestors a
                JOIN sessions child ON child.id = a.cur_id
                WHERE NOT EXISTS (
                    SELECT 1
                    FROM sessions parent
                    WHERE ` + edge + `
                )
            ),
            chain(root_id, cur_id) AS (
                SELECT root_id, root_id FROM candidate_roots
                UNION
                SELECT c.root_id, child.id
                FROM chain c
                JOIN sessions parent ON parent.id = c.cur_id
                JOIN sessions child ON ` + edge + `
                LIMIT ?
            ),
            chain_rows AS (
                SELECT
                    c.root_id,
                    c.cur_id,
                    ` + sqlSessionLastActiveByID("c.cur_id") + ` AS activity,
                    CASE WHEN EXISTS (
                        SELECT 1
                        FROM sessions parent
                        JOIN sessions child ON ` + edge + `
                        WHERE parent.id = c.cur_id
                    ) THEN 0 ELSE 1 END AS is_tip
                FROM chain c
            ),
            ranked_tips AS (
                SELECT root_id, cur_id, activity,
                       ROW_NUMBER() OVER (
                           PARTITION BY root_id
                           ORDER BY activity DESC, cur_id DESC
                       ) AS rank_in_root
                FROM chain_rows
                WHERE is_tip = 1
            )
            SELECT
                tip.id,
                tip.source,
                tip.model,
                COALESCE(tip.title, s.title) AS title,
                s.started_at AS started_at,
                tip.ended_at,
                tip.end_reason,
                tip.message_count,
                tip.tool_call_count,
                rt.activity AS last_active,
                ` + previewRawSQL("tip.id") + `,
                CASE WHEN s.id != tip.id THEN s.id ELSE NULL END
                    AS _lineage_root_id
            FROM ranked_tips rt
            JOIN sessions s ON s.id = rt.root_id
            JOIN sessions tip ON tip.id = rt.cur_id
            WHERE rt.rank_in_root = 1
              AND s.archived = 0
              AND s.hidden = 0
              AND ` + listableChildSQL + `
              AND ` + delegateFromSQL("s.model_config") + ` IS NULL
            ORDER BY rt.activity DESC, s.started_at DESC, tip.id DESC
            LIMIT ?
        `
	params := append(args, candidate, lineage, lineage, limit)
	var rows []SessionListing
	deadline := time.Now().Add(timeout)
	err := s.Read(ctx, func(c Conn) error {
		qctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		var err error
		rows, err = queryListings(c.QueryContext(qctx, query, params...))
		if err != nil && ctx.Err() == nil && errors.Is(qctx.Err(), context.DeadlineExceeded) {
			return &BrowseTimeoutError{Timeout: timeout, cause: err}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Unread = sessionUnread(rows[i].LastReadAt, rows[i].LastActive, rows[i].StartedAt)
	}
	return rows, nil
}

// Predicate dropping empty launch stubs, messages alone deciding on a store without the admissions table
func (s *Store) launchStubFilterSQL(ctx context.Context) (string, error) {
	var names []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		names, err = queryStrings(ctx, c, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_admissions'")
		return err
	})
	if err != nil {
		return "", err
	}
	if len(names) > 0 {
		return notALaunchStubSQL, nil
	}
	return "s.message_count > 0", nil
}

// Filters of the most recently active first session search
type SessionSearchOptions struct {
	Sources             []string
	LocalCreatedSources []string
	// Scopes to one workspace so a continue or resume picks its last session
	WorkspaceKey string
	// Drops empty launch stubs before paging
	SkipLaunchStubs bool
	// Rows per page as SQLite reads a LIMIT, zero listing none and a negative value listing all
	Limit  int
	Offset int
}

// Sessions most recently active first with a computed last_active
func (s *Store) SearchSessions(ctx context.Context, o SessionSearchOptions) ([]SessionListing, error) {
	limit := o.Limit
	var where []string
	var args []any
	if o.SkipLaunchStubs {
		stub, err := s.launchStubFilterSQL(ctx)
		if err != nil {
			return nil, err
		}
		where = append(where, stub)
	}
	if len(o.Sources) > 0 {
		clause, bound := sourceMatchClause(o.Sources, o.LocalCreatedSources)
		where, args = append(where, clause), append(args, bound...)
	}
	if o.WorkspaceKey != "" {
		clause, bound := workspaceKeyClause(o.WorkspaceKey)
		where, args = append(where, clause), append(args, bound...)
	}
	rows, err := s.readListings(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		SessionLastActiveSQL("s")+" AS last_active "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
		whereSQL(where, " ")+" "+
		"ORDER BY last_active DESC, s.started_at DESC, s.id DESC LIMIT ? OFFSET ?", append(args, limit, o.Offset))
	return rows, err
}

// Count of sessions under the listing filters, children excluded the way the listing hides them
func (s *Store) SessionCount(ctx context.Context, f SessionFilter, excludeChildren bool) (int64, error) {
	where, args := sessionFilterWhere(excludeChildren, f)
	if excludeChildren {
		if _, err := s.excludeSupersededSegments(ctx, &where, &args); err != nil {
			return 0, err
		}
	}
	var n int64
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions s"+whereSQL(where, " "), args...).Scan(&n)
	})
	return n, err
}

// Reports whether at least n sessions exist, archived ones included
func (s *Store) SessionCountAtLeast(ctx context.Context, n int) (bool, error) {
	var ids []string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		ids, err = queryStrings(ctx, c, "SELECT 1 FROM sessions LIMIT ?", n)
		return err
	})
	return len(ids) >= n, err
}

// Source and its session count, an empty source counting as cli
type SourceCount struct {
	Source string
	Count  int64
}

// Session counts per source in descending count order
func (s *Store) SessionCountBySource(ctx context.Context, includeArchived, archivedOnly, excludeChildren bool) ([]SourceCount, error) {
	where, args := sessionFilterWhere(excludeChildren, SessionFilter{IncludeArchived: includeArchived, ArchivedOnly: archivedOnly})
	if excludeChildren {
		if _, err := s.excludeSupersededSegments(ctx, &where, &args); err != nil {
			return nil, err
		}
	}
	var out []SourceCount
	err := s.Read(ctx, func(c Conn) error {
		out = out[:0]
		rows, err := c.QueryContext(ctx, "SELECT COALESCE(NULLIF(s.source, ''), 'cli') AS source, COUNT(*) AS count "+
			"FROM sessions s"+whereSQL(where, " ")+" "+
			"GROUP BY COALESCE(NULLIF(s.source, ''), 'cli') ORDER BY count DESC", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sc SourceCount
			if err := rows.Scan(&sc.Source, &sc.Count); err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rows.Err()
	})
	return out, err
}

// Lifecycle status of each session from its last message row, empty for a session with none
func (s *Store) SessionLifecycleStatuses(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	var ids []string
	for _, id := range sessionIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		out[id] = SessionStatusEmpty
	}
	err := s.Read(ctx, func(c Conn) error {
		rows, err := c.QueryContext(ctx, `
            SELECT m.session_id, m.role,
                   m.tool_calls IS NOT NULL AS has_tool_calls,
                   m.finish_reason
            FROM messages m
            JOIN (
                SELECT session_id, MAX(id) AS max_id
                FROM messages
                WHERE session_id IN (`+bindMarks(len(ids))+`)
                GROUP BY session_id
            ) latest ON m.id = latest.max_id
        `, stringArgs(ids)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sid string
			var role, finish sql.NullString
			var calls bool
			if err := rows.Scan(&sid, &role, &calls, &finish); err != nil {
				return err
			}
			out[sid] = ClassifySessionStatus(role.String, calls, finish.String)
		}
		return rows.Err()
	})
	return out, err
}

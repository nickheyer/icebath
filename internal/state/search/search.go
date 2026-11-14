package search

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/state"
)

type Field string

const (
	FieldID             Field = "id"
	FieldSessionID      Field = "session_id"
	FieldRole           Field = "role"
	FieldSnippet        Field = "snippet"
	FieldTimestamp      Field = "timestamp"
	FieldToolName       Field = "tool_name"
	FieldSource         Field = "source"
	FieldModel          Field = "model"
	FieldSessionStarted Field = "session_started"
	FieldContext        Field = "context"
)

var resultFields = []Field{FieldID, FieldSessionID, FieldRole, FieldSnippet, FieldTimestamp, FieldToolName, FieldSource,
	FieldModel, FieldSessionStarted, FieldContext}

type Query struct {
	Text            string
	Sources         []string
	ExcludeSources  []string
	Roles           []string
	Limit           int
	Offset          int
	Sort            string
	IncludeInactive bool
	Fields          []Field
	StartedAfter    *int64
	StartedBefore   *int64
}

type ContextMessage struct {
	Role    string
	Content string
}

type Match struct {
	ID             int64
	SessionID      string
	Role           string
	Snippet        sql.Null[string]
	Timestamp      sql.Null[float64]
	ToolName       sql.Null[string]
	Source         sql.Null[string]
	Model          sql.Null[string]
	SessionStarted sql.Null[float64]
	Context        []ContextMessage
}

type IDQuery struct {
	Limit           int
	IncludeArchived bool
	Source          string
	Sources         []string
	ExcludeSources  []string
}

type Searcher struct {
	store   *state.Store
	setting func(ctx context.Context, key string) (any, bool)
}

type MissingCollaboratorError struct {
	Name string
}

func (e *MissingCollaboratorError) Error() string {
	return "session search has no " + e.Name
}

func New(store *state.Store, sessionsSetting func(ctx context.Context, key string) (any, bool)) (*Searcher, error) {
	switch {
	case store == nil:
		return nil, &MissingCollaboratorError{Name: "session store"}
	case sessionsSetting == nil:
		return nil, &MissingCollaboratorError{Name: "sessions settings hook"}
	}
	return &Searcher{store: store, setting: sessionsSetting}, nil
}

const (
	defaultSlowMS     = 1000.0
	slowEnv           = "ICEBATH_SEARCH_SLOW_MS"
	contextBatch      = 500
	contextChars      = 200
	searchSelectTail  = "m.timestamp, m.tool_name, s.source, s.model, s.started_at AS session_started"
	likeSnippetSQL    = "substr(m.content, max(1, instr(m.content, ?) - 40), 120) AS snippet"
	contextWindowHead = `WITH target AS (
    SELECT session_id, timestamp, id FROM messages WHERE id IN (`
	contextWindowTail = `)
)
SELECT t.id AS match_id, m.role, m.content
FROM target t JOIN messages m ON m.id IN (
    t.id,
    (SELECT p.id FROM messages p
     WHERE p.session_id = t.session_id AND (p.timestamp, p.id) < (t.timestamp, t.id)
     ORDER BY p.timestamp DESC, p.id DESC LIMIT 1),
    (SELECT n.id FROM messages n
     WHERE n.session_id = t.session_id AND (n.timestamp, n.id) > (t.timestamp, t.id)
     ORDER BY n.timestamp, n.id LIMIT 1)
)
ORDER BY t.id, m.timestamp, m.id`
)

func (x *Searcher) slowMS(ctx context.Context) float64 {
	var value any
	set := false
	if home.Override(ctx) != "" {
		value, set = x.setting(ctx, "search_slow_ms")
	} else {
		value, set = os.LookupEnv(slowEnv)
	}
	if !set || value == nil {
		return defaultSlowMS
	}
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return defaultSlowMS
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return defaultSlowMS
		}
		return f
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	}
	return defaultSlowMS
}

func validateFields(fields []Field) ([]Field, error) {
	if fields == nil {
		return nil, nil
	}
	var unknown []string
	for _, f := range fields {
		if !slices.Contains(resultFields, f) && !slices.Contains(unknown, string(f)) {
			unknown = append(unknown, string(f))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown search result field(s): %s", strings.Join(unknown, ", "))
	}
	out := []Field{}
	for _, f := range resultFields {
		if slices.Contains(fields, f) {
			out = append(out, f)
		}
	}
	return out, nil
}

type filters struct {
	includeInactive bool
	sources         []string
	excludeSources  []string
	roles           []string
	after, before   *int64
}

func marks(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func strArgs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func (f filters) apply(where []string, args []any) ([]string, []any) {
	if !f.includeInactive {
		where = append(where, "(m.active = 1 OR m.compacted = 1)")
	}
	where = append(where, "COALESCE(m.display_kind, '') <> 'hidden'")
	if f.sources != nil {
		where, args = append(where, "s.source IN ("+marks(len(f.sources))+")"), append(args, strArgs(f.sources)...)
	}
	if f.excludeSources != nil {
		where, args = append(where, "s.source NOT IN ("+marks(len(f.excludeSources))+")"), append(args, strArgs(f.excludeSources)...)
	}
	if len(f.roles) > 0 {
		where, args = append(where, "m.role IN ("+marks(len(f.roles))+")"), append(args, strArgs(f.roles)...)
	}
	if f.after != nil {
		where, args = append(where, "s.started_at >= ?"), append(args, *f.after)
	}
	if f.before != nil {
		where, args = append(where, "s.started_at < ?"), append(args, *f.before)
	}
	return where, args
}

func selectSQL(snippetSQL, fromSQL string, where []string, orderBy, limitSQL string) string {
	return `
            SELECT m.id, m.session_id, m.role,
                   ` + snippetSQL + `,
                   ` + searchSelectTail + `
            FROM ` + fromSQL + `
            JOIN sessions s ON s.id = m.session_id
            WHERE ` + strings.Join(where, " AND ") + `
            ` + orderBy + `
            ` + limitSQL + `
        `
}

type route struct {
	orderBy       string
	limit, offset int
	filters
}

func ftsMatchSQL(table, matchQuery string, r route) (string, []any) {
	where, args := r.filters.apply([]string{table + " MATCH ?"}, []any{matchQuery})
	args = append(args, r.limit, r.offset)
	return selectSQL("snippet("+table+", -1, '>>>', '<<<', '...', 40) AS snippet",
		table+"\n            JOIN messages m ON m.id = "+table+".rowid", where, r.orderBy, "LIMIT ? OFFSET ?"), args
}

func (x *Searcher) rows(ctx context.Context, query string, args []any) ([]Match, error) {
	var out []Match
	err := x.store.Read(ctx, func(c state.Conn) error {
		out = nil
		rows, err := c.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Match
			if err := rows.Scan(state.LenientScan(&m.ID), state.LenientScan(&m.SessionID), state.LenientScan(&m.Role),
				state.LenientScan(&m.Snippet), state.LenientScan(&m.Timestamp), state.LenientScan(&m.ToolName),
				state.LenientScan(&m.Source), state.LenientScan(&m.Model), state.LenientScan(&m.SessionStarted)); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func (x *Searcher) matchRows(ctx context.Context, table, matchQuery string, r route, failOpen, debugMsg string) ([]Match, bool, error) {
	q, args := ftsMatchSQL(table, matchQuery, r)
	out, err := x.rows(ctx, q, args)
	if err == nil {
		return out, true, nil
	}
	if state.IsCallFailure(err) && ctx.Err() == nil {
		if debugMsg != "" {
			slog.Debug(debugMsg, "error", err)
		}
		return nil, false, nil
	}
	if failOpen == "" || !state.IsEngineFailure(err) {
		return nil, false, err
	}
	entered, ferr := x.store.EnterFTSFailOpen(ctx, err)
	if ferr != nil {
		return nil, false, ferr
	}
	if !entered {
		return nil, false, err
	}
	slog.Warn(fmt.Sprintf("%s FTS search hit a corruption error (%s); detached FTS and falling back to canonical LIKE.", failOpen, err))
	return nil, false, nil
}

func (x *Searcher) likeRows(ctx context.Context, where []string, args []any, orderBy, limitSQL string) ([]Match, error) {
	return x.rows(ctx, selectSQL(likeSnippetSQL, "messages m", where, orderBy, limitSQL), args)
}

func (x *Searcher) likeFallback(ctx context.Context, query string, limit, offset int, sortBy string, f filters) ([]Match, error) {
	predicate, args, snippet := compileLikeBooleanQuery(query)
	if predicate == "" || snippet == "" {
		return nil, nil
	}
	where, args := f.apply([]string{"(" + predicate + ")"}, args)
	order := "DESC"
	if strings.ToLower(strings.TrimSpace(sortBy)) == "oldest" {
		order = "ASC"
	}
	return x.likeRows(ctx, where, append(append([]any{snippet}, args...), limit, offset),
		"ORDER BY m.timestamp "+order+", m.id "+order, "LIMIT ? OFFSET ?")
}

func flattenText(c state.Content) (string, error) {
	if text, ok := c.Text(); ok {
		return text, nil
	}
	raw, ok := c.JSON()
	if !ok || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return "", nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var texts []string
	for _, p := range parts {
		var obj map[string]json.RawMessage
		if json.Unmarshal(p, &obj) != nil || obj == nil {
			continue
		}
		var kind string
		if json.Unmarshal(obj["type"], &kind) != nil || kind != "text" {
			continue
		}
		value, present := obj["text"]
		if !present {
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			if jsonTruthy(value) {
				return "", fmt.Errorf("multimodal text part is not a string: %s", value)
			}
			continue
		}
		if text != "" {
			texts = append(texts, text)
		}
	}
	if joined := strings.TrimSpace(strings.Join(texts, " ")); joined != "" {
		return joined, nil
	}
	return "[multimodal content]", nil
}

func jsonTruthy(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	switch {
	case len(t) == 0, string(t) == "null", string(t) == "false", string(t) == `""`, string(t) == "[]", string(t) == "{}":
		return false
	}
	if f, err := strconv.ParseFloat(string(t), 64); err == nil {
		return f != 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(t, &arr) == nil {
		return len(arr) > 0
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(t, &obj) == nil {
		return len(obj) > 0
	}
	return true
}

type contextRow struct {
	matchID int64
	role    string
	content any
}

func (x *Searcher) attachContext(ctx context.Context, batch []Match) error {
	var ids []int64
	for _, m := range batch {
		if !slices.Contains(ids, m.ID) {
			ids = append(ids, m.ID)
		}
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var found []contextRow
	err := x.store.Read(ctx, func(c state.Conn) error {
		found = nil
		rows, err := c.QueryContext(ctx, contextWindowHead+marks(len(ids))+contextWindowTail, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r contextRow
			var role sql.Null[string]
			if err := rows.Scan(state.LenientScan(&r.matchID), state.LenientScan(&role), &r.content); err != nil {
				return err
			}
			r.role = role.V
			found = append(found, r)
		}
		return rows.Err()
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		found = nil
	}
	grouped := map[int64][]contextRow{}
	for _, r := range found {
		grouped[r.matchID] = append(grouped[r.matchID], r)
	}
	for i := range batch {
		window := []ContextMessage{}
		for _, r := range grouped[batch[i].ID] {
			text, err := flattenText(state.DecodeContent(r.content))
			if err != nil {
				window = []ContextMessage{}
				break
			}
			window = append(window, ContextMessage{Role: r.role, Content: truncateRunes(text, contextChars)})
		}
		batch[i].Context = window
	}
	return nil
}

func (x *Searcher) finalize(ctx context.Context, matches []Match, fields []Field) ([]Match, error) {
	if fields == nil || slices.Contains(fields, FieldContext) {
		for start := 0; start < len(matches); start += contextBatch {
			if err := x.attachContext(ctx, matches[start:min(start+contextBatch, len(matches))]); err != nil {
				return nil, err
			}
		}
	}
	if fields == nil {
		return matches, nil
	}
	keep := func(f Field) bool { return slices.Contains(fields, f) }
	for i := range matches {
		m := &matches[i]
		if !keep(FieldID) {
			m.ID = 0
		}
		if !keep(FieldSessionID) {
			m.SessionID = ""
		}
		if !keep(FieldRole) {
			m.Role = ""
		}
		if !keep(FieldSnippet) {
			m.Snippet = sql.Null[string]{}
		}
		if !keep(FieldTimestamp) {
			m.Timestamp = sql.Null[float64]{}
		}
		if !keep(FieldToolName) {
			m.ToolName = sql.Null[string]{}
		}
		if !keep(FieldSource) {
			m.Source = sql.Null[string]{}
		}
		if !keep(FieldModel) {
			m.Model = sql.Null[string]{}
		}
		if !keep(FieldSessionStarted) {
			m.SessionStarted = sql.Null[float64]{}
		}
	}
	return matches, nil
}

func (x *Searcher) describePath(query string) string {
	ft := x.store.FullText()
	if ft.Stale {
		return "like_scan_fts_stale"
	}
	sanitized := sanitizeQuery(query)
	if sanitized == "" {
		return "empty"
	}
	if !containsCJK(sanitized) {
		return "fts5"
	}
	raw := unquoted(sanitized)
	if ft.CJK && !hasLoneCJKRun(raw) {
		return "fts_cjk"
	}
	if x.trigramRouteOK(raw) {
		return "trigram"
	}
	return "like_scan"
}

func (x *Searcher) trigramRouteOK(raw string) bool {
	return countCJK(raw) >= 3 && !hasShortCJKToken(raw) && x.store.FullText().Trigram
}

func (x *Searcher) Messages(ctx context.Context, q Query) ([]Match, error) {
	started := time.Now()
	out, err := x.messages(ctx, q)
	elapsed := float64(time.Since(started).Nanoseconds()) / 1e6
	if elapsed >= x.slowMS(ctx) {
		rows := "err"
		if err == nil {
			rows = strconv.Itoa(len(out))
		}
		slog.Info(fmt.Sprintf("slow session search: path=%s elapsed=%.0fms rows=%s query=%q",
			x.describePath(q.Text), elapsed, rows, truncateRunes(q.Text, 200)))
	}
	return out, err
}

func (x *Searcher) messages(ctx context.Context, q Query) ([]Match, error) {
	fields, err := validateFields(q.Fields)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	query := sanitizeQuery(q.Text)
	if query == "" {
		return nil, nil
	}
	f := filters{includeInactive: q.IncludeInactive, sources: q.Sources, excludeSources: q.ExcludeSources, roles: q.Roles,
		after: q.StartedAfter, before: q.StartedBefore}
	toolRole := slices.Contains(q.Roles, "tool")
	if toolRole {
		matches, err := x.likeFallback(ctx, query, q.Limit, q.Offset, q.Sort, f)
		if err != nil {
			return nil, err
		}
		return x.finalize(ctx, matches, fields)
	}
	if err := x.store.RefreshFTSStale(ctx); err != nil {
		return nil, err
	}
	ft := x.store.FullText()
	if ft.Stale {
		matches, err := x.likeFallback(ctx, query, q.Limit, q.Offset, q.Sort, f)
		if err != nil {
			return nil, err
		}
		return x.finalize(ctx, matches, fields)
	}
	if !ft.Enabled {
		return nil, nil
	}
	orderBy := "ORDER BY rank"
	switch strings.ToLower(strings.TrimSpace(q.Sort)) {
	case "newest":
		orderBy = "ORDER BY m.timestamp DESC, rank"
	case "oldest":
		orderBy = "ORDER BY m.timestamp ASC, rank"
	}
	r := route{orderBy: orderBy, limit: q.Limit, offset: q.Offset, filters: f}
	wantsUnindexed := toolRole
	for _, src := range q.Sources {
		wantsUnindexed = wantsUnindexed || slices.Contains(state.FTSTrigramExcludedSources(), src)
	}
	cjk := containsCJK(query)
	var matches []Match
	if cjk {
		if matches, err = x.searchCJK(ctx, query, wantsUnindexed, r); err != nil {
			return nil, err
		}
	} else {
		sqlText, args := ftsMatchSQL("messages_fts", query, r)
		matches, err = x.rows(ctx, sqlText, args)
		switch {
		case err == nil:
		case state.IsCallFailure(err) && ctx.Err() == nil:
			return nil, nil
		case !state.IsEngineFailure(err):
			return nil, err
		default:
			entered, ferr := x.store.EnterFTSFailOpen(ctx, err)
			if ferr != nil {
				return nil, ferr
			}
			if !entered {
				return nil, err
			}
			if matches, err = x.likeFallback(ctx, query, q.Limit, q.Offset, q.Sort, f); err != nil {
				return nil, err
			}
		}
	}
	_, pending, err := x.store.FTSRebuildStatus(ctx)
	if err != nil {
		return nil, err
	}
	if pending && len(matches) < q.Limit {
		gap, err := x.unindexedGap(ctx, query, q.Limit-len(matches), f)
		switch {
		case err == nil:
			seen := map[int64]bool{}
			for _, m := range matches {
				seen[m.ID] = true
			}
			for _, m := range gap {
				if !seen[m.ID] {
					matches = append(matches, m)
				}
			}
		case state.IsCallFailure(err) && ctx.Err() == nil:
			slog.Debug(fmt.Sprintf("Unindexed-gap supplement skipped: %s", err))
		default:
			return nil, err
		}
	}
	if len(matches) == 0 && !cjk && !toolRole {
		fallback := quoteFTSTokens(unquoted(query))
		if x.store.FullText().CJK {
			found, _, err := x.matchRows(ctx, "messages_fts_cjk", fallback, r, "", "")
			if err != nil {
				return nil, err
			}
			if len(found) > 0 {
				matches = found
			}
		}
		if len(matches) == 0 && x.store.FullText().Trigram && trigramEligibleTokens(query) {
			found, _, err := x.matchRows(ctx, "messages_fts_trigram", fallback, r, "", "")
			if err != nil {
				return nil, err
			}
			if len(found) > 0 {
				matches = found
			}
		}
	}
	if len(matches) == 0 && !cjk && !x.store.FullText().Stale {
		if relaxed, ok := orRelaxedQuery(query); ok {
			found, _, err := x.matchRows(ctx, "messages_fts", relaxed, r, "OR-relaxed", "OR-relaxed FTS retry failed; keeping empty result")
			if err != nil {
				return nil, err
			}
			if len(found) > 0 {
				matches = found
			}
		}
	}
	return x.finalize(ctx, matches, fields)
}

func (x *Searcher) searchCJK(ctx context.Context, query string, wantsUnindexed bool, r route) ([]Match, error) {
	raw := unquoted(stripCJKWildcards(query))
	matchQuery := quoteFTSTokens(raw)
	if x.store.FullText().CJK && !wantsUnindexed && !hasLoneCJKRun(raw) {
		matches, ok, err := x.matchRows(ctx, "messages_fts_cjk", matchQuery, r, "CJK-bigram",
			"messages_fts_cjk query failed; falling back to trigram/LIKE")
		if err != nil || ok {
			return matches, err
		}
	}
	if x.trigramRouteOK(raw) && !wantsUnindexed {
		matches, ok, err := x.matchRows(ctx, "messages_fts_trigram", matchQuery, r, "Trigram", "")
		if err != nil || ok {
			return matches, err
		}
	}
	tokens := nonOperatorTokens(raw)
	if len(tokens) == 0 {
		tokens = []string{raw}
	}
	var args []any
	clauses := make([]string, len(tokens))
	for i, t := range tokens {
		clauses[i] = likeAnyColumnSQL
		args = append(args, likeParams(t)...)
	}
	where, args := r.filters.apply([]string{"(" + strings.Join(clauses, " OR ") + ")"}, args)
	return x.likeRows(ctx, where, append(append([]any{tokens[0]}, args...), r.limit, r.offset), "ORDER BY m.timestamp DESC", "LIMIT ? OFFSET ?")
}

func (x *Searcher) unindexedGap(ctx context.Context, ftsQuery string, limit int, f filters) ([]Match, error) {
	status, pending, err := x.store.FTSRebuildStatus(ctx)
	if err != nil || !pending || limit <= 0 {
		return nil, err
	}
	terms := gapTerms(ftsQuery)
	if len(terms) == 0 {
		return nil, nil
	}
	where := []string{"m.id > ? AND m.id <= ?"}
	args := []any{status.Indexed, status.Total}
	for _, t := range terms {
		where = append(where, likeAnyColumnSQL)
		args = append(args, likeParams(t)...)
	}
	where, args = f.apply(where, args)
	return x.likeRows(ctx, where, append(append([]any{terms[0]}, args...), limit), "ORDER BY m.timestamp DESC", "LIMIT ?")
}

func (x *Searcher) SessionsByID(ctx context.Context, text string, q IDQuery) ([]state.SessionListing, error) {
	needle := strings.ToLower(strings.TrimSpace(text))
	if needle == "" || q.Limit <= 0 {
		return nil, nil
	}
	candidates, err := x.store.ListSessionsRich(ctx, state.SessionListOptions{
		SessionFilter: state.SessionFilter{Source: q.Source, Sources: q.Sources, ExcludeSources: q.ExcludeSources,
			IncludeArchived: q.IncludeArchived},
		Limit: max(q.Limit*4, q.Limit), OrderByLastActive: true, IDQuery: needle,
	})
	if err != nil {
		return nil, err
	}
	score := func(row state.SessionListing) int {
		best := 2
		for _, v := range []string{row.ID, row.LineageRootID} {
			if v == "" {
				continue
			}
			v = strings.ToLower(v)
			if v == needle {
				return 0
			}
			if strings.HasPrefix(v, needle) {
				best = 1
			}
		}
		return best
	}
	sort.SliceStable(candidates, func(i, j int) bool { return score(candidates[i]) < score(candidates[j]) })
	return candidates[:min(q.Limit, len(candidates))], nil
}

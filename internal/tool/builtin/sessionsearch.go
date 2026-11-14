package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/state/search"
	"github.com/nickheyer/icebath/internal/tool"
)

var hiddenSessionSources = []string{"kanban", "subagent", "tool"}

const (
	readMaxContent         = 2000
	discoverScanLimit      = 300
	excludeSessionIDsCap   = 20
	sessionDBUnavailable   = "icebath can't open its session history right now"
	discoverLinkHint       = "When referring the user to a session, write its `link` value verbatim inline mid-sentence (it renders as a titled link) — never as markdown, in backticks, on its own line, or next to the title/id/date. To read more around a compact result, scroll: session_search(session_id=..., around_message_id=match_message_id)."
	discoverNoMatchMessage = "No matching sessions found. FTS5 ANDs all terms by default — broaden with OR (`alpha OR beta`), exact-match with quoted phrases, exclude with NOT, or prefix-match with `deploy*`."
	scrollHint             = "Scroll forward: re-call with around_message_id = the LAST message's id; backward: the FIRST message's id (the boundary message repeats as an orientation marker). messages_before/messages_after < window means you've hit that end of the session."
)

const sessionSearchDescription = "Search or read past icebath conversations (FTS5), or scroll inside one. Four shapes, picked by args: `query` = discovery " +
	"(top-N matching sessions, top result fully hydrated); `session_id` + " +
	"`around_message_id` = scroll (window of messages around an anchor); " +
	"`session_id` alone = read a whole session — how you resolve an " +
	"`@session:<profile>/<id>` link (split on '/' into profile + id); no " +
	"args = browse recent sessions. Results are actual DB messages, no LLM. " +
	"Searches conversation history ONLY — when the user gave a direct " +
	"source (URL, file, contact, live system), inspect that first; never " +
	"conclude 'not found' from history alone. Use for questions about past " +
	"conversations: 'what did we do about X', 'where did we leave Y'. When " +
	"referring the user to a session, write its `link` value verbatim " +
	"inline (it renders as a titled link)."

const sessionSearchParameters = `{"type": "object", "properties": {` +
	`"query": {"type": "string", "description": "Search query (discovery shape). Keywords, phrases, or boolean expressions to find in past sessions. Omit to browse recent sessions. Ignored when session_id + around_message_id are set (scroll shape)."}, ` +
	`"limit": {"type": "integer", "description": "Max sessions to return. Discovery defaults to 3; browse/no-args defaults to 10. Max 10. Bump discovery to 5–10 when the topic likely spans several sessions and you want to pick the right one to scroll into."}, ` +
	`"sort": {"type": "string", "enum": ["newest", "oldest"], "description": "Discovery shape only. Temporal bias on top of FTS5 ranking: omit for relevance-only (exploratory recall), 'newest' for \"where did we leave X\", 'oldest' for \"how did X start\"."}, ` +
	`"detail": {"type": "string", "enum": ["adaptive", "full"], "description": "Discovery shape only. 'adaptive' (default) fully hydrates the top-ranked result and returns only the exact anchor message for lower-ranked results. 'full' returns bookends and the complete anchored window for every result.", "default": "adaptive"}, ` +
	`"after": {"type": "string", "description": "Discovery shape only. Inclusive lower bound on session start time. ISO date/datetime (e.g. 2026-06-01) or relative duration (7d, 24h, 2w = within the last N). Use only when the user names a time frame. sort is a ranking bias, not a bound."}, ` +
	`"before": {"type": "string", "description": "Discovery shape only. Exclusive upper bound on session start time. ISO date/datetime (a date-only value is midnight UTC that day) or relative duration (7d = older than a week). Use only when the user names a time frame."}, ` +
	`"exclude_session_ids": {"type": "array", "items": {"type": "string"}, "description": "Discovery shape only. Session ids already inspected this task. Those sessions and their lineage are omitted so a later query explores instead of repeating the same hit. Cap 20."}, ` +
	`"session_id": {"type": "string", "description": "Scroll shape. Session to read inside. Use the session_id returned from a prior discovery call. Must be paired with around_message_id."}, ` +
	`"around_message_id": {"type": "integer", "description": "Scroll shape. Message id to center the window on — use match_message_id from a discovery result, or any id from a prior window."}, ` +
	`"window": {"type": "integer", "description": "Scroll shape only. Messages to return on each side of the anchor (anchor itself always included). Clamped to [1, 20]. Default 5.", "default": 5}, ` +
	`"role_filter": {"type": "string", "description": "Optional. Comma-separated roles to include. Discovery defaults to 'user,assistant' (tool output is usually noise). Pass 'user,assistant,tool' to include tool output (debugging tool behaviour) or 'tool' to search tool output only."}, ` +
	"\"profile\": {\"type\": \"string\", \"description\": \"Optional. Read sessions from another icebath profile's database (read-only). Use when resolving an `@session:<profile>/<id>` link: pass the profile segment here with session_id as the id segment. Omit to use the current profile.\"}" +
	`}, "required": []}`

type Profiles interface {
	Home(ctx context.Context, name string) (string, error)
	Active(ctx context.Context) (string, error)
}

type SessionSearchOptions struct {
	Store    *state.Store
	Engine   *state.Engine
	Database state.Database
	Homes    *home.Resolver
	Setting  func(ctx context.Context, key string) (any, bool)
	Profiles Profiles
}

type sessionSearch struct {
	o SessionSearchOptions
}

type searchDB struct {
	store    *state.Store
	searcher *search.Searcher
}

func SessionSearch(o SessionSearchOptions) (tool.Tool, error) {
	switch {
	case o.Engine == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "session store engine"}
	case o.Homes == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "home resolver"}
	case o.Setting == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "session search settings hook"}
	case o.Profiles == nil:
		return tool.Tool{}, &tool.UnfilledError{Name: "profile directory"}
	}
	s := &sessionSearch{o: o}
	return tool.Tool{
		Name:        "session_search",
		Toolset:     "session_search",
		Description: sessionSearchDescription,
		Parameters:  json.RawMessage(sessionSearchParameters),
		Handler:     s.handle,
		Check:       &tool.Check{Name: "session_search", Probe: s.available},
		Emoji:       "🔍",
		Concurrency: tool.ParallelSafe,
		Effects:     tool.NoEffect | tool.Idempotent,
	}, nil
}

func (s *sessionSearch) available(ctx context.Context) (bool, error) {
	dir, err := s.o.Homes.Home(ctx)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(dir)
	return err == nil, nil
}

func failure(msg string) tool.Result {
	return tool.Failure(msg, tool.Field{Key: "success", Value: false})
}

func (s *sessionSearch) open(ctx context.Context, dir string) (searchDB, error) {
	st, err := state.Open(ctx, s.o.Engine, state.Options{Path: home.StateDB.In(dir), ReadOnly: true, Database: s.o.Database})
	if err != nil {
		return searchDB{}, err
	}
	searcher, err := search.New(st, s.o.Setting)
	if err != nil {
		return searchDB{}, errors.Join(err, st.Close(context.WithoutCancel(ctx)))
	}
	return searchDB{store: st, searcher: searcher}, nil
}

func (s *sessionSearch) handle(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	query := ""
	if q := args["query"]; truthy(q) {
		query = pyStr(q)
	}
	_, queryIsString := args["query"].(string)
	window, present := args["window"]
	if !present {
		window = json.Number("5")
	}
	detail, present := args["detail"]
	if !present {
		detail = "adaptive"
	}
	sessionID := args["session_id"]
	profile := args["profile"]
	if sid, ok := sessionID.(string); ok && strings.Contains(sid, "/") {
		embProfile, embID, _ := strings.Cut(sid, "/")
		if embID != "" {
			sessionID = embID
			if embProfile != "" && (profile == nil || strip(pyStr(profile)) == "") {
				profile = embProfile
			}
		}
	}
	current := c.SessionID
	var owned []searchDB
	defer func() {
		for i := len(owned) - 1; i >= 0; i-- {
			if err := owned[i].store.Close(context.WithoutCancel(ctx)); err != nil {
				slog.DebugContext(ctx, "Failed to close session_search SessionDB", "error", err)
			}
		}
	}()
	var db searchDB
	if profile != nil && strip(pyStr(profile)) != "" {
		dir, err := s.o.Profiles.Home(ctx, pyStr(profile))
		if err == nil {
			db, err = s.open(ctx, dir)
		}
		if err != nil {
			slog.DebugContext(ctx, "session_search: cannot open profile", "profile", pyStr(profile), "error", err)
			return failure(fmt.Sprintf("profile '%s': %v", pyStr(profile), err)), nil
		}
		owned = append(owned, db)
		current = ""
	} else if s.o.Store != nil {
		searcher, err := search.New(s.o.Store, s.o.Setting)
		if err != nil {
			return tool.Result{}, err
		}
		db = searchDB{store: s.o.Store, searcher: searcher}
	} else {
		dir := c.Origin.ProfileHome
		var err error
		if dir == "" {
			dir, err = s.o.Homes.Home(ctx)
		}
		if err == nil {
			db, err = s.open(ctx, dir)
		}
		if err != nil {
			slog.DebugContext(ctx, "SessionDB unavailable for session_search", "error", err)
			return failure(s.o.Engine.UnavailableMessage(ctx, sessionDBUnavailable, false)), nil
		}
		owned = append(owned, db)
	}
	r := &searchRun{s: s, db: db, current: current, profile: profile}
	if sid, ok := sessionID.(string); ok && strip(sid) != "" {
		if around, present := args["around_message_id"]; present && around != nil {
			return r.scroll(ctx, strip(sid), around, window)
		}
		return r.readScoped(ctx, strip(sid))
	}
	browse := !queryIsString || strip(query) == ""
	defLimit := int64(3)
	if browse {
		defLimit = 10
	}
	limit := clampInt(args["limit"], defLimit, 1, 10)
	if browse {
		return r.browse(ctx, int(limit))
	}
	sortNorm := ""
	if v, ok := args["sort"].(string); ok {
		if v = strings.ToLower(strip(v)); v == "newest" || v == "oldest" {
			sortNorm = v
		}
	}
	now := time.Now()
	after, err := parseBound(args["after"], now)
	if err != nil {
		return failure(err.Error()), nil
	}
	before, err := parseBound(args["before"], now)
	if err != nil {
		return failure(err.Error()), nil
	}
	var roles []string
	if v, ok := args["role_filter"].(string); ok {
		for _, part := range strings.Split(v, ",") {
			if part = strip(part); part != "" {
				roles = append(roles, part)
			}
		}
	}
	mode := "adaptive"
	if v, ok := detail.(string); ok && strings.ToLower(strip(v)) == "full" {
		mode = "full"
	}
	return r.discover(ctx, strip(query), roles, int(limit), sortNorm, mode, after, before, excludeIDs(args["exclude_session_ids"]))
}

func clampInt(v any, def, lo, hi int64) int64 {
	n := def
	if v != nil {
		if parsed, ok := pyInt(v); ok {
			n = parsed
		}
	}
	return max(lo, min(n, hi))
}

func excludeIDs(raw any) []string {
	var items []any
	switch t := raw.(type) {
	case string:
		items = []any{t}
	case []any:
		items = t
	}
	var out []string
	for _, item := range items {
		sid := ""
		if s, ok := item.(string); ok {
			sid = strip(s)
		}
		if sid != "" && !contains(out, sid) {
			out = append(out, sid)
		}
		if len(out) >= excludeSessionIDsCap {
			break
		}
	}
	return out
}

type searchRun struct {
	s       *sessionSearch
	db      searchDB
	current string
	profile any
}

func ok(o *object) (tool.Result, error) {
	raw, err := o.json()
	if err != nil {
		return tool.Result{}, err
	}
	return tool.JSON(raw), nil
}

func okObject(pairs ...any) *object {
	o := &object{}
	o.set("success", true)
	for i := 0; i+1 < len(pairs); i += 2 {
		o.set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func (r *searchRun) meta(ctx context.Context, sid string) (state.Session, bool) {
	if sid == "" {
		return state.Session{}, false
	}
	sess, found, err := r.db.store.GetSession(ctx, sid)
	if err != nil {
		slog.DebugContext(ctx, "get_session failed", "session", sid, "error", err)
		return state.Session{}, false
	}
	return sess, found
}

func (r *searchRun) resolveToParent(ctx context.Context, sid string) (string, bool) {
	visited := map[string]bool{}
	cur, compression := sid, false
	for cur != "" && !visited[cur] {
		visited[cur] = true
		sess, _ := r.meta(ctx, cur)
		compression = compression || sess.EndReason.Valid && sess.EndReason.V == "compression"
		if !sess.ParentSessionID.Valid || sess.ParentSessionID.V == "" {
			break
		}
		cur = sess.ParentSessionID.V
	}
	return cur, compression
}

func (r *searchRun) lineage(ctx context.Context, sid string) string {
	root, _ := r.resolveToParent(ctx, sid)
	return root
}

func (r *searchRun) sameLineage(ctx context.Context, a, b string) bool {
	root := r.lineage(ctx, a)
	return root != "" && root == r.lineage(ctx, b)
}

func (r *searchRun) leftLiveContext(ctx context.Context, sid string) bool {
	sess, found := r.meta(ctx, sid)
	if !found || !sess.EndReason.Valid || sess.EndReason.V == "" {
		return false
	}
	return sess.EndReason.V == "compression" || state.IsBoundaryEndReason(sess.EndReason.V)
}

type storageState struct {
	sessionID string
	active    int64
	compacted int64
}

func (r *searchRun) storageState(ctx context.Context, messageID int64) (*storageState, bool) {
	if messageID == 0 {
		return nil, false
	}
	var st storageState
	err := r.db.store.Read(ctx, func(c state.Conn) error {
		return c.QueryRowContext(ctx, "SELECT session_id, active, compacted FROM messages WHERE id = ?", messageID).
			Scan(&st.sessionID, &st.active, &st.compacted)
	})
	if err != nil {
		slog.DebugContext(ctx, "message storage-state lookup failed", "message", messageID, "error", err)
		return nil, false
	}
	return &st, true
}

func compactedState(st *storageState) bool {
	return st != nil && st.active == 0 && st.compacted == 1
}

func formatTimestamp(sess state.Session) string {
	if sess.StartedAt.Valid {
		return formatEpoch(sess.StartedAt.V)
	}
	return "unknown"
}

func formatEpoch(v float64) string {
	sec := int64(v)
	nsec := int64((v - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).Local().Format("January 02, 2006 at 03:04 PM")
}

func metaBlock(sess state.Session) *object {
	o := &object{}
	o.set("when", formatTimestamp(sess))
	o.set("source", sess.Source)
	o.set("model", nullable(sess.Model.V, sess.Model.Valid))
	o.set("title", nullable(sess.Title.V, sess.Title.Valid))
	return o
}

func nullable(v string, valid bool) any {
	if !valid {
		return nil
	}
	return v
}

func compactionSummary(content string) bool {
	trimmed := strings.TrimLeftFunc(content, isPySpace)
	return content != "" && (strings.HasPrefix(trimmed, "[CONTEXT COMPACTION") || strings.HasPrefix(trimmed, "[CONTEXT SUMMARY]:"))
}

func toolCallsPresent(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	switch t {
	case "", "[]", "{}", "null", `""`, "0", "false":
		return false
	}
	return true
}

func shapeMessage(m state.MessageRow, anchor int64, hasAnchor bool, maxLen int) *object {
	o := &object{}
	o.set("id", m.ID)
	o.set("role", m.Role)
	text, isText := m.Content.Text()
	switch {
	case isText:
		if strings.Contains(text, "\x1b") {
			text = tool.StripANSI(text)
		}
		o.set("content", text)
	default:
		if raw, isJSON := m.Content.JSON(); isJSON {
			o.set("content", raw)
		} else {
			o.set("content", nil)
		}
	}
	if m.Timestamp.Valid {
		o.set("timestamp", m.Timestamp.V)
	}
	if m.ToolName.Valid && m.ToolName.V != "" {
		o.set("tool_name", m.ToolName.V)
	}
	if toolCallsPresent(m.ToolCalls) {
		o.set("tool_calls", json.RawMessage(m.ToolCalls))
	}
	if m.ToolCallID.Valid && m.ToolCallID.V != "" {
		o.set("tool_call_id", m.ToolCallID.V)
	}
	if hasAnchor && m.ID == anchor {
		o.set("anchor", true)
	}
	if isText && maxLen > 0 && text != "" {
		if runes := []rune(text); len(runes) > maxLen {
			o.set("content", string(runes[:maxLen])+"…")
			o.set("content_truncated", true)
			o.set("original_content_chars", len(runes))
		}
	}
	return o
}

func contentText(m state.MessageRow) string {
	text, _ := m.Content.Text()
	return text
}

func (r *searchRun) link(ctx context.Context, sid string) string {
	name := ""
	if r.profile != nil {
		name = strip(pyStr(r.profile))
	}
	if name == "" {
		active, err := r.s.o.Profiles.Active(ctx)
		if err != nil {
			slog.DebugContext(ctx, "get_active_profile_name failed for session link", "error", err)
		} else if active != "custom" {
			name = active
		}
	}
	if name != "" {
		return "@session:" + name + "/" + sid
	}
	return "@session:" + sid
}

type discovery struct {
	obj      *object
	session  string
	lineage  string
	titleHit bool
}

func discoveryEntry(lineageRoot string, sid string, fields ...any) *object {
	o := &object{}
	o.set("session_id", sid)
	for i := 0; i+1 < len(fields); i += 2 {
		o.set(fields[i].(string), fields[i+1])
	}
	if lineageRoot != "" && lineageRoot != sid {
		o.set("parent_session_id", lineageRoot)
	}
	return o
}

func shapeAll(rows []state.MessageRow, anchor int64, hasAnchor bool, maxLen int) []*object {
	out := []*object{}
	for _, m := range rows {
		out = append(out, shapeMessage(m, anchor, hasAnchor, maxLen))
	}
	return out
}

func (r *searchRun) titleMatch(ctx context.Context, query, currentRoot string) (*discovery, bool) {
	titleQuery := strings.Trim(strip(query), "`'\"")
	if titleQuery == "" {
		return nil, false
	}
	sid, found, err := r.db.store.ResolveSessionByTitle(ctx, titleQuery)
	if err != nil {
		slog.DebugContext(ctx, "resolve_session_by_title failed", "title", titleQuery, "error", err)
		return nil, false
	}
	if !found || sid == "" {
		return nil, false
	}
	root := r.lineage(ctx, sid)
	if currentRoot != "" && root == currentRoot && !r.leftLiveContext(ctx, sid) {
		return nil, false
	}
	sess, ok := r.meta(ctx, root)
	if !ok {
		sess, _ = r.meta(ctx, sid)
	}
	if sess.ID != "" && contains(hiddenSessionSources, sess.Source) {
		return nil, false
	}
	messages, err := r.db.store.GetMessages(ctx, sid, state.MessageQuery{})
	if err != nil {
		slog.DebugContext(ctx, "get_messages failed for title match", "session", sid, "error", err)
		messages = nil
	}
	var anchor int64
	hasAnchor := len(messages) > 0
	var view state.AnchoredView
	viewOK := false
	if hasAnchor {
		anchor = messages[0].ID
		view, err = r.db.store.GetAnchoredView(ctx, sid, anchor, 5, 3, []string{"user", "assistant"})
		if err != nil {
			slog.DebugContext(ctx, "get_anchored_view failed for title match", "session", sid, "anchor", anchor, "error", err)
		} else {
			viewOK = true
		}
	}
	title := titleQuery
	if sess.Title.Valid && sess.Title.V != "" {
		title = sess.Title.V
	}
	pick := func(rows, fallback []state.MessageRow) []state.MessageRow {
		if viewOK && len(rows) > 0 {
			return rows
		}
		return fallback
	}
	source := any("unknown")
	if sess.ID != "" {
		source = sess.Source
	}
	model := "unknown"
	if sess.Model.Valid && sess.Model.V != "" {
		model = sess.Model.V
	}
	var matchID any
	if hasAnchor {
		matchID = anchor
	}
	before, after := 0, max(len(messages)-5, 0)
	if viewOK {
		before, after = view.MessagesBefore, view.MessagesAfter
	}
	o := discoveryEntry(root, sid,
		"when", formatTimestamp(sess), "source", source, "model", model, "title", title,
		"matched_role", "session_title", "match_message_id", matchID, "snippet", "Session title matched: "+title,
		"bookend_start", shapeAll(pick(view.BookendStart, head(messages, 3)), 0, false, 1200),
		"messages", shapeAll(pick(view.Window, head(messages, 5)), anchor, hasAnchor, 4000),
		"bookend_end", shapeAll(pick(view.BookendEnd, tail(messages, 3)), 0, false, 1200),
		"messages_before", before, "messages_after", after, "detail", "full")
	return &discovery{obj: o, session: sid, lineage: root, titleHit: true}, true
}

func head(rows []state.MessageRow, n int) []state.MessageRow {
	if len(rows) > n {
		return rows[:n]
	}
	return rows
}

func tail(rows []state.MessageRow, n int) []state.MessageRow {
	if len(rows) > n {
		return rows[len(rows)-n:]
	}
	return rows
}

func startedTS(sess state.Session) *int64 {
	if !sess.StartedAt.Valid {
		return nil
	}
	v := int64(sess.StartedAt.V)
	return &v
}

func inWindow(started, after, before *int64) bool {
	if after == nil && before == nil {
		return true
	}
	if started == nil {
		return false
	}
	return (after == nil || *started >= *after) && (before == nil || *started < *before)
}

func (r *searchRun) payload(ctx context.Context, query, detail string, results []*object, extra ...any) (tool.Result, error) {
	o := okObject("mode", "discover", "query", query, "detail", detail, "results", results, "count", len(results))
	for i := 0; i+1 < len(extra); i += 2 {
		o.set(extra[i].(string), extra[i+1])
	}
	status, pending, err := r.db.store.FTSRebuildStatus(ctx)
	if err != nil {
		slog.DebugContext(ctx, "fts_rebuild_status failed", "error", err)
	} else if pending {
		note := fmt.Sprintf("The search index is rebuilding in the background (%d%% done, %s of %s messages). "+
			"Results from older messages may be incomplete until it finishes.", status.Percent, grouped(status.Indexed), grouped(status.Total))
		rebuild := &object{}
		rebuild.set("percent", status.Percent)
		rebuild.set("note", note)
		o.set("index_rebuild", rebuild)
	}
	return ok(o)
}

func grouped(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func (r *searchRun) bookend(rows []state.MessageRow) []*object {
	out := []*object{}
	for _, m := range rows {
		if !compactionSummary(contentText(m)) {
			out = append(out, shapeMessage(m, 0, false, 1200))
		}
	}
	return out
}

func (r *searchRun) hydrate(ctx context.Context, root string, m search.Match, detail string) (*object, bool) {
	sid := m.SessionID
	if sid == "" {
		sid = root
	}
	view, err := r.db.store.GetAnchoredView(ctx, sid, m.ID, 5, 3, []string{"user", "assistant"})
	if err != nil {
		slog.WarnContext(ctx, "get_anchored_view failed", "session", sid, "message", m.ID, "error", err)
		return nil, false
	}
	sess, _ := r.meta(ctx, root)
	full := detail == "full"
	var when string
	switch {
	case sess.StartedAt.Valid && sess.StartedAt.V != 0:
		when = formatEpoch(sess.StartedAt.V)
	case m.SessionStarted.Valid:
		when = formatEpoch(m.SessionStarted.V)
	default:
		when = "unknown"
	}
	var source any
	switch {
	case sess.Source != "":
		source = sess.Source
	case m.Source.Valid && m.Source.V != "":
		source = m.Source.V
	case m.Source.Valid:
		source = m.Source.V
	}
	model := "unknown"
	switch {
	case sess.Model.Valid && sess.Model.V != "":
		model = sess.Model.V
	case m.Model.Valid && m.Model.V != "":
		model = m.Model.V
	}
	var title any
	if sess.Title.Valid && sess.Title.V != "" {
		title = sess.Title.V
	}
	snippet := ""
	if m.Snippet.Valid {
		snippet = m.Snippet.V
	}
	var start, end []*object
	if full {
		start, end = r.bookend(view.BookendStart), r.bookend(view.BookendEnd)
	} else {
		start, end = []*object{}, []*object{}
	}
	window := []*object{}
	for _, row := range view.Window {
		if full || row.ID == m.ID {
			window = append(window, shapeMessage(row, m.ID, true, 4000))
		}
	}
	return discoveryEntry(root, sid, "when", when, "source", source, "model", model, "title", title,
		"matched_role", m.Role, "match_message_id", m.ID, "snippet", snippet, "bookend_start", start,
		"messages", window, "bookend_end", end, "messages_before", view.MessagesBefore,
		"messages_after", view.MessagesAfter, "detail", detail), true
}

func (r *searchRun) discover(ctx context.Context, query string, roles []string, limit int, sortBy, detail string,
	after, before *int64, exclude []string) (tool.Result, error) {
	currentRoot := ""
	if r.current != "" {
		currentRoot = r.lineage(ctx, r.current)
	}
	excluded := map[string]bool{}
	for _, sid := range exclude {
		excluded[sid] = true
		root := r.lineage(ctx, sid)
		if root == "" {
			root = sid
		}
		excluded[root] = true
	}
	title, hasTitle := r.titleMatch(ctx, query, currentRoot)
	if hasTitle {
		root := title.lineage
		if root == "" {
			root = title.session
		}
		sess, found := r.meta(ctx, root)
		if !found {
			sess, _ = r.meta(ctx, title.session)
		}
		if excluded[title.session] || excluded[root] || !inWindow(startedTS(sess), after, before) {
			title, hasTitle = nil, false
		}
	}
	if len(roles) == 0 {
		roles = []string{"user", "assistant"}
	}
	rows, err := r.db.searcher.Messages(ctx, search.Query{Text: query, Roles: roles, ExcludeSources: hiddenSessionSources,
		Limit: discoverScanLimit, Sort: sortBy, StartedAfter: after, StartedBefore: before,
		Fields: []search.Field{search.FieldID, search.FieldSessionID, search.FieldRole, search.FieldSnippet,
			search.FieldSource, search.FieldModel, search.FieldSessionStarted}})
	if err != nil {
		slog.ErrorContext(ctx, "FTS5 search failed", "error", err)
		return failure("Search failed: " + err.Error()), nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return !(rows[i].Source.Valid && rows[i].Source.V == "cron") && rows[j].Source.Valid && rows[j].Source.V == "cron"
	})
	if len(rows) == 0 && !hasTitle {
		return r.payload(ctx, query, detail, []*object{}, "message", discoverNoMatchMessage)
	}
	type seenEntry struct {
		root      string
		match     search.Match
		titleOnly bool
	}
	var seen []seenEntry
	seenAt := map[string]bool{}
	var results []*object
	if hasTitle {
		results = append(results, title.obj)
		if title.lineage != "" {
			seen = append(seen, seenEntry{root: title.lineage, titleOnly: true})
			seenAt[title.lineage] = true
		}
	}
	for _, m := range rows {
		if len(seen) >= limit {
			break
		}
		raw, resolved := m.SessionID, r.lineage(ctx, m.SessionID)
		if excluded[raw] || excluded[resolved] {
			continue
		}
		st, _ := r.storageState(ctx, m.ID)
		compactedHit := compactedState(st)
		if currentRoot != "" && resolved == currentRoot && !(r.leftLiveContext(ctx, raw) || compactedHit) {
			continue
		}
		if r.current != "" && raw == r.current && !compactedHit {
			continue
		}
		if !seenAt[resolved] {
			seenAt[resolved] = true
			seen = append(seen, seenEntry{root: resolved, match: m})
		}
	}
	for _, e := range seen {
		if e.titleOnly {
			continue
		}
		mode := "compact"
		if detail == "full" || len(results) == 0 {
			mode = "full"
		}
		if entry, ok := r.hydrate(ctx, e.root, e.match, mode); ok {
			results = append(results, entry)
		}
	}
	for _, entry := range results {
		raw, _ := entry.get("session_id")
		var sid string
		if err := json.Unmarshal(raw, &sid); err != nil {
			return tool.Result{}, fmt.Errorf("discovery entry session id: %w", err)
		}
		entry.set("link", r.link(ctx, sid))
	}
	if results == nil {
		results = []*object{}
	}
	return r.payload(ctx, query, detail, results, "sessions_searched", len(seen), "link_hint", discoverLinkHint)
}

func (r *searchRun) readSession(ctx context.Context, sid string) (tool.Result, bool, error) {
	sess, found := r.meta(ctx, sid)
	if !found {
		return failure("session_id not found: " + sid), false, nil
	}
	rows, err := r.db.store.GetMessages(ctx, sid, state.MessageQuery{})
	if err != nil {
		slog.ErrorContext(ctx, "get_messages failed", "session", sid, "error", err)
		return failure("failed to load session: " + err.Error()), false, nil
	}
	shaped := shapeAll(rows, 0, false, readMaxContent)
	total := len(shaped)
	const headN, tailN = 20, 10
	truncated := total > headN+tailN
	messages := shaped
	if truncated {
		messages = append(append([]*object{}, shaped[:headN]...), shaped[total-tailN:]...)
	}
	o := okObject("mode", "read", "session_id", sid, "link", r.link(ctx, sid), "session_meta", metaBlock(sess),
		"message_count", total, "truncated", truncated, "messages", messages)
	if truncated {
		o.set("message", fmt.Sprintf("Session has %d messages; showing first %d + last %d. "+
			"Pass around_message_id (any id above) to scroll the middle.", total, headN, tailN))
	}
	res, err := ok(o)
	return res, true, err
}

func (r *searchRun) readScoped(ctx context.Context, sid string) (tool.Result, error) {
	res, success, err := r.readSession(ctx, sid)
	if err != nil {
		return tool.Result{}, err
	}
	if success || r.profile != nil && truthy(r.profile) {
		return res, nil
	}
	return failure("session_id not found in this profile: " + sid + ". If it belongs to another " +
		"profile, pass profile=<name> (or the @session:<profile>/<id> link)."), nil
}

func (r *searchRun) browse(ctx context.Context, limit int) (tool.Result, error) {
	sessions, err := r.db.store.ListRecentSessionsBounded(ctx, state.RecentBrowseOptions{Limit: limit + 15,
		ExcludeSources: hiddenSessionSources, Timeout: 3 * time.Second})
	if err != nil {
		slog.ErrorContext(ctx, "Error listing recent sessions", "error", err)
		return failure("Failed to list recent sessions: " + err.Error()), nil
	}
	hidden := map[string]bool{}
	if r.current != "" {
		hidden[r.current] = true
		if root, compression := r.resolveToParent(ctx, r.current); compression && root != "" {
			hidden[root] = true
		}
	}
	results := []*object{}
	for _, sess := range sessions {
		if hidden[sess.ID] {
			continue
		}
		if len(results) >= limit {
			break
		}
		o := &object{}
		o.set("session_id", sess.ID)
		o.set("link", r.link(ctx, sess.ID))
		var title any
		if sess.Title.Valid && sess.Title.V != "" {
			title = sess.Title.V
		}
		o.set("title", title)
		o.set("source", sess.Source)
		o.set("started_at", floatOrNil(sess.StartedAt.V, sess.StartedAt.Valid))
		o.set("last_active", floatOrNil(sess.LastActive.V, sess.LastActive.Valid))
		if sess.MessageCount.Valid {
			o.set("message_count", sess.MessageCount.V)
		} else {
			o.set("message_count", nil)
		}
		o.set("preview", sess.Preview)
		results = append(results, o)
	}
	return ok(okObject("mode", "browse", "results", results, "count", len(results), "message",
		fmt.Sprintf("Showing %d most recent sessions. Pass a query= to search, "+
			"or session_id+around_message_id to scroll.", len(results))))
}

func floatOrNil(v float64, valid bool) any {
	if !valid {
		return nil
	}
	return v
}

func (r *searchRun) anchorInLiveContext(ctx context.Context, st *storageState, anchorSID string) bool {
	if !r.sameLineage(ctx, anchorSID, r.current) || compactedState(st) {
		return false
	}
	return st != nil && st.active == 0 || !r.leftLiveContext(ctx, anchorSID)
}

func (r *searchRun) scroll(ctx context.Context, sid string, rawAround, rawWindow any) (tool.Result, error) {
	around, valid := pyInt(rawAround)
	if !valid {
		return failure("scroll requires integer around_message_id"), nil
	}
	window := int(clampInt(rawWindow, 5, 1, 20))
	st, found := r.storageState(ctx, around)
	owning := ""
	if found {
		owning = st.sessionID
	} else {
		st = nil
	}
	anchorSID := owning
	if anchorSID == "" {
		anchorSID = sid
	}
	if r.current != "" && r.anchorInLiveContext(ctx, st, anchorSID) {
		return failure("scroll rejected: anchor lives in the current session lineage (already in your active context)"), nil
	}
	sess, exists := r.meta(ctx, sid)
	if !exists {
		return failure("session_id not found: " + sid), nil
	}
	view, err := r.db.store.GetMessagesAround(ctx, sid, around, window)
	if err != nil {
		slog.ErrorContext(ctx, "get_messages_around failed", "error", err)
		return failure("failed to load messages: " + err.Error()), nil
	}
	warning := ""
	if len(view.Window) == 0 && owning != "" && owning != sid && r.sameLineage(ctx, sid, owning) {
		rebind, err := r.db.store.GetMessagesAround(ctx, owning, around, window)
		if err != nil {
			slog.DebugContext(ctx, "rebind get_messages_around failed", "error", err)
		} else if len(rebind.Window) > 0 {
			warning = fmt.Sprintf("around_message_id %d lives in %s (child of %s); rebound transparently", around, owning, sid)
			view, sid = rebind, owning
			if owner, found := r.meta(ctx, owning); found {
				sess = owner
			}
		}
	}
	if len(view.Window) == 0 {
		return failure(fmt.Sprintf("around_message_id %d not in session_id %s", around, sid)), nil
	}
	o := okObject("mode", "scroll", "session_id", sid, "around_message_id", around, "session_meta", metaBlock(sess),
		"window", window, "messages", shapeAll(view.Window, around, true, 0),
		"messages_before", view.MessagesBefore, "messages_after", view.MessagesAfter, "hint", scrollHint)
	if warning != "" {
		o.set("warning", warning)
	}
	return ok(o)
}

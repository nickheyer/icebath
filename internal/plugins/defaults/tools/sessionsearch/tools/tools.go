package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/sessions/export"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	Name    = "session_search"
	toolset = "session_search"
	// Characters of a message a read shows
	readChars = 2000
	// Session ids a discovery may be told to leave out
	excludeCap = 20
	// Matches a discovery reads before folding them into sessions
	scanLimit = 300
	// Messages a read shows from the start and the end of a long session
	readHead, readTail = 20, 10
)

var (
	// Sources whose sessions are never searched or listed: integrations, delegated runs and board workers
	hiddenSources = []string{"kanban", "subagent", "tool"}
	// Sources searched but ranked below sessions a person had
	demotedSources = map[string]bool{"cron": true}
	relativeBound  = regexp.MustCompile(`(?i)^(\d+)\s*(h|d|w)$`)
	unitSeconds    = map[string]int64{"h": 3600, "d": 86400, "w": 604800}
	// Layouts a time bound may take, naive ones read as UTC
	boundLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02T15:04Z07:00", "2006-01-02T15:04",
		"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15", "2006-01-02"}
	roles = []sessionsv1.Role{sessionsv1.Role_ROLE_USER, sessionsv1.Role_ROLE_ASSISTANT}
)

const (
	noMatches = "No matching sessions found. FTS5 ANDs all terms by default — broaden with OR (`alpha OR beta`), exact-match with quoted phrases, " +
		"exclude with NOT, or prefix-match with `deploy*`."
	linkHint = "When referring the user to a session, write its `link` value verbatim inline mid-sentence (it renders as a titled link) — never " +
		"as markdown, in backticks, on its own line, or next to the title/id/date. To read more around a compact result, scroll: " +
		"session_search(session_id=..., around_message_id=match_message_id)."
	scrollHint = "Scroll forward: re-call with around_message_id = the LAST message's id; backward: the FIRST message's id (the boundary " +
		"message repeats as an orientation marker). messages_before/messages_after < window means you've hit that end of the session."
)

type registrant struct {
	config   *config.Config
	sessions *sessions.Registry
}

func Register(registry *tools.Registry, cfg *config.Config, stores *sessions.Registry) {
	registry.Register(registrant{config: cfg, sessions: stores})
}

func (r registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:    Name,
		Toolset: toolset,
		Title:   "Session search",
		About:   "Search and read your past sessions",
		Group:   "Sessions",
		Icon:    tools.IconSearch,
		Subject: "query",
		Schema: tools.Schema("Search or read past Icebath conversations (FTS5), or scroll inside one. Four shapes, picked by args: `query` = discovery "+
			"(top-N matching sessions, top result fully hydrated); `session_id` + `around_message_id` = scroll (window of messages around an anchor); "+
			"`session_id` alone = read a whole session — how you resolve an `@session:<profile>/<id>` link (split on '/' into profile + id); no "+
			"args = browse recent sessions. Results are actual DB messages, no LLM. Searches conversation history ONLY — when the user gave a direct "+
			"source (URL, file, contact, live system), inspect that first; never conclude 'not found' from history alone. Use for questions about past "+
			"conversations: 'what did we do about X', 'where did we leave Y'. When referring the user to a session, write its `link` value verbatim "+
			"inline (it renders as a titled link).",
			[]string{},
			map[string]any{
				"query": tools.Property("string", "Search query (discovery shape). Keywords, phrases, or boolean expressions to find in past sessions. "+
					"Omit to browse recent sessions. Ignored when session_id + around_message_id are set (scroll shape)."),
				"limit": tools.Property("integer", "Max sessions to return. Discovery defaults to 3; browse/no-args defaults to 10. Max 10. Bump discovery "+
					"to 5–10 when the topic likely spans several sessions and you want to pick the right one to scroll into."),
				"sort": map[string]any{"type": "string", "enum": []string{"newest", "oldest"}, "description": "Discovery shape only. Temporal bias on " +
					"top of FTS5 ranking: omit for relevance-only (exploratory recall), 'newest' for \"where did we leave X\", 'oldest' for \"how did X start\"."},
				"detail": map[string]any{"type": "string", "enum": []string{"adaptive", "full"}, "default": "adaptive", "description": "Discovery shape " +
					"only. 'adaptive' (default) fully hydrates the top-ranked result and returns only the exact anchor message for lower-ranked results. " +
					"'full' returns bookends and the complete anchored window for every result."},
				"after": tools.Property("string", "Discovery shape only. Inclusive lower bound on session start time. ISO date/datetime (e.g. 2026-06-01) "+
					"or relative duration (7d, 24h, 2w = within the last N). Use only when the user names a time frame. sort is a ranking bias, not a bound."),
				"before": tools.Property("string", "Discovery shape only. Exclusive upper bound on session start time. ISO date/datetime (a date-only value "+
					"is midnight UTC that day) or relative duration (7d = older than a week). Use only when the user names a time frame."),
				"exclude_session_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Discovery shape only. " +
					"Session ids already inspected this task. Those sessions and their lineage are omitted so a later query explores instead of repeating " +
					"the same hit. Cap 20."},
				"session_id": tools.Property("string", "Scroll shape. Session to read inside. Use the session_id returned from a prior discovery call. Must "+
					"be paired with around_message_id."),
				"around_message_id": tools.Property("integer", "Scroll shape. Message id to center the window on — use match_message_id from a discovery "+
					"result, or any id from a prior window."),
				"window": map[string]any{"type": "integer", "default": 5, "description": "Scroll shape only. Messages to return on each side of the anchor " +
					"(anchor itself always included). Clamped to [1, 20]. Default 5."},
				"role_filter": tools.Property("string", "Optional. Comma-separated roles to include. Discovery defaults to 'user,assistant' (tool output is "+
					"usually noise). Pass 'user,assistant,tool' to include tool output (debugging tool behaviour) or 'tool' to search tool output only."),
				"profile": tools.Property("string", "Optional. Read sessions from another icebath profile's database (read-only). Use when resolving an "+
					"`@session:<profile>/<id>` link: pass the profile segment here with session_id as the id segment. Omit to use the current profile."),
			},
		),
		Check:    func(*tools.Env) bool { _, err := r.store(); return err == nil },
		Handler:  r.handle,
		Effect:   tools.ReadOnly,
		Parallel: true,
	}}
}

func (r registrant) store() (sessions.Store, error) {
	return r.sessions.Store(r.config.GetString(sessionsconfig.Store))
}

// One call: the store and profile it reads, and the session it runs in, whose own lineage is left out of what it finds
type search struct {
	store   sessions.Store
	profile string
	current string
}

func (r registrant) handle(ctx context.Context, call *tools.Call) string {
	store, err := r.store()
	if err != nil {
		return failure("icebath can't open its session history right now. Sessions will not be saved until this is fixed. Run `icebath doctor` to " +
			"check the storage location.")
	}
	args := call.Args
	cfg := r.config
	if call.Env != nil && call.Env.Config != nil {
		cfg = call.Env.Config
	}
	id := strings.TrimPrefix(strings.TrimSpace(text(args["session_id"])), "@session:")
	profileArg := strings.TrimSpace(text(args["profile"]))
	if named, rest, ok := strings.Cut(id, "/"); ok && rest != "" {
		id = rest
		if named != "" && profileArg == "" {
			profileArg = named
		}
	}
	s := search{store: store, profile: cfg.Profile(), current: call.SessionID}
	if profileArg != "" {
		canonical, err := profile.Normalize(profileArg)
		if err == nil {
			err = profile.Validate(canonical)
		}
		switch {
		case err != nil:
			return failure(fmt.Sprintf("profile '%s': %v", profileArg, err))
		case canonical != profile.Default && !cfg.Exists(canonical):
			return failure(fmt.Sprintf("profile '%s': profile '%s' does not exist", profileArg, canonical))
		}
		s.profile, s.current = canonical, ""
	}
	if id != "" {
		if anchor, present := args["around_message_id"]; present && anchor != nil {
			return s.scroll(ctx, id, anchor, argOr(args, "window", 5))
		}
		return s.readScoped(ctx, id, profileArg != "")
	}
	query := strings.TrimSpace(text(args["query"]))
	limit := clamp(args["limit"], 3, 1, 10)
	if query == "" {
		return s.browse(ctx, clamp(args["limit"], 10, 1, 10))
	}
	order := ""
	if sorted := strings.ToLower(strings.TrimSpace(text(args["sort"]))); sorted == "newest" || sorted == "oldest" {
		order = sorted
	}
	after, err := parseBound(args["after"])
	if err != nil {
		return failure(err.Error())
	}
	before, err := parseBound(args["before"])
	if err != nil {
		return failure(err.Error())
	}
	var roleFilter []string
	if filter, ok := args["role_filter"].(string); ok {
		for _, role := range strings.Split(filter, ",") {
			if role = strings.TrimSpace(role); role != "" {
				roleFilter = append(roleFilter, role)
			}
		}
	}
	detail := "adaptive"
	if strings.ToLower(strings.TrimSpace(text(args["detail"]))) == "full" {
		detail = "full"
	}
	return s.discover(ctx, query, roleFilter, limit, order, detail, after, before, excluded(args["exclude_session_ids"]))
}

// Text of an argument, empty for one that is not text
func text(value any) string {
	s, _ := value.(string)
	return s
}

func argOr(args map[string]any, key string, fallback any) any {
	if value, ok := args[key]; ok {
		return value
	}
	return fallback
}

// Whole number of an argument, false for one that is not
func integer(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int64(math.Max(math.Min(v, math.MaxInt64), math.MinInt64)), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		text := strings.ReplaceAll(strings.TrimSpace(v), "_", "")
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil && f == math.Trunc(f) {
			return int64(f), true
		}
	}
	return 0, false
}

// Number of an argument held within bounds, the default for one that is no number
func clamp(value any, fallback, low, high int) int {
	n, ok := integer(value)
	if !ok {
		return fallback
	}
	return int(min(max(n, int64(low)), int64(high)))
}

// Session ids a discovery leaves out, each once, at most twenty
func excluded(raw any) []string {
	var items []any
	switch v := raw.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	}
	out := []string{}
	for _, item := range items {
		if id := strings.TrimSpace(text(item)); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
		if len(out) >= excludeCap {
			break
		}
	}
	return out
}

// Start of sessions a discovery is bounded by, and whether it lies beyond what the store can hold
type bound struct {
	at       time.Time
	overflow bool
}

// Bound of an ISO date or time, UTC when it names no zone, or of a duration back from now, nil when empty
func parseBound(value any) (*bound, error) {
	if value == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(fmt.Sprint(value))
	if raw == "" {
		return nil, nil
	}
	if match := relativeBound.FindStringSubmatch(raw); match != nil {
		n, _ := new(big.Int).SetString(match[1], 10)
		back := new(big.Int).Mul(n, big.NewInt(unitSeconds[strings.ToLower(match[2])]))
		at := new(big.Int).Sub(big.NewInt(time.Now().Unix()), back)
		if !at.IsInt64() || at.Int64() < -62135596800 {
			return &bound{overflow: true}, nil
		}
		return &bound{at: time.Unix(at.Int64(), 0)}, nil
	}
	for _, layout := range boundLayouts {
		if at, err := time.Parse(layout, strings.Replace(raw, "z", "Z", 1)); err == nil {
			return &bound{at: at}, nil
		}
	}
	return nil, fmt.Errorf("invalid time bound: '%s' (expected ISO date/datetime like 2026-07-01, or a relative duration like 7d, 24h, 2w)", raw)
}

// Whether a session started within the bounds
func within(started *timestamppb.Timestamp, after, before *bound) bool {
	if after == nil && before == nil {
		return true
	}
	if started == nil {
		return false
	}
	at := started.AsTime()
	return (after == nil || after.overflow || !at.Before(after.at)) && (before == nil || !before.overflow && at.Before(before.at))
}

// Result the model reads when the call fails
func failure(message string) string {
	if runes := []rune(message); len(runes) > 2000 {
		message = string(runes[:2000]) + "… [truncated]"
	}
	return tools.JSON(struct {
		Error   string `json:"error"`
		Success bool   `json:"success"`
	}{message, false})
}

// Session, empty when it is not there or belongs to another profile than the one searched
func (s search) session(ctx context.Context, id string) *sessionsv1.Session {
	if id == "" {
		return &sessionsv1.Session{}
	}
	found, err := s.store.Get(ctx, id)
	if err != nil || cmp.Or(found.GetProfile(), "default") != s.profile {
		return &sessionsv1.Session{}
	}
	return found
}

// First session of the session's lineage, through every parent, and whether a compression lies on the way
func (s search) root(ctx context.Context, id string) (string, bool) {
	seen := map[string]bool{}
	current, compressed := id, false
	for current != "" && !seen[current] {
		seen[current] = true
		session := s.session(ctx, current)
		compressed = compressed || session.GetEndReason() == sessions.EndCompression
		if session.GetParentId() == "" {
			break
		}
		current = session.GetParentId()
	}
	return current, compressed
}

func (s search) lineage(ctx context.Context, id string) string {
	root, _ := s.root(ctx, id)
	return root
}

func (s search) sameLineage(ctx context.Context, a, b string) bool {
	root := s.lineage(ctx, a)
	return root != "" && root == s.lineage(ctx, b)
}

// Whether a session's conversation left the agent's live context: compressed into a child, or ended on purpose
func (s search) leftContext(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	reason := s.session(ctx, id).GetEndReason()
	return reason == sessions.EndCompression || sessions.Boundary(reason)
}

// Message by id and its session, nil when there is none
func (s search) locate(ctx context.Context, id int64) (string, *sessionsv1.Message) {
	session, message, err := s.store.Locate(ctx, id)
	if err != nil {
		return "", nil
	}
	return session, message
}

// When a session started, as a model reads it
func when(t *timestamppb.Timestamp) string {
	if t == nil {
		return "unknown"
	}
	return t.AsTime().Local().Format("January 02, 2006 at 03:04 PM")
}

type meta struct {
	When   string  `json:"when"`
	Source *string `json:"source"`
	Model  *string `json:"model"`
	Title  *string `json:"title"`
}

func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func metaOf(session *sessionsv1.Session) meta {
	return meta{When: when(session.GetCreated()), Source: optional(session.GetSource()), Model: optional(session.GetModel()),
		Title: optional(session.GetTitle())}
}

type call struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Message as a result shows it
type shaped struct {
	ID         int64   `json:"id"`
	Role       string  `json:"role"`
	Content    string  `json:"content"`
	Timestamp  float64 `json:"timestamp"`
	ToolName   string  `json:"tool_name,omitempty"`
	ToolCalls  []call  `json:"tool_calls,omitempty"`
	ToolCallID string  `json:"tool_call_id,omitempty"`
	Anchor     bool    `json:"anchor,omitempty"`
	Truncated  bool    `json:"content_truncated,omitempty"`
	Original   int     `json:"original_content_chars,omitempty"`
}

// Message as a result shows it, the anchor marked and its text cut to the limit, none when zero
func shape(m *sessionsv1.Message, anchor int64, limit int) shaped {
	content := m.GetText()
	if strings.Contains(content, "\x1b") {
		content = term.StripANSI(content)
	}
	out := shaped{ID: m.GetId(), Role: export.Role(m), Content: content, ToolName: m.GetToolName(), ToolCallID: m.GetToolCallId(),
		Anchor: anchor != 0 && m.GetId() == anchor}
	if t := m.GetTime(); t != nil {
		out.Timestamp = float64(t.AsTime().UnixNano()) / 1e9
	}
	for _, c := range m.GetToolCalls() {
		entry := call{ID: c.GetId(), Type: "function"}
		entry.Function.Name, entry.Function.Arguments = c.GetName(), c.GetArguments()
		out.ToolCalls = append(out.ToolCalls, entry)
	}
	if n := utf8.RuneCountInString(content); limit > 0 && n > limit {
		out.Content, out.Truncated, out.Original = string([]rune(content)[:limit])+"…", true, n
	}
	return out
}

func shapeAll(messages []*sessionsv1.Message, anchor int64, limit int) []shaped {
	out := make([]shaped, 0, len(messages))
	for _, m := range messages {
		out = append(out, shape(m, anchor, limit))
	}
	return out
}

// Session reference the agent writes, which the app shows as a titled link
func (s search) link(id string) string {
	return "@session:" + s.profile + "/" + id
}

// Session a discovery found
type found struct {
	SessionID      string   `json:"session_id"`
	When           string   `json:"when"`
	Source         string   `json:"source"`
	Model          string   `json:"model"`
	Title          *string  `json:"title"`
	MatchedRole    string   `json:"matched_role"`
	MatchMessageID int64    `json:"match_message_id"`
	Snippet        string   `json:"snippet"`
	BookendStart   []shaped `json:"bookend_start"`
	Messages       []shaped `json:"messages"`
	BookendEnd     []shaped `json:"bookend_end"`
	MessagesBefore int      `json:"messages_before"`
	MessagesAfter  int      `json:"messages_after"`
	Detail         string   `json:"detail"`
	Parent         string   `json:"parent_session_id,omitempty"`
	Link           string   `json:"link"`
	// Lineage the session belongs to, which no other result repeats
	root string
}

type rebuild struct {
	Percent int32  `json:"percent"`
	Note    string `json:"note"`
}

type discovery struct {
	Success  bool     `json:"success"`
	Mode     string   `json:"mode"`
	Query    string   `json:"query"`
	Detail   string   `json:"detail"`
	Results  []*found `json:"results"`
	Count    int      `json:"count"`
	Message  string   `json:"message,omitempty"`
	Searched *int     `json:"sessions_searched,omitempty"`
	LinkHint string   `json:"link_hint,omitempty"`
	Rebuild  *rebuild `json:"index_rebuild,omitempty"`
}

// Discovery result, noting a rebuild of the search index under way so thin results are explained
func (s search) discovered(ctx context.Context, d discovery) string {
	d.Success, d.Mode, d.Count = true, "discover", len(d.Results)
	if d.Results == nil {
		d.Results = []*found{}
	}
	if indexing, err := s.store.Indexing(ctx); err == nil && indexing != nil {
		d.Rebuild = &rebuild{Percent: indexing.GetPercent(), Note: fmt.Sprintf("The search index is rebuilding in the background (%d%% done, %s of %s "+
			"messages). Results from older messages may be incomplete until it finishes.", indexing.GetPercent(), strs.Thousands(indexing.GetIndexed()),
			strs.Thousands(indexing.GetTotal()))}
	}
	return tools.JSON(d)
}

// Session whose title the query names, as the first result, nil when none does or it is in the session's own lineage
func (s search) titleMatch(ctx context.Context, query, currentRoot string) *found {
	title := strings.Trim(strings.TrimSpace(query), "`'\"")
	if title == "" {
		return nil
	}
	session, err := s.store.Titled(ctx, s.profile, title)
	if err != nil || session.GetId() == "" {
		return nil
	}
	id := session.GetId()
	root := s.lineage(ctx, id)
	if currentRoot != "" && root == currentRoot && !s.leftContext(ctx, id) {
		return nil
	}
	described := s.session(ctx, root)
	if described.GetId() == "" {
		described = s.session(ctx, id)
	}
	if slices.Contains(hiddenSources, described.GetSource()) {
		return nil
	}
	messages, err := s.store.Messages(ctx, id, sessions.Model)
	if err != nil {
		messages = nil
	}
	var anchor int64
	var window sessions.Window
	if len(messages) > 0 {
		anchor = messages[0].GetId()
		if window, err = s.store.Around(ctx, id, anchor, 5, 3, roles); err != nil {
			window = sessions.Window{}
		}
	}
	or := func(view []*sessionsv1.Message, fallback []*sessionsv1.Message) []*sessionsv1.Message {
		if len(view) > 0 {
			return view
		}
		return fallback
	}
	after := window.After
	if len(window.Messages) == 0 {
		after = max(len(messages)-5, 0)
	}
	shown := cmp.Or(described.GetTitle(), title)
	result := &found{SessionID: id, When: when(described.GetCreated()), Source: cmp.Or(described.GetSource(), "unknown"),
		Model: cmp.Or(described.GetModel(), "unknown"), Title: &shown, MatchedRole: "session_title", MatchMessageID: anchor,
		Snippet: "Session title matched: " + shown, BookendStart: shapeAll(or(window.Start, messages[:min(3, len(messages))]), 0, 1200),
		Messages:   shapeAll(or(window.Messages, messages[:min(5, len(messages))]), anchor, 4000),
		BookendEnd: shapeAll(or(window.End, messages[max(0, len(messages)-3):]), 0, 1200), MessagesBefore: window.Before, MessagesAfter: after,
		Detail: "full", root: root}
	if root != "" && root != id {
		result.Parent = root
	}
	return result
}

// Bookend messages of a window, summaries of compacted history left out
func bookends(messages []*sessionsv1.Message) []shaped {
	out := []shaped{}
	for _, m := range messages {
		if !sessions.IsSummary(m.GetText()) {
			out = append(out, shape(m, 0, 1200))
		}
	}
	return out
}

// Result of a match: the messages around it, all of them in full detail, only the match itself otherwise; nil when they can't be read
func (s search) hydrate(ctx context.Context, root string, hit *sessionsv1.Hit, detail string) *found {
	id := cmp.Or(hit.GetSessionId(), root)
	window, err := s.store.Around(ctx, id, hit.GetId(), 5, 3, roles)
	if err != nil {
		return nil
	}
	described, full := s.session(ctx, root), detail == "full"
	messages, start, end := []shaped{}, []shaped{}, []shaped{}
	for _, m := range window.Messages {
		if full || m.GetId() == hit.GetId() {
			messages = append(messages, shape(m, hit.GetId(), 4000))
		}
	}
	if full {
		start, end = bookends(window.Start), bookends(window.End)
	}
	started := described.GetCreated()
	if started == nil {
		started = hit.GetSessionStarted()
	}
	result := &found{SessionID: id, When: when(started), Source: cmp.Or(described.GetSource(), cmp.Or(hit.GetSource(), "unknown")),
		Model: cmp.Or(described.GetModel(), cmp.Or(hit.GetModel(), "unknown")), Title: optional(described.GetTitle()),
		MatchedRole: export.Role(&sessionsv1.Message{Role: hit.GetRole()}), MatchMessageID: hit.GetId(), Snippet: hit.GetSnippet(), BookendStart: start,
		Messages: messages, BookendEnd: end, MessagesBefore: window.Before, MessagesAfter: window.After, Detail: detail, root: root}
	if root != "" && root != id {
		result.Parent = root
	}
	return result
}

// Discovery: the sessions whose messages match, each lineage once, the best fully shown and the rest by their match unless full detail is asked
func (s search) discover(ctx context.Context, query string, roleFilter []string, limit int, order, detail string, after, before *bound,
	exclude []string) string {
	currentRoot := ""
	if s.current != "" {
		currentRoot = s.lineage(ctx, s.current)
	}
	excludedRoots := map[string]bool{}
	for _, id := range exclude {
		excludedRoots[id] = true
		excludedRoots[cmp.Or(s.lineage(ctx, id), id)] = true
	}
	title := s.titleMatch(ctx, query, currentRoot)
	if title != nil {
		described := s.session(ctx, title.root)
		if described.GetId() == "" {
			described = s.session(ctx, title.SessionID)
		}
		if excludedRoots[title.SessionID] || excludedRoots[title.root] || !within(described.GetCreated(), after, before) {
			title = nil
		}
	}
	if roleFilter == nil {
		roleFilter = []string{"user", "assistant"}
	}
	for _, edge := range []struct {
		name string
		b    *bound
	}{{"after", after}, {"before", before}} {
		if edge.b != nil && edge.b.overflow {
			return failure("Search failed: the " + edge.name + " timestamp is beyond the 64-bit integer range the session store holds")
		}
	}
	request := &sessionsv1.SearchRequest{Query: query, Roles: roleFilter, ExcludeSources: hiddenSources, Limit: scanLimit, Sort: order,
		Profile: s.profile}
	if after != nil {
		request.After = timestamppb.New(after.at)
	}
	if before != nil {
		request.Before = timestamppb.New(before.at)
	}
	hits, err := s.store.Search(ctx, request)
	if err != nil {
		return failure("Search failed: " + err.Error())
	}
	slices.SortStableFunc(hits, func(a, b *sessionsv1.Hit) int {
		da, db := demotedSources[a.GetSource()], demotedSources[b.GetSource()]
		switch {
		case !da && db:
			return -1
		case da && !db:
			return 1
		}
		return 0
	})
	if len(hits) == 0 && title == nil {
		return s.discovered(ctx, discovery{Query: query, Detail: detail, Message: noMatches})
	}
	type match struct {
		hit       *sessionsv1.Hit
		titleOnly bool
	}
	var ranked []string
	seen := map[string]match{}
	var results []*found
	if title != nil {
		results = append(results, title)
		if title.root != "" {
			ranked = append(ranked, title.root)
			seen[title.root] = match{titleOnly: true}
		}
	}
	for _, hit := range hits {
		if len(seen) >= limit {
			break
		}
		id := hit.GetSessionId()
		root := s.lineage(ctx, id)
		if excludedRoots[id] || excludedRoots[root] {
			continue
		}
		_, located := s.locate(ctx, hit.GetId())
		compacted := located.GetCompacted()
		if currentRoot != "" && root == currentRoot && !(s.leftContext(ctx, id) || compacted) {
			continue
		}
		if s.current != "" && id == s.current && !compacted {
			continue
		}
		if _, dup := seen[root]; !dup {
			ranked = append(ranked, root)
			seen[root] = match{hit: hit}
		}
	}
	for _, root := range ranked {
		entry := seen[root]
		if entry.titleOnly {
			continue
		}
		depth := "compact"
		if detail == "full" || len(results) == 0 {
			depth = "full"
		}
		if result := s.hydrate(ctx, root, entry.hit, depth); result != nil {
			results = append(results, result)
		}
	}
	for _, result := range results {
		result.Link = s.link(result.SessionID)
	}
	searched := len(seen)
	return s.discovered(ctx, discovery{Query: query, Detail: detail, Results: results, Searched: &searched, LinkHint: linkHint})
}

type read struct {
	Success  bool     `json:"success"`
	Mode     string   `json:"mode"`
	ID       string   `json:"session_id"`
	Link     string   `json:"link"`
	Meta     meta     `json:"session_meta"`
	Count    int      `json:"message_count"`
	Cut      bool     `json:"truncated"`
	Messages []shaped `json:"messages"`
	Message  string   `json:"message,omitempty"`
}

// Read: the whole session, or its start and end with a pointer to scroll the middle
func (s search) read(ctx context.Context, id string) (string, bool) {
	session := s.session(ctx, id)
	if session.GetId() == "" {
		return failure("session_id not found: " + id), false
	}
	messages, err := s.store.Messages(ctx, id, sessions.Model)
	if err != nil {
		return failure("failed to load session: " + err.Error()), true
	}
	all := shapeAll(messages, 0, readChars)
	total := len(all)
	out := read{Success: true, Mode: "read", ID: id, Link: s.link(id), Meta: metaOf(session), Count: total, Cut: total > readHead+readTail,
		Messages: all}
	if out.Cut {
		out.Messages = append(append([]shaped{}, all[:readHead]...), all[total-readTail:]...)
		out.Message = fmt.Sprintf("Session has %d messages; showing first %d + last %d. Pass around_message_id (any id above) to scroll the middle.",
			total, readHead, readTail)
	}
	return tools.JSON(out), true
}

// Read of a session of the profile searched, a bare id never taken from another profile
func (s search) readScoped(ctx context.Context, id string, named bool) string {
	result, ok := s.read(ctx, id)
	if ok || named {
		return result
	}
	return failure(fmt.Sprintf("session_id not found in this profile: %s. If it belongs to another profile, pass profile=<name> (or the "+
		"@session:<profile>/<id> link).", id))
}

type recent struct {
	ID       string  `json:"session_id"`
	Link     string  `json:"link"`
	Title    *string `json:"title"`
	Source   string  `json:"source"`
	Started  float64 `json:"started_at"`
	Active   float64 `json:"last_active"`
	Messages int64   `json:"message_count"`
	Preview  string  `json:"preview"`
}

func unix(t *timestamppb.Timestamp) float64 {
	if t == nil {
		return 0
	}
	return float64(t.AsTime().UnixNano()) / 1e9
}

// Browse: the most recent sessions, the session itself and the one it was compressed from left out
func (s search) browse(ctx context.Context, limit int) string {
	listed, _, err := s.store.List(ctx, &sessionsv1.Filter{Limit: int32(limit + 15), ExcludeSources: hiddenSources, Profile: s.profile})
	if err != nil {
		return failure("Failed to list recent sessions: " + err.Error())
	}
	hidden := map[string]bool{}
	if s.current != "" {
		hidden[s.current] = true
		if root, compressed := s.root(ctx, s.current); compressed && root != "" {
			hidden[root] = true
		}
	}
	results := []recent{}
	for _, session := range listed {
		if hidden[session.GetId()] {
			continue
		}
		if len(results) >= limit {
			break
		}
		results = append(results, recent{ID: session.GetId(), Link: s.link(session.GetId()), Title: optional(session.GetTitle()),
			Source: session.GetSource(), Started: unix(session.GetCreated()), Active: unix(cmpTime(session.GetUpdated(), session.GetCreated())),
			Messages: session.GetMessages(), Preview: session.GetPreview()})
	}
	return tools.JSON(struct {
		Success bool     `json:"success"`
		Mode    string   `json:"mode"`
		Results []recent `json:"results"`
		Count   int      `json:"count"`
		Message string   `json:"message"`
	}{true, "browse", results, len(results), fmt.Sprintf("Showing %d most recent sessions. Pass a query= to search, or session_id+around_message_id "+
		"to scroll.", len(results))})
}

func cmpTime(a, b *timestamppb.Timestamp) *timestamppb.Timestamp {
	if a != nil {
		return a
	}
	return b
}

type scrolled struct {
	Success  bool     `json:"success"`
	Mode     string   `json:"mode"`
	ID       string   `json:"session_id"`
	Anchor   int64    `json:"around_message_id"`
	Meta     meta     `json:"session_meta"`
	Window   int      `json:"window"`
	Messages []shaped `json:"messages"`
	Before   int      `json:"messages_before"`
	After    int      `json:"messages_after"`
	Hint     string   `json:"hint"`
	Warning  string   `json:"warning,omitempty"`
}

// Whether a scroll anchor is still in the session's live context, which it already holds
func (s search) live(ctx context.Context, message *sessionsv1.Message, owner string) bool {
	if !s.sameLineage(ctx, owner, s.current) || message.GetCompacted() {
		return false
	}
	if message != nil && message.GetRemoved() {
		return true
	}
	return !s.leftContext(ctx, owner)
}

// Scroll: the messages around an anchor, no search and no bookends
func (s search) scroll(ctx context.Context, id string, raw any, size any) string {
	anchor, ok := integer(raw)
	if _, isBool := raw.(bool); !ok || isBool {
		return failure("scroll requires integer around_message_id")
	}
	width := clamp(size, 5, 1, 20)
	owner, message := s.locate(ctx, anchor)
	in := cmp.Or(owner, id)
	if s.current != "" && s.live(ctx, message, in) {
		return failure("scroll rejected: anchor lives in the current session lineage (already in your active context)")
	}
	session := s.session(ctx, id)
	if session.GetId() == "" {
		return failure("session_id not found: " + id)
	}
	window, err := s.store.Around(ctx, id, anchor, width, 0, nil)
	if err != nil {
		return failure("failed to load messages: " + err.Error())
	}
	warning := ""
	if len(window.Messages) == 0 && owner != "" && owner != id && s.sameLineage(ctx, id, owner) {
		if moved, err := s.store.Around(ctx, owner, anchor, width, 0, nil); err == nil && len(moved.Messages) > 0 {
			warning = fmt.Sprintf("around_message_id %d lives in %s (child of %s); rebound transparently", anchor, owner, id)
			window, id = moved, owner
			if described := s.session(ctx, owner); described.GetId() != "" {
				session = described
			}
		}
	}
	if len(window.Messages) == 0 {
		return failure(fmt.Sprintf("around_message_id %d not in session_id %s", anchor, id))
	}
	return tools.JSON(scrolled{Success: true, Mode: "scroll", ID: id, Anchor: anchor, Meta: metaOf(session), Window: width,
		Messages: shapeAll(window.Messages, anchor, 0), Before: window.Before, After: window.After, Hint: scrollHint, Warning: warning})
}

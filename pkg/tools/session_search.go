package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session sources hidden from browsing and searching, integrations, delegate runs and kanban workers
var sessionSearchHiddenSources = []string{"kanban", "subagent", "tool"}

// Automation sources kept searchable but ranked below interactive sessions in discovery
var sessionSearchDemotedSources = map[string]bool{"cron": true}

const (
	sessionSearchReadMaxContent = 2000
	excludeSessionIDsCap        = 20
)

// FTS rows scanned before the lineage dedup, a test lowers it to force the SQL time window
var discoverScanLimit = 300

var (
	relativeBoundRe      = regexp.MustCompile(`(?i)^(\d+)\s*(h|d|w)$`)
	relativeUnitSeconds  = map[string]int64{"h": 3600, "d": 86400, "w": 604800}
	discoverSearchFields = []string{"id", "session_id", "role", "snippet", "source", "model", "session_started"}
	compactionPrefixes   = []string{"[CONTEXT COMPACTION", "[CONTEXT SUMMARY]:"}
	freshResetEndReasons = state.BoundaryEndReasons()
)

// SessionSearchStore is the session database surface the session_search shapes read
type SessionSearchStore interface {
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
	GetMessages(ctx context.Context, sessionID string, o state.GetMessagesOptions) ([]*pyjson.Object, error)
	SearchMessages(ctx context.Context, query string, o state.SearchMessagesOptions) ([]*pyjson.Object, error)
	GetAnchoredView(ctx context.Context, sessionID string, aroundMessageID int64, o state.AnchoredViewOptions) (*pyjson.Object, error)
	GetMessagesAround(ctx context.Context, sessionID string, aroundMessageID int64, window int) (*pyjson.Object, error)
	ResolveSessionByTitle(ctx context.Context, title string) (string, bool, error)
	FTSRebuildStatus(ctx context.Context) (*pyjson.Object, error)
	ReadCtx(ctx context.Context, fn func(conn state.SQLiteConn) error) error
	Close() error
}

// Bounded recent session browse a store may offer, the whole database listing is never used
type boundedRecentSessionLister interface {
	ListRecentSessionsBounded(ctx context.Context, o state.RecentSessionsBoundedOptions) ([]*pyjson.Object, error)
}

// ProfileStoreHooks resolve profile names and homes for cross profile reads, assigned by pkg/profiles at init
var ProfileStoreHooks struct {
	ActiveName func(ctx context.Context) string
	Normalize  func(name string) (string, error)
	Validate   func(name string) error
	Exists     func(name string) bool
	Dir        func(name string) (string, error)
}

// SessionSearchArgs are the session_search keyword arguments in their hermes order
type SessionSearchArgs struct {
	Query             any
	RoleFilter        any
	Limit             any
	DB                SessionSearchStore
	CurrentSessionID  string
	SessionID         any
	AroundMessageID   any
	Window            any
	Sort              any
	Profile           any
	Detail            any
	After             any
	Before            any
	ExcludeSessionIDs any
}

// Dict value for a key, nil when the object or key is absent
func oget(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}

// Dict get with a default for a missing key, a present None staying None
func ogetDefault(o *pyjson.Object, key string, def any) any {
	if o != nil {
		if v, ok := o.Get(key); ok {
			return v
		}
	}
	return def
}

// String form of a dict value, empty for nil
func ostr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Python int of a value as int64, the sign extreme when it does not fit, false where int raises
func sessionSearchInt(v any) (int64, bool) {
	if v == nil {
		return 0, false
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, false
	}
	if n.IsInt64() {
		return n.Int64(), true
	}
	if n.Sign() > 0 {
		return math.MaxInt64, true
	}
	return math.MinInt64, true
}

// Two ids compared as Python ints, false when either is not an int
func sameInt(a, b any) bool {
	x, okA := sessionSearchInt(a)
	y, okB := sessionSearchInt(b)
	_, boolA := a.(bool)
	_, boolB := b.(bool)
	return okA && okB && !boolA && !boolB && x == y
}

// Compact JSON with non ASCII kept, the json.dumps ensure_ascii False every shape returns
func sessionSearchDumps(v any) string {
	out, err := pyjson.Dumps(v, pyjson.EnsureASCII(false))
	if err != nil {
		return ErrorResult(err)
	}
	return out
}

// Success payload with success first and the given keys in order
func sessionSearchOK(payload *pyjson.Object) string {
	out := pyjson.NewObject()
	out.Set("success", true)
	for _, k := range payload.Keys {
		out.Set(k, payload.Vals[k])
	}
	return sessionSearchDumps(out)
}

// Result of fn, or the error payload the model must see after an error level log
func loud[T any](fn func() (T, error), logMsg, errorPrefix string, logArgs ...any) (T, string) {
	out, err := fn()
	if err != nil {
		slog.Error(fmt.Sprintf(logMsg, append(logArgs, err)...))
		var zero T
		return zero, sessionSearchError(fmt.Sprintf("%s: %v", errorPrefix, err))
	}
	return out, ""
}

// Error payload the model sees, the bounded error text and success False
func sessionSearchError(message string) string {
	out := pyjson.NewObject()
	out.Set("error", boundErrorText(message))
	out.Set("success", false)
	return sessionSearchDumps(out)
}

// Unix timestamp as a readable local date, ISO text passes through, unknown for None
func formatTimestamp(ts any) string {
	if ts == nil {
		return "unknown"
	}
	if s, ok := ts.(string); ok {
		digits := strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), "-", "")
		if digits == "" || strings.ContainsFunc(digits, func(r rune) bool { return !pystr.IsDigit(r) }) {
			return s
		}
	}
	f, ok := sessionSearchFloat(ts)
	if !ok {
		return pyrepr.Str(ts)
	}
	sec := math.Floor(f)
	t := time.Unix(int64(sec), int64(math.Round((f-sec)*1e6))*1000).Local()
	if math.IsNaN(f) || math.IsInf(f, 0) || t.Year() < 1 || t.Year() > 9999 {
		return pyrepr.Str(ts)
	}
	return config.SafeStrftime(t, "%B %d, %Y at %I:%M %p")
}

// Python float of a number or numeric string, false where float raises
func sessionSearchFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case *big.Int:
		f, _ := new(big.Float).SetInt(x).Float64()
		return f, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(pystr.Strip(x), "_", ""), 64)
		return f, err == nil
	}
	return 0, false
}

// Session row of an id, an empty dict when it is missing or the read fails
func getSessionMeta(ctx context.Context, db SessionSearchStore, sessionID string) *pyjson.Object {
	meta, err := db.GetSession(ctx, sessionID)
	if err != nil {
		slog.Debug(fmt.Sprintf("get_session failed for %s: %v", sessionID, err))
		return pyjson.NewObject()
	}
	if meta == nil {
		return pyjson.NewObject()
	}
	return meta
}

// Session metadata block of a result, when, source, model and title
func sessionMetaBlock(meta *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("when", formatTimestamp(oget(meta, "started_at")))
	out.Set("source", oget(meta, "source"))
	out.Set("model", oget(meta, "model"))
	out.Set("title", oget(meta, "title"))
	return out
}

// True for a compaction handoff summary, False for empty or non text content
func isCompactionSummary(content any) bool {
	s, ok := content.(string)
	if !ok || s == "" {
		return false
	}
	trimmed := strings.TrimLeftFunc(s, pystr.IsSpace)
	for _, p := range compactionPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// Lineage root of a session and whether the parent chain crosses a compression rotation
func resolveToParent(ctx context.Context, db SessionSearchStore, sessionID string) (string, bool) {
	visited := map[string]bool{}
	cur, hasCompression := sessionID, false
	for cur != "" && !visited[cur] {
		visited[cur] = true
		s := getSessionMeta(ctx, db, cur)
		reason, _ := oget(s, "end_reason").(string)
		hasCompression = hasCompression || reason == "compression"
		parent := oget(s, "parent_session_id")
		if !pyrepr.Truthy(parent) {
			break
		}
		cur = ostr(parent)
	}
	return cur, hasCompression
}

// Lineage root of a session
func resolveLineage(ctx context.Context, db SessionSearchStore, sessionID string) string {
	root, _ := resolveToParent(ctx, db, sessionID)
	return root
}

// Time bound of a discovery, an int unix timestamp or a relative bound too far below zero for SQLite
type sessionSearchBound struct {
	value    int64
	overflow bool
}

// Python ValueError text of an invalid bound
func invalidTimeBound(value any) error {
	return errors.New("invalid time bound: " + pyrepr.Repr(value) + " (expected ISO date/datetime like " +
		"2026-07-01, or a relative duration like 7d, 24h, 2w)")
}

// ISO date or datetime or a relative duration as a UTC unix timestamp, nil for an empty bound
func parseISOBound(value any) (*sessionSearchBound, error) {
	if value == nil {
		return nil, nil
	}
	text := pystr.Strip(pyrepr.Str(value))
	if text == "" {
		return nil, nil
	}
	if rel := relativeBoundRe.FindStringSubmatch(text); rel != nil {
		n, _ := new(big.Int).SetString(rel[1], 10)
		unit := big.NewInt(relativeUnitSeconds[strings.ToLower(rel[2])])
		ts := new(big.Int).Sub(big.NewInt(time.Now().Unix()), n.Mul(n, unit))
		if !ts.IsInt64() {
			return &sessionSearchBound{overflow: true}, nil
		}
		return &sessionSearchBound{value: ts.Int64()}, nil
	}
	parsed, aware, err := pydatetime.ParseISOFormat(text)
	if err != nil {
		return nil, invalidTimeBound(value)
	}
	if !aware {
		parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.UTC)
	}
	sec := parsed.Unix()
	if sec < 0 && parsed.Nanosecond() > 0 {
		sec++
	}
	return &sessionSearchBound{value: sec}, nil
}

// sessions.started_at as an int unix timestamp from a number or ISO text, nil when unusable
func coerceStartedTS(value any) *int64 {
	if value == nil {
		return nil
	}
	if _, isBool := value.(bool); isBool {
		return nil
	}
	if s, ok := value.(string); ok {
		if s == "" {
			return nil
		}
		text := pystr.Strip(s)
		if f, ok := sessionSearchFloat(text); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			v := int64(f)
			return &v
		}
		bound, err := parseISOBound(text)
		if err != nil || bound == nil || bound.overflow {
			return nil
		}
		return &bound.value
	}
	if f, ok := sessionSearchFloat(value); ok {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		v := int64(f)
		return &v
	}
	return nil
}

// True when after <= started < before, an unknown start failing any bound
func inTimeWindow(started *int64, after, before *sessionSearchBound) bool {
	if after == nil && before == nil {
		return true
	}
	if started == nil {
		return false
	}
	afterOK := after == nil || after.overflow || *started >= after.value
	beforeOK := before == nil || (!before.overflow && *started < before.value)
	return afterOK && beforeOK
}

// Deduped stripped session ids from a string or list, capped at twenty
func normalizeExcludeSessionIDs(raw any) []string {
	var items []any
	switch v := raw.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	}
	out := []string{}
	for _, item := range items {
		sid := ""
		if s, ok := item.(string); ok {
			sid = pystr.Strip(s)
		}
		if sid != "" && !containsString(out, sid) {
			out = append(out, sid)
		}
		if len(out) >= excludeSessionIDsCap {
			break
		}
	}
	return out
}

// Excluded ids plus their lineage roots, so a child id hides its parent chain too
func excludedLineageRoots(ctx context.Context, db SessionSearchStore, exclude []string) map[string]bool {
	roots := map[string]bool{}
	for _, sid := range exclude {
		roots[sid] = true
		root := resolveLineage(ctx, db, sid)
		if root == "" {
			root = sid
		}
		roots[root] = true
	}
	return roots
}

// True when both sessions share a lineage root
func sameLineage(ctx context.Context, db SessionSearchStore, a, b string) bool {
	aRoot := resolveLineage(ctx, db, a)
	return aRoot != "" && aRoot == resolveLineage(ctx, db, b)
}

// True when the transcript left live context through compression or a fresh reset
func sessionLeftLiveContext(ctx context.Context, db SessionSearchStore, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	reason := oget(getSessionMeta(ctx, db, sessionID), "end_reason")
	if !pyrepr.Truthy(reason) {
		return false
	}
	text, ok := reason.(string)
	return ok && (text == "compression" || freshResetEndReasons[text])
}

// Owning session and visibility flags of a message id, nil when missing or on error
func getMessageStorageState(ctx context.Context, db SessionSearchStore, messageID any) *pyjson.Object {
	if !pyrepr.Truthy(messageID) {
		return nil
	}
	var sessionID string
	var active, compacted int64
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT session_id, active, compacted FROM messages WHERE id = ?", messageID).Scan(&sessionID, &active, &compacted)
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Debug(fmt.Sprintf("message storage-state lookup failed for %v", messageID))
		}
		return nil
	}
	out := pyjson.NewObject()
	out.Set("session_id", sessionID)
	out.Set("active", active)
	out.Set("compacted", compacted)
	return out
}

// True for a compaction archive row, active zero and compacted one
func isCompactedState(st *pyjson.Object) bool {
	if st == nil {
		return false
	}
	active, _ := sessionSearchInt(oget(st, "active"))
	compacted, _ := sessionSearchInt(oget(st, "compacted"))
	return active == 0 && compacted == 1
}

// True for a compaction archived row, False on any error
func isCompactedMessage(ctx context.Context, db SessionSearchStore, messageID any) bool {
	return isCompactedState(getMessageStorageState(ctx, db, messageID))
}

// Slim message row keeping content even when empty, the anchor flagged and long content capped
func shapeMessage(m *pyjson.Object, anchorID any, maxContentLen int) *pyjson.Object {
	content := oget(m, "content")
	if s, ok := content.(string); ok && strings.Contains(s, "\x1b") {
		content = StripANSI(s)
	}
	entry := pyjson.NewObject()
	entry.Set("id", oget(m, "id"))
	entry.Set("role", oget(m, "role"))
	entry.Set("content", content)
	entry.Set("timestamp", oget(m, "timestamp"))
	for _, k := range []string{"tool_name", "tool_calls", "tool_call_id"} {
		if v := oget(m, k); pyrepr.Truthy(v) {
			entry.Set(k, v)
		}
	}
	if anchorID != nil && sameInt(oget(m, "id"), anchorID) {
		entry.Set("anchor", true)
	}
	if s, ok := content.(string); ok && maxContentLen > 0 && s != "" && pystr.Len(s) > maxContentLen {
		entry.Set("content", pystr.Slice(s, 0, maxContentLen)+"…")
		entry.Set("content_truncated", true)
		entry.Set("original_content_chars", pystr.Len(s))
	}
	out := pyjson.NewObject()
	for _, k := range entry.Keys {
		if v := entry.Vals[k]; v != nil || k == "content" {
			out.Set(k, v)
		}
	}
	return out
}

// Session reference the agent writes, the profile segment only when it can be named
func sessionLink(ctx context.Context, sessionID string, profile any) string {
	name := pystr.Strip(ostr(profile))
	if name == "" {
		name = ProfileStoreHooks.ActiveName(ctx)
		if name == "custom" {
			name = ""
		}
	}
	if name != "" {
		return "@session:" + name + "/" + sessionID
	}
	return "@session:" + sessionID
}

// Discovery result fields in their canonical key order
type discoveryFields struct {
	sessionID      string
	when           string
	source         any
	model          any
	title          any
	matchedRole    any
	matchMessageID any
	snippet        string
	bookendStart   []any
	messages       []any
	bookendEnd     []any
	messagesBefore any
	messagesAfter  any
	detail         string
}

// Discovery entry in canonical key order, parent_session_id set when the hit lives in a child
func discoveryEntry(lineageRoot string, f discoveryFields) *pyjson.Object {
	entry := pyjson.NewObject()
	entry.Set("session_id", f.sessionID)
	entry.Set("when", f.when)
	entry.Set("source", f.source)
	entry.Set("model", f.model)
	entry.Set("title", f.title)
	entry.Set("matched_role", f.matchedRole)
	entry.Set("match_message_id", f.matchMessageID)
	entry.Set("snippet", f.snippet)
	entry.Set("bookend_start", f.bookendStart)
	entry.Set("messages", f.messages)
	entry.Set("bookend_end", f.bookendEnd)
	entry.Set("messages_before", f.messagesBefore)
	entry.Set("messages_after", f.messagesAfter)
	entry.Set("detail", f.detail)
	if lineageRoot != "" && lineageRoot != f.sessionID {
		entry.Set("parent_session_id", lineageRoot)
	}
	return entry
}

// Message objects of a view key, or the fallback rows when the key is empty
func viewRows(view *pyjson.Object, key string, fallback []*pyjson.Object) []*pyjson.Object {
	if rows, ok := oget(view, key).([]any); ok && len(rows) > 0 {
		out := make([]*pyjson.Object, 0, len(rows))
		for _, r := range rows {
			if o, ok := r.(*pyjson.Object); ok {
				out = append(out, o)
			}
		}
		return out
	}
	return fallback
}

// Shaped messages of a list as a JSON array value
func shapeAll(rows []*pyjson.Object, anchorID any, maxContentLen int) []any {
	out := make([]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, shapeMessage(m, anchorID, maxContentLen))
	}
	return out
}

// Discovery shaped result when the query matches a session title, nil otherwise
func titleMatchResult(ctx context.Context, db SessionSearchStore, query, currentLineageRoot string) *pyjson.Object {
	titleQuery := strings.Trim(pystr.Strip(query), "`'\"")
	if titleQuery == "" {
		return nil
	}
	sessionID, found, err := db.ResolveSessionByTitle(ctx, titleQuery)
	if err != nil {
		slog.Debug(fmt.Sprintf("resolve_session_by_title failed for %s", pyrepr.Repr(titleQuery)))
		return nil
	}
	if !found || sessionID == "" {
		return nil
	}
	lineageRoot := resolveLineage(ctx, db, sessionID)
	if currentLineageRoot != "" && lineageRoot == currentLineageRoot && !sessionLeftLiveContext(ctx, db, sessionID) {
		return nil
	}
	sessionMeta := pyjson.NewObject()
	if root, err := db.GetSession(ctx, lineageRoot); err != nil {
		slog.Debug(fmt.Sprintf("get_session failed for title match %s", sessionID))
	} else if root != nil {
		sessionMeta = root
	} else if own, err := db.GetSession(ctx, sessionID); err != nil {
		slog.Debug(fmt.Sprintf("get_session failed for title match %s", sessionID))
	} else if own != nil {
		sessionMeta = own
	}
	if source, ok := oget(sessionMeta, "source").(string); ok && containsString(sessionSearchHiddenSources, source) {
		return nil
	}
	messages, err := db.GetMessages(ctx, sessionID, state.GetMessagesOptions{})
	if err != nil {
		slog.Debug(fmt.Sprintf("get_messages failed for title match %s", sessionID))
		messages = nil
	}
	var anchorID any
	view := pyjson.NewObject()
	if len(messages) > 0 {
		anchorID = oget(messages[0], "id")
	}
	if anchorID != nil {
		if id, ok := sessionSearchInt(anchorID); ok {
			v, err := db.GetAnchoredView(ctx, sessionID, id, state.AnchoredViewOptions{Window: 5, Bookend: 3, KeepRoles: []string{"user", "assistant"}})
			if err != nil {
				slog.Debug(fmt.Sprintf("get_anchored_view failed for title match %s/%v", sessionID, anchorID))
			} else if v != nil {
				view = v
			}
		} else {
			slog.Debug(fmt.Sprintf("get_anchored_view failed for title match %s/%v", sessionID, anchorID))
		}
	}
	title := pyOr(oget(sessionMeta, "title"), titleQuery)
	head := messages[:min(3, len(messages))]
	window := messages[:min(5, len(messages))]
	tail := messages[max(0, len(messages)-3):]
	entry := discoveryEntry(lineageRoot, discoveryFields{
		sessionID:      sessionID,
		when:           formatTimestamp(oget(sessionMeta, "started_at")),
		source:         ogetDefault(sessionMeta, "source", "unknown"),
		model:          pyOr(oget(sessionMeta, "model"), "unknown"),
		title:          title,
		matchedRole:    "session_title",
		matchMessageID: anchorID,
		snippet:        "Session title matched: " + ostr(title),
		bookendStart:   shapeAll(viewRows(view, "bookend_start", head), nil, 1200),
		messages:       shapeAll(viewRows(view, "window", window), anchorID, 4000),
		bookendEnd:     shapeAll(viewRows(view, "bookend_end", tail), nil, 1200),
		messagesBefore: ogetDefault(view, "messages_before", int64(0)),
		messagesAfter:  ogetDefault(view, "messages_after", int64(max(len(messages)-5, 0))),
		detail:         "full",
	})
	entry.Set("_lineage_root", lineageRoot)
	return entry
}

// Decimal digits of an int with thousands separators like the Python comma format
func thousands(v any) string {
	n, err := pyint.Of(v)
	if err != nil {
		return pyrepr.Str(v)
	}
	digits := n.String()
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var out strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(r)
	}
	return sign + out.String()
}

// Discovery response, the FTS rebuild progress noted so thin results are explained
func discoverPayload(ctx context.Context, db SessionSearchStore, query, detail string, results []*pyjson.Object, extra *pyjson.Object) string {
	status, err := db.FTSRebuildStatus(ctx)
	if err != nil {
		slog.Debug("fts_rebuild_status failed", "err", err)
		status = nil
	}
	payload := pyjson.NewObject()
	payload.Set("mode", "discover")
	payload.Set("query", query)
	payload.Set("detail", detail)
	list := make([]any, 0, len(results))
	for _, r := range results {
		list = append(list, r)
	}
	payload.Set("results", list)
	payload.Set("count", len(results))
	for _, k := range extra.Keys {
		payload.Set(k, extra.Vals[k])
	}
	if status != nil {
		rebuild := pyjson.NewObject()
		rebuild.Set("percent", oget(status, "percent"))
		rebuild.Set("note", fmt.Sprintf("The search index is rebuilding in the background (%s%% done, %s of %s messages). "+
			"Results from older messages may be incomplete until it finishes.",
			pyrepr.Str(oget(status, "percent")), thousands(oget(status, "indexed")), thousands(oget(status, "total"))))
		payload.Set("index_rebuild", rebuild)
	}
	return sessionSearchOK(payload)
}

// Bookend messages of a view capped at 1200 characters with compaction summaries dropped
func bookend(view *pyjson.Object, key string) []any {
	out := []any{}
	for _, m := range viewRows(view, key, nil) {
		if isCompactionSummary(ogetDefault(m, "content", "")) {
			continue
		}
		out = append(out, shapeMessage(m, nil, 1200))
	}
	return out
}

// Discovery result of a surviving FTS row, nil when its anchored view cannot load
func hydrateHit(ctx context.Context, db SessionSearchStore, lineageRoot string, matchInfo *pyjson.Object, resultDetail string) *pyjson.Object {
	hitSID := ostr(pyOr(oget(matchInfo, "session_id"), lineageRoot))
	msgID := oget(matchInfo, "id")
	id, ok := sessionSearchInt(msgID)
	var view *pyjson.Object
	var err error
	if ok {
		view, err = db.GetAnchoredView(ctx, hitSID, id, state.AnchoredViewOptions{Window: 5, Bookend: 3, KeepRoles: []string{"user", "assistant"}})
	} else {
		err = fmt.Errorf("anchor id %v is not an integer", msgID)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("get_anchored_view failed for %s/%v: %v", hitSID, msgID, err))
		return nil
	}
	if view == nil {
		view = pyjson.NewObject()
	}
	sessionMeta, full := getSessionMeta(ctx, db, lineageRoot), resultDetail == "full"
	messages := []any{}
	for _, m := range viewRows(view, "window", nil) {
		if full || sameInt(oget(m, "id"), msgID) {
			messages = append(messages, shapeMessage(m, msgID, 4000))
		}
	}
	bookendStart, bookendEnd := []any{}, []any{}
	if full {
		bookendStart, bookendEnd = bookend(view, "bookend_start"), bookend(view, "bookend_end")
	}
	return discoveryEntry(lineageRoot, discoveryFields{
		sessionID:      hitSID,
		when:           formatTimestamp(pyOr(oget(sessionMeta, "started_at"), oget(matchInfo, "session_started"))),
		source:         pyOr(oget(sessionMeta, "source"), ogetDefault(matchInfo, "source", "unknown")),
		model:          pyOr(pyOr(oget(sessionMeta, "model"), oget(matchInfo, "model")), "unknown"),
		title:          pyOr(oget(sessionMeta, "title"), nil),
		matchedRole:    oget(matchInfo, "role"),
		matchMessageID: msgID,
		snippet:        ostr(pyOr(oget(matchInfo, "snippet"), "")),
		bookendStart:   bookendStart,
		messages:       messages,
		bookendEnd:     bookendEnd,
		messagesBefore: ogetDefault(view, "messages_before", int64(0)),
		messagesAfter:  ogetDefault(view, "messages_after", int64(0)),
		detail:         resultDetail,
	})
}

// One lineage root of the discovery dedup, the title hit or the first surviving FTS row
type seenSession struct {
	match     *pyjson.Object
	titleOnly bool
}

// Discovery shape, FTS plus adaptive or full result hydration
func discover(ctx context.Context, db SessionSearchStore, query string, roleFilter []string, limit int, sortOrder string,
	detail, currentSessionID string, linkProfile any, afterTS, beforeTS *sessionSearchBound, excludeSessionIDs []string) string {
	currentLineageRoot := ""
	if currentSessionID != "" {
		currentLineageRoot = resolveLineage(ctx, db, currentSessionID)
	}
	excludedRoots := excludedLineageRoots(ctx, db, excludeSessionIDs)
	titleResult := titleMatchResult(ctx, db, query, currentLineageRoot)
	if titleResult != nil {
		titleSID := ostr(oget(titleResult, "session_id"))
		titleRoot := ostr(pyOr(oget(titleResult, "_lineage_root"), titleSID))
		rootMeta := getSessionMeta(ctx, db, titleRoot)
		if rootMeta.Len() == 0 {
			rootMeta = getSessionMeta(ctx, db, titleSID)
		}
		titleStarted := coerceStartedTS(oget(rootMeta, "started_at"))
		if excludedRoots[titleSID] || excludedRoots[titleRoot] || !inTimeWindow(titleStarted, afterTS, beforeTS) {
			titleResult = nil
		}
	}
	if roleFilter == nil {
		roleFilter = []string{"user", "assistant"}
	}
	opts := state.NewSearchMessagesOptions()
	opts.RoleFilter = roleFilter
	opts.ExcludeSources = append([]string(nil), sessionSearchHiddenSources...)
	opts.Limit = discoverScanLimit
	opts.Offset = 0
	opts.Sort = sortOrder
	opts.Fields = append([]string(nil), discoverSearchFields...)
	if afterTS != nil {
		v := afterTS.value
		opts.AfterTS = &v
	}
	if beforeTS != nil {
		v := beforeTS.value
		opts.BeforeTS = &v
	}
	rawResults, errJSON := loud(func() ([]*pyjson.Object, error) {
		if (afterTS != nil && afterTS.overflow) || (beforeTS != nil && beforeTS.overflow) {
			return nil, errors.New("Python int too large to convert to SQLite INTEGER")
		}
		return db.SearchMessages(ctx, query, opts)
	}, "FTS5 search failed: %v", "Search failed")
	if errJSON != "" {
		return errJSON
	}
	sort.SliceStable(rawResults, func(i, j int) bool {
		di := sessionSearchDemotedSources[ostr(pyOr(oget(rawResults[i], "source"), ""))]
		dj := sessionSearchDemotedSources[ostr(pyOr(oget(rawResults[j], "source"), ""))]
		return !di && dj
	})
	if len(rawResults) == 0 && titleResult == nil {
		extra := pyjson.NewObject()
		extra.Set("message", "No matching sessions found. FTS5 ANDs all terms by default — "+
			"broaden with OR (`alpha OR beta`), exact-match with quoted "+
			"phrases, exclude with NOT, or prefix-match with `deploy*`.")
		return discoverPayload(ctx, db, query, detail, nil, extra)
	}
	seenOrder := []string{}
	seen := map[string]seenSession{}
	results := []*pyjson.Object{}
	if titleResult != nil {
		results = append(results, titleResult)
		if titleLineage, _ := titleResult.Pop("_lineage_root"); pyrepr.Truthy(titleLineage) {
			seenOrder = append(seenOrder, ostr(titleLineage))
			seen[ostr(titleLineage)] = seenSession{titleOnly: true}
		}
	}
	for _, r := range rawResults {
		if len(seen) >= limit {
			break
		}
		rawSID := ostr(oget(r, "session_id"))
		resolvedSID := resolveLineage(ctx, db, rawSID)
		if excludedRoots[rawSID] || excludedRoots[resolvedSID] {
			continue
		}
		isCompactedHit := isCompactedMessage(ctx, db, oget(r, "id"))
		if currentLineageRoot != "" && resolvedSID == currentLineageRoot && !(sessionLeftLiveContext(ctx, db, rawSID) || isCompactedHit) {
			continue
		}
		if currentSessionID != "" && rawSID == currentSessionID && !isCompactedHit {
			continue
		}
		if _, dup := seen[resolvedSID]; !dup {
			match := r.Copy()
			match.Set("_lineage_root", resolvedSID)
			seenOrder = append(seenOrder, resolvedSID)
			seen[resolvedSID] = seenSession{match: match}
		}
	}
	for _, lineageRoot := range seenOrder {
		entry := seen[lineageRoot]
		if entry.titleOnly {
			continue
		}
		resultDetail := "compact"
		if detail == "full" || len(results) == 0 {
			resultDetail = "full"
		}
		if hit := hydrateHit(ctx, db, lineageRoot, entry.match, resultDetail); hit != nil {
			results = append(results, hit)
		}
	}
	for _, entry := range results {
		entry.Set("link", sessionLink(ctx, ostr(oget(entry, "session_id")), linkProfile))
	}
	extra := pyjson.NewObject()
	extra.Set("sessions_searched", len(seen))
	extra.Set("link_hint", "When referring the user to a session, write its `link` value "+
		"verbatim inline mid-sentence (it renders as a titled link) — never "+
		"as markdown, in backticks, on its own line, or next to the "+
		"title/id/date. To read more around a compact result, scroll: "+
		"session_search(session_id=..., around_message_id=match_message_id).")
	return discoverPayload(ctx, db, query, detail, results, extra)
}

// Another profile's state database opened read only, nil for the current profile
func resolveProfileDB(ctx context.Context, profile any) (SessionSearchStore, error) {
	if profile == nil || pystr.Strip(pyrepr.Str(profile)) == "" {
		return nil, nil
	}
	canon, err := ProfileStoreHooks.Normalize(pyrepr.Str(profile))
	if err != nil {
		return nil, err
	}
	if err := ProfileStoreHooks.Validate(canon); err != nil {
		return nil, err
	}
	if !ProfileStoreHooks.Exists(canon) {
		return nil, fmt.Errorf("profile '%s' does not exist", canon)
	}
	dir, err := ProfileStoreHooks.Dir(canon)
	if err != nil {
		return nil, err
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(dir, "state.db"), true)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Seam over the profile database resolver for tests that swap in a fake store
var resolveProfileDBFn = resolveProfileDB

// Seam over the read only default store open, the hermes SessionDB constructor
var sessionSearchOpenDefaultFn = func(ctx context.Context) (SessionSearchStore, error) {
	db, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), true)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Seam over release_or_close, a registry held database released and any other store closed
var sessionSearchReleaseFn = func(db SessionSearchStore) error {
	if real, ok := db.(*state.DB); ok {
		state.ReleaseOrClose(real)
		return nil
	}
	return db.Close()
}

// Read shape, the whole session or head plus tail messages with a scroll pointer
func readSession(ctx context.Context, db SessionSearchStore, sessionID string, head, tail int, linkProfile any) string {
	meta := getSessionMeta(ctx, db, sessionID)
	if meta.Len() == 0 {
		return sessionSearchError("session_id not found: " + sessionID)
	}
	rows, errJSON := loud(func() ([]*pyjson.Object, error) { return db.GetMessages(ctx, sessionID, state.GetMessagesOptions{}) },
		"get_messages failed for %s: %v", "failed to load session", sessionID)
	if errJSON != "" {
		return errJSON
	}
	shaped := shapeAll(rows, nil, sessionSearchReadMaxContent)
	total := len(shaped)
	truncated := total > head+tail
	payload := pyjson.NewObject()
	payload.Set("mode", "read")
	payload.Set("session_id", sessionID)
	payload.Set("link", sessionLink(ctx, sessionID, linkProfile))
	payload.Set("session_meta", sessionMetaBlock(meta))
	payload.Set("message_count", total)
	payload.Set("truncated", truncated)
	if truncated {
		payload.Set("messages", append(append([]any{}, shaped[:head]...), shaped[total-tail:]...))
		payload.Set("message", fmt.Sprintf("Session has %d messages; showing first %d + last %d. "+
			"Pass around_message_id (any id above) to scroll the middle.", total, head, tail))
	} else {
		payload.Set("messages", shaped)
	}
	return sessionSearchOK(payload)
}

// Read shape scoped to one store, a bare id never falling through to another profile
func readScoped(ctx context.Context, db SessionSearchStore, sid string, profile any) string {
	result := readSession(ctx, db, sid, 20, 10, profile)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		panic(fmt.Sprintf("session_search read result is not JSON: %v", err))
	}
	if success, ok := decoded["success"].(bool); !(ok && !success) || pyrepr.Truthy(profile) {
		return result
	}
	return sessionSearchError(fmt.Sprintf("session_id not found in this profile: %s. If it belongs to another "+
		"profile, pass profile=<name> (or the @session:<profile>/<id> link).", sid))
}

// Browse shape, metadata of the most recent sessions through the bounded query only
func listRecentSessions(ctx context.Context, db SessionSearchStore, limit int, currentSessionID string, linkProfile any) string {
	browse := func() (string, error) {
		bounded, ok := db.(boundedRecentSessionLister)
		if !ok {
			return "", errors.New("session database does not support bounded recent-session browse")
		}
		want := limit + 15
		timeout := 3.0
		sessions, err := bounded.ListRecentSessionsBounded(ctx, state.RecentSessionsBoundedOptions{
			Limit: &want, ExcludeSources: append([]string(nil), sessionSearchHiddenSources...), TimeoutSeconds: &timeout})
		if err != nil {
			return "", err
		}
		hidden := map[string]bool{}
		if currentSessionID != "" {
			hidden[currentSessionID] = true
			if root, hop := resolveToParent(ctx, db, currentSessionID); hop && root != "" {
				hidden[root] = true
			}
		}
		results := []any{}
		for _, s := range sessions {
			id := ostr(ogetDefault(s, "id", ""))
			if hidden[id] {
				continue
			}
			if len(results) >= limit {
				break
			}
			row := pyjson.NewObject()
			row.Set("session_id", id)
			row.Set("link", sessionLink(ctx, id, linkProfile))
			row.Set("title", pyOr(oget(s, "title"), nil))
			for _, k := range []string{"source", "started_at", "last_active"} {
				row.Set(k, ogetDefault(s, k, ""))
			}
			row.Set("message_count", ogetDefault(s, "message_count", int64(0)))
			row.Set("preview", ogetDefault(s, "preview", ""))
			results = append(results, row)
		}
		payload := pyjson.NewObject()
		payload.Set("mode", "browse")
		payload.Set("results", results)
		payload.Set("count", len(results))
		payload.Set("message", fmt.Sprintf("Showing %d most recent sessions. Pass a query= to search, "+
			"or session_id+around_message_id to scroll.", len(results)))
		return sessionSearchOK(payload), nil
	}
	out, errJSON := loud(browse, "Error listing recent sessions: %v", "Failed to list recent sessions")
	if errJSON != "" {
		return errJSON
	}
	return out
}

// Python int of a value clamped into the bounds, the default where int raises
func sessionSearchClamp(value any, def, lo, hi int) int {
	n, ok := sessionSearchInt(value)
	if !ok {
		return def
	}
	if n < int64(lo) {
		return lo
	}
	if n > int64(hi) {
		return hi
	}
	return int(n)
}

// True when a scroll anchor is still in the caller's active context and must be rejected
func anchorInLiveContext(ctx context.Context, db SessionSearchStore, anchorState *pyjson.Object, anchorSID, currentSessionID string) bool {
	if !sameLineage(ctx, db, anchorSID, currentSessionID) || isCompactedState(anchorState) {
		return false
	}
	if anchorState != nil {
		if active, _ := sessionSearchInt(oget(anchorState, "active")); active == 0 {
			return true
		}
	}
	return !sessionLeftLiveContext(ctx, db, anchorSID)
}

// Scroll shape, a window centered on an anchor with no FTS and no bookends
func scroll(ctx context.Context, db SessionSearchStore, sessionID string, aroundMessageID any, window any, currentSessionID string) string {
	anchor, ok := sessionSearchInt(aroundMessageID)
	if !ok {
		return sessionSearchError("scroll requires integer around_message_id")
	}
	win := sessionSearchClamp(window, 5, 1, 20)
	anchorState := getMessageStorageState(ctx, db, anchor)
	owning := ostr(oget(anchorState, "session_id"))
	anchorSID := owning
	if anchorSID == "" {
		anchorSID = sessionID
	}
	if currentSessionID != "" && anchorInLiveContext(ctx, db, anchorState, anchorSID, currentSessionID) {
		return sessionSearchError("scroll rejected: anchor lives in the current session lineage (already in your active context)")
	}
	sessionMeta := getSessionMeta(ctx, db, sessionID)
	if sessionMeta.Len() == 0 {
		return sessionSearchError("session_id not found: " + sessionID)
	}
	view, errJSON := loud(func() (*pyjson.Object, error) { return db.GetMessagesAround(ctx, sessionID, anchor, win) },
		"get_messages_around failed: %v", "failed to load messages")
	if errJSON != "" {
		return errJSON
	}
	var err error
	if view == nil {
		view = pyjson.NewObject()
	}
	messages := viewRows(view, "window", nil)
	extra := pyjson.NewObject()
	if len(messages) == 0 && owning != "" && owning != sessionID {
		var rebind *pyjson.Object
		if sameLineage(ctx, db, sessionID, owning) {
			if rebind, err = db.GetMessagesAround(ctx, owning, anchor, win); err != nil {
				slog.Debug(fmt.Sprintf("rebind get_messages_around failed: %v", err))
				rebind = nil
			}
		}
		if rows := viewRows(rebind, "window", nil); rebind != nil && len(rows) > 0 {
			extra.Set("warning", fmt.Sprintf("around_message_id %d lives in %s (child of %s); rebound transparently", anchor, owning, sessionID))
			view, messages, sessionID = rebind, rows, owning
			if meta := getSessionMeta(ctx, db, owning); meta.Len() > 0 {
				sessionMeta = meta
			}
		}
	}
	if len(messages) == 0 {
		return sessionSearchError(fmt.Sprintf("around_message_id %d not in session_id %s", anchor, sessionID))
	}
	payload := pyjson.NewObject()
	payload.Set("mode", "scroll")
	payload.Set("session_id", sessionID)
	payload.Set("around_message_id", anchor)
	payload.Set("session_meta", sessionMetaBlock(sessionMeta))
	payload.Set("window", win)
	payload.Set("messages", shapeAll(messages, anchor, 0))
	payload.Set("messages_before", ogetDefault(view, "messages_before", int64(0)))
	payload.Set("messages_after", ogetDefault(view, "messages_after", int64(0)))
	payload.Set("hint", "Scroll forward: re-call with around_message_id = the LAST message's "+
		"id; backward: the FIRST message's id (the boundary message repeats "+
		"as an orientation marker). messages_before/messages_after < window "+
		"means you've hit that end of the session.")
	for _, k := range extra.Keys {
		payload.Set(k, extra.Vals[k])
	}
	return sessionSearchOK(payload)
}

// Mode dispatch, scroll when an anchor is set, read for a bare session id, browse without a query
func sessionSearchDispatch(ctx context.Context, a SessionSearchArgs, db SessionSearchStore, currentSessionID string) string {
	if sid, ok := a.SessionID.(string); ok && pystr.Strip(sid) != "" {
		if a.AroundMessageID != nil {
			return scroll(ctx, db, pystr.Strip(sid), a.AroundMessageID, a.Window, currentSessionID)
		}
		return readScoped(ctx, db, pystr.Strip(sid), a.Profile)
	}
	query, isText := a.Query.(string)
	browse := !pyrepr.Truthy(a.Query) || !isText || pystr.Strip(query) == ""
	def := 3
	if browse {
		def = 10
	}
	limit := sessionSearchClamp(a.Limit, def, 1, 10)
	if browse {
		return listRecentSessions(ctx, db, limit, currentSessionID, a.Profile)
	}
	sortOrder := ""
	if s, ok := a.Sort.(string); ok {
		norm := pystr.Lower(pystr.Strip(s))
		if norm == "newest" || norm == "oldest" {
			sortOrder = norm
		}
	}
	afterTS, err := parseISOBound(a.After)
	if err != nil {
		return sessionSearchError(err.Error())
	}
	beforeTS, err := parseISOBound(a.Before)
	if err != nil {
		return sessionSearchError(err.Error())
	}
	var roleFilter []string
	if rf, ok := a.RoleFilter.(string); ok {
		for _, r := range strings.Split(rf, ",") {
			if r = pystr.Strip(r); r != "" {
				roleFilter = append(roleFilter, r)
			}
		}
	}
	detail := "adaptive"
	if d, ok := a.Detail.(string); ok && pystr.Lower(pystr.Strip(d)) == "full" {
		detail = "full"
	}
	return discover(ctx, db, pystr.Strip(query), roleFilter, limit, sortOrder, detail, currentSessionID, a.Profile,
		afterTS, beforeTS, normalizeExcludeSessionIDs(a.ExcludeSessionIDs))
}

// SessionSearch runs one session_search call, closing every database it opened itself
func SessionSearch(ctx context.Context, a SessionSearchArgs) string {
	var owned []SessionSearchStore
	if sid, ok := a.SessionID.(string); ok && strings.Contains(sid, "/") {
		embProfile, embID, _ := strings.Cut(sid, "/")
		if embID != "" {
			a.SessionID = embID
			if embProfile != "" && (a.Profile == nil || pystr.Strip(pyrepr.Str(a.Profile)) == "") {
				a.Profile = embProfile
			}
		}
	}
	profileDB, err := resolveProfileDBFn(ctx, a.Profile)
	if err != nil {
		slog.Debug(fmt.Sprintf("session_search: cannot open profile %s", pyrepr.Repr(a.Profile)), "err", err)
		return sessionSearchError(fmt.Sprintf("profile '%s': %v", pyrepr.Str(a.Profile), err))
	}
	db, currentSessionID := a.DB, a.CurrentSessionID
	if profileDB != nil {
		db, currentSessionID = profileDB, ""
		owned = append(owned, profileDB)
	}
	if db == nil {
		opened, err := sessionSearchOpenDefaultFn(ctx)
		if err != nil {
			slog.Debug("SessionDB unavailable for session_search", "err", err)
			return sessionSearchError(state.FormatSessionDBUnavailable(ctx, state.SessionDBUnavailablePrefix, false))
		}
		db = opened
		owned = append(owned, opened)
	}
	defer func() {
		for i := len(owned) - 1; i >= 0; i-- {
			if err := sessionSearchReleaseFn(owned[i]); err != nil {
				slog.Debug("Failed to close session_search SessionDB", "err", err)
			}
		}
	}()
	return sessionSearchDispatch(ctx, a, db, currentSessionID)
}

// Requires the SQLite state database directory of the active home
func checkSessionSearchRequirements(env *Env) bool {
	ctx := context.Background()
	if env != nil && strings.TrimSpace(env.Home) != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	_, err := os.Stat(filepath.Dir(state.ResolveDefaultDBPath(ctx)))
	return err == nil
}

// Argument value with a default when the key is absent, a present nil staying nil
func argDefault(args map[string]any, key string, def any) any {
	if v, ok := args[key]; ok {
		return v
	}
	return def
}

// session_search arguments of a registry call, the handler kwargs mapped like the hermes lambda
func sessionSearchArgsOf(args map[string]any, db *state.DB, currentSessionID string) SessionSearchArgs {
	a := SessionSearchArgs{
		Query:             pyOr(args["query"], ""),
		Limit:             args["limit"],
		Window:            argDefault(args, "window", 5),
		Detail:            argDefault(args, "detail", "adaptive"),
		CurrentSessionID:  currentSessionID,
		RoleFilter:        args["role_filter"],
		SessionID:         args["session_id"],
		AroundMessageID:   args["around_message_id"],
		Sort:              args["sort"],
		Profile:           args["profile"],
		After:             args["after"],
		Before:            args["before"],
		ExcludeSessionIDs: args["exclude_session_ids"],
	}
	if db != nil {
		a.DB = db
	}
	return a
}

// Inline executor over the agent's recall database, the unavailable notice when it has none
func sessionSearchInlineExecutor(ctx context.Context, host ExecutorHost, args map[string]any, _ InlineToolContext) (string, error) {
	db := host.SessionDBForRecall(ctx)
	if db == nil {
		out := pyjson.NewObject()
		out.Set("success", false)
		out.Set("error", state.FormatSessionDBUnavailable(ctx, state.SessionDBUnavailablePrefix, false))
		text, err := pyjson.Dumps(out)
		if err != nil {
			return "", err
		}
		return text, nil
	}
	a := sessionSearchArgsOf(args, db, host.SessionID())
	a.Query = argDefault(args, "query", "")
	return SessionSearch(ctx, a), nil
}

var sessionSearchSchema = map[string]any{
	"name": "session_search",
	"description": "Search or read past Icebath conversations (FTS5), or scroll inside one. Four shapes, picked by args: `query` = discovery " +
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
		"inline (it renders as a titled link).",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "Search query (discovery shape). Keywords, phrases, or boolean " +
					"expressions to find in past sessions. Omit to browse recent " +
					"sessions. Ignored when session_id + around_message_id are set " +
					"(scroll shape).",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": "Max sessions to return. Discovery defaults to 3; browse/no-args " +
					"defaults to 10. Max 10. Bump discovery to 5–10 when the topic " +
					"likely spans several sessions and you want to pick the right one " +
					"to scroll into.",
			},
			"sort": map[string]any{
				"type": "string",
				"enum": []any{"newest", "oldest"},
				"description": "Discovery shape only. Temporal bias on top of FTS5 ranking: omit " +
					"for relevance-only (exploratory recall), 'newest' for " +
					"\"where did we leave X\", 'oldest' for \"how did X start\".",
			},
			"detail": map[string]any{
				"type": "string",
				"enum": []any{"adaptive", "full"},
				"description": "Discovery shape only. 'adaptive' (default) fully hydrates the " +
					"top-ranked result and returns only the exact anchor message for " +
					"lower-ranked results. 'full' returns bookends and the complete " +
					"anchored window for every result.",
				"default": "adaptive",
			},
			"after": map[string]any{
				"type": "string",
				"description": "Discovery shape only. Inclusive lower bound on session start " +
					"time. ISO date/datetime (e.g. 2026-06-01) or relative duration " +
					"(7d, 24h, 2w = within the last N). Use only when the user names " +
					"a time frame. sort is a ranking bias, not a bound.",
			},
			"before": map[string]any{
				"type": "string",
				"description": "Discovery shape only. Exclusive upper bound on session start " +
					"time. ISO date/datetime (a date-only value is midnight UTC that " +
					"day) or relative duration (7d = older than a week). Use only " +
					"when the user names a time frame.",
			},
			"exclude_session_ids": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Discovery shape only. Session ids already inspected this task. " +
					"Those sessions and their lineage are omitted so a later query " +
					"explores instead of repeating the same hit. Cap 20.",
			},
			"session_id": map[string]any{
				"type": "string",
				"description": "Scroll shape. Session to read inside. Use the session_id returned " +
					"from a prior discovery call. Must be paired with " +
					"around_message_id.",
			},
			"around_message_id": map[string]any{
				"type": "integer",
				"description": "Scroll shape. Message id to center the window on — use " +
					"match_message_id from a discovery result, or any id from a " +
					"prior window.",
			},
			"window": map[string]any{
				"type": "integer",
				"description": "Scroll shape only. Messages to return on each side of the anchor " +
					"(anchor itself always included). Clamped to [1, 20]. Default 5.",
				"default": 5,
			},
			"role_filter": map[string]any{
				"type": "string",
				"description": "Optional. Comma-separated roles to include. Discovery defaults to " +
					"'user,assistant' (tool output is usually noise). Pass " +
					"'user,assistant,tool' to include tool output (debugging tool " +
					"behaviour) or 'tool' to search tool output only.",
			},
			"profile": map[string]any{
				"type": "string",
				"description": "Optional. Read sessions from another icebath profile's database " +
					"(read-only). Use when resolving an `@session:<profile>/<id>` link: " +
					"pass the profile segment here with session_id as the id segment. " +
					"Omit to use the current profile.",
			},
		},
		"required": []any{},
	},
}

// Registers the session_search tool and its inline executor over the agent recall database
func registerSessionSearchTool() {
	Register(Def{Name: "session_search", Toolset: "session_search", Schema: sessionSearchSchema, Emoji: "🔍",
		Check: checkSessionSearchRequirements,
		Handler: func(ctx context.Context, call *Call) string {
			var db *state.DB
			if call.Env != nil {
				db = call.Env.State
			}
			return SessionSearch(ctx, sessionSearchArgsOf(call.Args, db, call.SessionID))
		}})
	RegisterInlineToolExecutor("session_search", sessionSearchInlineExecutor)
}

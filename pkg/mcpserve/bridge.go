package mcpserve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
	_ "modernc.org/sqlite"
)

// Events the bridge queue keeps, the oldest dropped past it
const QueueLimit = 1000

// Wait between two database polls
const pollInterval = 200 * time.Millisecond

func sessionsDir(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "sessions") }

// PRAGMA data_version of state.db on a private read only connection, moving on every commit of another connection
type stateDBVersion struct {
	mu   sync.Mutex
	path string
	db   *sql.DB
	conn *sql.Conn
}

// Current data_version, false when state.db is absent or unreadable so the caller polls unconditionally
func (g *stateDBVersion) read(ctx context.Context) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := filepath.Join(home.Dir(ctx), "state.db")
	if g.conn == nil || g.path != path {
		g.closeLocked()
		if _, err := os.Stat(path); err != nil {
			return 0, false
		}
		resolved, err := filepath.Abs(path)
		if err != nil {
			return 0, false
		}
		if real, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = real
		}
		db, err := sql.Open("sqlite", urlutil.FileURL(resolved)+"?mode=ro")
		if err != nil {
			return 0, false
		}
		conn, err := db.Conn(context.Background())
		if err != nil {
			db.Close()
			return 0, false
		}
		g.db, g.conn, g.path = db, conn, path
	}
	var version int64
	if err := g.conn.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&version); err != nil {
		g.closeLocked()
		return 0, false
	}
	return version, true
}

func (g *stateDBVersion) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeLocked()
}

func (g *stateDBVersion) closeLocked() {
	if g.conn != nil {
		g.conn.Close()
	}
	if g.db != nil {
		g.db.Close()
	}
	g.db, g.conn, g.path = nil, nil, ""
}

// Parsed JSON file, an empty dict when missing or unreadable
func readJSON(path string) any {
	if _, err := os.Stat(path); err != nil {
		return jsonutil.NewObject()
	}
	data, err := os.ReadFile(path)
	if err == nil && !utf8.Valid(data) {
		err = errors.New("'utf-8' codec can't decode the file")
	}
	var parsed any
	if err == nil {
		parsed, err = jsonutil.Decode(string(data))
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to load %s: %s", filepath.Base(path), err.Error()))
		return jsonutil.NewObject()
	}
	return parsed
}

// Shared session database for reading transcripts, nil when it cannot open
func getSessionDB(ctx context.Context) *state.DB {
	db, err := state.Acquire(ctx, "")
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("SessionDB unavailable: %s", err.Error()))
		return nil
	}
	return db
}

// Closes a session database handle, logging a failure at debug
func closeQuietly(db *state.DB, what string) {
	if err := db.Close(); err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to close MCP %s SessionDB", what), "error", err)
	}
}

// Messages of one session, anything raised by the read returned as the error
func getMessages(ctx context.Context, db *state.DB, sessionID string) (msgs []*jsonutil.Object, err error) {
	defer func() {
		if r := recover(); r != nil {
			msgs, err = nil, errors.New(raisedText(r))
		}
	}()
	return db.GetMessages(ctx, sessionID, state.GetMessagesOptions{})
}

// Messages of one session or the error text, releasing the database handle either way
func loadSessionMessages(ctx context.Context, sessionID string) ([]*jsonutil.Object, string) {
	db := getSessionDB(ctx)
	if db == nil {
		return nil, "Session database unavailable"
	}
	defer state.ReleaseOrClose(db)
	msgs, err := getMessages(ctx, db, sessionID)
	if err != nil {
		return nil, "Failed to read messages: " + err.Error()
	}
	return msgs, ""
}

// Gateway routing index of session_key to entry, from state.db rows and else the legacy sessions.json
func loadSessionsIndex(ctx context.Context) *jsonutil.Object {
	if idx := loadSessionsIndexFromDB(ctx); idx.Len() > 0 {
		return idx
	}
	return loadSessionsIndexFromJSON(ctx)
}

// Local RFC 3339 time of epoch seconds to the microsecond, empty when unset, not a number or outside the years 1 to 9999
func isoTime(ts any) string {
	if !anyutil.IsTrue(ts) {
		return ""
	}
	f, err := anyutil.ToFloat(ts)
	if err != nil {
		return ""
	}
	t := timeutil.FromEpoch(f)
	if t.IsZero() {
		return ""
	}
	return t.Format(timeutil.RFC3339Micro)
}

// Converts a state.db gateway session row to the sessions.json entry shape
func rowToIndexEntry(row *jsonutil.Object) *jsonutil.Object {
	origin := jsonutil.NewObject()
	if raw := pyGet(row, "origin_json", nil); anyutil.IsTrue(raw) {
		var parsed any
		var err error
		switch t := raw.(type) {
		case string:
			parsed, err = jsonutil.Decode(t)
		case []byte:
			parsed, err = jsonutil.Decode(t)
		default:
			err = errors.New("the JSON object must be str, bytes or bytearray")
		}
		if d, ok := asDict(parsed); err == nil && ok {
			origin = d
		}
	}
	if origin.Len() == 0 {
		origin = jsonutil.ObjectOf("platform", pyGet(row, "source", ""))
		for _, k := range []string{"chat_id", "chat_type", "thread_id", "user_id"} {
			origin.Set(k, pyGet(row, k, nil))
		}
	}
	tokens := func(key string) int64 {
		v := pyGet(row, key, nil)
		if !anyutil.IsTrue(v) {
			return 0
		}
		n, err := anyutil.ToInt(v)
		if err != nil {
			panic(err)
		}
		return n
	}
	inputTokens, outputTokens := tokens("input_tokens"), tokens("output_tokens")
	chatType := pyGet(row, "chat_type", nil)
	if !anyutil.IsTrue(chatType) {
		chatType = pyGet(origin, "chat_type", "")
	}
	displayName := pyGet(row, "display_name", nil)
	if !anyutil.IsTrue(displayName) {
		displayName = pyGet(origin, "chat_name", nil)
	}
	if !anyutil.IsTrue(displayName) {
		displayName = ""
	}
	lastActive := pyGet(row, "last_active", nil)
	if !anyutil.IsTrue(lastActive) {
		lastActive = pyGet(row, "started_at", nil)
	}
	return jsonutil.ObjectOf(
		"session_id", anyutil.String(pyGet(row, "id", "")), "session_key", pyGet(row, "session_key", ""),
		"platform", pyGet(row, "source", ""),
		"chat_type", chatType,
		"display_name", displayName,
		"origin", origin,
		"created_at", isoTime(pyGet(row, "started_at", nil)),
		"updated_at", isoTime(lastActive),
		"input_tokens", inputTokens, "output_tokens", outputTokens,
		"total_tokens", inputTokens+outputTokens,
	)
}

// Routing index built from the open gateway session rows of state.db, empty when it cannot be read
func loadSessionsIndexFromDB(ctx context.Context) (index *jsonutil.Object) {
	db := getSessionDB(ctx)
	if db == nil {
		return jsonutil.NewObject()
	}
	defer db.Close()
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("Failed to load gateway sessions from state.db: %s", raisedText(r)))
			index = jsonutil.NewObject()
		}
	}()
	rows, err := db.ListGatewaySessions(ctx, "", true)
	if err != nil {
		panic(err)
	}
	out := jsonutil.NewObject()
	for _, row := range rows {
		key := pyGet(row, "session_key", nil)
		if !anyutil.IsTrue(key) {
			continue
		}
		out.Set(anyutil.String(key), rowToIndexEntry(row))
	}
	return out
}

// Legacy sessions.json index with the underscore prefixed metadata sentinels left out
func loadSessionsIndexFromJSON(ctx context.Context) *jsonutil.Object {
	out := jsonutil.NewObject()
	data, ok := readJSON(filepath.Join(sessionsDir(ctx), "sessions.json")).(*jsonutil.Object)
	if !ok {
		return out
	}
	for _, k := range data.Keys() {
		if !strings.HasPrefix(k, "_") {
			out.Set(k, data.Value(k))
		}
	}
	return out
}

// Cached channel directory of available send targets
func loadChannelDirectory(ctx context.Context) any {
	return readJSON(filepath.Join(home.Dir(ctx), "channel_directory.json"))
}

// Clamped int of a tool argument, def when the value does not convert
func coerceInt(value any, def, minimum, maximum int64) int64 {
	n, err := anyutil.ToInt(value)
	if errors.Is(err, anyutil.ErrNotNumber) {
		n = def
	}
	if n > maximum {
		return maximum
	}
	if n < minimum {
		return minimum
	}
	return n
}

// Text of a message, the text parts of multi part content joined by newlines
func extractMessageContent(msg any) string {
	content := pyGet(msg, "content", "")
	if list, ok := content.([]any); ok {
		parts := []string{}
		for _, p := range list {
			if d, ok := asDict(p); ok && isStr(pyGet(d, "type", nil), "text") {
				text := pyGet(d, "text", "")
				s, ok := text.(string)
				if !ok {
					panic(&pyError{"TypeError", fmt.Sprintf("sequence item %d: expected str instance, %s found", len(parts), anyutil.Kind(text))})
				}
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	if anyutil.IsTrue(content) {
		return anyutil.String(content)
	}
	return ""
}

var mediaTagRE = regexutil.Lazy(regexp.MustCompile, `MEDIA:[\s\v\x{85}\p{Z}]*([^\s\v\x{85}\p{Z}]+)`)

// Reports whether v is the str s
func isStr(v any, s string) bool {
	t, ok := v.(string)
	return ok && t == s
}

// Non text attachments, image and file content blocks plus the MEDIA tags of the text
func extractAttachments(msg any) []any {
	attachments := []any{}
	content := pyGet(msg, "content", "")
	if list, ok := content.([]any); ok {
		for _, part := range list {
			if _, ok := asDict(part); !ok {
				continue
			}
			ptype := pyGet(part, "type", "")
			var url any
			switch {
			case isStr(ptype, "image_url"):
				url = ""
				if image, ok := asDict(pyGet(part, "image_url", nil)); ok {
					url = pyGet(image, "url", "")
				}
			case isStr(ptype, "image"):
				fallback := pyGet(pyGet(part, "source", jsonutil.NewObject()), "url", "")
				url = pyGet(part, "url", fallback)
			default:
				if !isStr(ptype, "text") {
					attachments = append(attachments, jsonutil.ObjectOf("type", ptype, "data", part))
				}
				continue
			}
			if anyutil.IsTrue(url) {
				attachments = append(attachments, jsonutil.ObjectOf("type", "image", "url", url))
			}
		}
	}
	for _, m := range mediaTagRE().FindAllStringSubmatch(extractMessageContent(msg), -1) {
		attachments = append(attachments, jsonutil.ObjectOf("type", "media", "path", m[1]))
	}
	return attachments
}

// One entry of the bridge event queue, a message or an approval request or resolution
type QueueEvent struct {
	Cursor     int64
	Type       string
	SessionKey any
	Data       *jsonutil.Object
}

// Wire dict of the event, its data keys following cursor, type and session_key
func (e *QueueEvent) asDict() *jsonutil.Object {
	out := jsonutil.ObjectOf("cursor", e.Cursor, "type", e.Type, "session_key", e.SessionKey)
	out.Merge(e.Data)
	return out
}

// Float of a message timestamp given as epoch number or ISO text, zero when neither
func tsFloat(ts any) float64 {
	if f, isFloat := ts.(float64); isFloat {
		return f
	}
	if n, isInt := anyutil.Int(ts); isInt {
		return float64(n)
	}
	text, ok := ts.(string)
	if !ok || text == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
		return f
	}
	t, err := timeutil.ParseISO(text, time.Local)
	if err != nil {
		return 0
	}
	return timeutil.Epoch(t)
}

// Newest normalized timestamp among messages, zero when there are none
func latestTS(messages []*jsonutil.Object) float64 {
	if len(messages) == 0 {
		return 0
	}
	latest := tsFloat(pyGet(messages[0], "timestamp", int64(0)))
	for _, m := range messages[1:] {
		if v := tsFloat(pyGet(m, "timestamp", int64(0))); v > latest {
			latest = v
		}
	}
	return latest
}

// threading.Event analogue, a flag waiters block on until it is set
type pyEvent struct {
	mu   sync.Mutex
	flag bool
	ch   chan struct{}
}

func newPyEvent() *pyEvent { return &pyEvent{ch: make(chan struct{})} }

func (e *pyEvent) set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.flag {
		e.flag = true
		close(e.ch)
	}
}

func (e *pyEvent) clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flag {
		e.flag = false
		e.ch = make(chan struct{})
	}
}

// Blocks until the flag is set or timeout passes
func (e *pyEvent) wait(timeout time.Duration) {
	e.mu.Lock()
	if e.flag {
		e.mu.Unlock()
		return
	}
	ch := e.ch
	e.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}

// Poller of state.db feeding new conversation messages into an in memory event queue that waiters block on
type EventBridge struct {
	ctx                 context.Context
	mu                  sync.Mutex
	queue               []*QueueEvent
	cursor              int64
	newEvent            *pyEvent
	running             atomic.Bool
	done                chan struct{}
	lastPollTimestamps  map[string]float64
	pendingApprovals    *jsonutil.Object
	dbVersion           *stateDBVersion
	stateDBVersion      *int64
	cachedSessionsIndex *jsonutil.Object
}

// Stopped bridge over the state.db of the home ctx names
func NewEventBridge(ctx context.Context) *EventBridge {
	return &EventBridge{ctx: ctx, newEvent: newPyEvent(), lastPollTimestamps: map[string]float64{},
		pendingApprovals: jsonutil.NewObject(), dbVersion: &stateDBVersion{}, cachedSessionsIndex: jsonutil.NewObject()}
}

// Baselines existing history without emitting it, then polls in the background
func (b *EventBridge) Start() {
	if b.running.Load() {
		return
	}
	b.establishBaseline()
	b.running.Store(true)
	b.done = make(chan struct{})
	go b.pollLoop(b.done)
	pkgLog().Debug("EventBridge started")
}

// Ends the polling loop, wakes every waiter and closes the version gate
func (b *EventBridge) Stop() {
	b.running.Store(false)
	b.newEvent.set()
	if b.done != nil {
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-b.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	b.dbVersion.close()
	pkgLog().Debug("EventBridge stopped")
}

// Event dicts after a cursor, filtered to one session key when given, at most limit
func (b *EventBridge) matching(afterCursor int64, sessionKey string, limit int64) []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []any{}
	for _, e := range b.queue {
		if e.Cursor > afterCursor && (sessionKey == "" || isStr(e.SessionKey, sessionKey)) {
			out = append(out, e.asDict())
		}
	}
	if int64(len(out)) > limit {
		out = out[:limit]
	}
	return out
}

// Events since afterCursor and the cursor to poll from next
func (b *EventBridge) PollEvents(afterCursor int64, sessionKey string, limit int64) *jsonutil.Object {
	events := b.matching(afterCursor, sessionKey, limit)
	var next any = afterCursor
	if len(events) > 0 {
		next = pyGet(events[len(events)-1], "cursor", nil)
	}
	return jsonutil.ObjectOf("events", events, "next_cursor", next)
}

// Blocks until a matching event arrives or the timeout expires, nil on timeout
func (b *EventBridge) WaitForEvent(afterCursor int64, sessionKey string, timeoutMs int64) any {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		if found := b.matching(afterCursor, sessionKey, 1); len(found) > 0 {
			return found[0]
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		b.newEvent.clear()
		b.newEvent.wait(min(remaining, pollInterval))
	}
	return nil
}

// Approval requests seen this session ordered by creation time
func (b *EventBridge) ListPendingApprovals() []any {
	b.mu.Lock()
	approvals := make([]any, 0, b.pendingApprovals.Len())
	for _, k := range b.pendingApprovals.Keys() {
		approvals = append(approvals, b.pendingApprovals.Value(k))
	}
	b.mu.Unlock()
	sortByKey(approvals, func(a any) any { return pyGet(a, "created_at", "") }, false)
	return approvals
}

// Resolves a pending approval and queues its resolution event
func (b *EventBridge) RespondToApproval(approvalID, decision string) *jsonutil.Object {
	b.mu.Lock()
	approval, _ := b.pendingApprovals.Take(approvalID)
	b.mu.Unlock()
	if !anyutil.IsTrue(approval) {
		return jsonutil.ObjectOf("error", "Approval not found: "+approvalID)
	}
	b.enqueue(&QueueEvent{Type: "approval_resolved", SessionKey: pyGet(approval, "session_key", ""),
		Data: jsonutil.ObjectOf("approval_id", approvalID, "decision", decision)})
	return jsonutil.ObjectOf("resolved", true, "approval_id", approvalID, "decision", decision)
}

// Appends an event under the next cursor, trims the queue to QueueLimit and wakes every waiter
func (b *EventBridge) enqueue(event *QueueEvent) {
	b.mu.Lock()
	b.cursor++
	event.Cursor = b.cursor
	b.queue = append(b.queue, event)
	if over := len(b.queue) - QueueLimit; over > 0 {
		b.queue = append([]*QueueEvent(nil), b.queue[over:]...)
	}
	b.mu.Unlock()
	b.newEvent.set()
}

// Records each existing session's latest timestamp and the version gate without emitting events
func (b *EventBridge) establishBaseline() {
	db := getSessionDB(b.ctx)
	if db == nil {
		return
	}
	defer closeQuietly(db, "baseline")
	b.stateDBVersion = nil
	if v, ok := b.dbVersion.read(b.ctx); ok {
		b.stateDBVersion = &v
	}
	b.cachedSessionsIndex = func() (index *jsonutil.Object) {
		defer func() {
			if r := recover(); r != nil {
				index = jsonutil.NewObject()
			}
		}()
		return loadSessionsIndex(b.ctx)
	}()
	for _, key := range b.cachedSessionsIndex.Keys() {
		sessionID := pyGet(b.cachedSessionsIndex.Value(key), "session_id", "")
		if !anyutil.IsTrue(sessionID) {
			continue
		}
		latest, ok := func() (latest float64, ok bool) {
			defer func() {
				if r := recover(); r != nil {
					ok = false
				}
			}()
			msgs, err := getMessages(b.ctx, db, anyutil.String(sessionID))
			if err != nil {
				return 0, false
			}
			return latestTS(msgs), true
		}()
		if ok && latest > 0 {
			b.lastPollTimestamps[key] = latest
		}
	}
}

// Background loop polling the session database until Stop
func (b *EventBridge) pollLoop(done chan struct{}) {
	defer close(done)
	db := getSessionDB(b.ctx)
	if db == nil {
		pkgLog().Warn("EventBridge: SessionDB unavailable, event polling disabled")
		return
	}
	defer closeQuietly(db, "polling")
	for b.running.Load() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					pkgLog().Debug(fmt.Sprintf("EventBridge poll error: %s", raisedText(r)))
				}
			}()
			b.pollOnce(db)
		}()
		time.Sleep(pollInterval)
	}
}

// Queues every new user or assistant message across sessions, skipping all work while data_version stands still
func (b *EventBridge) pollOnce(db *state.DB) {
	version, ok := b.dbVersion.read(b.ctx)
	if ok && b.stateDBVersion != nil && *b.stateDBVersion == version {
		return
	}
	b.stateDBVersion = nil
	if ok {
		b.stateDBVersion = &version
	}
	b.cachedSessionsIndex = loadSessionsIndex(b.ctx)
	for _, key := range b.cachedSessionsIndex.Keys() {
		sessionID := pyGet(b.cachedSessionsIndex.Value(key), "session_id", "")
		if !anyutil.IsTrue(sessionID) {
			continue
		}
		lastSeen := b.lastPollTimestamps[key]
		messages, err := getMessages(b.ctx, db, anyutil.String(sessionID))
		if err != nil || len(messages) == 0 {
			continue
		}
		for _, msg := range messages {
			role := pyGet(msg, "role", "")
			pyHashable(role)
			if !(isStr(role, "user") || isStr(role, "assistant")) || tsFloat(pyGet(msg, "timestamp", int64(0))) <= lastSeen {
				continue
			}
			content := extractMessageContent(msg)
			if content == "" {
				continue
			}
			b.enqueue(&QueueEvent{Type: "message", SessionKey: key, Data: jsonutil.ObjectOf(
				"role", pyGet(msg, "role", ""), "content", textutil.First(content, 500),
				"timestamp", anyutil.String(pyGet(msg, "timestamp", "")), "message_id", anyutil.String(pyGet(msg, "id", "")))})
		}
		if latest := latestTS(messages); latest > lastSeen {
			b.lastPollTimestamps[key] = latest
		}
	}
}

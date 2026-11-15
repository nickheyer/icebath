package mcpserve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	_ "modernc.org/sqlite"
)

// Events the bridge queue keeps, the oldest dropped past it
const QueueLimit = 1000

// Wait between two database polls
const pollInterval = 200 * time.Millisecond

func sessionsDir(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "sessions") }

// Percent encoding of a path the way pathlib as_uri writes it, keeping slashes and unreserved bytes
func uriPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' || c == '_' || c == '.' || c == '-' || c == '~' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

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
		db, err := sql.Open("sqlite", "file://"+uriPath(filepath.ToSlash(resolved))+"?mode=ro")
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
		return pyjson.NewObject()
	}
	data, err := os.ReadFile(path)
	if err == nil && !utf8.Valid(data) {
		err = errors.New("'utf-8' codec can't decode the file")
	}
	var parsed any
	if err == nil {
		parsed, err = pyjson.Loads(string(data))
	}
	if err != nil {
		bridgeLog.Debug(fmt.Sprintf("Failed to load %s: %s", filepath.Base(path), err.Error()))
		return pyjson.NewObject()
	}
	return parsed
}

// Shared session database for reading transcripts, nil when it cannot open
func getSessionDB(ctx context.Context) *state.DB {
	db, err := state.Acquire(ctx, "")
	if err != nil {
		bridgeLog.Debug(fmt.Sprintf("SessionDB unavailable: %s", err.Error()))
		return nil
	}
	return db
}

// Closes a session database handle, logging a failure at debug
func closeQuietly(db *state.DB, what string) {
	if err := db.Close(); err != nil {
		bridgeLog.Debug(fmt.Sprintf("Failed to close MCP %s SessionDB", what), "error", err)
	}
}

// Messages of one session, anything raised by the read returned as the error
func getMessages(ctx context.Context, db *state.DB, sessionID string) (msgs []*pyjson.Object, err error) {
	defer func() {
		if r := recover(); r != nil {
			msgs, err = nil, errors.New(raisedText(r))
		}
	}()
	return db.GetMessages(ctx, sessionID, state.GetMessagesOptions{})
}

// Messages of one session or the error text, releasing the database handle either way
func loadSessionMessages(ctx context.Context, sessionID string) ([]*pyjson.Object, string) {
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
func loadSessionsIndex(ctx context.Context) *pyjson.Object {
	if idx := loadSessionsIndexFromDB(ctx); idx.Len() > 0 {
		return idx
	}
	return loadSessionsIndexFromJSON(ctx)
}

// Naive local ISO time of epoch seconds, empty when unset or not convertible
func isoTime(ts any) string {
	if !pyrepr.Truthy(ts) {
		return ""
	}
	f, err := pyint.Float(ts)
	if err != nil {
		var pe *pyint.Error
		if errors.As(err, &pe) && pe.Kind == "OverflowError" {
			panic(err)
		}
		return ""
	}
	t, err := pydatetime.FromTimestampIn(f, time.Local, pydatetime.LocaltimeAccepts)
	if err != nil {
		var de *pydatetime.Error
		if errors.As(err, &de) && de.Kind == "OverflowError" {
			panic(err)
		}
		return ""
	}
	return pydatetime.ISOFormatNaive(t)
}

// Python int of v, raising what int raises
func pyIntOf(v any) *big.Int {
	n, err := pyint.Of(v)
	if err != nil {
		panic(err)
	}
	return n
}

// Converts a state.db gateway session row to the sessions.json entry shape
func rowToIndexEntry(row *pyjson.Object) *pyjson.Object {
	origin := pyjson.NewObject()
	if raw := pyGet(row, "origin_json", nil); pyrepr.Truthy(raw) {
		var parsed any
		var err error
		switch t := raw.(type) {
		case string:
			parsed, err = pyjson.Loads(t)
		case []byte:
			parsed, err = pyjson.LoadsBytes(t)
		default:
			err = errors.New("the JSON object must be str, bytes or bytearray")
		}
		if d, ok := asDict(parsed); err == nil && ok {
			origin = d
		}
	}
	if origin.Len() == 0 {
		origin = pyjson.ObjectOf("platform", pyGet(row, "source", ""))
		for _, k := range []string{"chat_id", "chat_type", "thread_id", "user_id"} {
			origin.Set(k, pyGet(row, k, nil))
		}
	}
	tokens := func(key string) *big.Int {
		v := pyGet(row, key, nil)
		if !pyrepr.Truthy(v) {
			v = int64(0)
		}
		return pyIntOf(v)
	}
	inputTokens, outputTokens := tokens("input_tokens"), tokens("output_tokens")
	chatType := pyGet(row, "chat_type", nil)
	if !pyrepr.Truthy(chatType) {
		chatType = pyGet(origin, "chat_type", "")
	}
	displayName := pyGet(row, "display_name", nil)
	if !pyrepr.Truthy(displayName) {
		displayName = pyGet(origin, "chat_name", nil)
	}
	if !pyrepr.Truthy(displayName) {
		displayName = ""
	}
	lastActive := pyGet(row, "last_active", nil)
	if !pyrepr.Truthy(lastActive) {
		lastActive = pyGet(row, "started_at", nil)
	}
	return pyjson.ObjectOf(
		"session_id", pyrepr.Str(pyGet(row, "id", "")), "session_key", pyGet(row, "session_key", ""),
		"platform", pyGet(row, "source", ""),
		"chat_type", chatType,
		"display_name", displayName,
		"origin", origin,
		"created_at", isoTime(pyGet(row, "started_at", nil)),
		"updated_at", isoTime(lastActive),
		"input_tokens", pyint.Value(inputTokens), "output_tokens", pyint.Value(outputTokens),
		"total_tokens", pyint.Value(new(big.Int).Add(inputTokens, outputTokens)),
	)
}

// Routing index built from the open gateway session rows of state.db, empty when it cannot be read
func loadSessionsIndexFromDB(ctx context.Context) (index *pyjson.Object) {
	db := getSessionDB(ctx)
	if db == nil {
		return pyjson.NewObject()
	}
	defer db.Close()
	defer func() {
		if r := recover(); r != nil {
			bridgeLog.Debug(fmt.Sprintf("Failed to load gateway sessions from state.db: %s", raisedText(r)))
			index = pyjson.NewObject()
		}
	}()
	rows, err := db.ListGatewaySessions(ctx, "", true)
	if err != nil {
		panic(err)
	}
	out := pyjson.NewObject()
	for _, row := range rows {
		key := pyGet(row, "session_key", nil)
		if !pyrepr.Truthy(key) {
			continue
		}
		out.Set(pyrepr.Str(key), rowToIndexEntry(row))
	}
	return out
}

// Legacy sessions.json index with the underscore prefixed metadata sentinels left out
func loadSessionsIndexFromJSON(ctx context.Context) *pyjson.Object {
	out := pyjson.NewObject()
	data, ok := readJSON(filepath.Join(sessionsDir(ctx), "sessions.json")).(*pyjson.Object)
	if !ok {
		return out
	}
	for _, k := range data.Keys {
		if !strings.HasPrefix(k, "_") {
			out.Set(k, data.Vals[k])
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
	n, err := pyint.Of(value)
	if err != nil {
		var pe *pyint.Error
		if !errors.As(err, &pe) || (pe.Kind != "TypeError" && pe.Kind != "ValueError") {
			panic(err)
		}
		n = big.NewInt(def)
	}
	if n.Cmp(big.NewInt(maximum)) > 0 {
		return maximum
	}
	if n.Cmp(big.NewInt(minimum)) < 0 {
		return minimum
	}
	return n.Int64()
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
					panic(&pyError{"TypeError", fmt.Sprintf("sequence item %d: expected str instance, %s found", len(parts), pyrepr.TypeName(text))})
				}
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	if pyrepr.Truthy(content) {
		return pyrepr.Str(content)
	}
	return ""
}

var mediaTagRE = pyre.MustCompile(`MEDIA:\s*(\S+)`)

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
				fallback := pyGet(pyGet(part, "source", pyjson.NewObject()), "url", "")
				url = pyGet(part, "url", fallback)
			default:
				if !isStr(ptype, "text") {
					attachments = append(attachments, pyjson.ObjectOf("type", ptype, "data", part))
				}
				continue
			}
			if pyrepr.Truthy(url) {
				attachments = append(attachments, pyjson.ObjectOf("type", "image", "url", url))
			}
		}
	}
	m, _ := mediaTagRE.FindStringMatch(extractMessageContent(msg))
	for m != nil {
		attachments = append(attachments, pyjson.ObjectOf("type", "media", "path", m.GroupByNumber(1).String()))
		m, _ = mediaTagRE.FindNextMatch(m)
	}
	return attachments
}

// One entry of the bridge event queue, a message or an approval request or resolution
type QueueEvent struct {
	Cursor     int64
	Type       string
	SessionKey any
	Data       *pyjson.Object
}

// Wire dict of the event, its data keys following cursor, type and session_key
func (e *QueueEvent) asDict() *pyjson.Object {
	out := pyjson.ObjectOf("cursor", e.Cursor, "type", e.Type, "session_key", e.SessionKey)
	out.Update(e.Data)
	return out
}

// Float of a message timestamp given as epoch number or ISO text, zero when neither
func tsFloat(ts any) float64 {
	if f, isFloat := ts.(float64); isFloat {
		return f
	}
	if _, isInt := pyint.Instance(ts); isInt {
		f, err := pyint.Float(ts)
		if err != nil {
			panic(err)
		}
		return f
	}
	text, ok := ts.(string)
	if !ok || text == "" {
		return 0
	}
	if f, err := pyint.ParseFloat(text); err == nil {
		return f
	}
	t, aware, err := pydatetime.ParseISOFormat(text)
	if err != nil {
		return 0
	}
	if aware {
		return pydatetime.Timestamp(t)
	}
	seconds, ok := pydatetime.LocalToSeconds(t, time.Local, 0, pydatetime.LocaltimeAccepts)
	if !ok {
		return 0
	}
	return float64(seconds) + float64(t.Nanosecond()/1000)/1e6
}

// Newest normalized timestamp among messages, zero when there are none
func latestTS(messages []*pyjson.Object) float64 {
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
	pendingApprovals    *pyjson.Object
	dbVersion           *stateDBVersion
	stateDBVersion      *int64
	cachedSessionsIndex *pyjson.Object
}

// Stopped bridge over the state.db of the home ctx names
func NewEventBridge(ctx context.Context) *EventBridge {
	return &EventBridge{ctx: ctx, newEvent: newPyEvent(), lastPollTimestamps: map[string]float64{},
		pendingApprovals: pyjson.NewObject(), dbVersion: &stateDBVersion{}, cachedSessionsIndex: pyjson.NewObject()}
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
	bridgeLog.Debug("EventBridge started")
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
	bridgeLog.Debug("EventBridge stopped")
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
func (b *EventBridge) PollEvents(afterCursor int64, sessionKey string, limit int64) *pyjson.Object {
	events := b.matching(afterCursor, sessionKey, limit)
	var next any = afterCursor
	if len(events) > 0 {
		next = pyGet(events[len(events)-1], "cursor", nil)
	}
	return pyjson.ObjectOf("events", events, "next_cursor", next)
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
	for _, k := range b.pendingApprovals.Keys {
		approvals = append(approvals, b.pendingApprovals.Vals[k])
	}
	b.mu.Unlock()
	sortByKey(approvals, func(a any) any { return pyGet(a, "created_at", "") }, false)
	return approvals
}

// Resolves a pending approval and queues its resolution event
func (b *EventBridge) RespondToApproval(approvalID, decision string) *pyjson.Object {
	b.mu.Lock()
	approval, _ := b.pendingApprovals.Pop(approvalID)
	b.mu.Unlock()
	if !pyrepr.Truthy(approval) {
		return pyjson.ObjectOf("error", "Approval not found: "+approvalID)
	}
	b.enqueue(&QueueEvent{Type: "approval_resolved", SessionKey: pyGet(approval, "session_key", ""),
		Data: pyjson.ObjectOf("approval_id", approvalID, "decision", decision)})
	return pyjson.ObjectOf("resolved", true, "approval_id", approvalID, "decision", decision)
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
	b.cachedSessionsIndex = func() (index *pyjson.Object) {
		defer func() {
			if r := recover(); r != nil {
				index = pyjson.NewObject()
			}
		}()
		return loadSessionsIndex(b.ctx)
	}()
	for _, key := range b.cachedSessionsIndex.Keys {
		sessionID := pyGet(b.cachedSessionsIndex.Vals[key], "session_id", "")
		if !pyrepr.Truthy(sessionID) {
			continue
		}
		latest, ok := func() (latest float64, ok bool) {
			defer func() {
				if r := recover(); r != nil {
					ok = false
				}
			}()
			msgs, err := getMessages(b.ctx, db, pyrepr.Str(sessionID))
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
		bridgeLog.Warn("EventBridge: SessionDB unavailable, event polling disabled")
		return
	}
	defer closeQuietly(db, "polling")
	for b.running.Load() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					bridgeLog.Debug(fmt.Sprintf("EventBridge poll error: %s", raisedText(r)))
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
	for _, key := range b.cachedSessionsIndex.Keys {
		sessionID := pyGet(b.cachedSessionsIndex.Vals[key], "session_id", "")
		if !pyrepr.Truthy(sessionID) {
			continue
		}
		lastSeen := b.lastPollTimestamps[key]
		messages, err := getMessages(b.ctx, db, pyrepr.Str(sessionID))
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
			b.enqueue(&QueueEvent{Type: "message", SessionKey: key, Data: pyjson.ObjectOf(
				"role", pyGet(msg, "role", ""), "content", pystr.Slice(content, 0, 500),
				"timestamp", pyrepr.Str(pyGet(msg, "timestamp", "")), "message_id", pyrepr.Str(pyGet(msg, "id", "")))})
		}
		if latest := latestTS(messages); latest > lastSeen {
			b.lastPollTimestamps[key] = latest
		}
	}
}

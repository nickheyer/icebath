package gateway

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Store over a temp sessions dir pinned to db and marked loaded without reading the routing index
func bareTranscriptStore(ctx context.Context, t *testing.T, db SessionStoreDB) *SessionStore {
	t.Helper()
	store := NewSessionStore(ctx, t.TempDir(), &GatewayConfig{}, nil)
	store.PinDB(db)
	store.loaded = true
	return store
}

// Contents of a session's conversation rows in order
func transcriptContents(ctx context.Context, t *testing.T, db *state.SessionDB, sessionID string) []any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := []any{}
	for _, row := range rows {
		out = append(out, row["content"])
	}
	return out
}

// Contents queued in memory for a session
func queuedContents(store *SessionStore, sessionID string) []any {
	out := []any{}
	if q := store.dirtyTranscripts[sessionID]; q != nil {
		for _, item := range q.items {
			out = append(out, item.message["content"])
		}
	}
	return out
}

// Pending spool payload files under the home in name order
func pendingSpoolFiles(t *testing.T, home string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(home, "pending_messages", "pending-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// Decoded spool payload
func spoolPayloadObject(t *testing.T, path string) *pyjson.Object {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

// Message content carried by a spool payload
func spoolPayloadContent(t *testing.T, path string) any {
	t.Helper()
	data, _ := spoolPayloadObject(t, path).Get("data")
	msg, _ := data.(*pyjson.Object).Get("message")
	content, _ := msg.(*pyjson.Object).Get("content")
	return content
}

// Levels of captured records whose text contains the marker
func capturedLevels(buf interface{ String() string }, marker string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, marker) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if level, ok := strings.CutPrefix(field, "level="); ok {
				out = append(out, level)
			}
		}
	}
	return out
}

// Store double answering only what a test sets
type fakeTranscriptDB struct {
	SessionStoreDB
	appendFn func(sessionID string, fields map[string]any) error
	tipFn    func(sessionID string) (string, error)
	getFn    func(sessionID string) (*pyjson.Object, error)
}

func (f *fakeTranscriptDB) AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error) {
	return 0, f.appendFn(sessionID, fields)
}

func (f *fakeTranscriptDB) GetCompressionTip(ctx context.Context, sessionID string) (string, error) {
	return f.tipFn(sessionID)
}

func (f *fakeTranscriptDB) GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	return f.getFn(sessionID)
}

// Store double whose FTS rebuild records each call and reports a fixed count
type fakeFTSRebuildDB struct {
	SessionStoreDB
	calls   *[]float64
	now     *float64
	rebuilt int
}

func (f *fakeFTSRebuildDB) RebuildFTS(ctx context.Context) (int, error) {
	*f.calls = append(*f.calls, *f.now)
	return f.rebuilt, nil
}

// Appender failing while broken and recording every written row
type brokenThenHealedDB struct {
	SessionStoreDB
	broken     bool
	rows       []map[string]any
	failReplay []string
}

func (b *brokenThenHealedDB) AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error) {
	if b.broken {
		return 0, errors.New("db unavailable")
	}
	if content, _ := fields["content"].(string); slices.Contains(b.failReplay, content) {
		return 0, errors.New("still broken for replays")
	}
	b.rows = append(b.rows, fields)
	return int64(len(b.rows)), nil
}

func (b *brokenThenHealedDB) contents() []any {
	out := []any{}
	for _, row := range b.rows {
		out = append(out, row["content"])
	}
	return out
}

func endCompression(t *testing.T, db *state.SessionDB, sessionID, reason string) {
	t.Helper()
	if err := db.EndSession(sessionID, reason); err != nil {
		t.Fatal(err)
	}
}

func createTestSession(t *testing.T, db *state.SessionDB, n state.NewSession) {
	t.Helper()
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
}

func replaceTestMessages(ctx context.Context, t *testing.T, db *state.SessionDB, sessionID string, messages ...map[string]any) {
	t.Helper()
	if err := db.ReplaceMessages(ctx, sessionID, messages, state.ReplaceOptions{}); err != nil {
		t.Fatal(err)
	}
}

func appendTranscript(ctx context.Context, t *testing.T, store *SessionStore, sessionID, role, content string) {
	t.Helper()
	if err := store.AppendToTranscript(ctx, sessionID, map[string]any{"role": role, "content": content}, false); err != nil {
		t.Fatal(err)
	}
}

func TestCompressionClosedParentReroutesWithoutRetryQueue(t *testing.T) {
	ctx, db := newTestStateDB(t)
	createTestSession(t, db, state.NewSession{ID: "parent", Source: "telegram"})
	endCompression(t, db, "parent", "compression")
	createTestSession(t, db, state.NewSession{ID: "child", Source: "telegram", ParentSessionID: "parent"})
	replaceTestMessages(ctx, t, db, "child", map[string]any{"role": "user", "content": "summary"})
	store := bareTranscriptStore(ctx, t, db)
	store.entries.set("route", &SessionEntry{SessionKey: "route", SessionID: "parent"})

	appendTranscript(ctx, t, store, "parent", "assistant", "routed to child")

	if got := store.entries.get("route").SessionID; got != "child" {
		t.Fatalf("route = %q", got)
	}
	if _, ok := store.dirtyTranscripts["parent"]; ok {
		t.Fatal("parent queue left behind")
	}
	if got := transcriptContents(ctx, t, db, "parent"); len(got) != 0 {
		t.Fatalf("parent rows = %v", got)
	}
	if got := transcriptContents(ctx, t, db, "child"); !slices.Equal(got, []any{"summary", "routed to child"}) {
		t.Fatalf("child rows = %v", got)
	}
}

func TestTranscriptRerouteFollowsMultiHopCompressionChain(t *testing.T) {
	ctx, db := newTestStateDB(t)
	createTestSession(t, db, state.NewSession{ID: "root", Source: "telegram"})
	endCompression(t, db, "root", "compression")
	createTestSession(t, db, state.NewSession{ID: "mid", Source: "telegram", ParentSessionID: "root"})
	endCompression(t, db, "mid", "compression")
	createTestSession(t, db, state.NewSession{ID: "tip", Source: "telegram", ParentSessionID: "mid"})
	replaceTestMessages(ctx, t, db, "tip", map[string]any{"role": "user", "content": "summary"})
	store := bareTranscriptStore(ctx, t, db)
	store.entries.set("route", &SessionEntry{SessionKey: "route", SessionID: "root"})

	appendTranscript(ctx, t, store, "root", "assistant", "routed to tip")

	if got := store.entries.get("route").SessionID; got != "tip" {
		t.Fatalf("route = %q", got)
	}
	if _, ok := store.dirtyTranscripts["root"]; ok {
		t.Fatal("root queue left behind")
	}
	if got := transcriptContents(ctx, t, db, "root"); len(got) != 0 {
		t.Fatalf("root rows = %v", got)
	}
	if got := transcriptContents(ctx, t, db, "tip"); !slices.Equal(got, []any{"summary", "routed to tip"}) {
		t.Fatalf("tip rows = %v", got)
	}
}

func TestTranscriptRerouteFailsClosedOnStaleClosedTip(t *testing.T) {
	ctx, db := newTestStateDB(t)
	createTestSession(t, db, state.NewSession{ID: "root", Source: "telegram"})
	endCompression(t, db, "root", "compression")
	createTestSession(t, db, state.NewSession{ID: "stale", Source: "telegram", ParentSessionID: "root"})
	endCompression(t, db, "stale", "ws_orphan_reap")
	store := bareTranscriptStore(ctx, t, db)
	store.entries.set("route", &SessionEntry{SessionKey: "route", SessionID: "root"})

	appendTranscript(ctx, t, store, "root", "assistant", "must not land")

	if got := store.entries.get("route").SessionID; got != "root" {
		t.Fatalf("route = %q", got)
	}
	if got := transcriptContents(ctx, t, db, "stale"); len(got) != 0 {
		t.Fatalf("stale rows = %v", got)
	}
}

func TestTranscriptRerouteMigratesRemainingBacklogToChild(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	var childAttempts []any
	failedOld2 := false
	fake := &fakeTranscriptDB{
		tipFn: func(sessionID string) (string, error) {
			if sessionID != "parent" {
				t.Fatalf("tip lookup for %q", sessionID)
			}
			return "child", nil
		},
		getFn: func(sessionID string) (*pyjson.Object, error) {
			row := pyjson.NewObject()
			row.Set("id", sessionID)
			row.Set("ended_at", nil)
			return row, nil
		},
		appendFn: func(sessionID string, fields map[string]any) error {
			if sessionID == "parent" {
				return &state.CompressionSessionClosedError{SessionID: "parent"}
			}
			childAttempts = append(childAttempts, fields["content"])
			if fields["content"] == "old-2" && !failedOld2 {
				failedOld2 = true
				return errors.New("transient child failure")
			}
			return nil
		},
	}
	store := bareTranscriptStore(ctx, t, fake)
	store.entries.set("route", &SessionEntry{SessionKey: "route", SessionID: "parent"})
	store.dirtyTranscripts["parent"] = &transcriptQueue{items: []*queuedTranscript{
		{message: map[string]any{"role": "user", "content": "old-1"}},
		{message: map[string]any{"role": "assistant", "content": "old-2"}},
	}}
	store.transcriptAppendFailures["parent"] = 1
	now := transcriptMonotonic()
	store.ftsRebuildLastAttemptAt = &now

	appendTranscript(ctx, t, store, "parent", "user", "old-3")

	if !slices.Equal(childAttempts, []any{"old-1", "old-2"}) {
		t.Fatalf("child attempts = %v", childAttempts)
	}
	if got := store.entries.get("route").SessionID; got != "child" {
		t.Fatalf("route = %q", got)
	}
	if _, ok := store.dirtyTranscripts["parent"]; ok {
		t.Fatal("parent queue left behind")
	}
	if got := queuedContents(store, "child"); !slices.Equal(got, []any{"old-2", "old-3"}) {
		t.Fatalf("child queue = %v", got)
	}
	if store.transcriptAppendFailures["child"] < 2 {
		t.Fatalf("child failures = %d", store.transcriptAppendFailures["child"])
	}

	appendTranscript(ctx, t, store, "parent", "assistant", "new-after-reroute")

	if !slices.Equal(childAttempts, []any{"old-1", "old-2", "old-2", "old-3", "new-after-reroute"}) {
		t.Fatalf("child attempts = %v", childAttempts)
	}
	if _, ok := store.dirtyTranscripts["parent"]; ok {
		t.Fatal("parent queue left behind")
	}
	if _, ok := store.dirtyTranscripts["child"]; ok {
		t.Fatal("child queue left behind")
	}
}

func TestFTSCorruptionErrorRequiresFTSProvenance(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("database disk image is malformed"), false},
		{&state.SQLiteError{Class: state.SQLiteDatabaseError, Msg: "database disk image is malformed"}, false},
		{errors.New("no such table: messages_fts"), true},
		{&state.SQLiteError{Class: state.SQLiteDatabaseError, Msg: `fts5: corrupt structure record for table "messages_fts"`}, true},
		{errors.New("shifts were applied"), false},
		{errors.New("gifts received"), false},
	}
	for _, c := range cases {
		if got := isFTSCorruptionError(c.err); got != c.want {
			t.Fatalf("%q: got %v want %v", c.err, got, c.want)
		}
	}
}

func TestRebuildFTSOnceRetriesAfterCooldown(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	now := 1000.0
	patch(t, &transcriptMonotonic, func() float64 { return now })
	var calls []float64
	store := bareTranscriptStore(ctx, t, nil)
	if rebuilt, err := store.rebuildFTSOnce(ctx); rebuilt || err != nil {
		t.Fatalf("no db rebuilt = %v, %v", rebuilt, err)
	}
	if store.ftsRebuildLastAttemptAt != nil {
		t.Fatal("an attempt with no db started the cooldown")
	}
	store.PinDB(&fakeFTSRebuildDB{calls: &calls, now: &now, rebuilt: 0})
	if rebuilt, _ := store.rebuildFTSOnce(ctx); rebuilt {
		t.Fatal("a deferred rebuild reported success")
	}
	now += transcriptFTSRebuildCooldownSeconds - 1
	if rebuilt, _ := store.rebuildFTSOnce(ctx); rebuilt {
		t.Fatal("a cooling down rebuild reported success")
	}
	if len(calls) != 1 {
		t.Fatalf("calls inside the cooldown = %v", calls)
	}
	now += 2
	store.PinDB(&fakeFTSRebuildDB{calls: &calls, now: &now, rebuilt: 1})
	if rebuilt, _ := store.rebuildFTSOnce(ctx); !rebuilt {
		t.Fatal("rebuild after the cooldown did not run")
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestTranscriptAppendFailuresEscalateToError(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	fake := &fakeTranscriptDB{appendFn: func(string, map[string]any) error {
		return errors.New("database disk image is malformed")
	}}
	store := bareTranscriptStore(ctx, t, fake)
	now := transcriptMonotonic()
	store.ftsRebuildLastAttemptAt = &now
	logs := captureLogs(t)
	threshold := transcriptAppendFailureEscalationThreshold
	for i := range threshold {
		appendTranscript(ctx, t, store, "s-esc", "user", "m"+string(rune('0'+i)))
	}
	want := append(slices.Repeat([]string{"WARN"}, threshold-1), "ERROR")
	if got := capturedLevels(logs, "transcript append failed"); !slices.Equal(got, want) {
		t.Fatalf("levels = %v want %v", got, want)
	}
	if store.transcriptAppendFailures["s-esc"] != threshold {
		t.Fatalf("failures = %d", store.transcriptAppendFailures["s-esc"])
	}
}

func TestNoUsableDBCountsFailuresAndSpoolsBacklogBeforeCap(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	home := os.Getenv("ICEBATH_HOME")
	store := bareTranscriptStore(ctx, t, nil)
	now := transcriptMonotonic()
	store.ftsRebuildLastAttemptAt = &now
	logs := captureLogs(t)
	threshold := transcriptAppendFailureEscalationThreshold
	var want []any
	for i := range threshold {
		content := "m" + string(rune('0'+i))
		want = append(want, content)
		appendTranscript(ctx, t, store, "s-dead", "user", content)
	}
	if store.transcriptAppendFailures["s-dead"] != threshold {
		t.Fatalf("failures = %d", store.transcriptAppendFailures["s-dead"])
	}
	if levels := capturedLevels(logs, "transcript append failed"); len(levels) == 0 || levels[len(levels)-1] != "ERROR" {
		t.Fatalf("levels = %v", levels)
	}
	var spooled []any
	for _, path := range pendingSpoolFiles(t, home) {
		spooled = append(spooled, spoolPayloadContent(t, path))
	}
	sort.Slice(spooled, func(i, j int) bool { return spooled[i].(string) < spooled[j].(string) })
	if !slices.Equal(spooled, want) {
		t.Fatalf("spooled = %v", spooled)
	}
	if _, ok := store.dirtyTranscripts["s-dead"]; ok {
		t.Fatal("stalled backlog still in memory")
	}
	healed := &brokenThenHealedDB{}
	store.PinDB(healed)
	appendTranscript(ctx, t, store, "s-dead", "assistant", "recovered")
	if got := healed.contents(); !slices.Equal(got, append(want, "recovered")) {
		t.Fatalf("rows = %v", got)
	}
	if files := pendingSpoolFiles(t, home); len(files) != 0 {
		t.Fatalf("spool left = %v", files)
	}
}

// Store over a broken then healed appender with the stalled backlog spool out of the way
func spoolCapStore(ctx context.Context, t *testing.T, maxPending int) (*SessionStore, *brokenThenHealedDB) {
	t.Helper()
	patch(t, &transcriptMaxPendingPerSession, maxPending)
	patch(t, &transcriptAppendFailureEscalationThreshold, 1000000)
	db := &brokenThenHealedDB{broken: true}
	store := bareTranscriptStore(ctx, t, db)
	now := transcriptMonotonic()
	store.ftsRebuildLastAttemptAt = &now
	return store, db
}

func TestDropSpoolDrainRoundtrip(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	home := os.Getenv("ICEBATH_HOME")
	store, db := spoolCapStore(ctx, t, 5)
	logs := captureLogs(t)
	nExtra := 3
	for i := range transcriptMaxPendingPerSession + nExtra {
		appendTranscript(ctx, t, store, "sess-1", "user", "msg"+string(rune('0'+i)))
	}
	files := pendingSpoolFiles(t, home)
	if len(files) != nExtra {
		t.Fatalf("spool files = %v", files)
	}
	var spooled []any
	for _, path := range files {
		if reason, _ := spoolPayloadObject(t, path).Get("reason"); reason != TranscriptCapDropReason {
			t.Fatalf("reason = %v", reason)
		}
		spooled = append(spooled, spoolPayloadContent(t, path))
	}
	sort.Slice(spooled, func(i, j int) bool { return spooled[i].(string) < spooled[j].(string) })
	if !slices.Equal(spooled, []any{"msg0", "msg1", "msg2"}) {
		t.Fatalf("spooled = %v", spooled)
	}
	if levels := capturedLevels(logs, "spooled oldest message"); !slices.Equal(levels, slices.Repeat([]string{"WARN"}, nExtra)) {
		t.Fatalf("drop warnings = %v", levels)
	}
	if !strings.Contains(logs.String(), filepath.Join(home, "pending_messages")) {
		t.Fatal("drop warning lacks the spool path")
	}

	db.broken = false
	appendTranscript(ctx, t, store, "sess-1", "assistant", "recovered")

	contents := db.contents()
	for i := nExtra; i < transcriptMaxPendingPerSession+nExtra; i++ {
		if !slices.Contains(contents, any("msg"+string(rune('0'+i)))) {
			t.Fatalf("msg%d missing from %v", i, contents)
		}
	}
	if !slices.Contains(contents, any("recovered")) {
		t.Fatalf("recovered missing from %v", contents)
	}
	var replayed []any
	for _, c := range contents {
		if c == "msg0" || c == "msg1" || c == "msg2" {
			replayed = append(replayed, c)
		}
	}
	if !slices.Equal(replayed, []any{"msg0", "msg1", "msg2"}) {
		t.Fatalf("replayed = %v", replayed)
	}
	if files := pendingSpoolFiles(t, home); len(files) != 0 {
		t.Fatalf("spool left = %v", files)
	}
	if _, ok := store.dirtyTranscripts["sess-1"]; ok {
		t.Fatal("pending left in memory")
	}
}

func TestDrainOnlyTouchesOwnSession(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	home := os.Getenv("ICEBATH_HOME")
	store, db := spoolCapStore(ctx, t, 3)
	for i := range transcriptMaxPendingPerSession + 1 {
		appendTranscript(ctx, t, store, "sess-a", "user", "a"+string(rune('0'+i)))
		appendTranscript(ctx, t, store, "sess-b", "user", "b"+string(rune('0'+i)))
	}
	if files := pendingSpoolFiles(t, home); len(files) != 2 {
		t.Fatalf("spool files = %v", files)
	}
	db.broken = false
	appendTranscript(ctx, t, store, "sess-a", "user", "go-a")
	remaining := pendingSpoolFiles(t, home)
	if len(remaining) != 1 {
		t.Fatalf("remaining = %v", remaining)
	}
	if key, _ := spoolPayloadObject(t, remaining[0]).Get("session_key"); key != "sess-b" {
		t.Fatalf("remaining session = %v", key)
	}
	var aRows []any
	for _, row := range db.rows {
		if row["session_id"] == "sess-a" {
			aRows = append(aRows, row["content"])
		}
	}
	if !slices.Contains(aRows, any("a0")) {
		t.Fatalf("sess-a rows = %v", aRows)
	}
}

func TestSpoolFailureDegradesToPlainDrop(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	home := os.Getenv("ICEBATH_HOME")
	store, db := spoolCapStore(ctx, t, 4)
	patch(t, &shutdownFlushDir, func(context.Context) (string, error) { return "", errors.New("disk full") })
	logs := captureLogs(t)
	for i := range transcriptMaxPendingPerSession + 5 {
		appendTranscript(ctx, t, store, "sess-x", "user", "msg"+string(rune('0'+i)))
	}
	if got := queuedContents(store, "sess-x"); len(got) > transcriptMaxPendingPerSession {
		t.Fatalf("pending = %v", got)
	}
	if files := pendingSpoolFiles(t, home); len(files) != 0 {
		t.Fatalf("spool files = %v", files)
	}
	if levels := capturedLevels(logs, "on-disk spool unavailable"); !slices.Equal(levels, slices.Repeat([]string{"WARN"}, 5)) {
		t.Fatalf("degraded warnings = %v", levels)
	}
	db.broken = false
	appendTranscript(ctx, t, store, "sess-x", "user", "fin")
	if contents := db.contents(); contents[len(contents)-1] != "fin" {
		t.Fatalf("rows = %v", contents)
	}
}

func TestReplayFailureKeepsSpoolFiles(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	home := os.Getenv("ICEBATH_HOME")
	store, _ := spoolCapStore(ctx, t, 3)
	for i := range transcriptMaxPendingPerSession + 2 {
		appendTranscript(ctx, t, store, "sess-r", "user", "m"+string(rune('0'+i)))
	}
	if files := pendingSpoolFiles(t, home); len(files) != 2 {
		t.Fatalf("spool files = %v", files)
	}
	store.PinDB(&brokenThenHealedDB{failReplay: []string{"m0", "m1", "m2"}})
	appendTranscript(ctx, t, store, "sess-r", "user", "go")
	if files := pendingSpoolFiles(t, home); len(files) != 3 {
		t.Fatalf("spool files after failed replay = %v", files)
	}
	if !store.spooledDropSessions["sess-r"] {
		t.Fatal("sess-r no longer marked for a spool drain")
	}
}

func TestRewriteReplacesTranscript(t *testing.T) {
	ctx, db := newTestStateDB(t)
	store := makeTestSessionStore(ctx, t, t.TempDir(), db, &GatewayConfig{})
	createTestSession(t, db, state.NewSession{ID: "test_session_1", Source: "test"})
	for _, m := range [][2]string{{"user", "hello"}, {"assistant", "hi"}, {"user", "undo this"}, {"assistant", "ok"}} {
		appendTranscript(ctx, t, store, "test_session_1", m[0], m[1])
	}
	store.RewriteTranscript(ctx, "test_session_1", []map[string]any{
		{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi"}}, false, false)
	reloaded, err := store.LoadTranscript(ctx, "test_session_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 2 || reloaded[0].(map[string]any)["content"] != "hello" || reloaded[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("reloaded = %v", reloaded)
	}
}

func TestReasoningSurvivesRewrite(t *testing.T) {
	ctx, db := newTestStateDB(t)
	createTestSession(t, db, state.NewSession{ID: "reasoning-test", Source: "cli"})
	details := []any{map[string]any{"type": "summary", "text": "step by step"}}
	items := []any{map[string]any{"id": "r1", "type": "reasoning"}}
	if _, err := db.AppendMessage(ctx, "reasoning-test", map[string]any{"role": "assistant", "content": "The answer is 42.",
		"reasoning": "I need to think step by step.", "reasoning_content": "provider scratchpad",
		"reasoning_details": details, "codex_reasoning_items": items}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	check := func(rows []map[string]any) {
		t.Helper()
		row := rows[0]
		if row["reasoning"] != "I need to think step by step." || row["reasoning_content"] != "provider scratchpad" {
			t.Fatalf("reasoning = %v / %v", row["reasoning"], row["reasoning_content"])
		}
		if !pyjsonEqual(row["reasoning_details"], details) || !pyjsonEqual(row["codex_reasoning_items"], items) {
			t.Fatalf("details = %v items = %v", row["reasoning_details"], row["codex_reasoning_items"])
		}
	}
	before, err := db.GetMessagesAsConversation(ctx, "reasoning-test", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check(before)
	store := bareTranscriptStore(ctx, t, db)
	store.RewriteTranscript(ctx, "reasoning-test", before, false, false)
	after, err := db.GetMessagesAsConversation(ctx, "reasoning-test", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check(after)
}

// Equality of decoded JSON values with state ordered objects read as plain maps
func pyjsonEqual(a, b any) bool {
	return reflect.DeepEqual(statePlainJSON(a), statePlainJSON(b))
}

// Decoded JSON value with every state ordered object turned into a map
func statePlainJSON(v any) any {
	switch x := v.(type) {
	case state.PyObject:
		out := map[string]any{}
		for _, kv := range x {
			out[kv.Key] = statePlainJSON(kv.Value)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[k] = statePlainJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = statePlainJSON(val)
		}
		return out
	}
	return v
}

func TestFTSRepairedRetryWritesTheRowOnce(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	db := &repairableDB{broken: true}
	store := bareTranscriptStore(ctx, t, db)
	appendTranscript(ctx, t, store, "sess-1", "user", "live")
	appendTranscript(ctx, t, store, "sess-1", "user", "next")
	if db.repairs != 1 || !slices.Equal(db.rows, []any{"live", "next"}) {
		t.Fatalf("repairs %d rows %v", db.repairs, db.rows)
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

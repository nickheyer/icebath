package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type interruptingAppender struct{}

func (interruptingAppender) AppendMessage(context.Context, string, map[string]any, state.AppendGuards) (int64, error) {
	panic("KeyboardInterrupt")
}

type funcAppender func(sessionID string, fields map[string]any) error

func (f funcAppender) AppendMessage(_ context.Context, sessionID string, fields map[string]any, _ state.AppendGuards) (int64, error) {
	return 1, f(sessionID, fields)
}

func writeCapDropSpool(t *testing.T, dir, name, sessionID string, message map[string]any, tsJSON, seqJSON string) string {
	t.Helper()
	msg, err := pyjson.Dumps(message)
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf(`{"session_key": %q, "reason": %q, "ts": %s, "seq": %s, "data": {"session_id": %q, "message": %s}}`,
		sessionID, TranscriptCapDropReason, tsJSON, seqJSON, sessionID, msg)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendedContents(calls []appendCall) []any {
	out := make([]any, len(calls))
	for i, c := range calls {
		out[i] = c.fields["content"]
	}
	return out
}

func spoolJSONFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRecoverClosesOwnedDBWhenUnexpectedExceptionEscapes(t *testing.T) {
	dir := useFlushDir(t)
	writeFlushJSON(t, filepath.Join(dir, "pending.json"), map[string]any{
		"session_key": "agent:main:telegram:123", "data": map[string]any{"text": "message", "session_id": "sid"}})
	released := false
	patch(t, &spoolAcquireDB, func(context.Context) (TranscriptAppender, func(), error) {
		return interruptingAppender{}, func() { released = true }, nil
	})
	func() {
		defer func() {
			if r := recover(); r != "KeyboardInterrupt" {
				t.Fatalf("recovered %v", r)
			}
		}()
		RecoverPendingToDB(context.Background(), nil, nil)
	}()
	if !released {
		t.Fatal("owned db was not released")
	}
}

func TestRecoveryPayloadMemoryDoesNotScaleWithBacklog(t *testing.T) {
	dir := useFlushDir(t)
	content := strings.Repeat("x", 256*1024)
	for seq := 0; seq < 24; seq++ {
		if SpoolDroppedTranscriptMessage(context.Background(), "sess", map[string]any{"role": "user", "content": content, "timestamp": seq}) == "" {
			t.Fatal("spool write failed")
		}
	}
	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)
	var peak uint64
	sink := funcAppender(func(_ string, fields map[string]any) error {
		if fields["content"] != content {
			t.Fatalf("content mismatch")
		}
		var now runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&now)
		if now.HeapAlloc > base.HeapAlloc && now.HeapAlloc-base.HeapAlloc > peak {
			peak = now.HeapAlloc - base.HeapAlloc
		}
		return nil
	})
	n, err := RecoverPendingToDB(context.Background(), sink, nil)
	if err != nil || n != 24 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if peak >= 4*1024*1024 {
		t.Fatalf("recovery retained %d bytes", peak)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
}

func TestSameSecondShutdownSlotPrecedesOverflow(t *testing.T) {
	dir := useFlushDir(t)
	patch(t, &shutdownFlushNow, func() float64 { return 100 })
	names := []string{"zzz", "aaa", "bbb"}
	patch(t, &flushUUIDHex, func() string {
		name := names[0]
		names = names[1:]
		return name
	})
	ctx := context.Background()
	if n, err := FlushPendingValuesToFile(ctx, map[string]any{"session": map[string]any{"text": "A", "session_id": "sess"}}, "shutdown"); n != 1 || err != nil {
		t.Fatalf("pending %d %v", n, err)
	}
	if n, err := FlushOverflowToFile(ctx, map[string][]any{"session": {
		map[string]any{"text": "B", "session_id": "sess"}, map[string]any{"text": "C", "session_id": "sess"}}}, "shutdown"); n != 2 || err != nil {
		t.Fatalf("overflow %d %v", n, err)
	}
	db := &recordingAppender{}
	if n, err := RecoverPendingToDB(ctx, db, nil); n != 3 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"A", "B", "C"}) {
		t.Fatalf("order %v", got)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
}

func TestReplaysInDropOrderNotFileNameOrder(t *testing.T) {
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-zzz.json", "sess-1", map[string]any{"role": "user", "content": "first"}, "100", "0")
	writeCapDropSpool(t, dir, "pending-mmm.json", "sess-1", map[string]any{"role": "assistant", "content": "second"}, "100", "1")
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "third"}, "101", "2")
	db := &recordingAppender{}
	if n, err := RecoverPendingToDB(context.Background(), db, nil); n != 3 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"first", "second", "third"}) {
		t.Fatalf("order %v", got)
	}
}

func TestAnOutOfRangeOrderingFieldDoesNotAbortRecovery(t *testing.T) {
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-bad.json", "sess-1", map[string]any{"role": "user", "content": "huge ts"}, "1"+strings.Repeat("0", 400), "0")
	writeCapDropSpool(t, dir, "pending-ok.json", "sess-2", map[string]any{"role": "user", "content": "valid"}, "100", "1")
	db := &recordingAppender{}
	if n, err := RecoverPendingToDB(context.Background(), db, nil); n != 2 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}
	got := appendedContents(db.calls)
	if len(got) != 2 || !((got[0] == "huge ts" && got[1] == "valid") || (got[0] == "valid" && got[1] == "huge ts")) {
		t.Fatalf("contents %v", got)
	}
}

func TestLiveDrainAndRestartPassOrderMalformedFilesAlike(t *testing.T) {
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-a.json", "sess-1", map[string]any{"role": "user", "content": "string ts"}, `"later"`, "0")
	writeCapDropSpool(t, dir, "pending-b.json", "sess-1", map[string]any{"role": "user", "content": "nan ts"}, "NaN", "1")
	writeCapDropSpool(t, dir, "pending-c.json", "sess-1", map[string]any{"role": "user", "content": "ts 5"}, "5", "2")
	var restart []any
	for _, path := range orderFlushFiles(spoolJSONFiles(t, dir)) {
		raw, err := readSpoolPayload(path)
		if err != nil {
			t.Fatal(err)
		}
		restart = append(restart, pyjson.Plain(raw).(map[string]any)["data"].(map[string]any)["message"].(map[string]any)["content"])
	}
	var live []any
	n, remaining, err := DrainTranscriptSpool(context.Background(), "sess-1", func(m map[string]any) error {
		live = append(live, m["content"])
		return nil
	}, false)
	if n != 3 || remaining != 0 || err != nil {
		t.Fatalf("drain %d %d %v", n, remaining, err)
	}
	want := []any{"string ts", "nan ts", "ts 5"}
	if !reflect.DeepEqual(live, want) || !reflect.DeepEqual(restart, want) {
		t.Fatalf("live %v restart %v", live, restart)
	}
}

func TestAFailedReplayHoldsBackThatSessionsLaterMessagesOnly(t *testing.T) {
	dir := useFlushDir(t)
	logs := captureLogs(t)
	first := writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "first"}, "100", "0")
	second := writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "user", "content": "second"}, "101", "1")
	other := writeCapDropSpool(t, dir, "pending-ccc.json", "sess-2", map[string]any{"role": "user", "content": "other-session"}, "102", "2")
	var contents []any
	db := funcAppender(func(_ string, fields map[string]any) error {
		contents = append(contents, fields["content"])
		if fields["content"] == "first" {
			return errors.New("controlled database outage")
		}
		return nil
	})
	if n, err := RecoverPendingToDB(context.Background(), db, nil); n != 1 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}
	if !reflect.DeepEqual(contents, []any{"first", "other-session"}) {
		t.Fatalf("contents %v", contents)
	}
	for _, p := range []string{first, second} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s gone", p)
		}
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("other session file kept")
	}
	text := logs.String()
	if !strings.Contains(text, "Held back 2 spooled transcript file(s)") || !strings.Contains(text, "sess-1 (2)") {
		t.Fatalf("log %s", text)
	}
}

func TestOnlyErrorsAboutTheRowsOwnValuesAreRejections(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("Error binding parameter 4: type 'dict' is not supported"), true},
		{errors.New("Error binding parameter 3 - probably unsupported type"), true},
		{&PyError{Kind: "OverflowError", Msg: "Python int too large to convert to SQLite INTEGER"}, true},
		{errors.New("no more rows available"), false},
		{errors.New("database is locked"), false},
		{errors.New("FOREIGN KEY constraint failed"), false},
		{errors.New("database disk image is malformed"), false},
	} {
		if got := IsRowRejection(tc.err); got != tc.want {
			t.Errorf("%v: %v", tc.err, got)
		}
	}
}

func TestSpoolDrainSkipsOtherReasons(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	if _, err := FlushPendingValuesToFile(ctx, map[string]any{"key1": "hello"}, "shutdown"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pending_messages")
	if files := spoolJSONFiles(t, dir); len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	n, _, err := DrainTranscriptSpool(ctx, "key1", func(map[string]any) error { return nil }, false)
	if n != 0 || err != nil {
		t.Fatalf("drain %d %v", n, err)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 1 {
		t.Fatalf("files %v", files)
	}
}

func TestSpoolRoundtripOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if SpoolDroppedTranscriptMessage(ctx, "s", map[string]any{"role": "user", "content": fmt.Sprintf("c%d", i)}) == "" {
			t.Fatal("spool write failed")
		}
	}
	var seen []any
	n, remaining, err := DrainTranscriptSpool(ctx, "s", func(m map[string]any) error {
		seen = append(seen, m["content"])
		return nil
	}, false)
	if n != 3 || remaining != 0 || err != nil || !reflect.DeepEqual(seen, []any{"c0", "c1", "c2"}) {
		t.Fatalf("drain %d %d %v %v", n, remaining, err, seen)
	}
	if files := spoolJSONFiles(t, filepath.Join(root, "pending_messages")); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
}

func TestPreservesAgentHistoryWhenFlushRaises(t *testing.T) {
	dir := useFlushDir(t)
	sid := "sess:abc123"
	FlushAgentHistoryToFile(context.Background(), &sid, []any{map[string]any{"role": "user", "content": "lost msg"}})
	payloads := readFlushPayloads(t, dir)
	if len(payloads) != 1 {
		t.Fatalf("expected recovery snapshot, got %v", payloads)
	}
	data := payloads[0]
	if data["issue"] != "#72680" || data["session_id"] != sid || data["count"] != int64(1) {
		t.Fatalf("payload %v", data)
	}
	if data["messages"].([]any)[0].(map[string]any)["content"] != "lost msg" {
		t.Fatalf("messages %v", data["messages"])
	}
}

func TestNoRecoveryFileOnEmptyHistory(t *testing.T) {
	dir := useFlushDir(t)
	sid := "sess:abc123"
	FlushAgentHistoryToFile(context.Background(), &sid, nil)
	if files := spoolJSONFiles(t, dir); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
}

// Appender double recording every call and failing with the queued errors in order
type sequenceAppender struct {
	SessionStoreDB
	calls   []appendCall
	results []error
	fn      func(fields map[string]any) error
}

func (q *sequenceAppender) AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error) {
	q.calls = append(q.calls, appendCall{sessionID, fields})
	if q.fn != nil {
		return 1, q.fn(fields)
	}
	if len(q.results) > 0 {
		err := q.results[0]
		q.results = q.results[1:]
		return 1, err
	}
	return 1, nil
}

// Boot recovery's default state.db becomes the given handle
func useBootDB(t *testing.T, db TranscriptAppender) {
	t.Helper()
	patch(t, &spoolAcquireDB, func(context.Context) (TranscriptAppender, func(), error) { return db, func() {}, nil })
}

// Replays the launch home's spool through the store as the gateway boot does
func bootRecover(ctx context.Context, t *testing.T, store *SessionStore) int {
	t.Helper()
	n, err := RecoverGatewayPending(ctx, store, &GatewayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Empties the process memory of replayed files that could not be deleted for the test
func freshReplayedUnremovable(t *testing.T) {
	t.Helper()
	replayedUnremovable.Lock()
	saved := replayedUnremovable.paths
	replayedUnremovable.paths = map[string]bool{}
	replayedUnremovable.Unlock()
	t.Cleanup(func() {
		replayedUnremovable.Lock()
		replayedUnremovable.paths = saved
		replayedUnremovable.Unlock()
	})
}

// Makes deleting the named spool file fail as on a directory that stopped being writable
func failSpoolRemoveFor(t *testing.T, name string) {
	t.Helper()
	patch(t, &spoolRemove, func(path string) error {
		if filepath.Base(path) == name {
			return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission}
		}
		return os.Remove(path)
	})
}

// Store over a temp sessions dir writing transcripts to db
func flushTestStore(ctx context.Context, t *testing.T, db SessionStoreDB) *SessionStore {
	t.Helper()
	freshReplayedUnremovable(t)
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), &GatewayConfig{}, nil)
	store.loaded = true
	store.PinDB(db)
	return store
}

func liveAppend(ctx context.Context, t *testing.T, store *SessionStore, content string) {
	t.Helper()
	if err := store.AppendToTranscript(ctx, "sess-1", map[string]any{"role": "user", "content": content}, false); err != nil {
		t.Fatal(err)
	}
}

func TestAReplayedRowIsTheRowTheLiveWriterWrites(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	toolCalls := []any{map[string]any{"id": "call-1", "type": "function",
		"function": map[string]any{"name": "send_payment", "arguments": "{}"}}}
	assistant := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls,
		"reasoning": "deliberating", "reasoning_content": "chain",
		"reasoning_details": []any{map[string]any{"type": "text"}}, "codex_reasoning_items": []any{map[string]any{"id": "r1"}},
		"codex_message_items": []any{map[string]any{"id": "m1"}}, "platform_message_id": "tg-42",
		"observed": true, "timestamp": int64(0), "api_content": "exact bytes sent to the API",
		"display_kind": "internal_notification"}
	tool := map[string]any{"role": "tool", "content": "receipt-1", "tool_call_id": "call-1", "tool_name": "send_payment"}
	user := map[string]any{"role": "user", "content": "hi", "reasoning": "leaked", "message_id": "tg-7"}
	writeCapDropSpool(t, dir, "pending-ccc.json", "sess-1", assistant, "100", "0")
	writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", tool, "100", "1")
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", user, "999", "2")
	recovery, live := &sequenceAppender{}, &sequenceAppender{}
	if n, err := RecoverPendingToDB(ctx, recovery, nil); n != 3 || err != nil {
		t.Fatalf("recovered %d %v", n, err)
	}
	store := flushTestStore(ctx, t, live)
	for _, message := range []map[string]any{assistant, tool, user} {
		if err := store.appendTranscriptMessage(ctx, "sess-1", message); err != nil {
			t.Fatal(err)
		}
	}
	popStamps := func(calls []appendCall) ([]string, []string) {
		var stamps, rows []string
		for _, c := range calls {
			fields := maps.Clone(c.fields)
			fields["session_id"] = c.sessionID
			stamp, _ := pyjson.Dumps(fields["timestamp"])
			stamps = append(stamps, stamp)
			delete(fields, "timestamp")
			row, err := pyjson.Dumps(fields, pyjson.SortKeys(true))
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
		return stamps, rows
	}
	replayedStamps, replayed := popStamps(recovery.calls)
	writtenStamps, written := popStamps(live.calls)
	if !reflect.DeepEqual(replayedStamps, []string{"0", "100", "999"}) || !reflect.DeepEqual(writtenStamps, []string{"0", "null", "null"}) {
		t.Fatalf("stamps %v %v", replayedStamps, writtenStamps)
	}
	if !reflect.DeepEqual(replayed, written) {
		t.Fatalf("replayed %v\nwritten %v", replayed, written)
	}
	first, second, third := recovery.calls[0].fields, recovery.calls[1].fields, recovery.calls[2].fields
	if !pyjsonEqual(first["tool_calls"], toolCalls) || first["api_content"] != "exact bytes sent to the API" {
		t.Fatalf("assistant row %v", first)
	}
	if second["tool_call_id"] != "call-1" || second["tool_name"] != "send_payment" {
		t.Fatalf("tool row %v", second)
	}
	if third["reasoning"] != nil || third["platform_message_id"] != "tg-7" {
		t.Fatalf("user row %v", third)
	}
}

func TestHeldBackFilesDrainBeforeThatSessionsNextLiveWrite(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "user", "content": "old0"}, "100", "0")
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "old1"}, "100", "1")
	db := &sequenceAppender{results: []error{errors.New("still locked at boot")}}
	useBootDB(t, db)
	store := flushTestStore(ctx, t, db)
	if n := bootRecover(ctx, t, store); n != 0 {
		t.Fatalf("boot recovered %d", n)
	}
	liveAppend(ctx, t, store, "new-live")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old0", "old0", "old1", "new-live"}) {
		t.Fatalf("contents %v", got)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
}

func TestLiveWriteWaitsWhileHeldBackFilesStillFailToDrain(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "user", "content": "old0"}, "100", "0")
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "old1"}, "100", "1")
	db := &sequenceAppender{results: []error{errors.New("still locked at boot"), errors.New("still locked at drain")}}
	useBootDB(t, db)
	store := flushTestStore(ctx, t, db)
	if n := bootRecover(ctx, t, store); n != 0 {
		t.Fatalf("boot recovered %d", n)
	}
	liveAppend(ctx, t, store, "new-live")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old0", "old0"}) {
		t.Fatalf("contents %v", got)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 2 {
		t.Fatalf("files %v", files)
	}
	liveAppend(ctx, t, store, "new-live-2")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old0", "old0", "old0", "old1", "new-live", "new-live-2"}) {
		t.Fatalf("contents %v", got)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

func TestAPoisonedSpoolRowDoesNotStopLaterLiveRows(t *testing.T) {
	ctx, db := newTestStateDB(t)
	dir := useFlushDir(t)
	createTestSession(t, db, state.NewSession{ID: "sess-1", Source: "telegram"})
	writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "tool", "content": "result", "tool_call_id": map[string]any{"bad": int64(1)}}, "100", "0")
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "older"}, "100", "1")
	big := `{"session_key": "sess-1", "reason": "` + TranscriptCapDropReason + `", "ts": 100, "seq": 2, "data": {"session_id": "sess-1", ` +
		`"message": {"role": "tool", "content": "big id", "tool_call_id": 1267650600228229401496703205376}}}`
	if err := os.WriteFile(filepath.Join(dir, "pending-ccc.json"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	useBootDB(t, db)
	store := flushTestStore(ctx, t, db)
	if n := bootRecover(ctx, t, store); n != 3 {
		t.Fatalf("boot recovered %d", n)
	}
	liveAppend(ctx, t, store, "live")
	if got := transcriptContents(ctx, t, db, "sess-1"); !reflect.DeepEqual(got, []any{"result", "older", "big id", "live"}) {
		t.Fatalf("rows %v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

func TestARowTheDatabaseRejectsIsQuarantinedNotRetried(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	db := &sequenceAppender{fn: func(fields map[string]any) error {
		if content, _ := fields["content"].(string); strings.HasPrefix(content, "poison") {
			return &state.SQLiteError{Class: state.SQLiteInterfaceError, Msg: "Error binding parameter 3: unsupported type"}
		}
		return nil
	}}
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "poison at boot"}, "100", "0")
	writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "user", "content": "after"}, "100", "1")
	useBootDB(t, db)
	store := flushTestStore(ctx, t, db)
	if n := bootRecover(ctx, t, store); n != 1 {
		t.Fatalf("boot recovered %d", n)
	}
	writeCapDropSpool(t, dir, "pending-ccc.json", "sess-1", map[string]any{"role": "user", "content": "poison live"}, "200", "2")
	writeCapDropSpool(t, dir, "pending-ddd.json", "sess-1", map[string]any{"role": "user", "content": "after live"}, "200", "3")
	store.markSpooledDropSessions(map[string]bool{"sess-1": true})
	liveAppend(ctx, t, store, "live")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"poison at boot", "after", "poison live", "after live", "live"}) {
		t.Fatalf("contents %v", got)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{"pending-aaa.json" + QuarantineSuffix, "pending-ccc.json" + QuarantineSuffix}) {
		t.Fatalf("names %v", names)
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

func TestTransientNoMoreRowsErrorKeepsTheRowForRetry(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	transient := &state.SQLiteError{Class: state.SQLiteInterfaceError, Msg: "no more rows available"}
	path := writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "old"}, "100", "0")
	db := &sequenceAppender{results: []error{transient, transient}}
	useBootDB(t, db)
	store := flushTestStore(ctx, t, db)
	if n := bootRecover(ctx, t, store); n != 0 {
		t.Fatalf("boot recovered %d", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("spool row gone after boot")
	}
	liveAppend(ctx, t, store, "live")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old", "old"}) {
		t.Fatalf("contents %v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("spool row gone after the failed drain")
	}
	liveAppend(ctx, t, store, "live-2")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old", "old", "old", "live", "live-2"}) {
		t.Fatalf("contents %v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

func TestAReplayedFileThatCannotBeDeletedIsNotReplayedAgain(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "old0"}, "100", "0")
	second := writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "user", "content": "old1"}, "100", "1")
	failSpoolRemoveFor(t, "pending-aaa.json")
	db := &sequenceAppender{}
	store := flushTestStore(ctx, t, db)
	store.markSpooledDropSessions(map[string]bool{"sess-1": true})
	liveAppend(ctx, t, store, "live")
	store.markSpooledDropSessions(map[string]bool{"sess-1": true})
	liveAppend(ctx, t, store, "live-2")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old0", "old1", "live", "live-2"}) {
		t.Fatalf("contents %v", got)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatal("second spool file kept")
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

func TestBootRecoveryCountsAndNeverRepeatsAFileItCouldNotDelete(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "old0"}, "100", "0")
	writeCapDropSpool(t, dir, "pending-bbb.json", "sess-1", map[string]any{"role": "user", "content": "old1"}, "100", "1")
	failSpoolRemoveFor(t, "pending-aaa.json")
	db := &sequenceAppender{}
	useBootDB(t, db)
	store := flushTestStore(ctx, t, db)
	if n := bootRecover(ctx, t, store); n != 2 {
		t.Fatalf("boot recovered %d", n)
	}
	store.markSpooledDropSessions(map[string]bool{"sess-1": true})
	liveAppend(ctx, t, store, "live")
	if got := appendedContents(db.calls); !reflect.DeepEqual(got, []any{"old0", "old1", "live"}) {
		t.Fatalf("contents %v", got)
	}
}

// Store double whose appends fail on the FTS index until one rebuild repairs it
type repairableDB struct {
	SessionStoreDB
	broken  bool
	rows    []any
	repairs int
}

func (r *repairableDB) AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error) {
	if r.broken {
		return 0, errors.New("no such table: messages_fts")
	}
	r.rows = append(r.rows, fields["content"])
	return int64(len(r.rows)), nil
}

func (r *repairableDB) RebuildFTS(ctx context.Context) (int, error) {
	r.repairs++
	r.broken = false
	return 1, nil
}

func TestRepairableSpoolReplayFailureStillReachesFTSRebuild(t *testing.T) {
	ctx, _ := newTestStateDB(t)
	dir := useFlushDir(t)
	writeCapDropSpool(t, dir, "pending-aaa.json", "sess-1", map[string]any{"role": "user", "content": "old"}, "100", "0")
	db := &repairableDB{broken: true}
	store := flushTestStore(ctx, t, db)
	store.markSpooledDropSessions(map[string]bool{"sess-1": true})
	liveAppend(ctx, t, store, "live")
	if db.repairs != 1 || !reflect.DeepEqual(db.rows, []any{"old", "live"}) {
		t.Fatalf("repairs %d rows %v", db.repairs, db.rows)
	}
	if files := spoolJSONFiles(t, dir); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
	if len(store.dirtyTranscripts) != 0 {
		t.Fatalf("dirty %v", store.dirtyTranscripts)
	}
}

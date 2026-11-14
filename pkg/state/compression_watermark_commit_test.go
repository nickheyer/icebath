package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var watermarkSummary = []map[string]any{
	{"role": "user", "content": "[CONTEXT COMPACTION] summary of turns 0-5"},
	{"role": "assistant", "content": "Continuing from the summary."},
}

func watermarkDB(t *testing.T, n int) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "sess1", Source: "test"})
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		wmAppend(t, db, "sess1", map[string]any{"role": role, "content": "turn " + pyStr(i)}, AppendGuards{})
	}
	return db
}

func wmAppend(t *testing.T, db *SessionDB, sid string, fields map[string]any, guards AppendGuards) {
	t.Helper()
	if _, err := db.AppendMessage(context.Background(), sid, fields, guards); err != nil {
		t.Fatal(err)
	}
}

func wmMark(t *testing.T, db *SessionDB) *int64 {
	t.Helper()
	w, err := db.GetActiveMessageWatermark(context.Background(), "sess1")
	if err != nil {
		t.Fatal(err)
	}
	return &w
}

func wmContents(t *testing.T, db *SessionDB, sid string, inactive bool) []any {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{IncludeInactive: inactive})
	if err != nil {
		t.Fatal(err)
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, rowValue(r, "content"))
	}
	return out
}

func wmConversation(t *testing.T, db *SessionDB, sid string) []any {
	t.Helper()
	conv, err := db.GetMessagesAsConversation(context.Background(), sid, ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := []any{}
	for _, m := range conv {
		out = append(out, m["content"])
	}
	return out
}

func wmCompact(t *testing.T, db *SessionDB, o ArchiveCompactOptions) (int, error) {
	t.Helper()
	return db.ArchiveAndCompact(context.Background(), "sess1", watermarkSummary, o)
}

func holderPtr(s string) *string { return &s }

func TestConcurrentTailSurvivesCompaction(t *testing.T) {
	db := watermarkDB(t, 6)
	w := wmMark(t, db)
	wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": "mid-compression steer"}, AppendGuards{})
	wmAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "mid-compression reply"}, AppendGuards{})
	count, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w})
	want := []any{"[CONTEXT COMPACTION] summary of turns 0-5", "Continuing from the summary.", "mid-compression steer", "mid-compression reply"}
	if err != nil || count != 4 || !reflect.DeepEqual(wmContents(t, db, "sess1", false), want) {
		t.Fatalf("count %d %v live %v", count, err, wmContents(t, db, "sess1", false))
	}
}

func TestTailClonePreservesEveryColumn(t *testing.T) {
	db := watermarkDB(t, 2)
	w := wmMark(t, db)
	wmAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "tool caller", "tool_calls": []any{
		map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}, AppendGuards{})
	wmAppend(t, db, "sess1", map[string]any{"role": "tool", "content": "tool output", "tool_call_id": "c1", "tool_name": "terminal"}, AppendGuards{})
	if _, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(context.Background(), "sess1", GetMessagesOptions{})
	byContent := map[any]map[string]any{}
	for _, r := range rows {
		m := map[string]any{}
		for _, k := range r.Keys {
			m[k] = r.Vals[k]
		}
		byContent[m["content"]] = m
	}
	parsed := byContent["tool caller"]["tool_calls"]
	if text, ok := parsed.(string); ok {
		parsed = mustLoads(t, text)
	}
	if text := tailKey(parsed); !strings.HasPrefix(text, "[{") || !strings.Contains(text, `"id": "c1"`) {
		t.Fatalf("tool calls %v", parsed)
	}
	if byContent["tool output"]["tool_call_id"] != "c1" || byContent["tool output"]["tool_name"] != "terminal" {
		t.Fatalf("tool row %v", byContent["tool output"])
	}
}

func TestConversationLoadIsCorrectAfterCommit(t *testing.T) {
	db := watermarkDB(t, 6)
	w := wmMark(t, db)
	wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": "late arrival"}, AppendGuards{})
	if _, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w}); err != nil {
		t.Fatal(err)
	}
	want := []any{"[CONTEXT COMPACTION] summary of turns 0-5", "Continuing from the summary.", "late arrival"}
	if got := wmConversation(t, db, "sess1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("conversation %v", got)
	}
}

func TestNoTailBehavesIdenticallyToLegacyAndNoneWatermarkArchivesAll(t *testing.T) {
	db := watermarkDB(t, 6)
	if count, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: wmMark(t, db)}); err != nil || count != 2 {
		t.Fatalf("count %d %v", count, err)
	}
	if got := wmContents(t, db, "sess1", false); !reflect.DeepEqual(got, []any{watermarkSummary[0]["content"], watermarkSummary[1]["content"]}) {
		t.Fatalf("live %v", got)
	}
	other := watermarkDB(t, 6)
	wmAppend(t, other, "sess1", map[string]any{"role": "user", "content": "gets archived"}, AppendGuards{})
	if count, err := wmCompact(t, other, ArchiveCompactOptions{}); err != nil || count != 2 {
		t.Fatalf("none watermark count %d %v", count, err)
	}
	for _, c := range wmContents(t, other, "sess1", false) {
		if c == "gets archived" {
			t.Fatal("a None watermark kept the tail")
		}
	}
}

func TestArchivedRowsStayRecoverable(t *testing.T) {
	db := watermarkDB(t, 4)
	w := wmMark(t, db)
	wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": "tail row"}, AppendGuards{})
	if _, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.GetMessages(context.Background(), "sess1", GetMessagesOptions{IncludeInactive: true})
	turnZero, tailActive := 0, []bool{}
	for _, r := range rows {
		active := pyTruthy(rowValue(r, "active"))
		if !active && rowValue(r, "content") == "turn 0" {
			turnZero++
		}
		if rowValue(r, "content") == "tail row" {
			tailActive = append(tailActive, active)
		}
	}
	if turnZero != 1 || len(tailActive) != 2 || tailActive[0] == tailActive[1] {
		t.Fatalf("archived turn 0 %d tail %v", turnZero, tailActive)
	}
}

func TestSessionCountersIncludeTail(t *testing.T) {
	db := watermarkDB(t, 6)
	w := wmMark(t, db)
	wmAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "tail with tools", "tool_calls": []any{
		map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}}}}, AppendGuards{})
	if _, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w}); err != nil {
		t.Fatal(err)
	}
	info, _ := db.GetSession(context.Background(), "sess1")
	if rowValue(info, "message_count") != int64(3) || rowValue(info, "tool_call_count") != int64(1) {
		t.Fatalf("counters %v %v", rowValue(info, "message_count"), rowValue(info, "tool_call_count"))
	}
}

func wantInProgress(t *testing.T, err error) {
	t.Helper()
	var target *SessionCompressionInProgressError
	if !errors.As(err, &target) {
		t.Fatalf("want SessionCompressionInProgressError, got %v", err)
	}
}

func TestCommitFence(t *testing.T) {
	db := watermarkDB(t, 6)
	w := wmMark(t, db)
	if !db.TryAcquireCompressionLock("sess1", "worker-A", 300) {
		t.Fatal("worker-A lock")
	}
	db.ReleaseCompressionLock("sess1", "worker-A")
	if !db.TryAcquireCompressionLock("sess1", "worker-B", 300) {
		t.Fatal("worker-B lock")
	}
	_, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w, LockHolder: holderPtr("worker-A")})
	wantInProgress(t, err)
	want := []any{"turn 0", "turn 1", "turn 2", "turn 3", "turn 4", "turn 5"}
	if got := wmContents(t, db, "sess1", false); !reflect.DeepEqual(got, want) {
		t.Fatalf("lost lease committed %v", got)
	}
	expired := watermarkDB(t, 6)
	if !expired.TryAcquireCompressionLock("sess1", "worker-A", 0.05) {
		t.Fatal("short lock")
	}
	time.Sleep(100 * time.Millisecond)
	_, err = wmCompact(t, expired, ArchiveCompactOptions{LockHolder: holderPtr("worker-A")})
	wantInProgress(t, err)
	live := watermarkDB(t, 6)
	lw := wmMark(t, live)
	if !live.TryAcquireCompressionLock("sess1", "worker-A", 300) {
		t.Fatal("live lock")
	}
	if count, err := wmCompact(t, live, ArchiveCompactOptions{Watermark: lw, LockHolder: holderPtr("worker-A")}); err != nil || count != 2 {
		t.Fatalf("live holder %d %v", count, err)
	}
	never := watermarkDB(t, 6)
	before, _ := never.GetMessages(context.Background(), "sess1", GetMessagesOptions{IncludeInactive: true})
	_, err = wmCompact(t, never, ArchiveCompactOptions{LockHolder: holderPtr("never-held")})
	wantInProgress(t, err)
	after, _ := never.GetMessages(context.Background(), "sess1", GetMessagesOptions{IncludeInactive: true})
	if len(before) != len(after) {
		t.Fatalf("rows %d -> %d", len(before), len(after))
	}
	for _, r := range after {
		if !pyTruthy(rowValue(r, "active")) {
			t.Fatal("refused commit archived a row")
		}
	}
}

func TestAppendRacingTheCommitTransaction(t *testing.T) {
	db := watermarkDB(t, 6)
	w := wmMark(t, db)
	var wg sync.WaitGroup
	start := make(chan struct{})
	var appendErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, appendErr = db.AppendMessage(context.Background(), "sess1", map[string]any{"role": "user", "content": "racer"}, AppendGuards{})
	}()
	close(start)
	if _, err := wmCompact(t, db, ArchiveCompactOptions{Watermark: w}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if appendErr != nil {
		t.Fatalf("append died during commit race %v", appendErr)
	}
	found := false
	for _, c := range wmContents(t, db, "sess1", false) {
		found = found || c == "racer"
	}
	if !found {
		t.Fatal("racing append was lost")
	}
}

func rotationPublish(w, ceiling *int64) CompressionChildPublish {
	p := NewCompressionChildPublish("sess1", "child1", "test", watermarkSummary)
	p.CompressionLockHolder = holderPtr("rotator")
	p.Watermark, p.WatermarkCeiling = w, ceiling
	return p
}

func TestRotationTailClonesIntoTheChild(t *testing.T) {
	for _, tail := range []any{"mid-rotation steer", []any{map[string]any{"type": "text", "text": "inspect this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}} {
		db := watermarkDB(t, 6)
		w := wmMark(t, db)
		if !db.TryAcquireCompressionLock("sess1", "rotator", 300) {
			t.Fatal("rotator lock")
		}
		wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": tail}, AppendGuards{})
		ceiling := wmMark(t, db)
		if err := db.PublishCompressionChild(context.Background(), rotationPublish(w, ceiling)); err != nil {
			t.Fatal(err)
		}
		child := wmConversation(t, db, "child1")
		if len(child) != 3 || child[0] != watermarkSummary[0]["content"] || child[1] != watermarkSummary[1]["content"] || tailKey(child[2]) != tailKey(tail) {
			t.Fatalf("child %v", child)
		}
		model, display, err := db.GetResumeConversations(context.Background(), "child1")
		if err != nil {
			t.Fatal(err)
		}
		var visible []map[string]any
		for _, m := range display {
			if tailKey(m["content"]) == tailKey(tail) {
				visible = append(visible, m)
			}
		}
		if len(visible) != 1 || !reflect.DeepEqual(visible[0]["_row_id"], model[len(model)-1]["_row_id"]) {
			t.Fatalf("visible steers %v", visible)
		}
		prefix, err := db.GetAncestorDisplayPrefix(context.Background(), "child1")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range prefix {
			if tailKey(m["content"]) == tailKey(tail) {
				t.Fatal("tail painted in the ancestor prefix")
			}
		}
		info, _ := db.GetSession(context.Background(), "child1")
		parent, _ := db.GetSession(context.Background(), "sess1")
		if rowValue(info, "message_count") != int64(3) || rowValue(parent, "end_reason") != "compression" {
			t.Fatalf("child count %v parent %v", rowValue(info, "message_count"), rowValue(parent, "end_reason"))
		}
	}
}

func tailKey(v any) string {
	text, err := pyDumps(v, pyJSONDefault)
	if err != nil {
		return "unencodable"
	}
	return text
}

func TestCeilingExcludesTheRotatorsOwnFlush(t *testing.T) {
	db := watermarkDB(t, 6)
	w := wmMark(t, db)
	if !db.TryAcquireCompressionLock("sess1", "rotator", 300) {
		t.Fatal("rotator lock")
	}
	wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": "foreign steer"}, AppendGuards{})
	ceiling := wmMark(t, db)
	wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": "rotator's own flush"}, AppendGuards{CompressionLockHolder: holderPtr("rotator")})
	if err := db.PublishCompressionChild(context.Background(), rotationPublish(w, ceiling)); err != nil {
		t.Fatal(err)
	}
	got := wmConversation(t, db, "child1")
	hasForeign, hasOwn := false, false
	for _, c := range got {
		hasForeign = hasForeign || c == "foreign steer"
		hasOwn = hasOwn || c == "rotator's own flush"
	}
	if !hasForeign || hasOwn {
		t.Fatalf("child %v", got)
	}
}

func TestNoWatermarkKeepsHistoricalRotation(t *testing.T) {
	db := watermarkDB(t, 6)
	if !db.TryAcquireCompressionLock("sess1", "rotator", 300) {
		t.Fatal("rotator lock")
	}
	wmAppend(t, db, "sess1", map[string]any{"role": "user", "content": "stranded either way"}, AppendGuards{})
	if err := db.PublishCompressionChild(context.Background(), rotationPublish(nil, nil)); err != nil {
		t.Fatal(err)
	}
	if got := wmConversation(t, db, "child1"); !reflect.DeepEqual(got, []any{watermarkSummary[0]["content"], watermarkSummary[1]["content"]}) {
		t.Fatalf("child %v", got)
	}
}

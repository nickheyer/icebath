package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/state"
)

func newRecoveryDB(t *testing.T) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(t.Context(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func appendRecoveryRow(db *state.SessionDB, sid, content string) error {
	_, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": "user", "content": content}, state.AppendGuards{})
	return err
}

func TestRecoverRotatedCompressionSessionReopensLegacyOrphan(t *testing.T) {
	db := newRecoveryDB(t)
	if err := db.CreateSession(state.NewSession{ID: "orphan", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := appendRecoveryRow(db, "orphan", "before compression"); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("orphan", "compression"); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{sessionDB: db, sessionID: "orphan"}
	if got, ok := recoverRotatedCompressionSession(t.Context(), a); ok || got != nil {
		t.Fatalf("recovered %v", got)
	}
	if err := appendRecoveryRow(db, "orphan", "after recovery"); err != nil {
		t.Fatalf("orphan stayed closed: %v", err)
	}
}

func TestRecoverRotatedCompressionSessionKeepsParentClosedWithChild(t *testing.T) {
	db := newRecoveryDB(t)
	for _, err := range []error{
		db.CreateSession(state.NewSession{ID: "parent", Source: "cli"}),
		appendRecoveryRow(db, "parent", "before compression"),
		db.EndSession("parent", "compression"),
		db.CreateSession(state.NewSession{ID: "child", Source: "cli", ParentSessionID: "parent"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	a := &AIAgent{sessionDB: db, sessionID: "parent"}
	if got, ok := recoverRotatedCompressionSession(t.Context(), a); ok || got != nil {
		t.Fatalf("recovered %v", got)
	}
	var closed *state.CompressionSessionClosedError
	if err := appendRecoveryRow(db, "parent", "must stay closed"); !errors.As(err, &closed) {
		t.Fatalf("compression parent with child was reopened: %v", err)
	}
}

type recoveryEngine struct {
	*ContextCompressor
	starts []map[string]any
}

func (e *recoveryEngine) OnSessionStart(sessionID string, kwargs map[string]any) {
	kwargs["session_id"] = sessionID
	e.starts = append(e.starts, kwargs)
}

func TestRecoverRotatedCompressionSessionStopsPollingAfterTwentyOneTries(t *testing.T) {
	old := compressionRecoveryPoll
	compressionRecoveryPoll = time.Millisecond
	t.Cleanup(func() { compressionRecoveryPoll = old })
	db := newRecoveryDB(t)
	for _, err := range []error{
		db.CreateSession(state.NewSession{ID: "parent", Source: "cli"}),
		db.EndSession("parent", "compression"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !db.TryAcquireCompressionLock("parent", "rival", 300) {
		t.Fatal("lock not taken")
	}
	a := &AIAgent{sessionDB: db, sessionID: "parent"}
	start := time.Now()
	if got, ok := recoverRotatedCompressionSession(t.Context(), a); ok || got != nil {
		t.Fatalf("recovered %v", got)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("recovery returned before waiting out the rotation hand-off")
	}
	if holder, _ := db.GetCompressionLockHolder("parent"); holder == nil || *holder != "rival" {
		t.Fatal("rival lease disturbed")
	}
}

func TestCompressionLockHolderIsUniquePerAcquire(t *testing.T) {
	a := &AIAgent{}
	h1, h2 := compressionLockHolder(a), compressionLockHolder(a)
	if h1 == h2 || !strings.HasPrefix(h1, "pid=") || !strings.Contains(h1, ":tid=") || !strings.Contains(h1, ":agent=") || !strings.Contains(h1, ":nonce=") {
		t.Fatalf("holders %q %q", h1, h2)
	}
}

func TestContextEngineCompressionNotificationIsStagedOnce(t *testing.T) {
	engine := &recoveryEngine{ContextCompressor: newCompressorWith(t, 128_000, nil)}
	a := &AIAgent{}
	a.contextCompressor = engine
	if err := queueContextEngineCompressionNotification(t.Context(), a, "new", "old"); err != nil {
		t.Fatal(err)
	}
	if err := queueContextEngineCompressionNotification(t.Context(), a, "new", "old"); err == nil || err.Error() != "a compression notification is already pending" {
		t.Fatalf("second stage: %v", err)
	}
	if finalizeContextEngineCompressionNotification(a, false) || len(engine.starts) != 0 {
		t.Fatal("discarded notification fired")
	}
	if finalizeContextEngineCompressionNotification(a, true) {
		t.Fatal("cleared slot fired")
	}
	_ = queueContextEngineCompressionNotification(t.Context(), a, "new", "old")
	if !finalizeContextEngineCompressionNotification(a, true) || len(engine.starts) != 1 || engine.starts[0]["platform"] != "cli" || engine.starts[0]["conversation_id"] != nil {
		t.Fatalf("committed notification %v", engine.starts)
	}
	if finalizeContextEngineCompressionNotification(a, true) {
		t.Fatal("repeated finalize fired again")
	}
}

type refreshCountDB struct {
	*state.SessionDB
	refreshes atomic.Int32
	ok        bool
}

func (d *refreshCountDB) RefreshCompressionLock(string, string, float64) bool {
	d.refreshes.Add(1)
	return d.ok
}

func TestLeaseRefresherRefreshesAtOnceAndStopsAfterATTLOfFailures(t *testing.T) {
	db := &refreshCountDB{}
	interval := 0.1
	r := newCompressionLockLeaseRefresher(db, "s", "h", 0.3, &interval)
	if r.maxConsecutiveFailures != 2 {
		t.Fatalf("max failures %d", r.maxConsecutiveFailures)
	}
	r.start()
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("refresher outlived its TTL")
	}
	if n := db.refreshes.Load(); n != 2 {
		t.Fatalf("refreshes %d", n)
	}
	healthy := &refreshCountDB{ok: true}
	r = newCompressionLockLeaseRefresher(healthy, "s", "h", 300, nil)
	if r.interval != 60*time.Second {
		t.Fatalf("default interval %v", r.interval)
	}
	r.start()
	deadline := time.Now().Add(time.Second)
	for healthy.refreshes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.stop()
	if healthy.refreshes.Load() != 1 {
		t.Fatalf("first refresh not immediate: %d", healthy.refreshes.Load())
	}
}

func TestHeartbeatSuppressesAfterATerminalStampAndAFenceCancel(t *testing.T) {
	a := &AIAgent{}
	fence := NewCompressionCommitFence()
	h := newCompressionActivityHeartbeat(a, 0.1, false, fence).start()
	if a.lastActivityProvenanceValue() != state.ActivityProvenanceAgentCompression {
		t.Fatalf("start provenance %q", a.lastActivityProvenanceValue())
	}
	a.touchActivity("timed out", string(state.ActivityProvenanceAgentCompressionTimeout), false)
	time.Sleep(250 * time.Millisecond)
	h.stop("context compression completed")
	if a.lastActivityProvenanceValue() != state.ActivityProvenanceAgentCompressionTimeout {
		t.Fatal("heartbeat overwrote the terminal timeout stamp")
	}
	b := &AIAgent{}
	f2 := NewCompressionCommitFence()
	h2 := newCompressionActivityHeartbeat(b, nil, false, f2).start()
	if h2.interval != 60*time.Second {
		t.Fatalf("default interval %v", h2.interval)
	}
	f2.CancelBeforeCommit(nil)
	b.touchActivity("host", string(state.ActivityProvenanceAgentCompressionCooldown), false)
	h2.stop("context compression completed")
	if b.lastActivityProvenanceValue() != state.ActivityProvenanceAgentCompressionCooldown {
		t.Fatal("cancelled heartbeat republished completion")
	}
	for _, v := range []any{"abc", 0, nil, "", 0.01} {
		if got := heartbeatSeconds(v); v == 0.01 && got != 0.01 || v != 0.01 && got != 60 {
			t.Fatalf("heartbeatSeconds(%v) = %v", v, got)
		}
	}
}

func TestDirectMessagesForPreCompressMemoryKeepsOnlyDirectProse(t *testing.T) {
	in := []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "q"},
		{"role": "assistant", "content": "  ", "tool_calls": []any{"x"}},
		{"role": "assistant", "content": "prose", "tool_calls": []any{"x"}},
		{"role": "tool", "content": "out"},
		{"role": "user", "content": "s", state.CompressedSummaryMetadataKey: true},
		{"role": "assistant", "content": "a"},
	}
	got := directMessagesForPreCompressMemory(in)
	if len(got) != 3 || got[0]["content"] != "q" || got[1]["content"] != "prose" || got[1]["tool_calls"] != nil || got[2]["content"] != "a" {
		t.Fatalf("got %v", got)
	}
	if in[3]["tool_calls"] == nil {
		t.Fatal("caller row mutated")
	}
}

func TestSupportedCompressionKwargsFollowsTheEngineSurface(t *testing.T) {
	c := newCompressorWith(t, 128_000, nil)
	got := supportedCompressionKwargs(c, iptr(5), nil, true, "mem", true)
	if len(got) != 5 || got["bypass_cooldown"] != true || got["memory_context"] != "mem" {
		t.Fatalf("built in %v", got)
	}
	plain := supportedCompressionKwargs(c, iptr(5), nil, false, "", false)
	if _, has := plain["memory_context"]; has || len(plain) != 3 {
		t.Fatalf("plain %v", plain)
	}
	limited := supportedCompressionKwargs(keywordEngine{c}, iptr(5), nil, false, "mem", true)
	if len(limited) != 1 || limited["current_tokens"] == nil {
		t.Fatalf("inspected %v", limited)
	}
}

type keywordEngine struct{ *ContextCompressor }

func (keywordEngine) AcceptsCompressKeyword(keyword string) bool { return keyword == "current_tokens" }

func TestCompactionLifecycleTerminalEdge(t *testing.T) {
	var events []string
	a := &AIAgent{statusCallback: func(kind, text string) { events = append(events, kind) }}
	l := newCompactionLifecycle(a, true)
	l.complete(false)
	l.complete(true)
	if len(events) != 0 {
		t.Fatal("aborted lifecycle emitted the done edge")
	}
	l = newCompactionLifecycle(a, true)
	l.commitStatus = "committed"
	l.complete(false)
	l.complete(false)
	if len(events) != 1 {
		t.Fatalf("committed edge emitted %d times", len(events))
	}
	newCompactionLifecycle(a, false).complete(true)
	if len(events) != 1 {
		t.Fatal("suppressed start emitted a terminal edge")
	}
}

func TestCompressionLeaseAcquireContendAndRelease(t *testing.T) {
	db := newRecoveryDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{sessionDB: db, sessionID: "s", cachedSystemPrompt: "sp"}
	attempt := &compressionAttempt{startedAt: time.Now(), seed: pyjson.NewObject()}
	fence := NewCompressionCommitFence()
	lease, prompt, err := acquireCompressionLease(t.Context(), a, fence, newCompactionLifecycle(a, false), nil, attempt)
	if err != nil || lease == nil || prompt != "" || lease.holder == nil || lease.watermark == nil {
		t.Fatalf("lease %v %q %v", lease, prompt, err)
	}
	lease.finishLockSetup()
	if a.activeCompressionLockHolder == nil || *a.activeCompressionLockHolder != *lease.holder || !fence.CommitWatermarkFenced() {
		t.Fatal("holder not published")
	}
	rival := &AIAgent{sessionDB: db, sessionID: "s", cachedSystemPrompt: "rival sp"}
	r2, p2, err := acquireCompressionLease(t.Context(), rival, nil, newCompactionLifecycle(rival, false), nil, attempt)
	if err != nil || r2 != nil || p2 != "rival sp" || rival.compressionSkippedDueToLock != *lease.holder {
		t.Fatalf("contender %v %q %v %v", r2, p2, err, rival.compressionSkippedDueToLock)
	}
	if rival.lastCompressionLockWarningSID == nil || *rival.lastCompressionLockWarningSID != "s" {
		t.Fatal("contention warning latch not set")
	}
	lease.startRefresher()
	lease.release()
	lease.release()
	if a.activeCompressionLockHolder != nil {
		t.Fatal("holder slot not cleared")
	}
	if holder, _ := db.GetCompressionLockHolder("s"); holder != nil {
		t.Fatalf("lock still held by %q", *holder)
	}
	cancelled := NewCompressionCommitFence()
	cancelled.CancelBeforeCommit(nil)
	none, p3, err := acquireCompressionLease(t.Context(), a, cancelled, newCompactionLifecycle(a, false), nil, attempt)
	if err != nil || none != nil || p3 != "sp" || a.lastCompactionInPlace {
		t.Fatalf("cancelled fence %v %q %v", none, p3, err)
	}
	if holder, _ := db.GetCompressionLockHolder("s"); holder != nil {
		t.Fatal("cancelled fence acquired the lock")
	}
}

func TestAdoptGrownDurableParentAdvancesTheWatermark(t *testing.T) {
	db := newRecoveryDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"one", "two", "three"} {
		if err := appendRecoveryRow(db, "s", c); err != nil {
			t.Fatal(err)
		}
	}
	a := &AIAgent{sessionDB: db, sessionID: "s"}
	wm := int64(1)
	lease := &compressionLease{a: a, db: db, sid: "s", watermark: &wm}
	got, err := adoptGrownDurableParent(t.Context(), a, lease, []map[string]any{{"role": "user", "content": "one"}})
	if err != nil || len(got) != 3 || *lease.watermark != 3 {
		t.Fatalf("adopted %v %v wm=%d", got, err, *lease.watermark)
	}
	for _, m := range got {
		if _, has := m["_row_id"]; has {
			t.Fatal("row id leaked into the adopted transcript")
		}
	}
	same, _ := adoptGrownDurableParent(t.Context(), a, lease, got)
	if same != nil {
		t.Fatal("unchanged durable parent adopted")
	}
}

func TestCompressedUserTurnAnchorsTheLatestIntent(t *testing.T) {
	const requestA, steerB, requestC = "Historical request A: audit the auth module.", "Steer B: stop, switch to fixing the login bug instead.", "Newer real user request C: now write the release notes."
	toolTurns := func(start, count, steerAt int) []map[string]any {
		var out []map[string]any
		for i := start; i < start+count; i++ {
			id := "call-" + string(rune('0'+i))
			out = append(out, map[string]any{"role": "assistant", "content": "Working.", "tool_calls": []any{map[string]any{"id": id, "function": map[string]any{"name": "terminal", "arguments": "{}"}}}})
			content := "tool output " + string(rune('0'+i))
			if steerAt == i {
				content += FormatSteerMarker(steerB)
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
		}
		return out
	}
	summary := func() map[string]any {
		return map[string]any{"role": "user", "content": state.SummaryPrefix + "\n\nEarlier work summarized."}
	}
	users := func(ms []map[string]any) []string {
		var out []string
		for _, m := range ms {
			if m["role"] == "user" {
				out = append(out, strOrEmpty(m["content"]))
			}
		}
		return out
	}
	alternates := func(ms []map[string]any) {
		for i := 1; i < len(ms); i++ {
			if ms[i]["role"] == ms[i-1]["role"] && (ms[i]["role"] == "user" || ms[i]["role"] == "assistant") {
				t.Fatalf("adjacent %v at %d", ms[i]["role"], i)
			}
		}
	}
	countSteer := func(ms []map[string]any) int {
		n := 0
		for _, m := range ms {
			n += strings.Count(strOrEmpty(m["content"]), steerB)
		}
		return n
	}
	user := func(c string) map[string]any { return map[string]any{"role": "user", "content": c} }

	original := append([]map[string]any{user(requestA)}, toolTurns(0, 6, 2)...)
	compressed := append([]map[string]any{summary()}, toolTurns(5, 1, -1)...)
	if out := ensureCompressedHasUserTurn(original, &compressed); out != "inserted" {
		t.Fatalf("s1 outcome %q", out)
	}
	alternates(compressed)
	if u := users(compressed); !slices.Contains(u, steerB) || slices.Contains(u, requestA) || slices.Contains(u, state.CompressionContinuationUserContent) || countSteer(compressed) != 1 {
		t.Fatalf("s1 users %q", u)
	}

	original = append([]map[string]any{user(requestA)}, toolTurns(0, 6, 5)...)
	compressed = append([]map[string]any{summary()}, toolTurns(5, 1, 5)...)
	before := len(compressed)
	if out := ensureCompressedHasUserTurn(original, &compressed); out != "already_present" || len(compressed) != before || countSteer(compressed) != 1 {
		t.Fatalf("s2 outcome %q", out)
	}

	original = append(append(append([]map[string]any{user(requestA)}, toolTurns(0, 3, 1)...), user(requestC)), toolTurns(3, 4, -1)...)
	compressed = append([]map[string]any{summary()}, toolTurns(6, 1, -1)...)
	if out := ensureCompressedHasUserTurn(original, &compressed); out != "inserted" {
		t.Fatalf("s3 outcome %q", out)
	}
	alternates(compressed)
	if u := users(compressed); !slices.Contains(u, requestC) || slices.Contains(u, requestA) || slices.ContainsFunc(u, func(s string) bool { return strings.Contains(s, steerB) }) {
		t.Fatalf("s3 users %q", u)
	}

	original = append([]map[string]any{user(requestA)}, toolTurns(0, 4, 3)...)
	compressed = append([]map[string]any{summary()}, toolTurns(4, 1, -1)...)
	if out := ensureCompressedHasUserTurn(original, &compressed); out != "inserted" {
		t.Fatalf("newer steer outcome %q", out)
	}
	alternates(compressed)
	if u := users(compressed); !slices.Contains(u, steerB) || slices.Contains(u, requestA) {
		t.Fatalf("newer steer users %q", u)
	}

	for _, role := range []string{"user", "assistant"} {
		quoted := map[string]any{"role": role, "content": state.SummaryPrefix + "\n" + FormatSteerMarker(steerB)}
		if compressedHasBusySteer([]map[string]any{quoted}) || !strings.Contains(strOrEmpty(quoted["content"]), SteerMarkerOpen) {
			t.Fatalf("quoted %s steer counted", role)
		}
	}
	live := map[string]any{"role": "tool", "tool_call_id": "c", "content": "ok" + FormatSteerMarker(steerB)}
	if !compressedHasBusySteer([]map[string]any{live}) {
		t.Fatal("live tool steer not counted")
	}
}

func TestCompressedUserTurnPlaceholderMergeAndTwins(t *testing.T) {
	compressed := []map[string]any{{"role": "assistant", "content": "a"}}
	if out := ensureCompressedHasUserTurn(nil, &compressed); out != "placeholder_appended" || compressed[1]["content"] != state.CompressionContinuationUserContent {
		t.Fatalf("placeholder %q %v", out, compressed)
	}
	scaffold := []map[string]any{{"role": "assistant", "content": "a"}, {"role": "user", "content": "todo scaffolding", "_todo_snapshot_synthetic": true}}
	if out := insertRealUserAnchor(&scaffold, map[string]any{"role": "user", "content": "real ask"}); out != "inserted" || scaffold[0]["content"] != "real ask" {
		t.Fatalf("boundary insert %q %v", out, scaffold)
	}
	trailing := []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}, {"role": "user", "content": "todo scaffolding", "_todo_snapshot_synthetic": true}}
	if out := insertRealUserAnchor(&trailing, map[string]any{"role": "user", "content": "real ask"}); out != "merged" || len(trailing) != 3 ||
		trailing[2]["content"] != "real ask\n\ntodo scaffolding" || trailing[2]["_todo_snapshot_synthetic"] != nil || trailing[2][dbPersistedMarker] != true {
		t.Fatalf("merge %q %v", out, trailing)
	}
	src := map[string]any{"role": "user", "content": "hi", "timestamp": 5.0}
	targets := []map[string]any{{"role": "user", "content": "hi", "timestamp": 5}, {"role": "user", "content": "hi"}}
	stampScopedTwins(targets, src, false)
	if targets[0][dbPersistedMarker] != true || targets[1][dbPersistedMarker] != nil {
		t.Fatalf("exact twin pass %v", targets)
	}
	targets = []map[string]any{{"role": "user", "content": "hi", "timestamp": 5, dbPersistedMarker: true}, {"role": "user", "content": "hi"}}
	stampScopedTwins(targets, src, true)
	if targets[1][dbPersistedMarker] != nil {
		t.Fatal("stamped exact twin did not count as the hit")
	}
	stampScopedTwins(targets, src, false)
	if targets[1][dbPersistedMarker] != true {
		t.Fatal("broad pass skipped without an exact stamp")
	}
}

func TestTodoSnapshotStripAndSkillReloadNotice(t *testing.T) {
	text := "ask\n\n" + state.TodoInjectionHeader + "\n- a"
	if got := stripStaleTodoSnapshot(text); got != "ask" {
		t.Fatalf("string strip %q", got)
	}
	parts := []any{map[string]any{"type": "image_url"}, map[string]any{"type": "text", "text": state.TodoInjectionHeader + "\n- a"}, map[string]any{"type": "text", "text": "keep " + state.TodoInjectionHeader}}
	got := stripStaleTodoSnapshot(parts).([]any)
	if len(got) != 2 || got[1].(map[string]any)["text"] != "keep" {
		t.Fatalf("list strip %v", got)
	}
	if !todoSnapshotIsOnlyContent(text, "  ") || todoSnapshotIsOnlyContent(text, "ask") || !todoSnapshotIsOnlyContent(parts, []any{}) || todoSnapshotIsOnlyContent(text, []any{}) {
		t.Fatal("only content classification")
	}
	m := map[string]any{"content": "x", "api_content": "old"}
	replaceMessageContent(m, "y")
	if m["content"] != "y" || m["api_content"] != nil {
		t.Fatalf("replace %v", m)
	}
	if prunedSkillReloadNotice([]map[string]any{{"role": "user", "content": "none"}}) != "" {
		t.Fatal("notice without pruned skills")
	}
	notice := prunedSkillReloadNotice([]map[string]any{{"role": "tool", "content": "[SKILL_PRUNED: alpha] [SKILL_PRUNED: beta] [SKILL_PRUNED: alpha]"}})
	if notice != "" && (!strings.HasPrefix(notice, prunedSkillReloadNoticeHeader+"\n") || !strings.Contains(notice, "skill_view(name='alpha'); skill_view(name='beta')")) {
		t.Fatalf("notice %q", notice)
	}
}

func TestConversationHistoryAfterCompression(t *testing.T) {
	msgs := []map[string]any{{"role": "user"}}
	prev := []map[string]any{{"role": "assistant"}}
	a := &AIAgent{}
	if conversationHistoryAfterCompression(a, msgs, prev) != nil {
		t.Fatal("rotation keeps a baseline")
	}
	a.lastCompactionInPlace = true
	if got := conversationHistoryAfterCompression(a, msgs, prev); len(got) != 1 || &got[0] == &msgs[0] {
		t.Fatal("in place baseline is not a shallow copy")
	}
	a.lastCompressionAttemptRecorded = true
	if got := conversationHistoryAfterCompression(a, msgs, prev); len(got) != 1 || got[0]["role"] != "assistant" {
		t.Fatal("aborted attempt dropped the previous baseline")
	}
	f := false
	a.lastCompressionAttemptInPlace = &f
	if conversationHistoryAfterCompression(a, msgs, prev) != nil {
		t.Fatal("recorded rotation keeps a baseline")
	}
}

func TestRecoverRotatedCompressionSessionAdoptsTheLiveChild(t *testing.T) {
	t.Setenv("ICEBATH_SESSION_ID", "parent")
	db := newRecoveryDB(t)
	for _, err := range []error{
		db.CreateSession(state.NewSession{ID: "parent", Source: "cli"}),
		appendRecoveryRow(db, "parent", "before compression"),
		db.EndSession("parent", "compression"),
		db.CreateSession(state.NewSession{ID: "child", Source: "cli", ParentSessionID: "parent"}),
		appendRecoveryRow(db, "child", "summary carried"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	engine := &recoveryEngine{ContextCompressor: newCompressorWith(t, 128_000, nil)}
	a := &AIAgent{sessionDB: db, sessionID: "parent", platform: "telegram", cachedSystemPrompt: "stale"}
	a.contextCompressor = engine
	h := &sessionTagHandler{}
	lg := logging.GetLogger("agent.test_adopt_live_child")
	lg.SetLevel(logging.LevelDebug)
	lg.SetPropagate(false)
	lg.AddHandler(h)
	t.Cleanup(func() { lg.RemoveHandler(h); lg.SetPropagate(true); logging.ClearSessionContext() })
	got, ok := recoverRotatedCompressionSession(t.Context(), a)
	if !ok || len(got) != 1 || got[0]["content"] != "summary carried" {
		t.Fatalf("recovered %v %v", got, ok)
	}
	if a.sessionID != "child" || !a.sessionDBCreated || a.cachedSystemPrompt != "" || a.lastFlushedDBIdx != 1 {
		t.Fatalf("agent not re-pointed: %q %v %q %d", a.sessionID, a.sessionDBCreated, a.cachedSystemPrompt, a.lastFlushedDBIdx)
	}
	if a.flushedDBMessageSessionID == nil || *a.flushedDBMessageSessionID != "child" || !a.flushedDBMessageIDs[rowIdentity(got[0])] {
		t.Fatal("flush identity not seeded from the adopted rows")
	}
	if len(engine.starts) != 1 || engine.starts[0]["session_id"] != "child" || engine.starts[0]["old_session_id"] != "parent" ||
		engine.starts[0]["boundary_reason"] != "compression" || engine.starts[0]["platform"] != "telegram" {
		t.Fatalf("engine start %v", engine.starts)
	}
	if v := strings.TrimSpace(os.Getenv("ICEBATH_SESSION_ID")); v != "child" {
		t.Fatalf("session context not rebound: %q", v)
	}
	lg.Info("after adopt")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.tags) != 1 || h.tags[0] != " [child]" {
		t.Fatalf("log records not tagged with the adopted session: %q", h.tags)
	}
}

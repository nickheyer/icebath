package tuigateway

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Clean replay state around one case
func replayClean(t *testing.T) {
	t.Helper()
	ResetReplayState()
	t.Cleanup(ResetReplayState)
}

// Event frame of a session with an empty payload
func replayFrameOf(sid, etype string) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event",
		"params", pyjson.ObjectOf("type", etype, "session_id", sid, "payload", pyjson.NewObject()))
}

func replayParams(frame *pyjson.Object) *pyjson.Object {
	return frame.GetOr("params", nil).(*pyjson.Object)
}

func replaySeqs(events []*pyjson.Object) []int {
	out := []int{}
	for _, event := range events {
		out = append(out, event.GetOr("seq", nil).(int))
	}
	return out
}

func replayStat(key string) int { return ReplayStats().GetOr(key, nil).(int) }

// Restores a patched ring bound when the case ends
func patchReplayBound(t *testing.T, bound *int, value int) {
	t.Helper()
	old := *bound
	*bound = value
	t.Cleanup(func() { *bound = old })
}

func TestStampAddsMonotonicSeqPerSession(t *testing.T) {
	replayClean(t)
	f1, f2, other := replayFrameOf("s1", "message.delta"), replayFrameOf("s1", "message.delta"), replayFrameOf("s2", "message.delta")
	StampEvent(f1)
	StampEvent(other)
	StampEvent(f2)
	if replayParams(f1).GetOr("seq", nil) != 1 || replayParams(f2).GetOr("seq", nil) != 2 || replayParams(other).GetOr("seq", nil) != 1 {
		t.Fatalf("seqs %v %v %v", replayParams(f1).GetOr("seq", nil), replayParams(f2).GetOr("seq", nil), replayParams(other).GetOr("seq", nil))
	}
}

func TestStampIgnoresNonEventAndSessionlessFrames(t *testing.T) {
	replayClean(t)
	rpc := pyjson.ObjectOf("jsonrpc", "2.0", "id", 1, "result", pyjson.NewObject())
	noSid := pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params", pyjson.ObjectOf("type", "skin.changed"))
	StampEvent(rpc)
	StampEvent(noSid)
	if _, has := rpc.Get("seq"); has {
		t.Fatal("rpc frame stamped")
	}
	if _, has := replayParams(noSid).Get("seq"); has {
		t.Fatal("sessionless frame stamped")
	}
	if replayStat("events") != 0 {
		t.Fatal("events recorded")
	}
}

func TestEventsSinceReturnsOnlyNewerFramesInOrder(t *testing.T) {
	replayClean(t)
	frames := []*pyjson.Object{}
	for range 5 {
		frame := replayFrameOf("s1", "message.delta")
		frames = append(frames, frame)
		StampEvent(frame)
	}
	if got := replaySeqs(EventsSince("s1", 3)); fmt.Sprint(got) != "[4 5]" {
		t.Fatalf("got %v", got)
	}
	all := EventsSince("s1", 0)
	if len(all) != len(frames) {
		t.Fatalf("got %d", len(all))
	}
	for i, event := range all {
		want, _ := pyjson.Dumps(replayParams(frames[i]))
		got, _ := pyjson.Dumps(event)
		if want != got {
			t.Fatalf("event %d %s != %s", i, got, want)
		}
	}
	if len(EventsSince("s1", 99)) != 0 || LatestSeq("s1") != 5 {
		t.Fatal("tail or latest seq wrong")
	}
}

func TestReplaySnapshotIsDetachedFromLiveEventMutation(t *testing.T) {
	replayClean(t)
	frame := replayFrameOf("s1", "tool.complete")
	replayParams(frame).Set("payload", pyjson.ObjectOf("result", "small"))
	StampEvent(frame)
	retained := replayStat("bytes")
	replayParams(frame).GetOr("payload", nil).(*pyjson.Object).Set("result", strings.Repeat("x", 1_000_000))
	events := EventsSince("s1", 0)
	if len(events) != 1 || events[0].GetOr("payload", nil).(*pyjson.Object).GetOr("result", nil) != "small" {
		t.Fatal("snapshot followed the live mutation")
	}
	if replayStat("bytes") != retained {
		t.Fatal("retained bytes grew")
	}
}

func TestEachReplayReadReturnsAnIndependentEventGraph(t *testing.T) {
	replayClean(t)
	frame := replayFrameOf("s1", "tool.complete")
	replayParams(frame).Set("payload", pyjson.ObjectOf("nested", pyjson.ObjectOf("value", "original")))
	StampEvent(frame)
	first := EventsSince("s1", 0)[0]
	first.GetOr("payload", nil).(*pyjson.Object).GetOr("nested", nil).(*pyjson.Object).Set("value", "caller-mutated")
	second := EventsSince("s1", 0)[0]
	if second.GetOr("payload", nil).(*pyjson.Object).GetOr("nested", nil).(*pyjson.Object).GetOr("value", nil) != "original" {
		t.Fatal("reads share an object graph")
	}
}

func TestEventsSinceReturnsClientDispatchableEventObjects(t *testing.T) {
	replayClean(t)
	StampEvent(replayFrameOf("s1", "message.delta"))
	events := EventsSince("s1", 0)
	if len(events) != 1 {
		t.Fatalf("got %d", len(events))
	}
	event := events[0]
	if event.GetOr("type", nil) != "message.delta" || event.GetOr("session_id", nil) != "s1" || event.GetOr("seq", nil) != 1 {
		t.Fatal("not a bare event object")
	}
	for _, key := range []string{"jsonrpc", "method", "params"} {
		if _, has := event.Get(key); has {
			t.Fatalf("envelope key %s present", key)
		}
	}
}

func TestUnknownSessionReturnsEmpty(t *testing.T) {
	replayClean(t)
	if len(EventsSince("nope", 0)) != 0 || LatestSeq("nope") != 0 {
		t.Fatal("unknown session not empty")
	}
}

func TestRingBufferIsBounded(t *testing.T) {
	replayClean(t)
	for range replayBufferMax + 50 {
		StampEvent(replayFrameOf("s1", "message.delta"))
	}
	if replayStat("events") != replayBufferMax {
		t.Fatalf("events %d", replayStat("events"))
	}
	replay.Lock()
	oldest := replay.buffers["s1"].Value.(*replayRing).frames[0].seq
	replay.Unlock()
	if oldest <= 1 {
		t.Fatal("oldest not evicted")
	}
}

func TestSessionCountBoundedWithFifoEviction(t *testing.T) {
	replayClean(t)
	for i := range replaySessionsMax + 10 {
		StampEvent(replayFrameOf(fmt.Sprintf("s%d", i), "message.delta"))
	}
	if replayStat("sessions") != replaySessionsMax {
		t.Fatalf("sessions %d", replayStat("sessions"))
	}
	if len(EventsSince("s0", 0)) != 0 || LatestSeq(fmt.Sprintf("s%d", replaySessionsMax+9)) != 1 {
		t.Fatal("fifo eviction wrong")
	}
}

func TestFifoEvictionKeepsSeqMonotonicWithinEpoch(t *testing.T) {
	replayClean(t)
	first, second := replayFrameOf("s0", "message.delta"), replayFrameOf("s0", "message.delta")
	StampEvent(first)
	StampEvent(second)
	if replayParams(second).GetOr("seq", nil) != 2 {
		t.Fatal("second seq")
	}
	for index := 1; index <= replaySessionsMax; index++ {
		StampEvent(replayFrameOf(fmt.Sprintf("s%d", index), "message.delta"))
	}
	replay.Lock()
	_, kept := replay.buffers["s0"]
	replay.Unlock()
	if kept {
		t.Fatal("s0 ring survived")
	}
	revisited := replayFrameOf("s0", "message.delta")
	StampEvent(revisited)
	if replayParams(revisited).GetOr("seq", nil) != 3 {
		t.Fatal("revisited seq restarted")
	}
	if got := replaySeqs(EventsSince("s0", 2)); fmt.Sprint(got) != "[3]" {
		t.Fatalf("got %v", got)
	}
	if !IsTruncated("s0", 1) || IsTruncated("s0", 2) || IsTruncated("s0", 3) {
		t.Fatal("truncation after eviction wrong")
	}
}

func TestFifoEvictionMarksTruncationForOldWatermarks(t *testing.T) {
	replayClean(t)
	for range 3 {
		StampEvent(replayFrameOf("s0", "message.delta"))
	}
	if LatestSeq("s0") != 3 {
		t.Fatal("latest seq")
	}
	for index := 1; index <= replaySessionsMax; index++ {
		StampEvent(replayFrameOf(fmt.Sprintf("s%d", index), "message.delta"))
	}
	if !IsTruncated("s0", 0) || !IsTruncated("s0", 2) || IsTruncated("s0", 3) {
		t.Fatal("old watermarks not truncated")
	}
}

func TestConcurrentStampingNeverDropsOrDuplicatesSeq(t *testing.T) {
	replayClean(t)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := range 8 {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			seen := map[int]bool{}
			for range 200 {
				frame := replayFrameOf(sid, "message.delta")
				StampEvent(frame)
				seq := replayParams(frame).GetOr("seq", nil).(int)
				if seen[seq] {
					errs <- fmt.Sprintf("%s duplicate %d", sid, seq)
					return
				}
				seen[seq] = true
			}
		}(fmt.Sprintf("t%d", i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if replayStat("events") != 8*200 {
		t.Fatalf("events %d", replayStat("events"))
	}
}

func TestByteBudgetEvictsPayloadsAndPreservesGapSemantics(t *testing.T) {
	replayClean(t)
	patchReplayBound(t, &replayBufferBytesMax, 700)
	patchReplayBound(t, &replayProcessBytesMax, 5_000)
	first, second := replayFrameOf("s1", "tool.complete"), replayFrameOf("s1", "tool.complete")
	replayParams(first).Set("payload", pyjson.ObjectOf("result", strings.Repeat("x", 400)))
	replayParams(second).Set("payload", pyjson.ObjectOf("result", strings.Repeat("y", 400)))
	StampEvent(first)
	StampEvent(second)
	if replayStat("bytes") > replayStat("max_bytes_per_session") {
		t.Fatal("session budget exceeded")
	}
	if got := replaySeqs(EventsSince("s1", 0)); fmt.Sprint(got) != "[2]" || !IsTruncated("s1", 0) {
		t.Fatalf("got %v", got)
	}

	ResetReplayState()
	replayBufferBytesMax, replayProcessBytesMax = 1_000, 700
	first = replayFrameOf("s1", "tool.complete")
	replayParams(first).Set("payload", pyjson.ObjectOf("result", strings.Repeat("x", 400)))
	StampEvent(first)
	other := replayFrameOf("s2", "tool.complete")
	replayParams(other).Set("payload", pyjson.ObjectOf("result", strings.Repeat("y", 400)))
	StampEvent(other)
	if replayStat("bytes") > replayStat("max_bytes_process") {
		t.Fatal("process budget exceeded")
	}
	if len(EventsSince("s1", 0)) != 0 || !IsTruncated("s1", 0) {
		t.Fatal("s1 not evicted")
	}
	if got := replaySeqs(EventsSince("s2", 0)); fmt.Sprint(got) != "[1]" {
		t.Fatalf("got %v", got)
	}
}

func TestOversizedEventMarksGapEvenWithEmptyBuffer(t *testing.T) {
	replayClean(t)
	patchReplayBound(t, &replayBufferBytesMax, 300)
	patchReplayBound(t, &replayProcessBytesMax, 300)
	patchReplayBound(t, &replayBufferMax, replayBufferMax)
	oversized := replayFrameOf("s1", "tool.complete")
	replayParams(oversized).Set("payload", pyjson.ObjectOf("result", strings.Repeat("x", 1000)))
	StampEvent(oversized)
	if len(EventsSince("s1", 0)) != 0 || !IsTruncated("s1", 0) || IsTruncated("s1", 1) {
		t.Fatal("oversized frame gap wrong")
	}
	StampEvent(replayFrameOf("s1", "message.delta"))
	if got := replaySeqs(EventsSince("s1", 0)); fmt.Sprint(got) != "[2]" {
		t.Fatalf("got %v", got)
	}
	if !IsTruncated("s1", 0) || IsTruncated("s1", 1) || IsTruncated("s1", 2) {
		t.Fatal("gap after small frame wrong")
	}

	ResetReplayState()
	replayBufferMax, replayBufferBytesMax, replayProcessBytesMax = 1, 1000, 1000
	StampEvent(replayFrameOf("s", "message.delta"))
	large := replayFrameOf("s", "message.delta")
	replayParams(large).Set("payload", pyjson.ObjectOf("data", strings.Repeat("x", 2000)))
	StampEvent(large)
	StampEvent(replayFrameOf("s", "message.delta"))
	if !IsTruncated("s", 1) {
		t.Fatal("gap watermark moved backwards")
	}
}

func TestTruncationDetectionSemantics(t *testing.T) {
	replayClean(t)
	for range replayBufferMax + 10 {
		StampEvent(replayFrameOf("s1", "message.delta"))
	}
	replay.Lock()
	oldest := replay.buffers["s1"].Value.(*replayRing).frames[0].seq
	replay.Unlock()
	if oldest <= 1 {
		t.Fatal("no eviction")
	}
	if IsTruncated("s1", oldest-1) || !IsTruncated("s1", 5) || IsTruncated("nope", 0) {
		t.Fatal("truncation semantics wrong")
	}
}

func TestForgetSessionFreesTheRingAndKeepsSeqAndGapSemantics(t *testing.T) {
	replayClean(t)
	for range 3 {
		StampEvent(replayFrameOf("gone", "message.delta"))
	}
	StampEvent(replayFrameOf("kept", "message.delta"))
	before := replayStat("bytes")
	ForgetSession("gone")
	if replayStat("sessions") != 1 {
		t.Fatal("ring kept")
	}
	if after := replayStat("bytes"); after <= 0 || after >= before {
		t.Fatalf("bytes %d of %d", after, before)
	}
	if len(EventsSince("gone", 0)) != 0 || LatestSeq("gone") != 3 || !IsTruncated("gone", 2) || IsTruncated("gone", 3) {
		t.Fatal("forget semantics wrong")
	}
	late := replayFrameOf("gone", "message.delta")
	StampEvent(late)
	if replayParams(late).GetOr("seq", nil) != 4 || len(EventsSince("kept", 0)) != 1 {
		t.Fatal("late frame or kept ring wrong")
	}
	ForgetSession("never-seen")
}

func TestAFrameStampedAfterForgetDoesNotRebuildTheRing(t *testing.T) {
	replayClean(t)
	StampEvent(replayFrameOf("closed", "message.delta"))
	ForgetSession("closed")
	late := replayFrameOf("closed", "message.delta")
	replayParams(late).Set("payload", pyjson.ObjectOf("text", strings.Repeat("x", 100_000)))
	StampEvent(late)
	if replayParams(late).GetOr("seq", nil) != 2 || len(EventsSince("closed", 0)) != 0 {
		t.Fatal("late frame retained")
	}
	if replayStat("sessions") != 0 || replayStat("bytes") != 0 || !IsTruncated("closed", 1) {
		t.Fatal("ring rebuilt")
	}
}

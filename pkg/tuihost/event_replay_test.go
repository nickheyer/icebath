package tuihost

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

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

// Buffer occupancy for the ops and debug surface
func ReplayStats() *pyjson.Object {
	replay.Lock()
	defer replay.Unlock()
	drainReleasedLocked()
	events := 0
	for e := replay.order.Front(); e != nil; e = e.Next() {
		events += len(e.Value.(*replayRing).frames)
	}
	return pyjson.ObjectOf("sessions", len(replay.buffers), "events", events, "bytes", replay.totalBytes,
		"max_per_session", replayBufferMax, "max_bytes_per_session", replayBufferBytesMax,
		"max_bytes_process", replayProcessBytesMax)
}

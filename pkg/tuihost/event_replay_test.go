package tuihost

import (
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Event frame of a session with an empty payload
func replayFrameOf(sid, etype string) *jsonutil.Object {
	return jsonutil.ObjectOf("jsonrpc", "2.0", "method", "event",
		"params", jsonutil.ObjectOf("type", etype, "session_id", sid, "payload", jsonutil.NewObject()))
}

func replayParams(frame *jsonutil.Object) *jsonutil.Object {
	return frame.ValueOr("params", nil).(*jsonutil.Object)
}

func replaySeqs(events []*jsonutil.Object) []int {
	out := []int{}
	for _, event := range events {
		out = append(out, event.ValueOr("seq", nil).(int))
	}
	return out
}

func replayStat(key string) int { return ReplayStats().ValueOr(key, nil).(int) }

// Restores a patched ring bound when the case ends
func patchReplayBound(t *testing.T, bound *int, value int) {
	t.Helper()
	old := *bound
	*bound = value
	t.Cleanup(func() { *bound = old })
}

// Buffer occupancy for the ops and debug surface
func ReplayStats() *jsonutil.Object {
	replay.Lock()
	defer replay.Unlock()
	drainReleasedLocked()
	events := 0
	for e := replay.order.Front(); e != nil; e = e.Next() {
		events += len(e.Value.(*replayRing).frames)
	}
	return jsonutil.ObjectOf("sessions", len(replay.buffers), "events", events, "bytes", replay.totalBytes,
		"max_per_session", replayBufferMax, "max_bytes_per_session", replayBufferBytesMax,
		"max_bytes_process", replayProcessBytesMax)
}

package tuigateway

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Random 32 hex character identifier, uuid4 hex
func uuidHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

var replayEpochToken = uuidHex()

// Ring bounds per session and per process, swapped only where a hermes case patches them
var (
	replayBufferMax       = 512
	replaySessionsMax     = 64
	replayBufferBytesMax  = 4 * 1024 * 1024
	replayProcessBytesMax = 64 * 1024 * 1024
)

type replayFrame struct {
	seq     int
	payload []byte
	size    int
}

type replayRing struct {
	sid    string
	frames []replayFrame
}

var replay = struct {
	sync.Mutex
	order          *list.List
	buffers        map[string]*list.Element
	bufferBytes    map[string]int
	evictedThrough map[string]int
	totalBytes     int
	nextSeq        map[string]int
	forgotten      map[string]bool
	releasedMu     sync.Mutex
	released       []string
}{order: list.New(), buffers: map[string]*list.Element{}, bufferBytes: map[string]int{}, evictedThrough: map[string]int{},
	nextSeq: map[string]int{}, forgotten: map[string]bool{}}

// Opaque token identifying this process's seq numbering
func ReplayEpoch() string { return replayEpochToken }

// Serialized replay snapshot detached from the caller's object graph
func freezeParams(params *pyjson.Object) []byte {
	text, err := pyjson.Dumps(params, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		panic(err)
	}
	return []byte(text)
}

// Client event object rebuilt from its snapshot with the server owned seq restored
func thawParams(payload []byte, seq int) *pyjson.Object {
	decoded, err := pyjson.LoadsBytes(payload)
	if err != nil {
		panic(err)
	}
	event := decoded.(*pyjson.Object)
	event.Set("seq", seq)
	return event
}

// Stamps an outgoing event frame with its per session seq and records a frozen snapshot
func StampEvent(obj *pyjson.Object) {
	if method, _ := obj.Get("method"); method != "event" {
		return
	}
	raw, _ := obj.Get("params")
	params, ok := raw.(*pyjson.Object)
	if !ok {
		return
	}
	sidValue, _ := params.Get("session_id")
	if !pyrepr.Truthy(sidValue) {
		return
	}
	sid, isStr := sidValue.(string)
	if !isStr {
		sid = pyrepr.Repr(sidValue)
	}
	payload := freezeParams(params)
	size := len(payload)
	replay.Lock()
	defer replay.Unlock()
	drainReleasedLocked()
	seq := replay.nextSeq[sid] + 1
	replay.nextSeq[sid] = seq
	params.Set("seq", seq)
	if replay.forgotten[sid] {
		replay.evictedThrough[sid] = seq
		return
	}
	elem, found := replay.buffers[sid]
	if !found {
		elem = replay.order.PushBack(&replayRing{sid: sid})
		replay.buffers[sid] = elem
		replay.bufferBytes[sid] = 0
		for len(replay.buffers) > replaySessionsMax {
			dropRingLocked(replay.order.Front().Value.(*replayRing).sid)
		}
	}
	if size > replayBufferBytesMax || size > replayProcessBytesMax {
		replay.evictedThrough[sid] = seq
		return
	}
	ring := elem.Value.(*replayRing)
	ring.frames = append(ring.frames, replayFrame{seq: seq, payload: payload, size: size})
	replay.bufferBytes[sid] += size
	replay.totalBytes += size
	for len(ring.frames) > replayBufferMax || replay.bufferBytes[sid] > replayBufferBytesMax {
		evicted := ring.frames[0]
		ring.frames = ring.frames[1:]
		replay.bufferBytes[sid] -= evicted.size
		replay.totalBytes -= evicted.size
		replay.evictedThrough[sid] = max(replay.evictedThrough[sid], evicted.seq)
	}
	for replay.totalBytes > replayProcessBytesMax {
		for e := replay.order.Front(); e != nil; e = e.Next() {
			evictRing := e.Value.(*replayRing)
			if len(evictRing.frames) == 0 {
				continue
			}
			evicted := evictRing.frames[0]
			evictRing.frames = evictRing.frames[1:]
			replay.bufferBytes[evictRing.sid] -= evicted.size
			replay.totalBytes -= evicted.size
			replay.evictedThrough[evictRing.sid] = max(replay.evictedThrough[evictRing.sid], evicted.seq)
			break
		}
	}
}

// Frees one session's retained frames, keeping its seq numbering and raising its truncation watermark
func dropRingLocked(sid string) {
	elem, ok := replay.buffers[sid]
	if !ok {
		return
	}
	replay.order.Remove(elem)
	delete(replay.buffers, sid)
	replay.totalBytes -= replay.bufferBytes[sid]
	delete(replay.bufferBytes, sid)
	replay.evictedThrough[sid] = max(replay.evictedThrough[sid], replay.nextSeq[sid])
}

// Releases a torn down session's replay ring so late frames are never retained again
func ForgetSession(sid string) {
	if sid == "" {
		return
	}
	replay.Lock()
	defer replay.Unlock()
	replay.forgotten[sid] = true
	dropRingLocked(sid)
}

// Drops the ring and every counter recorded under one private key
func purgeKeyLocked(key string) {
	if elem, ok := replay.buffers[key]; ok {
		replay.order.Remove(elem)
		delete(replay.buffers, key)
		replay.totalBytes -= replay.bufferBytes[key]
		delete(replay.bufferBytes, key)
	}
	delete(replay.evictedThrough, key)
	delete(replay.nextSeq, key)
	delete(replay.forgotten, key)
}

// Purges every key released while the lock was held elsewhere
func drainReleasedLocked() {
	replay.releasedMu.Lock()
	keys := replay.released
	replay.released = nil
	replay.releasedMu.Unlock()
	for i := len(keys) - 1; i >= 0; i-- {
		purgeKeyLocked(keys[i])
	}
}

// Drops everything recorded under a private key, queued for the next lock holder when non blocking
func ReleaseKey(key string, blocking bool) {
	if key == "" {
		return
	}
	replay.releasedMu.Lock()
	replay.released = append(replay.released, key)
	replay.releasedMu.Unlock()
	if blocking {
		replay.Lock()
	} else if !replay.TryLock() {
		return
	}
	defer replay.Unlock()
	drainReleasedLocked()
}

// Recorded event objects newer than lastSeen for a session, decoded outside the ring lock
func EventsSince(sid string, lastSeen int) []*pyjson.Object {
	replay.Lock()
	var snapshots []replayFrame
	if elem, ok := replay.buffers[sid]; ok {
		for _, frame := range elem.Value.(*replayRing).frames {
			if frame.seq > lastSeen {
				snapshots = append(snapshots, frame)
			}
		}
	}
	replay.Unlock()
	out := make([]*pyjson.Object, 0, len(snapshots))
	for _, frame := range snapshots {
		out = append(out, thawParams(frame.payload, frame.seq))
	}
	return out
}

// True when frames between lastSeen and the oldest retained seq were evicted
func IsTruncated(sid string, lastSeen int) bool {
	replay.Lock()
	defer replay.Unlock()
	return lastSeen < replay.evictedThrough[sid]
}

// Highest stamped seq of a session, zero when unknown
func LatestSeq(sid string) int {
	replay.Lock()
	defer replay.Unlock()
	return replay.nextSeq[sid]
}

// Clears every ring, counter and released key
func ResetReplayState() {
	replay.Lock()
	defer replay.Unlock()
	replay.order.Init()
	clear(replay.buffers)
	clear(replay.bufferBytes)
	clear(replay.evictedThrough)
	clear(replay.nextSeq)
	clear(replay.forgotten)
	replay.releasedMu.Lock()
	replay.released = nil
	replay.releasedMu.Unlock()
	replay.totalBytes = 0
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

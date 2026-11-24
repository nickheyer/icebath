package gateway

import (
	"fmt"
	"runtime"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

// Authority local subscription ordering over the bounded fanout and the shared replay cache
type SessionEvents struct {
	Lock       syncutil.ReentrantMutex
	Fanout     *tuihost.FanoutTransport
	key        string
	Epoch      string
	Sequence   int
	publicID   any
	Execution  *jsonutil.Object
	Observers  map[*SessionEventObserver]bool
	OnOverflow func(transport tuihost.Transport)
}

// Synchronous same goroutine recipient of every published frame
type SessionEventObserver struct {
	Observe func(frame *jsonutil.Object)
}

// Event stream under a private replay key freed when the stream is collected
func NewSessionEvents() *SessionEvents {
	e := &SessionEvents{Fanout: tuihost.NewFanoutTransport(), key: uuid4HexString(), Epoch: uuid4HexString(),
		Execution: jsonutil.NewObject(), Observers: map[*SessionEventObserver]bool{}}
	runtime.AddCleanup(e, func(key string) { tuihost.ReleaseKey(key, false) }, e.key)
	return e
}

// Frees the replay frames of a session that ended with no viewer left
func (e *SessionEvents) Release() {
	e.Lock.Lock()
	defer e.Lock.Unlock()
	tuihost.ReleaseKey(e.key, true)
}

// Current replay epoch and sequence, a fresh epoch once the shared cache evicted this session
func (e *SessionEvents) Watermark() (string, int) {
	e.Lock.Lock()
	defer e.Lock.Unlock()
	if e.Sequence != 0 && tuihost.LatestSeq(e.key) == 0 {
		e.Epoch = uuid4HexString()
		e.Sequence = 0
	}
	return e.Epoch, e.Sequence
}

// Event payload checked against its typed contract, a misfit reported and the payload published as given
func sealCanonicalPayload(eventType string, payload any) any {
	if typed := tuigateway.Lookup(tuigateway.KindEvent, eventType); typed != nil && payload != nil {
		if err := typed.CheckResult(payload); err != nil {
			tuigateway.ReportMisfit("payload of", eventType, err)
		}
	}
	return payload
}

// Stamps, records and fans out one event frame, true when a live viewer or observer took it
func (e *SessionEvents) Publish(sessionID any, payload any, eventType string, execution *jsonutil.Object) bool {
	e.Lock.Lock()
	defer e.Lock.Unlock()
	e.Watermark()
	stamp := e.Execution
	if execution != nil {
		stamp = execution
	}
	params := stamp.Clone()
	params.Set("type", eventType)
	params.Set("session_id", e.key)
	params.Set("payload", jsonutil.Clone(sealCanonicalPayload(eventType, payload)))
	params.Set("replay_epoch", e.Epoch)
	frame := jsonutil.ObjectOf("jsonrpc", "2.0", "method", "event", "params", params)
	tuihost.StampEvent(frame)
	seqValue, _ := params.Get("seq")
	seq, _ := seqValue.(int)
	if seq <= e.Sequence {
		e.Epoch = uuid4HexString()
		params.Set("replay_epoch", e.Epoch)
	}
	params.Set("session_id", sessionID)
	e.publicID = sessionID
	e.Sequence = seq
	overflow := func(transport tuihost.Transport) *jsonutil.Object {
		if e.OnOverflow != nil {
			e.OnOverflow(transport)
		}
		return jsonutil.ObjectOf("jsonrpc", "2.0", "method", "event", "params", jsonutil.ObjectOf(
			"type", "session.replay_gap", "session_id", sessionID,
			"payload", tuigateway.SessionReplayGapEvent.Wire(tuigateway.SessionReplayGapPayload{ReplayEpoch: e.Epoch, LatestSeq: int64(e.Sequence)})))
	}
	delivered := e.Fanout.WriteOverflow(frame, overflow)
	for observer := range e.Observers {
		if e.observe(observer, frame, sessionID) {
			delivered = true
		}
	}
	return delivered
}

// Hands a frame to one observer, a failing observer logged without undoing publication
func (e *SessionEvents) observe(observer *SessionEventObserver, frame *jsonutil.Object, sessionID any) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Warn(fmt.Sprintf("Session event observer failed for %v: %v", sessionID, r))
			ok = false
		}
	}()
	observer.Observe(frame)
	return true
}

// Writes a joining transport the frames published after its replay cursor, the caller holding Lock
func (e *SessionEvents) CatchUp(transport tuihost.Transport, epoch string, sequence int) {
	replay := e.Since(epoch, sequence)
	if required, _ := replay.Get("snapshot_required"); required == true {
		return
	}
	events, _ := replay.Get("events")
	for _, frame := range events.([]any) {
		transport.Write(jsonutil.ObjectOf("jsonrpc", "2.0", "method", "event", "params", frame))
	}
}

// Replay window after a cursor, empty and snapshot required when the cursor fell outside it
func (e *SessionEvents) Since(epoch string, sequence int) *jsonutil.Object {
	e.Lock.Lock()
	defer e.Lock.Unlock()
	currentEpoch, latest := e.Watermark()
	frames := tuihost.EventsSince(e.key, sequence)
	missing := epoch != currentEpoch || sequence > latest || tuihost.IsTruncated(e.key, sequence) ||
		tuihost.LatestSeq(e.key) != latest
	events := []any{}
	if !missing {
		for _, frame := range frames {
			if sid, _ := frame.Get("session_id"); sid == e.key {
				frame = frame.Clone()
				frame.Set("session_id", e.publicID)
			}
			events = append(events, frame)
		}
	}
	return jsonutil.ObjectOf("events", events, "latest_seq", latest, "last_sequence", latest, "epoch", currentEpoch,
		"replay_epoch", currentEpoch, "truncated", missing, "snapshot_required", missing, "count", len(events))
}

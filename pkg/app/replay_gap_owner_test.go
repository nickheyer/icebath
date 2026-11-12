package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Deltas of the owner's long turn and the reply they join into
var (
	replayGapDeltas = func() []string {
		out := make([]string, 600)
		for i := range out {
			out[i] = fmt.Sprintf("%07d|", i)
		}
		return out
	}()
	replayGapFinal = strings.Join(replayGapDeltas, "")
)

// Pins the fanout frame cap to the old 256 so the paused viewer overflows within one turn
func replayGapFrameCap(t *testing.T) {
	t.Helper()
	previous := tuigateway.FanoutMaxPendingFrames
	tuigateway.FanoutMaxPendingFrames = 256
	t.Cleanup(func() { tuigateway.FanoutMaxPendingFrames = previous })
}

// Real owner of session s with one viewer whose socket stops reading while paused
type replayGapOwner struct {
	t         *testing.T
	ctx       context.Context
	events    *gateway.EventQueue
	running   pythread.Event
	mu        sync.Mutex
	pause     string
	gaps      []string
	turns     sync.WaitGroup
	ref       gateway.SessionRef
	db        *state.SessionDB
	epoch     int64
	authority *gateway.SessionAuthority
	live      *gateway.LiveSession
	conn      *gateway.AuthorityConnection
}

// Owner over the state.db at dbPath, the next turn pausing the viewer throughout or until delta 400
func newReplayGapOwner(t *testing.T, dbPath, pause string) *replayGapOwner {
	t.Helper()
	ctx := context.Background()
	o := &replayGapOwner{t: t, ctx: ctx, events: gateway.NewEventQueue(0), pause: pause, ref: gateway.SessionRef{ProfileID: "owned", SessionID: "s"}}
	o.running.Set()
	db, err := state.NewSessionDB(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	o.db = db
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if o.epoch, err = db.BeginRuntimeEpoch(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	o.authority = gateway.NewSessionAuthority(&queueOwnerRunner{}, "owned", "owner", db, o.epoch)
	o.live = gateway.NewLiveSession(nil, "route")
	o.authority.Sessions["s"] = o.live
	if o.conn, err = gateway.NewAuthorityConnection(o.authority, replayGapSocket{o}, map[string]any{"user_id": "viewer"}, false); err != nil {
		t.Fatal(err)
	}
	return o
}

// The viewer's socket, blocked while the viewer is paused and recording each replay gap it is sent
func (o *replayGapOwner) Write(frame *pyjson.Object) bool {
	o.running.Wait(30)
	text, err := pyjson.Dumps(frame)
	if err != nil {
		panic(err)
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		panic(err)
	}
	if params, _ := frame.Get("params"); params != nil {
		if object, ok := params.(*pyjson.Object); ok {
			if kind, _ := object.Get("type"); kind == "session.replay_gap" {
				sid, _ := object.Get("session_id")
				o.mu.Lock()
				o.gaps = append(o.gaps, fmt.Sprint(sid))
				o.mu.Unlock()
			}
		}
	}
	o.events.PutNowait(decoded)
	return true
}

func (o *replayGapOwner) Close() error { return nil }

// Viewer socket side of the owner, the transport its authority connection writes frames to
type replayGapSocket struct{ owner *replayGapOwner }

func (s replayGapSocket) Write(frame *pyjson.Object) bool { return s.owner.Write(frame) }
func (s replayGapSocket) Close()                          {}

func (o *replayGapOwner) EventQueue() *gateway.EventQueue { return o.events }
func (o *replayGapOwner) SendFrame(string) error          { return nil }

// Admits and claims a submitted prompt and runs its turn, answering resume, receipt and detach through the viewer's connection
func (o *replayGapOwner) RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	if method == "prompt.submit" {
		requestID, _ := params["input_id"].(string)
		if requestID == "" {
			requestID, _ = params["submission_id"].(string)
		}
		payload := pyjson.NewObject()
		payload.Set("text", params["text"])
		if _, err := o.db.AdmitSessionInput(ctx, state.SessionInput{Epoch: o.epoch, PrincipalID: o.conn.Actor().Subject, SessionID: "s",
			RequestID: requestID, Payload: payload}); err != nil {
			return nil, err
		}
		row, _, err := o.db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: o.epoch, SessionID: "s"})
		if err != nil {
			return nil, err
		}
		o.mu.Lock()
		pause := o.pause
		o.pause = ""
		o.mu.Unlock()
		o.turns.Add(1)
		go o.turn(row, pause)
		admissionID, _ := row.Get("admission_id")
		return map[string]any{"admission_id": admissionID, "status": "queued"}, nil
	}
	switch method {
	case "session.resume", "prompt.receipt", "session.detach":
		return (&ownerLife{connection: o.conn, events: o.events}).RPCTimeout(ctx, method, params, timeout)
	}
	return map[string]any{}, nil
}

// One claimed turn published as the drain does, the viewer paused for the pause mode given
func (o *replayGapOwner) turn(row *pyjson.Object, pause string) {
	defer o.turns.Done()
	if err := o.authority.StampClaim(o.ctx, o.ref, o.live, row); err != nil {
		panic(err)
	}
	if pause != "" {
		o.running.Clear()
	}
	generation := replayGapGeneration(row)
	for index, delta := range replayGapDeltas {
		if pause == "live" && index == 400 {
			o.running.Set()
			deadline := time.Now().Add(5 * time.Second)
			for !o.subscribed() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if _, err := o.authority.PublishExecution("s", generation, "message.delta", map[string]any{"text": delta}); err != nil {
			panic(err)
		}
	}
	o.authority.EndExecution(o.live, "s")
	if err := gateway.CommitAndPublish(o.ctx, o.authority, o.live, o.ref, row, replayGapFinal, "completed", nil, &gateway.TurnSettlement{}); err != nil {
		panic(err)
	}
	o.running.Set()
}

// Execution generation the claim stamped on a row
func replayGapGeneration(row *pyjson.Object) int64 {
	value, _ := row.Get("generation")
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	panic(fmt.Sprintf("claimed row generation %#v", value))
}

// Whether a viewer is subscribed to session s, read under the authority lock
func (o *replayGapOwner) subscribed() bool {
	for _, view := range o.authority.LiveSessionViews() {
		if view.SessionID == "s" {
			return view.Subscribed
		}
	}
	return false
}

// Gap frames the viewer was sent
func (o *replayGapOwner) gapCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.gaps)
}

// Releases the viewer, waits for every turn and closes the store
func (o *replayGapOwner) finish() {
	o.running.Set()
	done := make(chan struct{})
	go func() {
		o.turns.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	o.db.Close()
}

var (
	_ tuigateway.Transport        = replayGapSocket{}
	_ gatewaycli.ConnectionClient = (*replayGapOwner)(nil)
)

// Connection whose every dial meets a fresh owner over the case's state.db
func replayGapConnection(t *testing.T, owners *[]*replayGapOwner, ownersMu *sync.Mutex) *gatewaycli.GatewayConnection {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	return gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) {
		owner := newReplayGapOwner(t, dbPath, "ended")
		ownersMu.Lock()
		*owners = append(*owners, owner)
		ownersMu.Unlock()
		return owner, nil
	})
}

func TestTurnEndedInTheHoleReportsTheAnswerAndTheNextTurnStreams(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installSessionsStoreWiring(t)
	replayGapFrameCap(t)
	ctx := context.Background()
	var owners []*replayGapOwner
	var ownersMu, streamMu sync.Mutex
	var streamed []string
	joined := func() string {
		streamMu.Lock()
		defer streamMu.Unlock()
		return strings.Join(streamed, "")
	}
	connection := replayGapConnection(t, &owners, &ownersMu)
	if err := connection.Start(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ownersMu.Lock()
		first := owners[0]
		ownersMu.Unlock()
		first.finish()
		connection.Close(ctx)
	}()
	snapshot, err := connection.Call(ctx, "session.resume", map[string]any{"session_id": "s"})
	if err != nil {
		t.Fatal(err)
	}
	connection.State("s").AdoptSnapshot(snapshot)
	a, err := newGatewayAgent(ctx, gatewayAgentOptions{Connection: connection, SessionID: "s", MaxIterations: 90, ToolProgressMode: "all", QuietMode: true,
		StreamDeltaCallback: func(text any) {
			if piece, ok := text.(string); ok && piece != "" {
				streamMu.Lock()
				streamed = append(streamed, piece)
				streamMu.Unlock()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	first := runTurnWithin(a, "write a long answer", 30*time.Second, agent.RunConversationOptions{})
	ownersMu.Lock()
	owner := owners[0]
	ownersMu.Unlock()
	if owner.gapCount() == 0 {
		t.Fatal("probe did not overflow the fanout (vacuous)")
	}
	if first == nil || pyrepr.Truthy(first["failed"]) || first["final_response"] != replayGapFinal {
		t.Fatalf("first turn %v", first)
	}
	if joined() != replayGapFinal {
		t.Fatalf("hole or duplicate: streamed %d bytes, want %d", len(joined()), len(replayGapFinal))
	}
	streamMu.Lock()
	streamed = nil
	streamMu.Unlock()
	second := runTurnWithin(a, "and a short one", 15*time.Second, agent.RunConversationOptions{})
	if second == nil {
		t.Fatal("turn 2 hangs: the gap detached the viewer and nothing re-attached")
	}
	if pyrepr.Truthy(second["failed"]) || joined() != replayGapFinal {
		t.Fatalf("second turn %v streamed %d bytes", second["failed"], len(joined()))
	}
}

package tuiserver

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Drives one session to its disconnect teardown, seed running with the live transport at the first read
func wsTestRunDisconnect(t *testing.T, seed func(transport *tuihost.WSTransport)) {
	t.Helper()
	wsTestNoOrphanGrace(t)
	wsTestFakeAuthority(t, nil)
	sock := newWSTestSocket()
	sock.onReceive = func() { seed(wsTestLiveTransport(t)) }
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, nil, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestWsConnectionRegistersThenDisconnectUnregistersLiveTransport(t *testing.T) {
	registered := false
	var seeded *tuihost.WSTransport
	wsTestRunDisconnect(t, func(transport *tuihost.WSTransport) {
		seeded = transport
		registered = wsTestIndex(tuihost.LiveTransports(), tuihost.Transport(transport)) >= 0
	})
	if !registered {
		t.Fatal("the connected transport was not registered for global broadcasts")
	}
	if wsTestIndex(tuihost.LiveTransports(), tuihost.Transport(seeded)) >= 0 {
		t.Fatal("the disconnect left the transport registered")
	}
}

func TestWsDisconnectReleasesWakeWordOwner(t *testing.T) {
	var released, created []tuihost.Transport
	prev := wsReleaseWakeForTransport
	wsReleaseWakeForTransport = func(transport tuihost.Transport) bool {
		released = append(released, transport)
		return true
	}
	t.Cleanup(func() { wsReleaseWakeForTransport = prev })
	wsTestRunDisconnect(t, func(transport *tuihost.WSTransport) { created = append(created, transport) })
	if len(released) != 1 || !reflect.DeepEqual(released, created) {
		t.Fatalf("released %v, created %v", released, created)
	}
}

func TestWsReadyComesFirstAndPingIsInline(t *testing.T) {
	wsTestNoOrphanGrace(t)
	wsTestFakeAuthority(t, nil)
	sock := newWSTestSocket()
	sock.inbound <- wsTestRPC("heartbeat-1", "gateway.ping")
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	sent := sock.frames()
	if len(sent) < 2 {
		t.Fatalf("sent %v", sent)
	}
	ready, _ := sent[0]["params"].(map[string]any)
	payload, _ := ready["payload"].(map[string]any)
	if ready["type"] != "gateway.ready" || payload["build"] == nil || payload["replay_epoch"] == nil {
		t.Fatalf("ready frame %v", sent[0])
	}
	if want := map[string]any{"jsonrpc": "2.0", "result": map[string]any{"ok": true}, "id": "heartbeat-1"}; !reflect.DeepEqual(sent[1], want) {
		t.Fatalf("ping reply %v", sent[1])
	}
	if sock.lines[1] != `{"jsonrpc": "2.0", "result": {"ok": true}, "id": "heartbeat-1"}` {
		t.Fatalf("ping reply on the wire %s", sock.lines[1])
	}
}

// Session over a scripted socket whose slow and context breakdown calls block in dispatch until released
func wsTestSlowDispatchHarness(t *testing.T) (*wsTestSocket, *wsTestLog, chan struct{}) {
	t.Helper()
	wsTestNoOrphanGrace(t)
	log, release := &wsTestLog{}, make(chan struct{})
	wsTestFakeAuthority(t, func(_ context.Context, request *pyjson.Object) (*pyjson.Object, error) {
		method, _ := request.GetOr("method", nil).(string)
		log.add("dispatch:" + method)
		if method == "slow" || method == "session.context_breakdown" {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		log.add("done:" + method)
		return pyjson.ObjectOf("jsonrpc", "2.0", "id", request.GetOr("id", nil), "result", pyjson.ObjectOf("method", method)), nil
	})
	prevRelease := wsReleaseSetupForTransport
	wsReleaseSetupForTransport = func(tuihost.Transport) { log.add("teardown") }
	t.Cleanup(func() { wsReleaseSetupForTransport = prevRelease })
	return newWSTestSocket(), log, release
}

func TestWsPingIsAnsweredWhileAnEarlierRpcBlocksDispatch(t *testing.T) {
	sock, log, release := wsTestSlowDispatchHarness(t)
	done := wsTestServe(context.Background(), sock, nil, false)
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()
	sock.inbound <- wsTestRPC("r1", "slow")
	sock.inbound <- wsTestRPC("r2", "fast")
	sock.inbound <- wsTestRPC("heartbeat-1", "gateway.ping")
	if !wsTestSettle(func() bool { return wsTestHasID(sock.ids(), "heartbeat-1") }, time.Second) {
		t.Fatalf("gateway.ping went unanswered while dispatch was busy; sent ids=%v", sock.ids())
	}
	if log.has("done:slow") || log.has("dispatch:fast") {
		t.Fatalf("the serial lane ran past the blocked call: %v", log.snapshot())
	}
	releaseOnce()
	if !wsTestSettle(func() bool { return wsTestHasID(sock.ids(), "r1") && wsTestHasID(sock.ids(), "r2") }, 2*time.Second) {
		t.Fatalf("replies missing, ids=%v", sock.ids())
	}
	sock.inbound <- nil
	if err := wsTestAwait(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	entries := log.snapshot()
	if wsTestIndex(entries, "done:slow") >= wsTestIndex(entries, "dispatch:fast") {
		t.Fatalf("calls left arrival order: %v", entries)
	}
	if ids := sock.ids(); wsTestIndex(ids, any("r1")) >= wsTestIndex(ids, any("r2")) {
		t.Fatalf("replies left arrival order: %v", ids)
	}
}

func TestWsContextBreakdownDoesNotBlockALaterControlRead(t *testing.T) {
	sock, log, release := wsTestSlowDispatchHarness(t)
	done := wsTestServe(context.Background(), sock, nil, false)
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()
	sock.inbound <- wsTestRPC("context", "session.context_breakdown")
	sock.inbound <- wsTestRPC("control", "session.control.read")
	if !wsTestSettle(func() bool { return wsTestHasID(sock.ids(), "control") }, 2*time.Second) {
		t.Fatalf("the control read waited behind the context meter, ids=%v", sock.ids())
	}
	if wsTestHasID(sock.ids(), "context") {
		t.Fatal("the gated context read answered early")
	}
	releaseOnce()
	if !wsTestSettle(func() bool { return wsTestHasID(sock.ids(), "context") }, 2*time.Second) {
		t.Fatal("the context read never answered")
	}
	sock.inbound <- nil
	if err := wsTestAwait(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	entries := log.snapshot()
	if wsTestIndex(entries, "done:session.context_breakdown") <= wsTestIndex(entries, "done:session.control.read") {
		t.Fatalf("order %v", entries)
	}
}

func TestWsDisconnectTeardownWaitsForInFlightDispatch(t *testing.T) {
	sock, log, release := wsTestSlowDispatchHarness(t)
	done := wsTestServe(context.Background(), sock, nil, false)
	sock.inbound <- wsTestRPC("r1", "slow")
	if !wsTestSettle(func() bool { return log.has("dispatch:slow") }, 2*time.Second) {
		close(release)
		t.Fatal("the slow call never started")
	}
	sock.inbound <- wsTestRPC("r2", "queued")
	sock.inbound <- nil
	time.Sleep(100 * time.Millisecond)
	torn := log.has("teardown")
	close(release)
	if torn {
		t.Fatal("sessions were torn down under a running handler")
	}
	if err := wsTestAwait(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if want := []string{"dispatch:slow", "done:slow", "dispatch:queued", "done:queued", "teardown"}; !reflect.DeepEqual(log.snapshot(), want) {
		t.Fatalf("log %v", log.snapshot())
	}
}

func TestWsFailedReplyFromDispatcherEndsTheConnection(t *testing.T) {
	sock, log, _ := wsTestSlowDispatchHarness(t)
	sock.failIDs["r1"] = true
	sock.inbound <- wsTestRPC("r1", "fast")
	done := wsTestServe(context.Background(), sock, nil, false)
	if err := wsTestAwait(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if want := []string{"dispatch:fast", "done:fast", "teardown"}; !reflect.DeepEqual(log.snapshot(), want) {
		t.Fatalf("log %v", log.snapshot())
	}
}

func TestASocketWithoutASessionAuthorityIsRefused(t *testing.T) {
	wsTestNoOrphanGrace(t)
	sock := newWSTestSocket()
	sock.authority = nil
	sock.inbound <- nil
	err := HandleWS(context.Background(), sock, map[string]any{"user_id": "u"}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "without a session authority") {
		t.Fatalf("a socket without a session authority was served: %v", err)
	}
	if sent := sock.frames(); len(sent) != 0 {
		t.Fatalf("frames went out before the refusal: %v", sent)
	}
}

func TestHandleWsStartsHeartbeatRefresher(t *testing.T) {
	wsTestNoOrphanGrace(t)
	wsTestFakeAuthority(t, nil)
	started := 0
	prev := wsStartHeartbeatRefresher
	wsStartHeartbeatRefresher = func(context.Context) { started++ }
	wsStartBackendLiveness = startBackendLiveness
	t.Cleanup(func() { wsStartHeartbeatRefresher = prev })
	sock := newWSTestSocket()
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("the heartbeat refresher started %d times", started)
	}
}

// A slow skin resolve of one peer holds only its own connection and its result still reaches the ready frame
func TestHandleWsResolvesSkinOffTheLoopThread(t *testing.T) {
	wsTestNoOrphanGrace(t)
	wsTestFakeAuthority(t, nil)
	gate, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	prev := wsResolveSkin
	slow := true
	var mu sync.Mutex
	wsResolveSkin = func(context.Context) map[string]any {
		mu.Lock()
		block := slow
		slow = false
		mu.Unlock()
		if block {
			once.Do(func() { close(entered) })
			<-gate
		}
		return map[string]any{"palette": "wired"}
	}
	t.Cleanup(func() { wsResolveSkin = prev })
	first := newWSTestSocket()
	firstDone := wsTestServe(context.Background(), first, nil, false)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("the skin resolve never started")
	}
	second := newWSTestSocket()
	second.inbound <- wsTestRPC("heartbeat-1", "gateway.ping")
	second.inbound <- nil
	secondDone := wsTestServe(context.Background(), second, nil, false)
	var stalled bool
	select {
	case err := <-secondDone:
		if err != nil {
			close(gate)
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		stalled = true
	}
	close(gate)
	if stalled {
		t.Fatal("a second peer waited behind another peer's skin resolve")
	}
	if !wsTestHasID(second.ids(), "heartbeat-1") {
		t.Fatalf("the second peer got %v", second.frames())
	}
	first.inbound <- nil
	if err := wsTestAwait(t, firstDone, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var ready map[string]any
	for _, frame := range first.frames() {
		if params, _ := frame["params"].(map[string]any); params["type"] == "gateway.ready" {
			ready = params
		}
	}
	payload, _ := ready["payload"].(map[string]any)
	if !reflect.DeepEqual(payload["skin"], map[string]any{"palette": "wired"}) {
		t.Fatalf("ready payload %v", payload)
	}
}

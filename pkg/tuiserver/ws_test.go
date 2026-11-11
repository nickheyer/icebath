package tuiserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Drives one session to its disconnect teardown, seed running with the live transport at the first read
func wsTestRunDisconnect(t *testing.T, seed func(transport *tuigateway.WSTransport)) {
	t.Helper()
	wsTestNoOrphanGrace(t)
	sock := newWSTestSocket()
	sock.onReceive = func() { seed(wsTestLiveTransport(t)) }
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, nil, nil, false); err != nil {
		t.Fatal(err)
	}
}

type wsTestWorker struct {
	mu     sync.Mutex
	closed []bool
}

func (w *wsTestWorker) Close() {
	w.mu.Lock()
	w.closed = append(w.closed, true)
	w.mu.Unlock()
}

func TestWsDisconnectReapsFlaggedSessionAndClosesWorker(t *testing.T) {
	worker := &wsTestWorker{}
	t.Cleanup(func() { tuigateway.SetSessionRecord("flagged", nil) })
	wsTestRunDisconnect(t, func(transport *tuigateway.WSTransport) {
		tuigateway.SetSessionRecord("flagged", tuigateway.NewSession(map[string]any{
			"transport": tuigateway.Transport(transport), "close_on_disconnect": true, "slash_worker": worker, "session_key": "k"}))
	})
	if tuigateway.SessionRecord("flagged") != nil {
		t.Fatal("the flagged session outlived its transport")
	}
	if !reflect.DeepEqual(worker.closed, []bool{true}) {
		t.Fatalf("worker closes %v", worker.closed)
	}
}

func TestWsConnectionRegistersThenDisconnectUnregistersLiveTransport(t *testing.T) {
	registered := false
	var seeded *tuigateway.WSTransport
	wsTestRunDisconnect(t, func(transport *tuigateway.WSTransport) {
		seeded = transport
		registered = wsTestIndex(tuigateway.LiveTransports(), tuigateway.Transport(transport)) >= 0
	})
	if !registered {
		t.Fatal("the connected transport was not registered for global broadcasts")
	}
	if wsTestIndex(tuigateway.LiveTransports(), tuigateway.Transport(seeded)) >= 0 {
		t.Fatal("the disconnect left the transport registered")
	}
}

func TestWsDisconnectReleasesWakeWordOwner(t *testing.T) {
	var released, created []tuigateway.Transport
	prev := wsReleaseWakeForTransport
	wsReleaseWakeForTransport = func(transport tuigateway.Transport) bool {
		released = append(released, transport)
		return true
	}
	t.Cleanup(func() { wsReleaseWakeForTransport = prev })
	wsTestRunDisconnect(t, func(transport *tuigateway.WSTransport) { created = append(created, transport) })
	if len(released) != 1 || !reflect.DeepEqual(released, created) {
		t.Fatalf("released %v, created %v", released, created)
	}
}

func TestWsReadyAdvertisesHeartbeatAndPingIsInline(t *testing.T) {
	wsTestNoOrphanGrace(t)
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
	if ready["type"] != "gateway.ready" || payload["heartbeat"] != true {
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
	prevDispatch, prevClose := wsServerDispatch, wsCloseSessionsForTransport
	wsServerDispatch = func(_ context.Context, req any, _ tuigateway.Transport) map[string]any {
		frame, _ := plainRequest(req).(map[string]any)
		method, _ := frame["method"].(string)
		log.add("dispatch:" + method)
		if method == "slow" || method == "session.context_breakdown" {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		log.add("done:" + method)
		return map[string]any{"jsonrpc": "2.0", "id": frame["id"], "result": map[string]any{"method": method}}
	}
	wsCloseSessionsForTransport = func(context.Context, tuigateway.Transport, string) (int, int) {
		log.add("teardown")
		return 0, 0
	}
	t.Cleanup(func() { wsServerDispatch, wsCloseSessionsForTransport = prevDispatch, prevClose })
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

// Authority connection of the fallback cases, every request answered as an unknown method
type wsTestConnection struct {
	actor gateway.Principal
}

func (c *wsTestConnection) Dispatch(_ context.Context, request *pyjson.Object) (*pyjson.Object, error) {
	return pyjson.ObjectOf("jsonrpc", "2.0", "id", request.GetOr("id", nil),
		"error", pyjson.ObjectOf("code", -32601, "message", "unknown method")), nil
}
func (c *wsTestConnection) Close(context.Context) error { return nil }
func (c *wsTestConnection) Actor() gateway.Principal    { return c.actor }
func (c *wsTestConnection) AwaitsProfileStart(context.Context, map[string]any, []string) (string, <-chan struct{}, bool, error) {
	return "", nil, false, nil
}

// Installs a fake authority connection with the capabilities and profile for one case
func wsTestFakeAuthority(t *testing.T, capabilities []string, profileID string) {
	t.Helper()
	held := map[string]bool{}
	for _, capability := range capabilities {
		held[capability] = true
	}
	prev := wsNewAuthorityConnection
	wsNewAuthorityConnection = func(*gateway.SessionAuthority, *tuigateway.WSTransport, map[string]any, bool) (authorityConn, error) {
		return &wsTestConnection{actor: gateway.Principal{Capabilities: held, ProfileID: profileID}}, nil
	}
	t.Cleanup(func() { wsNewAuthorityConnection = prev })
}

var wsTestInteractive = []string{"session:create", "session:read", "session:submit", "session:control", "session:approve", "session:respond"}

// One call through an authority socket whose dispatch answers unknown method, returning the reply to id 1
func wsTestDriveAuthorityFallback(t *testing.T, capabilities []string, profileID, method string) map[string]any {
	t.Helper()
	wsTestFakeAuthority(t, capabilities, profileID)
	wsTestNoOrphanGrace(t)
	sock := newWSTestSocket()
	sock.authority = new(gateway.SessionAuthority)
	sock.inbound <- pyjson.ObjectOf("jsonrpc", "2.0", "id", 1, "method", method, "params", pyjson.NewObject())
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, map[string]any{"user_id": "u"}, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, frame := range sock.frames() {
		if pyrepr.Equal(frame["id"], 1) {
			return frame
		}
	}
	t.Fatalf("no reply to id 1 in %v", sock.frames())
	return nil
}

// Registers a method for one case and restores the registry after it
func wsTestMethod(t *testing.T, name string, fn Handler) {
	t.Helper()
	prev := lookupMethod(name)
	registerMethod(name, fn)
	t.Cleanup(func() {
		methodsMu.Lock()
		defer methodsMu.Unlock()
		if prev == nil {
			delete(methods, name)
			return
		}
		methods[name] = prev
	})
}

func TestReadyFrameAdvertisesASessionAuthorityOnlyWhenOneIsBound(t *testing.T) {
	wsTestFakeAuthority(t, nil, "p")
	wsTestNoOrphanGrace(t)
	ready := func(authority *gateway.SessionAuthority) map[string]any {
		sock := newWSTestSocket()
		sock.authority = authority
		sock.inbound <- nil
		if err := HandleWS(context.Background(), sock, map[string]any{"user_id": "u"}, nil, false); err != nil {
			t.Fatal(err)
		}
		params, _ := sock.frames()[0]["params"].(map[string]any)
		payload, _ := params["payload"].(map[string]any)
		return payload
	}
	if ready(new(gateway.SessionAuthority))["session_authority"] != true {
		t.Fatal("a bound authority was not advertised")
	}
	if _, advertised := ready(nil)["session_authority"]; advertised {
		t.Fatal("a socket without an authority advertised one")
	}
}

func TestLegacyFallbackRequiresTheInteractiveGrant(t *testing.T) {
	var ran []any
	wsTestMethod(t, "probe.legacy", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		ran = append(ran, rid)
		return map[string]any{"jsonrpc": "2.0", "id": rid, "result": map[string]any{}}
	})
	launch := launchHome()
	for _, restricted := range [][]string{{"worker:adopt"}, nil} {
		reply := wsTestDriveAuthorityFallback(t, restricted, launch, "probe.legacy")
		failure, _ := reply["error"].(map[string]any)
		if !pyrepr.Equal(failure["code"], -32601) || len(ran) != 0 {
			t.Fatalf("grant %v reached the legacy handler: %v ran=%v", restricted, reply, ran)
		}
	}
	reply := wsTestDriveAuthorityFallback(t, wsTestInteractive, launch, "probe.legacy")
	if result, isDict := reply["result"].(map[string]any); !isDict || len(result) != 0 {
		t.Fatalf("interactive reply %v", reply)
	}
	if len(ran) != 1 || !pyrepr.Equal(ran[0], 1) {
		t.Fatalf("ran %v", ran)
	}
}

func TestHandleWsStartsHeartbeatRefresher(t *testing.T) {
	wsTestNoOrphanGrace(t)
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

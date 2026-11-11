package tuiserver

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Gates of the read lane cases, a method waiting on its gate when one is installed
type wsTestGates struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
}

func (g *wsTestGates) install(method string) chan struct{} {
	gate := make(chan struct{})
	g.mu.Lock()
	g.gates[method] = gate
	g.mu.Unlock()
	return gate
}

func (g *wsTestGates) of(method string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gates[method]
}

// Real socket dispatcher over a gated stand in for the request dispatch and a logged teardown
func wsTestReadRig(t *testing.T) (*wsTestSocket, *wsTestGates, *wsTestLog) {
	t.Helper()
	wsTestNoOrphanGrace(t)
	gates, log := &wsTestGates{gates: map[string]chan struct{}{}}, &wsTestLog{}
	prevDispatch, prevClose := wsDispatchRequestFn, wsCloseSessionsForTransport
	wsDispatchRequestFn = func(ctx context.Context, _ authorityConn, req any, method string, _ *tuigateway.WSTransport) (any, error) {
		id := pyrepr.Str(req.(*pyjson.Object).GetOr("id", nil))
		log.add("start:" + id)
		if gate := gates.of(method); gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				log.add("cancelled:" + id)
				return nil, ctx.Err()
			}
		}
		log.add("done:" + id)
		return pyjson.ObjectOf("jsonrpc", "2.0", "id", req.(*pyjson.Object).GetOr("id", nil), "result", pyjson.NewObject()), nil
	}
	wsCloseSessionsForTransport = func(context.Context, tuigateway.Transport, string) (int, int) {
		log.add("teardown")
		return 0, 0
	}
	t.Cleanup(func() { wsDispatchRequestFn, wsCloseSessionsForTransport = prevDispatch, prevClose })
	return newWSTestSocket(), gates, log
}

func TestWaitingContextReadsKeepReaderBackpressure(t *testing.T) {
	sock, gates, log := wsTestReadRig(t)
	release := gates.install("session.context_breakdown")
	done := wsTestServe(context.Background(), sock, nil, false)
	burst := wsDispatchQueueMax * 4
	for n := 0; n < burst; n++ {
		sock.inbound <- wsTestRPC(n, "session.context_breakdown")
	}
	drained := wsTestSettle(func() bool { return len(sock.inbound) == 0 }, 2*time.Second)
	received, started := sock.receivedCount(), log.count("start:")
	close(release)
	if drained {
		t.Fatal("the reader drained every frame past the waiting bound")
	}
	if bound := wsDispatchQueueMax + wsConcurrentReadLimit + wsWaitingReadLimit + 2; received > bound {
		t.Fatalf("reader took %d frames, bound %d", received, bound)
	}
	if started != wsConcurrentReadLimit {
		t.Fatalf("%d reads started, want %d", started, wsConcurrentReadLimit)
	}
	if !wsTestSettle(func() bool { return len(sock.inbound) == 0 }, 30*time.Second) {
		t.Fatal("the released reads never drained the socket")
	}
	sock.inbound <- nil
	if err := wsTestAwait(t, done, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	frames := sock.frames()
	ready, _ := frames[0]["params"].(map[string]any)
	if ready["type"] != "gateway.ready" {
		t.Fatalf("first frame %v", frames[0])
	}
	if replies := len(sock.ids()); replies != burst {
		t.Fatalf("%d replies for %d reads", replies, burst)
	}
}

func TestServerShutdownDoesNotWaitForARunningContextRead(t *testing.T) {
	sock, gates, log := wsTestReadRig(t)
	release := gates.install("session.context_breakdown")
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := wsTestServe(ctx, sock, nil, false)
	sock.inbound <- wsTestRPC(1, "session.context_breakdown")
	if !wsTestSettle(func() bool { return log.has("start:1") }, 5*time.Second) {
		t.Fatal("the read never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("socket shutdown still waits for the slow context read")
	}
	if !wsTestSettle(func() bool { return log.has("cancelled:1") }, 5*time.Second) {
		t.Fatalf("the running read was not cancelled: %v", log.snapshot())
	}
}

func TestHangUpDropsReadsThatNeverStarted(t *testing.T) {
	sock, gates, log := wsTestReadRig(t)
	release := gates.install("session.context_breakdown")
	done := wsTestServe(context.Background(), sock, nil, false)
	for n := 0; n < 6; n++ {
		sock.inbound <- wsTestRPC(n, "session.context_breakdown")
	}
	if !wsTestSettle(func() bool { return sock.receivedCount() == 6 && log.count("start:") == 2 }, 5*time.Second) {
		close(release)
		t.Fatalf("reads did not settle: received %d, log %v", sock.receivedCount(), log.snapshot())
	}
	sock.inbound <- nil
	torn := wsTestSettle(func() bool { return log.has("teardown") }, 2*time.Second)
	close(release)
	if torn {
		t.Fatal("teardown ran under a started read")
	}
	if err := wsTestAwait(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var started []string
	entries := log.snapshot()
	for _, entry := range entries {
		if len(entry) > 6 && entry[:6] == "start:" {
			started = append(started, entry)
		}
	}
	sort.Strings(started)
	if len(started) != 2 || started[0] != "start:0" || started[1] != "start:1" {
		t.Fatalf("started %v", started)
	}
	if entries[len(entries)-1] != "teardown" {
		t.Fatalf("log %v", entries)
	}
}

func TestAFailedReadReplyDoesNotCancelTheRunningControl(t *testing.T) {
	sock, gates, log := wsTestReadRig(t)
	release := gates.install("prompt.submit")
	readGate := gates.install("session.context_breakdown")
	sock.failIDs["read"] = true
	done := wsTestServe(context.Background(), sock, nil, false)
	sock.inbound <- wsTestRPC("read", "session.context_breakdown")
	if !wsTestSettle(func() bool { return log.has("start:read") }, 5*time.Second) {
		t.Fatal("the read never started")
	}
	sock.inbound <- wsTestRPC("submit", "prompt.submit")
	if !wsTestSettle(func() bool { return log.has("start:submit") }, 5*time.Second) {
		t.Fatal("the control never started")
	}
	close(readGate)
	if !wsTestSettle(func() bool { return log.has("done:read") }, 5*time.Second) {
		t.Fatal("the read never finished")
	}
	ended := false
	select {
	case <-done:
		ended = true
	case <-time.After(2 * time.Second):
	}
	close(release)
	if ended {
		t.Fatal("the link ended under the running control")
	}
	if err := wsTestAwait(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	entries := log.snapshot()
	if log.has("cancelled:submit") || !log.has("done:submit") || entries[len(entries)-1] != "teardown" {
		t.Fatalf("log %v", entries)
	}
}

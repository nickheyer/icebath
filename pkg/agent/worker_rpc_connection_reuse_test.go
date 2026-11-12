package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Loopback TCP relay whose owner to worker direction can be held, the replies a stalled owner has not written yet
type ownerRelay struct {
	mu       sync.Mutex
	gate     chan struct{}
	listener net.Listener
}

func newOwnerRelay(t *testing.T, ownerAddr string) *ownerRelay {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &ownerRelay{listener: listener, gate: make(chan struct{})}
	close(r.gate)
	go func() {
		for {
			worker, err := listener.Accept()
			if err != nil {
				return
			}
			owner, err := net.Dial("tcp", ownerAddr)
			if err != nil {
				worker.Close()
				continue
			}
			go r.pump(worker, owner, false)
			go r.pump(owner, worker, true)
		}
	}()
	return r
}

func (r *ownerRelay) pump(src, dst net.Conn, gated bool) {
	defer src.Close()
	defer dst.Close()
	buf := make([]byte, 65536)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if gated {
				r.mu.Lock()
				gate := r.gate
				r.mu.Unlock()
				<-gate
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Holds the owner to worker direction for the given seconds
func (r *ownerRelay) stall(seconds float64) {
	gate := make(chan struct{})
	r.mu.Lock()
	r.gate = gate
	r.mu.Unlock()
	time.AfterFunc(time.Duration(seconds*float64(time.Second)), func() { close(gate) })
}

// In process owner of the reuse cases, its state the hermes fixture's SimpleNamespace
type reuseOwner struct {
	mu          sync.Mutex
	connections int
	tickets     int
	requests    []string
	drop        string
	closed      chan struct{}
	pongs       []bool
	flooded     chan struct{}
	relay       *ownerRelay
}

func (o *reuseOwner) snapshot() (int, int, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.connections, o.tickets, append([]string(nil), o.requests...)
}

func (o *reuseOwner) setDrop(mode string) {
	o.mu.Lock()
	o.drop = mode
	o.mu.Unlock()
}

// Serves one worker connection the way the hermes fixture's handler does
func (o *reuseOwner) serve(ws *websocket.Conn) {
	o.mu.Lock()
	o.connections++
	o.mu.Unlock()
	pong := make(chan struct{}, 1)
	ws.SetPongHandler(func(string) error {
		select {
		case pong <- struct{}{}:
		default:
		}
		return nil
	})
	incoming := make(chan []byte)
	go func() {
		defer close(incoming)
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			incoming <- data
		}
	}()
	send := func(v any) { data, _ := json.Marshal(v); _ = ws.WriteMessage(websocket.TextMessage, data) }
	for raw := range incoming {
		var request map[string]any
		if json.Unmarshal(raw, &request) != nil {
			break
		}
		method, _ := request["method"].(string)
		params, _ := request["params"].(map[string]any)
		o.mu.Lock()
		o.requests = append(o.requests, method)
		n, drop := len(o.requests), o.drop
		o.mu.Unlock()
		if drop == "before_reply" {
			break
		}
		if method == "stall" {
			o.relay.stall(params["seconds"].(float64))
		}
		send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "sessions.changed"}})
		if method == "fail" {
			send(map[string]any{"jsonrpc": "2.0", "id": request["id"], "error": map[string]any{"code": -32000, "message": "stale_generation"}})
			continue
		}
		result := map[string]any{"n": n}
		for k, v := range params {
			result[k] = v
		}
		send(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
		if method == "flood" {
			for index := range int(params["events"].(float64)) {
				time.Sleep(5 * time.Millisecond)
				send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "sessions.changed", "index": index}})
			}
			_ = ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
			answered := false
			select {
			case <-pong:
				answered = true
			case <-time.After(2 * time.Second):
			}
			o.mu.Lock()
			o.pongs = append(o.pongs, answered)
			o.mu.Unlock()
			close(o.flooded)
			if !answered {
				break
			}
		}
		if drop == "after_reply" {
			break
		}
	}
	o.setDrop("")
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	ws.Close()
	close(o.closed)
}

// Starts the owner's WS server behind a relay and its control socket answering identify and session-ticket for the home
func startReuseOwner(t *testing.T, home string) *reuseOwner {
	t.Helper()
	owner := &reuseOwner{closed: make(chan struct{}), flooded: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{gateway.CanonicalGatewayProtocol}, CheckOrigin: func(*http.Request) bool { return true }}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			owner.serve(ws)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	owner.relay = newOwnerRelay(t, listener.Addr().String())
	t.Cleanup(func() {
		owner.relay.mu.Lock()
		select {
		case <-owner.relay.gate:
		default:
			close(owner.relay.gate)
		}
		owner.relay.mu.Unlock()
		owner.relay.listener.Close()
		server.Close()
	})
	canonical, err := gateway.CanonicalHome(home)
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + owner.relay.listener.Addr().String()
	control, err := gateway.NewGatewayControlServer(canonical, map[string]gateway.ControlVerbHandler{
		"identify": func(map[string]any) (any, error) {
			payload := gateway.BuildIdentifyPayload()
			for _, kv := range [][2]any{{"pid", int64(-1)}, {"runtime_protocol", int64(1)}, {"state", "ready"}, {"supervisor", "external"},
				{"capabilities", []any{"session-authority-v1"}}, {"authority_epoch", int64(1)}, {"instance_id", "owner-instance"},
				{"api_origin", origin}, {"served_profiles", []any{pyjson.ObjectOf("home", canonical, "profile_id", canonical)}}} {
				payload.Set(kv[0].(string), kv[1])
			}
			return payload, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.AddPrivateHandlers(map[string]gateway.ControlPrivateHandler{
		"session-ticket": func(params map[string]any, _ string) (any, error) {
			if params["purpose"] != "worker-adoption" {
				t.Errorf("ticket purpose %v", params["purpose"])
			}
			owner.mu.Lock()
			owner.tickets++
			n := owner.tickets
			owner.mu.Unlock()
			return pyjson.ObjectOf("ticket", "ticket-"+string(rune('0'+n)), "profile_id", canonical, "instance_id", "owner-instance",
				"runtime_protocol", int64(1)), nil
		},
	})
	if !control.Start() {
		t.Fatal("control server did not start")
	}
	t.Cleanup(control.Stop)
	return owner
}

// One call's result as a plain map
func reuseCall(t *testing.T, rpc *WorkerRPC, method string, kv ...any) map[string]any {
	t.Helper()
	result, err := rpc.Call(context.Background(), method, pyjson.ObjectOf(kv...))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	plain, _ := pyjson.Plain(result).(map[string]any)
	return plain
}

func waitLinkClosed(t *testing.T, rpc *WorkerRPC) {
	t.Helper()
	select {
	case <-rpc.link.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the link reader never observed the close")
	}
}

func TestOneConnectionServesEveryCall(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	for i := 1; i <= 5; i++ {
		if n := reuseCall(t, rpc, "worker.persist")["n"]; !pyjson.Equal(n, int64(i)) {
			t.Fatalf("call %d answered n=%v", i, n)
		}
	}
	if connections, tickets, _ := owner.snapshot(); connections != 1 || tickets != 1 {
		t.Fatalf("connections %d tickets %d", connections, tickets)
	}
}

func TestReconnectsWithAFreshTicketAfterTheOwnerDropsIt(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	owner.setDrop("after_reply")
	if got := reuseCall(t, rpc, "worker.persist"); !pyjson.Equal(got, map[string]any{"n": int64(1)}) {
		t.Fatalf("first %v", got)
	}
	select {
	case <-owner.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never closed")
	}
	waitLinkClosed(t, rpc)
	if got := reuseCall(t, rpc, "worker.persist"); !pyjson.Equal(got, map[string]any{"n": int64(2)}) {
		t.Fatalf("second %v", got)
	}
	if connections, tickets, _ := owner.snapshot(); connections != 2 || tickets != 2 {
		t.Fatalf("connections %d tickets %d", connections, tickets)
	}
}

func TestInFlightCallOnADroppedConnectionFailsAndIsNotResent(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	if got := reuseCall(t, rpc, "worker.persist"); !pyjson.Equal(got, map[string]any{"n": int64(1)}) {
		t.Fatalf("first %v", got)
	}
	owner.setDrop("before_reply")
	started := time.Now()
	_, err := rpc.Call(context.Background(), "worker.persist", nil)
	var closed *websocket.CloseError
	if err == nil || !(asCloseError(err, &closed) || isNetClosed(err)) {
		t.Fatalf("in flight call error %v, want the transport close", err)
	}
	if time.Since(started) >= 5*time.Second {
		t.Fatal("the failure waited out the receipt budget")
	}
	if _, _, requests := owner.snapshot(); len(requests) != 2 || requests[0] != "worker.persist" || requests[1] != "worker.persist" {
		t.Fatalf("requests %v", requests)
	}
	if got := reuseCall(t, rpc, "worker.persist"); !pyjson.Equal(got, map[string]any{"n": int64(3)}) {
		t.Fatalf("third %v", got)
	}
	if connections, _, _ := owner.snapshot(); connections != 2 {
		t.Fatalf("connections %d", connections)
	}
}

func asCloseError(err error, target **websocket.CloseError) bool {
	closeErr, ok := err.(*websocket.CloseError)
	if ok {
		*target = closeErr
	}
	return ok
}

func isNetClosed(err error) bool {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	_, isNet := err.(net.Error)
	_, isOp := err.(*net.OpError)
	return isNet || isOp
}

func TestIdleBroadcastsPastTheReceiveHighWaterMarkKeepTheConnection(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	if n := reuseCall(t, rpc, "flood", "events", int64(64))["n"]; !pyjson.Equal(n, int64(1)) {
		t.Fatalf("flood n=%v", n)
	}
	select {
	case <-owner.flooded:
	case <-time.After(10 * time.Second):
		t.Fatal("owner never finished the flood")
	}
	if n := reuseCall(t, rpc, "worker.persist")["n"]; !pyjson.Equal(n, int64(2)) {
		t.Fatalf("persist n=%v", n)
	}
	owner.mu.Lock()
	pongs := append([]bool(nil), owner.pongs...)
	owner.mu.Unlock()
	if len(pongs) != 1 || !pongs[0] {
		t.Fatalf("pongs %v", pongs)
	}
	if connections, tickets, _ := owner.snapshot(); connections != 1 || tickets != 1 {
		t.Fatalf("connections %d tickets %d", connections, tickets)
	}
}

func TestOwnerStallShorterThanTheReceiptBudgetDoesNotFailOnAPingDeadline(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	if n := reuseCall(t, rpc, "worker.persist")["n"]; !pyjson.Equal(n, int64(1)) {
		t.Fatalf("first n=%v", n)
	}
	started := time.Now()
	if got := reuseCall(t, rpc, "stall", "seconds", 1.5); !pyjson.Equal(got, map[string]any{"n": int64(2), "seconds": 1.5}) {
		t.Fatalf("stall %v", got)
	}
	if time.Since(started) < 1400*time.Millisecond {
		t.Fatalf("stall answered after %v", time.Since(started))
	}
	if n := reuseCall(t, rpc, "worker.persist")["n"]; !pyjson.Equal(n, int64(3)) {
		t.Fatalf("third n=%v", n)
	}
	if connections, _, _ := owner.snapshot(); connections != 1 {
		t.Fatalf("connections %d", connections)
	}
}

func TestConcurrentCallersAreSerializedAndEachGetsItsOwnReply(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	replies := map[string][]map[string]any{"a": nil, "b": nil}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, tag := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range 25 {
				result, err := rpc.Call(context.Background(), "worker.persist", pyjson.ObjectOf("tag", tag, "index", int64(index)))
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				replies[tag] = append(replies[tag], pyjson.Plain(result).(map[string]any))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("errors %v", errs)
	}
	var ns []int
	for tag, got := range replies {
		if len(got) != 25 {
			t.Fatalf("%s got %d replies", tag, len(got))
		}
		for index, reply := range got {
			if reply["tag"] != tag || !pyjson.Equal(reply["index"], int64(index)) {
				t.Fatalf("%s reply %d is %v", tag, index, reply)
			}
			n, _ := reply["n"].(int64)
			ns = append(ns, int(n))
		}
	}
	sort.Ints(ns)
	for i, n := range ns {
		if n != i+1 {
			t.Fatalf("reply counters %v", ns)
		}
	}
	if connections, tickets, _ := owner.snapshot(); connections != 1 || tickets != 1 {
		t.Fatalf("connections %d tickets %d", connections, tickets)
	}
}

func TestAnOwnerErrorReplyKeepsTheConnectionForTheNextCall(t *testing.T) {
	home := t.TempDir()
	owner := startReuseOwner(t, home)
	rpc := mustNewWorkerRPC(t, home)
	defer rpc.Close()
	_, err := rpc.Call(context.Background(), "fail", nil)
	var persistence *tools.WorkerPersistenceError
	if !asWorkerPersistence(err, &persistence) || err.Error() != "stale_generation" {
		t.Fatalf("fail error %v", err)
	}
	if got := reuseCall(t, rpc, "worker.persist"); !pyjson.Equal(got, map[string]any{"n": int64(2)}) {
		t.Fatalf("persist %v", got)
	}
	if connections, tickets, requests := owner.snapshot(); connections != 1 || tickets != 1 || len(requests) != 2 || requests[0] != "fail" || requests[1] != "worker.persist" {
		t.Fatalf("connections %d tickets %d requests %v", connections, tickets, requests)
	}
}

func asWorkerPersistence(err error, target **tools.WorkerPersistenceError) bool {
	typed, ok := err.(*tools.WorkerPersistenceError)
	if ok {
		*target = typed
	}
	return ok
}

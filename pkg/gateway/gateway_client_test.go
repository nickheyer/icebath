package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Socket whose frames are all buffered already, staying open after them until closed
type backlogSocket struct {
	mu      sync.Mutex
	frames  []string
	sent    []string
	onSend  func(data string)
	closed  chan struct{}
	more    chan struct{}
	once    sync.Once
	isShut  bool
	subprot string
}

func newBacklogSocket(frames []string) *backlogSocket {
	return &backlogSocket{frames: frames, closed: make(chan struct{}), more: make(chan struct{}, 1)}
}

func (s *backlogSocket) Recv() (string, error) {
	for {
		s.mu.Lock()
		if len(s.frames) > 0 {
			f := s.frames[0]
			s.frames = s.frames[1:]
			s.mu.Unlock()
			return f, nil
		}
		s.mu.Unlock()
		select {
		case <-s.closed:
			return "", errors.New("connection closed")
		case <-s.more:
		}
	}
}

func (s *backlogSocket) push(frames ...string) {
	s.mu.Lock()
	s.frames = append(s.frames, frames...)
	s.mu.Unlock()
	select {
	case s.more <- struct{}{}:
	default:
	}
}

func (s *backlogSocket) Send(data string) error {
	s.mu.Lock()
	s.sent = append(s.sent, data)
	hook := s.onSend
	s.mu.Unlock()
	if hook != nil {
		hook(data)
	}
	return nil
}

func (s *backlogSocket) Open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.isShut
}

func (s *backlogSocket) Subprotocol() string { return s.subprot }

func (s *backlogSocket) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.isShut = true
		s.mu.Unlock()
		close(s.closed)
	})
	return nil
}

func backlogFrame(kind, text string) string {
	return fmt.Sprintf(`{"jsonrpc": "2.0", "method": "event", "params": {"type": %q, "session_id": "local-backlog", "payload": {"text": %q}}}`, kind, text)
}

func frameText(t *testing.T, item any) string {
	t.Helper()
	obj, ok := item.(*pyjson.Object)
	if !ok {
		t.Fatalf("not a frame: %#v", item)
	}
	return obj.Vals["params"].(*pyjson.Object).Vals["payload"].(*pyjson.Object).Vals["text"].(string)
}

func TestBacklogLargerThanTheClientQueueReachesTheConsumerIntact(t *testing.T) {
	frames := make([]string, 0, 6001)
	for i := 0; i < 6000; i++ {
		frames = append(frames, backlogFrame("message.delta", fmt.Sprint(i)))
	}
	frames = append(frames, backlogFrame("message.complete", "done"))
	client := NewGatewayClient(newBacklogSocket(frames)).Start()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 6000; i++ {
		item, err := client.Events.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := frameText(t, item); got != fmt.Sprint(i) {
			t.Fatalf("frame %d = %s", i, got)
		}
	}
	if item, err := client.Events.Get(ctx); err != nil || frameText(t, item) != "done" {
		t.Fatal(item, err)
	}
}

func TestAConsumerThatStopsDrainingIsStillFailedWithTheQueueBounded(t *testing.T) {
	savedStall, savedFrames := EventStallTimeout, EventOverflowFrames
	t.Cleanup(func() { EventStallTimeout, EventOverflowFrames = savedStall, savedFrames })
	EventStallTimeout, EventOverflowFrames = 50*time.Millisecond, 100
	frames := make([]string, 6000)
	for i := range frames {
		frames[i] = backlogFrame("message.delta", fmt.Sprint(i))
	}
	client := NewGatewayClient(newBacklogSocket(frames))
	pending := make(chan rpcAnswer, 1)
	client.pending[1] = pending
	done := make(chan struct{})
	go func() { client.Read(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never failed the stalled consumer")
	}
	var clientErr *GatewayClientError
	if a := <-pending; !errors.As(a.err, &clientErr) || !strings.Contains(a.err.Error(), "outcome is unknown") {
		t.Fatal(a.err)
	}
	if !client.Events.Full() || client.Events.OverflowLen() != 101 {
		t.Fatal(client.Events.Full(), client.Events.OverflowLen())
	}
	var queued []any
	for i := 0; i < client.Events.Maxsize()+101; i++ {
		item, ok := client.Events.GetNowait()
		if !ok {
			t.Fatal("queue drained early at", i)
		}
		queued = append(queued, item)
	}
	if !client.Events.Empty() {
		t.Fatal("queue not empty")
	}
	if _, ok := queued[len(queued)-1].(*GatewayClientError); !ok {
		t.Fatal(queued[len(queued)-1])
	}
	for i, item := range queued[:len(queued)-1] {
		if frameText(t, item) != fmt.Sprint(i) {
			t.Fatal(i)
		}
	}
	if len(queued)-1 != 4196 {
		t.Fatal(len(queued))
	}
}

func TestClosingAsAWaitingFrameLandsStillEndsTheReader(t *testing.T) {
	frames := make([]string, 4097)
	for i := range frames {
		frames[i] = backlogFrame("message.delta", fmt.Sprint(i))
	}
	client := NewGatewayClient(newBacklogSocket(frames)).Start()
	for !client.Events.Full() {
		time.Sleep(time.Millisecond)
	}
	client.Events.GetNowait()
	closed := make(chan struct{})
	go func() { client.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close waited on the open socket")
	}
}

func TestResumeAnswerBehindABurstStaysReachable(t *testing.T) {
	sock := newBacklogSocket(nil)
	sock.onSend = func(data string) {
		burst := make([]string, 0, 5001)
		for i := 0; i < 5000; i++ {
			burst = append(burst, backlogFrame("message.delta", fmt.Sprint(i)))
		}
		burst = append(burst, `{"jsonrpc": "2.0", "id": 1, "result": {"session_id": "s", "resumed": true}}`)
		sock.push(burst...)
	}
	client := NewGatewayClient(sock).Start()
	defer client.Close()
	got, err := client.RPC(context.Background(), "session.resume", map[string]any{"session_id": "s"})
	if err != nil || got["resumed"] != true || got["session_id"] != "s" {
		t.Fatal(got, err)
	}
	if !strings.Contains(sock.sent[0], `"method": "session.resume"`) || !strings.Contains(sock.sent[0], `"id": 1`) {
		t.Fatal(sock.sent)
	}
	if client.Events.QSize()+client.Events.OverflowLen() != 5000 {
		t.Fatal(client.Events.QSize(), client.Events.OverflowLen())
	}
}

func TestAFullQueueNeverDropsAPostAckRecovery(t *testing.T) {
	q := NewEventQueue(2)
	q.Hold("a", 1)
	q.Hold("b", 1)
	PutBehind(q, "recovery")
	if q.OverflowLen() != 1 || q.OverflowBytes != 0 {
		t.Fatal(q.OverflowLen(), q.OverflowBytes)
	}
	for _, want := range []string{"a", "b", "recovery"} {
		if got, ok := q.GetNowait(); !ok || got != want {
			t.Fatal(got, want)
		}
	}
}

func TestRPCRefusalsAndTransportFailures(t *testing.T) {
	sock := newBacklogSocket(nil)
	sock.onSend = func(string) {
		sock.push(`{"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "unknown method: x", "data": {"reason": "owner_gone", "nested": {"a": 1}, "list": [1, "x", null], "long": "` + strings.Repeat("y", 201) + `"}}}`)
	}
	client := NewGatewayClient(sock).Start()
	_, err := client.RPC(context.Background(), "x", nil)
	var rpcErr *GatewayRPCError
	if !errors.As(err, &rpcErr) || rpcErr.Msg != "owner_gone" || *rpcErr.Code != -32000 {
		t.Fatal(err)
	}
	if _, ok := rpcErr.Data["nested"]; ok || len(rpcErr.Data) != 2 || len(rpcErr.Data["list"].([]any)) != 3 {
		t.Fatal(rpcErr.Data)
	}
	var clientErr *GatewayClientError
	if !errors.As(err, &clientErr) {
		t.Fatal("a refusal is a client error")
	}
	client.Close()
	if _, err := client.RPC(context.Background(), "x", nil); err == nil || err.Error() != GatewayNotSent {
		t.Fatal(err)
	}
	if got := RPCError(pyjson.ObjectOf("message", "bad reason!", "code", true)); got.Msg != "request_failed" || got.Code != nil {
		t.Fatal(got)
	}
	if got := RPCError("nope"); got.Msg != "request_failed" || len(got.Data) != 0 {
		t.Fatal(got)
	}
}

func TestRPCTimeoutAndPendingFailureOnHangup(t *testing.T) {
	sock := newBacklogSocket(nil)
	client := NewGatewayClient(sock).Start()
	if _, err := client.RPCTimeout(context.Background(), "slow", nil, 20*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := client.RPCTimeout(context.Background(), "forever", nil, -1)
		result <- err
	}()
	for {
		client.mu.Lock()
		n := len(client.pending)
		client.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	sock.Close()
	if err := <-result; err == nil || err.Error() != GatewayDisconnected {
		t.Fatal(err)
	}
}

func TestGatewayWSTargetDialsTheTicketSubprotocol(t *testing.T) {
	url, protocols := GatewayWSTarget(&GatewayEndpoint{APIOrigin: "https://127.0.0.1:8443"}, "abc")
	if url != "wss://127.0.0.1:8443/api/ws" || protocols[0] != CanonicalGatewayProtocol || protocols[1] != "icebath-gateway-ticket.abc" {
		t.Fatal(url, protocols)
	}
	if url, _ := GatewayWSTarget(&GatewayEndpoint{APIOrigin: "http://127.0.0.1:9"}, "t"); url != "ws://127.0.0.1:9/api/ws" {
		t.Fatal(url)
	}
}

func TestExplicitGatewayURLMustBeAWebSocket(t *testing.T) {
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "http://example.com/ws")
	if _, err := ConnectGateway(context.Background()); err == nil || err.Error() != "Invalid explicit gateway WebSocket URL; no local fallback" {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://127.0.0.1:1/api/ws")
	if _, err := ConnectGateway(context.Background()); err == nil || err.Error() != "Gateway connection failed; no local fallback" {
		t.Fatal(err)
	}
}

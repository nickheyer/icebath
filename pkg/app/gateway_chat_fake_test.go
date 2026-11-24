package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Answer that holds the request until the caller gives up, a turn still running at the owner
var errFakeBlock = errors.New("fake owner holds this request")

// One recorded request of the fake owner
type fakeGatewayCall struct {
	method string
	params map[string]any
}

// Owner fake answering requests through a handler and queueing events like the attached client
type fakeGatewayPeer struct {
	t      *testing.T
	mu     sync.Mutex
	events *gateway.EventQueue
	calls  []fakeGatewayCall
	answer func(p *fakeGatewayPeer, method string, params map[string]any) (any, error)
	closed bool
}

func newFakeGatewayPeer(t *testing.T, answer func(p *fakeGatewayPeer, method string, params map[string]any) (any, error)) *fakeGatewayPeer {
	return &fakeGatewayPeer{t: t, events: gateway.NewEventQueue(4096), answer: answer}
}

// Decoded JSON form of a native value, the shape answers arrive in off the socket
func wireValue(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jsonutil.Decode(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func (p *fakeGatewayPeer) RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	plain, _ := jsonutil.Plain(params).(map[string]any)
	p.mu.Lock()
	p.calls = append(p.calls, fakeGatewayCall{method, plain})
	p.mu.Unlock()
	result, err := p.answer(p, method, plain)
	if errors.Is(err, errFakeBlock) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil || result == nil {
		return nil, err
	}
	return wireValue(p.t, result), nil
}

func (p *fakeGatewayPeer) EventQueue() *gateway.EventQueue { return p.events }

func (p *fakeGatewayPeer) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

// Queues one owner event notification
func (p *fakeGatewayPeer) emit(params map[string]any) {
	p.events.PutNowait(wireValue(p.t, map[string]any{"method": "event", "params": params}))
}

// Waits until the renderer drained every queued event
func (p *fakeGatewayPeer) drained() {
	deadline := time.Now().Add(2 * time.Second)
	for !p.events.Empty() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
}

// Connector handing the launch this fake
func (p *fakeGatewayPeer) connector() func(ctx context.Context) (gatewayChatConn, error) {
	return func(ctx context.Context) (gatewayChatConn, error) { return p, nil }
}

// Runs the view to completion within the deadline the Python cases give asyncio.wait_for
func runViewWithin(t *testing.T, v *GatewayChatView, query string) (int, error) {
	t.Helper()
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		code, err := v.Run(ctx, query)
		done <- outcome{code, err}
	}()
	select {
	case o := <-done:
		return o.code, o.err
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("view did not finish within 2s")
	}
	return 0, nil
}

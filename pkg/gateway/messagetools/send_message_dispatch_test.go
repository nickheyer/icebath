package messagetools

import (
	"context"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type dispatchLoopKey struct{}

// Runner double exposing its _gateway_loop, counted on that loop when the context carries the marker
type dispatchRunner struct{ loop gateway.EventLoop }

func (r dispatchRunner) Running() bool                  { return true }
func (r dispatchRunner) GatewayLoop() gateway.EventLoop { return r.loop }
func (r dispatchRunner) OnGatewayLoop(ctx context.Context) bool {
	return r.loop != nil && ctx.Value(dispatchLoopKey{}) != nil
}

// Closed loop still reporting running, the race between the running check and the threadsafe schedule
type closedRunningLoop struct{ *gateway.CallbackLoop }

func (closedRunningLoop) IsRunning() bool { return true }

// Send dispatch awaits directly without a loop or on it, refuses a stopped or closed loop, else runs elsewhere
func TestDispatchOnGatewayLoopFollowsTheGatewayLoop(t *testing.T) {
	ctx := context.Background()
	send := func(context.Context) (any, error) { return "sent", nil }
	if got, err := dispatchOnGatewayLoop(ctx, struct{}{}, send, ""); err != nil || got != "sent" {
		t.Fatalf("runner without a loop %v %v", got, err)
	}
	if got, err := dispatchOnGatewayLoop(ctx, dispatchRunner{}, send, ""); err != nil || got != "sent" {
		t.Fatalf("nil loop %v %v", got, err)
	}
	loop := gateway.NewCallbackLoop()
	runner := dispatchRunner{loop: loop}
	stopped, err := dispatchOnGatewayLoop(ctx, runner, send, "")
	if err != nil || !reflect.DeepEqual(stopped, map[string]any{"error": "Gateway loop is not running; cannot dispatch adapter send"}) {
		t.Fatalf("stopped loop %v %v", stopped, err)
	}
	if got, err := dispatchOnGatewayLoop(context.WithValue(ctx, dispatchLoopKey{}, true), runner, send, ""); err != nil || got != "sent" {
		t.Fatalf("on the loop %v %v", got, err)
	}
	loopCtx, stop := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() { ran <- loop.Run(loopCtx) }()
	for !loop.IsRunning() {
		runtime.Gosched()
	}
	if got, err := dispatchOnGatewayLoop(ctx, runner, send, ""); err != nil || got != "sent" {
		t.Fatalf("running loop %v %v", got, err)
	}
	stop()
	<-ran
	if loop.IsRunning() {
		t.Fatal("a returned loop still reports running")
	}
	loop.Close()
	closed, err := dispatchOnGatewayLoop(ctx, dispatchRunner{loop: closedRunningLoop{loop}}, send, "send_message: failed to schedule on gateway loop")
	if err != nil || !reflect.DeepEqual(closed, map[string]any{"error": "Gateway loop unavailable for send dispatch"}) {
		t.Fatalf("closed loop %v %v", closed, err)
	}
}

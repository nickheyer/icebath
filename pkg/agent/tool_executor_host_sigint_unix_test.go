//go:build unix

package agent

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSIGINTDuringToolRaisesKeyboardInterrupt(t *testing.T) {
	started := make(chan struct{})
	a, _, _ := executorTestAgent(t, map[string]tools.Handler{"web_search": func(ctx context.Context, _ *tools.Call) string {
		close(started)
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		return "late"
	}})
	ctx, stop := KeyboardInterruptContext(t.Context())
	defer stop()
	go func() {
		<-started
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	}()
	var messages []*state.Message
	err := a.toolExecutor().ExecuteToolCallsSequential(ctx, []state.ToolCall{toolCall("c1", "web_search", "{}")}, &messages, "task-1", true)
	if !tools.IsKeyboardInterrupt(ctx, err) || !errors.Is(context.Cause(ctx), tools.ErrKeyboardInterrupt) {
		t.Fatalf("err %v cause %v", err, context.Cause(ctx))
	}
	if relayOutcomeForFailure(ctx, errors.New("dispatch aborted")) != "cancelled" {
		t.Fatal("a failure under the SIGINT cause is not a cancelled turn")
	}
}

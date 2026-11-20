package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Records every heartbeat touch of the parent, signalling when the count passes threshold
func recordHeartbeatTouches(t *testing.T, threshold int) (func() []string, <-chan struct{}) {
	t.Helper()
	var mu sync.Mutex
	var descs []string
	passed := make(chan struct{})
	var once sync.Once
	prev := childHeartbeatTouchFn
	childHeartbeatTouchFn = func(_ *AIAgent, desc string) {
		mu.Lock()
		descs = append(descs, desc)
		n := len(descs)
		mu.Unlock()
		if n > threshold {
			once.Do(func() { close(passed) })
		}
	}
	t.Cleanup(func() { childHeartbeatTouchFn = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), descs...)
	}, passed
}

// Heartbeat knobs of the delegate heartbeat tests
func heartbeatKnobs(t *testing.T, idle, inTool int) {
	t.Helper()
	prevInterval, prevIdle, prevInTool := delegateHeartbeatInterval, delegateHeartbeatStaleIdle, delegateHeartbeatStaleInTool
	delegateHeartbeatInterval, delegateHeartbeatStaleIdle, delegateHeartbeatStaleInTool = 10*time.Millisecond, idle, inTool
	t.Cleanup(func() {
		delegateHeartbeatInterval, delegateHeartbeatStaleIdle, delegateHeartbeatStaleInTool = prevInterval, prevIdle, prevInTool
	})
}

// Conversation that returns done once the signal fires or five seconds pass
func waitThenDone(signal <-chan struct{}, calls int) func(context.Context) (map[string]any, error) {
	return func(context.Context) (map[string]any, error) {
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
		}
		return map[string]any{"final_response": "done", "completed": true, "api_calls": calls}, nil
	}
}

func TestDelegateHeartbeatHeartbeatTouchesParentActivityDuringChildRun(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	heartbeatKnobs(t, delegateHeartbeatStaleIdle, delegateHeartbeatStaleInTool)
	touches, first := recordHeartbeatTouches(t, 0)
	child := livenessChild(parent.env, "hb-child", 3, 0)
	child.currentTool, child.lastActivityDesc = "terminal", "executing tool: terminal"
	scriptChildConversation(t, child, waitThenDone(first, 3))
	livenessKnobs(t, nil)
	runLiveness(t, parent.env, child, "Test heartbeat")
	descs := touches()
	if len(descs) == 0 {
		t.Fatal("Heartbeat did not propagate activity to parent")
	}
	for _, d := range descs {
		if strings.Contains(d, "terminal") {
			return
		}
	}
	t.Fatalf("Heartbeat descriptions should include child tool info: %v", descs)
}

func TestDelegateHeartbeatHeartbeatStopsAfterChildCompletes(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	heartbeatKnobs(t, delegateHeartbeatStaleIdle, delegateHeartbeatStaleInTool)
	touches, _ := recordHeartbeatTouches(t, 1<<30)
	child := livenessChild(parent.env, "hb-done", 1, 0)
	child.lastActivityDesc = "done"
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		return map[string]any{"final_response": "done", "completed": true, "api_calls": 1}, nil
	})
	livenessKnobs(t, nil)
	runLiveness(t, parent.env, child, "Test cleanup")
	after := len(touches())
	time.Sleep(50 * time.Millisecond)
	if len(touches()) != after {
		t.Fatalf("Heartbeat continued firing after child completed: %d then %d", after, len(touches()))
	}
}

func TestDelegateHeartbeatHeartbeatDoesNotTripIdleStaleWhileInsideTool(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	heartbeatKnobs(t, 2, 40)
	touches, keptGoing := recordHeartbeatTouches(t, 2)
	child := livenessChild(parent.env, "hb-tool", 1, 0)
	child.currentTool, child.lastActivityDesc = "terminal", "executing tool: terminal"
	scriptChildConversation(t, child, waitThenDone(keptGoing, 1))
	livenessKnobs(t, nil)
	runLiveness(t, parent.env, child, "Test long-running tool")
	if n := len(touches()); n <= 2 {
		t.Fatalf("Heartbeat stopped too early while child was inside a tool; got %d touches", n)
	}
}

func TestDelegateHeartbeatHeartbeatDoesNotTripIdleStaleWhileWaitingOnModel(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	heartbeatKnobs(t, 2, 40)
	touches, keptGoing := recordHeartbeatTouches(t, 2)
	child := livenessChild(parent.env, "hb-model", 1, 1000.0)
	child.lastActivityDesc = "waiting for non-streaming API response"
	stopTicking := make(chan struct{})
	var tickWG sync.WaitGroup
	tickWG.Add(1)
	go func() {
		defer tickWG.Done()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopTicking:
				return
			case <-ticker.C:
				child.TouchActivity("waiting for non-streaming API response")
			}
		}
	}()
	scriptChildConversation(t, child, func(ctx context.Context) (map[string]any, error) {
		out, err := waitThenDone(keptGoing, 1)(ctx)
		close(stopTicking)
		tickWG.Wait()
		return out, err
	})
	livenessKnobs(t, nil)
	runLiveness(t, parent.env, child, "Test slow model wait")
	if n := len(touches()); n <= 2 {
		t.Fatalf("Heartbeat stopped too early while child was waiting on the model; got %d touches", n)
	}
}

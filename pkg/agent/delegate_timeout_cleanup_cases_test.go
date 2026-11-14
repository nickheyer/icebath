package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/memory"
)

// Memory provider whose shutdown records whether the child closed before its worker finished
type unwindCloseProbe struct {
	delegationRecorder
	finished          <-chan struct{}
	closed            chan struct{}
	closeWhileRunning atomic.Bool
}

func (p *unwindCloseProbe) Shutdown(context.Context) error {
	select {
	case <-p.finished:
	default:
		p.closeWhileRunning.Store(true)
	}
	close(p.closed)
	return nil
}

// test_delegate_timeout_cleanup.py test_timeout_does_not_close_child_while_worker_is_unwinding
func TestTimeoutDoesNotCloseChildWhileWorkerIsUnwinding(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	livenessKnobs(t, capSeconds(0.5))
	child := interruptibleChild()
	child.env, child.delegateRole, child.delegateDepth, child.model = env, "leaf", 1, "test-model"
	child.sessionCostStatus = "unknown"
	child.iterationBudget.Store(NewIterationBudget(50))
	child.apiCallCount.Store(1)
	unwinding, allowFinish, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	probe := &unwindCloseProbe{finished: finished, closed: make(chan struct{})}
	child.memoryManager = memory.NewMemoryManager(context.Background(), nil, nil)
	if err := child.memoryManager.AddProvider(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	var allowOnce sync.Once
	t.Cleanup(func() { allowOnce.Do(func() { close(allowFinish) }) })
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		if !child.hardInterruptRequested.Wait(30) {
			t.Error("the timeout never interrupted the child")
		}
		close(unwinding)
		select {
		case <-allowFinish:
		case <-time.After(30 * time.Second):
			t.Error("the test never allowed the worker to finish")
		}
		close(finished)
		return map[string]any{"final_response": "", "completed": false, "interrupted": true, "api_calls": 1, "messages": []any{}}, nil
	})
	parent := &AIAgent{sessionID: "parent-timeout-test", env: env, activeChildren: []*AIAgent{child}, activeChildrenLock: &sync.Mutex{}}
	result := runSingleChild(context.Background(), 0, "exercise timeout teardown", child, parent, "", nil, nil)
	if entryText(result, "status") != "timeout" {
		t.Fatalf("result %v", result)
	}
	waitSignal(t, unwinding, "worker unwind")
	select {
	case <-probe.closed:
		t.Fatal("timed-out child.close() ran before its conversation thread unwound")
	default:
	}
	allowOnce.Do(func() { close(allowFinish) })
	waitSignal(t, finished, "worker finish")
	waitSignal(t, probe.closed, "child close")
	if probe.closeWhileRunning.Load() {
		t.Fatal("timed-out child.close() raced its still-running conversation thread")
	}
}

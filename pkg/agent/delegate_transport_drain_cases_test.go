package agent

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child of test_timeout_transport_drain.py _SslBlockedChild, its worker parked until the drain settles the read
type sslBlockedChild struct {
	agent             *AIAgent
	readSettled       chan struct{}
	settleOnce        sync.Once
	unwound           chan struct{}
	closed            chan struct{}
	closeWhileBlocked atomic.Bool
	mu                sync.Mutex
	drainCalls        []string
}

func (c *sslBlockedChild) settle() { c.settleOnce.Do(func() { close(c.readSettled) }) }

func (c *sslBlockedChild) drains() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.drainCalls...)
}

// Memory provider whose shutdown is the child's close signal
type closeSignal struct {
	delegationRecorder
	child *sslBlockedChild
}

func (s *closeSignal) Shutdown(context.Context) error {
	select {
	case <-s.child.unwound:
	default:
		s.child.closeWhileBlocked.Store(true)
	}
	close(s.child.closed)
	return nil
}

func newSSLBlockedChild(t *testing.T, env *tools.Env) *sslBlockedChild {
	t.Helper()
	c := &sslBlockedChild{readSettled: make(chan struct{}), unwound: make(chan struct{}), closed: make(chan struct{})}
	child := interruptibleChild()
	child.env, child.delegateRole, child.delegateDepth, child.model, child.apiCallCount = env, "leaf", 1, "test-model", 1
	child.sessionCostStatus = "unknown"
	child.iterationBudget = NewIterationBudget(50)
	child.memoryManager = memory.NewMemoryManager(context.Background(), nil, nil)
	if err := child.memoryManager.AddProvider(context.Background(), &closeSignal{child: c}); err != nil {
		t.Fatal(err)
	}
	c.agent = child
	prev := conversationLoopRun
	exited := make(chan struct{})
	var exitOnce sync.Once
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a != child {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		defer exitOnce.Do(func() { close(exited) })
		select {
		case <-c.readSettled:
		case <-time.After(10 * time.Second):
			t.Error("drain never settled the read")
		}
		time.Sleep(50 * time.Millisecond)
		close(c.unwound)
		return map[string]any{"final_response": "", "completed": false, "interrupted": true, "api_calls": 1, "messages": []any{}}, nil
	}
	t.Cleanup(func() {
		c.settle()
		awaitWorkerExit(t, exited)
		conversationLoopRun = prev
	})
	return c
}

// Installs the drain hook of the test, recording every reason before running fn
func installChildDrain(t *testing.T, c *sslBlockedChild, fn func(reason string) int) {
	t.Helper()
	prev := drainChildTransportsFn
	drainChildTransportsFn = func(child *AIAgent, reason string) int {
		if child != c.agent {
			return prev(child, reason)
		}
		c.mu.Lock()
		c.drainCalls = append(c.drainCalls, reason)
		c.mu.Unlock()
		return fn(reason)
	}
	t.Cleanup(func() { drainChildTransportsFn = prev })
}

// The _run helper, a 0.4s child timeout without worktree isolation
func runSSLBlockedChild(t *testing.T, env *tools.Env, c *sslBlockedChild) string {
	t.Helper()
	livenessKnobs(t, capSeconds(0.4))
	parent := &AIAgent{sessionID: "parent-94248-drain", env: env, activeChildren: []*AIAgent{c.agent}, activeChildrenLock: &sync.Mutex{}}
	result := runSingleChild(tools.WithToolEnv(context.Background(), env), 0, "exercise timeout transport drain", c.agent, parent, "", nil, nil)
	return entryText(result, "status")
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

func TestTimeoutDrainsTransportsSoBlockedWorkerCanUnwind(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	c := newSSLBlockedChild(t, env)
	installChildDrain(t, c, func(string) int { c.settle(); return 1 })
	if status := runSSLBlockedChild(t, env, c); status != "timeout" {
		t.Fatalf("status %q", status)
	}
	drained := false
	for _, r := range c.drains() {
		drained = drained || strings.HasPrefix(r, "delegate_timeout")
	}
	if !drained {
		t.Fatalf("timeout path never drained the abandoned child's transports %v", c.drains())
	}
	waitSignal(t, c.unwound, "worker unwind")
	waitSignal(t, c.closed, "child close")
	if c.closeWhileBlocked.Load() {
		t.Fatal("child.close() ran while the worker was still inside its blocked read")
	}
}

func TestTimeoutDrainFailureDoesNotBreakTimeoutResult(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	c := newSSLBlockedChild(t, env)
	installChildDrain(t, c, func(string) int { panic("transport sweep exploded") })
	if status := runSSLBlockedChild(t, env, c); status != "timeout" {
		t.Fatalf("status %q", status)
	}
	if len(c.drains()) == 0 {
		t.Fatal("drain hook was never attempted")
	}
	c.settle()
	waitSignal(t, c.unwound, "worker unwind")
	waitSignal(t, c.closed, "child close")
}

func TestTimeoutWithoutDrainHookStillDefersClose(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	c := newSSLBlockedChild(t, env)
	installChildDrain(t, c, func(string) int { return 0 })
	if status := runSSLBlockedChild(t, env, c); status != "timeout" {
		t.Fatalf("status %q", status)
	}
	select {
	case <-c.closed:
		t.Fatal("close must stay deferred while the worker future is running")
	default:
	}
	c.settle()
	waitSignal(t, c.unwound, "worker unwind")
	waitSignal(t, c.closed, "child close")
	if c.closeWhileBlocked.Load() {
		t.Fatal("child closed before its worker unwound")
	}
}

// Socket double recording shutdown and close calls
type drainFakeSocket struct {
	net.Conn
	mu        sync.Mutex
	shutdowns int
	closed    bool
}

func (s *drainFakeSocket) CloseRead() error  { s.mu.Lock(); s.shutdowns++; s.mu.Unlock(); return nil }
func (s *drainFakeSocket) CloseWrite() error { s.mu.Lock(); s.shutdowns++; s.mu.Unlock(); return nil }
func (s *drainFakeSocket) Close() error      { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }

func TestAgentDrainShutsSocketsDownWithoutFdRelease(t *testing.T) {
	sock := &drainFakeSocket{}
	client := &http.Client{Transport: provider.TrackPoolConns(&http.Transport{}, sock)}
	agent := interruptibleChild()
	agent.client = client
	drained := agent.drainTransportsAfterAbandonment("delegate_timeout_test")
	sock.mu.Lock()
	defer sock.mu.Unlock()
	if drained != 1 || sock.shutdowns != 2 {
		t.Fatalf("drained %d shutdown calls %d", drained, sock.shutdowns)
	}
	if sock.closed {
		t.Fatal("drain must never release socket FDs")
	}
	if agent.client != client {
		t.Fatal("drain must never close or replace the client")
	}
}

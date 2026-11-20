package acp

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type scriptedGateway struct {
	*fakeGateway
	mu  sync.Mutex
	rpc func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error)
}

func (s *scriptedGateway) set(rpc func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpc = rpc
}

func (s *scriptedGateway) reset() {
	s.fakeGateway.mu.Lock()
	defer s.fakeGateway.mu.Unlock()
	s.fakeGateway.calls = nil
}

func (s *scriptedGateway) log() []rpcCall {
	s.fakeGateway.mu.Lock()
	defer s.fakeGateway.mu.Unlock()
	return append([]rpcCall(nil), s.fakeGateway.calls...)
}

func (s *scriptedGateway) methods() []string {
	var out []string
	for _, call := range s.log() {
		out = append(out, call.method)
	}
	return out
}

func fixedReply(t *testing.T, text string) func(context.Context, string, map[string]any) (*pyjson.Object, error) {
	return func(context.Context, string, map[string]any) (*pyjson.Object, error) { return jsonObj(t, text), nil }
}

func agentWithQueuedPrompt(t *testing.T) (*GatewayACPAgent, *scriptedGateway) {
	t.Helper()
	a := newGatewayAgent(t)
	link := &scriptedGateway{}
	link.fakeGateway = newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		link.mu.Lock()
		rpc := link.rpc
		link.mu.Unlock()
		return rpc(ctx, method, params)
	})
	link.set(fixedReply(t, `{}`))
	a.loop.enter()
	a.snapshots.Set("s", jsonObj(t, `{"execution_generation": 7}`))
	a.gateway, a.conn = link, &editorFake{}
	a.loop.leave()
	return a, link
}

func startPrompt(a *GatewayACPAgent, ctx context.Context, text string) *asked {
	q := &asked{done: make(chan struct{})}
	go func() {
		defer close(q.done)
		q.resp, q.err = a.Prompt(ctx, textPrompt("s", text))
	}()
	return q
}

func queuedPrompt(t *testing.T, a *GatewayACPAgent, link *scriptedGateway) *asked {
	t.Helper()
	link.set(fixedReply(t, `{"admission_id": "ours"}`))
	task := startPrompt(a, context.Background(), "hello")
	until(t, "the admission", func() bool { return onLoop(a, func() string { return a.admissions["s"] }) == "ours" })
	time.Sleep(20 * time.Millisecond)
	if task.finished() {
		t.Fatal("the prompt finished before its turn settled")
	}
	link.reset()
	return task
}

func settlePrompt(t *testing.T, a *GatewayACPAgent, task *asked, outcome string) *pyacp.PromptResponse {
	t.Helper()
	event := pyjson.ObjectOf("session_id", "s", "type", "message.complete", "admission_id", "ours", "payload", pyjson.ObjectOf("outcome", outcome, "text", ""))
	if err := onLoop(a, func() error { return a.project(context.Background(), event) }); err != nil {
		t.Fatal(err)
	}
	resp, err := task.wait(t, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func cancelSession(t *testing.T, a *GatewayACPAgent) {
	t.Helper()
	if err := a.Cancel(context.Background(), &pyacp.CancelNotification{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelTargetsQueuedAcpAdmissionNotTheRunningGeneration(t *testing.T) {
	a, link := agentWithQueuedPrompt(t)
	task := queuedPrompt(t, a, link)
	link.set(fixedReply(t, `{"admission_id": "ours", "status": "terminal", "outcome": "cancelled"}`))
	cancelSession(t, a)
	calls := link.log()
	if len(calls) != 1 || calls[0].method != "prompt.cancel" || !reflect.DeepEqual(calls[0].params, map[string]any{"session_id": "s", "admission_id": "ours"}) {
		t.Fatalf("calls = %v", calls)
	}
	if resp := settlePrompt(t, a, task, "cancelled"); resp.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q", resp.StopReason)
	}
}

func TestCancelInterruptsOnlyWhenOurAdmissionIsTheOneRunning(t *testing.T) {
	a, link := agentWithQueuedPrompt(t)
	task := queuedPrompt(t, a, link)
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "prompt.cancel":
			return nil, &gateway.GatewayClientError{Msg: "stale_generation"}
		case "prompt.receipt":
			return jsonObj(t, `{"admission_id": "ours", "status": "started", "execution_generation": 8}`), nil
		}
		return pyjson.NewObject(), nil
	})
	cancelSession(t, a)
	var interrupt map[string]any
	for _, call := range link.log() {
		if call.method == "session.interrupt" {
			interrupt = call.params
		}
	}
	if !reflect.DeepEqual(interrupt, map[string]any{"session_id": "s", "execution_generation": int64(8)}) {
		t.Fatalf("interrupt = %v", interrupt)
	}
	if resp := settlePrompt(t, a, task, "cancelled"); resp.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q", resp.StopReason)
	}
}

func hasCancelOf(link *scriptedGateway, admission string) bool {
	for _, call := range link.log() {
		if call.method == "prompt.cancel" && call.params["admission_id"] == admission {
			return true
		}
	}
	return false
}

func TestCancelDuringSubmitIsAppliedWhenAdmissionIdArrives(t *testing.T) {
	a, link := agentWithQueuedPrompt(t)
	submitStarted, releaseSubmit := make(chan struct{}), make(chan struct{})
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "prompt.submit":
			close(submitStarted)
			<-releaseSubmit
			return jsonObj(t, `{"admission_id": "ours"}`), nil
		case "prompt.cancel":
			return jsonObj(t, `{"admission_id": "ours", "status": "terminal", "outcome": "cancelled"}`), nil
		}
		return pyjson.NewObject(), nil
	})
	task := startPrompt(a, context.Background(), "hello")
	select {
	case <-submitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("submit did not start")
	}
	cancelSession(t, a)
	if methods := link.methods(); !reflect.DeepEqual(methods, []string{"prompt.submit"}) {
		t.Fatalf("methods = %v", methods)
	}
	close(releaseSubmit)
	until(t, "the deferred cancel", func() bool { return hasCancelOf(link, "ours") })
	for _, call := range link.log() {
		if call.method == "prompt.cancel" && !reflect.DeepEqual(call.params, map[string]any{"session_id": "s", "admission_id": "ours"}) {
			t.Fatalf("cancel params = %v", call.params)
		}
	}
	if resp := settlePrompt(t, a, task, "cancelled"); resp.StopReason != "cancelled" {
		t.Fatalf("stop reason = %q", resp.StopReason)
	}
}

func TestCancelWithoutAnAcpPromptLeavesOtherSurfacesTurnAlone(t *testing.T) {
	a, link := agentWithQueuedPrompt(t)
	cancelSession(t, a)
	if calls := link.log(); len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDeferredCancelFailureCleansMappingBeforeNextSubmit(t *testing.T) {
	a, link := agentWithQueuedPrompt(t)
	firstStarted, firstRelease := make(chan struct{}), make(chan struct{})
	secondStarted, secondRelease := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	submits := 0
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "prompt.submit":
			mu.Lock()
			submits++
			n := submits
			mu.Unlock()
			if n == 1 {
				close(firstStarted)
				<-firstRelease
				return jsonObj(t, `{"admission_id": "first"}`), nil
			}
			close(secondStarted)
			<-secondRelease
			return jsonObj(t, `{"admission_id": "second"}`), nil
		case "prompt.cancel":
			if params["admission_id"] == "first" {
				return nil, &gateway.GatewayClientError{Msg: "cancel_failed"}
			}
			return jsonObj(t, `{"admission_id": "second", "status": "terminal", "outcome": "cancelled"}`), nil
		}
		return pyjson.NewObject(), nil
	})
	first := startPrompt(a, context.Background(), "first")
	<-firstStarted
	cancelSession(t, a)
	close(firstRelease)
	if _, err := first.wait(t, 2*time.Second); !refusedWith(err, "cancel_failed") {
		t.Fatalf("err = %v", err)
	}
	if _, held := onLoop(a, func() map[string]string { return a.admissions })["s"]; held {
		t.Fatal("the failed admission stayed mapped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	second := startPrompt(a, ctx, "second")
	<-secondStarted
	cancelSession(t, a)
	close(secondRelease)
	until(t, "the second cancel", func() bool { return hasCancelOf(link, "second") })
	var cancelled []any
	for _, call := range link.log() {
		if call.method == "prompt.cancel" {
			cancelled = append(cancelled, call.params["admission_id"])
		}
	}
	if !reflect.DeepEqual(cancelled, []any{"first", "second"}) {
		t.Fatalf("cancelled = %v", cancelled)
	}
	cancel()
	if _, err := second.wait(t, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, held := onLoop(a, func() map[string]string { return a.admissions })["s"]; held {
		t.Fatal("the cancelled admission stayed mapped")
	}
}

func TestInterruptCompletionRaceRechecksExactAdmission(t *testing.T) {
	a, link := agentWithQueuedPrompt(t)
	receipts := []string{
		`{"admission_id": "ours", "status": "started", "execution_generation": 8}`,
		`{"admission_id": "ours", "status": "terminal", "outcome": "completed", "execution_generation": 8}`,
	}
	var mu sync.Mutex
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "prompt.cancel", "session.interrupt":
			return nil, &gateway.GatewayClientError{Msg: "stale_generation"}
		case "prompt.receipt":
			mu.Lock()
			defer mu.Unlock()
			next := receipts[0]
			receipts = receipts[1:]
			return jsonObj(t, next), nil
		}
		return pyjson.NewObject(), nil
	})
	if err := onLoop(a, func() error { return a.cancelAdmission(context.Background(), "s", "ours") }); err != nil {
		t.Fatal(err)
	}
	want := []rpcCall{
		{"prompt.cancel", map[string]any{"session_id": "s", "admission_id": "ours"}},
		{"prompt.receipt", map[string]any{"session_id": "s", "admission_id": "ours"}},
		{"session.interrupt", map[string]any{"session_id": "s", "execution_generation": int64(8)}},
		{"prompt.receipt", map[string]any{"session_id": "s", "admission_id": "ours"}},
	}
	if calls := link.log(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
}

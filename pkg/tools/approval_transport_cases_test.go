package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Request of the hermes _request fixture
func atFixtureRequest(t *testing.T, command, description, patternKey string, allowSession, allowPermanent bool) ApprovalRequest {
	t.Helper()
	req, err := newApprovalRequest(command, description, patternKey, []string{patternKey}, "session-a", "cli", allowSession, allowPermanent, 1)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// Transport answering every request with one choice
func atRespond(choice string) ApprovalPresentFunc {
	return func(_ context.Context, req ApprovalRequest) (ApprovalTransportDecision, error) {
		return req.Respond(choice), nil
	}
}

func TestHostAcceptsBoundSyncAndAsyncDecisions(t *testing.T) {
	req := atFixtureRequest(t, "rm -rf /tmp/example", "recursive delete", "rm_recursive", true, true)
	syncResult := invokeApprovalTransport(context.Background(), atRespond("session"), req, time.Second, transportWait{})
	async := func(_ context.Context, received ApprovalRequest) (ApprovalTransportDecision, error) {
		done := make(chan ApprovalTransportDecision, 1)
		go func() { done <- received.Respond("once") }()
		return <-done, nil
	}
	asyncResult := invokeApprovalTransport(context.Background(), async, req, time.Second, transportWait{})
	if syncResult.choice != "session" || syncResult.failure != "" {
		t.Fatalf("sync %+v", syncResult)
	}
	if asyncResult.choice != "once" || asyncResult.failure != "" {
		t.Fatalf("async %+v", asyncResult)
	}
}

func TestHostRejectsScopeNotOfferedByRequest(t *testing.T) {
	req := atFixtureRequest(t, "dangerous", "dangerous", "danger", false, false)
	result := invokeApprovalTransport(context.Background(), atRespond("always"), req, time.Second, transportWait{})
	if result.choice != "deny" || result.failure != "invalid" {
		t.Fatalf("%+v", result)
	}
}

func TestHostRejectsInvalidOrStaleDecisions(t *testing.T) {
	cases := []struct {
		name    string
		present func(ApprovalRequest) ApprovalTransportDecision
		failure string
	}{
		{"uncorrelated mapping", func(ApprovalRequest) ApprovalTransportDecision { return ApprovalTransportDecision{Choice: "once"} }, "invalid"},
		{"disallowed choice", func(r ApprovalRequest) ApprovalTransportDecision { return r.Respond("bogus") }, "invalid"},
		{"stale request id", func(r ApprovalRequest) ApprovalTransportDecision {
			return ApprovalTransportDecision{RequestID: "stale", RequestDigest: r.Digest, Choice: "once"}
		}, "stale"},
		{"changed digest", func(r ApprovalRequest) ApprovalTransportDecision {
			return ApprovalTransportDecision{RequestID: r.RequestID, RequestDigest: "changed", Choice: "once"}
		}, "stale"},
	}
	for _, tc := range cases {
		req := atFixtureRequest(t, "rm -rf /tmp/example", "recursive delete", "rm_recursive", true, true)
		present := func(_ context.Context, r ApprovalRequest) (ApprovalTransportDecision, error) {
			return tc.present(r), nil
		}
		result := invokeApprovalTransport(context.Background(), present, req, time.Second, transportWait{})
		if result.choice != "deny" || result.failure != tc.failure {
			t.Fatalf("%s: %+v", tc.name, result)
		}
	}
}

func TestHostTimeoutAndExceptionDenyWithoutWaitingForever(t *testing.T) {
	hangs := func(context.Context, ApprovalRequest) (ApprovalTransportDecision, error) {
		time.Sleep(time.Second)
		return ApprovalTransportDecision{}, nil
	}
	crashes := func(context.Context, ApprovalRequest) (ApprovalTransportDecision, error) {
		return ApprovalTransportDecision{}, errors.New("transport offline")
	}
	req := atFixtureRequest(t, "rm -rf /tmp/example", "recursive delete", "rm_recursive", true, true)
	started := time.Now()
	timeout := invokeApprovalTransport(context.Background(), hangs, req, 20*time.Millisecond, transportWait{})
	exception := invokeApprovalTransport(context.Background(), crashes, req, time.Second, transportWait{})
	if time.Since(started) >= 500*time.Millisecond {
		t.Fatalf("host waited %s", time.Since(started))
	}
	if timeout.choice != "deny" || timeout.failure != "timeout" {
		t.Fatalf("timeout %+v", timeout)
	}
	if exception.choice != "deny" || exception.failure != "error" {
		t.Fatalf("exception %+v", exception)
	}
}

func TestHostTransportWaitIsInterruptibleAndPollable(t *testing.T) {
	var polls atomic.Int32
	hangs := func(context.Context, ApprovalRequest) (ApprovalTransportDecision, error) {
		time.Sleep(time.Second)
		return ApprovalTransportDecision{}, nil
	}
	req := atFixtureRequest(t, "rm -rf /tmp/example", "recursive delete", "rm_recursive", true, true)
	result := invokeApprovalTransport(context.Background(), hangs, req, time.Second, transportWait{
		pollInterval:  10 * time.Millisecond,
		onPoll:        func() { polls.Add(1) },
		isInterrupted: func() bool { return polls.Load() >= 1 },
	})
	if result.choice != "deny" || result.failure != "interrupted" {
		t.Fatalf("%+v", result)
	}
	if polls.Load() == 0 {
		t.Fatal("no poll")
	}
}

func TestHostRejectsDecisionCompletedAfterDeadline(t *testing.T) {
	base := time.Now()
	var late atomic.Bool
	prev := swapTransportClock(func() time.Time {
		if late.Load() {
			return base.Add(2 * time.Second)
		}
		return base
	})
	t.Cleanup(func() { swapTransportClock(prev) })
	present := func(_ context.Context, r ApprovalRequest) (ApprovalTransportDecision, error) {
		late.Store(true)
		return r.Respond("once"), nil
	}
	req := atFixtureRequest(t, "rm -rf /tmp/example", "recursive delete", "rm_recursive", true, true)
	result := invokeApprovalTransport(context.Background(), present, req, time.Second, transportWait{})
	if result.choice != "deny" || result.failure != "timeout" {
		t.Fatalf("%+v", result)
	}
}

func TestHostCapsHungTransportWorkers(t *testing.T) {
	release := make(chan struct{})
	hangs := func(context.Context, ApprovalRequest) (ApprovalTransportDecision, error) {
		<-release
		return ApprovalTransportDecision{}, nil
	}
	req := atFixtureRequest(t, "rm -rf /tmp/example", "recursive delete", "rm_recursive", true, true)
	var results []approvalTransportResult
	for i := 0; i < maxActiveTransportWorkers+1; i++ {
		results = append(results, invokeApprovalTransport(context.Background(), hangs, req, time.Millisecond, transportWait{}))
	}
	close(release)
	last := results[len(results)-1]
	if last.choice != "deny" || last.failure != "busy" {
		t.Fatalf("%+v", last)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(transportWorkerSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Installs a transport resolver for the test and restores the previous one afterwards
func atInstallResolver(t *testing.T, fn func(env *Env, name string) (*RegisteredApprovalTransport, error)) {
	t.Helper()
	approvalTransportResolver.RLock()
	previous := approvalTransportResolver.fn
	approvalTransportResolver.RUnlock()
	RegisterApprovalTransportResolver(fn)
	t.Cleanup(func() { RegisterApprovalTransportResolver(previous) })
}

func TestTransportResolutionErrorDoesNotLogPluginException(t *testing.T) {
	logs := glCaptureLog(t)
	atInstallResolver(t, func(*Env, string) (*RegisteredApprovalTransport, error) {
		return nil, errors.New("plugin-owned-secret-value")
	})
	env := &Env{Config: config.Tree{"security": map[string]any{"approval": map[string]any{"transport": "phone"}}}}
	attempt := presentWithSelectedTransport(context.Background(), env, transportRequest{
		command: "rm -rf /tmp/example", description: "dangerous", patternKey: "rm_recursive", patternKeys: []string{"rm_recursive"},
		sessionKey: "session-a", surface: "cli", allowSession: true, allowPermanent: true})
	if attempt.choice != "deny" || attempt.failure != "unavailable" {
		t.Fatalf("%+v", attempt)
	}
	if strings.Contains(logs.String(), "plugin-owned-secret-value") {
		t.Fatalf("plugin exception text logged: %s", logs.String())
	}
}

func TestRedactionFailureDeniesBeforeTransportCallback(t *testing.T) {
	var mu sync.Mutex
	var calls []ApprovalRequest
	atInstallResolver(t, func(_ *Env, name string) (*RegisteredApprovalTransport, error) {
		return &RegisteredApprovalTransport{Name: name, PluginID: "fixture-approval", Present: func(_ context.Context, req ApprovalRequest) (ApprovalTransportDecision, error) {
			mu.Lock()
			calls = append(calls, req)
			mu.Unlock()
			return req.Respond("once"), nil
		}}, nil
	})
	prev := approvalRequestRedactor
	approvalRequestRedactor = func(env *Env, text string) (string, error) {
		if text == "rm -rf /tmp/example" || text == "dangerous" {
			return "", errors.New("redactor unavailable")
		}
		return prev(env, text)
	}
	t.Cleanup(func() { approvalRequestRedactor = prev })
	env := approvalTestEnv(t, "redaction-failure-session", nil, map[string]any{"interactive": true})
	env.Config["security"] = map[string]any{"approval": map[string]any{"transport": "phone"}}
	env.Ask = forbiddenAsker{t}
	result := CheckAllCommandGuards(context.Background(), env, "rm -rf /tmp/example", "local", false)
	if result.Approved || result.Outcome != "transport_error" {
		t.Fatalf("%+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("transport called %d times", len(calls))
	}
}

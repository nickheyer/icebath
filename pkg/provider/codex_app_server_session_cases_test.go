package provider

import (
	"strings"
	"sync"
	"testing"
)

const (
	fakeThreadID = "thread-fake-001"
	fakeTurnID   = "turn-fake-001"
)

func fakeTurnCompleted(f *fakeCodexClient) {
	f.note("turn/completed", map[string]any{"threadId": fakeThreadID, "turn": map[string]any{"id": fakeTurnID, "status": "completed", "error": nil}})
}

func fakeToolCompleted(f *fakeCodexClient) {
	f.note("item/completed", map[string]any{"threadId": fakeThreadID, "turnId": fakeTurnID, "item": map[string]any{
		"type": "commandExecution", "id": "ex1", "command": "echo hi", "cwd": "/tmp", "status": "completed",
		"aggregatedOutput": "hi", "exitCode": 0, "commandActions": []any{}}})
}

func TestCodexOnEventFiresDuringApprovalDrain(t *testing.T) {
	f := &fakeCodexClient{}
	f.note("item/started", map[string]any{"item": map[string]any{"type": "commandExecution", "id": "exec-1", "command": "echo drained", "cwd": "/tmp"}})
	f.serverRequests = []any{map[string]any{"id": "req-d", "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "echo drained", "cwd": "/tmp"}}}
	fakeTurnCompleted(f)
	var mu sync.Mutex
	var events []any
	s := newFakeSession(f, CodexAppServerSessionOptions{
		ApprovalCallback: func(string, string, bool) (string, error) { return "once", nil },
		OnEvent:          func(note any) { mu.Lock(); events = append(events, note); mu.Unlock() },
	})
	s.RunTurn("hi", CodexTurnOptions{TurnTimeout: 0.2})
	seen := false
	for _, e := range events {
		seen = seen || cxGet(e, "method") == "item/started"
	}
	if !seen {
		t.Fatalf("item/started drained alongside the approval was not forwarded to on_event: %v", events)
	}
}

func TestCodexPostToolSilenceWarnsButDoesNotRetireAHealthyTurn(t *testing.T) {
	f := &fakeCodexClient{}
	fakeToolCompleted(f)
	fakeTurnCompleted(f)
	s := newFakeSession(f, CodexAppServerSessionOptions{})
	s.EnsureStarted()
	logs := captureLogs(t)
	values := []float64{1000.0, 999.0, 999.0, 999.0}
	prev := codexMonotonic
	codexMonotonic = func() float64 {
		if len(values) == 0 {
			return 1000.2
		}
		v := values[0]
		values = values[1:]
		return v
	}
	t.Cleanup(func() { codexMonotonic = prev })
	r, err := s.RunTurn("tool then silence", CodexTurnOptions{TurnTimeout: 5, NotificationPollTimeout: 0, PostToolQuietTimeout: 0.15})
	codexMonotonic = prev
	if err != nil || r.Interrupted || r.ShouldRetire || r.Error != "" || r.ToolIterations != 1 {
		t.Fatalf("result %+v %v", r, err)
	}
	for _, m := range f.methods() {
		if m == "turn/interrupt" {
			t.Fatalf("interrupted %v", f.methods())
		}
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	warned := false
	for _, line := range logs.lines {
		warned = warned || strings.Contains(line, "no events for")
	}
	if !warned {
		t.Fatalf("no quiet warning in %v", logs.lines)
	}
}

func TestCodexPostToolActivityClearsTheQuietTimerAndNeverRetires(t *testing.T) {
	f := &fakeCodexClient{}
	fakeToolCompleted(f)
	f.note("item/completed", map[string]any{"threadId": fakeThreadID, "turnId": fakeTurnID, "item": map[string]any{"type": "agentMessage", "id": "m1", "text": "tool finished"}})
	fakeTurnCompleted(f)
	r, _ := newFakeSession(f, CodexAppServerSessionOptions{}).RunTurn("tool then talk", CodexTurnOptions{TurnTimeout: 2, NotificationPollTimeout: 0.01, PostToolQuietTimeout: 0.05})
	if r.ToolIterations != 1 || r.FinalText != "tool finished" || r.ShouldRetire || r.Interrupted {
		t.Fatalf("result %+v", r)
	}
}

func transportLossHandler(failOn *string) func(string, any) (any, error) {
	return func(method string, params any) (any, error) {
		if method == *failOn {
			return nil, codexTransportLost("codex app-server stdin closed unexpectedly: Broken pipe")
		}
		switch method {
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": fakeThreadID}}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": fakeTurnID}}, nil
		case "turn/steer":
			return map[string]any{"turnId": cxGet(params, "expectedTurnId")}, nil
		}
		return map[string]any{}, nil
	}
}

func TestCodexWriteFailureReturnsRetiringResultAndControlPathsStayNonFatal(t *testing.T) {
	for _, tc := range []struct{ run, failOn string }{{"run_turn", "turn/start"}, {"compact_thread", "thread/compact/start"}, {"run_turn", "respond"}} {
		failOn := tc.failOn
		f := &fakeCodexClient{handler: transportLossHandler(&failOn)}
		if failOn == "respond" {
			f.respondErr = codexTransportLost("codex app-server stdin closed unexpectedly: Broken pipe")
			f.serverRequests = []any{map[string]any{"id": "req-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "ls"}}}
		}
		s := newFakeSession(f, CodexAppServerSessionOptions{ApprovalCallback: func(string, string, bool) (string, error) { return "once", nil }})
		var r *CodexTurnResult
		var err error
		if tc.run == "run_turn" {
			r, err = s.RunTurn("hi", CodexTurnOptions{TurnTimeout: 2, NotificationPollTimeout: 0.001})
		} else {
			r, err = s.CompactThread(2, 0.001)
		}
		if err != nil || !r.ShouldRetire || !strings.Contains(r.Error, "stdin closed unexpectedly") {
			t.Fatalf("%s %s: %+v %v", tc.run, failOn, r, err)
		}
		controlFail := "turn/steer"
		control := &fakeCodexClient{handler: transportLossHandler(&controlFail)}
		steer := newFakeSession(control, CodexAppServerSessionOptions{})
		steer.EnsureStarted()
		steer.activeTurnMu.Lock()
		steer.activeTurnID = fakeTurnID
		steer.activeTurnMu.Unlock()
		if ok, err := steer.RequestSteer("more"); ok || err != nil {
			t.Fatalf("steer over a lost transport %v %v", ok, err)
		}
		controlFail = "turn/interrupt"
		steer.issueInterrupt(fakeTurnID)
	}
}

func TestCodexPlugin401StderrKeepsPrimaryRPCErrorVisible(t *testing.T) {
	for _, op := range []string{"run_turn", "compact_thread"} {
		f := &fakeCodexClient{stderr: []string{"WARN ChatGPT plugin prewarm failed: HTTP 401 Unauthorized"}}
		f.handler = func(method string, params any) (any, error) {
			if method == "turn/start" || method == "thread/compact/start" {
				return nil, &CodexAppServerError{Code: -32603, Message: "internal error: workspace initialization failed"}
			}
			return map[string]any{"thread": map[string]any{"id": "t"}, "activePermissionProfile": map[string]any{"id": "x"}}, nil
		}
		s := newFakeSession(f, CodexAppServerSessionOptions{})
		var r *CodexTurnResult
		if op == "run_turn" {
			r, _ = s.RunTurn("hi", CodexTurnOptions{TurnTimeout: 2})
		} else {
			r, _ = s.CompactThread(2, 0)
		}
		if r.Error == "" || !strings.Contains(r.Error, "workspace initialization failed") || !strings.Contains(r.Error, "plugin prewarm failed") ||
			strings.Contains(r.Error, "codex login") || r.ShouldRetire {
			t.Fatalf("%s: %+v", op, r)
		}
	}
}

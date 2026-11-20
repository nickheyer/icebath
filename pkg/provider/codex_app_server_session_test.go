package provider

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type fakeCodexRequest struct {
	method string
	params any
}

type fakeCodexClient struct {
	mu             sync.Mutex
	requests       []fakeCodexRequest
	responses      [][2]any
	errorResponses []string
	notifications  []any
	serverRequests []any
	stderr         []string
	closed         bool
	handler        func(method string, params any) (any, error)
	onRespond      func()
	respondErr     error
}

func (f *fakeCodexClient) Initialize(string, string, string, any, time.Duration) (any, error) {
	return map[string]any{"userAgent": "fake/0.0.0"}, nil
}

func (f *fakeCodexClient) Request(method string, params any, _ time.Duration) (any, error) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeCodexRequest{method, pyjson.Plain(params)})
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(method, params)
	}
	switch method {
	case "thread/start":
		return map[string]any{"thread": map[string]any{"id": "thread-fake-001"}}, nil
	case "turn/start":
		return map[string]any{"turn": map[string]any{"id": "turn-fake-001"}}, nil
	case "turn/steer":
		return map[string]any{"turnId": cxGet(params, "expectedTurnId")}, nil
	}
	return map[string]any{}, nil
}

func (f *fakeCodexClient) Respond(id any, result any) error {
	if f.respondErr != nil {
		return f.respondErr
	}
	f.responses = append(f.responses, [2]any{id, pyjson.Plain(result)})
	if f.onRespond != nil {
		f.onRespond()
	}
	return nil
}

func (f *fakeCodexClient) RespondError(id any, code int, message string, _ any) error {
	f.errorResponses = append(f.errorResponses, message)
	return nil
}

func (f *fakeCodexClient) TakeNotification(timeout time.Duration) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.notifications) > 0 {
		n := f.notifications[0]
		f.notifications = f.notifications[1:]
		return n
	}
	if timeout > 0 {
		time.Sleep(time.Millisecond)
	}
	return nil
}

func (f *fakeCodexClient) TakeServerRequest(time.Duration) any {
	if len(f.serverRequests) > 0 {
		r := f.serverRequests[0]
		f.serverRequests = f.serverRequests[1:]
		return r
	}
	return nil
}

func (f *fakeCodexClient) StderrTail(n int) []string {
	if len(f.stderr) > n {
		return f.stderr[len(f.stderr)-n:]
	}
	return f.stderr
}

func (f *fakeCodexClient) IsAlive() bool       { return !f.closed }
func (f *fakeCodexClient) Close(time.Duration) { f.closed = true }

func (f *fakeCodexClient) note(method string, params map[string]any) {
	f.mu.Lock()
	f.notifications = append(f.notifications, map[string]any{"method": method, "params": params})
	f.mu.Unlock()
}

func (f *fakeCodexClient) methods() []string {
	var out []string
	for _, r := range f.requests {
		out = append(out, r.method)
	}
	return out
}

func newFakeSession(f *fakeCodexClient, opts CodexAppServerSessionOptions) *CodexAppServerSession {
	opts.Cwd = "/tmp"
	opts.ClientFactory = func(string, string) (CodexRPCClient, error) { return f, nil }
	return NewCodexAppServerSession(opts)
}

func agentDone(f *fakeCodexClient, thread, turn, text string) {
	f.note("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "agentMessage", "id": "m-" + text, "text": text}})
	f.note("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": "completed", "error": nil}})
}

func TestCodexApprovalChoiceMappingAndTurnInput(t *testing.T) {
	for choice, want := range map[string]string{"once": "accept", "session": "acceptForSession", "always": "acceptForSession", "deny": "decline", "": "decline"} {
		if got := codexApprovalChoiceToDecision(choice); got != want {
			t.Fatalf("%q -> %q", choice, got)
		}
	}
	items, text := codexBuildTurnInput([]any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
		map[string]any{"type": "input_image", "path": "file:///tmp/a.png"},
	})
	want := []any{map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image", "url": "data:image/png;base64,AAA"}, map[string]any{"type": "localImage", "path": "/tmp/a.png"}}
	if text != "look" || !reflect.DeepEqual(items, want) {
		t.Fatalf("turn input %v %q", items, text)
	}
	items, text = codexBuildTurnInput([]any{map[string]any{"type": "image", "url": "https://x/y.png"}})
	if text != "What do you see in this image?" || len(items) != 2 {
		t.Fatalf("image only %v %q", items, text)
	}
	if items, text = codexBuildTurnInput("plain"); text != "plain" || !reflect.DeepEqual(items, []any{map[string]any{"type": "text", "text": "plain"}}) {
		t.Fatalf("plain %v", items)
	}
}

func TestCodexLifecycle(t *testing.T) {
	f := &fakeCodexClient{}
	s := newFakeSession(f, CodexAppServerSessionOptions{DeveloperInstructions: "SOUL", ModelProvider: " custom ", Model: "gpt-5.5"})
	a, _ := s.EnsureStarted()
	b, _ := s.EnsureStarted()
	if a != "thread-fake-001" || b != a || !reflect.DeepEqual(f.methods(), []string{"thread/start"}) {
		t.Fatalf("not idempotent %v", f.methods())
	}
	want := map[string]any{"cwd": "/tmp", "personality": "none", "developerInstructions": "SOUL", "modelProvider": "custom", "model": "gpt-5.5"}
	if !reflect.DeepEqual(f.requests[0].params, want) {
		t.Fatalf("thread/start params %v", f.requests[0].params)
	}
	f2 := &fakeCodexClient{}
	newFakeSession(f2, CodexAppServerSessionOptions{DeveloperInstructions: "   "}).EnsureStarted()
	if _, ok := f2.requests[0].params.(map[string]any)["developerInstructions"]; ok {
		t.Fatal("blank developer instructions sent")
	}
	f3 := &fakeCodexClient{handler: func(method string, params any) (any, error) {
		if method == "thread/resume" {
			return map[string]any{"thread": map[string]any{"id": cxGet(params, "threadId")}}, nil
		}
		return map[string]any{"thread": map[string]any{"id": "fresh-1"}}, nil
	}}
	s3 := newFakeSession(f3, CodexAppServerSessionOptions{ResumeThreadID: "stored-1", DeveloperInstructions: "SOUL", HistorySeed: "seed"})
	if id, _ := s3.EnsureStarted(); id != "stored-1" || !reflect.DeepEqual(f3.methods(), []string{"thread/resume"}) {
		t.Fatalf("resume %q %v", id, f3.methods())
	}
	if !reflect.DeepEqual(f3.requests[0].params, map[string]any{"threadId": "stored-1", "cwd": "/tmp", "personality": "none", "developerInstructions": "SOUL"}) {
		t.Fatalf("resume params %v", f3.requests[0].params)
	}
	f4 := &fakeCodexClient{handler: func(method string, params any) (any, error) {
		if method == "thread/resume" {
			return nil, &CodexAppServerError{Code: -32600, Message: "no rollout found for thread id gone-1"}
		}
		return map[string]any{"thread": map[string]any{"id": "fresh-2"}}, nil
	}}
	s4 := newFakeSession(f4, CodexAppServerSessionOptions{ResumeThreadID: "gone-1", HistorySeed: "HISTORY"})
	_, err := s4.EnsureStarted()
	var resumeErr *CodexThreadResumeError
	var appErr *CodexAppServerError
	if !errors.As(err, &resumeErr) || resumeErr.ThreadID != "gone-1" || !errors.As(err, &appErr) {
		t.Fatalf("resume failure %v", err)
	}
	if id, _ := s4.EnsureStarted(); id != "fresh-2" || !reflect.DeepEqual(f4.methods(), []string{"thread/resume", "thread/start"}) {
		t.Fatalf("fallback %q %v", id, f4.methods())
	}
	if f4.requests[1].params.(map[string]any)["developerInstructions"] != "HISTORY" {
		t.Fatalf("history seed %v", f4.requests[1].params)
	}
	s.Close()
	s.Close()
	if !f.closed || s.ThreadID() != "" {
		t.Fatal("close did not release the client")
	}
	f5 := &fakeCodexClient{handler: func(string, any) (any, error) { return map[string]any{"thread": map[string]any{}, "b": 1}, nil }}
	if _, err := newFakeSession(f5, CodexAppServerSessionOptions{}).EnsureStarted(); err == nil || err.Error() != "codex app-server error -32603: codex thread/start returned no thread id (payload keys: ['b', 'thread'])" {
		t.Fatalf("missing thread id %v", err)
	}
}

func TestCodexRunTurn(t *testing.T) {
	f := &fakeCodexClient{}
	s := newFakeSession(f, CodexAppServerSessionOptions{})
	agentDone(f, "thread-fake-001", "turn-fake-001", "hello back")
	tier := "priority"
	r, err := s.RunTurn("hi", CodexTurnOptions{Model: "gpt-5.5", ServiceTier: &tier})
	if err != nil || r.FinalText != "hello back" || r.Interrupted || r.Error != "" || r.TurnID != "turn-fake-001" || *r.SubmittedUserText != "hi" {
		t.Fatalf("turn %+v %v", r, err)
	}
	turnStart := f.requests[1].params.(map[string]any)
	if turnStart["model"] != "gpt-5.5" || turnStart["serviceTier"] != "priority" {
		t.Fatalf("turn/start %v", turnStart)
	}
	agentDone(f, "thread-fake-001", "turn-fake-001", "again")
	s.RunTurn("next", CodexTurnOptions{ServiceTier: &tier})
	if _, ok := f.requests[2].params.(map[string]any)["serviceTier"]; ok {
		t.Fatal("unchanged tier resent")
	}
	if _, ok := f.requests[2].params.(map[string]any)["model"]; ok {
		t.Fatal("model sent without a turn model")
	}
	agentDone(f, "thread-fake-001", "turn-fake-001", "cleared")
	s.RunTurn("third", CodexTurnOptions{})
	if v, ok := f.requests[3].params.(map[string]any)["serviceTier"]; !ok || v != nil {
		t.Fatal("cleared tier not sent as null")
	}
}

func TestCodexForeignCompletionInServerRequestDrainIsIgnored(t *testing.T) {
	f := &fakeCodexClient{}
	f.serverRequests = []any{map[string]any{"id": "approval-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "pwd", "cwd": "/tmp"}}}
	agentDone(f, "thread-child-001", "turn-child-001", "child drain summary")
	f.onRespond = func() { agentDone(f, "thread-fake-001", "turn-fake-001", "parent after approval") }
	r, _ := newFakeSession(f, CodexAppServerSessionOptions{RequestRouting: &CodexServerRequestRouting{AutoApproveExec: true}}).RunTurn("go", CodexTurnOptions{})
	if r.FinalText != "parent after approval" || len(r.ProjectedMessages) != 1 {
		t.Fatalf("drain leaked child %+v", r)
	}
	if !reflect.DeepEqual(f.responses, [][2]any{{"approval-1", map[string]any{"decision": "accept"}}}) {
		t.Fatalf("responses %v", f.responses)
	}
}

func TestCodexTurnStartFailureAttachesRedactedStderrTail(t *testing.T) {
	f := &fakeCodexClient{stderr: []string{"ERROR boom", "OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"}}
	f.handler = func(method string, params any) (any, error) {
		if method == "turn/start" {
			return nil, &CodexAppServerError{Code: -32000, Message: "model not available"}
		}
		return map[string]any{"thread": map[string]any{"id": "t1"}}, nil
	}
	r, _ := newFakeSession(f, CodexAppServerSessionOptions{}).RunTurn("hi", CodexTurnOptions{})
	if !strings.HasPrefix(r.Error, "turn/start failed: codex app-server error -32000: model not available\ncodex stderr (last 2 lines):\nERROR boom") || strings.Contains(r.Error, "abcdefghijklmnop") || r.ShouldRetire {
		t.Fatalf("error %q retire %v", r.Error, r.ShouldRetire)
	}
	f.handler = func(method string, params any) (any, error) {
		return nil, &CodexRequestTimeoutError{Method: method, Timeout: 10 * time.Second}
	}
	r2, _ := newFakeSession(f, CodexAppServerSessionOptions{}).RunTurn("hi", CodexTurnOptions{})
	if !strings.HasPrefix(r2.Error, "codex app-server startup failed: codex app-server method 'thread/start' timed out after 10.0s") || !r2.ShouldRetire {
		t.Fatalf("timeout %q", r2.Error)
	}
	f.stderr = []string{"error: refresh token expired"}
	f.handler = func(method string, params any) (any, error) {
		if method == "turn/start" {
			return nil, &CodexAppServerError{Code: -1, Message: "401 Unauthorized"}
		}
		return map[string]any{"thread": map[string]any{"id": "t1"}}, nil
	}
	r3, _ := newFakeSession(f, CodexAppServerSessionOptions{}).RunTurn("hi", CodexTurnOptions{})
	if r3.Error != codexOAuthReauthHint || !r3.ShouldRetire {
		t.Fatalf("oauth %q", r3.Error)
	}
}

func TestCodexToolIterationsSteerAndCompaction(t *testing.T) {
	f := &fakeCodexClient{}
	s := newFakeSession(f, CodexAppServerSessionOptions{})
	f.note("item/completed", map[string]any{"item": map[string]any{"type": "commandExecution", "id": "c1", "command": "ls", "cwd": "/", "exitCode": 0}})
	f.note("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 5}, "modelContextWindow": 272000}})
	agentDone(f, "thread-fake-001", "turn-fake-001", "done")
	r, _ := s.RunTurn("go", CodexTurnOptions{})
	if r.ToolIterations != 1 || r.ModelContextWindow != 272000 || cxGet(r.TokenUsageLast, "inputTokens") != 5 {
		t.Fatalf("turn %+v", r)
	}
	if ok, _ := s.RequestSteer("more"); ok {
		t.Fatal("steer accepted without an active turn")
	}
	s.activeTurnID = "turn-fake-001"
	if ok, _ := s.RequestSteer("  more  "); !ok || f.requests[len(f.requests)-1].method != "turn/steer" {
		t.Fatalf("steer %v", f.methods())
	}
	s.activeTurnID = ""
	f.note("item/completed", map[string]any{"threadId": "thread-child", "turnId": "child", "item": map[string]any{"type": "agentMessage", "id": "x", "text": "child"}})
	f.note("turn/started", map[string]any{"threadId": "thread-fake-001", "turn": map[string]any{"id": "compact-1"}})
	f.note("item/completed", map[string]any{"threadId": "thread-fake-001", "turnId": "compact-1", "item": map[string]any{"type": "contextCompaction", "id": "cc"}})
	f.note("turn/completed", map[string]any{"threadId": "thread-fake-001", "turn": map[string]any{"id": "compact-1", "status": "completed"}})
	c, _ := s.CompactThread(0, 0)
	if !c.Compacted || c.TurnID != "compact-1" || c.Error != "" || c.Interrupted || len(c.ProjectedMessages) != 1 {
		t.Fatalf("compact %+v", c)
	}
}

func TestCodexServerRequestRoutingAndPrompts(t *testing.T) {
	f := &fakeCodexClient{}
	var prompts [][2]string
	s := newFakeSession(f, CodexAppServerSessionOptions{ApprovalCallback: func(command, description string, allowPermanent bool) (string, error) {
		prompts = append(prompts, [2]string{command, description})
		return "session", nil
	}})
	s.EnsureStarted()
	s.handleServerRequest(map[string]any{"id": 9, "method": "weird/request", "params": map[string]any{}})
	if !reflect.DeepEqual(f.errorResponses, []string{"Unsupported method: weird/request"}) {
		t.Fatalf("unknown %v", f.errorResponses)
	}
	s.handleServerRequest(map[string]any{"id": 1, "method": "item/commandExecution/requestApproval", "params": map[string]any{"command": "rm -rf build", "reason": "cleanup"}})
	s.trackPendingFileChange(map[string]any{"method": "item/started", "params": map[string]any{"item": map[string]any{"type": "fileChange", "id": "fc", "changes": []any{
		map[string]any{"kind": map[string]any{"type": "update"}, "path": "a.py"}, map[string]any{"kind": map[string]any{"type": "add"}, "path": "b.py"},
		map[string]any{"path": "c.py"}, map[string]any{"kind": map[string]any{"type": "update"}, "path": "d.py"}}}}})
	s.handleServerRequest(map[string]any{"id": 2, "method": "item/fileChange/requestApproval", "params": map[string]any{"itemId": "fc", "grantRoot": "/repo"}})
	s.handleServerRequest(map[string]any{"id": 3, "method": "item/fileChange/requestApproval", "params": map[string]any{"itemId": "other"}})
	s.handleServerRequest(map[string]any{"id": 4, "method": "mcpServer/elicitation/request", "params": map[string]any{"serverName": "icebath-tools"}})
	s.handleServerRequest(map[string]any{"id": 5, "method": "item/permissions/requestApproval", "params": map[string]any{}})
	want := [][2]string{
		{"rm -rf build", "Codex requests exec in /tmp — cleanup"},
		{"apply_patch: 1 add, 3 update: a.py, b.py, c.py, +1 more", "1 add, 3 update: a.py, b.py, c.py, +1 more; grants write to /repo"},
		{"apply_patch", "Codex requests to apply a patch"},
	}
	if !reflect.DeepEqual(prompts, want) {
		t.Fatalf("prompts %q", prompts)
	}
	wantResp := [][2]any{
		{1, map[string]any{"decision": "acceptForSession"}}, {2, map[string]any{"decision": "acceptForSession"}}, {3, map[string]any{"decision": "acceptForSession"}},
		{4, map[string]any{"action": "accept", "content": nil, "_meta": nil}}, {5, map[string]any{"decision": "decline"}},
	}
	if !reflect.DeepEqual(f.responses, wantResp) {
		t.Fatalf("responses %v", f.responses)
	}
	if got := codexSummarizeFileChanges([]any{}); got != "1 change pending" {
		t.Fatalf("empty summary %q", got)
	}
	noCb := newFakeSession(&fakeCodexClient{}, CodexAppServerSessionOptions{})
	if noCb.decideExecApproval(map[string]any{}) != "decline" {
		t.Fatal("no callback must fail closed")
	}
}

func TestCodexSessionRetirement(t *testing.T) {
	f := &fakeCodexClient{}
	f.note("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "a", "text": "final words"}})
	r, _ := newFakeSession(f, CodexAppServerSessionOptions{}).RunTurn("go", CodexTurnOptions{TurnTimeout: 0.05})
	if r.FinalText != "final words" || r.Interrupted || r.Error != "" {
		t.Fatalf("recovered final message %+v", r)
	}
	f2 := &fakeCodexClient{stderr: []string{"panic: something"}}
	s2 := newFakeSession(f2, CodexAppServerSessionOptions{})
	s2.EnsureStarted()
	f2.closed = true
	r2, _ := s2.RunTurn("go", CodexTurnOptions{})
	if !r2.ShouldRetire || !strings.HasPrefix(r2.Error, "codex app-server subprocess exited unexpectedly\ncodex stderr (last 1 lines):\npanic: something") {
		t.Fatalf("dead subprocess %+v", r2)
	}
	f3 := &fakeCodexClient{}
	r3, _ := newFakeSession(f3, CodexAppServerSessionOptions{}).RunTurn("go", CodexTurnOptions{TurnTimeout: 0.05})
	if !r3.Interrupted || !r3.ShouldRetire || r3.Error != "turn timed out after 0.05s" || f3.methods()[len(f3.methods())-1] != "turn/interrupt" {
		t.Fatalf("deadline %+v %v", r3, f3.methods())
	}
	f4 := &fakeCodexClient{}
	s4 := newFakeSession(f4, CodexAppServerSessionOptions{})
	s4.EnsureStarted()
	s4.Close()
	r4, _ := s4.runTurnLoopAfterClose()
	if !r4.Interrupted || r4.Error != "codex app-server session closed while the turn was in flight" {
		t.Fatalf("closed %+v", r4)
	}
	f5 := &fakeCodexClient{respondErr: codexTransportLost("codex app-server stdin closed unexpectedly: [Errno 32] Broken pipe")}
	f5.serverRequests = []any{map[string]any{"id": "a", "method": "item/permissions/requestApproval", "params": map[string]any{}}}
	r5, err := newFakeSession(f5, CodexAppServerSessionOptions{}).RunTurn("go", CodexTurnOptions{})
	if err != nil || !r5.ShouldRetire || !strings.HasPrefix(r5.Error, "codex app-server request response failed: codex app-server stdin closed unexpectedly") {
		t.Fatalf("respond failure %+v %v", r5, err)
	}
}

func (s *CodexAppServerSession) runTurnLoopAfterClose() (*CodexTurnResult, error) {
	result := &CodexTurnResult{TurnID: "t"}
	return result, s.driveTurn(result, codexDriveOptions{turnTimeout: 1, pollTimeout: 0.01, timeoutLabel: "turn",
		onServerRequest: func(any) (bool, error) { return false, nil }, onNote: func(any, string) bool { return false }})
}

func TestCodexTurnAbortedMarkerAndOAuthClassifier(t *testing.T) {
	if codexHasTurnAbortedMarker("") || codexHasTurnAbortedMarker("plain") || !codexHasTurnAbortedMarker("x <turn_aborted> y") || !codexHasTurnAbortedMarker("<turn_aborted/>") {
		t.Fatal("marker detection")
	}
	cases := []struct {
		primary, stderr string
		want            bool
	}{
		{"", "", false}, {"invalid_grant", "", true}, {"", "Token has expired", true},
		{"401 Unauthorized", "", true}, {"", "401 unauthorized", false}, {"", "oauth", false}, {"model missing", "", false},
	}
	for _, c := range cases {
		if got := codexClassifyOAuthFailure(c.primary, c.stderr) != ""; got != c.want {
			t.Fatalf("%q %q -> %v", c.primary, c.stderr, got)
		}
	}
	scoped := map[string]any{"params": map[string]any{"turn": map[string]any{"id": "x", "threadId": "th"}}}
	if codexNotificationBelongsToTurn(scoped, "th", "y") || !codexNotificationBelongsToTurn(scoped, "th", "x") || !codexNotificationBelongsToTurn(map[string]any{"method": "m"}, "a", "b") {
		t.Fatal("scope filter")
	}
}

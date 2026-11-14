package tuiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestUnknownMethod(t *testing.T) {
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "bogus"})
	if rpcCode(resp) != -32601 {
		t.Fatalf("response %v", resp)
	}
}

func TestDispatchRunsShortHandlersInline(t *testing.T) {
	swapMethod(t, "fast.ping", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return ok(rid, map[string]any{"pong": true})
	})
	resp := Dispatch(context.Background(), map[string]any{"id": "r1", "method": "fast.ping", "params": map[string]any{}}, nil)
	want := map[string]any{"jsonrpc": "2.0", "id": "r1", "result": map[string]any{"pong": true}}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response %v", resp)
	}
}

func TestSlowHandlersRunOffTheReaderThread(t *testing.T) {
	for _, slowMethod := range []string{"complete.path", "complete.slash", "voice.toggle", "voice.record", "voice.tts", "wake.start", "wake.status"} {
		t.Run(slowMethod, func(t *testing.T) {
			release := make(chan struct{})
			ran := make(chan struct{}, 1)
			swapMethod(t, slowMethod, func(_ context.Context, rid any, _ map[string]any) map[string]any {
				ran <- struct{}{}
				select {
				case <-release:
				case <-time.After(10 * time.Second):
				}
				return ok(rid, map[string]any{})
			})
			transport := &recordingTransport{}
			started := time.Now()
			resp := Dispatch(context.Background(), map[string]any{"id": "slow", "method": slowMethod, "params": map[string]any{}}, transport)
			elapsed := time.Since(started)
			close(release)
			if resp != nil {
				t.Fatalf("%s ran inline on the reader thread: %v", slowMethod, resp)
			}
			if elapsed >= 5*time.Second {
				t.Fatalf("dispatch took %s", elapsed)
			}
			deadline := time.Now().Add(10 * time.Second)
			var frames []map[string]any
			for time.Now().Before(deadline) {
				transport.mu.Lock()
				frames = append([]map[string]any(nil), transport.frames...)
				transport.mu.Unlock()
				if len(frames) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(frames) == 0 || frames[0]["id"] != "slow" {
				t.Fatalf("frames %v", frames)
			}
			select {
			case <-ran:
			default:
				t.Fatal("handler never ran")
			}
		})
	}
}

// Line buffer standing in for the real stdout of the capture fixture
type stdoutCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *stdoutCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *stdoutCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Every JSON frame written so far, one per line
func (c *stdoutCapture) frames(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.text()), "\n") {
		if line == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("frame %q: %v", line, err)
		}
		out = append(out, frame)
	}
	return out
}

// Redirects the real stdout of the server to a buffer and drops every server request when the case ends
func captureProtocol(t *testing.T) *stdoutCapture {
	t.Helper()
	capture := &stdoutCapture{}
	t.Cleanup(tuigateway.ResetForTests)
	t.Cleanup(tuigateway.SwapRealStdoutForTests(capture))
	savedChannel := tuigateway.StdioIsRPCChannel()
	tuigateway.SetStdioIsRPCChannel(true)
	t.Cleanup(func() { tuigateway.SetStdioIsRPCChannel(savedChannel) })
	return capture
}

// The open request of a session once registered and delivered to the capture and every window
func waitOpen(t *testing.T, sid string, capture *stdoutCapture, windows ...*wsTestSocket) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if open := tuigateway.OpenRequests(sid); len(open) > 0 {
			req, _ := pyjson.Plain(open[0]).(map[string]any)
			id, _ := req["id"].(string)
			delivered := capture == nil || strings.Contains(capture.text(), id)
			for _, window := range windows {
				window.mu.Lock()
				seen := slices.ContainsFunc(window.sent, func(frame map[string]any) bool { return frame["id"] == id })
				window.mu.Unlock()
				delivered = delivered && seen
			}
			if delivered {
				return req
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server request never registered")
	return nil
}

// WebSocket client that receives frames and never answers, attached as the only client of a session
func silentWSSession(t *testing.T, sid string, transport tuigateway.Transport) {
	t.Helper()
	session := tuigateway.NewSession(map[string]any{"session_key": sid, "transport": transport, "history": []any{}, "agent_ready": nil})
	tuigateway.SetSessionRecord(sid, session)
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
}

// Frames a silent WebSocket client received
func socketFrames(sock *wsTestSocket) []map[string]any {
	sock.mu.Lock()
	defer sock.mu.Unlock()
	return append([]map[string]any(nil), sock.sent...)
}

func TestServerRequestRoundTripUsesResponseFrame(t *testing.T) {
	ctx := context.Background()
	capture := captureProtocol(t)
	box := make(chan string, 1)
	go func() { timeout := 5.0; box <- ask(ctx, "sudo", "s1", map[string]any{}, &timeout) }()
	req := waitOpen(t, "s1", capture)
	frames := capture.frames(t)
	want := map[string]any{"jsonrpc": "2.0", "id": req["id"], "method": "sudo", "params": map[string]any{"session_id": "s1"}}
	if !reflect.DeepEqual(frames[len(frames)-1], want) || !strings.HasPrefix(req["id"].(string), "srq-") {
		t.Fatalf("frame %v, request %v", frames[len(frames)-1], req)
	}
	if resp := Dispatch(ctx, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"value": "hunter2"}}, nil); resp != nil {
		t.Fatalf("response frame got a reply %v", resp)
	}
	if got := <-box; got != "hunter2" {
		t.Fatalf("answer %q", got)
	}
	if tuigateway.OpenRequestCount() != 0 {
		t.Fatal("request left open")
	}
}

func TestServerRequestWaitsForAWsClientThatAdvertised(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(tuigateway.ResetForTests)
	sock := newWSTestSocket()
	peer := tuigateway.NewWSTransport(sock, "test", nil)
	silentWSSession(t, "ws-new", peer)
	response := handleRequest(tuigateway.BindTransport(ctx, peer), map[string]any{"id": 1, "method": "client.capabilities",
		"params": map[string]any{"server_requests": true}})
	if methods, _ := resultOf(t, response)["server_requests"].([]string); !slices.Contains(methods, "clarify") {
		t.Fatalf("capabilities %v", response)
	}
	box := make(chan *pyjson.Object, 1)
	go func() {
		timeout := 5.0
		result, _ := tuigateway.Send(ctx, "sudo", "ws-new", pyjson.NewObject(), &timeout, nil)
		box <- result
	}()
	req := waitOpen(t, "ws-new", nil, sock)
	if frames := socketFrames(sock); frames[len(frames)-1]["id"] != req["id"] {
		t.Fatalf("frames %v", frames)
	}
	if resp := Dispatch(ctx, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"value": "yes"}}, nil); resp != nil {
		t.Fatalf("response frame got a reply %v", resp)
	}
	if result := <-box; result == nil || !reflect.DeepEqual(pyjson.Plain(result), map[string]any{"value": "yes"}) {
		t.Fatalf("result %v", result)
	}
	tuigateway.UnregisterLiveTransport(peer)
	if tuigateway.AnswersRequests(peer) {
		t.Fatal("disconnect kept the advertisement")
	}
}

func TestServerRequestErrorResponseFailsFast(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(tuigateway.ResetForTests)
	box := make(chan *pyjson.Object, 1)
	go func() {
		timeout := 5.0
		result, _ := tuigateway.Send(ctx, "sudo", "s1", pyjson.NewObject(), &timeout, nil)
		box <- result
	}()
	req := waitOpen(t, "s1", nil)
	started := time.Now()
	frame, _ := pyjson.LoadsDict(fmt.Sprintf(`{"id": %q, "error": {"code": -32601}}`, req["id"]))
	if !tuigateway.ResolveResponse(frame, nil) {
		t.Fatal("error frame did not resolve")
	}
	select {
	case result := <-box:
		if result != nil || time.Since(started) >= time.Second {
			t.Fatalf("result %v after %s", result, time.Since(started))
		}
	case <-time.After(time.Second):
		t.Fatal("send still waiting")
	}
}

// Session two advertised WebSocket windows show through one fanout transport
func twoWindowSession(t *testing.T, sid string) (*wsTestSocket, *wsTestSocket, []*tuigateway.WSTransport) {
	t.Helper()
	first, second := newWSTestSocket(), newWSTestSocket()
	windows := []*tuigateway.WSTransport{tuigateway.NewWSTransport(first, "test", nil), tuigateway.NewWSTransport(second, "test", nil)}
	for _, window := range windows {
		tuigateway.Advertise(window, true)
	}
	silentWSSession(t, sid, tuigateway.NewFanoutTransport(windows[0], windows[1]))
	return first, second, windows
}

// Not shown refusal frame for a request
func notShown(req map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": tuigateway.NotShownCode, "message": "not here"}}
}

func TestWindowOwnedRequestRefusesAtOnceWhenNoWindowShowsTheSession(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(tuigateway.ResetForTests)
	first, second, windows := twoWindowSession(t, "s-none")
	box := make(chan string, 1)
	go func() { timeout := 30.0; box <- ask(ctx, "preview.read", "s-none", map[string]any{}, &timeout) }()
	req := waitOpen(t, "s-none", nil, first, second)
	started := time.Now()
	if resp := Dispatch(ctx, notShown(req), windows[0]); resp != nil {
		t.Fatalf("decline got a reply %v", resp)
	}
	Dispatch(ctx, notShown(req), windows[0])
	select {
	case got := <-box:
		t.Fatalf("one window's replayed decline settled the request: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	Dispatch(ctx, notShown(req), windows[1])
	select {
	case got := <-box:
		var decoded any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil || !reflect.DeepEqual(decoded, map[string]any{"success": false, "error": tuigateway.NotShownMessage}) {
			t.Fatalf("answer %q", got)
		}
		if time.Since(started) >= 2*time.Second {
			t.Fatalf("refusal took %s", time.Since(started))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request still waiting")
	}
	if open := tuigateway.OpenRequests("s-none"); len(open) != 0 {
		t.Fatalf("open %v", open)
	}
}

func TestWindowOwnedRequestDeclineDoesNotBeatTheOwnerWindow(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(tuigateway.ResetForTests)
	bystanderSock, ownerSock, windows := twoWindowSession(t, "s-own")
	box := make(chan string, 1)
	go func() { timeout := 5.0; box <- ask(ctx, "preview.read", "s-own", map[string]any{}, &timeout) }()
	req := waitOpen(t, "s-own", nil, bystanderSock, ownerSock)
	Dispatch(ctx, notShown(req), windows[0])
	frame, _ := pyjson.LoadsDict(fmt.Sprintf(`{"jsonrpc": "2.0", "id": %q, "error": {"code": %d, "message": "not here"}}`, req["id"], tuigateway.NotShownCode))
	if !tuigateway.ResolveResponse(frame, nil) {
		t.Fatal("unattributed decline was not consumed")
	}
	open := tuigateway.OpenRequests("s-own")
	if len(open) != 1 || pyjson.Plain(open[0]).(map[string]any)["id"] != req["id"] {
		t.Fatalf("open %v", open)
	}
	select {
	case got := <-box:
		t.Fatalf("settled early %q", got)
	default:
	}
	Dispatch(ctx, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"value": `{"text": "page"}`}}, windows[1])
	select {
	case got := <-box:
		if got != `{"text": "page"}` {
			t.Fatalf("answer %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner answer never settled")
	}
}

func TestClientCapabilitiesAdvertisesCountingNotShownDeclines(t *testing.T) {
	t.Cleanup(tuigateway.ResetForTests)
	peer := tuigateway.NewWSTransport(newWSTestSocket(), "test", nil)
	response := handleRequest(tuigateway.BindTransport(context.Background(), peer), map[string]any{"id": 1,
		"method": "client.capabilities", "params": map[string]any{"server_requests": true}})
	if resultOf(t, response)["declines_not_shown"] != true {
		t.Fatalf("capabilities %v", response)
	}
}

func TestServerRequestTimeoutEmitsOneRequestCancel(t *testing.T) {
	for _, method := range []string{"secret", "sudo", "terminal.read", "tour"} {
		t.Run(method, func(t *testing.T) {
			capture := captureProtocol(t)
			timeout := 0.0
			if result, err := tuigateway.Send(context.Background(), method, "s1", pyjson.NewObject(), &timeout, nil); result != nil || err != nil {
				t.Fatalf("result %v %v", result, err)
			}
			frames := capture.frames(t)
			if len(frames) != 2 || frames[0]["method"] != method {
				t.Fatalf("frames %v", frames)
			}
			params, _ := frames[1]["params"].(map[string]any)
			want := map[string]any{"id": frames[0]["id"], "method": method, "reason": "timeout"}
			if params["type"] != "request.cancel" || params["session_id"] != "s1" || !reflect.DeepEqual(params["payload"], want) {
				t.Fatalf("cancel %v", frames[1])
			}
		})
	}
}

func TestLateResponseAndLockAreDroppedQuietly(t *testing.T) {
	ctx := context.Background()
	if resp := Dispatch(ctx, map[string]any{"jsonrpc": "2.0", "id": "srq-gone", "result": map[string]any{"value": ""}}, nil); resp != nil {
		t.Fatalf("late response got a reply %v", resp)
	}
	response := handleRequest(ctx, map[string]any{"id": "late", "method": "clarify.lock",
		"params": map[string]any{"request_id": "srq-gone", "question_id": "q0", "answer": ""}})
	if !reflect.DeepEqual(resultOf(t, response), map[string]any{"status": "expired"}) {
		t.Fatalf("response %v", response)
	}
}

// Starts a blocking batch clarify on s1 and returns its answer channel with the open request
func startBatchClarify(t *testing.T, capture *stdoutCapture, qids []string) (chan map[string]any, map[string]any) {
	t.Helper()
	normalized := make([]map[string]any, len(qids))
	for i, q := range qids {
		normalized[i] = map[string]any{"qid": q, "id": "", "question": q, "choices": nil, "choices_offered": []any{}, "multi_select": false}
	}
	box := make(chan map[string]any, 1)
	go func() { box <- clarifyBlock(context.Background(), "s1", normalized) }()
	return box, waitOpen(t, "s1", capture)
}

// clarify.lock of one question of an open batch
func lockClarify(rid, requestID any, questionID, answer string) map[string]any {
	return handleRequest(context.Background(), map[string]any{"id": rid, "method": "clarify.lock",
		"params": map[string]any{"request_id": requestID, "question_id": questionID, "answer": answer}})
}

func TestClarifyBatchLocksResolveInOrderAndKeepPartialOnTimeout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	capture := captureProtocol(t)
	box, req := startBatchClarify(t, capture, []string{"q0", "q1"})
	frames := capture.frames(t)
	sent := frames[len(frames)-1]["params"].(map[string]any)["questions"].([]any)[0].(map[string]any)
	if keys := slices.Sorted(maps.Keys(sent)); !slices.Equal(keys, []string{"choices", "multi_select", "qid", "question"}) {
		t.Fatalf("wire fields %v", keys)
	}
	want := map[string]any{"status": "ok", "remaining": []string{"q1"}}
	if first := lockClarify("a1", req["id"], "q0", "x"); !reflect.DeepEqual(resultOf(t, first), want) {
		t.Fatalf("first lock %v", first)
	}
	if redo := lockClarify("a1b", req["id"], "q0", "y"); !reflect.DeepEqual(resultOf(t, redo), want) {
		t.Fatalf("relock %v", redo)
	}
	if bad := lockClarify("bad", req["id"], "nope", ""); rpcCode(bad) != 4002 {
		t.Fatalf("unknown question %v", bad)
	}
	open := pyjson.Plain(tuigateway.OpenRequests("s1")[0]).(map[string]any)
	if answers := open["params"].(map[string]any)["answers"]; !reflect.DeepEqual(answers, map[string]any{"q0": "y"}) {
		t.Fatalf("snapshot answers %v", answers)
	}
	if last := lockClarify("a2", req["id"], "q1", ""); !reflect.DeepEqual(resultOf(t, last), map[string]any{"status": "ok", "remaining": []string{}}) {
		t.Fatalf("last lock %v", last)
	}
	if answer := <-box; !reflect.DeepEqual(answer, map[string]any{"answers": map[string]any{"q0": "y", "q1": ""}, "outcome": "submitted"}) {
		t.Fatalf("answer %v", answer)
	}
	writeFile(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), "clarify:\n  timeout: 2\n")
	box, req = startBatchClarify(t, capture, []string{"q0", "q1"})
	if locked := lockClarify("b1", req["id"], "q0", "kept"); resultOf(t, locked)["status"] != "ok" {
		t.Fatalf("lock %v", locked)
	}
	select {
	case answer := <-box:
		if !reflect.DeepEqual(answer, map[string]any{"answers": map[string]any{"q0": "kept"}, "outcome": "timed_out"}) {
			t.Fatalf("timed out answer %v", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch never timed out")
	}
	var cancelled []any
	for _, frame := range capture.frames(t) {
		if params, _ := frame["params"].(map[string]any); frame["method"] == "event" && params["type"] == "request.cancel" {
			cancelled = append(cancelled, params["payload"].(map[string]any)["id"])
		}
	}
	if !reflect.DeepEqual(cancelled, []any{req["id"]}) {
		t.Fatalf("cancels %v", cancelled)
	}
}

func TestClarifyBatchCancelAllIsAResponseWithoutAnswers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	capture := captureProtocol(t)
	box, req := startBatchClarify(t, capture, []string{"q0", "q1"})
	Dispatch(context.Background(), map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}}, nil)
	select {
	case answer := <-box:
		if !reflect.DeepEqual(answer, map[string]any{"answers": map[string]any{}, "outcome": "cancelled"}) {
			t.Fatalf("answer %v", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel all never settled")
	}
}

func TestClearPendingCancelsOnlyThatSession(t *testing.T) {
	capture := captureProtocol(t)
	ctx := context.Background()
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() { _, _ = tuigateway.Send(ctx, "sudo", "sid-a", pyjson.NewObject(), nil, nil); close(doneA) }()
	go func() { _, _ = tuigateway.Send(ctx, "sudo", "sid-b", pyjson.NewObject(), nil, nil); close(doneB) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && tuigateway.OpenRequestCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if sessionPendingKind("sid-a") != "sudo" {
		t.Fatalf("pending kind %q", sessionPendingKind("sid-a"))
	}
	sidA := "sid-a"
	clearPending(&sidA)
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("sid-a request survived its clear")
	}
	select {
	case <-doneB:
		t.Fatal("clearing sid-a ended sid-b")
	default:
	}
	if sessionPendingKind("sid-a") != "" || sessionPendingKind("sid-b") != "sudo" {
		t.Fatalf("pending kinds %q %q", sessionPendingKind("sid-a"), sessionPendingKind("sid-b"))
	}
	clearPending(nil)
	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatal("sid-b request survived the shutdown clear")
	}
	var reasons []any
	for _, frame := range capture.frames(t) {
		if frame["method"] == "event" {
			reasons = append(reasons, frame["params"].(map[string]any)["payload"].(map[string]any)["reason"])
		}
	}
	if !reflect.DeepEqual(reasons, []any{"interrupted", "shutdown"}) {
		t.Fatalf("reasons %v", reasons)
	}
}

// Stop doubles of the three agent shapes the explicit stop compatibility case drives
type legacyStopAgent struct{ calls *[]string }

func (a legacyStopAgent) Interrupt(string) { *a.calls = append(*a.calls, "legacy") }

type hardOnlyStopAgent struct{ calls *[]string }

func (a hardOnlyStopAgent) HardInterrupt(string) { *a.calls = append(*a.calls, "hard") }

func TestSessionInterruptUsesExplicitStopCompatibility(t *testing.T) {
	for _, kind := range []string{"legacy", "hard-only", "dynamic-getattr"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			var calls []string
			var stopper any = legacyStopAgent{&calls}
			if kind == "hard-only" {
				stopper = hardOnlyStopAgent{&calls}
			}
			session := tuigateway.NewSession(map[string]any{"agent": stopper, "running": true, "queued_prompt": "later",
				"session_key": "session-key", "_run_thread": nil})
			swapSessions(t, map[string]*tuigateway.Session{"ui-session": session})
			response := sessionInterrupt(context.Background(), "stop", map[string]any{"session_id": "ui-session"})
			if resultOf(t, response)["status"] != "interrupted" {
				t.Fatalf("response %v", response)
			}
			want := []string{"legacy"}
			if kind == "hard-only" {
				want = []string{"hard"}
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("calls %v", calls)
			}
		})
	}
}

// Live record whose payload a reattaching client receives
func payloadSession() *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"agent": nil, "cols": 80, "created_at": 1.0, "history": []any{},
		"running": true, "session_key": "stored-session"})
}

func TestLiveSessionPayloadReplaysPendingApproval(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	first := map[string]any{"choices": []any{"once", "deny"}, "command": "rm -rf /tmp/example", "description": "recursive delete"}
	tools.QueueGatewayApprovalForTests("stored-session", first)
	tools.QueueGatewayApprovalForTests("stored-session", map[string]any{"command": "rm -rf /tmp/later", "description": "later"})
	t.Cleanup(func() { tools.UnregisterGatewayNotify("stored-session") })
	payload := liveSessionPayload(context.Background(), "runtime-session", payloadSession(), livePayloadOptions{})
	replayed, _ := payload["pending_approval"].(map[string]any)
	if replayed == nil || reflect.ValueOf(replayed).Pointer() == reflect.ValueOf(first).Pointer() {
		t.Fatalf("pending approval %v", payload["pending_approval"])
	}
	if !truthy(replayed["request_id"]) {
		t.Fatal("no request id")
	}
	delete(replayed, "request_id")
	if !reflect.DeepEqual(replayed, first) {
		t.Fatalf("replayed %v", replayed)
	}
}

func TestLiveSessionPayloadReplaysOpenRequests(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(tuigateway.ResetForTests)
	params, _ := pyjson.LoadsDict(`{"question": "Which?", "choices": ["a", "b"]}`)
	req := map[string]any{"id": tuigateway.OpenRequestForTests("runtime-session", "clarify", params)}
	payload := liveSessionPayload(context.Background(), "runtime-session", payloadSession(), livePayloadOptions{})
	other := liveSessionPayload(context.Background(), "other-session", payloadSession(), livePayloadOptions{})
	want := []any{map[string]any{"id": req["id"], "method": "clarify",
		"params": map[string]any{"session_id": "runtime-session", "question": "Which?", "choices": []any{"a", "b"}}}}
	if got := pyjson.Plain(payload["open_requests"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("open requests %v", got)
	}
	snapshot := payload["open_requests"].([]any)[0]
	if replayedParams, isObj := snapshot.(*pyjson.Object).GetOr("params", nil).(*pyjson.Object); isObj {
		replayedParams.Set("question", "mutated")
	}
	if live := pyjson.Plain(tuigateway.OpenRequests("runtime-session")[0]).(map[string]any); live["params"].(map[string]any)["question"] != "Which?" {
		t.Fatal("payload holds a live reference to the request params")
	}
	if _, present := other["open_requests"]; present {
		t.Fatalf("other session payload %v", other)
	}
}

// Live record of a UI session bound to a stored session key
func approvalSession(t *testing.T, sid, key string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swapSessions(t, map[string]*tuigateway.Session{sid: tuigateway.NewSession(map[string]any{"session_key": key, "history": []any{}})})
	t.Cleanup(func() { tools.UnregisterGatewayNotify(key) })
}

func TestApprovalPendingReplaysUnresolvedRequests(t *testing.T) {
	approvalSession(t, "ui-1", "agent-1")
	tools.QueueGatewayApprovalForTests("agent-1", map[string]any{"request_id": "req-1", "command": "danger"})
	response := handleRequest(context.Background(), map[string]any{"id": "r1", "method": "approval.pending", "params": map[string]any{"session_id": "ui-1"}})
	want := map[string]any{"approvals": []map[string]any{{"request_id": "req-1", "command": "danger"}}}
	if !reflect.DeepEqual(resultOf(t, response), want) {
		t.Fatalf("response %v", response)
	}
}

func TestApprovalReceivedAcknowledgesExactRequest(t *testing.T) {
	approvalSession(t, "ui-1", "agent-1")
	entry := tools.QueueGatewayApprovalForTests("agent-1", map[string]any{"request_id": "req-1", "command": "danger"})
	other := tools.QueueGatewayApprovalForTests("agent-1", map[string]any{"request_id": "req-2", "command": "other"})
	response := handleRequest(context.Background(), map[string]any{"id": "r2", "method": "approval.received",
		"params": map[string]any{"session_id": "ui-1", "request_id": "req-1"}})
	if !reflect.DeepEqual(resultOf(t, response), map[string]any{"acknowledged": true}) {
		t.Fatalf("response %v", response)
	}
	if !entry.Acknowledged || other.Acknowledged {
		t.Fatalf("acknowledged %v %v", entry.Acknowledged, other.Acknowledged)
	}
}

// Waits until a queued approval entry settles and returns its choice
func settledChoice(t *testing.T, entry *tools.ApprovalEntry) string {
	t.Helper()
	select {
	case <-entry.Done():
		return entry.Result
	case <-time.After(2 * time.Second):
		t.Fatal("approval entry never settled")
		return ""
	}
}

func TestApprovalResponseCorrelatesRequestId(t *testing.T) {
	approvalSession(t, "ui-1", "agent-1")
	earlier := tools.QueueGatewayApprovalForTests("agent-1", map[string]any{"request_id": "req-0", "command": "first"})
	entry := tools.QueueGatewayApprovalForTests("agent-1", map[string]any{"request_id": "req-1", "command": "danger"})
	response := handleRequest(context.Background(), map[string]any{"id": "r3", "method": "approval.respond",
		"params": map[string]any{"session_id": "ui-1", "request_id": "req-1", "choice": "once"}})
	if !reflect.DeepEqual(resultOf(t, response), map[string]any{"resolved": 1}) {
		t.Fatalf("response %v", response)
	}
	if settledChoice(t, entry) != "once" {
		t.Fatalf("choice %q", entry.Result)
	}
	select {
	case <-earlier.Done():
		t.Fatal("respond resolved another request")
	default:
	}
}

func TestApprovalRespondFallsBackToRequestIdLookup(t *testing.T) {
	approvalSession(t, "ui-live", "agent-live")
	entry := tools.QueueGatewayApprovalForTests("agent-live", map[string]any{"request_id": "req-91684"})
	response := handleRequest(context.Background(), map[string]any{"id": "r-fallback", "method": "approval.respond",
		"params": map[string]any{"session_id": "gone-sid", "request_id": "req-91684", "choice": "once"}})
	if !reflect.DeepEqual(resultOf(t, response), map[string]any{"resolved": 1}) {
		t.Fatalf("response %v", response)
	}
	if settledChoice(t, entry) != "once" {
		t.Fatalf("choice %q", entry.Result)
	}
}

func TestApprovalRespondFallsBackToStoredSessionId(t *testing.T) {
	approvalSession(t, "ui-stored", "stored-91684")
	entry := tools.QueueGatewayApprovalForTests("stored-91684", map[string]any{"command": "danger"})
	response := handleRequest(context.Background(), map[string]any{"id": "r-stored", "method": "approval.respond",
		"params": map[string]any{"session_id": "stored-91684", "choice": "deny"}})
	if !reflect.DeepEqual(resultOf(t, response), map[string]any{"resolved": 1}) {
		t.Fatalf("response %v", response)
	}
	if settledChoice(t, entry) != "deny" {
		t.Fatalf("choice %q", entry.Result)
	}
}

func TestApprovalForAWsClientThatNeverAdvertisedSettlesTheQueueEntry(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeFile(t, filepath.Join(h, "config.yaml"), "approvals:\n  timeout: 3\n")
	t.Cleanup(tuigateway.ResetForTests)
	sock := newWSTestSocket()
	silentWSSession(t, "ws-old-approval", tuigateway.NewWSTransport(sock, "test", nil))
	tools.RegisterGatewayNotify("ws-old-approval", func(data map[string]any) error {
		emitApprovalRequest(ctx, "ws-old-approval", data)
		return nil
	})
	started := time.Now()
	resolved, choice, cancelled := func() (bool, string, string) {
		defer tools.UnregisterGatewayNotify("ws-old-approval")
		return tools.AwaitGatewayDecisionForTests(ctx, &tools.Env{Home: h}, "ws-old-approval", tools.GatewayNotifyForTests("ws-old-approval"),
			map[string]any{"command": "rm -rf build", "description": "", "pattern_key": "dangerous", "pattern_keys": []any{"dangerous"}})
	}()
	if waited := time.Since(started); waited >= time.Second {
		t.Fatalf("waited %s: %v %q %q", waited, resolved, choice, cancelled)
	}
	if choice != "" || cancelled == "" {
		t.Fatalf("decision choice %q cancelled %q", choice, cancelled)
	}
	if frames := socketFrames(sock); len(frames) != 0 {
		t.Fatalf("frames %v", frames)
	}
	if tools.GetPendingGatewayApproval("ws-old-approval") != nil {
		t.Fatal("queue entry left behind")
	}
}

func TestApprovalThatEndsBeforeItsSettleHookAttachesIsStillWithdrawn(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(tuigateway.ResetForTests)
	sock := newWSTestSocket()
	peer := tuigateway.NewWSTransport(sock, "test", nil)
	silentWSSession(t, "ws-raced", peer)
	t.Cleanup(func() { tuigateway.UnregisterLiveTransport(peer) })
	handleRequest(tuigateway.BindTransport(ctx, peer), map[string]any{"id": 1, "method": "client.capabilities", "params": map[string]any{"server_requests": true}})
	emitApprovalRequest(ctx, "ws-raced", map[string]any{"command": "rm -rf build", "description": "", "request_id": "appr-already-resolved"})
	frames := socketFrames(sock)
	if len(frames) != 2 {
		t.Fatalf("the sent approval was never withdrawn: %v", frames)
	}
	sent, cancel := frames[0], frames[1]
	params, _ := cancel["params"].(map[string]any)
	if sent["method"] != "approval" || params["type"] != "request.cancel" || params["payload"].(map[string]any)["id"] != sent["id"] {
		t.Fatalf("frames %v", frames)
	}
	if open := tuigateway.OpenRequests("ws-raced"); len(open) != 0 {
		t.Fatalf("open %v", open)
	}
}

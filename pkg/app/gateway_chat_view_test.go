package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Polls the condition until it holds, failing the case after the deadline
func gatewayWaitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %s", what)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFiniteFollowerDoesNotWaitForeverAfterWorkerLoss(t *testing.T) {
	for _, beforeAck := range []bool{false, true} {
		for _, streamJSON := range []bool{false, true} {
			out, errOut := captureStreams(t)
			peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
				if method != "prompt.submit" {
					t.Fatalf("unexpected %s", method)
				}
				p.emit(map[string]any{"session_id": "s", "type": "session.info", "admission_id": "predecessor", "payload": map[string]any{
					"pending": []any{map[string]any{"admission_id": "predecessor", "status": "unknown"},
						map[string]any{"admission_id": "ours", "status": "queued"}}}})
				if beforeAck {
					p.drained()
				}
				return map[string]any{"admission_id": "ours"}, nil
			})
			var emitter *StreamJSONEmitter
			if streamJSON {
				emitter = NewStreamJSONEmitter("m", "s")
			}
			view := newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, true, emitter, "")
			code, err := runViewWithin(t, view, "queued", true)
			if err != nil || code != 3 {
				t.Fatalf("before_ack=%v stream=%v: code %d err %v", beforeAck, streamJSON, code, err)
			}
			calls := peer.recorded()
			if len(calls) != 1 || calls[0].params["text"] != "queued" {
				t.Fatalf("calls %v", calls)
			}
			if !strings.Contains(errOut.String(), "Your message is saved") || !strings.Contains(errOut.String(), "don't send it again") {
				t.Fatalf("stderr %q", errOut.String())
			}
			if streamJSON {
				records := jsonlRecords(t, out.String())
				if last := records[len(records)-1]; last["type"] != "result" || last["exit_code"] != float64(3) {
					t.Fatalf("records %v", records)
				}
			}
		}
	}
}

func TestSuccessorUnknownDoesNotReplaceCompletedPrompt(t *testing.T) {
	captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.receipt" {
			return map[string]any{"result": map[string]any{"completed": true}}, nil
		}
		p.emit(map[string]any{"session_id": "s", "type": "message.complete", "admission_id": "ours", "payload": map[string]any{"outcome": "completed", "text": "done"}})
		p.emit(map[string]any{"session_id": "s", "type": "session.info", "admission_id": "next", "payload": map[string]any{
			"pending": []any{map[string]any{"admission_id": "next", "status": "unknown"}}}})
		return map[string]any{"admission_id": "ours"}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, true, nil, "")
	if code, err := runViewWithin(t, view, "work", true); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestOwnLostTurnExitsFromItsUnknownCompletionNotQueuedDetach(t *testing.T) {
	for _, yieldBetween := range []bool{false, true} {
		_, errOut := captureStreams(t)
		var view *GatewayChatView
		peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
			if method == "prompt.receipt" {
				return map[string]any{"result": map[string]any{}}, nil
			}
			go func() {
				gatewayWaitUntil(t, func() bool { return onViewLoop(view, func() bool { return view.finiteAdmission == "ours" }) }, "the receipt")
				p.emit(map[string]any{"session_id": "s", "type": "session.info", "admission_id": "ours", "payload": map[string]any{
					"pending": []any{map[string]any{"admission_id": "ours", "status": "unknown"}}}})
				if yieldBetween {
					gatewayWaitUntil(t, func() bool {
						return onViewLoop(view, func() bool {
							for _, raw := range view.pending {
								if row, _ := raw.(map[string]any); row["admission_id"] == "ours" {
									return true
								}
							}
							return false
						})
					}, "session.info rendered")
				}
				p.emit(map[string]any{"session_id": "s", "type": "message.complete", "admission_id": "ours",
					"payload": map[string]any{"outcome": "unknown", "text": "Worker execution is unknown."}})
			}()
			return map[string]any{"admission_id": "ours"}, nil
		})
		view = newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, true, nil, "")
		if code, err := runViewWithin(t, view, "work", true); err != nil || code != 1 {
			t.Fatalf("yield=%v: code %d err %v", yieldBetween, code, err)
		}
		if strings.Contains(errOut.String(), "Your message is saved") {
			t.Fatalf("stderr %q", errOut.String())
		}
	}
}

func TestResumeListsPendingAdmissionTextCappedAndTerminalSafe(t *testing.T) {
	_, errOut := captureStreams(t)
	hostile := "\x1b]0;pwned\x07\x1b[2Jdeploy\rhidden\x08‮\x9b31m now " + strings.Repeat("x", 200)
	snapshot := map[string]any{"stored_session_id": "stored", "pending": []any{
		map[string]any{"admission_id": "adm-unknown", "status": "unknown", "execution_generation": int64(4), "input_id": "in-1",
			"text": "summarise the logs", "controls": []any{}, "steering": false},
		map[string]any{"admission_id": "adm-queued", "status": "queued", "execution_generation": nil, "input_id": "in-2",
			"text": hostile, "controls": []any{}, "steering": false},
	}}
	newGatewayChatView(nil, snapshot, false, nil, "").showPending()
	text := errOut.String()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	index := func(want string) int {
		for i, line := range lines {
			if line == want {
				return i
			}
		}
		return -1
	}
	if a, b := index(`    "summarise the logs"`), index("/discard adm-unknown"); a < 0 || b < 0 || a >= b {
		t.Fatalf("lines %q", lines)
	}
	queued := ""
	for _, line := range lines {
		if strings.Contains(line, "deploy hidden") {
			queued = line
		}
	}
	if !strings.Contains(queued, "now xxx") || !strings.HasPrefix(queued, `    "`) || !strings.HasSuffix(queued, `…"`) {
		t.Fatalf("queued %q", queued)
	}
	if body := []rune(queued)[5 : len([]rune(queued))-1]; len(body) > 80 {
		t.Fatalf("preview %d chars", len(body))
	}
	for _, r := range text {
		if r != '\n' && !pystr.IsPrintable(r) {
			t.Fatalf("non printable %q in %q", r, text)
		}
	}
}

func TestQueryFileReadMatchesMain(t *testing.T) {
	cases := []struct{ name, expected string }{{"missing", "cannot read --query-file"}, {"directory", "cannot read --query-file"},
		{"empty", "--query-file is empty"}, {"latin1", ""}}
	for _, tc := range cases {
		for _, format := range []string{"text", "stream-json"} {
			tempHome(t)
			chdirFor(t, t.TempDir())
			out, errOut := captureStreams(t)
			dir := t.TempDir()
			path := map[string]string{"missing": filepath.Join(dir, "gone.txt"), "directory": dir}[tc.name]
			if path == "" {
				path = filepath.Join(dir, "dm.txt")
			}
			if tc.name == "empty" {
				os.WriteFile(path, []byte("  \n"), 0o644)
			}
			if tc.name == "latin1" {
				os.WriteFile(path, []byte("caf\xe9 $(id) \"quoted\""), 0o644)
			}
			var launched []string
			peer := oneshotSessionPeer(t, &launched)
			args := map[string]any{"query_file": path, "output_format": format}
			code := launchFromArgs(context.Background(), args, ensureConfigured, peer.connector())
			if tc.expected == "" {
				if code != 0 || len(launched) != 1 || launched[0] != "caf� $(id) \"quoted\"" {
					t.Fatalf("%s/%s: code %d launched %q", tc.name, format, code, launched)
				}
				continue
			}
			if code != 2 || len(launched) != 0 {
				t.Fatalf("%s/%s: code %d launched %q", tc.name, format, code, launched)
			}
			if !strings.Contains(strings.ReplaceAll(errOut.String(), "--query-file "+path, "--query-file"), "Error: "+tc.expected) {
				t.Fatalf("%s/%s: stderr %q", tc.name, format, errOut.String())
			}
			if format == "stream-json" {
				records := jsonlRecords(t, out.String())
				result := records[len(records)-1]
				isError, ok := result["is_error"]
				if !ok {
					isError = result["subtype"] != "success"
				}
				if result["type"] != "result" || isError != true {
					t.Fatalf("%s: result %v", tc.name, result)
				}
			}
		}
	}
}

func TestClassicCLINewSendsThisShellsClientEnv(t *testing.T) {
	t.Setenv("PATH", "/client/venv/bin:/usr/bin")
	t.Setenv("VIRTUAL_ENV", "/client/venv")
	t.Setenv("ICEBATH_EPHEMERAL_SYSTEM_PROMPT", "mine")
	t.Setenv("NODE_AUTH_TOKEN", "npm-secret")
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	tempHome(t)
	captureStreams(t)
	sent := map[string]any{}
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		switch method {
		case "session.info":
			return map[string]any{"launch_request": map[string]any{"source": "cli"}, "cwd": "/tmp"}, nil
		case "runtime.describe":
			return map[string]any{"session_create": map[string]any{"parameters": []any{"source", "cwd", "client_env"}}}, nil
		}
		for k, v := range params {
			sent[k] = v
		}
		return map[string]any{"stored_session_id": "new", "execution_generation": 0}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "old"}, true, nil, "")
	view.finite = true
	if err := onViewLoop(view, func() error { return gatewayNew(context.Background(), view, "") }); err != nil {
		t.Fatal(err)
	}
	env, _ := sent["client_env"].(map[string]any)
	if env["PATH"] != "/client/venv/bin:/usr/bin" || env["VIRTUAL_ENV"] != "/client/venv" || env["ICEBATH_EPHEMERAL_SYSTEM_PROMPT"] != "mine" {
		t.Fatalf("client env %v", env)
	}
	if _, ok := env["NODE_AUTH_TOKEN"]; ok {
		t.Fatalf("secret forwarded %v", env)
	}
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatalf("secret forwarded %v", env)
	}
}

// Session replay gap notification of session s
func replayGapEvent() map[string]any {
	return map[string]any{"type": "session.replay_gap", "session_id": "s", "payload": map[string]any{}}
}

// Runs fn on a view whose acked admission a a replay gap then cuts, the view and result
func gappedView(t *testing.T, answer func(p *fakeGatewayPeer, method string, params map[string]any) (any, error),
	fn func(ctx context.Context, v *GatewayChatView) (any, error)) (*GatewayChatView, any, error) {
	t.Helper()
	acked := make(chan struct{}, 1)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		result, err := answer(p, method, params)
		if method == "prompt.submit" {
			select {
			case acked <- struct{}{}:
			default:
			}
		}
		return result, err
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, false, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := fn(ctx, view)
		done <- outcome{result, err}
	}()
	gatewayWaitUntil(t, func() bool {
		if onViewLoop(view, func() bool { return view.finiteAdmission != nil }) {
			return true
		}
		select {
		case <-acked:
			acked <- struct{}{}
			return !onViewLoop(view, func() bool { return view.finite })
		default:
			return false
		}
	}, "the acked admission")
	peer.emit(replayGapEvent())
	select {
	case o := <-done:
		return view, o.result, o.err
	case <-time.After(2 * time.Second):
		t.Fatal("view did not finish within 2s")
	}
	return view, nil, nil
}

func TestAdoptedApprovalEndsTheOneshotOnlyWhileTheSnapshotListsIt(t *testing.T) {
	for _, tc := range []struct {
		own  any
		code int
	}{{"queued", 3}, {nil, 0}} {
		captureStreams(t)
		answer := func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
			switch method {
			case "prompt.submit":
				return map[string]any{"admission_id": "a", "status": "queued"}, nil
			case "session.resume":
				pending := []any{map[string]any{"admission_id": "b", "status": "started"}}
				if tc.own != nil {
					pending = append(pending, map[string]any{"admission_id": "a", "status": tc.own})
				}
				return map[string]any{"pending": pending, "prompts": []any{map[string]any{"prompt_id": "p", "kind": "approval", "execution_generation": 8}}}, nil
			}
			return map[string]any{"admission_id": "a", "status": "terminal", "outcome": "completed",
				"result": map[string]any{"final_response": "DONE", "completed": true}}, nil
		}
		view, result, err := gappedView(t, answer, func(ctx context.Context, v *GatewayChatView) (any, error) { return v.Run(ctx, "q", true) })
		if err != nil || result != tc.code {
			t.Fatalf("own %v: result %v err %v", tc.own, result, err)
		}
		if tc.own == nil {
			completion, _ := onViewLoop(view, func() map[string]any { c, _ := view.completions.Get("a"); return c })["text"].(string)
			if completion != "DONE" {
				t.Fatalf("completion %q", completion)
			}
		}
	}
}

func TestUnknownRowSettlesTheInteractiveTurn(t *testing.T) {
	out, _ := captureStreams(t)
	answer := func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.submit" {
			return map[string]any{"admission_id": "a", "status": "queued"}, nil
		}
		return map[string]any{"pending": []any{map[string]any{"admission_id": "a", "status": "unknown"}}, "prompts": []any{}}, nil
	}
	view, failure, err := gappedView(t, answer, func(ctx context.Context, v *GatewayChatView) (any, error) {
		renderCtx, stop := context.WithCancel(ctx)
		rendered := make(chan struct{})
		go func() {
			defer close(rendered)
			v.render(renderCtx)
		}()
		defer func() {
			stop()
			<-rendered
		}()
		v.mu.Lock()
		_, err := v.submit(ctx, "q")
		v.mu.Unlock()
		if err != nil {
			return nil, err
		}
		gatewayWaitUntil(t, func() bool {
			return onViewLoop(v, func() bool { return v.completions.Has("a") || v.failure != nil })
		}, "the settled turn")
		return onViewLoop(v, func() error { return v.failure }), nil
	})
	if err != nil || failure != error(nil) {
		t.Fatalf("failure %v err %v", failure, err)
	}
	completion := onViewLoop(view, func() map[string]any { c, _ := view.completions.Get("a"); return c })
	if completion["outcome"] != "unknown" || !strings.Contains(out.String(), "couldn't confirm how this turn ended") {
		t.Fatalf("completion %v stdout %q", completion, out.String())
	}
}

func TestAckDuringTheReattachSettlesAgainstItsSnapshot(t *testing.T) {
	out, _ := captureStreams(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	event := func(kind string, payload map[string]any) map[string]any {
		return map[string]any{"type": kind, "session_id": "s", "admission_id": "a", "payload": payload}
	}
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.submit" {
			p.emit(replayGapEvent())
			<-entered
			return map[string]any{"admission_id": "a"}, nil
		}
		enteredOnce.Do(func() { close(entered) })
		p.emit(event("message.delta", map[string]any{"text": "Hello"}))
		p.emit(event("message.complete", map[string]any{"text": "Hello", "outcome": "completed"}))
		<-release
		return map[string]any{"pending": []any{map[string]any{"admission_id": "a", "status": "started"}}, "prompts": []any{}}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, false, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	rendered := make(chan struct{})
	go func() {
		defer close(rendered)
		view.render(ctx)
	}()
	defer func() {
		cancel()
		<-rendered
	}()
	submitted := make(chan error, 1)
	go func() {
		view.mu.Lock()
		_, err := view.submit(ctx, "q")
		view.mu.Unlock()
		submitted <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("re-attach never resumed")
	}
	close(release)
	select {
	case err := <-submitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit never returned")
	}
	gatewayWaitUntil(t, func() bool {
		return peer.events.Empty() && onViewLoop(view, func() bool { return view.completions.Has("a") })
	}, "the settled turn")
	if out.String() != "Hello\n" {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestSendDirectivePrintsNoticeAndSubmitsThroughTheView(t *testing.T) {
	out, _ := captureStreams(t)
	reply := map[string]any{"type": "send", "message": "PLAN PROMPT BODY", "display": "/plan add a cache", "notice": "Planning: add a cache"}
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.submit" {
			return map[string]any{"admission_id": "adm"}, nil
		}
		return reply, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, false, nil, "")
	err := onViewLoop(view, func() error {
		_, err := gatewayRunCommand(context.Background(), view, "/plan", "add a cache")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := peer.recorded()
	if len(calls) != 2 || calls[0].method != "slash.exec" || len(calls[0].params) != 2 || calls[0].params["session_id"] != "s" ||
		calls[0].params["command"] != "plan add a cache" || calls[1].method != "prompt.submit" || calls[1].params["text"] != "PLAN PROMPT BODY" {
		t.Fatalf("calls %v", calls)
	}
	printed := out.String()
	if !strings.Contains(printed, "Planning: add a cache") || !strings.Contains(printed, "● /plan add a cache") || strings.Contains(printed, "PLAN PROMPT BODY") {
		t.Fatalf("stdout %q", printed)
	}
}

func TestExecOutputStillPrints(t *testing.T) {
	out, _ := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method != "slash.exec" {
			t.Fatalf("unexpected %s", method)
		}
		return map[string]any{"type": "exec", "output": "curator ok"}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "s"}, false, nil, "")
	err := onViewLoop(view, func() error {
		_, err := gatewayRunCommand(context.Background(), view, "/curator", "")
		return err
	})
	if err != nil || !strings.Contains(out.String(), "curator ok") {
		t.Fatalf("err %v stdout %q", err, out.String())
	}
}

// A foreign admission's approval or clarify control event
func controlEvent(kind string) map[string]any {
	control := "clarify"
	if strings.HasPrefix(kind, "approval") {
		control = "approval"
	}
	payload := map[string]any{"prompt_id": "p1", "kind": control, "execution_generation": 7}
	if strings.HasSuffix(kind, ".request") {
		if control == "approval" {
			payload["command"], payload["choices"] = "secret command", []any{"yes", "no"}
		} else {
			payload["question"] = "secret question"
		}
	}
	return map[string]any{"type": kind, "session_id": "stored", "admission_id": "neighbor", "payload": payload}
}

func TestOneshotTracksForeignBlockingControlAndDetaches(t *testing.T) {
	for _, kind := range []string{"approval.request", "clarify.request"} {
		for _, timing := range []string{"before_receipt", "after_receipt"} {
			out, errOut := captureStreams(t)
			peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
				if method != "prompt.submit" {
					t.Fatalf("unexpected %s", method)
				}
				neighbor := func() {
					p.emit(map[string]any{"type": "message.delta", "session_id": "stored", "admission_id": "neighbor",
						"payload": map[string]any{"text": "neighbor output"}})
					p.emit(controlEvent(kind))
				}
				if timing == "before_receipt" {
					neighbor()
					p.drained()
				} else {
					go func() {
						time.Sleep(time.Millisecond)
						neighbor()
					}()
				}
				return map[string]any{"admission_id": "mine"}, nil
			})
			view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored"}, true, nil, "")
			code, err := runViewWithin(t, view, "query", true)
			if err != nil || code != 3 {
				t.Fatalf("%s/%s: code %d err %v", kind, timing, code, err)
			}
			if strings.Contains(out.String(), "neighbor output") || strings.Contains(errOut.String(), "secret command") ||
				strings.Contains(errOut.String(), "secret question") || !strings.Contains(errOut.String(), "Input required; detached without cancelling") {
				t.Fatalf("%s/%s: stdout %q stderr %q", kind, timing, out.String(), errOut.String())
			}
		}
	}
}

func TestOneshotAppliesForeignControlSettlementBeforeReceipt(t *testing.T) {
	for _, kind := range []string{"approval.settled", "clarify.settled"} {
		captureStreams(t)
		control := "clarify"
		pending := map[string]any{"prompt_id": "p1", "execution_generation": 7}
		if strings.HasPrefix(kind, "approval") {
			control = "approval"
			pending["command"], pending["choices"] = "old command", []any{"yes", "no"}
		} else {
			pending["question"] = "old question"
		}
		pending["kind"] = control
		peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
			if method == "prompt.receipt" {
				return map[string]any{"status": "terminal", "result": map[string]any{"completed": true, "final_response": "done"}}, nil
			}
			p.emit(controlEvent(kind))
			p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": "mine",
				"payload": map[string]any{"text": "done", "outcome": "completed"}})
			p.drained()
			return map[string]any{"admission_id": "mine"}, nil
		})
		view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored", "prompts": []any{pending}}, true, nil, "")
		code, err := runViewWithin(t, view, "query", true)
		if err != nil || code != 0 {
			t.Fatalf("%s: code %d err %v", kind, code, err)
		}
		if onViewLoop(view, func() bool { return view.prompts.Has("p1") }) {
			t.Fatalf("%s: p1 still pending", kind)
		}
	}
}

func TestFiniteRendererAppliesForeignSettlementAfterReceipt(t *testing.T) {
	for _, kind := range []string{"approval.settled", "clarify.settled"} {
		control := "clarify"
		if strings.HasPrefix(kind, "approval") {
			control = "approval"
		}
		peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
			t.Fatalf("unexpected %s", method)
			return nil, nil
		})
		view := newGatewayChatView(peer, map[string]any{"stored_session_id": "stored", "prompts": []any{
			map[string]any{"prompt_id": "p1", "kind": control, "execution_generation": 7}}}, true, nil, "")
		view.finite, view.finiteAdmission = true, "mine"
		ctx, cancel := context.WithCancel(context.Background())
		rendered := make(chan struct{})
		go func() {
			defer close(rendered)
			view.render(ctx)
		}()
		changed := onViewLoop(view, func() chan struct{} { return view.changed.ch })
		peer.emit(controlEvent(kind))
		select {
		case <-changed:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the renderer never woke the view", kind)
		}
		if onViewLoop(view, func() bool { return view.prompts.Has("p1") }) {
			t.Fatalf("%s: p1 still pending", kind)
		}
		cancel()
		<-rendered
	}
}

func TestCompressWaitsPastTheDefaultRPCBudget(t *testing.T) {
	var sent []map[string]any
	var budgets []time.Duration
	rpc := func(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
		sent = append(sent, params)
		if method == "session.resume" {
			return map[string]any{"revision": int64(1), "execution_generation": int64(1)}, nil
		}
		budgets = append(budgets, timeout)
		return map[string]any{"status": "applied"}, nil
	}
	result, err := NewPreparedMutations().Apply(context.Background(), rpc, "s", "compress", pyjson.NewObject(), nil)
	if err != nil || len(result) != 1 || result["status"] != "applied" {
		t.Fatalf("result %v err %v", result, err)
	}
	if len(budgets) != 1 || budgets[0] != gateway.CompressRPCTimeout || budgets[0] <= gateway.DefaultRPCTimeout {
		t.Fatalf("mutate budgets %v", budgets)
	}
	for _, params := range sent {
		if _, ok := params["_timeout"]; ok {
			t.Fatalf("budget on the wire: %v", params)
		}
	}
}

// Socket whose owner answers session.resume only after a burst of events, recording when the answer was read
type resumeWire struct {
	burst      int
	frames     chan string
	answerRead chan struct{}
	readOnce   sync.Once
	closed     chan struct{}
	closeOnce  sync.Once
}

func newResumeWire(burst int) *resumeWire {
	return &resumeWire{burst: burst, frames: make(chan string, burst+16), answerRead: make(chan struct{}), closed: make(chan struct{})}
}

// Event frame of session s
func wireEvent(kind string, seq int) string {
	text, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params", pyjson.ObjectOf(
		"session_id", "s", "type", kind, "replay_epoch", "e", "seq", seq, "admission_id", nil, "payload", pyjson.ObjectOf("text", "x"))))
	return text
}

func (w *resumeWire) Recv() (string, error) {
	select {
	case frame := <-w.frames:
		if strings.Contains(frame, `"id"`) {
			w.readOnce.Do(func() { close(w.answerRead) })
		}
		return frame, nil
	case <-w.closed:
		return "", net.ErrClosed
	}
}

func (w *resumeWire) Send(data string) error {
	decoded, _ := pyjson.Loads(data)
	request := decoded.(*pyjson.Object)
	if request.Vals["method"] != "session.resume" {
		return errors.New("AssertionError: " + pyjson.Plain(request.Vals["method"]).(string))
	}
	for seq := 2; seq < w.burst+2; seq++ {
		w.frames <- wireEvent("message.delta", seq)
	}
	answer, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", request.Vals["id"], "result", pyjson.ObjectOf(
		"stored_session_id", "s", "replay_epoch", "e", "last_sequence", 1, "pending", []any{}, "prompts", []any{})))
	w.frames <- answer
	return nil
}

func (w *resumeWire) Open() bool          { return true }
func (w *resumeWire) Subprotocol() string { return "" }
func (w *resumeWire) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

func TestResumeAnswerBehindABurstStaysReachable(t *testing.T) {
	prev := gateway.EventStallTimeout
	gateway.EventStallTimeout = 30 * time.Second
	t.Cleanup(func() { gateway.EventStallTimeout = prev })
	for _, burst := range []int{4096, 4097, 6000} {
		captureStreams(t)
		wire := newResumeWire(burst)
		client := gateway.NewGatewayClient(wire).Start()
		view := newGatewayChatView(client, map[string]any{"stored_session_id": "s"}, true, nil, "")
		ctx, cancel := context.WithCancel(context.Background())
		pumped := make(chan struct{})
		go func() {
			defer close(pumped)
			view.render(ctx)
		}()
		wire.frames <- wireEvent("session.replay_gap", 1)
		select {
		case <-wire.answerRead:
		case <-pumped:
		case <-time.After(10 * time.Second):
		}
		select {
		case <-wire.answerRead:
		default:
			t.Fatalf("burst %d: answer unread, queued=%d", burst, client.EventQueue().QSize())
		}
		if failure := onViewLoop(view, func() error { return view.failure }); failure != nil {
			t.Fatalf("burst %d: failure %v", burst, failure)
		}
		cancel()
		<-pumped
		client.Close()
	}
}

func TestChatQSaysWhyATurnWasInterrupted(t *testing.T) {
	_, errOut := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		switch method {
		case "prompt.submit":
			go p.emit(map[string]any{"type": "message.complete", "session_id": "sid-1", "admission_id": "adm-1",
				"payload": map[string]any{"text": "", "outcome": "cancelled", "status": "interrupted"}})
			return map[string]any{"admission_id": "adm-1", "status": "queued", "execution_generation": 7}, nil
		case "prompt.receipt":
			return map[string]any{"result": map[string]any{"final_response": "", "interrupted": true,
				"interrupt_message": "Gateway shutting down"}}, nil
		}
		return map[string]any{}, nil
	})
	code, err := runViewWithin(t, newGatewayChatView(peer, map[string]any{"stored_session_id": "sid-1"}, false, nil, ""), "hi", true)
	if err != nil || code != 130 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(errOut.String(), "Turn interrupted (Gateway shutting down); it was not retried.") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestNonWorkerOneShotPrintsItsExitSummary(t *testing.T) {
	out, errOut := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": "mine",
			"payload": map[string]any{"text": "API call failed after 3 retries", "outcome": "failed"}})
		return map[string]any{"admission_id": "mine"}, nil
	})
	code, err := runViewWithin(t, newGatewayChatView(peer, map[string]any{"stored_session_id": "stored"}, true, nil, ""), "hello", true)
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v", code, err)
	}
	submits := 0
	for _, method := range peer.methods() {
		if method == "prompt.submit" {
			submits++
		} else if method != "prompt.receipt" {
			t.Fatalf("unexpected %s", method)
		}
	}
	if submits != 1 || !strings.Contains(out.String(), "API call failed") || !strings.Contains(errOut.String(), "session_id: stored") {
		t.Fatalf("submits %d stdout %q stderr %q", submits, out.String(), errOut.String())
	}
}

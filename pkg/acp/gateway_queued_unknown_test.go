package acp

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func bareGatewayAgent(t *testing.T, snapshots map[string]string) (*GatewayACPAgent, *scriptedGateway) {
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
	for sid, snapshot := range snapshots {
		a.snapshots.Set(sid, jsonObj(t, snapshot))
	}
	a.gateway = link
	a.loop.leave()
	return a, link
}

func projectJSON(t testing.TB, a *GatewayACPAgent, event string) {
	t.Helper()
	if err := onLoop(a, func() error { return a.project(context.Background(), jsonObj(t, event)) }); err != nil {
		t.Error(err)
	}
}

func promptOn(a *GatewayACPAgent, ctx context.Context, session, text string) *asked {
	q := &asked{done: make(chan struct{})}
	go func() {
		defer close(q.done)
		q.resp, q.err = a.Prompt(ctx, textPrompt(session, text))
	}()
	return q
}

func isUnknownExecution(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown_execution")
}

const unknownHeadEvent = `{"session_id": "s", "type": "session.info", "payload": {"pending": [{"admission_id": "head", "status": "unknown"}, {"admission_id": "ours", "status": "queued"}]}}`

func TestEditorFollowerReportsUnknownWithoutPoisoningOtherSessions(t *testing.T) {
	for _, beforeAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_ack", true: "before_ack"}[beforeAck], func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{}`, "other": `{}`})
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method != "prompt.submit" {
					t.Errorf("unexpected %s", method)
				}
				if params["session_id"] == "other" {
					projectJSON(t, a, `{"session_id": "other", "type": "message.complete", "admission_id": "other-turn", "payload": {"outcome": "completed"}}`)
					return jsonObj(t, `{"admission_id": "other-turn"}`), nil
				}
				if beforeAck {
					projectJSON(t, a, unknownHeadEvent)
				}
				return jsonObj(t, `{"admission_id": "ours"}`), nil
			})
			submitted := promptOn(a, context.Background(), "s", "queued")
			if !beforeAck {
				until(t, "the admission", func() bool { return onLoop(a, func() string { return a.admissions["s"] }) == "ours" })
				projectJSON(t, a, unknownHeadEvent)
			}
			_, err := submitted.wait(t, 5*time.Second)
			if err == nil || !strings.Contains(err.Error(), "unknown_execution: do not resend accepted input") {
				t.Fatalf("err = %v", err)
			}
			if calls := link.log(); len(calls) != 1 {
				t.Fatalf("calls = %v", calls)
			}
			if failure := onLoop(a, func() error { return a.failure }); failure != nil {
				t.Fatalf("failure = %v", failure)
			}
			resp, err := promptOn(a, context.Background(), "other", "independent").wait(t, 5*time.Second)
			if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("other = %+v, %v", resp, err)
			}
		})
	}
}

const lostPending = `[{"admission_id": "lost", "status": "unknown"}]`

func TestEditorRefusesNewPromptUntilLostTurnIsResolved(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": ` + lostPending + `}`})
	link.set(fixedReply(t, `{"pending": `+lostPending+`}`))
	_, err := promptOn(a, context.Background(), "s", "new input").wait(t, 5*time.Second)
	if !isUnknownExecution(err) {
		t.Fatalf("err = %v", err)
	}
	if calls := link.log(); !reflect.DeepEqual(calls, []rpcCall{{"session.resume", map[string]any{"session_id": "s"}}}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestEditorRechecksLostTurnAfterDiscardInAnotherViewer(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": ` + lostPending + `, "replay_epoch": "owner", "last_sequence": 1}`})
	var mu sync.Mutex
	ownerPending := lostPending
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "session.resume" {
			mu.Lock()
			defer mu.Unlock()
			return jsonObj(t, `{"pending": `+ownerPending+`, "replay_epoch": "owner", "last_sequence": 2}`), nil
		}
		if method != "prompt.submit" || params["session_id"] != "s" {
			t.Errorf("unexpected %s %v", method, params)
		}
		projectJSON(t, a, `{"session_id": "s", "type": "message.complete", "admission_id": "next", "payload": {"outcome": "completed"}}`)
		return jsonObj(t, `{"admission_id": "next"}`), nil
	})
	if _, err := promptOn(a, context.Background(), "s", "still blocked").wait(t, 5*time.Second); !isUnknownExecution(err) {
		t.Fatalf("err = %v", err)
	}
	if hasMethod(link.methods(), "prompt.submit") {
		t.Fatal("a blocked prompt was submitted")
	}
	mu.Lock()
	ownerPending = `[]`
	mu.Unlock()
	resp, err := promptOn(a, context.Background(), "s", "next prompt").wait(t, 5*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if countMethod(link.methods(), "prompt.submit") != 1 {
		t.Fatalf("methods = %v", link.methods())
	}
}

func hasMethod(methods []string, want string) bool { return countMethod(methods, want) > 0 }

func countMethod(methods []string, want string) int {
	n := 0
	for _, m := range methods {
		if m == want {
			n++
		}
	}
	return n
}

func ownerEvent(t *testing.T, seq int, kind, admission, payload string) *pyjson.Object {
	params := pyjson.ObjectOf("session_id", "s", "replay_epoch", "owner", "seq", int64(seq), "type", kind, "payload", jsonObj(t, payload))
	if admission != "" {
		params.Set("admission_id", admission)
	}
	return pyjson.ObjectOf("params", params)
}

func startPump(t *testing.T, a *GatewayACPAgent) {
	t.Helper()
	task := onLoop(a, func() *loopTask {
		return a.loop.createTask(a.ctx, func(ctx context.Context) (any, error) { return nil, a.events(ctx) }, nil)
	})
	t.Cleanup(func() {
		task.Cancel()
		<-task.done
	})
}

func lastAgentText(e *editorFake) string {
	text := ""
	for _, u := range updatesOf(e) {
		if chunk, ok := u.(*pyacp.AgentMessageChunk); ok {
			text = chunk.Content.(*pyacp.TextContentBlock).Text
		}
	}
	return text
}

func TestUnknownRefreshKeepsUnreadReplyEventsWithoutRestoringOldPending(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": ` + lostPending + `, "replay_epoch": "owner", "last_sequence": 1}`})
	editor := &editorFake{}
	approvalSeen, releaseApproval := make(chan struct{}), make(chan struct{})
	var once sync.Once
	onLoop(a, func() bool {
		a.conn = editor
		a.answerApproval = func(ctx context.Context, sessionID string, prompt *pyjson.Object) bool {
			once.Do(func() { close(approvalSeen) })
			a.loop.await(func() {
				select {
				case <-releaseApproval:
				case <-ctx.Done():
				}
			})
			return true
		}
		return true
	})
	t.Cleanup(func() { close(releaseApproval) })
	link.set(fixedReply(t, `{"pending": [], "replay_epoch": "owner", "last_sequence": 4}`))
	if err := onLoop(a, func() error { return a.refreshUnknown(context.Background(), "s") }); err != nil {
		t.Fatal(err)
	}
	if onLoop(a, func() bool { return a.hasUnknown("s") }) {
		t.Fatal("the lost turn survived the refresh")
	}
	link.put(ownerEvent(t, 2, "session.info", "", `{"pending": `+lostPending+`}`))
	link.put(ownerEvent(t, 3, "approval.request", "", `{"kind": "approval", "prompt_id": "approval-1", "execution_generation": 1}`))
	link.put(ownerEvent(t, 4, "message.delta", "follower", `{"text": "Follower answer"}`))
	startPump(t, a)
	select {
	case <-approvalSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("the buffered approval did not reach the editor")
	}
	until(t, "the follower reply", func() bool { return len(editor.seen()) == 1 })
	if lastAgentText(editor) != "Follower answer" {
		t.Fatalf("shown = %q", lastAgentText(editor))
	}
	if onLoop(a, func() bool { return a.hasUnknown("s") }) {
		t.Fatal("an old pending event restored the lost turn")
	}
}

func TestUnknownRefreshOrdersPendingStateIndependentlyOfReplyEvents(t *testing.T) {
	for _, newerPending := range []bool{false, true} {
		t.Run(map[bool]string{false: "older_pending", true: "newer_pending"}[newerPending], func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": ` + lostPending + `, "replay_epoch": "owner", "last_sequence": 1}`})
			editor := &editorFake{}
			onLoop(a, func() bool { a.conn = editor; return true })
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method == "session.resume" {
					once.Do(func() { close(entered) })
					<-release
					return jsonObj(t, `{"pending": [], "replay_epoch": "owner", "last_sequence": 3}`), nil
				}
				if method != "prompt.submit" {
					t.Errorf("unexpected %s", method)
				}
				projectJSON(t, a, `{"session_id": "s", "type": "message.complete", "admission_id": "next", "payload": {"outcome": "completed"}}`)
				return jsonObj(t, `{"admission_id": "next"}`), nil
			})
			startPump(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prompt := promptOn(a, ctx, "s", "next prompt")
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the refresh did not start")
			}
			link.put(ownerEvent(t, 2, "session.info", "", `{"pending": `+lostPending+`}`))
			link.put(ownerEvent(t, 4, "message.delta", "follower", `{"text": "Follower answer"}`))
			want := int64(4)
			if newerPending {
				link.put(ownerEvent(t, 5, "session.info", "", `{"pending": [{"admission_id": "follower", "status": "unknown"}]}`))
				want = 5
			}
			until(t, "the follower events", func() bool {
				return onLoop(a, func() any {
					snapshot, _ := a.snapshots.Get("s")
					return dget(snapshot, "last_sequence")
				}) == want
			})
			if lastAgentText(editor) != "Follower answer" {
				t.Fatalf("shown = %q", lastAgentText(editor))
			}
			releaseOnce.Do(func() { close(release) })
			resp, err := prompt.wait(t, 5*time.Second)
			if newerPending {
				if !isUnknownExecution(err) {
					t.Fatalf("err = %v", err)
				}
			} else if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			wantSubmits := 1
			if newerPending {
				wantSubmits = 0
			}
			if got := countMethod(link.methods(), "prompt.submit"); got != wantSubmits {
				t.Fatalf("submits = %d", got)
			}
		})
	}
}

func TestStopDuringUnknownRefreshCancelsTheUnsentPrompt(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": ` + lostPending + `}`})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "session.resume" {
			once.Do(func() { close(entered) })
			<-release
			return jsonObj(t, `{"pending": []}`), nil
		}
		if method != "prompt.submit" {
			t.Errorf("unexpected %s", method)
		}
		projectJSON(t, a, `{"session_id": "s", "type": "message.complete", "admission_id": "next", "payload": {"outcome": "completed"}}`)
		return jsonObj(t, `{"admission_id": "next"}`), nil
	})
	prompt := promptOn(a, context.Background(), "s", "new input")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh did not start")
	}
	if err := a.Cancel(context.Background(), &pyacp.CancelNotification{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	resp, err := prompt.wait(t, 5*time.Second)
	if err != nil || resp.StopReason != "cancelled" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if methods := link.methods(); !reflect.DeepEqual(methods, []string{"session.resume"}) {
		t.Fatalf("methods = %v", methods)
	}
}

const lostAdmission = "admission-lost-0123456789abcdef"

func TestResumedUnknownTurnNamesTheExistingDiscardRecovery(t *testing.T) {
	a, link := bareGatewayAgent(t, nil)
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	t.Cleanup(a.Close)
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "session.info", "config.set":
			return pyjson.NewObject(), nil
		case "session.resume":
			return jsonObj(t, `{"session_id": "s", "messages": [], "prompts": [], "pending": [{"admission_id": "`+lostAdmission+`", "status": "unknown", "execution_generation": 4}]}`), nil
		}
		return nil, context.Canceled
	})
	held, _ := pyacp.WithResponseScope(context.Background())
	if _, err := a.LoadSession(held, &pyacp.LoadSessionRequest{Cwd: "/", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	notice := lastAgentText(editor)
	_, err := promptOn(a, context.Background(), "s", "new input").wait(t, 5*time.Second)
	if err == nil {
		t.Fatal("the prompt was not refused")
	}
	for _, text := range []string{notice, err.Error()} {
		if !strings.Contains(text, "sessions discard s") || !strings.Contains(text, "/discard "+lostAdmission) || !strings.Contains(text, "do not resend") {
			t.Fatalf("text = %q", text)
		}
	}
	if !strings.Contains(err.Error(), "nothing was submitted") {
		t.Fatalf("refusal = %q", err.Error())
	}
	if asked := link.methods(); !reflect.DeepEqual(asked, []string{"session.info", "session.resume", "config.set", "session.resume"}) {
		t.Fatalf("asked = %v", asked)
	}
}

func TestResumedUnknownTurnCanBeDiscardedWithoutLeavingTheEditor(t *testing.T) {
	unknown := `{"session_id": "s", "messages": [], "prompts": [], "pending": [{"admission_id": "` + lostAdmission + `", "status": "unknown", "execution_generation": 4}]}`
	cleared := `{"session_id": "s", "messages": [], "prompts": [], "pending": []}`
	a, link := bareGatewayAgent(t, map[string]string{"s": unknown})
	var mu sync.Mutex
	resolved := 0
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if method == "prompt.resolve_unknown" {
			resolved++
			return jsonObj(t, `{"admission_id": "`+lostAdmission+`", "status": "terminal", "outcome": "interrupted"}`), nil
		}
		if resolved > 0 {
			return jsonObj(t, cleared), nil
		}
		return jsonObj(t, unknown), nil
	})
	resp, err := promptOn(a, context.Background(), "s", "/discard "+lostAdmission).wait(t, 5*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	pending := onLoop(a, func() any {
		snapshot, _ := a.snapshots.Get("s")
		return dget(snapshot, "pending")
	})
	if items, ok := pending.([]any); !ok || len(items) != 0 {
		t.Fatalf("pending = %#v", pending)
	}
	want := []rpcCall{
		{"session.resume", map[string]any{"session_id": "s"}},
		{"session.resume", map[string]any{"session_id": "s"}},
		{"prompt.resolve_unknown", map[string]any{"session_id": "s", "admission_id": lostAdmission, "execution_generation": int64(4)}},
		{"session.resume", map[string]any{"session_id": "s"}},
	}
	if calls := link.log(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestEditorOwnLostTurnReportsUnknownNotEndTurn(t *testing.T) {
	for _, yieldBetween := range []bool{false, true} {
		t.Run(map[bool]string{false: "together", true: "info_first"}[yieldBetween], func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
			link.set(fixedReply(t, `{"admission_id": "ours"}`))
			submitted := promptOn(a, context.Background(), "s", "work")
			until(t, "the admission", func() bool { return onLoop(a, func() string { return a.admissions["s"] }) == "ours" })
			projectJSON(t, a, `{"session_id": "s", "type": "session.info", "payload": {"pending": [{"admission_id": "ours", "status": "unknown"}]}}`)
			if yieldBetween {
				until(t, "the prompt woke on session.info", submitted.finished)
			}
			projectJSON(t, a, `{"session_id": "s", "type": "message.complete", "admission_id": "ours", "payload": {"outcome": "unknown", "text": "Worker execution is unknown."}}`)
			if _, err := submitted.wait(t, 5*time.Second); !isUnknownExecution(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

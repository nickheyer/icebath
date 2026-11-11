package acp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type rpcFunc func(method string, params map[string]any) (*pyjson.Object, error)

func cmdAgent(t *testing.T, rpc func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error)) (*GatewayACPAgent, *fakeGateway, *editorFake) {
	t.Helper()
	a := newGatewayAgent(t)
	link := newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		return rpc(a, method, params)
	})
	editor := &editorFake{}
	a.loop.enter()
	a.snapshots.Set("s", jsonObj(t, `{"execution_generation": 7}`))
	a.gateway, a.conn = link, editor
	a.loop.leave()
	return a, link, editor
}

func promptWithin(t *testing.T, a *GatewayACPAgent, text string) (*pyacp.PromptResponse, error) {
	t.Helper()
	type outcome struct {
		resp *pyacp.PromptResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := a.Prompt(context.Background(), textPrompt("s", text))
		done <- outcome{resp, err}
	}()
	select {
	case out := <-done:
		return out.resp, out.err
	case <-time.After(5 * time.Second):
		t.Fatalf("prompt %q did not answer", text)
		return nil, nil
	}
}

func completeTurn(t *testing.T, a *GatewayACPAgent) {
	t.Helper()
	event := jsonObj(t, `{"session_id": "s", "type": "message.complete", "admission_id": "a", "payload": {"outcome": "completed", "text": "done"}}`)
	if err := onLoop(a, func() error { return a.project(context.Background(), event) }); err != nil {
		t.Error(err)
	}
}

func lastText(e *editorFake) string {
	seen := e.seen()
	if len(seen) == 0 {
		return ""
	}
	if chunk, ok := seen[len(seen)-1].update.(*pyacp.AgentMessageChunk); ok {
		return chunk.Content.(*pyacp.TextContentBlock).Text
	}
	return ""
}

func receiptOf(t *testing.T, status string, generation any) *pyjson.Object {
	return pyjson.ObjectOf("admission_id", "mine", "status", status, "execution_generation", generation)
}

type methodLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *methodLog) add(method string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, method)
}

func (l *methodLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func TestQueueSubmitsItsTextAsTheNextPrompt(t *testing.T) {
	a, _, _ := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		if method != "prompt.submit" || params["text"] != "run the linter" {
			t.Errorf("unexpected %s %v", method, params)
		}
		completeTurn(t, a)
		return jsonObj(t, `{"admission_id": "a"}`), nil
	})
	resp, err := promptWithin(t, a, "/queue run the linter")
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

func TestSteerReachesThisEditorsRunningTurn(t *testing.T) {
	a, _, editor := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "prompt.receipt" {
			if !reflect.DeepEqual(params, map[string]any{"session_id": "s", "admission_id": "mine"}) {
				t.Errorf("receipt params %v", params)
			}
			return receiptOf(t, "started", int64(9)), nil
		}
		if method != "session.steer" || !reflect.DeepEqual(params, map[string]any{"session_id": "s", "text": "use pytest", "execution_generation": int64(9)}) {
			t.Errorf("unexpected %s %v", method, params)
		}
		return jsonObj(t, `{"status": "queued", "text": "use pytest", "execution_generation": 9, "authority_epoch": 1}`), nil
	})
	onLoop(a, func() bool { a.admissions["s"] = "mine"; return true })
	resp, err := promptWithin(t, a, "/steer use pytest")
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if !strings.Contains(lastText(editor), "Steer queued for the active turn: use pytest") {
		t.Fatalf("reply = %q", lastText(editor))
	}
}

func TestASteerThatEndedTheVoiceChatIsReportedAndNeverResentAsAPrompt(t *testing.T) {
	calls := &methodLog{}
	a, _, editor := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		calls.add(method)
		if method == "prompt.receipt" {
			return receiptOf(t, "started", int64(9)), nil
		}
		return jsonObj(t, `{"voice_stopped": true}`), nil
	})
	onLoop(a, func() bool { a.admissions["s"] = "mine"; return true })
	resp, err := promptWithin(t, a, "/steer stop")
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if !reflect.DeepEqual(calls.list(), []string{"prompt.receipt", "session.steer"}) {
		t.Fatalf("calls = %v", calls.list())
	}
	if !strings.Contains(lastText(editor), i18n.T(context.Background(), "cli.voice.stop_phrase_typed", "", nil)) {
		t.Fatalf("reply = %q", lastText(editor))
	}
}

func TestSteerWhileThisEditorsPromptWaitsBehindAnotherClientsTurnIsQueued(t *testing.T) {
	calls := &methodLog{}
	a, _, _ := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		calls.add(method)
		if method == "prompt.receipt" {
			return receiptOf(t, "queued", nil), nil
		}
		if method != "prompt.submit" || params["text"] != "use pytest" {
			t.Errorf("unexpected %s %v", method, params)
		}
		completeTurn(t, a)
		return jsonObj(t, `{"admission_id": "a"}`), nil
	})
	onLoop(a, func() bool { a.admissions["s"] = "mine"; return true })
	resp, err := promptWithin(t, a, "/steer use pytest")
	if err != nil || resp.StopReason != "end_turn" || !reflect.DeepEqual(calls.list(), []string{"prompt.receipt", "prompt.submit"}) {
		t.Fatalf("resp = %+v, %v, calls %v", resp, err, calls.list())
	}
}

func TestARefusedSteerIsReportedAndNeverResentAsAPrompt(t *testing.T) {
	calls := &methodLog{}
	a, _, _ := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		calls.add(method)
		if method == "prompt.receipt" {
			return receiptOf(t, "started", int64(7)), nil
		}
		return nil, &gateway.GatewayRPCError{Msg: "unsupported_control"}
	})
	onLoop(a, func() bool { a.admissions["s"] = "mine"; return true })
	_, err := promptWithin(t, a, "/steer use pytest")
	var refused *gateway.GatewayRPCError
	if !errors.As(err, &refused) || refused.Msg != "unsupported_control" {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(calls.list(), []string{"prompt.receipt", "session.steer"}) {
		t.Fatalf("calls = %v", calls.list())
	}
}

func TestASteerForATurnThatJustEndedBecomesTheNextPrompt(t *testing.T) {
	calls := &methodLog{}
	a, _, _ := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		calls.add(method)
		switch method {
		case "prompt.receipt":
			return receiptOf(t, "started", int64(7)), nil
		case "session.steer":
			return nil, &gateway.GatewayRPCError{Msg: "stale_generation"}
		}
		if method != "prompt.submit" || params["text"] != "use pytest" {
			t.Errorf("unexpected %s %v", method, params)
		}
		completeTurn(t, a)
		return jsonObj(t, `{"admission_id": "a"}`), nil
	})
	onLoop(a, func() bool { a.admissions["s"] = "mine"; return true })
	resp, err := promptWithin(t, a, "/steer use pytest")
	if err != nil || resp.StopReason != "end_turn" || !reflect.DeepEqual(calls.list(), []string{"prompt.receipt", "session.steer", "prompt.submit"}) {
		t.Fatalf("resp = %+v, %v, calls %v", resp, err, calls.list())
	}
}

type sentMutation struct {
	operation  string
	payload    any
	generation any
}

func TestModesAndTheConfigOptionSetTheOwnersEditPolicy(t *testing.T) {
	var mu sync.Mutex
	var sent []sentMutation
	a, _, _ := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "session.resume":
			return jsonObj(t, `{"revision": 3, "execution_generation": 7}`), nil
		case "session.info":
			return pyjson.NewObject(), nil
		}
		if method != "session.mutate" {
			t.Errorf("unexpected %s", method)
		}
		mu.Lock()
		sent = append(sent, sentMutation{pyStr(params["operation"]), pyjson.Plain(params["payload"]), params["expected_generation"]})
		mu.Unlock()
		return jsonObj(t, `{"session_id": "s", "operation": "edit_policy", "revision": 4}`), nil
	})
	ctx := context.Background()
	if _, err := a.SetSessionMode(ctx, &pyacp.SetSessionModeRequest{ModeID: "accept_edits", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetConfigOption(ctx, &pyacp.SetConfigOptionParams{ConfigID: "edit_approval_policy", SessionID: "s", Value: "session"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetSessionMode(ctx, &pyacp.SetSessionModeRequest{ModeID: "default", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	want := []sentMutation{
		{"edit_policy", map[string]any{"policy": "workspace_session"}, int64(7)},
		{"edit_policy", map[string]any{"policy": "session"}, int64(7)},
		{"edit_policy", map[string]any{"policy": "ask"}, int64(7)},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v", sent)
	}
	sent = nil
	_, err := a.SetSessionMode(ctx, &pyacp.SetSessionModeRequest{ModeID: "yolo", SessionID: "s"})
	if !refusedWith(err, "invalid_params") {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.SetConfigOption(ctx, &pyacp.SetConfigOptionParams{ConfigID: "theme", SessionID: "s", Value: "dark"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent = %v", sent)
	}
}

func TestTheEditorsOwnPickProjectedBeforeTheMutationAnswersIsHarmless(t *testing.T) {
	a, _, editor := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "session.resume" {
			return jsonObj(t, `{"revision": 3, "execution_generation": 7}`), nil
		}
		policy := pyjson.Plain(params["payload"]).(map[string]any)["policy"]
		event := pyjson.ObjectOf("session_id", "s", "type", "session.info", "payload", pyjson.ObjectOf("edit_approval_policy", policy))
		if err := onLoop(a, func() error { return a.project(context.Background(), event) }); err != nil {
			t.Error(err)
		}
		return jsonObj(t, `{"session_id": "s", "operation": "edit_policy", "revision": 4}`), nil
	})
	onLoop(a, func() bool { a.modes["s"] = "default"; return true })
	if _, err := a.SetSessionMode(context.Background(), &pyacp.SetSessionModeRequest{ModeID: "dont_ask", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	shown := updatesOf(editor)
	if len(shown) != 1 {
		t.Fatalf("shown = %v", shown)
	}
	if update, ok := shown[0].(*pyacp.CurrentModeUpdate); !ok || update.CurrentModeID != "dont_ask" {
		t.Fatalf("shown = %#v", shown[0])
	}
	mode, failure := onLoop(a, func() string { return a.modes["s"] }), onLoop(a, func() error { return a.failure })
	if mode != "dont_ask" || failure != nil {
		t.Fatalf("mode %q failure %v", mode, failure)
	}
}

func anyIdleNotice(e *editorFake) bool {
	for _, u := range updatesOf(e) {
		if chunk, ok := u.(*pyacp.AgentMessageChunk); ok && strings.Contains(chunk.Content.(*pyacp.TextContentBlock).Text, "idle") {
			return true
		}
	}
	return false
}

func TestAModePickDuringATurnIsRefusedWithTheReason(t *testing.T) {
	picks := map[string]func(a *GatewayACPAgent) error{
		"set_session_mode": func(a *GatewayACPAgent) error {
			_, err := a.SetSessionMode(context.Background(), &pyacp.SetSessionModeRequest{ModeID: "dont_ask", SessionID: "s"})
			return err
		},
		"set_config_option": func(a *GatewayACPAgent) error {
			_, err := a.SetConfigOption(context.Background(), &pyacp.SetConfigOptionParams{ConfigID: "edit_approval_policy", SessionID: "s", Value: "session"})
			return err
		},
	}
	for name, pick := range picks {
		t.Run(name, func(t *testing.T) {
			a, _, editor := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
				if method == "session.resume" {
					return jsonObj(t, `{"revision": 3, "execution_generation": 7}`), nil
				}
				return nil, &gateway.GatewayRPCError{Msg: "session_busy"}
			})
			onLoop(a, func() bool { a.modes["s"] = "default"; return true })
			err := pick(a)
			var refused *pyacp.RequestError
			if !errors.As(err, &refused) || !strings.Contains(refused.Message, "idle") {
				t.Fatalf("err = %v", err)
			}
			if !anyIdleNotice(editor) {
				t.Fatalf("the editor was not told why: %v", updatesOf(editor))
			}
			if mode := onLoop(a, func() string { return a.modes["s"] }); mode != "default" {
				t.Fatalf("mode = %q", mode)
			}
		})
	}
}

func TestAModePickBehindALostTurnSaysToResolveItNotToWait(t *testing.T) {
	a, _, _ := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "session.resume" {
			return jsonObj(t, `{"revision": 3, "execution_generation": 7}`), nil
		}
		return nil, &gateway.GatewayRPCError{Msg: "unknown_execution"}
	})
	onLoop(a, func() bool {
		snapshot, _ := a.snapshots.Get("s")
		snapshot.Set("pending", []any{jsonObj(t, `{"admission_id": "lost-1", "status": "unknown"}`)})
		return true
	})
	_, err := a.SetSessionMode(context.Background(), &pyacp.SetSessionModeRequest{ModeID: "dont_ask", SessionID: "s"})
	var refused *pyacp.RequestError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(refused.Message, "after this turn") || !strings.Contains(refused.Message, "/discard lost-1") {
		t.Fatalf("text = %q", refused.Message)
	}
}

type appliedMutations struct{ result map[string]any }

func (m *appliedMutations) Apply(ctx context.Context, rpc MutationRPC, sessionID, operation string, payload any, confirm MutationConfirm) (map[string]any, error) {
	return m.result, nil
}
func (m *appliedMutations) Acknowledge(sessionID, operation string, payload any) {}
func (m *appliedMutations) Retire(sessionID string)                              {}

func contextShown(e *editorFake) [][2]int {
	var out [][2]int
	for _, u := range updatesOf(e) {
		if usage, ok := u.(*pyacp.UsageUpdate); ok {
			out = append(out, [2]int{usage.Size, usage.Used})
		}
	}
	return out
}

func boundaryAgent(t *testing.T, messages string) (*GatewayACPAgent, *editorFake) {
	a, _, editor := cmdAgent(t, func(a *GatewayACPAgent, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "session.resume" {
			return jsonObj(t, `{"execution_generation": 8, "messages": `+messages+`}`), nil
		}
		if method != "session.stats" {
			t.Errorf("unexpected %s", method)
		}
		return jsonObj(t, `{"input_tokens": 0, "context_tokens": 0, "context_length": null}`), nil
	})
	onLoop(a, func() bool {
		a.usage["s"] = jsonObj(t, `{"context_length": 200000, "context_tokens": 150000}`)
		a.mutations = &appliedMutations{result: map[string]any{"status": "applied", "summary": map[string]any{}}}
		return true
	})
	return a, editor
}

func TestResetEmptiesTheEditorsContextIndicator(t *testing.T) {
	a, editor := boundaryAgent(t, `[]`)
	if _, err := promptWithin(t, a, "/reset"); err != nil {
		t.Fatal(err)
	}
	if shown := contextShown(editor); !reflect.DeepEqual(shown, [][2]int{{200000, 0}}) {
		t.Fatalf("context shown = %v", shown)
	}
}

func TestCompressShowsTheCompressedTranscriptsOccupancy(t *testing.T) {
	content := strings.Repeat("summary of the earlier turns ", 40)
	kept := []map[string]any{{"role": "user", "content": content}}
	dumped, _ := pyjson.Dumps([]any{pyjson.ObjectOf("role", "user", "content", content)})
	a, editor := boundaryAgent(t, dumped)
	if _, err := promptWithin(t, a, "/compress"); err != nil {
		t.Fatal(err)
	}
	want := estimateRequestTokensRough(context.Background(), kept, "", nil)
	if shown := contextShown(editor); want <= 0 || !reflect.DeepEqual(shown, [][2]int{{200000, want}}) {
		t.Fatalf("context shown = %v, want used %d", shown, want)
	}
}

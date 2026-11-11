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
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func agentWithAdmission(t *testing.T) (*GatewayACPAgent, *scriptedGateway, *editorFake) {
	t.Helper()
	a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
	link.set(fixedReply(t, `{"admission_id": "current"}`))
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	return a, link, editor
}

func completeAdmission(t *testing.T, a *GatewayACPAgent, admission, outcome, text string) {
	t.Helper()
	event := pyjson.ObjectOf("session_id", "s", "type", "message.complete", "admission_id", admission, "payload", pyjson.ObjectOf("outcome", outcome, "text", text))
	if err := onLoop(a, func() error { return a.project(context.Background(), event) }); err != nil {
		t.Error(err)
	}
}

func emittedText(e *editorFake) []string {
	var out []string
	for _, u := range updatesOf(e) {
		if chunk, ok := u.(*pyacp.AgentMessageChunk); ok {
			out = append(out, chunk.Content.(*pyacp.TextContentBlock).Text)
		}
	}
	return out
}

func TestCancelledAdmissionSuppressesOnlyInterruptMetadata(t *testing.T) {
	for _, tc := range []struct{ text, visible string }{
		{"Operation interrupted: waiting for model response (0.3s elapsed).", ""},
		{"Partial answer", "Partial answer"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			a, _, editor := agentWithAdmission(t)
			completeAdmission(t, a, "other", "completed", "")
			task := promptOn(a, context.Background(), "s", "hello")
			until(t, "the admission", func() bool { return onLoop(a, func() string { return a.admissions["s"] }) == "current" })
			if task.finished() {
				t.Fatal("the prompt ended on another admission's completion")
			}
			completeAdmission(t, a, "current", "cancelled", tc.text)
			resp, err := task.wait(t, 2*time.Second)
			if err != nil || resp.StopReason != "cancelled" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			if emitted := strings.Join(emittedText(editor), ""); emitted != tc.visible {
				t.Fatalf("emitted = %q", emitted)
			}
			other, current := onLoop(a, func() bool { return a.terminals.Has("other") }), onLoop(a, func() bool { return a.terminals.Has("current") })
			if !other || current {
				t.Fatalf("terminals other %v current %v", other, current)
			}
		})
	}
}

func TestRefusedReattachAfterAReplayGapStillFailsThePump(t *testing.T) {
	a, link, _ := agentWithAdmission(t)
	onLoop(a, func() bool {
		a.snapshots.Set("s", jsonObj(t, `{"replay_epoch": "e", "last_sequence": 9}`))
		return true
	})
	link.put(jsonObj(t, `{"method": "event", "params": {"session_id": "s", "type": "session.replay_gap", "payload": {"reason": "subscriber_overflow"}}}`))
	link.set(func(context.Context, string, map[string]any) (*pyjson.Object, error) {
		return nil, &gateway.GatewayRPCError{Msg: "not_found"}
	})
	done := make(chan error, 1)
	go func() { done <- onLoop(a, func() error { return a.events(context.Background()) }) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the pump kept running")
	}
	var refused *gateway.GatewayRPCError
	if failure := onLoop(a, func() error { return a.failure }); !errors.As(failure, &refused) || refused.Msg != "not_found" {
		t.Fatalf("failure = %v", failure)
	}
}

func TestFailedAdmissionRaisesWithoutPoisoningTheNextSuccess(t *testing.T) {
	a, _, editor := agentWithAdmission(t)
	completeAdmission(t, a, "current", "failed", "The admitted turn failed.")
	if _, err := promptOn(a, context.Background(), "s", "hello").wait(t, 2*time.Second); !refusedWith(err, "admitted_turn_failed") {
		t.Fatalf("err = %v", err)
	}
	if onLoop(a, func() bool { return a.terminals.Has("current") }) {
		t.Fatal("the failed terminal was kept")
	}
	completeAdmission(t, a, "current", "completed", "Operation interrupted: quoted model text")
	resp, err := promptOn(a, context.Background(), "s", "retry").wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	texts := emittedText(editor)
	if len(texts) == 0 || texts[len(texts)-1] != "Operation interrupted: quoted model text" {
		t.Fatalf("emitted = %v", texts)
	}
}

func receiptAgent(t *testing.T, final string, gate func(a *GatewayACPAgent)) (*GatewayACPAgent, *editorFake) {
	t.Helper()
	a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "prompt.submit" {
			return jsonObj(t, `{"admission_id": "a1", "status": "terminal", "outcome": "completed"}`), nil
		}
		if method != "prompt.receipt" {
			t.Errorf("unexpected %s", method)
		}
		if gate != nil {
			gate(a)
		}
		return pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", final)), nil
	})
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	return a, editor
}

func TestLiveCompletionDuringTheReceiptEditorUpdateIsNotShownTwice(t *testing.T) {
	a, editor := receiptAgent(t, "Final answer", nil)
	var mu sync.Mutex
	count := 0
	editor.onUpdate = func(string, pyacp.SessionUpdate) error {
		mu.Lock()
		count++
		first := count == 1
		mu.Unlock()
		if first {
			completeAdmission(t, a, "a1", "completed", "Final answer")
		}
		return nil
	}
	resp, err := promptOn(a, context.Background(), "s", "hello").wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if shown := emittedText(editor); !reflect.DeepEqual(shown, []string{"Final answer"}) {
		t.Fatalf("shown = %v", shown)
	}
	completeAdmission(t, a, "a1", "completed", "Final answer")
	if shown := emittedText(editor); !reflect.DeepEqual(shown, []string{"Final answer"}) {
		t.Fatalf("shown = %v", shown)
	}
}

func TestReceiptShowsOnlyWhatTheLiveStreamDidNot(t *testing.T) {
	a, editor := receiptAgent(t, "Hello world", nil)
	projectJSON(t, a, `{"session_id": "s", "type": "message.delta", "admission_id": "a1", "payload": {"text": "Hello "}}`)
	resp, err := promptOn(a, context.Background(), "s", "hello").wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if shown := emittedText(editor); !reflect.DeepEqual(shown, []string{"Hello ", "world"}) {
		t.Fatalf("shown = %v", shown)
	}
}

func TestCompletionThatLandsBeforeTheReceiptIsTheOneShown(t *testing.T) {
	a, editor := receiptAgent(t, "Final answer", func(a *GatewayACPAgent) { completeAdmission(t, a, "a1", "completed", "Final answer") })
	resp, err := promptOn(a, context.Background(), "s", "hello").wait(t, 2*time.Second)
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if shown := emittedText(editor); !reflect.DeepEqual(shown, []string{"Final answer"}) {
		t.Fatalf("shown = %v", shown)
	}
}

func configAgent(t *testing.T, modelState func(ctx context.Context, info *pyjson.Object) *pyacp.SessionModelState) (*GatewayACPAgent, *editorFake) {
	t.Helper()
	swap(t, &gatewayModelState, modelState)
	a := newGatewayAgent(t)
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	return a, editor
}

func projectModeOf(t *testing.T, a *GatewayACPAgent, info string) {
	t.Helper()
	if err := onLoop(a, func() error { return a.projectMode(context.Background(), "s", "s", jsonObj(t, info)) }); err != nil {
		t.Error(err)
	}
}

func shownConfigs(e *editorFake) []map[string]string {
	var out []map[string]string
	for _, u := range updatesOf(e) {
		update, ok := u.(*pyacp.ConfigOptionUpdate)
		if !ok {
			continue
		}
		row := map[string]string{}
		for _, option := range update.ConfigOptions {
			sel := option.(*pyacp.SessionConfigOptionSelect)
			row[*sel.Category] = sel.CurrentValue
		}
		out = append(out, row)
	}
	return out
}

func TestAnotherSurfacesModeAndEffortChangeResendTheConfigOptions(t *testing.T) {
	a, editor := configAgent(t, func(context.Context, *pyjson.Object) *pyacp.SessionModelState { return nil })
	projectModeOf(t, a, `{"edit_approval_policy": "ask", "model": "fixture", "reasoning_effort": "medium"}`)
	projectModeOf(t, a, `{"edit_approval_policy": "session", "model": "fixture", "reasoning_effort": "medium"}`)
	projectModeOf(t, a, `{"reasoning_effort": "high"}`)
	want := []map[string]string{{"mode": "session", "thought_level": "medium"}, {"mode": "session", "thought_level": "high"}}
	if shown := shownConfigs(editor); !reflect.DeepEqual(shown, want) {
		t.Fatalf("shown = %v", shown)
	}
}

func heldModelState() (func(ctx context.Context, info *pyjson.Object) *pyacp.SessionModelState, <-chan struct{}, func()) {
	building, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	state := func(context.Context, *pyjson.Object) *pyacp.SessionModelState {
		first := false
		once.Do(func() { first = true; close(building) })
		if first {
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
		}
		return nil
	}
	return state, building, func() { releaseOnce.Do(func() { close(release) }) }
}

func TestAProjectionSupersededDuringItsCatalogBuildNeverLandsAfterTheNewerOne(t *testing.T) {
	state, building, release := heldModelState()
	a, editor := configAgent(t, state)
	defer release()
	info := `{"edit_approval_policy": "session", "model": "fixture", "reasoning_effort": "medium"}`
	projectModeOf(t, a, info)
	older := make(chan struct{})
	go func() {
		defer close(older)
		projectModeOf(t, a, `{"edit_approval_policy": "ask", "model": "fixture", "reasoning_effort": "medium"}`)
	}()
	<-building
	if err := onLoop(a, func() error { return a.adopt(context.Background(), "s", jsonObj(t, `{"info": `+info+`}`), nil) }); err != nil {
		t.Fatal(err)
	}
	release()
	<-older
	shown := shownConfigs(editor)
	if len(shown) == 0 || shown[len(shown)-1]["mode"] != "session" {
		t.Fatalf("modes = %v", shown)
	}
}

func TestAnEqualAdoptionDuringTheCatalogBuildDoesNotSwallowTheUpdate(t *testing.T) {
	state, building, release := heldModelState()
	a, editor := configAgent(t, func(context.Context, *pyjson.Object) *pyacp.SessionModelState { return nil })
	defer release()
	projectModeOf(t, a, `{"edit_approval_policy": "ask", "model": "fixture", "reasoning_effort": "medium"}`)
	swap(t, &gatewayModelState, state)
	changed := `{"edit_approval_policy": "session", "model": "fixture", "reasoning_effort": "medium"}`
	event := make(chan struct{})
	go func() {
		defer close(event)
		projectModeOf(t, a, changed)
	}()
	<-building
	if err := onLoop(a, func() error { return a.adopt(context.Background(), "s", jsonObj(t, `{"info": `+changed+`}`), nil) }); err != nil {
		t.Fatal(err)
	}
	release()
	<-event
	var modes []string
	for _, row := range shownConfigs(editor) {
		modes = append(modes, row["mode"])
	}
	if !reflect.DeepEqual(modes, []string{"session"}) {
		t.Fatalf("modes = %v", modes)
	}
}

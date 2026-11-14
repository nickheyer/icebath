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

const toolResult = `{"content": "owned line", "total_lines": 1}`

func dumpUpdates(t *testing.T, updates []pyacp.SessionUpdate) []string {
	t.Helper()
	var out []string
	for _, u := range updates {
		text, err := pyjson.Dumps(pyacp.Dump(u, pyacp.DumpOptions{ByAlias: true, ExcludeNone: true}))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, text)
	}
	return out
}

func TestLiveToolEventsProjectExactlyLikeReplay(t *testing.T) {
	a, _, editor := mappingAgent(t, `{}`)
	projectEvent(t, a, "tool.start", `{"tool_call_id": "call_1", "tool_name": "read_file", "args": {"path": "/tmp/owned/notes.txt"}}`, "a")
	result, _ := pyjson.Dumps(toolResult)
	projectEvent(t, a, "tool.complete", `{"tool_call_id": "call_1", "tool_name": "read_file", "is_error": false, "args": {"path": "/tmp/owned/notes.txt"}, "result": `+result+`}`, "a")
	live := updatesOf(editor)
	if len(live) != 2 {
		t.Fatalf("live = %v", live)
	}
	start, okStart := live[0].(*pyacp.ToolCallStart)
	complete, okComplete := live[1].(*pyacp.ToolCallProgress)
	if !okStart || !okComplete {
		t.Fatalf("live types %T %T", live[0], live[1])
	}
	history := []map[string]any{
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": `{"path": "/tmp/owned/notes.txt"}`}}}},
		{"role": "tool", "tool_call_id": "call_1", "tool_name": "read_file", "content": toolResult},
	}
	if got, want := dumpUpdates(t, live), dumpUpdates(t, historyReplayUpdates(history)); !reflect.DeepEqual(got, want) {
		t.Fatalf("live\n%v\nreplay\n%v", got, want)
	}
	if start.ToolCallID != "call_1" || len(start.Locations) == 0 || start.Locations[0].Path != "/tmp/owned/notes.txt" {
		t.Fatalf("start = %+v", start)
	}
	if complete.Status == nil || *complete.Status != "completed" {
		t.Fatalf("complete = %+v", complete)
	}
}

const (
	commentaryText = "Let me check the file. "
	finalText      = "FINAL-ANSWER-OMEGA"
)

var deliveryShapes = map[string][][2]string{
	"interim_only": {{"message.interim", `{"text": "FINAL-ANSWER-OMEGA", "already_streamed": false}`},
		{"message.complete", `{"outcome": "completed", "text": ""}`}},
	"streamed_null_completion": {{"message.delta", `{"text": "FINAL-ANSWER-OMEGA"}`},
		{"message.complete", `{"outcome": "completed", "text": null}`}},
	"commentary_tool_final": {{"message.delta", `{"text": "Let me check the file. "}`},
		{"tool.start", `{"tool_call_id": "t1", "tool_name": "terminal", "args": {}}`},
		{"tool.complete", `{"tool_call_id": "t1", "tool_name": "terminal", "result": "ok"}`},
		{"message.delta", `{"text": "FINAL-ANSWER-OMEGA"}`},
		{"message.complete", `{"outcome": "completed", "text": "FINAL-ANSWER-OMEGA"}`}},
	"reused_after_tool": {{"message.delta", `{"text": "FINAL-ANSWER-OMEGA"}`},
		{"tool.start", `{"tool_call_id": "t1", "tool_name": "memory", "args": {}}`},
		{"tool.complete", `{"tool_call_id": "t1", "tool_name": "memory", "result": "ok"}`},
		{"message.complete", `{"outcome": "completed", "text": "FINAL-ANSWER-OMEGA", "response_reused": true}`}},
}

func TestEachDeliveryShapeReachesTheEditorOnceAndThePumpSurvives(t *testing.T) {
	for shape, frames := range deliveryShapes {
		t.Run(shape, func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{"replay_epoch": "e", "last_sequence": 0}`})
			editor := &editorFake{}
			onLoop(a, func() bool { a.conn = editor; return true })
			startPump(t, a)
			seq := 0
			rounds := []struct {
				admission string
				frames    [][2]string
				prompt    string
			}{{"a1", frames, "first"}, {"a2", [][2]string{{"message.complete", `{"outcome": "completed", "text": "SECOND"}`}}, "second"}}
			for _, round := range rounds {
				editor.reset()
				link.set(fixedReply(t, `{"admission_id": "`+round.admission+`"}`))
				for _, f := range round.frames {
					seq++
					link.put(pyjson.ObjectOf("params", pyjson.ObjectOf("session_id", "s", "type", f[0], "admission_id", round.admission,
						"replay_epoch", "e", "seq", int64(seq), "payload", jsonObj(t, f[1]))))
				}
				resp, err := promptOn(a, context.Background(), "s", round.prompt).wait(t, 5*time.Second)
				if err != nil || resp.StopReason != "end_turn" {
					t.Fatalf("resp = %+v, %v", resp, err)
				}
				if failure := onLoop(a, func() error { return a.failure }); failure != nil {
					t.Fatalf("failure = %v", failure)
				}
				text := shownText(editor, "s")
				if round.admission == "a1" {
					wantCommentary := 0
					if shape == "commentary_tool_final" {
						wantCommentary = 1
					}
					if strings.Count(text, finalText) != 1 || strings.Count(text, commentaryText) != wantCommentary {
						t.Fatalf("text = %q", text)
					}
				} else if text != "SECOND" {
					t.Fatalf("text = %q", text)
				}
			}
		})
	}
}

const (
	approvalPrompt = `{"kind": "approval", "prompt_id": "p-1", "execution_generation": 3, "command": "rm -r x", "description": "remove", "choices": ["once", "deny"]}`
	clarifyPrompt  = `{"kind": "clarify", "prompt_id": "q-1", "execution_generation": 3, "question": "Pick a color", "choices": ["blue", "green"], "multi_select": false}`
)

type cardStep struct {
	outcome pyacp.PermissionOutcome
	err     error
}

type respondOwner struct {
	mu        sync.Mutex
	prompt    string
	lose      bool
	responses [][2]string
}

func (o *respondOwner) sent() [][2]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][2]string(nil), o.responses...)
}

func settleCards(a *GatewayACPAgent) {
	for range 5 {
		for _, task := range onLoop(a, func() []*loopTask {
			var tasks []*loopTask
			for _, task := range a.permissions {
				tasks = append(tasks, task)
			}
			return tasks
		}) {
			<-task.done
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUnacceptedCardIsReofferedOnReattachWithoutReplayingItsAnswer(t *testing.T) {
	cases := []struct {
		name             string
		prompt           string
		first            cardStep
		loseFirstRespond bool
		option, method   string
		value            string
	}{
		{"approval-editor-cancel", approvalPrompt, cardStep{outcome: &pyacp.DeniedOutcome{}}, false, "allow_once", "approval.respond", "once"},
		{"approval-transport-loss", approvalPrompt, cardStep{err: errors.New("editor transport lost")}, false, "allow_once", "approval.respond", "once"},
		{"approval-lost-respond", approvalPrompt, cardStep{outcome: &pyacp.AllowedOutcome{OptionID: "allow_once"}}, true, "deny", "approval.respond", "deny"},
		{"clarify-transport-loss", clarifyPrompt, cardStep{err: errors.New("editor transport lost")}, false, "choice-1", "clarify.respond", "green"},
		{"clarify-lost-respond", clarifyPrompt, cardStep{outcome: &pyacp.AllowedOutcome{OptionID: "choice-0"}}, true, "choice-1", "clarify.respond", "green"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := []cardStep{tc.first, {outcome: &pyacp.AllowedOutcome{OptionID: tc.option}}}
			var mu sync.Mutex
			cards := 0
			editor := &editorFake{}
			editor.permission = func(options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate) (*pyacp.RequestPermissionResponse, error) {
				mu.Lock()
				cards++
				step := script[0]
				script = script[1:]
				mu.Unlock()
				if step.err != nil {
					return nil, step.err
				}
				return &pyacp.RequestPermissionResponse{Outcome: step.outcome}, nil
			}
			owner := &respondOwner{prompt: tc.prompt, lose: tc.loseFirstRespond}
			a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				switch method {
				case "session.resume":
					return jsonObj(t, `{"session_id": "s", "prompts": [`+owner.prompt+`], "pending": []}`), nil
				case "config.set":
					return jsonObj(t, `{"key": "show_reasoning", "value": "1", "scope": "session"}`), nil
				}
				owner.mu.Lock()
				defer owner.mu.Unlock()
				answer := params["choice"]
				if answer == nil {
					answer = params["answer"]
				}
				owner.responses = append(owner.responses, [2]string{method, pyStr(answer)})
				if owner.lose {
					owner.lose = false
					return nil, &gateway.GatewayClientError{Msg: "Gateway disconnected"}
				}
				return jsonObj(t, `{"status": "resolved"}`), nil
			})
			onLoop(a, func() bool { a.conn = editor; return true })
			if err := onLoop(a, func() error { return a.permission("s", jsonObj(t, tc.prompt)) }); err != nil {
				t.Fatal(err)
			}
			settleCards(a)
			sentFirst := owner.sent()
			reattach := func() {
				if err := onLoop(a, func() error { return a.reattach(context.Background(), link) }); err != nil {
					t.Fatal(err)
				}
				settleCards(a)
			}
			reattach()
			mu.Lock()
			shown := cards
			mu.Unlock()
			if shown != 2 {
				t.Fatalf("%d cards shown, the unanswered prompt must be shown again", shown)
			}
			if got, want := owner.sent(), append(sentFirst, [2]string{tc.method, tc.value}); !reflect.DeepEqual(got, want) {
				t.Fatalf("responses = %v, want %v", got, want)
			}
			reattach()
			mu.Lock()
			defer mu.Unlock()
			if cards != 2 {
				t.Fatalf("%d cards shown after the accepted answer", cards)
			}
		})
	}
}

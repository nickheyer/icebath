package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

type passthroughTransport struct{ provider.Transport }

func (passthroughTransport) NormalizeResponse(response any, _ map[string]any) (*provider.Response, error) {
	return response.(*provider.Response), nil
}

func intakeRelayAgent(t *testing.T, delegateDepth int) (*AIAgent, *[][]any) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	calls := &[][]any{}
	a := &AIAgent{apiMode: "chat_completions", quietMode: true, delegateDepth: delegateDepth}
	a.transportCache = map[string]provider.Transport{"chat_completions": passthroughTransport{}}
	a.toolProgressCallback = func(event, name string, preview any, args any, extra *provider.CodexToolProgressExtra) {
		*calls = append(*calls, []any{event, name, preview, args})
	}
	return a, calls
}

func runIntake(t *testing.T, a *AIAgent, content string) PhaseVerdict {
	resp := &provider.Response{Message: &state.Message{Role: "assistant", Content: content}, FinishReason: "stop"}
	s := &LoopState{Response: resp, APICallCount: 1, APIRequestID: "r1", EffectiveTaskID: "t1", TurnID: "t1"}
	v, err := normalizeModelResponse(context.Background(), a, s)
	if err != nil {
		t.Fatal(err)
	}
	if s.AssistantMessage != resp || s.FinishReason != "stop" {
		t.Fatalf("state not rebound: %+v %q", s.AssistantMessage, s.FinishReason)
	}
	return v
}

func TestPlainReplyIsNotRelayedAsReasoning(t *testing.T) {
	a, calls := intakeRelayAgent(t, 0)
	if v := runIntake(t, a, "The answer is 42."); v.Action != "fallthrough" {
		t.Fatalf("action %q", v.Action)
	}
	if len(*calls) != 0 {
		t.Fatalf("relayed %v", *calls)
	}
}

func TestInlineThinkingBlockIsRelayedWithoutTheReply(t *testing.T) {
	a, calls := intakeRelayAgent(t, 0)
	runIntake(t, a, "<REASONING_SCRATCHPAD>plan step one</REASONING_SCRATCHPAD>\nThe answer is 42.")
	want := [][]any{{"reasoning.available", "_thinking", "plan step one", nil}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("got %v", *calls)
	}
}

func TestSubagentRelaysTheFirstLinePreview(t *testing.T) {
	a, calls := intakeRelayAgent(t, 1)
	runIntake(t, a, "<think>first line</think>\nsecond line")
	want := [][]any{{"_thinking", "first line", nil, nil}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("got %v", *calls)
	}
}

func TestNativeReasoningStreamedSuppressesTheRelay(t *testing.T) {
	a, calls := intakeRelayAgent(t, 0)
	a.nativeReasoningStreamed = true
	runIntake(t, a, "<think>plan</think>answer")
	if len(*calls) != 0 {
		t.Fatalf("relayed %v", *calls)
	}
}

func TestIncompleteScratchpadRetriesTwiceThenSavesPartial(t *testing.T) {
	a, _ := intakeRelayAgent(t, 0)
	content := "<REASONING_SCRATCHPAD>cut off"
	for i := 1; i <= 2; i++ {
		if v := runIntake(t, a, content); v.Action != "continue" || a.incompleteScratchpadRetries != i {
			t.Fatalf("attempt %d: %q retries %d", i, v.Action, a.incompleteScratchpadRetries)
		}
	}
	v := runIntake(t, a, content)
	if v.Action != "return" || v.Result["error"] != "Incomplete REASONING_SCRATCHPAD after 2 retries" || v.Result["partial"] != true {
		t.Fatalf("got %q %v", v.Action, v.Result)
	}
	if a.incompleteScratchpadRetries != 0 {
		t.Fatalf("retries %d", a.incompleteScratchpadRetries)
	}
}

func TestCoerceContentTextFlattensStructuredContent(t *testing.T) {
	cases := []struct {
		raw  any
		want any
	}{
		{map[string]any{"text": "hi"}, "hi"},
		{map[string]any{"content": "body"}, "body"},
		{map[string]any{"other": 1}, `{"other": 1}`},
		{[]any{"a", map[string]any{"type": "text", "text": "b"}, map[string]any{"type": "image", "text": 3}, map[string]any{"type": "image"}}, "a\nb\n3"},
		{7, "7"},
	}
	for _, c := range cases {
		got, err := coerceContentText(c.raw)
		if err != nil || got != c.want {
			t.Fatalf("%v: got %v %v", c.raw, got, err)
		}
	}
	if _, err := coerceContentText([]any{map[string]any{"type": "text", "text": 1}}); err == nil {
		t.Fatal("non str text part joined")
	}
}

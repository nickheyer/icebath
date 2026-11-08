package agent

import (
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
)

func streamAgent(t *testing.T) (*AIAgent, *[]string) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var got []string
	a := &AIAgent{showCommentary: true, deliveredInterimTexts: map[string]bool{}}
	a.streamDeltaCallback = func(s string) { got = append(got, s) }
	return a, &got
}

func visionToolResult() map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": "call_1", "content": []any{
		map[string]any{"type": "text", "text": "The image shows a red stop sign."},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}}
}

func TestInterimVisibleTextMultimodal(t *testing.T) {
	a, _ := streamAgent(t)
	t.Run("test_tool_list_content_does_not_crash", func(t *testing.T) {
		_ = a.interimAssistantVisibleText(visionToolResult())
	})
	t.Run("test_assistant_list_content_flattened", func(t *testing.T) {
		msg := map[string]any{"role": "assistant", "finish_reason": "incomplete", "content": []any{
			map[string]any{"type": "text", "text": "Let me look at the screenshot."},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}}
		if v := a.interimAssistantVisibleText(msg); !strings.Contains(v, "Let me look at the screenshot.") {
			t.Fatalf("visible %q", v)
		}
	})
}

func TestInterimVisibleTextPrefersCodexCommentary(t *testing.T) {
	a, _ := streamAgent(t)
	msg := map[string]any{"role": "assistant", "content": "partial final answer", "codex_message_items": []any{
		map[string]any{"type": "message", "phase": " Commentary ", "content": []any{
			map[string]any{"type": "output_text", "text": "Checking the logs <think>scratch</think>"},
			map[string]any{"type": "output_text", "text": "   "},
		}},
		map[string]any{"type": "message", "phase": "analysis", "content": []any{map[string]any{"type": "output_text", "text": "hidden"}}},
	}}
	if v := a.interimAssistantVisibleText(msg); v != "Checking the logs" {
		t.Fatalf("visible %q", v)
	}
	a.showCommentary = false
	if v := a.interimAssistantVisibleText(msg); v != "partial final answer" {
		t.Fatalf("commentary off must fall back to content, got %q", v)
	}
}

func TestEmitInterimAssistantMessageDeliversOncePerText(t *testing.T) {
	a, _ := streamAgent(t)
	type call struct {
		text     string
		streamed bool
	}
	var calls []call
	a.interimAssistantCallback = func(text string, alreadyStreamed bool) { calls = append(calls, call{text, alreadyStreamed}) }
	a.streamedAssistantTextParts = []string{"Looking   into it"}
	a.emitInterimAssistantMessage(map[string]any{"role": "assistant", "content": "Looking into it"})
	a.emitInterimAssistantMessage(map[string]any{"role": "assistant", "content": "Looking  into\nit"})
	a.emitInterimAssistantMessage(map[string]any{"role": "assistant", "content": "(empty)"})
	if len(calls) != 1 || calls[0] != (call{"Looking into it", true}) {
		t.Fatalf("calls %+v", calls)
	}
	a.holdStream = true
	a.emitInterimAssistantMessage(map[string]any{"role": "assistant", "content": "a held turn drops commentary"})
	if len(calls) != 1 {
		t.Fatalf("held turn delivered %+v", calls)
	}
}

func TestEmitInterimAssistantMessageFailingCallbackRecordsNothing(t *testing.T) {
	a, _ := streamAgent(t)
	n := 0
	a.interimAssistantCallback = func(string, bool) { n++; panic("boom") }
	a.emitInterimAssistantMessage(map[string]any{"role": "assistant", "content": "retry me"})
	a.emitInterimAssistantMessage(map[string]any{"role": "assistant", "content": "retry me"})
	if n != 2 {
		t.Fatalf("a failed delivery must not be recorded, calls %d", n)
	}
}

func TestFireStreamDeltaBreakScrubAndRecord(t *testing.T) {
	a, got := streamAgent(t)
	a.streamThinkScrubber = provider.NewStreamingThinkScrubber()
	a.streamContextScrubber = memory.NewStreamingContextScrubber()
	var reasoning []string
	a.reasoningCallback = func(s string) { reasoning = append(reasoning, s) }
	a.fireStreamDelta("\n\nHello")
	a.fireStreamDelta(" <think>plan</think>world")
	a.streamNeedsBreak = true
	a.fireStreamDelta("next")
	if strings.Join(*got, "|") != "Hello| world|\n\nnext" {
		t.Fatalf("delivered %q", *got)
	}
	if a.currentStreamedAssistantText() != "Hello world\n\nnext" || a.streamNeedsBreak {
		t.Fatalf("recorded %q", a.currentStreamedAssistantText())
	}
	if strings.Join(reasoning, "") != "plan" {
		t.Fatalf("inline reasoning %q", reasoning)
	}
	a.resetStreamDeliveryTracking()
	if a.currentStreamedAssistantText() != "" || a.nativeReasoningStreamed || a.streamReasoningHooksEnabled != nil {
		t.Fatal("reset must clear the per response delivery state")
	}
}

func TestFireReasoningDeltaStopsInlineForwarding(t *testing.T) {
	a, _ := streamAgent(t)
	a.streamThinkScrubber = provider.NewStreamingThinkScrubber()
	var reasoning []string
	a.reasoningCallback = func(s string) { reasoning = append(reasoning, s) }
	a.fireReasoningDelta("native")
	a.fireStreamDelta("<think>inline</think>text")
	if strings.Join(reasoning, "|") != "native" || !a.nativeReasoningStreamed || a.streamReasoningHooksEnabled == nil || *a.streamReasoningHooksEnabled {
		t.Fatalf("reasoning %q", reasoning)
	}
}

func TestSupersededWriterIsFenced(t *testing.T) {
	a, got := streamAgent(t)
	var reasoning []string
	a.reasoningCallback = func(s string) { reasoning = append(reasoning, s) }
	var wg sync.WaitGroup
	stale := make(chan struct{})
	resume := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.ClaimStreamWriter()
		close(stale)
		<-resume
		a.fireStreamDelta("late")
		a.fireReasoningDelta("late")
	}()
	<-stale
	token := a.ClaimStreamWriter()
	close(resume)
	wg.Wait()
	a.fireStreamDelta("fresh")
	if !a.StreamWriterIsCurrent(token) || strings.Join(*got, "|") != "fresh" || len(reasoning) != 0 || a.streamWriterDropped != 2 {
		t.Fatalf("delivered %q reasoning %q dropped %d", *got, reasoning, a.streamWriterDropped)
	}
}

func TestStreamHookBasePayloadDefaultsTheSurface(t *testing.T) {
	a := &AIAgent{currentTurnID: "t1", apiCallCount: 3, sessionID: "s", model: "m", provider: "p"}
	p := a.streamHookBasePayload()
	if p["surface"] != "cli" || p["iteration"] != 3 || p["turn_id"] != "t1" {
		t.Fatalf("payload %v", p)
	}
}

func TestHeldRowIsHiddenThenPublishedWithCommentaryWithheld(t *testing.T) {
	a, _ := streamAgent(t)
	a.holdStream = true
	row := withholdHeldRow(a, map[string]any{"role": "assistant", "content": "first half"})
	if row["display_kind"] != "hidden" {
		t.Fatalf("row %v", row)
	}
	final := map[string]any{"role": "assistant", "content": "second half", "display_kind": "hidden",
		"codex_message_items": `[{"type": "message", "role": "assistant", "phase": "commentary", "content": [{"type": "output_text", "text": "thinking aloud"}]}]`}
	publishHeldFinalRow(a, final, "first half second half", true)
	meta, _ := final["display_metadata"].(map[string]any)
	if _, hidden := final["display_kind"]; hidden || meta[CommentaryWithheld] != true || final["content"] != "first half second half" || final["api_content"] != "second half" {
		t.Fatalf("final %v", final)
	}
	a.holdStream = false
	if row := withholdHeldRow(a, map[string]any{"role": "assistant", "content": "x"}); row["display_kind"] != nil {
		t.Fatalf("an unheld turn is untouched, got %v", row)
	}
}

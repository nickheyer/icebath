package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func partialStreamStub(content string) map[string]any {
	return map[string]any{"id": home.PartialStreamStubID, "model": "test/model", "usage": nil,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": home.FinishReasonLength}}}
}

func wedgeRun(t *testing.T, a *AIAgent, ctx context.Context, message string, history []map[string]any) map[string]any {
	t.Helper()
	result, err := a.RunConversation(ctx, message, RunConversationOptions{ConversationHistory: history})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func wedgeExhaust(t *testing.T) (*AIAgent, context.Context, *thinkingScript, map[string]any) {
	script := &thinkingScript{responses: []map[string]any{partialStreamStub("part one "), partialStreamStub("part two "),
		partialStreamStub("part three "), partialStreamStub("part four.")}}
	a, ctx := thinkingLoopAgent(t, script)
	return a, ctx, script, wedgeRun(t, a, ctx, "write me a long report", nil)
}

func wedgeText(v any) string {
	s, _ := v.(string)
	return s
}

func TestContinuationCeilingWedge(t *testing.T) {
	t.Run("test_partial_text_surfaced_at_ceiling", func(t *testing.T) {
		_, _, _, result := wedgeExhaust(t)
		final := wedgeText(result["final_response"])
		if result["completed"] != false || result["partial"] != true || !strings.Contains(final, "part one") || !strings.Contains(final, "part four") {
			t.Fatalf("completed %v partial %v final %q", result["completed"], result["partial"], final)
		}
	})
	t.Run("test_new_user_message_issues_fresh_request", func(t *testing.T) {
		a, ctx, script, first := wedgeExhaust(t)
		if first["completed"] != false || len(script.calls()) != 4 {
			t.Fatalf("first turn completed %v after %d calls", first["completed"], len(script.calls()))
		}
		script.mu.Lock()
		script.responses = append(script.responses, thinkingMockResponse("Hello! How can I help?", "stop"))
		script.mu.Unlock()
		second := wedgeRun(t, a, ctx, "hi", resultRows(t, first))
		if len(script.calls()) != 5 || second["completed"] != true || second["final_response"] != "Hello! How can I help?" || pyTruthy(second["error"]) {
			t.Fatalf("calls %d completed %v final %v error %v", len(script.calls()), second["completed"], second["final_response"], second["error"])
		}
	})
	t.Run("test_ceiling_replaces_scaffolding_with_settled_turn", func(t *testing.T) {
		_, _, _, result := wedgeExhaust(t)
		msgs := resultRows(t, result)
		assistants := 0
		for _, m := range msgs {
			if m["role"] == "user" && strings.Contains(wedgeText(m["content"]), "Continue exactly where you left off") {
				t.Fatalf("a continuation nudge survived the ceiling exit %v", m)
			}
			if m["role"] == "assistant" {
				assistants++
			}
		}
		last := msgs[len(msgs)-1]
		if assistants != 1 || last["role"] != "assistant" {
			t.Fatalf("assistants %d last %v", assistants, last)
		}
		for _, part := range []string{"part one", "part two", "part three", "part four"} {
			if !strings.Contains(wedgeText(last["content"]), part) {
				t.Fatalf("stitched partial lost %q in %q", part, last["content"])
			}
		}
	})
	t.Run("test_continuation_requests_carry_no_marks", func(t *testing.T) {
		script := &thinkingScript{responses: []map[string]any{partialStreamStub("part one "), partialStreamStub("part two "),
			thinkingMockResponse("the rest.", "stop")}}
		a, ctx := thinkingLoopAgent(t, script)
		var seen [][]map[string]any
		prev := loopBuildAPIKwargs
		loopBuildAPIKwargs = func(agent *AIAgent, apiMessages, toolsForAPI []map[string]any, toolsOverride bool) (map[string]any, error) {
			batch := make([]map[string]any, len(apiMessages))
			for i, m := range apiMessages {
				batch[i] = shallowRow(m)
			}
			seen = append(seen, batch)
			return prev(agent, apiMessages, toolsForAPI, toolsOverride)
		}
		t.Cleanup(func() { loopBuildAPIKwargs = prev })
		result := wedgeRun(t, a, ctx, "write me a long report", nil)
		if result["completed"] != true || len(seen) < 3 {
			t.Fatalf("completed %v after %d request builds", result["completed"], len(seen))
		}
		for idx, batch := range seen {
			for _, m := range batch {
				for key := range m {
					if strings.HasPrefix(key, "_length_continuation") {
						t.Fatalf("continuation mark %q leaked into request %d", key, idx)
					}
				}
			}
		}
	})
	t.Run("test_prior_turn_marked_message_survives_later_ceiling", func(t *testing.T) {
		script := &thinkingScript{responses: []map[string]any{partialStreamStub("wedge one "), partialStreamStub("wedge two "),
			partialStreamStub("wedge three "), partialStreamStub("wedge four.")}}
		a, ctx := thinkingLoopAgent(t, script)
		reloaded := []map[string]any{{"role": "user", "content": "earlier question"},
			{"role": "assistant", "content": "earlier answer fragment", "_length_continuation_fragment": true}}
		result := wedgeRun(t, a, ctx, "another long report", reloaded)
		prior := 0
		for _, m := range resultRows(t, result) {
			if m["role"] == "assistant" && strings.Contains(wedgeText(m["content"]), "earlier answer fragment") {
				prior++
			}
		}
		if result["completed"] != false || prior != 1 {
			t.Fatalf("completed %v prior rows %d", result["completed"], prior)
		}
	})
	t.Run("test_new_turn_does_not_inherit_continuation_counter", func(t *testing.T) {
		a, ctx, script, first := wedgeExhaust(t)
		script.mu.Lock()
		script.responses = append(script.responses, partialStreamStub("second turn partial "), thinkingMockResponse("and the rest.", "stop"))
		script.mu.Unlock()
		second := wedgeRun(t, a, ctx, "try again", resultRows(t, first))
		final := wedgeText(second["final_response"])
		if second["completed"] != true || !strings.Contains(final, "second turn partial") || !strings.Contains(final, "and the rest.") {
			t.Fatalf("completed %v final %q", second["completed"], final)
		}
	})
}

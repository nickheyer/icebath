package tui

import (
	"strings"
	"testing"
)

func TestSplitReasoning(t *testing.T) {
	t.Run("extracts <think>…</think> and strips it from text", func(t *testing.T) {
		if got := splitReasoning("<think>plotting</think>\n\nhere is the answer"); got != (splitReply{Reasoning: "plotting", Text: "here is the answer"}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("handles multiple tag shapes", func(t *testing.T) {
		got := splitReasoning("<reasoning>a</reasoning> <THINKING>b</THINKING> <thought>c</thought> body")
		if !strings.Contains(got.Reasoning, "a") || !strings.Contains(got.Reasoning, "b") || !strings.Contains(got.Reasoning, "c") || got.Text != "body" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("treats unclosed leading <think>… as reasoning (real reasoning-model stream)", func(t *testing.T) {
		if got := splitReasoning("<think>still deciding"); got != (splitReply{Reasoning: "still deciding"}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("does not strip trailing prose after a stray mid-text <think> mention", func(t *testing.T) {
		in := "final answer paragraph one.\n\n<think>internal note never closed\n\nfinal answer paragraph two."
		if got := splitReasoning(in); got != (splitReply{Text: in}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("returns empty reasoning and untouched text when no tags present", func(t *testing.T) {
		if got := splitReasoning("plain body with no tags"); got != (splitReply{Text: "plain body with no tags"}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("preserves text when reasoning block is empty", func(t *testing.T) {
		if got := splitReasoning("<think></think>only body"); got != (splitReply{Text: "only body"}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("detects presence of any supported tag", func(t *testing.T) {
		if !hasReasoningTag("pre <think>x</think> post") || !hasReasoningTag("pre <reasoning>x</reasoning>") ||
			!hasReasoningTag("<REASONING_SCRATCHPAD>x</REASONING_SCRATCHPAD>") || hasReasoningTag("no tags at all") {
			t.Fatal("detection")
		}
	})
}

func TestCleanThinkingText(t *testing.T) {
	t.Run("removes face/status ticker fragments while preserving real reasoning", func(t *testing.T) {
		in := "(¬_¬) synthesizing...**Resolving comments on GitHub**\n( ͡° ͜ʖ ͡°) musing...\nActual step\n٩(๑❛ᴗ❛๑)۶ contemplating...next step"
		if got := cleanThinkingText(in); got != "**Resolving comments on GitHub**\nActual step\nnext step" {
			t.Fatalf("got %q", got)
		}
	})
}

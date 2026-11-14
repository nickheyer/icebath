package tui

import (
	"strings"
	"testing"
)

func flag(v bool) *bool { return &v }

func TestVirtualHeightEstimates(t *testing.T) {
	plain := heightOpts{}
	details := heightOpts{Details: true}
	t.Run("uses stable content keys across resumed message objects", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "same text", Tools: []string{"Search Files [long message]"}}
		copied := m
		if messageHeightKey(m) != messageHeightKey(copied) {
			t.Fatal("keys differ")
		}
	})
	t.Run("accounts for wrapping and preserved blank-block rhythm", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "one\n\n" + strings.Repeat("x", 90)}
		if wrappedLines(m.Text, 30, maxEstimateLines) != 5 || estimatedMsgHeight(m, 35, plain) <= 5 {
			t.Fatalf("wrapped %d height %d", wrappedLines(m.Text, 30, maxEstimateLines), estimatedMsgHeight(m, 35, plain))
		}
	})
	t.Run("uses compound user prompt width when estimating user message wrapping", func(t *testing.T) {
		m := msg{Role: "user", Text: strings.Repeat("x", 23)}
		if got := estimatedMsgHeight(m, 30, heightOpts{UserPrompt: "❯"}); got != 3 {
			t.Fatalf("single glyph %d", got)
		}
		if got := estimatedMsgHeight(m, 30, heightOpts{UserPrompt: "Ψ >"}); got != 4 {
			t.Fatalf("compound %d", got)
		}
	})
	t.Run("adds one row for a group-boundary lead gap", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "reply"}
		if estimatedMsgHeight(m, 80, heightOpts{LeadGap: true}) != estimatedMsgHeight(m, 80, plain)+1 {
			t.Fatal("lead gap")
		}
	})
	t.Run("includes detail sections when visible", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "ok", Thinking: "line 1\nline 2", Tools: []string{"Tool A", "Tool B"}}
		if estimatedMsgHeight(m, 80, details) <= estimatedMsgHeight(m, 80, plain) {
			t.Fatal("details")
		}
	})
	t.Run("accounts for the response separator when assistant details are visible", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "ok", Thinking: "plan"}
		if estimatedMsgHeight(m, 80, details) != estimatedMsgHeight(m, 80, plain)+3 {
			t.Fatal("separator")
		}
	})
	t.Run("does not account for a response separator without visible details", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "ok"}
		if estimatedMsgHeight(m, 80, details) != estimatedMsgHeight(m, 80, plain) {
			t.Fatal("separator without details")
		}
	})
	t.Run("honors per-section visibility when estimating response separators", func(t *testing.T) {
		thinkingOnly := msg{Role: "assistant", Text: "ok", Thinking: "plan"}
		toolsOnly := msg{Role: "assistant", Text: "ok", Tools: []string{"Tool A"}}
		if estimatedMsgHeight(thinkingOnly, 80, heightOpts{Details: true, ThinkingVisible: flag(false), ToolsVisible: flag(true)}) != estimatedMsgHeight(thinkingOnly, 80, plain) {
			t.Fatal("thinking hidden")
		}
		if estimatedMsgHeight(toolsOnly, 80, heightOpts{Details: true, ThinkingVisible: flag(true), ToolsVisible: flag(false)}) != estimatedMsgHeight(toolsOnly, 80, plain) {
			t.Fatal("tools hidden")
		}
	})
	t.Run("treats historical thinking blocks as collapsed unless explicitly expanded", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "ok", Thinking: "line 1\nline 2\nline 3"}
		collapsed := estimatedMsgHeight(m, 80, heightOpts{Details: true, ThinkingExpanded: flag(false), ThinkingVisible: flag(true), ToolsVisible: flag(false)})
		expanded := estimatedMsgHeight(m, 80, heightOpts{Details: true, ThinkingExpanded: flag(true), ThinkingVisible: flag(true), ToolsVisible: flag(false)})
		if collapsed >= expanded {
			t.Fatalf("collapsed %d expanded %d", collapsed, expanded)
		}
	})
	t.Run("reserves two extra rows for the inter-turn separator on non-first user messages", func(t *testing.T) {
		m := msg{Role: "user", Text: "follow-up question"}
		if estimatedMsgHeight(m, 80, heightOpts{WithSeparator: true}) != estimatedMsgHeight(m, 80, plain)+2 {
			t.Fatal("separator rows")
		}
	})
	t.Run("caps wrapped-line counting so giant assistant turns do not block offset rebuilds", func(t *testing.T) {
		if rows := wrappedLines(strings.Repeat("x", 1_000_000), 80, maxEstimateLines); rows > 800 {
			t.Fatalf("rows %d", rows)
		}
	})
}

func TestTermuxTUIMode(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	t.Run("defaults on inside Termux and honours the opt out", func(t *testing.T) {
		if !isTermuxTUIMode(env("TERMUX_VERSION", "0.118")) || !isTermuxTUIMode(env("PREFIX", termuxPrefix)) {
			t.Fatal("termux default")
		}
		if isTermuxTUIMode(env("TERMUX_VERSION", "0.118", "ICEBATH_TUI_TERMUX_MODE", "0")) || !isTermuxTUIMode(env("TERMUX_VERSION", "1", "ICEBATH_TUI_TERMUX_MODE", " Yes ")) {
			t.Fatal("override")
		}
		if isTermuxTUIMode(env("ICEBATH_TUI_TERMUX_MODE", "1")) {
			t.Fatal("outside termux")
		}
	})
	t.Run("hashes like the JavaScript djb2 xor over UTF-16 units", func(t *testing.T) {
		if got := hashText(""); got != "45h" {
			t.Fatalf("empty %q", got)
		}
		if hashText("a") == hashText("b") || hashText("😀") == hashText("x") {
			t.Fatal("collisions")
		}
	})
}

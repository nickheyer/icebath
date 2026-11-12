package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var sgrRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plainSGR(s string) string { return sgrRe.ReplaceAllString(s, "") }

// printRecorder keeps every console write as one entry, one entry per cprint call
type printRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *printRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.TrimSuffix(string(p), "\n"))
	r.mu.Unlock()
	return len(p), nil
}

// Entries printed so far with color escapes removed
func (r *printRecorder) plain() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = plainSGR(c)
	}
	return out
}

func (r *printRecorder) raw() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.calls...)
}

func (r *printRecorder) reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

// Routes every cprint of the test into a recorder under a fresh home
func recordPrints(t *testing.T) *printRecorder {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rec := &printRecorder{}
	restore := SetStreams(&cliui.Console{In: strings.NewReader(""), Out: rec, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}, io.Discard)
	t.Cleanup(restore)
	modernGapPending.Store(false)
	return rec
}

// A CLI built without its constructor, the attribute defaults hermes reads through getattr filled in
func bareCLI() *CLI {
	turnCtx, cancel := context.WithCancelCause(context.Background())
	c := &CLI{ctx: context.Background(), turnCtx: turnCtx, turnCancel: func() { cancel(tools.ErrKeyboardInterrupt) }, timestampFormat: "%H:%M", userMessagePreviewFirstLines: 2, userMessagePreviewLastLines: 2,
		finalResponseMarkdown: "raw", pendingEditSnapshots: map[string]*tools.LocalEditSnapshot{}, pendingToolInfo: map[string][]map[string]any{}}
	c.resetStreamStateLocked()
	return c
}

// A CLI built by its constructor over a clean config with the given display section
func configuredCLI(t *testing.T, display map[string]any, o CLIOptions) *CLI {
	t.Helper()
	t.Setenv("LLM_MODEL", "")
	t.Setenv("ICEBATH_MAX_ITERATIONS", "")
	full := map[string]any{"compact": false}
	for k, v := range display {
		full[k] = v
	}
	SetCLIConfig(map[string]any{
		"model":    map[string]any{"default": "anthropic/claude-opus-4.6", "base_url": "https://openrouter.ai/api/v1", "provider": "auto"},
		"display":  full,
		"agent":    map[string]any{},
		"terminal": map[string]any{"env_type": "local"},
	})
	t.Cleanup(func() { SetCLIConfig(nil) })
	c, err := NewCLI(context.Background(), o)
	if err != nil {
		t.Fatalf("NewCLI: %v", err)
	}
	t.Cleanup(func() {
		if c.sessionDB != nil {
			c.sessionDB.Close()
		}
	})
	return c
}

func thinkTagCLI(t *testing.T) (*CLI, *[]string, *[]string) {
	c := bareCLI()
	emitted, reasoning := &[]string{}, &[]string{}
	prevEmit, prevReasoning := emitStreamTextSink, streamReasoningSink
	t.Cleanup(func() { emitStreamTextSink, streamReasoningSink = prevEmit, prevReasoning })
	emitStreamTextSink = func(_ *CLI, text string) { *emitted = append(*emitted, text) }
	streamReasoningSink = func(_ *CLI, text string) { *reasoning = append(*reasoning, text) }
	return c, emitted, reasoning
}

func TestThinkTagInProse(t *testing.T) {
	t.Run("test_think_tag_mid_sentence", func(t *testing.T) {
		c, emitted, _ := thinkTagCLI(t)
		for _, tok := range []string{"  1. Fix reasoning mode in eval ", "(/think not producing ", "<think>", " tags — ~2% gap)", "\n  2. Launch production"} {
			c.streamDelta(tok)
		}
		if c.inReasoningBlock {
			t.Fatal("<think> in prose should not enter reasoning block")
		}
		full := strings.Join(*emitted, "")
		if !strings.Contains(full, "<think>") || !strings.Contains(full, "Launch production") {
			t.Fatalf("emitted %q", full)
		}
	})
	t.Run("test_think_tag_in_backticks", func(t *testing.T) {
		c, _, _ := thinkTagCLI(t)
		c.streamDelta("Use the `<think>` tag for reasoning")
		if c.inReasoningBlock {
			t.Fatal("a tag in backticks must not start reasoning")
		}
	})
}

func TestRealReasoningBlock(t *testing.T) {
	t.Run("test_think_at_start_of_stream", func(t *testing.T) {
		c, emitted, _ := thinkTagCLI(t)
		c.streamDelta("<think>")
		if !c.inReasoningBlock {
			t.Fatal("a leading tag must open the reasoning block")
		}
		c.streamDelta("I need to analyze this")
		c.streamDelta("</think>")
		if c.inReasoningBlock {
			t.Fatal("the close tag must end the reasoning block")
		}
		c.streamDelta("Here is my answer")
		full := strings.Join(*emitted, "")
		if !strings.Contains(full, "Here is my answer") || strings.Contains(full, "I need to analyze") {
			t.Fatalf("emitted %q", full)
		}
	})
	t.Run("test_think_after_newline", func(t *testing.T) {
		c, emitted, _ := thinkTagCLI(t)
		c.streamDelta("Some preamble\n<think>")
		if !c.inReasoningBlock || !strings.Contains(strings.Join(*emitted, ""), "Some preamble") {
			t.Fatalf("block %v emitted %q", c.inReasoningBlock, *emitted)
		}
	})
	t.Run("test_think_with_only_whitespace_before", func(t *testing.T) {
		c, _, _ := thinkTagCLI(t)
		c.streamDelta("   <think>")
		if !c.inReasoningBlock {
			t.Fatal("whitespace before the tag still opens the block")
		}
	})
	t.Run("test_reasoning_tags_are_case_insensitive", func(t *testing.T) {
		for _, tag := range []string{"THINK", "Think", "ThInK", "THOUGHT", "REASONING", "Thinking"} {
			c, emitted, _ := thinkTagCLI(t)
			c.streamDelta("<" + tag + ">hidden reasoning</" + tag + ">Visible answer")
			if c.inReasoningBlock {
				t.Fatalf("%s left the block open", tag)
			}
			if full := strings.Join(*emitted, ""); full != "Visible answer" {
				t.Fatalf("%s emitted %q", tag, full)
			}
		}
	})
}

func TestFlushRecovery(t *testing.T) {
	t.Run("test_flush_recovers_buffered_content", func(t *testing.T) {
		recordPrints(t)
		t.Setenv("COLUMNS", "80")
		c, emitted, _ := thinkTagCLI(t)
		c.inReasoningBlock = true
		c.streamPrefilt = " tags — ~2% gap)\n  2. Launch production"
		c.streamBoxOpened = true
		c.flushStream()
		if c.inReasoningBlock || !strings.Contains(strings.Join(*emitted, ""), "Launch production") {
			t.Fatalf("block %v emitted %q", c.inReasoningBlock, *emitted)
		}
	})
}

func streamCLI(t *testing.T, columns string) (*CLI, *printRecorder) {
	t.Helper()
	rec := recordPrints(t)
	t.Setenv("COLUMNS", columns)
	return bareCLI(), rec
}

func TestStreamedContentLinesHaveNoLeadingWhitespace(t *testing.T) {
	c, rec := streamCLI(t, "76")
	c.streamDelta("First streamed line of text.\nSecond streamed line.\n")
	c.flushStream()
	found := 0
	for _, line := range rec.plain() {
		if strings.Contains(line, "streamed line") {
			found++
			if strings.HasPrefix(line, " ") {
				t.Fatalf("leading whitespace leaked: %q", line)
			}
		}
	}
	if found == 0 {
		t.Fatal("no content lines captured")
	}
}

func TestIntentionalMarkdownIndentationIsPreserved(t *testing.T) {
	c, rec := streamCLI(t, "76")
	c.streamDelta("- item\n  - nested item\n")
	c.flushStream()
	plain := rec.plain()
	has := func(want string) bool {
		for _, line := range plain {
			if line == want {
				return true
			}
		}
		return false
	}
	if !has("- item") || !has("  - nested item") {
		t.Fatalf("lines %q", plain)
	}
}

func TestLogicalLineStreaming(t *testing.T) {
	chunks := func(c *CLI, text string, n int) {
		for i := 0; i < len(text); i += n {
			c.streamDelta(text[i:min(i+n, len(text))])
		}
	}
	t.Run("test_long_paragraph_not_hard_wrapped_before_newline", func(t *testing.T) {
		c, rec := streamCLI(t, "76")
		text := strings.Repeat("This is a long opening paragraph that previously got chopped "+
			"into terminal-width chunks with real newlines, which is what "+
			"made copy/paste come out full of broken lines. ", 3)
		chunks(c, text, 12)
		if strings.Contains(strings.Join(rec.plain(), "\n"), "opening paragraph") {
			t.Fatal("content printed before any newline")
		}
		if !strings.HasPrefix(c.streamBuf, "This is a long opening") {
			t.Fatalf("buffer %q", c.streamBuf)
		}
	})
	t.Run("test_partial_tail_mirrored_into_spinner", func(t *testing.T) {
		c, _ := streamCLI(t, "76")
		chunks(c, strings.Repeat("A long paragraph streaming in without any newline ", 4), 16)
		if !strings.HasPrefix(c.spinnerText, "…") || !strings.Contains(c.spinnerText, "newline") {
			t.Fatalf("spinner %q", c.spinnerText)
		}
	})
	t.Run("test_no_content_lost_across_stream", func(t *testing.T) {
		c, rec := streamCLI(t, "76")
		words := make([]string, 120)
		for i := range words {
			words[i] = "word" + itoa(i)
		}
		chunks(c, strings.Join(words, " "), 7)
		c.flushStream()
		plain := " " + strings.Join(strings.Fields(strings.Join(rec.plain(), "\n")), " ") + " "
		for _, w := range words {
			if !strings.Contains(plain, " "+w+" ") {
				t.Fatalf("lost %s", w)
			}
		}
	})
	t.Run("test_table_rows_not_previewed_in_spinner", func(t *testing.T) {
		c, rec := streamCLI(t, "76")
		cells := make([]string, 20)
		for i := range cells {
			cells[i] = "cell" + itoa(i)
		}
		c.streamDelta("| " + strings.Join(cells, " | ") + " |")
		if strings.Contains(strings.Join(rec.plain(), "\n"), "cell19") || c.spinnerText != "" {
			t.Fatalf("printed %q spinner %q", rec.plain(), c.spinnerText)
		}
	})
	t.Run("test_newline_lines_still_emit_normally", func(t *testing.T) {
		c, rec := streamCLI(t, "76")
		c.streamDelta("line one\nline two\n")
		plain := strings.Join(rec.plain(), "\n")
		if !strings.Contains(plain, "line one") || !strings.Contains(plain, "line two") {
			t.Fatalf("printed %q", plain)
		}
	})
	t.Run("test_unbreakable_run_stays_single_line", func(t *testing.T) {
		c, rec := streamCLI(t, "76")
		c.streamDelta(strings.Repeat("x", 300))
		c.flushStream()
		if strings.Count(strings.Join(rec.plain(), "\n"), "x") != 300 {
			t.Fatalf("printed %q", rec.plain())
		}
		content := 0
		for _, e := range rec.plain() {
			if strings.Contains(e, "x") {
				content++
			}
		}
		if content != 1 {
			t.Fatalf("unbreakable run was hard-wrapped into %d prints", content)
		}
	})
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func indexWhere(lines []string, pred func(string) bool) int {
	for i, l := range lines {
		if pred(l) {
			return i
		}
	}
	return -1
}

func TestAnswerStreamsBeforeTurnEndAfterReasoning(t *testing.T) {
	c, rec := streamCLI(t, "74")
	c.showReasoning = true
	c.streamReasoningDelta("thinking about it\n")
	c.streamDelta("First answer line.\n")
	lines := rec.plain()
	answer := indexWhere(lines, func(l string) bool { return strings.Contains(l, "First answer line.") })
	reasoning := indexWhere(lines, func(l string) bool { return strings.Contains(l, "thinking about it") })
	if answer == -1 || reasoning == -1 || reasoning >= answer {
		t.Fatalf("lines %q", lines)
	}
}

func TestLateReasoningDoesNotReopenBoxInsideAnswer(t *testing.T) {
	c, rec := streamCLI(t, "74")
	c.showReasoning = true
	c.streamReasoningDelta("early thought\n")
	c.streamDelta("Answer.\n")
	c.streamReasoningDelta("late thought\n")
	c.flushStream()
	for _, e := range rec.plain() {
		if strings.Contains(e, "late thought") {
			t.Fatalf("late reasoning printed: %q", rec.plain())
		}
	}
}

func TestStatusLineWaitsForBoxFooter(t *testing.T) {
	c, rec := streamCLI(t, "74")
	c.streamDelta("First paragraph.\n")
	c.agentStatusPrint("  ✓ [set 7 · 2/2] worker  (2175.14s)")
	c.streamDelta("Second paragraph.\n")
	c.flushStream()
	lines := rec.plain()
	notice := indexWhere(lines, func(l string) bool { return strings.Contains(l, "set 7") })
	footer := indexWhere(lines, func(l string) bool { return strings.HasPrefix(l, "╰") })
	second := indexWhere(lines, func(l string) bool { return strings.Contains(l, "Second paragraph") })
	if second == -1 || !(second < footer && footer < notice) {
		t.Fatalf("lines %q", lines)
	}
}

func TestStatusLinePrintsImmediatelyOutsideABox(t *testing.T) {
	c, rec := streamCLI(t, "74")
	c.agentStatusPrint("  ✓ [set 1 · 1/1] worker  (3.0s)")
	if got := rec.plain(); len(got) != 1 || got[0] != "  ✓ [set 1 · 1/1] worker  (3.0s)" {
		t.Fatalf("printed %q", got)
	}
	if len(c.heldStatusLines) != 0 {
		t.Fatalf("held %q", c.heldStatusLines)
	}
}

func TestRepeatedToolInOneBatchPrintsOnce(t *testing.T) {
	rec := recordPrints(t)
	c := configuredCLI(t, map[string]any{"tool_progress": "off"}, CLIOptions{})
	c.modernLayout = false
	announce := func(names ...string) []string {
		rec.reset()
		for _, n := range names {
			c.onToolGenStart(n)
		}
		return rec.raw()
	}
	count := func(printed []string, want string) int {
		n := 0
		for _, p := range printed {
			if strings.Contains(p, want) {
				n++
			}
		}
		return n
	}
	printed := announce("terminal", "terminal", "terminal", "read_file")
	if count(printed, "preparing terminal") != 1 || count(printed, "preparing read_file") != 1 {
		t.Fatalf("printed %q", printed)
	}
	c.onToolProgress("tool.started", "terminal", "ls", map[string]any{"command": "ls"}, nil)
	if printed := announce("terminal"); count(printed, "preparing terminal") != 1 {
		t.Fatalf("after a tool start printed %q", printed)
	}
	c.resetStreamState()
	if printed := announce("terminal"); count(printed, "preparing terminal") != 1 {
		t.Fatalf("after a reset printed %q", printed)
	}
}

func TestToolProgressScrollback(t *testing.T) {
	done := func(duration float64) map[string]any { return map[string]any{"duration": duration, "is_error": false} }
	t.Run("test_all_mode_prints_scrollback_on_completed", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		c.onToolProgress("tool.started", "terminal", "git log", map[string]any{"command": "git log"}, nil)
		rec.reset()
		c.onToolProgress("tool.completed", "terminal", "", nil, done(1.5))
		printed := rec.raw()
		if len(printed) != 1 || !(strings.Contains(printed[0], "git log") || strings.Contains(printed[0], "$")) {
			t.Fatalf("printed %q", printed)
		}
	})
	t.Run("test_all_mode_prints_every_call", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("tool.started", "read_file", "cli.py", map[string]any{"path": "cli.py"}, nil)
		c.onToolProgress("tool.completed", "read_file", "", nil, done(0.1))
		c.onToolProgress("tool.started", "read_file", "run_agent.py", map[string]any{"path": "run_agent.py"}, nil)
		c.onToolProgress("tool.completed", "read_file", "", nil, done(0.2))
		if printed := rec.raw(); len(printed) != 2 {
			t.Fatalf("printed %q", printed)
		}
	})
	t.Run("test_off_mode_no_scrollback", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "off"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("tool.started", "terminal", "ls", map[string]any{"command": "ls"}, nil)
		c.onToolProgress("tool.completed", "terminal", "", nil, done(0.5))
		if printed := rec.raw(); len(printed) != 0 {
			t.Fatalf("printed %q", printed)
		}
	})
	t.Run("test_concurrent_tools_produce_stacked_lines", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("tool.started", "web_search", "query 1", map[string]any{"query": "test 1"}, nil)
		c.onToolProgress("tool.started", "web_search", "query 2", map[string]any{"query": "test 2"}, nil)
		c.onToolProgress("tool.completed", "web_search", "", nil, done(1.0))
		c.onToolProgress("tool.completed", "web_search", "", nil, done(1.5))
		if printed := rec.raw(); len(printed) != 2 {
			t.Fatalf("printed %q", printed)
		}
	})
	t.Run("test_verbose_mode_commits_every_call", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "verbose"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("tool.started", "terminal", "echo one", map[string]any{"command": "echo one"}, nil)
		c.onToolProgress("tool.completed", "terminal", "", nil, done(0.1))
		c.onToolProgress("tool.started", "terminal", "echo two", map[string]any{"command": "echo two"}, nil)
		c.onToolProgress("tool.completed", "terminal", "", nil, done(0.1))
		if printed := rec.raw(); len(printed) != 2 {
			t.Fatalf("printed %q", printed)
		}
	})
}

func TestMoAReferenceBlocks(t *testing.T) {
	t.Run("test_reference_event_prints_labelled_block", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("moa.reference", "openrouter:openai/gpt-5.5", "Paris is the capital.", nil, map[string]any{"moa_index": 1, "moa_count": 2})
		printed := strings.Join(rec.raw(), " ")
		for _, want := range []string{"openrouter:openai/gpt-5.5", "Reference 1/2", "Paris is the capital."} {
			if !strings.Contains(printed, want) {
				t.Fatalf("printed %q lacks %q", printed, want)
			}
		}
	})
	t.Run("test_reference_event_prints_even_when_progress_off", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "off"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("moa.reference", "openrouter:anthropic/claude-opus-4.8", "Four.", nil, map[string]any{"moa_index": 2, "moa_count": 2})
		if len(rec.raw()) == 0 {
			t.Fatal("a reference block must print with tool progress off")
		}
	})
	t.Run("test_aggregating_event_updates_spinner_only", func(t *testing.T) {
		rec := recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
		rec.reset()
		c.onToolProgress("moa.aggregating", "openrouter:anthropic/claude-opus-4.8", "", nil, nil)
		if !strings.Contains(c.spinnerText, "aggregating") || len(rec.raw()) != 0 {
			t.Fatalf("spinner %q printed %q", c.spinnerText, rec.raw())
		}
	})
}

func TestSubmittedUserMessagePreview(t *testing.T) {
	t.Run("test_default_preview_shows_first_two_lines_and_last_two_lines", func(t *testing.T) {
		recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all", "user_message_preview": map[string]any{"first_lines": 2, "last_lines": 2}}, CLIOptions{})
		rendered := c.formatSubmittedUserMessagePreview("line1\nline2\nline3\nline4\nline5\nline6")
		for _, want := range []string{"line1", "line2", "line5", "line6", "(+2 more lines)"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("rendered %q lacks %q", rendered, want)
			}
		}
		if strings.Contains(rendered, "line3") || strings.Contains(rendered, "line4") {
			t.Fatalf("rendered %q", rendered)
		}
	})
	t.Run("test_preview_can_hide_last_lines", func(t *testing.T) {
		recordPrints(t)
		c := configuredCLI(t, map[string]any{"tool_progress": "all", "user_message_preview": map[string]any{"first_lines": 2, "last_lines": 0}}, CLIOptions{})
		rendered := c.formatSubmittedUserMessagePreview("line1\nline2\nline3\nline4\nline5\nline6")
		for _, want := range []string{"line1", "line2", "(+4 more lines)"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("rendered %q lacks %q", rendered, want)
			}
		}
		if strings.Contains(rendered, "line5") || strings.Contains(rendered, "line6") {
			t.Fatalf("rendered %q", rendered)
		}
	})
}

func TestReasoningPreviewBuffering(t *testing.T) {
	verboseCLI := func(t *testing.T) (*CLI, *printRecorder) {
		rec := recordPrints(t)
		t.Setenv("COLUMNS", "80")
		c := bareCLI()
		c.verbose = true
		return c, rec
	}
	t.Run("test_streamed_reasoning_chunks_wait_for_boundary", func(t *testing.T) {
		c, rec := verboseCLI(t)
		c.onReasoning("Let")
		c.onReasoning(" me")
		c.onReasoning(" think")
		if len(rec.raw()) != 0 {
			t.Fatalf("printed early %q", rec.raw())
		}
		c.onReasoning(" about this.\n")
		if printed := rec.raw(); len(printed) != 1 || !strings.Contains(printed[0], "[thinking] Let me think about this.") {
			t.Fatalf("printed %q", printed)
		}
	})
	t.Run("test_pending_reasoning_flushes_when_thinking_stops", func(t *testing.T) {
		c, rec := verboseCLI(t)
		for _, s := range []string{"see", " how", " this", " plays", " out"} {
			c.onReasoning(s)
		}
		if len(rec.raw()) != 0 {
			t.Fatalf("printed early %q", rec.raw())
		}
		c.onThinking("")
		if printed := rec.raw(); len(printed) != 1 || !strings.Contains(printed[0], "[thinking] see how this plays out") {
			t.Fatalf("printed %q", printed)
		}
	})
}

func TestReasoningDisplayModeSelection(t *testing.T) {
	t.Run("test_show_reasoning_non_streaming_uses_final_box_only", func(t *testing.T) {
		c := bareCLI()
		c.showReasoning = true
		if c.currentReasoningCallback() != nil {
			t.Fatal("non streaming reasoning has no live callback")
		}
	})
	t.Run("test_show_reasoning_streaming_uses_live_reasoning_box", func(t *testing.T) {
		c := bareCLI()
		c.showReasoning, c.streamingEnabled = true, true
		var got []string
		prevReasoning := streamReasoningSink
		t.Cleanup(func() { streamReasoningSink = prevReasoning })
		streamReasoningSink = func(_ *CLI, text string) { got = append(got, text) }
		cb := c.currentReasoningCallback()
		if cb == nil {
			t.Fatal("no callback")
		}
		cb("x")
		if len(got) != 1 || got[0] != "x" {
			t.Fatalf("stream callback saw %q", got)
		}
	})
	t.Run("test_verbose_without_show_reasoning_uses_preview_callback", func(t *testing.T) {
		recordPrints(t)
		c := bareCLI()
		c.verbose = true
		cb := c.currentReasoningCallback()
		if cb == nil {
			t.Fatal("no callback")
		}
		cb("x")
		if c.reasoningPreviewBuf != "x" {
			t.Fatalf("preview buffer %q", c.reasoningPreviewBuf)
		}
	})
}

func TestReasoningShownThisTurnFlag(t *testing.T) {
	t.Run("test_streaming_reasoning_sets_turn_flag", func(t *testing.T) {
		recordPrints(t)
		c := bareCLI()
		c.showReasoning, c.streamingEnabled = true, true
		if c.reasoningShownThisTurn {
			t.Fatal("flag set before any reasoning")
		}
		c.streamReasoningDelta("Thinking about it...")
		if !c.reasoningShownThisTurn {
			t.Fatal("streamed reasoning must set the turn flag")
		}
	})
}

func TestBackgroundDispatchPrintsResumeNotice(t *testing.T) {
	rec := recordPrints(t)
	c := bareCLI()
	result, _ := json.Marshal(map[string]any{"status": "dispatched", "mode": "background", "count": 1})
	c.onToolComplete("tc1", "delegate_task", map[string]any{"goal": "x"}, string(result))
	joined := strings.Join(rec.raw(), "\n")
	if !strings.Contains(strings.ToLower(joined), "resume") || !strings.Contains(joined, "it finishes") {
		t.Fatalf("printed %q", joined)
	}
}

func TestSynchronousDelegateResultPrintsNoNotice(t *testing.T) {
	rec := recordPrints(t)
	c := bareCLI()
	result, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"status": "completed", "summary": "done"}}})
	c.onToolComplete("tc3", "delegate_task", map[string]any{"goal": "x"}, string(result))
	for _, p := range rec.raw() {
		if strings.Contains(strings.ToLower(p), "resume") {
			t.Fatalf("printed %q", p)
		}
	}
}

func TestNonDelegateToolPrintsNoNotice(t *testing.T) {
	rec := recordPrints(t)
	c := bareCLI()
	c.onToolComplete("tc4", "read_file", map[string]any{"path": "a"}, `{"ok": true}`)
	for _, p := range rec.raw() {
		if strings.Contains(strings.ToLower(p), "resume") {
			t.Fatalf("printed %q", p)
		}
	}
}

func TestCliNoticeAndWaitCallbacksKeepDefaultOutput(t *testing.T) {
	for name, setting := range map[string]any{"unset": nil, "false": false, "true": true} {
		t.Run(name, func(t *testing.T) {
			rec := recordPrints(t)
			h := os.Getenv("ICEBATH_HOME")
			body := "{}\n"
			if setting != nil {
				body = "display:\n  suppress_warning_notifications: " + map[bool]string{true: "true", false: "false"}[setting.(bool)] + "\n"
			}
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			c := bareCLI()
			a, err := agent.NewAIAgent(c.ctx, agent.AIAgentOptions{Model: "test/model", APIKey: "k", BaseURL: "http://127.0.0.1:9/v1", Provider: "openai",
				Platform: "cli", QuietMode: true, SkipContextFiles: true, SkipMemory: true, NoticeCallback: c.onNotice, ThinkingCallback: c.onThinking})
			if err != nil {
				t.Fatalf("NewAIAgent: %v", err)
			}
			a.SetSuppressStatusOutput(true)
			c.agent = offlineAgent(t, c)
			a.EmitNotice(provider.AgentNotice{Level: "warn", Text: "arbitrary warning wording"})
			a.EmitDiagnosticWait("arbitrary retry wording")
			c.flushCreditNotices()
			suppressed := setting == true
			if (len(rec.raw()) > 0) == suppressed {
				t.Fatalf("printed %q with suppression %v", rec.raw(), suppressed)
			}
			want := "arbitrary retry wording"
			if suppressed {
				want = ""
			}
			if c.spinnerText != want {
				t.Fatalf("spinner %q want %q", c.spinnerText, want)
			}
			a.EmitNotice(provider.AgentNotice{Level: "info", Text: "ordinary progress"})
			a.EmitWaitNotice("⚠ requested progress")
			c.flushCreditNotices()
			printed := rec.raw()
			if len(printed) == 0 || !strings.Contains(printed[len(printed)-1], "ordinary progress") || c.spinnerText != "⚠ requested progress" {
				t.Fatalf("printed %q spinner %q", printed, c.spinnerText)
			}
			_ = config.DiagnosticText("")
		})
	}
}

package agent

import (
	"strings"
	"testing"
)

const completeWithProse = "Use <function> in JS. The arg_key field maps to arg_value.\n" +
	"<tool_call>x<arg_key>a</arg_key><arg_value>1</arg_value></tool_call>\nDone."

func TestNamespacePrefixedToolCallBlockStripped(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	text := "Checking the queue.\n<atem:function_calls>\n<atem:invoke name=\"default.terminal\">\n" +
		"<atem:parameter name=\"command\">echo hi</atem:parameter>\n</atem:invoke>\n</atem:function_calls>"
	out := (*AIAgent)(nil).stripThinkBlocks(text)
	if strings.Contains(out, "atem:") || strings.Contains(out, "function_calls") || !strings.Contains(strings.TrimSpace(out), "Checking the queue.") {
		t.Fatalf("storage strip left %q", out)
	}
}

func TestCutNamespacePrefixedToolCallStrippedToVisiblePrefix(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := (*AIAgent)(nil).stripThinkBlocks("Waiting.\n<atem:function_calls>\n<atem:invoke name=\"default.terminal\">")
	if strings.TrimSpace(out) != "Waiting." {
		t.Fatalf("storage strip left %q", out)
	}
}

func TestCompleteBlockAndInlineProseMentionsUntouched(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := (*AIAgent)(nil).stripThinkBlocks(completeWithProse)
	if !strings.Contains(out, "Use <function> in JS. The arg_key field maps to arg_value.") ||
		!strings.HasSuffix(strings.TrimRight(out, " \t\n\r"), "Done.") || strings.Contains(out, "<tool_call>") {
		t.Fatalf("storage strip left %q", out)
	}
}

func TestFinalResponseStripHidesChineseTags(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := (*AIAgent)(nil).stripThinkBlocks("<反思>内部推理</反思>最终答案\n<推理>未闭合的推理")
	if strings.Contains(out, "内部推理") || strings.Contains(out, "未闭合") || !strings.Contains(out, "最终答案") {
		t.Fatalf("storage strip left %q", out)
	}
}

package app

import (
	"strings"
	"testing"
)

func TestNamespacePrefixedToolCallBlockStripped(t *testing.T) {
	text := "Checking the queue.\n<atem:function_calls>\n<atem:invoke name=\"default.terminal\">\n" +
		"<atem:parameter name=\"command\">echo hi</atem:parameter>\n</atem:invoke>\n</atem:function_calls>"
	out := stripReasoningTags(text)
	if strings.Contains(out, "atem:") || strings.Contains(out, "function_calls") || !strings.Contains(strings.TrimSpace(out), "Checking the queue.") {
		t.Fatalf("display strip left %q", out)
	}
}

func TestCutNamespacePrefixedToolCallStrippedToVisiblePrefix(t *testing.T) {
	if out := stripReasoningTags("Waiting.\n<atem:function_calls>\n<atem:invoke name=\"default.terminal\">"); strings.TrimSpace(out) != "Waiting." {
		t.Fatalf("display strip left %q", out)
	}
}

func TestCompleteBlockAndInlineProseMentionsUntouched(t *testing.T) {
	text := "Use <function> in JS. The arg_key field maps to arg_value.\n" +
		"<tool_call>x<arg_key>a</arg_key><arg_value>1</arg_value></tool_call>\nDone."
	out := stripReasoningTags(text)
	if !strings.Contains(out, "Use <function> in JS. The arg_key field maps to arg_value.") ||
		!strings.HasSuffix(strings.TrimRight(out, " \t\n\r"), "Done.") || strings.Contains(out, "<tool_call>") {
		t.Fatalf("display strip left %q", out)
	}
}

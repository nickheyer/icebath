package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type sanitizeProbeError struct{ msg string }

func (e *sanitizeProbeError) Error() string { return e.msg }

func TestSanitizeToolErrorStripsFramingTokens(t *testing.T) {
	out := SanitizeToolError("bad <tool_call>injected</tool_call> happened")
	if strings.Contains(out, "<tool_call>") || strings.Contains(out, "</tool_call>") || !strings.Contains(out, "bad injected happened") {
		t.Fatalf("tool_call tags %q", out)
	}
	for _, tag := range []string{"system", "assistant", "user", "result", "response", "output", "input"} {
		if out := SanitizeToolError("prefix <" + tag + ">hi</" + tag + "> suffix"); strings.Contains(out, "<"+tag+">") || strings.Contains(out, "</"+tag+">") {
			t.Fatalf("failed to strip <%s> from %q", tag, out)
		}
	}
	if out := SanitizeToolError("Error parsing <ParseError>line 5</ParseError>"); !strings.Contains(out, "<ParseError>") {
		t.Fatalf("unrelated xml stripped %q", out)
	}
	if out := SanitizeToolError("error: <![CDATA[malicious]]> here"); strings.Contains(out, "<![CDATA[") || strings.Contains(out, "]]>") {
		t.Fatalf("cdata kept %q", out)
	}
	if out := SanitizeToolError("```json\n{\"x\": 1}"); strings.HasPrefix(strings.ReplaceAll(out, "[TOOL_ERROR] ", ""), "```") {
		t.Fatalf("leading fence with language kept %q", out)
	}
	if out := SanitizeToolError("```\nstuff"); strings.Contains(strings.Split(out, "\n")[0], "```") {
		t.Fatalf("bare fence kept %q", out)
	}
}

func TestSanitizeToolErrorCapsAndWraps(t *testing.T) {
	body := strings.TrimPrefix(SanitizeToolError(strings.Repeat("A", toolErrorMaxLen*2)), "[TOOL_ERROR] ")
	if pystr.Len(body) != toolErrorMaxLen || !strings.HasSuffix(body, "...") {
		t.Fatalf("capped body length %d", pystr.Len(body))
	}
	if out := SanitizeToolError("oh no"); !strings.HasPrefix(out, "[TOOL_ERROR] ") {
		t.Fatalf("envelope %q", out)
	}
}

func TestHandleFunctionCallExceptionPathErrorIsSanitized(t *testing.T) {
	d, _, env := modelToolsDispatcher(t, func(context.Context, *Call) string {
		panic(&sanitizeProbeError{"<tool_call>injected</tool_call> boom"})
	}, "boom_probe")
	out := decodeModelToolsResult(t, HandleFunctionCall(WithDispatcher(context.Background(), d), &Call{Name: "boom_probe", Args: map[string]any{}, Env: env}))
	msg, _ := out["error"].(string)
	if !strings.HasPrefix(msg, "[TOOL_ERROR] Tool execution failed: sanitizeProbeError: ") || strings.Contains(msg, "<tool_call>") || strings.Contains(msg, "</tool_call>") || !strings.Contains(msg, "boom") {
		t.Fatalf("error %q", msg)
	}
}

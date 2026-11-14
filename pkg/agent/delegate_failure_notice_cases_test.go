package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// test_subagent_failure_notice.py TestCleanErrorText
func TestCleanErrorText(t *testing.T) {
	t.Run("test_single_line_passthrough", func(t *testing.T) {
		if got := cleanErrorText("Error code: 404 - model not found", 200); got != "Error code: 404 - model not found" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_traceback_takes_last_line", func(t *testing.T) {
		tb := "Traceback (most recent call last):\n  File \"x.py\", line 1, in <module>\n    raise RuntimeError('boom')\nRuntimeError: boom"
		if got := cleanErrorText(tb, 200); got != "RuntimeError: boom" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_multiline_non_traceback_takes_first_line", func(t *testing.T) {
		if got := cleanErrorText("first line\nsecond line", 200); got != "first line" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_caps_length", func(t *testing.T) {
		out := cleanErrorText(strings.Repeat("x", 500), 100)
		if pystr.Len(out) != 100 || !strings.HasSuffix(out, "...") {
			t.Fatalf("got %d chars %q", pystr.Len(out), out)
		}
	})
	t.Run("test_empty_and_none", func(t *testing.T) {
		for _, in := range []any{"", nil, "   \n  "} {
			if got := cleanErrorText(in, 200); got != "" {
				t.Fatalf("%q gave %q", in, got)
			}
		}
	})
}

// test_subagent_failure_notice.py TestFormatSubagentFailureLine
func TestFormatSubagentFailureLine(t *testing.T) {
	t.Run("test_failed_with_goal_error_duration", func(t *testing.T) {
		line := formatSubagentFailureLine("research competitor pricing", "failed", "Error code: 404 - model not found", 12.4, nil)
		if !strings.Contains(line, `"research competitor pricing"`) || !strings.Contains(line, "404") {
			t.Fatalf("line %q", line)
		}
	})
	t.Run("test_timeout_verb", func(t *testing.T) {
		if line := formatSubagentFailureLine("do a thing", "timeout", nil, nil, nil); !strings.Contains(line, "timed out") {
			t.Fatalf("line %q", line)
		}
	})
	t.Run("test_long_goal_truncated", func(t *testing.T) {
		if line := formatSubagentFailureLine(strings.Repeat("g", 200), "failed", nil, nil, nil); strings.Contains(line, strings.Repeat("g", 61)) {
			t.Fatalf("line %q", line)
		}
	})
	t.Run("test_no_goal_no_error", func(t *testing.T) {
		if line := formatSubagentFailureLine("", "error", nil, nil, nil); strings.Contains(line, `"`) {
			t.Fatalf("line %q", line)
		}
	})
	t.Run("test_multiline_goal_flattened", func(t *testing.T) {
		if line := formatSubagentFailureLine("a\nb", "failed", nil, nil, nil); strings.Contains(line, "\n") {
			t.Fatalf("line %q", line)
		}
	})
}

package agent

import (
	"strings"
	"testing"
)

func TestClassifiedReasonReplacesRawExceptionText(t *testing.T) {
	line := formatSubagentFailureLine("research pricing", "failed", "Error code: 429 - {'error': 'rate limited'}", 12, "rate_limit")
	if !strings.Contains(line, "rate-limited") || strings.Contains(line, "Error code") || strings.Contains(line, "429") || !strings.Contains(line, "/agents") {
		t.Fatalf("line %q", line)
	}
}

func TestUnclassifiedFailureKeepsTheCleanedErrorAsCause(t *testing.T) {
	if got := describeSubagentFailure(nil, "Traceback...\nKeyError: 'x'", 200); got != "KeyError: 'x'" {
		t.Fatalf("describe %q", got)
	}
	line := formatSubagentFailureLine("g", "failed", "KeyError: 'x'", nil, nil)
	if !strings.Contains(line, "KeyError: 'x'") || !strings.Contains(line, "/agents") {
		t.Fatalf("line %q", line)
	}
}

func TestTimeoutLineNamesOutcomeAndTheTimeoutSettingWithoutDuplicatingDuration(t *testing.T) {
	raw := "Subagent timed out after 600s with 4 API call(s) completed — likely stuck on a slow API call."
	line := formatSubagentFailureLine("scan the repo", "timeout", raw, 600, nil)
	if !strings.Contains(line, "delegation.child_timeout_seconds") || strings.Count(line, "600") != 0 || !strings.Contains(line, "10 min") || strings.Contains(line, "API call") {
		t.Fatalf("line %q", line)
	}
}

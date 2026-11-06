package plugins

import (
	"context"
	"strings"
	"testing"
)

func TestConsentPythonDepsExportSharesAnswersAndReportsRefusals(t *testing.T) {
	var prompts []string
	answer := "n"
	restore := SwapTerminalIO(func() bool { return true }, func(prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return answer, nil
	})
	var out strings.Builder
	console := NewConsole(&out)
	ctx := WithSharedDependencyAnswers(context.Background())
	for range 2 {
		if ok, reason := ConsentPythonDeps(ctx, "twin", []string{"twin-client"}, console, false); ok || reason != "dependency install declined" {
			t.Fatalf("%v %q", ok, reason)
		}
	}
	if len(prompts) != 1 || !strings.Contains(out.String(), "Reusing your answer for the first profile: no") || !strings.Contains(out.String(), "  - twin-client") {
		t.Fatalf("prompts %v out %s", prompts, out.String())
	}
	answer = "y"
	if ok, reason := ConsentPythonDeps(context.Background(), "twin", nil, console, false); !ok || reason != "" || len(prompts) != 2 {
		t.Fatalf("%v %q %v", ok, reason, prompts)
	}
	if ok, reason := ConsentPythonDeps(context.Background(), "twin", nil, console, true); !ok || reason != "" || len(prompts) != 2 {
		t.Fatalf("assumed consent %v %q", ok, reason)
	}
	restore()
	restoreAgain := SwapTerminalIO(func() bool { return false }, func(string) (string, error) {
		t.Fatal("a non interactive gate must not prompt")
		return "", nil
	})
	defer restoreAgain()
	if ok, reason := ConsentPythonDeps(context.Background(), "twin", nil, console, false); ok || reason != "dependency install skipped (non-interactive)" {
		t.Fatalf("%v %q", ok, reason)
	}
}

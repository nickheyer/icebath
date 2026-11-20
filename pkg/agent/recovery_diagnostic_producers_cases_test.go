package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Writes the warning presentation policy of a private home, true when warnings are suppressed
func diagnosticPolicyHome(t *testing.T, policy *bool) bool {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(home, "managed"))
	cfg := "{}"
	if policy != nil {
		cfg = "display:\n  suppress_warning_notifications: " + map[bool]string{true: "true", false: "false"}[*policy] + "\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return policy != nil && *policy
}

func TestOuterLoopDiagnosticPreservesVerdictAndTrace(t *testing.T) {
	for _, policy := range []*bool{nil, new(false), new(true)} {
		for _, local := range []bool{false, true} {
			suppressed := diagnosticPolicyHome(t, policy)
			var printed []string
			a := &AIAgent{model: "fixture", platform: "cli", maxIterations: 1}
			a.printFn = func(s string) { printed = append(printed, s) }
			a.statusCallback = func(kind, text string) {}
			saved := localProcessingModules
			localProcessingModules = map[string]bool{}
			if local {
				localProcessingModules = map[string]bool{"recovery_diagnostic_producers_cases_test": true}
			}
			logs := captureInfoLogs(t)
			var traced error
			func() {
				defer func() { traced = RecoveredOuterLoopError(recover()) }()
				panic(errors.New("raw engine diagnostic detail"))
			}()
			messages := []*state.Message{}
			v, err := a.handleOuterLoopError(context.Background(), traced, 0, 1, &messages, []*state.Message{}, nil, false, "")
			localProcessingModules = saved
			if err != nil {
				t.Fatal(err)
			}
			output := strings.Join(printed, "\n")
			if strings.Contains(output, "raw engine diagnostic detail") == suppressed {
				t.Fatalf("policy %v local %v: printed %q", suppressed, local, output)
			}
			if v.Action != "break" || v.FinalResponse == "" || v.FinalResponse == nil || v.Failed != !local {
				t.Fatalf("policy %v local %v: verdict %+v", suppressed, local, v)
			}
			if text := logs.String(); !strings.Contains(text, "Outer loop error") || !strings.Contains(text, "raw engine diagnostic detail") {
				t.Fatalf("policy %v local %v: log %q", suppressed, local, text)
			}
		}
	}
}

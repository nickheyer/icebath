package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs a one shot turn whose agent answers response and result, returning the exit code and usage report
func oneshotExitRun(t *testing.T, response string, result map[string]any) (int, map[string]any) {
	t.Helper()
	h := oneshotTestEnv(t)
	usage := filepath.Join(h, "usage.json")
	swapSeam(t, &oneshotRunAgent, func(context.Context, *tools.Env, string, oneshotRunOptions) (string, map[string]any, error) {
		copied := map[string]any{}
		for k, v := range result {
			copied[k] = v
		}
		return response, copied, nil
	})
	code, err := RunOneshot(context.Background(), "q", OneshotOptions{UsageFile: usage})
	if err != nil {
		t.Fatal(err)
	}
	report := map[string]any{}
	if data, err := os.ReadFile(usage); err == nil {
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
	}
	return code, report
}

func TestIncompleteRunWithTextExitsNonzeroAndReportsWhy(t *testing.T) {
	code, report := oneshotExitRun(t, "Got as far as step 2.", map[string]any{"final_response": "Got as far as step 2.",
		"completed": false, "partial": true, "turn_exit_reason": "iteration_limit"})
	if code != 2 || report["completed"] != false || report["partial"] != true || report["turn_exit_reason"] != "iteration_limit" {
		t.Fatalf("code %d report %v", code, report)
	}
	code, report = oneshotExitRun(t, "Stopping.", map[string]any{"final_response": "Stopping.", "completed": false, "interrupted": true})
	if code != 130 || report["interrupted"] != true {
		t.Fatalf("code %d report %v", code, report)
	}
	code, _ = oneshotExitRun(t, "Provider returned 401.", map[string]any{"final_response": "Provider returned 401.", "completed": false, "failed": true})
	if code != 2 {
		t.Fatalf("code %d", code)
	}
}

func TestCompletedRunStillExitsZero(t *testing.T) {
	code, _ := oneshotExitRun(t, "Paris.", map[string]any{"final_response": "Paris.", "completed": true, "failed": false})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
}

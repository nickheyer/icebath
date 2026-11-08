package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home whose config holds only the display section with the warning suppression setting when given
func configureSuppressWarnings(t *testing.T, home string, setting any) *tools.Env {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	display := map[string]any{}
	if setting != nil {
		display["suppress_warning_notifications"] = setting
	}
	data, err := json.Marshal(map[string]any{"display": display})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return &tools.Env{Home: home, Config: config.Defaults(), Secrets: config.NewSecrets(home), Values: &sync.Map{}}
}

// Child reporting one of five API calls whose every conversation run fails with msg
func crashingDelegateChild(t *testing.T, env *tools.Env, msg string) *AIAgent {
	t.Helper()
	child := &AIAgent{model: "test-model", env: env, delegateDepth: 1, platform: "subagent", quietMode: true, suppressStatusOutput: true,
		iterationBudget: NewIterationBudget(5), maxIterations: 5, apiCallCount: 1}
	prev := conversationLoopRun
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a != child {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		return nil, errors.New(msg)
	}
	t.Cleanup(func() { conversationLoopRun = prev })
	return child
}

type syncFailureRow struct {
	setting any
	lines   []string
	events  []string
	result  string
}

// test_delegate_sync_failure_echo_policy.py test_sync_failure_real_run_to_spinner_three_modes
func TestSyncFailureRealRunToSpinnerThreeModes(t *testing.T) {
	var rows []syncFailureRow
	for _, setting := range []any{nil, false, true} {
		env := configureSuppressWarnings(t, filepath.Join(t.TempDir(), fmt.Sprintf("sync-%v", setting)), setting)
		var buffer bytes.Buffer
		prevStdout := statusStdout
		statusStdout = &buffer
		spinner := newKawaiiSpinner(context.Background(), "delegating", "dots", nil)
		statusStdout = prevStdout
		parent, rec := &AIAgent{sessionID: "parent", env: env}, &progressRecorder{}
		parent.delegateSpinner = spinner
		parent.toolProgressCallback = rec.cb
		child := crashingDelegateChild(t, env, "SYNC_REAL_CHILD_CRASH")
		child.toolProgressCallback = buildChildProgressCallback(0, "actual failing child", parent, 1, "", "", nil, "", nil, nil, nil)
		cfg := NotificationConfigSnapshot(context.Background())
		display, _ := cfg["display"].(map[string]any)
		if got := display["suppress_warning_notifications"]; got != setting {
			t.Fatalf("setting %v snapshot reads %v", setting, got)
		}
		restore := parent.NotificationPolicySnapshot("cli", cfg)
		result := runSingleChild(context.Background(), 0, "actual failing child", child, parent, "", nil, nil)
		restore()
		text, err := pyjson.Dumps(result)
		if err != nil {
			t.Fatal(err)
		}
		if status := entryText(result, "status"); (status != "error" && status != "failed") || !strings.Contains(text, "SYNC_REAL_CHILD_CRASH") {
			t.Fatalf("setting %v result %s", setting, text)
		}
		rows = append(rows, syncFailureRow{setting: setting, lines: strings.Split(strings.TrimRight(buffer.String(), "\n"), "\n"), events: rec.events(), result: text})
	}
	echoed := func(row syncFailureRow) bool {
		for _, line := range row.lines {
			if strings.Contains(line, "SYNC_REAL_CHILD_CRASH") {
				return true
			}
		}
		return false
	}
	if !echoed(rows[0]) || !echoed(rows[1]) {
		t.Fatalf("absent and false settings echo the crash, got %q and %q", rows[0].lines, rows[1].lines)
	}
	if echoed(rows[2]) {
		t.Fatalf("true setting suppresses the echo, got %q", rows[2].lines)
	}
	for _, row := range rows {
		relayed := false
		for _, event := range row.events {
			relayed = relayed || event == "subagent.complete"
		}
		if !relayed {
			t.Fatalf("setting %v events %v", row.setting, row.events)
		}
	}
}

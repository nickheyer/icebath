package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runner serving one telegram adapter with the notification mode written to its config
func notifyModeRunner(t *testing.T, mode string) (*GatewayRunner, *updateTestAdapter) {
	t.Helper()
	r, adapter, hermesHome := updateRunner(t)
	writeHomeFile(t, hermesHome, "config.yaml", []byte(fmt.Sprintf("display:\n  background_process_notifications: %s\n", mode)))
	return r, adapter
}

// Watcher descriptor of a telegram chat, the hermes _watcher_dict
func watcherDict(sessionID string) map[string]any {
	return map[string]any{"session_id": sessionID, "check_interval": 0, "platform": "telegram", "chat_id": "123"}
}

// Finished process recorded in the shared registry and forgotten when the case ends
func finishedProcess(t *testing.T, id, command, output string, exitCode int) {
	t.Helper()
	code := exitCode
	s := tools.NewProcessSession(id, command)
	s.OutputBuffer, s.Exited, s.ExitCode, s.StartedAt = output, true, &code, float64(time.Now().UnixNano())/1e9
	tools.DefaultProcessRegistry().InsertFinished(s)
	t.Cleanup(func() { tools.DefaultProcessRegistry().ForgetFinished(id) })
}

// Real background shell process of the shared registry, killed when the case ends
func spawnedProcess(t *testing.T, command string) string {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	env := &tools.Env{Home: home.Dir(context.Background())}
	s, err := tools.DefaultProcessRegistry().SpawnLocal(context.Background(), env, command, tools.SpawnLocalOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tools.DefaultProcessRegistry().KillProcess(context.Background(), env, s.ID, "test", false)
		tools.DefaultProcessRegistry().ForgetFinished(s.ID)
	})
	return s.ID
}

// Marks a completion consumed the way process(action=log) does
func consumeCompletion(id string) {
	tools.DefaultProcessRegistry().ReadLog(&tools.Env{Home: home.Dir(context.Background())}, id, nil, 200)
}

func TestConsumedCompletionSkipsRawNotification(t *testing.T) {
	r, adapter := notifyModeRunner(t, "all")
	finishedProcess(t, "proc_test", "sleep 1; echo done", "done\n", 0)
	consumeCompletion("proc_test")
	watcher := watcherDict("proc_test")
	watcher["notify_on_complete"] = true
	if err := r.runProcessWatcher(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	if sent := adapter.sends(); len(sent) != 0 {
		t.Fatalf("sent = %q", sent)
	}
}

func TestConsumedCompletionSkipsRawNotificationWithoutAgentNotify(t *testing.T) {
	r, adapter := notifyModeRunner(t, "all")
	finishedProcess(t, "proc_test2", "echo done", "done\n", 0)
	consumeCompletion("proc_test2")
	if err := r.runProcessWatcher(context.Background(), watcherDict("proc_test2")); err != nil {
		t.Fatal(err)
	}
	if sent := adapter.sends(); len(sent) != 0 {
		t.Fatalf("sent = %q", sent)
	}
}

func TestConciseModeNoInterimOutputUpdates(t *testing.T) {
	r, adapter := notifyModeRunner(t, "concise")
	id := spawnedProcess(t, "printf 'chunk one\\n'; sleep 0.6; printf 'chunk two\\n'")
	watcher := watcherDict(id)
	watcher["check_interval"] = 0.2
	if err := r.runProcessWatcher(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	sent := adapter.sends()
	if len(sent) != 1 {
		t.Fatalf("sent = %q", sent)
	}
	if strings.Contains(sent[0], "is still running") || !strings.HasPrefix(sent[0], "✅ Background task finished") {
		t.Fatalf("sent = %q", sent[0])
	}
}

func TestRawOutputModesAreHumanFacing(t *testing.T) {
	r, adapter := notifyModeRunner(t, "all")
	id := spawnedProcess(t, "printf '\\033[32mstep 1 ok\\033[0m\\n'; sleep 0.9; printf '\\033[31mlinker error\\033[0m\\n'; exit 2")
	watcher := watcherDict(id)
	watcher["check_interval"] = 0.3
	if err := r.runProcessWatcher(context.Background(), watcher); err != nil {
		t.Fatal(err)
	}
	sent := adapter.sends()
	if len(sent) != 2 {
		t.Fatalf("sent = %q", sent)
	}
	interim, final := sent[0], sent[1]
	if !strings.HasPrefix(interim, "⏳ Background task still running") || !strings.Contains(interim, "step 1 ok") {
		t.Fatalf("interim = %q", interim)
	}
	if !strings.HasPrefix(final, "❌ Background task failed") || !strings.Contains(final, "exit 2") || !strings.Contains(final, "linker error") {
		t.Fatalf("final = %q", final)
	}
	for _, text := range sent {
		if strings.Contains(text, id) || strings.Contains(text, "[Background process") || strings.Contains(text, "~") || strings.Contains(text, "\x1b[") {
			t.Fatalf("debug text leaked: %q", text)
		}
		if !strings.Contains(text, "printf") {
			t.Fatalf("command missing: %q", text)
		}
	}
}

func TestSuccessIsOneLineWithoutOutput(t *testing.T) {
	duration := 754.0
	text := formatConciseProcessNotification(context.Background(), "proc_abc", "python3 scan_fleet.py --all", intp(0),
		"1300\n1400\n1500\n{...huge json...}", &duration)
	if !strings.HasPrefix(text, "✅ Background task finished") || !strings.Contains(text, "scan_fleet.py") || !strings.Contains(text, "12m 34s") {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(text, "1300") || strings.Contains(text, "\n") {
		t.Fatalf("text = %q", text)
	}
}

func TestFailureAppendsShortTail(t *testing.T) {
	var lines []string
	for i := range 50 {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	out := strings.Join(lines, "\n") + "\nTraceback: boom"
	text := formatConciseProcessNotification(context.Background(), "proc_abc", "make build", intp(2), out, nil)
	if !strings.HasPrefix(text, "❌ Background task failed") || !strings.Contains(text, "exit 2") || !strings.Contains(text, "Traceback: boom") {
		t.Fatalf("text = %q", text)
	}
	if !strings.Contains(text, "rerun") || strings.Contains(text, "line0") {
		t.Fatalf("text = %q", text)
	}
}

func TestLongCommandIsTruncated(t *testing.T) {
	text := formatConciseProcessNotification(context.Background(), "proc_abc", strings.Repeat("x", 300), intp(0), "", nil)
	if !strings.Contains(text, "…") || len([]rune(text)) >= 200 {
		t.Fatalf("text = %q", text)
	}
}

// Pointer to an int literal
func intp(v int) *int { return &v }

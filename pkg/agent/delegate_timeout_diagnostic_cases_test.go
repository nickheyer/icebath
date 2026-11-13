package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Child of test_delegate_subagent_timeout_diagnostic.py _StubChild with the given api call count
func timeoutStubChild(env *tools.Env, apiCalls int) *AIAgent {
	child := interruptibleChild()
	child.env, child.subagentID, child.delegateDepth, child.delegateRole = env, "sa-0-stubabc", 1, "leaf"
	child.model, child.provider, child.apiMode, child.baseURL = "test/model", "testprov", "chat_completions", "https://example.test/v1"
	child.maxIterations, child.platform, child.ephemeralSystemPrompt = 30, "cli", "sys prompt"
	child.enabledToolsets = []string{"web", "terminal"}
	child.validToolNames = map[string]bool{"web_search": true, "terminal": true}
	child.tools = []any{map[string]any{"name": "web_search", "description": "search"}, map[string]any{"name": "terminal", "description": "shell"}}
	child.apiCallCount.Store(int64(apiCalls))
	child.iterationBudget.Store(NewIterationBudget(30))
	return child
}

// Child conversation that blocks until the test releases it, the stub child's hang
func hangChildConversation(t *testing.T, child *AIAgent, hang time.Duration) {
	t.Helper()
	release := make(chan struct{})
	exited := make(chan struct{})
	var exitOnce sync.Once
	prev := conversationLoopRun
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a != child {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		defer exitOnce.Do(func() { close(exited) })
		select {
		case <-release:
		case <-time.After(hang):
		}
		return map[string]any{"final_response": "", "completed": false, "api_calls": int(a.apiCallCount.Load())}, nil
	}
	t.Cleanup(func() {
		close(release)
		awaitWorkerExit(t, exited)
		conversationLoopRun = prev
	})
}

func TestDumpSubagentTimeoutDiagnosticReturnsNoneOnUnwritableLogsDir(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "does-not-exist", ".icebath")
	if err := os.MkdirAll(bogus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bogus, "logs"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", bogus)
	env := &tools.Env{Home: bogus}
	child := timeoutStubChild(env, 0)
	result := dumpSubagentTimeoutDiagnostic(context.Background(), child, 0, 300.0, 300.0, "", "x")
	if result != "" {
		if _, err := os.Stat(result); err != nil {
			t.Fatalf("path %q does not exist: %v", result, err)
		}
	}
}

func TestDumpSubagentTimeoutDiagnosticTimeoutDiagnosticMarksLongGoalAsNonOriginal(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	child := timeoutStubChild(env, 0)
	path := dumpSubagentTimeoutDiagnostic(context.Background(), child, 0, 300.0, 300.0, "", strings.Repeat("g", 1200))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !tools.HasCompressionMarker(text) || !strings.Contains(text, strings.Repeat("g", 100)) || strings.Contains(text, "...[truncated]") {
		t.Fatalf("diagnostic %q", text)
	}
}

func TestRunSingleChildTimeoutDumpZeroApiCallsWritesDumpAndSurfacesPath(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	livenessKnobs(t, capSeconds(0.3))
	child := timeoutStubChild(env, 0)
	hangChildConversation(t, child, 10*time.Second)
	parent := &AIAgent{env: env}
	result := runSingleChild(tools.WithToolEnv(context.Background(), env), 0, "test goal", child, parent, "", nil, nil)
	apiCalls, _ := result.Get("api_calls")
	if entryText(result, "status") != "timeout" || apiCalls != 0 {
		t.Fatalf("result %v", result)
	}
	dumpPath := entryText(result, "diagnostic_path")
	st, err := os.Stat(dumpPath)
	if dumpPath == "" || err != nil || !st.Mode().IsRegular() || filepath.Dir(dumpPath) != filepath.Join(env.Home, "logs") {
		t.Fatalf("diagnostic path %q %v", dumpPath, err)
	}
	errText := entryText(result, "error")
	if !strings.Contains(errText, "without making any API call") || !strings.Contains(errText, "Diagnostic:") || !strings.Contains(errText, dumpPath) {
		t.Fatalf("error %q", errText)
	}
}

func TestRunSingleChildTimeoutDumpNonTimeoutErrorHasNullTimeoutMetadata(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	livenessKnobs(t, capSeconds(30))
	child := timeoutStubChild(env, 1)
	prev := conversationLoopRun
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a != child {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		return nil, errors.New("child crashed")
	}
	t.Cleanup(func() { conversationLoopRun = prev })
	result := runSingleChild(tools.WithToolEnv(context.Background(), env), 0, "test goal", child, &AIAgent{env: env}, "", nil, nil)
	if entryText(result, "status") != "error" {
		t.Fatalf("result %v", result)
	}
	for _, key := range []string{"timeout_seconds", "timed_out_after_seconds", "timeout_phase"} {
		v, ok := result.Get(key)
		if !ok || v != nil {
			t.Fatalf("%s present %v value %v", key, ok, v)
		}
	}
}

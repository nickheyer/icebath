package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session with three persisted user turns and replies on a live agent, the store serving as the launch store
func undoSessionWithHistory(t *testing.T) (string, *tuigateway.Session) {
	t.Helper()
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sessionKey := "tui-undo-1"
	if err := db.CreateSession(state.NewSession{ID: sessionKey, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1", "2", "3"} {
		for _, m := range []map[string]any{{"role": "user", "content": "question " + n}, {"role": "assistant", "content": "answer " + n}} {
			if _, err := db.AppendMessage(ctx, sessionKey, m, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	history, err := db.GetMessagesAsConversation(ctx, sessionKey, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	useSharedDB(t, db)
	sid := "sid-undo"
	session := liveSession(t, sid, map[string]any{"session_key": sessionKey, "history": messagesAsHistory(history),
		"history_version": int64(0), "running": false, "agent": testAgent(t, agent.AIAgentOptions{Model: "test-model"}),
		"attached_images": []any{}, "cols": 120})
	return sid, session
}

func TestUndoReturnsPrefillWithTargetText(t *testing.T) {
	sid, session := undoSessionWithHistory(t)
	result := resultOf(t, callMethod(t, "command.dispatch", map[string]any{"session_id": sid, "name": "undo", "arg": ""}))
	if result["type"] != "prefill" {
		t.Fatalf("type = %v", result["type"])
	}
	if result["message"] != "question 3" {
		t.Fatalf("message = %v", result["message"])
	}
	history := historyList(session.Get("history"))
	if len(history) == 0 {
		t.Fatal("history emptied")
	}
	for _, m := range history {
		if _, has := m.(map[string]any)["_row_id"]; !has {
			t.Fatalf("row without _row_id: %v", m)
		}
	}
}

// Registers a minimal session whose attached cwd the init handler falls back to
func registerInitSession(t *testing.T, sid, sessionCwd string) {
	t.Helper()
	liveSession(t, sid, map[string]any{"agent": nil, "cwd": sessionCwd, "history": []any{}, "session_key": sid})
}

// Message of the /init directive command.dispatch answers for a session
func dispatchInit(t *testing.T, sid string) string {
	t.Helper()
	result := resultOf(t, callMethod(t, "command.dispatch", map[string]any{"name": "init", "arg": "", "session_id": sid}))
	return result["message"].(string)
}

func TestInitTargetsTheSessionsRecordedCwd(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	workspace, launchDir := filepath.Join(tmp, "proj"), filepath.Join(tmp, "home")
	for _, d := range []string{workspace, launchDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(launchDir)
	sid := "init-live-record"
	registerInitSession(t, sid, launchDir)
	tools.RecordSessionCwd(nil, sid, workspace)
	t.Cleanup(func() { tools.ClearSessionCwd(nil, sid) })
	prompt := dispatchInit(t, sid)
	if !strings.Contains(prompt, "for the project at: "+workspace) {
		t.Fatalf("prompt does not target the workspace: %q", prompt)
	}
	if strings.Contains(prompt, launchDir) {
		t.Fatalf("prompt names the launch dir: %q", prompt)
	}
}

func TestInitIgnoresAStaleRecordInFavourOfTheSessionWorkspace(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	workspace, launchDir, gone := filepath.Join(tmp, "proj"), filepath.Join(tmp, "home"), filepath.Join(tmp, "removed-worktree")
	for _, d := range []string{workspace, launchDir, gone} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(launchDir)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	sid := "init-stale-record"
	registerInitSession(t, sid, workspace)
	tools.RecordSessionCwd(nil, sid, gone)
	t.Cleanup(func() { tools.ClearSessionCwd(nil, sid) })
	prompt := dispatchInit(t, sid)
	if !strings.Contains(prompt, "for the project at: "+workspace) {
		t.Fatalf("prompt does not target the workspace: %q", prompt)
	}
	if strings.Contains(prompt, gone) {
		t.Fatalf("prompt names the stale record: %q", prompt)
	}
}

func TestQuickCommandExecSurvivesNonUtf8ChildOutput(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	cfg := "quick_commands:\n  badbytes:\n    type: exec\n    command: \"printf '\\\\377\\\\376 tail-ok'\"\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := handleRequest(context.Background(), map[string]any{"id": "enc", "method": "command.dispatch", "params": map[string]any{"name": "badbytes"}})
	if _, failed := resp["error"]; failed {
		t.Fatalf("error response: %v", resp)
	}
	result := resp["result"].(map[string]any)
	if result["type"] != "exec" {
		t.Fatalf("type = %v", result["type"])
	}
	if output := result["output"].(string); !strings.Contains(output, "tail-ok") || !strings.Contains(output, "�") {
		t.Fatalf("output = %q", output)
	}
}

// Idle session whose loop and goal stores live in an isolated home, cleared afterwards
func slashSession(t *testing.T, sid, sessionKey string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	goals.ClearSessionDBCache()
	liveSession(t, sid, map[string]any{"session_key": sessionKey, "history": []any{}, "history_version": int64(0),
		"running": false, "attached_images": []any{}, "cols": 120})
	t.Cleanup(func() {
		ctx := context.Background()
		goals.NewGoalManager(ctx, sessionKey, 0).Clear()
		agent.NewLoopManager(ctx, sessionKey).Clear(ctx)
		goals.ClearSessionDBCache()
	})
}

func TestLoopSetPersists(t *testing.T) {
	slashSession(t, "sid-loop-test", "tui-loop-session-1")
	result := resultOf(t, callMethod(t, "command.dispatch", map[string]any{"name": "loop", "arg": "5m check the deploy", "session_id": "sid-loop-test"}))
	if result["type"] != "exec" {
		t.Fatalf("type = %v", result["type"])
	}
	st := agent.NewLoopManager(context.Background(), "tui-loop-session-1").State()
	if st == nil || st.Prompt != "check the deploy" || st.Status != "active" || st.IntervalSeconds != 300.0 {
		t.Fatalf("loop state %+v", st)
	}
}

func TestLoopPauseResumeStop(t *testing.T) {
	slashSession(t, "sid-loop-test", "tui-loop-session-1")
	call := func(arg string) map[string]any {
		return callMethod(t, "command.dispatch", map[string]any{"name": "loop", "arg": arg, "session_id": "sid-loop-test"})
	}
	call("5m poll CI")
	for _, step := range [][2]string{{"pause", "paused"}, {"resume", "resumed"}, {"stop", "stopped"}} {
		output, _ := resultOf(t, call(step[0]))["output"].(string)
		if !strings.Contains(strings.ToLower(output), step[1]) {
			t.Fatalf("%s output %q", step[0], output)
		}
	}
	if agent.NewLoopManager(context.Background(), "tui-loop-session-1").HasLoop() {
		t.Fatal("loop survived stop")
	}
}

func TestLoopRequiresSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resp := callMethod(t, "command.dispatch", map[string]any{"name": "loop", "arg": "5m x", "session_id": "unknown"})
	errObj, _ := resp["error"].(map[string]any)
	if errObj["code"] != 4001 {
		t.Fatalf("response %v", resp)
	}
}

func TestGoalResumeAfterBudgetExhaustionDispatchesContinuation(t *testing.T) {
	slashSession(t, "sid-test", "tui-goal-session-1")
	ctx := context.Background()
	goals.SaveGoal(ctx, "tui-goal-session-1", &goals.GoalState{Goal: "finish the benchmark", Status: "paused", TurnsUsed: 1,
		MaxTurns: 1, CreatedAt: 100.0, LastTurnAt: 200.0, PausedReason: "turn budget exhausted (1/1)"})
	result := resultOf(t, callMethod(t, "command.dispatch", map[string]any{"name": "goal", "arg": "resume", "session_id": "sid-test"}))
	if result["type"] != "send" || strings.TrimSpace(result["message"].(string)) == "" || result["display"] != "/goal resume" {
		t.Fatalf("result %v", result)
	}
	st := goals.NewGoalManager(ctx, "tui-goal-session-1", 0).State()
	if st.Status != "active" || st.TurnsUsed != 0 {
		t.Fatalf("goal state %+v", st)
	}
}

func TestGoalResumeWithoutGoalStaysExec(t *testing.T) {
	slashSession(t, "sid-test", "tui-goal-session-1")
	result := resultOf(t, callMethod(t, "command.dispatch", map[string]any{"name": "goal", "arg": "resume", "session_id": "sid-test"}))
	if result["type"] != "exec" {
		t.Fatalf("type = %v", result["type"])
	}
}

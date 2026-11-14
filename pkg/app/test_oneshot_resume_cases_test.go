package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session store under dir with one cli session holding the given role and content pairs
func oneshotDBWithSession(t *testing.T, dir, sid string, messages ...[2]string) *state.SessionDB {
	t.Helper()
	if os.Getenv("ICEBATH_ONESHOT_TEST_HOME") == "" {
		tuiTestHome(t)
	}
	db, err := state.NewSessionDB(context.Background(), filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		oneshotAppend(t, db, sid, m[0], m[1])
	}
	return db
}

// Appends one message to a session of the store
func oneshotAppend(t *testing.T, db *state.SessionDB, sid, role, content string) {
	t.Helper()
	if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": role, "content": content}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

// Session row of the store at dir read through a fresh handle
func oneshotSessionRow(t *testing.T, dir, sid string) map[string]any {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil || row == nil {
		t.Fatalf("session %s: %v", sid, err)
	}
	return row.Vals
}

// Isolated home and process env a one shot run mutates, restored when the test ends
func oneshotTestEnv(t *testing.T) string {
	t.Helper()
	h := tuiTestHome(t)
	t.Setenv("ICEBATH_ONESHOT_TEST_HOME", h)
	for _, name := range []string{"ICEBATH_YOLO_MODE", "ICEBATH_ACCEPT_HOOKS", "ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_INFERENCE_MODEL", "ICEBATH_KANBAN_TASK"} {
		t.Setenv(name, "")
	}
	t.Cleanup(func() { logging.Disable(logging.LevelNotSet) })
	return h
}

func TestLoadResumeTargetNoResumeIsANoop(t *testing.T) {
	db := oneshotDBWithSession(t, t.TempDir(), "s1")
	defer db.Close()
	sid, history, meta, err := loadOneshotResumeTarget(context.Background(), db, "")
	if err != nil || sid != "" || history == nil || len(history) != 0 || meta != nil {
		t.Fatalf("got %q %v %v %v", sid, history, meta, err)
	}
}

func TestLoadResumeTargetMissingStoreRaisesRatherThanSilentFresh(t *testing.T) {
	tuiTestHome(t)
	_, _, _, err := loadOneshotResumeTarget(context.Background(), nil, "s1")
	if err == nil || !strings.Contains(err.Error(), "session store unavailable") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadResumeTargetUnknownSessionRaises(t *testing.T) {
	db := oneshotDBWithSession(t, t.TempDir(), "s1")
	defer db.Close()
	_, _, _, err := loadOneshotResumeTarget(context.Background(), db, "missing-sid")
	if err == nil || !strings.Contains(err.Error(), "session not found: missing-sid") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadResumeTargetReturnsHistoryForStoredSession(t *testing.T) {
	db := oneshotDBWithSession(t, t.TempDir(), "s1",
		[2]string{"user", "Remember the secret word ZEBRA42"}, [2]string{"assistant", "I've noted the secret word: ZEBRA42."})
	defer db.Close()
	sid, history, meta, err := loadOneshotResumeTarget(context.Background(), db, "s1")
	if err != nil || sid != "s1" {
		t.Fatalf("got %q %v", sid, err)
	}
	if len(history) != 2 || history[0]["role"] != "user" || history[1]["role"] != "assistant" {
		t.Fatalf("history %v", history)
	}
	if history[0]["content"] != "Remember the secret word ZEBRA42" || meta["id"] != "s1" {
		t.Fatalf("history %v meta %v", history, meta)
	}
}

func TestLoadResumeTargetEmptySessionKeepsResolvedId(t *testing.T) {
	db := oneshotDBWithSession(t, t.TempDir(), "s1")
	defer db.Close()
	sid, history, meta, err := loadOneshotResumeTarget(context.Background(), db, "s1")
	if err != nil || sid != "s1" || len(history) != 0 || meta["id"] != "s1" {
		t.Fatalf("got %q %v %v %v", sid, history, meta, err)
	}
}

func TestLoadResumeTargetCompressionChainRedirectsToChildWithMessages(t *testing.T) {
	tuiTestHome(t)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "parent", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "child", Source: "cli", ParentSessionID: "parent"}); err != nil {
		t.Fatal(err)
	}
	oneshotAppend(t, db, "child", "user", "hi")
	sid, history, _, err := loadOneshotResumeTarget(context.Background(), db, "parent")
	if err != nil || sid != "child" || len(history) != 1 || history[0]["role"] != "user" {
		t.Fatalf("got %q %v %v", sid, history, err)
	}
}

func TestLoadResumeTargetResumeReopensEndedSessionRow(t *testing.T) {
	db := oneshotDBWithSession(t, t.TempDir(), "s1", [2]string{"user", "hi"})
	defer db.Close()
	if err := db.EndSession("s1", "agent_close"); err != nil {
		t.Fatal(err)
	}
	row, _ := db.GetSession(context.Background(), "s1")
	if row.Vals["ended_at"] == nil || row.Vals["end_reason"] != "agent_close" {
		t.Fatalf("row %v", row.Vals)
	}
	sid, _, _, err := loadOneshotResumeTarget(context.Background(), db, "s1")
	if err != nil || sid != "s1" {
		t.Fatalf("got %q %v", sid, err)
	}
	reopened, _ := db.GetSession(context.Background(), "s1")
	if reopened.Vals["ended_at"] != nil || reopened.Vals["end_reason"] != nil {
		t.Fatalf("row %v", reopened.Vals)
	}
}

// Session row meta of a stored cli session with a model and a route written in both persisted shapes
func oneshotStoredMeta(t *testing.T, model string, route map[string]any) map[string]any {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if model != "" {
		if err := db.SetSessionModel(ctx, "s1", model, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if route != nil {
		patch := map[string]any{"gateway_runtime": route}
		for k, v := range route {
			patch[k] = v
		}
		if err := db.PatchSessionModelConfig(ctx, "s1", patch); err != nil {
			t.Fatal(err)
		}
	}
	row, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := pyjson.Plain(row).(map[string]any)
	return meta
}

const oneshotAmbientKey = "ambient-key"

func TestApplyStoredSessionRuntimeStoredRuntimeReplacesAmbientChoice(t *testing.T) {
	tuiTestHome(t)
	meta := oneshotStoredMeta(t, "stored-model", map[string]any{"provider": "custom:stored", "base_url": "http://stored:9", "api_mode": "responses"})
	choice := applyOneshotStoredSessionRuntime(context.Background(), &oneshotModelChoice{Model: "ambient-model", Provider: "openrouter", APIKey: oneshotAmbientKey}, meta, false)
	if choice.Model != "stored-model" || choice.Provider != "custom:stored" || choice.BaseURL != "http://stored:9" || choice.APIMode != "responses" {
		t.Fatalf("choice %+v", choice)
	}
	if choice.APIKey != "" {
		t.Fatalf("ambient key kept: %+v", choice)
	}
}

func TestApplyStoredSessionRuntimeExplicitModelFlagWins(t *testing.T) {
	tuiTestHome(t)
	meta := oneshotStoredMeta(t, "stored-model", map[string]any{"provider": "custom:stored"})
	choice := applyOneshotStoredSessionRuntime(context.Background(), &oneshotModelChoice{Model: "explicit-model", Provider: "openrouter", APIKey: oneshotAmbientKey}, meta, true)
	if choice.Model != "explicit-model" || choice.Provider != "openrouter" || choice.APIKey != oneshotAmbientKey {
		t.Fatalf("choice %+v", choice)
	}
}

func TestApplyStoredSessionRuntimeMatchingRouteIsNoopAndKeepsCredentials(t *testing.T) {
	tuiTestHome(t)
	meta := oneshotStoredMeta(t, "stored-model", map[string]any{"provider": "openrouter"})
	choice := applyOneshotStoredSessionRuntime(context.Background(), &oneshotModelChoice{Model: "stored-model", Provider: "openrouter", APIKey: oneshotAmbientKey}, meta, false)
	if choice.APIKey != oneshotAmbientKey || choice.APIMode != "" {
		t.Fatalf("choice %+v", choice)
	}
}

func TestApplyStoredSessionRuntimeOpencodeRowRederivesWireFromStoredModel(t *testing.T) {
	tuiTestHome(t)
	meta := oneshotStoredMeta(t, "deepseek-v4-flash-vision-exp", map[string]any{
		"provider": "opencode-go", "base_url": "https://opencode.ai/zen/go", "api_mode": "anthropic_messages"})
	choice := applyOneshotStoredSessionRuntime(context.Background(), &oneshotModelChoice{Model: "ambient-model", Provider: "openrouter", APIKey: oneshotAmbientKey}, meta, false)
	if choice.Model != "deepseek-v4-flash-vision-exp" || choice.Provider != "opencode-go" {
		t.Fatalf("choice %+v", choice)
	}
	if choice.APIMode != "chat_completions" || choice.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("choice %+v", choice)
	}
}

// Agent double capturing its construction keywords and the replayed history
type oneshotFakeAgent struct {
	captured map[string]any
	result   map[string]any
}

func (f *oneshotFakeAgent) SetSuppressStatusOutput(bool) {}
func (f *oneshotFakeAgent) SetStreamDeltaCallback(any)   {}
func (f *oneshotFakeAgent) SetToolGenCallback(any)       {}
func (f *oneshotFakeAgent) SessionID() string            { return "" }
func (f *oneshotFakeAgent) Platform() string             { return "" }
func (f *oneshotFakeAgent) RunConversation(_ context.Context, _ string, o agent.RunConversationOptions) (map[string]any, error) {
	f.captured["history"] = o.ConversationHistory
	return f.result, nil
}
func (f *oneshotFakeAgent) SessionMessages() []*state.Message                        { return nil }
func (f *oneshotFakeAgent) ShutdownMemoryProvider(context.Context, []map[string]any) {}
func (f *oneshotFakeAgent) Close() error                                             { return nil }

// Installs the run agent seams of the resume runtime cases and returns the captured keywords
func oneshotRunAgentSeams(t *testing.T, db *state.SessionDB, runtime *provider.Runtime, result map[string]any) map[string]any {
	t.Helper()
	captured := map[string]any{}
	swapSeam(t, &createOneshotSessionDB, func(context.Context) *state.SessionDB { return db })
	swapSeam(t, &oneshotLoadConfig, func(context.Context) (map[string]any, error) {
		return map[string]any{"model": map[string]any{"default": "ambient-model", "provider": "openrouter"}}, nil
	})
	swapSeam(t, &resolveRuntimeProviderFn, func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		copied := *runtime
		return &copied, nil
	})
	swapSeam(t, &oneshotPlatformTools, func(context.Context, map[string]any, string) []string { return []string{} })
	swapSeam(t, &oneshotEnsureMCPDiscovery, func(context.Context, *tools.Env) {})
	swapSeam(t, &newOneshotAgent, func(_ context.Context, o agent.AIAgentOptions) (oneshotAgent, error) {
		captured["model"], captured["provider"], captured["session_id"] = o.Model, o.Provider, o.SessionID
		return &oneshotFakeAgent{captured: captured, result: result}, nil
	})
	return captured
}

func TestRunAgentResumeRuntimeRunAgentUsesStoredRuntimeAndReopensRow(t *testing.T) {
	oneshotTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	db := oneshotDBWithSession(t, dir, "s1", [2]string{"user", "hi"})
	if err := db.SetSessionModel(ctx, "s1", "stored-model", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.PatchSessionModelConfig(ctx, "s1", map[string]any{
		"gateway_runtime": map[string]any{"provider": "custom:stored", "base_url": "http://stored:9"},
		"provider":        "custom:stored", "base_url": "http://stored:9"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("s1", "agent_close"); err != nil {
		t.Fatal(err)
	}
	captured := oneshotRunAgentSeams(t, db, &provider.Runtime{APIKey: "resolved", Provider: "custom:stored",
		RequestedProvider: "custom:stored", APIMode: "chat"}, map[string]any{"final_response": "ok", "session_id": "s1"})
	runCtx, env := oneshotToolEnv(ctx)
	text, result, err := runOneshotAgentTurn(runCtx, env, "hello", oneshotRunOptions{Resume: "s1", UseConfigToolsets: true})
	if err != nil || text != "ok" || result["final_response"] != "ok" {
		t.Fatalf("got %q %v %v", text, result, err)
	}
	if captured["model"] != "stored-model" || captured["provider"] != "custom:stored" || captured["session_id"] != "s1" {
		t.Fatalf("captured %v", captured)
	}
	history, _ := captured["history"].([]map[string]any)
	if len(history) == 0 || history[0]["role"] != "user" {
		t.Fatalf("history %v", captured["history"])
	}
	row := oneshotSessionRow(t, dir, "s1")
	if row["ended_at"] != nil || row["end_reason"] != nil {
		t.Fatalf("row %v", row)
	}
}

func TestRunAgentResumeRuntimeRunAgentExplicitModelBeatsStoredRuntime(t *testing.T) {
	oneshotTestEnv(t)
	ctx := context.Background()
	db := oneshotDBWithSession(t, t.TempDir(), "s1")
	if err := db.SetSessionModel(ctx, "s1", "stored-model", "", "", ""); err != nil {
		t.Fatal(err)
	}
	captured := oneshotRunAgentSeams(t, db, &provider.Runtime{APIKey: "resolved", Provider: "openrouter",
		RequestedProvider: "openrouter", APIMode: "chat"}, map[string]any{"final_response": "ok"})
	runCtx, env := oneshotToolEnv(ctx)
	if _, _, err := runOneshotAgentTurn(runCtx, env, "hello", oneshotRunOptions{Model: "explicit-model", Provider: "openrouter", Resume: "s1", UseConfigToolsets: true}); err != nil {
		t.Fatal(err)
	}
	if captured["model"] != "explicit-model" || captured["provider"] != "openrouter" {
		t.Fatalf("captured %v", captured)
	}
}

func TestRunOneshotForwardsResumeResumeKwargReachesRunAgent(t *testing.T) {
	oneshotTestEnv(t)
	captured := map[string]any{}
	swapSeam(t, &oneshotRunAgent, func(_ context.Context, _ *tools.Env, prompt string, o oneshotRunOptions) (string, map[string]any, error) {
		captured["prompt"], captured["resume"] = prompt, o.Resume
		return "ok", map[string]any{"final_response": "ok"}, nil
	})
	rc, err := RunOneshot(context.Background(), "hello", OneshotOptions{Model: "m", Provider: "custom", Resume: "sess-1"})
	if err != nil || rc != 0 {
		t.Fatalf("rc %d err %v", rc, err)
	}
	if captured["prompt"] != "hello" || captured["resume"] != "sess-1" {
		t.Fatalf("captured %v", captured)
	}
}

package provider

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Routes every default session client through f, counting the clients built
func withCodexClientFactory(t *testing.T, f *fakeCodexClient) {
	t.Helper()
	prev := codexAppServerClientFactory
	codexAppServerClientFactory = func(CodexAppServerOptions) (CodexRPCClient, error) { return f, nil }
	t.Cleanup(func() { codexAppServerClientFactory = prev })
}

func codexStartParams(f *fakeCodexClient, method, key string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []any
	for _, r := range f.requests {
		if r.method == method {
			v, _ := r.params.(map[string]any)[key]
			out = append(out, v)
		}
	}
	return out
}

func promptAgent(t *testing.T) *fakeCodexAgent {
	a := newFakeCodexAgent()
	a.cwd, a.cached, a.ephemeral = "/tmp", "SOUL: you are Icebath", "Always start with ZZZ"
	a.env = &tools.Env{Home: t.TempDir()}
	return a
}

func TestCodexRuntimeSendsComposedPromptOncePerThread(t *testing.T) {
	f := &fakeCodexClient{}
	withCodexClientFactory(t, f)
	a := promptAgent(t)
	for range 3 {
		if err := ensureCodexSession(a, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.session.EnsureStarted(); err != nil {
			t.Fatal(err)
		}
	}
	if got := codexStartParams(f, "thread/start", "developerInstructions"); len(got) != 1 || got[0] != "SOUL: you are Icebath\n\nAlways start with ZZZ" {
		t.Fatalf("thread starts %v", got)
	}
}

func TestCodexRuntimeOmitsPromptWhenAgentHasNone(t *testing.T) {
	f := &fakeCodexClient{}
	withCodexClientFactory(t, f)
	a := promptAgent(t)
	a.cached, a.ephemeral = "", ""
	if err := ensureCodexSession(a, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.session.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	if got := codexStartParams(f, "thread/start", "developerInstructions"); len(got) != 1 || got[0] != nil {
		t.Fatalf("developer instructions sent %v", got)
	}
}

func TestCodexRuntimeRetiresThreadWhenPromptCompositionChanges(t *testing.T) {
	f := &fakeCodexClient{}
	closes := 0
	prev := codexAppServerClientFactory
	codexAppServerClientFactory = func(CodexAppServerOptions) (CodexRPCClient, error) { return &countingCodexClient{f, &closes}, nil }
	t.Cleanup(func() { codexAppServerClientFactory = prev })
	a := promptAgent(t)
	for _, eph := range []string{"Always start with ZZZ", "Personality: pirate"} {
		a.ephemeral = eph
		if err := ensureCodexSession(a, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.session.EnsureStarted(); err != nil {
			t.Fatal(err)
		}
	}
	got := codexStartParams(f, "thread/start", "developerInstructions")
	if !slices.Equal(got, []any{"SOUL: you are Icebath\n\nAlways start with ZZZ", "SOUL: you are Icebath\n\nPersonality: pirate"}) || closes != 1 {
		t.Fatalf("starts %v closes %d", got, closes)
	}
}

type countingCodexClient struct {
	*fakeCodexClient
	closes *int
}

func (c *countingCodexClient) Close(timeout time.Duration) {
	*c.closes++
	c.fakeCodexClient.Close(timeout)
}

func TestCodexRuntimeRetiresThreadWhenAnInPlaceSwitchChangesTheCodexProvider(t *testing.T) {
	f := &fakeCodexClient{}
	closes := 0
	prev := codexAppServerClientFactory
	codexAppServerClientFactory = func(CodexAppServerOptions) (CodexRPCClient, error) { return &countingCodexClient{f, &closes}, nil }
	t.Cleanup(func() { codexAppServerClientFactory = prev })
	a := promptAgent(t)
	a.model = "gpt-5.5"
	if err := os.WriteFile(filepath.Join(a.env.Home, "config.yaml"), []byte("providers:\n  my-gateway:\n    api: https://gw.example/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pr := range [][2]string{{"openai-codex", "openai-codex"}, {"openai-codex", "openai-codex"}, {"custom", "custom:my-gateway"}} {
		a.provider, a.requestedProvider = pr[0], pr[1]
		if err := ensureCodexSession(a, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.session.EnsureStarted(); err != nil {
			t.Fatal(err)
		}
	}
	if got := codexStartParams(f, "thread/start", "modelProvider"); !slices.Equal(got, []any{nil, "my-gateway"}) || closes != 1 {
		t.Fatalf("model providers %v closes %d", got, closes)
	}
}

func TestCodexEachTurnSendsTheAgentsCurrentWireModel(t *testing.T) {
	f := &fakeCodexClient{}
	f.handler = func(method string, params any) (any, error) {
		switch method {
		case "thread/start":
			return map[string]any{"thread": map[string]any{"id": "t1"}}, nil
		case "turn/start":
			return nil, errors.New("stop after turn/start")
		}
		return map[string]any{}, nil
	}
	withCodexClientFactory(t, f)
	a := promptAgent(t)
	a.requestOverrides = map[string]any{}
	for _, model := range []string{"gpt-5.6-sol-900k", "gpt-5.5"} {
		a.model = model
		result, err := RunCodexAppServerTurn(a, "hi", "hi", nil, "t", false, nil)
		if err != nil || result["completed"] != false || !strings.HasPrefix(result["final_response"].(string), "Codex app-server turn failed: ") {
			t.Fatalf("result %v err %v", result, err)
		}
		if !strings.HasSuffix(result["final_response"].(string), ". Fall back to default runtime with `/codex-runtime auto`.") || result["api_calls"] != 0 {
			t.Fatalf("failure shape %v", result)
		}
	}
	if got := codexStartParams(f, "turn/start", "model"); !slices.Equal(got, []any{"gpt-5.6-sol", "gpt-5.5"}) {
		t.Fatalf("turn models %v", got)
	}
}

func TestCodexReplyIsRewrittenBeforeItIsReturnedOrStored(t *testing.T) {
	f := &fakeCodexClient{}
	agentDone(f, "thread-fake-001", "turn-fake-001", "RAW secret")
	a := newFakeCodexAgent("_sync_external_memory_for_turn")
	a.sessionID, a.platform = "sess-codex", "cli"
	a.session = newFakeSession(f, CodexAppServerSessionOptions{})
	a.requestOverrides = map[string]any{}
	a.transform = func(text string, turnID any) (string, bool, any) { return "REWRITE", true, text }
	result, err := RunCodexAppServerTurn(a, "config?", "config?", []map[string]any{{"role": "user", "content": "config?"}}, "task-1", false, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if result["final_response"] != "REWRITE" || result["response_transformed"] != true || result["pre_transform_response"] != "RAW secret" {
		t.Fatalf("result %v", result)
	}
	var assistant []any
	for _, m := range result["messages"].([]map[string]any) {
		if m["role"] == "assistant" {
			assistant = append(assistant, m["content"])
		}
	}
	if !slices.Equal(assistant, []any{"REWRITE"}) {
		t.Fatalf("assistant rows %v", assistant)
	}
	if calls := a.callsOf("_sync_external_memory_for_turn"); len(calls) != 1 || calls[0][1] != "REWRITE" {
		t.Fatalf("memory sync %v", calls)
	}
	if calls := a.callsOf("apply_llm_output_transform"); len(calls) != 1 || calls[0][1] != "t1" {
		t.Fatalf("transform calls %v", calls)
	}
}

func TestCodexHeldTurnHidesCommentaryAndKeepsThePreTransformInside(t *testing.T) {
	turn := &CodexTurnResult{FinalText: "final", Interrupted: true, ProjectedMessages: []map[string]any{
		{"role": "assistant", "content": "thinking aloud"}, {"role": "assistant", "content": "final"}}}
	a := newFakeCodexAgent()
	a.hold = true
	a.transform = func(text string, _ any) (string, bool, any) { return "FINAL", true, text }
	reply, err := transformCodexReply(a, turn, "t")
	if err != nil || reply["final_response"] != "FINAL" || reply["pre_transform_response"] != nil {
		t.Fatalf("reply %v %v", reply, err)
	}
	if turn.ProjectedMessages[0]["display_kind"] != "hidden" || turn.ProjectedMessages[1]["display_kind"] != nil || turn.ProjectedMessages[1]["content"] != "FINAL" {
		t.Fatalf("rows %v", turn.ProjectedMessages)
	}
	a.hold = false
	if reply, _ := transformCodexReply(a, &CodexTurnResult{FinalText: "x", Interrupted: true}, "t"); len(reply) != 1 || reply["final_response"] != "x" {
		t.Fatalf("interrupted unheld reply %v", reply)
	}
}

func TestCodexSuccessFlushesAndReportsPersisted(t *testing.T) {
	f := &fakeCodexClient{}
	agentDone(f, "thread-fake-001", "turn-fake-001", "done")
	db := &fakeCodexDB{values: map[string]any{}}
	a := newFakeCodexAgent()
	a.sessionID, a.db, a.dbCreated = "s1", db, true
	a.session = newFakeSession(f, CodexAppServerSessionOptions{})
	a.requestOverrides = map[string]any{}
	a.flush = func(m []map[string]any) (bool, error) { return true, nil }
	result, err := RunCodexAppServerTurn(a, "hi", "hi", []map[string]any{{"role": "user", "content": "hi"}}, "t", false, nil)
	if err != nil || result["agent_persisted"] != true || result["completed"] != true || result["codex_thread_id"] != "thread-fake-001" || result["codex_turn_id"] != "turn-fake-001" {
		t.Fatalf("result %v err %v", result, err)
	}
	if len(db.patches) != 1 || db.patches[0][codexThreadIDKey] != "thread-fake-001" {
		t.Fatalf("thread binding %v", db.patches)
	}
	f2 := &fakeCodexClient{}
	agentDone(f2, "thread-fake-001", "turn-fake-001", "done")
	db2 := &fakeCodexDB{values: map[string]any{}}
	b := newFakeCodexAgent()
	b.sessionID, b.db, b.dbCreated = "s2", db2, true
	b.session = newFakeSession(f2, CodexAppServerSessionOptions{})
	b.requestOverrides = map[string]any{}
	b.flush = func(m []map[string]any) (bool, error) { return false, nil }
	if _, err := RunCodexAppServerTurn(b, "hi", "hi", nil, "t", false, nil); err != nil || len(db2.patches) != 0 {
		t.Fatalf("an undurable transcript published its binding %v %v", db2.patches, err)
	}
}

func TestCodexCheckpointRequiredBlocksTheTurn(t *testing.T) {
	a := newFakeCodexAgent()
	a.checkpointRequired = true
	_, err := RunCodexAppServerTurn(a, "hi", "hi", nil, "t", false, nil)
	var blocked *CompressionCheckpointUnavailable
	if !errors.As(err, &blocked) || a.session != nil {
		t.Fatalf("err %v", err)
	}
}

func TestCodexTurnEffortAndServiceTier(t *testing.T) {
	a := newFakeCodexAgent()
	a.provider = "openai-codex"
	if codexTurnEffort(a, "gpt-5.5") != nil {
		t.Fatal("unset reasoning overrode codex's default")
	}
	a.reasoning = map[string]any{"effort": "high"}
	if got := codexTurnEffort(a, "gpt-5.5"); got == nil || *got != "high" {
		t.Fatalf("effort %v", got)
	}
	a.reasoning = map[string]any{"effort": "ultra"}
	if got := codexTurnEffort(a, "gpt-5.6-sol"); got == nil || *got != "ultra" {
		t.Fatalf("ultra on a max model %v", got)
	}
	if !slices.Contains(routeSupportedEfforts("openai-codex", "gpt-5.6-sol", "codex_app_server"), "ultra") || slices.Contains(routeSupportedEfforts("openai-codex", "gpt-5.5", "codex_app_server"), "ultra") {
		t.Fatal("ultra offered off the max ladder")
	}
	a.reasoning = map[string]any{"enabled": false}
	if got := codexTurnEffort(a, "gpt-5.5"); got == nil || *got != "none" {
		t.Fatalf("disabled effort %v", got)
	}
	a.requestOverrides = map[string]any{"service_tier": "priority"}
	if tier, err := codexTurnServiceTier(a); err != nil || tier == nil || *tier != "fast" {
		t.Fatalf("tier %v %v", tier, err)
	}
	a.requestOverrides = map[string]any{"service_tier": "ultrafast"}
	if tier, err := codexTurnServiceTier(a); err != nil || tier != nil {
		t.Fatalf("untranslatable tier sent %v %v", tier, err)
	}
	if got := codexWireModel(a, nil); got != "" {
		t.Fatalf("empty model %q", got)
	}
	a.model = "openai/gpt-5.5"
	if got := codexWireModel(a, nil); got != "gpt-5.5" {
		t.Fatalf("bare slug %q", got)
	}
	gw := "gw"
	if got := codexWireModel(a, &gw); got != "openai/gpt-5.5" {
		t.Fatalf("custom provider slug %q", got)
	}
}

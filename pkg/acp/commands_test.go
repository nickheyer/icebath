package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func noStore(t *testing.T) {
	t.Helper()
	swap(t, &acquireSessionDB, func(context.Context, string) (*state.DB, error) { return nil, errNoStore })
}

func runsOf(fake *mockAgent) []any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []any
	for _, call := range fake.runCalls {
		out = append(out, call.userMessage)
	}
	return out
}

func makeAgentAndState(t *testing.T) (*HermesACPAgent, *SessionState, *mockAgent, *captureConn) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	noStore(t)
	fake := &mockAgent{model: "fake-model", provider: "fake-provider", enabledToolsets: []string{"icebath-acp"}, disabledToolsets: []string{},
		toolDefs: []map[string]any{}, validToolNames: map[string]bool{}, supportsRedirect: true, steerResult: true, redirectResult: true,
		env: &tools.Env{Home: os.Getenv("ICEBATH_HOME"), Values: &sync.Map{}}}
	fake.run = func(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error) {
		messages := append([]map[string]any{}, o.ConversationHistory...)
		final := "ran: " + pyStr(userMessage)
		messages = append(messages, map[string]any{"role": "user", "content": userMessage}, map[string]any{"role": "assistant", "content": final})
		return map[string]any{"final_response": final, "messages": messages}, nil
	}
	m := NewSessionManager(func() any { return fake }, nil)
	a := NewHermesACPAgent(m)
	st := mustCreate(t, m, ".")
	conn := &captureConn{}
	a.conn = conn
	return a, st, fake, conn
}

func TestAcpRealAgentGetsSessionDbForRecall(t *testing.T) {
	db := provenanceDB(t)
	var captured []agent.AIAgentOptions
	built := &mockAgent{}
	fakeAgentBuild(t, config.Tree{"model": map[string]any{"default": "m", "provider": "p"}},
		func(requested, target string) (*provider.Runtime, error) {
			return &provider.Runtime{Provider: "p", APIMode: "chat_completions", BaseURL: "u", APIKey: "k", Args: []string{}}, nil
		},
		func(o agent.AIAgentOptions) (any, error) {
			captured = append(captured, o)
			return built, nil
		})
	got, err := NewSessionManager(nil, db).MakeAgent(context.Background(), MakeAgentOptions{SessionID: "acp-session", Cwd: "."})
	if err != nil || got != any(built) {
		t.Fatalf("agent = %v, %v", got, err)
	}
	if store, ok := captured[0].SessionDB.(*state.DB); !ok || store != db {
		t.Fatal("session store not forwarded by identity")
	}
	if captured[0].Platform != "acp" || captured[0].SessionID != "acp-session" {
		t.Fatalf("options = %+v", captured[0])
	}
}

func TestAcpSteerSlashCommandInjectsIntoRunningAgent(t *testing.T) {
	a, st, fake, _ := makeAgentAndState(t)
	st.IsRunning = true
	resp, err := a.Prompt(context.Background(), textPrompt(st.SessionID, "/steer prefer the simpler fix"))
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if !slices.Equal(fake.steerCalls, []string{"prefer the simpler fix"}) || len(runsOf(fake)) != 0 {
		t.Fatalf("steers %v runs %v", fake.steerCalls, runsOf(fake))
	}
}

func TestAcpResetRejectedWhileTurnRunning(t *testing.T) {
	a, st, fake, _ := makeAgentAndState(t)
	st.IsRunning = true
	st.History = []map[string]any{{"role": "user", "content": "earlier"}}
	resp, err := a.Prompt(context.Background(), textPrompt(st.SessionID, "/reset"))
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if !reflect.DeepEqual(st.History, []map[string]any{{"role": "user", "content": "earlier"}}) || len(runsOf(fake)) != 0 || len(st.QueuedPrompts) != 0 {
		t.Fatalf("history %v runs %v queued %v", st.History, runsOf(fake), st.QueuedPrompts)
	}
}

func TestAcpCompressRejectedWhileTurnRunning(t *testing.T) {
	a, st, fake, _ := makeAgentAndState(t)
	st.IsRunning = true
	st.History = []map[string]any{{"role": "user", "content": "earlier"}}
	sentinel := provenanceDB(t)
	fake.db, fake.cachedSystemPrompt = sentinel, "sys"
	compressed := false
	swap(t, &compressNow, func(context.Context, any, []map[string]any, agent.CompressRequest, agent.CompressNowOptions) (agent.CompressResult, error) {
		compressed = true
		return agent.CompressResult{Status: "compressed", AfterMessages: []map[string]any{{"role": "user", "content": "summary"}}}, nil
	})
	resp, err := a.Prompt(context.Background(), textPrompt(st.SessionID, "/compress"))
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if !reflect.DeepEqual(st.History, []map[string]any{{"role": "user", "content": "earlier"}}) || fake.db != sentinel || len(runsOf(fake)) != 0 || compressed {
		t.Fatalf("history %v compressed %v", st.History, compressed)
	}
}

func TestAcpPromptDuringMutatingCommandQueuesThenRuns(t *testing.T) {
	a, st, fake, _ := makeAgentAndState(t)
	ctx := context.Background()
	st.History = []map[string]any{{"role": "user", "content": "earlier"}}
	originalReset := a.commandHandlers["reset"]
	a.commandHandlers["reset"] = func(ctx context.Context, args string, s *SessionState) (string, error) {
		done := make(chan error, 1)
		go func() {
			_, err := a.Prompt(ctx, textPrompt(s.SessionID, "follow-up"))
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				return "", err
			}
		case <-time.After(10 * time.Second):
			return "", errors.New("follow-up prompt did not return")
		}
		return originalReset(ctx, args, s)
	}
	resp, err := a.Prompt(ctx, textPrompt(st.SessionID, "/reset"))
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if !reflect.DeepEqual(runsOf(fake), []any{"follow-up"}) {
		t.Fatalf("runs = %v", runsOf(fake))
	}
	want := []map[string]any{{"role": "user", "content": "follow-up"}, {"role": "assistant", "content": "ran: follow-up"}}
	if !reflect.DeepEqual(st.History, want) || st.CommandOp {
		t.Fatalf("history %v command_op %v", st.History, st.CommandOp)
	}
}

func TestAcpCancelPublishesHardStopWhileHoldingRuntimeLock(t *testing.T) {
	a, st, fake, _ := makeAgentAndState(t)
	st.IsRunning = true
	st.CurrentPromptText = "original request"
	lockHeld, called := false, false
	fake.interrupt = func() {
		called = true
		acquired := st.RuntimeLock.TryLock()
		lockHeld = !acquired
		if acquired {
			st.RuntimeLock.Unlock()
		}
	}
	if err := a.Cancel(context.Background(), &pyacp.CancelNotification{SessionID: st.SessionID}); err != nil {
		t.Fatal(err)
	}
	if !called || !lockHeld || !st.CancelEvent.IsSet() || st.InterruptedPromptText != "original request" {
		t.Fatalf("called %v lock held %v cancel %v interrupted %q", called, lockHeld, st.CancelEvent.IsSet(), st.InterruptedPromptText)
	}
}

type boomConn struct {
	captureConn
	calls int
}

func (c *boomConn) SessionUpdate(ctx context.Context, sessionID string, update pyacp.SessionUpdate, meta *pyjson.Object) error {
	c.calls++
	if c.calls == 1 {
		return errors.New("mid-tail boom")
	}
	return nil
}

type saveOnly struct{ *SessionManager }

func (saveOnly) SaveSession(context.Context, string) {}

func TestFinishTurnMidTailFailureReleasesAndDrains(t *testing.T) {
	a := NewHermesACPAgent(saveOnly{})
	type drainedPrompt struct {
		texts     []string
		sessionID string
	}
	var drained []drainedPrompt
	a.prompter = func(ctx context.Context, req *pyacp.PromptRequest) (*pyacp.PromptResponse, error) {
		var texts []string
		for _, block := range req.Prompt {
			texts = append(texts, block.(*pyacp.TextContentBlock).Text)
		}
		drained = append(drained, drainedPrompt{texts, req.SessionID})
		return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
	}
	st := &SessionState{SessionID: "s1", Agent: &mockAgent{sessionID: "h1"}, History: []map[string]any{}, IsRunning: true,
		QueuedPrompts: []string{"queued-one"}, CurrentPromptText: "hello"}
	conn := &boomConn{}
	result := map[string]any{"final_response": "done", "messages": []map[string]any{}}
	_, err := a.finishTurn(context.Background(), st, "s1", conn, result, "h1", false)
	if err == nil || err.Error() != "mid-tail boom" {
		t.Fatalf("err = %v", err)
	}
	if st.IsRunning || st.CurrentPromptText != "" || len(st.QueuedPrompts) != 0 {
		t.Fatalf("state = %+v", st)
	}
	if !reflect.DeepEqual(drained, []drainedPrompt{{[]string{"queued-one"}, "s1"}}) {
		t.Fatalf("drained = %v", drained)
	}
}

func capturingBuild(t *testing.T, runtime func(requested, target string) (*provider.Runtime, error)) *[]agent.AIAgentOptions {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	noStore(t)
	var seen []agent.AIAgentOptions
	swap(t, &resolveSessionRuntime, func(ctx context.Context, requested, target string) (*provider.Runtime, error) {
		return runtime(requested, target)
	})
	swap(t, &ensureMCPDiscoveryBeforeAgentBuild, func(context.Context, config.Tree) {})
	swap(t, &registerTaskCwdHook, func(context.Context, string, string) {})
	swap(t, &newAIAgent, func(ctx context.Context, o agent.AIAgentOptions) (any, error) {
		seen = append(seen, o)
		model := o.Model
		if model == "" {
			model = "fixture-model"
		}
		return &nsAgent{model: model}, nil
	})
	return &seen
}

func writeHomeConfig(t *testing.T, yaml string) {
	t.Helper()
	config.ResetCache()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcpAgentReceivesConfiguredReasoning(t *testing.T) {
	seen := capturingBuild(t, func(requested, target string) (*provider.Runtime, error) {
		name := requested
		if name == "" {
			name = "openai-api"
		}
		return &provider.Runtime{Provider: name, APIMode: "codex_responses", BaseURL: "https://example.invalid/v1", APIKey: "test-key"}, nil
	})
	ctx := context.Background()
	writeHomeConfig(t, "model:\n  default: gpt-4o-mini\n  provider: openai-api\nagent:\n  reasoning_effort: none\n")
	sm := NewSessionManager(nil, nil)
	if _, err := sm.MakeAgent(ctx, MakeAgentOptions{SessionID: "s1", Cwd: "."}); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[0].ReasoningConfig; !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("reasoning config = %v", got)
	}
	writeHomeConfig(t, "model:\n  default: gpt-4o-mini\n  provider: openai-api\nagent:\n  reasoning_effort: none\n  reasoning_overrides:\n    gpt-5.6: high\n")
	if _, err := sm.MakeAgent(ctx, MakeAgentOptions{SessionID: "s2", Cwd: ".", Model: "gpt-5.6"}); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[1].ReasoningConfig; !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "high"}) {
		t.Fatalf("reasoning config = %v", got)
	}
}

func TestAcpAgentReceivesCustomProviderRequestBody(t *testing.T) {
	overrides := map[string]any{"extra_body": map[string]any{"user": "proxy-user"}}
	seen := capturingBuild(t, func(requested, target string) (*provider.Runtime, error) {
		return &provider.Runtime{Provider: "custom", APIMode: "chat_completions", BaseURL: "https://proxy.example/v1", APIKey: "k",
			RequestOverrides: overrides}, nil
	})
	ctx := context.Background()
	writeHomeConfig(t, "model:\n  default: some-model\n  provider: custom:my-proxy\n")
	sm := NewSessionManager(nil, nil)
	for _, o := range []MakeAgentOptions{{SessionID: "s1", Cwd: "."}, {SessionID: "s2", Cwd: ".", BaseURL: "https://proxy.example/v1"},
		{SessionID: "s3", Cwd: ".", BaseURL: "https://elsewhere.example/v1"}} {
		if _, err := sm.MakeAgent(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual((*seen)[0].RequestOverrides, overrides) || !reflect.DeepEqual((*seen)[1].RequestOverrides, overrides) {
		t.Fatalf("request overrides = %v %v", (*seen)[0].RequestOverrides, (*seen)[1].RequestOverrides)
	}
	if (*seen)[2].RequestOverrides != nil {
		t.Fatalf("another endpoint inherited the request overrides: %v", (*seen)[2].RequestOverrides)
	}
}

const buildSeconds = 500 * time.Millisecond

func slowFactory() any {
	time.Sleep(buildSeconds)
	return &mockAgent{model: "m", sessionID: "s", enabledToolsets: []string{}, disabledToolsets: []string{}}
}

func ticksDuring(t *testing.T, call func()) int {
	t.Helper()
	ticks := 0
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				ticks++
			}
		}
	}()
	call()
	close(stop)
	<-stopped
	return ticks
}

func TestNewSessionKeepsTheEventLoopFree(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := NewHermesACPAgent(NewSessionManager(slowFactory, nil))
	var resp *pyacp.NewSessionResponse
	ticks := ticksDuring(t, func() { resp = newSession(t, a, "/tmp") })
	if resp.SessionID == "" || ticks < 5 {
		t.Fatalf("session %q ticks %d", resp.SessionID, ticks)
	}
}

func TestHandlersRestoreUnknownSessionsOffTheLoop(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(a *HermesACPAgent){
		"prompt": func(a *HermesACPAgent) { a.Prompt(ctx, textPrompt("gone", "hi")) },
		"cancel": func(a *HermesACPAgent) { a.Cancel(ctx, &pyacp.CancelNotification{SessionID: "gone"}) },
		"set_session_model": func(a *HermesACPAgent) {
			a.SetSessionModel(ctx, &pyacp.SetSessionModelRequest{ModelID: "m", SessionID: "gone"})
		},
		"set_session_mode": func(a *HermesACPAgent) {
			a.SetSessionMode(ctx, &pyacp.SetSessionModeRequest{ModeID: "ask", SessionID: "gone"})
		},
		"set_config_option": func(a *HermesACPAgent) {
			a.SetConfigOption(ctx, &pyacp.SetConfigOptionParams{ConfigID: "edit_approval_policy", SessionID: "gone", Value: "ask"})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			m := NewSessionManager(slowFactory, nil)
			m.restoreFn = func(context.Context, string) *SessionState {
				time.Sleep(buildSeconds)
				return nil
			}
			a := NewHermesACPAgent(m)
			if ticks := ticksDuring(t, func() { call(a) }); ticks < 5 {
				t.Fatalf("ticks = %d", ticks)
			}
		})
	}
}

func TestConcurrentRestoresOfOneSessionBuildASingleAgent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	m := NewSessionManager(func() any { return &nsAgent{model: "m"} }, nil)
	var mu sync.Mutex
	var restores []string
	m.restoreFn = func(ctx context.Context, sessionID string) *SessionState {
		mu.Lock()
		restores = append(restores, sessionID)
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		return m.installState(ctx, sessionID, m.agentFactory(), "/tmp", "m", []map[string]any{}, false)
	}
	results := make([]*SessionState, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = m.GetSession(ctx, "sid-1")
		}()
	}
	wg.Wait()
	if results[0] == nil || results[0] != results[1] {
		t.Fatalf("results = %v", results)
	}
	if len(restores) != 1 {
		t.Fatalf("restore ran %d agent builds for one session id", len(restores))
	}
}

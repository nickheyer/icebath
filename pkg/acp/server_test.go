package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func detectedProvider(t *testing.T, name string) {
	t.Helper()
	swap(t, &resolveRuntimeProvider, func(context.Context) (*provider.Runtime, error) {
		if name == "" {
			return nil, errors.New("no provider configured")
		}
		return &provider.Runtime{Provider: name, APIKey: "key"}, nil
	})
}

func newSession(t *testing.T, a *HermesACPAgent, cwd string) *pyacp.NewSessionResponse {
	t.Helper()
	resp, err := a.NewSession(context.Background(), &pyacp.NewSessionRequest{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mockOf(t *testing.T, st *SessionState) *mockAgent {
	t.Helper()
	m, ok := st.Agent.(*mockAgent)
	if !ok {
		t.Fatalf("agent is %T", st.Agent)
	}
	return m
}

func TestNewSessionExposesEditApprovalsAsModesNotConfigOptions(t *testing.T) {
	a, _ := newTestAgent(t)
	resp := newSession(t, a, "/tmp")
	if resp.ConfigOptions != nil {
		t.Fatalf("config options = %v", resp.ConfigOptions)
	}
	if resp.Modes == nil || resp.Modes.CurrentModeID != "default" {
		t.Fatalf("modes = %+v", resp.Modes)
	}
	var ids []string
	for _, mode := range resp.Modes.AvailableModes {
		ids = append(ids, mode.ID)
	}
	if !slices.Equal(ids, []string{"default", "accept_edits", "dont_ask"}) {
		t.Fatalf("mode ids = %v", ids)
	}
}

func TestSetConfigOptionPersistsEditApprovalPolicyWithoutAdvertisingConfig(t *testing.T) {
	a, _ := newTestAgent(t)
	ctx := context.Background()
	resp := newSession(t, a, "/tmp")
	update, err := a.SetConfigOption(ctx, &pyacp.SetConfigOptionParams{ConfigID: "edit_approval_policy", SessionID: resp.SessionID, Value: "workspace_session"})
	if err != nil {
		t.Fatal(err)
	}
	st := a.SessionManager.GetSession(ctx, resp.SessionID)
	if update == nil || update.ConfigOptions == nil || len(update.ConfigOptions) != 0 {
		t.Fatalf("update = %+v", update)
	}
	if st.Mode != "accept_edits" {
		t.Fatalf("mode = %q", st.Mode)
	}
}

func TestInitializeReturnsCorrectProtocolVersion(t *testing.T) {
	a, _ := newTestAgent(t)
	resp, err := a.Initialize(context.Background(), &pyacp.InitializeRequest{ProtocolVersion: 1})
	if err != nil || resp == nil || resp.ProtocolVersion != pyacp.ProtocolVersion {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

func TestInitializeAdvertisesProviderAndTerminalAuthMethods(t *testing.T) {
	a, _ := newTestAgent(t)
	detectedProvider(t, "openrouter")
	resp, err := a.Initialize(context.Background(), &pyacp.InitializeRequest{ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	var payloads []*pyjson.Object
	for _, method := range resp.AuthMethods {
		payloads = append(payloads, pyacp.Dump(method, pyacp.DumpOptions{ByAlias: true, ExcludeNone: true}).(*pyjson.Object))
	}
	if payloads[0].GetOr("id", nil) != "openrouter" {
		t.Fatalf("first method = %v", payloads[0].Vals)
	}
	var terminal *pyjson.Object
	for _, payload := range payloads {
		if payload.GetOr("id", nil) == TerminalSetupAuthMethodID {
			terminal = payload
		}
	}
	if terminal == nil || terminal.GetOr("type", nil) != "terminal" || !reflect.DeepEqual(terminal.GetOr("args", nil), []any{"--setup"}) {
		t.Fatalf("terminal = %v", terminal)
	}
}

func authenticateWith(t *testing.T, detected, methodID string) *pyacp.AuthenticateResponse {
	t.Helper()
	a, _ := newTestAgent(t)
	detectedProvider(t, detected)
	resp, err := a.Authenticate(context.Background(), &pyacp.AuthenticateRequest{MethodID: methodID})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthenticateWithMatchingMethodId(t *testing.T) {
	if authenticateWith(t, "openrouter", "openrouter") == nil {
		t.Fatal("matching method refused")
	}
}

func TestAuthenticateIsCaseInsensitive(t *testing.T) {
	if authenticateWith(t, "openrouter", "OpenRouter") == nil {
		t.Fatal("case variant refused")
	}
}

func TestAuthenticateRejectsMismatchedMethodId(t *testing.T) {
	if authenticateWith(t, "openrouter", "totally-invalid-method") != nil {
		t.Fatal("mismatched method accepted")
	}
}

func TestAuthenticateWithoutProvider(t *testing.T) {
	if authenticateWith(t, "", "openrouter") != nil {
		t.Fatal("accepted without a provider")
	}
}

func TestAuthenticateAcceptsTerminalSetupAfterProviderConfigured(t *testing.T) {
	if authenticateWith(t, "openrouter", TerminalSetupAuthMethodID) == nil {
		t.Fatal("terminal setup refused")
	}
}

func TestNewSessionReturnsAuthenticatedCrossProviderModelState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	m := NewSessionManager(func() any {
		return &nsAgent{model: "gpt-5.4", provider: "openai-codex", baseURL: "https://api.openai.com/v1"}
	}, nil)
	a := NewHermesACPAgent(m)
	payload := map[string]any{"providers": []any{
		map[string]any{"slug": "anthropic", "name": "Anthropic", "models": []any{"claude-sonnet-4-6", "claude-sonnet-4-6"}},
		map[string]any{"slug": "openai-codex", "name": "OpenAI Codex", "models": []any{map[string]any{"id": "gpt-5.4"}, "gpt-5.4-mini"}},
	}}
	var overrides []provider.PickerConfigContext
	swap(t, &buildModelsPayload, func(ctx context.Context, pc provider.PickerConfigContext, o provider.ModelsPayloadOptions) (map[string]any, error) {
		overrides = append(overrides, pc)
		return payload, nil
	})
	resp := newSession(t, a, "/tmp")
	if resp.Models == nil || resp.Models.CurrentModelID != "openai-codex:gpt-5.4" {
		t.Fatalf("models = %+v", resp.Models)
	}
	var ids []string
	for _, model := range resp.Models.AvailableModels {
		ids = append(ids, model.ModelID)
	}
	if !slices.Equal(ids, []string{"anthropic:claude-sonnet-4-6", "openai-codex:gpt-5.4", "openai-codex:gpt-5.4-mini"}) {
		t.Fatalf("model ids = %v", ids)
	}
	if len(overrides) != 1 || overrides[0].CurrentProvider != "openai-codex" || overrides[0].CurrentModel != "gpt-5.4" ||
		overrides[0].CurrentBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("picker overrides = %+v", overrides)
	}
}

func TestBuildUsageUpdateForZedContextIndicator(t *testing.T) {
	a, m := newTestAgent(t)
	st := mustCreate(t, m, "/tmp")
	st.History = []map[string]any{{"role": "user", "content": "hello"}}
	mock := mockOf(t, st)
	mock.contextLength, mock.cachedSystemPrompt = 100_000, "system"
	mock.toolDefs = []map[string]any{{"type": "function", "function": map[string]any{"name": "demo"}}}
	swap(t, &estimateRequestTokensRough, func(context.Context, []map[string]any, string, []map[string]any) int { return 25_000 })
	update := a.buildUsageUpdate(context.Background(), st)
	if update == nil || update.Size != 100_000 || update.Used != 25_000 {
		t.Fatalf("update = %+v", update)
	}
	if dumped := pyacp.Dump(update, pyacp.SerializeParams).(*pyjson.Object); dumped.GetOr("sessionUpdate", nil) != "usage_update" {
		t.Fatalf("dumped = %v", dumped.Vals)
	}
}

func TestLoadSessionNotFoundReturnsNone(t *testing.T) {
	a, _ := newTestAgent(t)
	resp, err := a.LoadSession(context.Background(), &pyacp.LoadSessionRequest{Cwd: "/tmp", SessionID: "bogus"})
	if resp != nil || err != nil {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

func TestResumeSessionReplaysPersistedHistoryToClient(t *testing.T) {
	a, _ := newTestAgent(t)
	conn := &captureConn{}
	a.conn = conn
	ctx := context.Background()
	created := newSession(t, a, "/tmp")
	st := a.SessionManager.GetSession(ctx, created.SessionID)
	st.History = []map[string]any{{"role": "user", "content": "So tell me the current state"}}
	a.background.Wait()
	conn.reset()
	resp, err := a.ResumeSession(ctx, &pyacp.ResumeSessionRequest{Cwd: "/tmp", SessionID: created.SessionID})
	if err != nil || resp == nil {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	a.background.Wait()
	found := false
	for _, u := range conn.seen() {
		if chunk, ok := u.update.(*pyacp.UserMessageChunk); ok {
			if text, ok := chunk.Content.(*pyacp.TextContentBlock); ok && text.Text == "So tell me the current state" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("history not replayed: %v", conn.seen())
	}
}

func TestForkSession(t *testing.T) {
	a, _ := newTestAgent(t)
	created := newSession(t, a, "/original")
	fork, err := a.ForkSession(context.Background(), &pyacp.ForkSessionRequest{Cwd: "/forked", SessionID: created.SessionID})
	if err != nil || fork.SessionID == "" || fork.SessionID == created.SessionID {
		t.Fatalf("fork = %+v, %v", fork, err)
	}
}

type listingSessions struct {
	*SessionManager
	rows []*pyjson.Object
}

func (l *listingSessions) ListSessions(ctx context.Context, cwd string) ([]*pyjson.Object, error) {
	return l.rows, nil
}

func TestListSessionsIncludesTitleAndUpdatedAt(t *testing.T) {
	_, m := newTestAgent(t)
	row := pyjson.NewObject()
	row.Set("session_id", "session-1")
	row.Set("cwd", "/tmp/project")
	row.Set("title", "Fix Zed session history")
	row.Set("updated_at", 123.0)
	a := NewHermesACPAgent(&listingSessions{SessionManager: m, rows: []*pyjson.Object{row}})
	cwd := "/tmp/project"
	resp, err := a.ListSessions(context.Background(), &pyacp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatal(err)
	}
	first := resp.Sessions[0]
	if first.Title == nil || *first.Title != "Fix Zed session history" || first.UpdatedAt == nil || *first.UpdatedAt != "123.0" {
		t.Fatalf("session = %+v", first)
	}
}

func jsonParams(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

func TestRouterAcceptsStableSessionConfigMethods(t *testing.T) {
	a, _ := newTestAgent(t)
	ctx := context.Background()
	created := newSession(t, a, "/tmp")
	router := pyacp.BuildAgentRouter(a, false)
	mode, err := router.Call(ctx, "session/set_mode", jsonParams(t, fmt.Sprintf(`{"modeId": "accept_edits", "sessionId": %q}`, created.SessionID)), false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := router.Call(ctx, "session/set_config_option",
		jsonParams(t, fmt.Sprintf(`{"configId": "approval_mode", "sessionId": %q, "value": "auto"}`, created.SessionID)), false)
	if err != nil {
		t.Fatal(err)
	}
	if obj, ok := mode.(*pyjson.Object); !ok || obj.Len() != 0 {
		t.Fatalf("mode result = %#v", mode)
	}
	options, ok := cfg.(*pyjson.Object).GetOr("configOptions", nil).([]any)
	if !ok || len(options) != 0 {
		t.Fatalf("config result = %#v", cfg)
	}
}

func TestPromptReturnsRefusalForUnknownSession(t *testing.T) {
	a, _ := newTestAgent(t)
	resp, err := a.Prompt(context.Background(), textPrompt("nonexistent", "hello"))
	if err != nil || resp == nil || resp.StopReason != "refusal" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

func TestPromptBindsSessionIdIntoSubprocessEnv(t *testing.T) {
	a, m := newTestAgent(t)
	ctx := context.Background()
	resp := newSession(t, a, ".")
	st := m.GetSession(ctx, resp.SessionID)
	mock := mockOf(t, st)
	child := ""
	mock.run = func(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error) {
		runEnv, err := tools.MakeRunEnv(mock.env, map[string]string{})
		if err != nil {
			return nil, err
		}
		child = runEnv["ICEBATH_SESSION_ID"]
		return map[string]any{"final_response": "ok", "messages": []map[string]any{}}, nil
	}
	mock.model, mock.provider = "test-model", "openrouter"
	a.conn = &captureConn{}
	if _, err := a.Prompt(ctx, textPrompt(resp.SessionID, "hi")); err != nil {
		t.Fatal(err)
	}
	if child != resp.SessionID {
		t.Fatalf("child session id = %q, want %q", child, resp.SessionID)
	}
}

func TestEmptyMessagesListReplacesStaleHistory(t *testing.T) {
	a, m := newTestAgent(t)
	ctx := context.Background()
	resp := newSession(t, a, ".")
	st := m.GetSession(ctx, resp.SessionID)
	st.History = []map[string]any{{"role": "user", "content": "old"}}
	mock := mockOf(t, st)
	mock.runResult = map[string]any{"final_response": "done", "messages": []map[string]any{}}
	mock.model, mock.provider = "test-model", "openrouter"
	a.conn = &captureConn{}
	if _, err := a.Prompt(ctx, textPrompt(resp.SessionID, "hi")); err != nil {
		t.Fatal(err)
	}
	if st.History == nil || len(st.History) != 0 {
		t.Fatalf("history = %v", st.History)
	}
}

type failingSave struct {
	*SessionManager
	failure error
}

func (f *failingSave) SaveSession(ctx context.Context, sessionID string) {
	if f.failure != nil {
		panic(f.failure)
	}
	f.SessionManager.SaveSession(ctx, sessionID)
}

func TestPromptAfterTailExceptionRunsInsteadOfQueueing(t *testing.T) {
	m := newMockManager(t)
	sessions := &failingSave{SessionManager: m}
	a := NewHermesACPAgent(sessions)
	ctx := context.Background()
	resp := newSession(t, a, ".")
	st := m.GetSession(ctx, resp.SessionID)
	mock := mockOf(t, st)
	mock.runResult = map[string]any{"final_response": "done", "messages": []map[string]any{}}
	mock.model, mock.provider = "test-model", "openrouter"
	a.conn = &captureConn{}
	sessions.failure = errors.New("disk full")
	raised := func() (raised any) {
		defer func() { raised = recover() }()
		a.Prompt(ctx, textPrompt(resp.SessionID, "first"))
		return nil
	}()
	if err, ok := raised.(error); !ok || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("raised = %v", raised)
	}
	sessions.failure = nil
	second, err := a.Prompt(ctx, textPrompt(resp.SessionID, "second"))
	if err != nil || second.StopReason != "end_turn" {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if len(st.QueuedPrompts) != 0 || len(mock.runCalls) != 2 {
		t.Fatalf("queued %v, runs %d", st.QueuedPrompts, len(mock.runCalls))
	}
}

func TestPromptFailsToolCallsLeftOpenBeforeResponding(t *testing.T) {
	for _, executorRaises := range []bool{false, true} {
		t.Run(fmt.Sprintf("executor_raises_%v", executorRaises), func(t *testing.T) {
			a, m := newTestAgent(t)
			ctx := context.Background()
			resp := newSession(t, a, ".")
			st := m.GetSession(ctx, resp.SessionID)
			mock := mockOf(t, st)
			mock.model, mock.provider = "test-model", "openrouter"
			conn := &captureConn{}
			a.conn = conn
			a.turnRunner = func(ctx context.Context, turn turnArgs) (map[string]any, error) {
				progress := mock.toolProgressCallback.(func(event, name, preview string, args map[string]any, extra map[string]any))
				progress("tool.started", "terminal", "ls", map[string]any{"command": "ls"}, nil)
				if executorRaises {
					return nil, errors.New("executor blew up")
				}
				return map[string]any{"final_response": "ok", "messages": []map[string]any{}}, nil
			}
			started := time.Now()
			type answer struct {
				resp *pyacp.PromptResponse
				seen []connUpdate
			}
			done := make(chan answer, 1)
			go func() {
				response, _ := a.Prompt(ctx, textPrompt(resp.SessionID, "hi"))
				done <- answer{response, conn.seen()}
			}()
			var got answer
			select {
			case got = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("prompt did not answer in 3s")
			}
			if time.Since(started) >= 4*time.Second {
				t.Fatal("prompt stalled on the flush")
			}
			if got.resp == nil {
				t.Fatal("no prompt response")
			}
			var start *pyacp.ToolCallStart
			for _, u := range got.seen {
				if s, ok := u.update.(*pyacp.ToolCallStart); ok && start == nil {
					start = s
				}
			}
			if start == nil {
				t.Fatalf("no tool call start in %v", got.seen)
			}
			var statuses []string
			for _, u := range got.seen {
				if p, ok := u.update.(*pyacp.ToolCallProgress); ok && p.ToolCallID == start.ToolCallID {
					statuses = append(statuses, *p.Status)
				}
			}
			if !slices.Equal(statuses, []string{"failed"}) {
				t.Fatalf("close statuses = %v", statuses)
			}
		})
	}
}

func slashState(t *testing.T, m *SessionManager) *SessionState {
	t.Helper()
	st := mustCreate(t, m, "/tmp")
	mock := mockOf(t, st)
	mock.model, mock.provider = "test-model", "openrouter"
	st.Model = "test-model"
	return st
}

func TestModelShowsCurrent(t *testing.T) {
	a, m := newTestAgent(t)
	st := slashState(t, m)
	reply, known := a.handleSlashCommand(context.Background(), "/model", st)
	if !known || !strings.Contains(reply, "test-model") {
		t.Fatalf("reply = %q, %v", reply, known)
	}
}

func TestResetClearsHistory(t *testing.T) {
	a, m := newTestAgent(t)
	st := slashState(t, m)
	st.History = []map[string]any{{"role": "user", "content": "hello"}}
	a.handleSlashCommand(context.Background(), "/reset", st)
	if len(st.History) != 0 {
		t.Fatalf("history = %v", st.History)
	}
}

type countingSave struct {
	*SessionManager
	saved []string
}

func (c *countingSave) SaveSession(ctx context.Context, sessionID string) {
	c.saved = append(c.saved, sessionID)
}

func TestCompactCompressesContext(t *testing.T) {
	m := newMockManager(t)
	sessions := &countingSave{SessionManager: m}
	a := NewHermesACPAgent(sessions)
	st := slashState(t, m)
	history := []map[string]any{
		{"role": "user", "content": "one"}, {"role": "assistant", "content": "two"},
		{"role": "user", "content": "three"}, {"role": "assistant", "content": "four"},
	}
	st.History = history
	mock := mockOf(t, st)
	mock.cachedSystemPrompt = "system"
	mock.db = provenanceDB(t)
	original := mock.db
	calls := 0
	swap(t, &compressNow, func(ctx context.Context, ag any, messages []map[string]any, request agent.CompressRequest, o agent.CompressNowOptions) (agent.CompressResult, error) {
		calls++
		if ag != any(mock) || !mock.storeDetached {
			t.Errorf("session store attached during compression")
		}
		if !reflect.DeepEqual(messages, history) || o.SystemMessage == nil || *o.SystemMessage != "system" || o.TaskID != st.SessionID {
			t.Errorf("compress arguments %v %+v", messages, o)
		}
		if request.FocusTopic != nil || request.Preview || request.Partial || request.Aggressive {
			t.Errorf("request = %+v", request)
		}
		return agent.CompressResult{Status: "compressed", BeforeMessages: messages, AfterMessages: []map[string]any{{"role": "user", "content": "summary"}},
			BeforeTokens: 40, AfterTokens: 12, Request: request}, nil
	})
	reply, _ := a.handleSlashCommand(context.Background(), "/compress", st)
	if !strings.Contains(reply, "Context compressed: 4 -> 1 messages") || !strings.Contains(reply, "~40 -> ~12 tokens") {
		t.Fatalf("reply = %q", reply)
	}
	if !reflect.DeepEqual(st.History, []map[string]any{{"role": "user", "content": "summary"}}) {
		t.Fatalf("history = %v", st.History)
	}
	if mock.db != original || mock.storeDetached || calls != 1 {
		t.Fatalf("store restored %v, detached %v, calls %d", mock.db == original, mock.storeDetached, calls)
	}
	if !slices.Equal(sessions.saved, []string{st.SessionID}) {
		t.Fatalf("saved = %v", sessions.saved)
	}
}

func TestUnknownCommandReturnsNone(t *testing.T) {
	a, m := newTestAgent(t)
	st := slashState(t, m)
	if reply, known := a.handleSlashCommand(context.Background(), "/nonexistent", st); known {
		t.Fatalf("reply = %q", reply)
	}
}

func TestSlashHandlerCwdPinDoesNotLeak(t *testing.T) {
	a, m := newTestAgent(t)
	workspace := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	st := mustCreate(t, m, workspace)
	st.Cwd = workspace
	mock := mockOf(t, st)
	mock.model, mock.provider = "test-model", "openrouter"
	before, err := tools.ResolveAgentCwd(mock.env)
	if err != nil {
		t.Fatal(err)
	}
	pinned := ""
	a.commandHandlers["help"] = func(ctx context.Context, args string, st *SessionState) (string, error) {
		pinned = tools.ScopedSessionCwd(mock.env)
		return a.cmdHelp(ctx, args, st)
	}
	a.handleSlashCommand(context.Background(), "/help", st)
	after, err := tools.ResolveAgentCwd(mock.env)
	if err != nil || after != before {
		t.Fatalf("cwd %q -> %q, %v", before, after, err)
	}
	if pinned != workspace {
		t.Fatalf("handler ran with cwd pin %q", pinned)
	}
}

func stdioServer(name, command string, args []string, env ...pyacp.EnvVariable) *pyacp.McpServerStdio {
	if args == nil {
		args = []string{}
	}
	if env == nil {
		env = []pyacp.EnvVariable{}
	}
	return &pyacp.McpServerStdio{Name: name, Command: command, Args: args, Env: env}
}

func TestRegistersStdioServers(t *testing.T) {
	a, m := newTestAgent(t)
	st := mustCreate(t, m, "/tmp")
	mock := mockOf(t, st)
	mock.enabledToolsets, mock.toolDefs, mock.validToolNames = []string{"icebath-acp"}, []map[string]any{}, map[string]bool{}
	registered := map[string]map[string]any{}
	pinnedCwd := ""
	swap(t, &registerMCPServers, func(ctx context.Context, env *tools.Env, configs map[string]map[string]any) ([]string, error) {
		for name, cfg := range configs {
			registered[name] = cfg
		}
		pinnedCwd, _ = tools.ResolveContextCwd(env, true)
		return []string{"mcp_test_server_tool1"}, nil
	})
	swap(t, &getToolDefinitions, func(context.Context, *tools.Env, []string, []string) []map[string]any { return []map[string]any{} })
	server := stdioServer("test-server", "/usr/bin/test", []string{"--flag"}, pyacp.EnvVariable{Name: "KEY", Value: "val"})
	a.registerSessionMCPServers(context.Background(), st, []pyacp.McpServer{server})
	cfg, ok := registered["test-server"]
	if !ok {
		t.Fatalf("registered = %v", registered)
	}
	if cfg["command"] != "/usr/bin/test" || !reflect.DeepEqual(cfg["args"], []any{"--flag"}) || !reflect.DeepEqual(cfg["env"], map[string]any{"KEY": "val"}) {
		t.Fatalf("config = %v", cfg)
	}
	if pinnedCwd != "/tmp" {
		t.Fatalf("pinned cwd = %q", pinnedCwd)
	}
}

type recallProvider struct{ memory.BaseProvider }

func (recallProvider) Name() string                   { return "hindsight" }
func (recallProvider) Available(context.Context) bool { return true }
func (recallProvider) Initialize(context.Context, string, memory.InitOptions) error {
	return nil
}
func (recallProvider) ToolSchemas(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"name": "hindsight_recall", "description": "Recall", "parameters": map[string]any{}}}, nil
}

func TestRefreshesAgentToolSurface(t *testing.T) {
	a, m := newTestAgent(t)
	ctx := context.Background()
	st := mustCreate(t, m, "/tmp")
	mock := mockOf(t, st)
	mock.enabledToolsets, mock.toolDefs, mock.validToolNames = []string{"icebath-acp"}, []map[string]any{}, map[string]bool{}
	mock.cachedSystemPrompt = "old prompt"
	manager := memory.NewMemoryManager(ctx, mock.env, nil)
	if err := manager.AddProvider(ctx, recallProvider{}); err != nil {
		t.Fatal(err)
	}
	mock.memoryManager = manager
	fakeTools := []map[string]any{
		{"function": map[string]any{"name": "mcp_srv_search"}},
		{"function": map[string]any{"name": "memory"}},
		{"function": map[string]any{"name": "terminal"}},
	}
	swap(t, &registerMCPServers, func(context.Context, *tools.Env, map[string]map[string]any) ([]string, error) {
		return []string{"mcp_srv_search"}, nil
	})
	type defsCall struct{ enabled, disabled []string }
	var defsCalls []defsCall
	swap(t, &getToolDefinitions, func(ctx context.Context, env *tools.Env, enabled, disabled []string) []map[string]any {
		defsCalls = append(defsCalls, defsCall{enabled, disabled})
		return fakeTools
	})
	a.registerSessionMCPServers(ctx, st, []pyacp.McpServer{stdioServer("srv", "/bin/test", nil)})
	if len(defsCalls) != 1 || !slices.Equal(defsCalls[0].enabled, []string{"icebath-acp", "mcp-srv"}) || defsCalls[0].disabled != nil {
		t.Fatalf("tool definition calls = %+v", defsCalls)
	}
	if !slices.Equal(mock.enabledToolsets, []string{"icebath-acp", "mcp-srv"}) {
		t.Fatalf("enabled toolsets = %v", mock.enabledToolsets)
	}
	if len(mock.toolDefs) != 4 || !reflect.DeepEqual(mock.toolDefs[:3], fakeTools) {
		t.Fatalf("tools = %v", mock.toolDefs)
	}
	last := map[string]any{"type": "function", "function": map[string]any{"name": "hindsight_recall", "description": "Recall", "parameters": map[string]any{}}}
	if !reflect.DeepEqual(mock.toolDefs[3], last) {
		t.Fatalf("injected tool = %v", mock.toolDefs[3])
	}
	want := map[string]bool{"hindsight_recall": true, "memory": true, "mcp_srv_search": true, "terminal": true}
	if !reflect.DeepEqual(mock.validToolNames, want) {
		t.Fatalf("valid names = %v", mock.validToolNames)
	}
	if mock.invalidations != 1 {
		t.Fatalf("system prompt invalidated %d times", mock.invalidations)
	}
}

func TestRegisterFailureLogsWarning(t *testing.T) {
	a, m := newTestAgent(t)
	st := mustCreate(t, m, "/tmp")
	swap(t, &registerMCPServers, func(context.Context, *tools.Env, map[string]map[string]any) ([]string, error) {
		return nil, errors.New("boom")
	})
	a.registerSessionMCPServers(context.Background(), st, []pyacp.McpServer{stdioServer("bad", "/nonexistent", nil)})
}

func TestCmdToolsStripsConfiguredDisabledToolsets(t *testing.T) {
	a, m := newTestAgent(t)
	st := mustCreate(t, m, "/tmp")
	mock := mockOf(t, st)
	mock.enabledToolsets = []string{"icebath-acp"}
	listed := func() map[string]bool {
		reply, err := a.cmdTools(context.Background(), "", st)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, line := range strings.Split(reply, "\n")[1:] {
			names[strings.SplitN(strings.TrimSpace(line), ":", 2)[0]] = true
		}
		return names
	}
	mock.disabledToolsets = nil
	if !listed()["execute_code"] {
		t.Fatal("execute_code not offered by default")
	}
	mock.disabledToolsets = []string{"code_execution"}
	if listed()["execute_code"] {
		t.Fatal("execute_code listed while its toolset is disabled")
	}
}

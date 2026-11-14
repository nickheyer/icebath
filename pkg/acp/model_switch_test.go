package acp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
)

type switchSessions struct {
	*SessionManager
	mu      sync.Mutex
	made    []MakeAgentOptions
	makeErr error
	state   *SessionState
}

func (s *switchSessions) MakeAgent(ctx context.Context, o MakeAgentOptions) (any, error) {
	if s.makeErr != nil {
		return nil, s.makeErr
	}
	s.mu.Lock()
	s.made = append(s.made, o)
	s.mu.Unlock()
	return &nsAgent{provider: o.RequestedProvider, model: o.Model}, nil
}

func (s *switchSessions) SaveSession(context.Context, string) {}

func (s *switchSessions) GetSession(context.Context, string) *SessionState { return s.state }

func switchAgent(t *testing.T) (*HermesACPAgent, *switchSessions) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	sessions := &switchSessions{}
	return NewHermesACPAgent(sessions), sessions
}

func switchState(enabled, disabled []string) *SessionState {
	return &SessionState{SessionID: "s1", Cwd: ".", Model: "claude-sonnet-5", CancelEvent: &pythread.Event{},
		Agent: &mockAgent{provider: "anthropic", baseURL: "https://api.anthropic.com", apiKey: "k", enabledToolsets: enabled, disabledToolsets: disabled}}
}

func switchResult(t *testing.T, fn func(opts provider.SwitchModelOptions) provider.ModelSwitchResult) {
	t.Helper()
	swap(t, &switchModelFn, func(ctx context.Context, opts provider.SwitchModelOptions) (provider.ModelSwitchResult, error) {
		return fn(opts), nil
	})
}

func TestAcpAndDashboardRejectWhatSwitchModelRejects(t *testing.T) {
	a, sessions := switchAgent(t)
	switchResult(t, func(provider.SwitchModelOptions) provider.ModelSwitchResult {
		return provider.ModelSwitchResult{Success: false, ErrorMessage: "Unknown provider 'notaprovider'."}
	})
	st := switchState(nil, nil)
	ctx := context.Background()
	_, _, _, err := a.switchModel(ctx, st, "notaprovider:whatever", false)
	var rejected *ModelRejected
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "Unknown provider") {
		t.Fatalf("err = %v", err)
	}
	if len(sessions.made) != 0 || st.Model != "claude-sonnet-5" {
		t.Fatalf("session touched: %v %q", sessions.made, st.Model)
	}
	_, err = provider.ApplyModelAssignmentSync(ctx, "main", "notaprovider", "whatever", "", "", "", provider.AuxReasoningEffort{}, nil)
	var refused *provider.HTTPException
	if !errors.As(err, &refused) || refused.StatusCode != 400 || !strings.Contains(refused.Detail, "Unknown provider") {
		t.Fatalf("dashboard err = %v", err)
	}
}

func TestAcpExplicitProviderPrefixBecomesExplicitProvider(t *testing.T) {
	a, sessions := switchAgent(t)
	var seen provider.SwitchModelOptions
	switchResult(t, func(opts provider.SwitchModelOptions) provider.ModelSwitchResult {
		seen = opts
		return provider.ModelSwitchResult{Success: true, NewModel: opts.RawInput, TargetProvider: opts.ExplicitProvider}
	})
	old, newProvider, model, err := a.switchModel(context.Background(), switchState(nil, nil), "anthropic:claude-sonnet-5", true)
	if err != nil {
		t.Fatal(err)
	}
	if seen.ExplicitProvider != "anthropic" || seen.RawInput != "claude-sonnet-5" {
		t.Fatalf("switch options = %+v", seen)
	}
	if old != "anthropic" || newProvider != "anthropic" || model != "claude-sonnet-5" {
		t.Fatalf("switch = %q %q %q", old, newProvider, model)
	}
	if sessions.made[0].RequestedProvider != "anthropic" || sessions.made[0].BaseURL != "https://api.anthropic.com" {
		t.Fatalf("rebuild options = %+v", sessions.made[0])
	}
}

func TestAcpSetSessionModelRunsSwitchModelOffTheEventLoop(t *testing.T) {
	a, sessions := switchAgent(t)
	st := switchState(nil, nil)
	sessions.state = st
	switched := make(chan struct{})
	release := make(chan struct{})
	switchResult(t, func(opts provider.SwitchModelOptions) provider.ModelSwitchResult {
		close(switched)
		<-release
		return provider.ModelSwitchResult{Success: true, NewModel: opts.RawInput, TargetProvider: "anthropic"}
	})
	done := make(chan *pyacp.SetSessionModelResponse, 1)
	go func() {
		resp, _ := a.SetSessionModel(context.Background(), &pyacp.SetSessionModelRequest{ModelID: "anthropic:claude-sonnet-5", SessionID: "s1"})
		done <- resp
	}()
	<-switched
	if listed, err := a.Initialize(context.Background(), &pyacp.InitializeRequest{ProtocolVersion: 1}); err != nil || listed == nil {
		t.Fatalf("another request stalled behind the switch: %v", err)
	}
	close(release)
	if resp := <-done; resp == nil || st.Model != "claude-sonnet-5" {
		t.Fatalf("resp = %v model %q", resp, st.Model)
	}
}

func TestAcpSetSessionModelRejectedWhileTurnRunning(t *testing.T) {
	a, sessions := switchAgent(t)
	st := switchState(nil, nil)
	st.IsRunning = true
	sessions.state = st
	called := false
	switchResult(t, func(provider.SwitchModelOptions) provider.ModelSwitchResult {
		called = true
		return provider.ModelSwitchResult{}
	})
	_, err := a.SetSessionModel(context.Background(), &pyacp.SetSessionModelRequest{ModelID: "anthropic:claude-sonnet-5", SessionID: "s1"})
	var refused *pyacp.RequestError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	if called || len(sessions.made) != 0 || st.Model != "claude-sonnet-5" || st.CommandOp {
		t.Fatalf("called %v made %v model %q command_op %v", called, sessions.made, st.Model, st.CommandOp)
	}
}

func TestAcpSwitchModelCarriesTheLiveAgentToolsetsIntoTheRebuild(t *testing.T) {
	a, sessions := switchAgent(t)
	switchResult(t, func(provider.SwitchModelOptions) provider.ModelSwitchResult {
		return provider.ModelSwitchResult{Success: true, TargetProvider: "anthropic", NewModel: "claude-sonnet-5"}
	})
	st := switchState([]string{"icebath-acp", "mcp-demo-search"}, []string{"browser"})
	if _, _, _, err := a.switchModel(context.Background(), st, "claude-sonnet-5", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sessions.made[0].EnabledToolsets, []string{"icebath-acp", "mcp-demo-search"}) ||
		!reflect.DeepEqual(sessions.made[0].DisabledToolsets, []string{"browser"}) {
		t.Fatalf("rebuild options = %+v", sessions.made[0])
	}
}

func TestAcpSetSessionModelRejectionIsInvalidParamsAndLeavesSessionUntouched(t *testing.T) {
	a, sessions := switchAgent(t)
	ctx := context.Background()
	st := switchState(nil, nil)
	sessions.state = st
	switchResult(t, func(provider.SwitchModelOptions) provider.ModelSwitchResult {
		return provider.ModelSwitchResult{Success: false, ErrorMessage: "`nope` is not a model"}
	})
	_, err := a.SetSessionModel(ctx, &pyacp.SetSessionModelRequest{ModelID: "nope", SessionID: "s1"})
	var refused *pyacp.RequestError
	if !errors.As(err, &refused) || refused.Code != -32602 {
		t.Fatalf("err = %v", err)
	}
	data, ok := refused.Data.(*pyjson.Object)
	if !ok || !reflect.DeepEqual(data.Vals, map[string]any{"details": "`nope` is not a model"}) {
		t.Fatalf("data = %#v", refused.Data)
	}
	switchResult(t, func(provider.SwitchModelOptions) provider.ModelSwitchResult {
		return provider.ModelSwitchResult{Success: true, NewModel: "other", TargetProvider: "anthropic"}
	})
	sessions.makeErr = errors.New("No Codex credentials stored")
	oldAgent := st.Agent
	if _, _, _, err := a.switchModel(ctx, st, "other", false); err == nil || !strings.Contains(err.Error(), "No Codex credentials") {
		t.Fatalf("err = %v", err)
	}
	if st.Model != "claude-sonnet-5" || st.Agent != oldAgent {
		t.Fatalf("session moved to %q", st.Model)
	}
	sessions.makeErr = errors.New("provider 'anthropic' is disabled in config")
	_, err = a.SetSessionModel(ctx, &pyacp.SetSessionModelRequest{ModelID: "other", SessionID: "s1"})
	if err == nil || !strings.Contains(err.Error(), "disabled in config") {
		t.Fatalf("err = %v", err)
	}
	var relabelled *pyacp.RequestError
	if errors.As(err, &relabelled) {
		t.Fatalf("a rebuild failure was relabelled as a request error: %v", err)
	}
	if st.Model != "claude-sonnet-5" || st.Agent != oldAgent {
		t.Fatalf("session moved to %q", st.Model)
	}
}

func TestAcpSetSessionModelDoesNotRunQueuedPromptsInsideTheRpc(t *testing.T) {
	a, sessions := switchAgent(t)
	ctx := context.Background()
	switchResult(t, func(provider.SwitchModelOptions) provider.ModelSwitchResult {
		return provider.ModelSwitchResult{Success: false, ErrorMessage: "`nope` is not a model"}
	})
	st := switchState(nil, nil)
	st.QueuedPrompts = []string{"hello, queued mid-switch"}
	sessions.state = st
	a.conn = &captureConn{}
	var mu sync.Mutex
	var ran []string
	gate := make(chan struct{})
	a.prompter = func(ctx context.Context, req *pyacp.PromptRequest) (*pyacp.PromptResponse, error) {
		<-gate
		mu.Lock()
		ran = append(ran, req.Prompt[0].(*pyacp.TextContentBlock).Text)
		mu.Unlock()
		return &pyacp.PromptResponse{StopReason: "end_turn"}, nil
	}
	_, err := a.SetSessionModel(ctx, &pyacp.SetSessionModelRequest{ModelID: "nope", SessionID: "s1"})
	var refused *pyacp.RequestError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	inside := len(ran)
	mu.Unlock()
	if inside != 0 {
		t.Fatal("the queued prompt ran inside the set_model RPC")
	}
	close(gate)
	a.background.Wait()
	if !reflect.DeepEqual(ran, []string{"hello, queued mid-switch"}) || st.CommandOp {
		t.Fatalf("ran %v command_op %v", ran, st.CommandOp)
	}
}

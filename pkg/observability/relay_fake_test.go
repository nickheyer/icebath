package observability

import (
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeHandle struct {
	kind, name string
	serial     int
}

type fakeRequest struct{ headers, content map[string]any }

type fakeEvent struct {
	op        string
	name      string
	handle    *fakeHandle
	scopeType any
	kwargs    map[string]any
	payload   any
}

type obsRelayFake struct {
	mu          sync.Mutex
	events      []fakeEvent
	callbacks   *pydict.Dict[string, func(*tools.RelayEventView)]
	starts      map[*fakeHandle]map[string]any
	toolStarts  map[*fakeHandle]map[string]any
	scopeStarts map[*fakeHandle]map[string]any
	stackVar    *pyctx.Var
	serial      int
	flushHook   func() error
	relay       *tools.NemoRelay
}

func newObsRelayFake() *obsRelayFake {
	f := &obsRelayFake{
		callbacks: pydict.New[string, func(*tools.RelayEventView)](), starts: map[*fakeHandle]map[string]any{},
		toolStarts: map[*fakeHandle]map[string]any{}, scopeStarts: map[*fakeHandle]map[string]any{},
		stackVar: pyctx.NewVar("relay_scope_stack", nil),
	}
	f.relay = &tools.NemoRelay{
		Scope:         &tools.NemoRelayScope{Push: f.push, Pop: f.pop, Event: f.event},
		ScopeType:     tools.NemoRelayScopeTypes{Agent: "agent", Function: "function"},
		GetScopeStack: f.getScopeStack,
		Subscribers:   &tools.NemoRelaySubscribers{Register: f.register, Deregister: f.deregister, Flush: f.flush},
		LLM:           &tools.NemoRelayLLM{Call: f.llmCall, CallEnd: f.llmCallEnd},
		Tools:         &tools.NemoRelayTools{Call: f.toolCall, CallEnd: f.toolCallEnd},
		LLMRequest: func(headers, content map[string]any) (any, error) {
			return &fakeRequest{headers, content}, nil
		},
		ToolExecutionResult: func(result any) (*tools.RelayToolExecutionResult, error) {
			return &tools.RelayToolExecutionResult{Result: result}, nil
		},
	}
	return f
}

func (f *obsRelayFake) record(e fakeEvent) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *obsRelayFake) emit(view *tools.RelayEventView) {
	f.mu.Lock()
	callbacks := f.callbacks.Values()
	f.mu.Unlock()
	for _, cb := range callbacks {
		cb(view)
	}
}

func (f *obsRelayFake) stack(ctx context.Context) *[]*fakeHandle {
	s, _ := f.stackVar.Get(ctx).(*[]*fakeHandle)
	if s == nil {
		s = &[]*fakeHandle{}
		f.stackVar.Set(ctx, s)
	}
	return s
}

func (f *obsRelayFake) push(ctx context.Context, name string, scopeType any, kwargs map[string]any) (any, error) {
	f.mu.Lock()
	f.serial++
	h := &fakeHandle{"scope", name, f.serial}
	f.mu.Unlock()
	s := f.stack(ctx)
	f.mu.Lock()
	*s = append(*s, h)
	if scopeType == "function" {
		f.scopeStarts[h] = kwargs
	}
	f.mu.Unlock()
	f.record(fakeEvent{op: "scope.push", name: name, handle: h, scopeType: scopeType, kwargs: kwargs})
	if scopeType == "function" {
		f.emit(&tools.RelayEventView{Kind: "scope", Category: "function", Name: name, ScopeCategory: "start", Metadata: kwargs["metadata"], Data: kwargs["input"]})
	}
	return h, nil
}

func (f *obsRelayFake) pop(ctx context.Context, handle any, kwargs map[string]any) (any, error) {
	h, _ := handle.(*fakeHandle)
	s := f.stack(ctx)
	f.mu.Lock()
	if len(*s) == 0 || (*s)[len(*s)-1] != h {
		f.mu.Unlock()
		f.record(fakeEvent{op: "scope.pop.rejected", handle: h})
		return nil, &tools.RelayError{Class: "RuntimeError", Message: "scope handle is not at the top of the stack"}
	}
	*s = (*s)[:len(*s)-1]
	start, ok := f.scopeStarts[h]
	delete(f.scopeStarts, h)
	f.mu.Unlock()
	f.record(fakeEvent{op: "scope.pop", name: h.name, handle: h, kwargs: kwargs})
	if ok {
		metadata := map[string]any{}
		if m, _ := start["metadata"].(map[string]any); m != nil {
			maps.Copy(metadata, m)
		}
		if m, _ := kwargs["metadata"].(map[string]any); m != nil {
			maps.Copy(metadata, m)
		}
		f.emit(&tools.RelayEventView{Kind: "scope", Category: "function", Name: h.name, ScopeCategory: "end", Metadata: metadata, Data: kwargs["output"]})
	}
	return nil, nil
}

func (f *obsRelayFake) event(_ context.Context, name string, kwargs map[string]any) (any, error) {
	f.record(fakeEvent{op: "scope.event", name: name, kwargs: kwargs})
	f.emit(&tools.RelayEventView{Kind: "mark", Name: name, Metadata: kwargs["metadata"], Data: kwargs["data"]})
	return nil, nil
}

func (f *obsRelayFake) getScopeStack(ctx context.Context) (any, error) {
	s := f.stack(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(*s) == 0 {
		return nil, nil
	}
	return (*s)[len(*s)-1], nil
}

func (f *obsRelayFake) llmCall(_ context.Context, name string, request any, kwargs map[string]any) (any, error) {
	f.mu.Lock()
	h := &fakeHandle{"llm", name, len(f.starts)}
	f.starts[h] = kwargs
	f.mu.Unlock()
	f.record(fakeEvent{op: "llm.call", name: name, handle: h, kwargs: kwargs, payload: request.(*fakeRequest).content})
	return h, nil
}

func (f *obsRelayFake) llmCallEnd(_ context.Context, handle, response any, kwargs map[string]any) error {
	h := handle.(*fakeHandle)
	f.mu.Lock()
	start := f.starts[h]
	delete(f.starts, h)
	f.mu.Unlock()
	f.record(fakeEvent{op: "llm.call_end", name: h.name, handle: h, kwargs: kwargs, payload: response})
	metadata := maps.Clone(start["metadata"].(map[string]any))
	maps.Copy(metadata, kwargs["metadata"].(map[string]any))
	metadata["otel.status_code"] = "OK"
	f.emit(&tools.RelayEventView{Kind: "scope", Category: "llm", Name: h.name, ScopeCategory: "end", CategoryProfile: map[string]any{"model_name": start["model_name"]}, Metadata: metadata, Data: response})
	return nil
}

func (f *obsRelayFake) toolCall(_ context.Context, name string, args map[string]any, kwargs map[string]any) (any, error) {
	f.mu.Lock()
	h := &fakeHandle{"tool", name, len(f.toolStarts)}
	f.toolStarts[h] = kwargs
	f.mu.Unlock()
	f.record(fakeEvent{op: "tool.call", name: name, handle: h, kwargs: kwargs, payload: args})
	return h, nil
}

func (f *obsRelayFake) toolCallEnd(_ context.Context, handle, result any, kwargs map[string]any) error {
	h := handle.(*fakeHandle)
	payload := result.(*tools.RelayToolExecutionResult).Result
	f.mu.Lock()
	start := f.toolStarts[h]
	delete(f.toolStarts, h)
	f.mu.Unlock()
	f.record(fakeEvent{op: "tool.call_end", name: h.name, handle: h, kwargs: kwargs, payload: payload})
	metadata := maps.Clone(start["metadata"].(map[string]any))
	maps.Copy(metadata, kwargs["metadata"].(map[string]any))
	metadata["otel.status_code"] = "OK"
	f.emit(&tools.RelayEventView{Kind: "scope", Category: "tool", Name: h.name, ScopeCategory: "end", CategoryProfile: map[string]any{}, Metadata: metadata, Data: payload})
	return nil
}

func (f *obsRelayFake) register(name string, callback func(*tools.RelayEventView)) error {
	f.mu.Lock()
	f.callbacks.Set(name, callback)
	f.mu.Unlock()
	f.record(fakeEvent{op: "subscribers.register", name: name})
	return nil
}

func (f *obsRelayFake) deregister(name string) error {
	f.mu.Lock()
	f.callbacks.Delete(name)
	f.mu.Unlock()
	f.record(fakeEvent{op: "subscribers.deregister", name: name})
	return nil
}

func (f *obsRelayFake) flush() error {
	f.record(fakeEvent{op: "subscribers.flush"})
	if f.flushHook != nil {
		return f.flushHook()
	}
	return nil
}

// Events of one operation in emission order
func (f *obsRelayFake) ops(op string) []fakeEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeEvent
	for _, e := range f.events {
		if e.op == op {
			out = append(out, e)
		}
	}
	return out
}

// Task scope pops in emission order
func (f *obsRelayFake) taskEnds() []map[string]any {
	var out []map[string]any
	for _, e := range f.ops("scope.pop") {
		if e.name == TaskScope {
			out = append(out, e.kwargs["output"].(map[string]any))
		}
	}
	return out
}

// Fake binding behind a fresh profile whose shared metrics are on
func directRuntime(t *testing.T) (*obsRelayFake, context.Context) {
	t.Helper()
	fake := newObsRelayFake()
	dir := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("telemetry:\n  shared_metrics:\n    enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", dir)
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return fake.relay, nil })
	resetForTests()
	tools.ResetRelayRuntimeForTests()
	consentReconcileMu.Lock()
	consentReconcileDone = false
	consentReconcileMu.Unlock()
	savedReasons := FailoverReasonsHook
	FailoverReasonsHook = func() []string { return failoverReasonNames }
	savedSkills, savedProfiles, savedPlatforms := SkillIndexFilesHook, ProfileNamesHook, ConnectedPlatformsHook
	SkillIndexFilesHook = walkIndexFiles
	ProfileNamesHook = func(context.Context) []string { return []string{"default"} }
	ConnectedPlatformsHook = func(context.Context) (int, error) { return 0, nil }
	savedEstimate := EstimateToolsTokensHook
	EstimateToolsTokensHook = roughToolsTokens
	ctx := context.Background()
	t.Cleanup(func() {
		rt := liveRuntime(ctx)
		resetForTests()
		joinRuntimeWorkers(t, rt)
		tools.ResetRelayRuntimeForTests()
		tools.RegisterNemoRelayBinding(nil)
		FailoverReasonsHook = savedReasons
		SkillIndexFilesHook, ProfileNamesHook, ConnectedPlatformsHook = savedSkills, savedProfiles, savedPlatforms
		EstimateToolsTokensHook = savedEstimate
	})
	return fake, ctx
}

// Quarter of the schema name, description and compact parameter JSON characters, the model metadata estimate
func roughToolsTokens(defs []map[string]any) int {
	total := 0
	for _, tool := range defs {
		src := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			src = fn
		}
		for _, key := range []string{"name", "description"} {
			if text, ok := src[key].(string); ok {
				total += utf8.RuneCountInString(text)
			}
		}
		params := src["parameters"]
		if params == nil {
			params = map[string]any{}
		}
		encoded, _ := json.Marshal(params)
		total += utf8.RuneCount(encoded)
	}
	return (total + 3) / 4
}

// Every file under a skills tree carrying the index name, the skills package walk
func walkIndexFiles(skillsDir, filename string) []string {
	var out []string
	_ = filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == filename {
			out = append(out, path)
		}
		return nil
	})
	return out
}

var failoverReasonNames = strings.Fields("auth auth_permanent billing rate_limit upstream_rate_limit upstream_blocked " +
	"overloaded server_error timeout ssl_cert_verification context_overflow payload_too_large image_too_large " +
	"image_corrupt model_not_found provider_policy_blocked data_policy_denied content_policy_blocked model_entitlement " +
	"incomplete_response format_error role_alternation invalid_encrypted_content multimodal_tool_content_unsupported " +
	"reasoning_mandatory thinking_signature long_context_tier oauth_long_context_beta_forbidden llama_cpp_grammar_pattern unknown")

// Lifecycle hook through the built in observers with no plugins loaded
func invokeHook(ctx context.Context, name string, kw map[string]any, extra ...map[string]any) {
	ObserveLifecycle(ctx, nil, name, mergeEvent(kw, extra...))
}

// Observers then the hard close of one core owned Relay conversation
func finalizeSession(ctx context.Context, sessionID string) {
	ObserveLifecycle(ctx, nil, "on_session_finalize", map[string]any{"session_id": sessionID})
	tools.RelayCoordinator.FinalizeConversation(ctx, tools.CurrentRelayProfileKey(ctx), sessionID)
}

// Waits for the active runtime's background flush and send workers
func joinExportWorkers(t *testing.T, ctx context.Context) {
	t.Helper()
	joinRuntimeWorkers(t, liveRuntime(ctx))
}

// Waits for one runtime's background flush and send workers
func joinRuntimeWorkers(t *testing.T, rt *Runtime) {
	t.Helper()
	if rt == nil {
		return
	}
	for {
		rt.flushMu.Lock()
		done := rt.flushThread
		rt.flushMu.Unlock()
		if done == nil {
			break
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("export worker still alive")
		}
	}
	rt.sendMu.Lock()
	send := rt.sendThread
	rt.sendMu.Unlock()
	if send != nil {
		<-send
	}
}

var successEnd = map[string]any{"completed": true, "failed": false, "interrupted": false, "turn_exit_reason": "text_response(stop)"}

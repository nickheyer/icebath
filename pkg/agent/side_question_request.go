package agent

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	sideQuestionOrigin     = "side_question"
	pluginSideQuestionTask = "plugin_side_question"
)

var (
	clientToolTypes              = map[string]bool{"function": true, "custom": true}
	routingOnlyOpenRouterPlugins = map[string]bool{"pareto-router": true}
)

func sideQuestionRequestLog() *slog.Logger {
	return slog.Default().With("logger", "agent.side_question_request")
}

// Model, provider, base URL, api mode and reasoning config the fork talks to
type sideQuestionRuntime struct {
	model, provider, baseURL, apiMode string
	reasoningConfig                   map[string]any
}

// Parent request inputs frozen at turn finalization, the parent held weakly
type sideQuestionCapture struct {
	parent             weak.Pointer[AIAgent]
	messages           []map[string]any
	systemPrompt       string
	systemPromptStatic string
	tools              []any
	sessionID          string
	turnID             string
	runtime            sideQuestionRuntime
	ctx                context.Context
}

// Runtime a turn routed elsewhere restores after post_llm_call, compared against the capture
func sideQuestionRuntimeKey(a *AIAgent) sideQuestionRuntime {
	return sideQuestionRuntime{a.model, a.provider, a.baseURL, a.apiMode, a.reasoningConfig}
}

type sideQuestionAudit struct {
	pluginID string
	started  float64
	outcome  string
	usage    map[string]any
}

// Why the runtime cannot serve a text only side request, MoA or an agent as provider, empty otherwise
func agentProviderRefusal(ctx context.Context, a *AIAgent) string {
	if a.provider == "moa" {
		return "moa"
	}
	base := pystr.Lower(a.baseURL)
	if strings.HasPrefix(base, "acp://") || strings.HasPrefix(base, "acp+tcp://") || a.apiMode == "codex_app_server" ||
		provider.IsExternalProcessProvider(ctx, a.provider) || !parentCanEmitToolCalls(a) {
		return "agent_provider"
	}
	return ""
}

// True when the final request asks the provider to execute a tool, web search or plugin itself
func hasServerSideTools(apiKwargs map[string]any) bool {
	extraBody, ok := apiKwargs["extra_body"].(map[string]any)
	if !ok {
		extraBody = map[string]any{}
	}
	for _, tool := range append(anyItems(apiKwargs["tools"]), anyItems(extraBody["tools"])...) {
		entry, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if toolType := entry["type"]; pyTruthy(toolType) {
			if s, ok := toolType.(string); !ok || !clientToolTypes[s] {
				return true
			}
		}
	}
	_, top := apiKwargs["web_search_options"]
	_, extra := extraBody["web_search_options"]
	if top || extra {
		return true
	}
	for _, p := range append(anyItems(apiKwargs["plugins"]), anyItems(extraBody["plugins"])...) {
		entry, ok := p.(map[string]any)
		if !ok {
			return true
		}
		if id, ok := entry["id"].(string); !ok || !routingOnlyOpenRouterPlugins[id] {
			return true
		}
	}
	return false
}

// Snapshot cut back to its last completed assistant text, keeping the warm prefix
func trimSnapshotForFork(history []map[string]any) []map[string]any {
	msgs := append([]map[string]any{}, history...)
	for len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last != nil && last["role"] == "assistant" && !pyTruthy(last["tool_calls"]) {
			break
		}
		msgs = msgs[:len(msgs)-1]
	}
	return msgs
}

// Never routed cache parity fork pinned to the captured request inputs
func sideQuestionBuildFork(ctx context.Context, parent *AIAgent, capture *sideQuestionCapture) (*AIAgent, error) {
	fork, _, _, err := buildCacheParityFork(ctx, parent, map[string]any{}, 1, sideQuestionOrigin)
	if err != nil {
		return nil, err
	}
	fork.cachedSystemPrompt = capture.systemPrompt
	fork.cachedSystemPromptStatic = capture.systemPromptStatic
	fork.tools = capture.tools
	names := map[string]bool{}
	for _, tool := range capture.tools {
		entry, _ := tool.(map[string]any)
		fn, _ := pyjson.Dict(entry["function"])
		name, ok := fn["name"].(string)
		if !ok {
			return nil, fmt.Errorf("KeyError: 'name'")
		}
		names[name] = true
	}
	fork.validToolNames = names
	fork.sessionID = capture.sessionID
	return fork, nil
}

// Assembles, builds and performs one request on the fork, nil on refusal or a cancelled call
func sendSideQuestion(ctx context.Context, fork *AIAgent, capture *sideQuestionCapture, prompt string, audit *sideQuestionAudit) (*provider.Response, error) {
	turnID := capture.turnID + ":" + sideQuestionOrigin
	taskID := uuid.NewString()
	apiRequestID := turnID + ":api:1"
	applyPerTurnResetState(fork)
	fork.currentTurnTimestamp = nowSeconds()
	fork.currentTurnID, fork.currentTaskID, fork.currentAPIRequestID = turnID, taskID, apiRequestID
	fork.DisableStreaming()

	rows := append(trimSnapshotForFork(capture.messages), map[string]any{"role": "user", "content": prompt})
	messages := make([]*state.Message, len(rows))
	for i, row := range rows {
		messages[i] = state.MessageFromDict(row)
	}
	s := &LoopState{
		Messages: messages, CurrentTurnUserIdx: len(messages) - 1, ActiveSystemPrompt: fork.cachedSystemPrompt,
		OriginalUserMessage: prompt, RequestLogger: sideQuestionRequestLog(),
		APICallCount: 1, APIRequestID: apiRequestID, EffectiveTaskID: taskID, TurnID: turnID,
	}
	if err := assembleAPIRequest(ctx, fork, s); err != nil {
		return nil, err
	}
	if err := buildRequestPayload(ctx, fork, s); err != nil {
		return nil, err
	}
	if hasServerSideTools(s.APIKwargs) {
		audit.outcome = "refused:server_side_tools"
		return nil, nil
	}
	client, _ := fork.client.(provider.AuxClient)
	hooks := provider.StartAuxCallHooks(ctx, provider.AuxCallHookArgs{
		AuxTask: pluginSideQuestionTask, Metadata: map[string]any{"api_request_id": apiRequestID, "plugin_id": audit.pluginID},
		Client: client, Kwargs: s.APIKwargs, Provider: fork.provider, Model: fork.model, APIMode: fork.apiMode, Streaming: false,
	})
	apiStart := nowSeconds()
	var assistant *provider.Response
	var normalized *provider.AuxNormalizedReply
	response, err := func() (any, error) {
		verdict, err := fork.performAPICall(ctx, APICallRequest{
			APIKwargs: s.APIKwargs, OriginalAPIKwargs: s.OriginalAPIKwargs, LLMMiddlewareTrace: s.LLMMiddlewareTrace,
			Retry: &TurnRetryState{}, RetryCount: 0, APICallCount: 1, APIRequestID: apiRequestID,
			EffectiveTaskID: taskID, TurnID: turnID, Interrupted: false,
		})
		if err != nil || verdict.Action == "break" || verdict.Response == nil {
			return nil, err
		}
		fork.recordResponseUsage(ctx, verdict.Response, messages, 1, nowSeconds()-apiStart, 0, 0)
		if assistant, err = fork.normalizeResponseForAgent(verdict.Response); err != nil {
			return nil, err
		}
		normalized = &provider.AuxNormalizedReply{Message: assistant, FinishReason: assistant.FinishReason,
			Usage: fork.usageSummaryForAPIRequestHook(verdict.Response)}
		return verdict.Response, nil
	}()
	if err != nil {
		provider.FinishAuxCallHooks(ctx, hooks, nil, err, nil)
		if cerr := provider.RelayCompleteLogicalCall(ctx, apiRequestID, provider.RelayLogicalCompletion{Outcome: "failed"}); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	if response == nil {
		provider.FinishAuxCallHooks(ctx, hooks, nil, &provider.InterruptedError{Message: "side question cancelled"}, nil)
		if cerr := provider.RelayCompleteLogicalCall(ctx, apiRequestID, provider.RelayLogicalCompletion{Outcome: "cancelled"}); cerr != nil {
			return nil, cerr
		}
		audit.outcome = "error"
		return nil, nil
	}
	if err := provider.RelayCompleteLogicalCall(ctx, apiRequestID, provider.RelayLogicalCompletion{Outcome: "success"}); err != nil {
		return nil, err
	}
	provider.FinishAuxCallHooks(ctx, hooks, response, nil, normalized)
	return assistant, nil
}

// Visible text of the reply with its tool calls dropped, empty for no answer
func sideQuestionAnswerText(fork *AIAgent, reply *provider.Response) string {
	var content any
	if reply.Message != nil {
		content = reply.Message.Content
	}
	text, ok := content.(string)
	if !ok {
		text = state.FlattenMessageText(content, "\n")
	}
	return pystr.Strip(fork.stripThinkBlocks(text))
}

// Logs one side question outcome with its usage buckets and latency
func sideQuestionLogAudit(audit *sideQuestionAudit) {
	pluginID := audit.pluginID
	if pluginID == "" {
		pluginID = "?"
	}
	counts := make([]any, 0, 5)
	for _, key := range []string{"api_calls", "input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens"} {
		counts = append(counts, pyIntOf(audit.usage[key]))
	}
	sideQuestionRequestLog().Info(fmt.Sprintf("side_question plugin=%s outcome=%s calls=%d in=%d cache_read=%d cache_write=%d out=%d latency=%.1fs",
		append(append([]any{pluginID, audit.outcome}, counts...), pythread.Monotonic()-audit.started)...))
}

// Asks the session's own agent the prompt after the captured turn, its text answer or nil
func askTextOnly(ctx context.Context, capture *sideQuestionCapture, prompt, pluginID string, timeout float64) (answer *string) {
	audit := &sideQuestionAudit{pluginID: pluginID, started: pythread.Monotonic(), outcome: "error", usage: map[string]any{}}
	parent := capture.parent.Value()
	refusal := "parent_gone"
	if parent != nil {
		refusal = agentProviderRefusal(ctx, parent)
	}
	if refusal != "" {
		audit.outcome = "refused:" + refusal
		sideQuestionLogAudit(audit)
		return nil
	}
	var (
		fork     *AIAgent
		builtMu  sync.Mutex
		built    *AIAgent
		timedOut atomic.Bool
	)
	timer := time.AfterFunc(time.Duration(timeout*float64(time.Second)), func() {
		timedOut.Store(true)
		builtMu.Lock()
		target := built
		builtMu.Unlock()
		if target != nil {
			target.InterruptWith("", InterruptOptions{ToolReason: "side question timed out"})
		}
	})
	fail := func(err any) {
		audit.outcome = "error"
		if timedOut.Load() {
			audit.outcome = "timeout"
		}
		name := pluginID
		if name == "" {
			name = "?"
		}
		sideQuestionRequestLog().Warn(fmt.Sprintf("side question %s (plugin=%s)", audit.outcome, name), "error", err)
		answer = nil
	}
	defer func() {
		if r := recover(); r != nil {
			fail(r)
		}
		timer.Stop()
		if fork != nil {
			usagePlugin := pluginID
			if usagePlugin == "" {
				usagePlugin = "?"
			}
			audit.usage = releaseCacheParityFork(ctx, parent, fork, pluginSideQuestionTask+":"+usagePlugin, capture.sessionID)
		}
		sideQuestionLogAudit(audit)
	}()
	var err error
	if fork, err = sideQuestionBuildFork(ctx, parent, capture); err != nil {
		fail(err)
		return nil
	}
	builtMu.Lock()
	built = fork
	builtMu.Unlock()
	if timedOut.Load() {
		audit.outcome = "timeout"
		return nil
	}
	if !reflect.DeepEqual(sideQuestionRuntimeKey(parent), capture.runtime) {
		audit.outcome = "refused:runtime_changed"
		return nil
	}
	reply, err := sendSideQuestion(ctx, fork, capture, prompt, audit)
	if err != nil {
		fail(err)
		return nil
	}
	if reply == nil {
		return nil
	}
	text := sideQuestionAnswerText(fork, reply)
	if text == "" {
		audit.outcome = "empty"
		return nil
	}
	audit.outcome = "ok"
	return &text
}

// Context the redeeming goroutine runs the side question under, a fresh copy of the captured one
func sideQuestionRunContext(capture *sideQuestionCapture) context.Context {
	return pyctx.With(capture.ctx, pyctx.CopyContext(capture.ctx))
}

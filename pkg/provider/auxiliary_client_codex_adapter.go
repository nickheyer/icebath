package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python ValueError an auxiliary adapter raises for a request its route refuses
type ValueError struct{ Message string }

func (e *ValueError) Error() string { return e.Message }

// SDK client the Codex adapter drives, chat completions plus the Responses create call
type auxResponsesClient interface {
	AuxClient
	CreateResponse(ctx context.Context, kwargs map[string]any) (any, error)
}

// Implemented by clients the resolver stamped with the provider they effectively serve
type auxEffectiveProviderClient interface {
	auxEffectiveProvider() string
}

// Provider the resolver stamped on the client, empty when none was stamped
func auxClientEffectiveProvider(client any) string {
	if c, ok := client.(auxEffectiveProviderClient); ok {
		return c.auxEffectiveProvider()
	}
	return ""
}

// Routes chat completions kwargs through a streamed Codex Responses request
type auxCodexCompletionsAdapter struct {
	client auxResponsesClient
	model  string
}

// Wire maps of a kwargs list value, nil for anything that is not a list of maps
func auxKwargMaps(v any) []map[string]any {
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		return mapsOfList(list)
	}
	return nil
}

// Python AttributeError of a lookup on a value lacking the attribute
type auxAttributeError struct{ typeName, attr string }

func (e *auxAttributeError) Error() string {
	return fmt.Sprintf("'%s' object has no attribute '%s'", e.typeName, e.attr)
}

// Python iteration of a kwargs list, empty when the key is absent and TypeError for a non list value
func auxIterKwarg(kwargs map[string]any, key string) ([]any, error) {
	v, has := kwargs[key]
	if !has {
		return nil, nil
	}
	switch list := v.(type) {
	case []any:
		return list, nil
	case []map[string]any:
		out := make([]any, len(list))
		for i, m := range list {
			out[i] = m
		}
		return out, nil
	}
	return nil, &auxTypeError{Message: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// Python list of dicts under the key, AttributeError for a non dict item and None empty only when allowed
func auxKwargDicts(kwargs map[string]any, key string, noneOK bool) ([]map[string]any, error) {
	if v, has := kwargs[key]; has && v == nil && noneOK {
		return nil, nil
	}
	items, err := auxIterKwarg(kwargs, key)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &auxAttributeError{typeName: pyrepr.TypeName(item), attr: "get"}
		}
		out = append(out, m)
	}
	return out, nil
}

// Fresh copy of a dict kwargs value of either map kind, false for anything that is not a dict
func auxDictKwarg(v any) (map[string]any, bool) {
	out := map[string]any{}
	switch m := v.(type) {
	case map[string]any:
		for k, item := range m {
			out[k] = item
		}
	case map[string]string:
		for k, item := range m {
			out[k] = item
		}
	default:
		return nil, false
	}
	return out, true
}

// Python TypeError an auxiliary adapter raises for a value of the wrong kind
type auxTypeError struct{ Message string }

func (e *auxTypeError) Error() string { return e.Message }

// Responses kwargs mirroring the main transport build, the alias map and issuer riding along under private keys
func (a *auxCodexCompletionsAdapter) buildResponsesKwargs(ctx context.Context, kwargs map[string]any) (map[string]any, string, any, error) {
	model := a.model
	if v, ok := kwargs["model"]; ok {
		model = pyStr(v)
	}
	wireModel := wireModelIdentity(model)
	host := a.client.BaseURL()
	route := classifyResponsesRoute("", host)
	isXAI := route.isXAI
	toolDefs := auxKwargMaps(kwargs["tools"])
	if len(toolDefs) > 0 {
		copied := make([]map[string]any, 0, len(toolDefs))
		for _, def := range toolDefs {
			copied = append(copied, deepCopyJSON(def).(map[string]any))
		}
		toolDefs, _ = tools.StripPatternAndFormat(copied)
		toolDefs, _ = tools.StripSlashEnum(toolDefs)
	}
	wireTools, wireAliases := aliasWireTools(responsesTools(toolDefs), codexParams{ctx: ctx, provider: auxClientEffectiveProvider(a.client), baseURL: host, isXAI: isXAI})
	renamed := map[string]string{}
	for alias, original := range wireAliases {
		renamed[original] = alias
	}
	instructions := "You are a helpful assistant."
	var replay []map[string]any
	messages, err := auxIterKwarg(kwargs, "messages")
	if err != nil {
		return nil, "", nil, err
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			return nil, "", nil, &auxAttributeError{typeName: pyrepr.TypeName(raw), attr: "get"}
		}
		content := msg["content"]
		if !pyrepr.Truthy(content) {
			content = ""
		}
		role, hasRole := msg["role"]
		if !hasRole {
			role = "user"
		}
		if role == "system" {
			if s, ok := content.(string); ok {
				instructions = s
			} else {
				instructions = pyrepr.Str(content)
			}
			continue
		}
		if len(renamed) > 0 && pyrepr.Truthy(msg["tool_calls"]) {
			calls, err := auxIterKwarg(msg, "tool_calls")
			if err != nil {
				return nil, "", nil, err
			}
			rewritten := make([]any, 0, len(calls))
			for _, call := range calls {
				tc, isMap := call.(map[string]any)
				if !isMap {
					rewritten = append(rewritten, call)
					continue
				}
				fn := map[string]any{}
				if fnRaw := tc["function"]; pyrepr.Truthy(fnRaw) {
					if fn, isMap = fnRaw.(map[string]any); !isMap {
						return nil, "", nil, &auxAttributeError{typeName: pyrepr.TypeName(fnRaw), attr: "get"}
					}
				}
				name, isStr := fn["name"].(string)
				alias, hit := renamed[name]
				if !isStr || !hit {
					rewritten = append(rewritten, tc)
					continue
				}
				fnCopy := make(map[string]any, len(fn))
				for k, v := range fn {
					fnCopy[k] = v
				}
				fnCopy["name"] = alias
				tcCopy := make(map[string]any, len(tc))
				for k, v := range tc {
					tcCopy[k] = v
				}
				tcCopy["function"] = fnCopy
				rewritten = append(rewritten, tcCopy)
			}
			msgCopy := make(map[string]any, len(msg))
			for k, v := range msg {
				msgCopy[k] = v
			}
			msgCopy["tool_calls"] = rewritten
			msg = msgCopy
		}
		replay = append(replay, msg)
	}
	issuerKind := classifyResponsesIssuer(route.isXAI, route.isCodexBackend, host, &route.endpoint)
	input, err := chatMessagesToResponsesInput(codexMsgsOf(messagesFromDicts(replay)), codexConvertOptions{
		dropReplayItemIDs: !route.endpoint.replayItemIDs, replayEncryptedReasoning: true,
		currentIssuerKind: issuerKind, currentIssuerModel: wireModel,
	})
	if err != nil {
		return nil, "", nil, err
	}
	if len(input) == 0 {
		input = []map[string]any{roleMessageItem("user", "", nil)}
	}
	resp := map[string]any{"model": wireModel, "instructions": instructions, "input": input, "store": false}
	timeout := kwargs["timeout"]
	if timeout != nil {
		resp["timeout"] = timeout
	}
	if headers, ok := auxDictKwarg(kwargs["extra_headers"]); ok && len(headers) > 0 {
		resp["extra_headers"] = headers
	}
	if extraBody, ok := kwargs["extra_body"].(map[string]any); ok {
		if tier, ok := extraBody["service_tier"].(string); ok && pyStripSpace(tier) != "" && !isXAI {
			resp["service_tier"] = pyStripSpace(tier)
		}
		if reasoning, ok := extraBody["reasoning"].(map[string]any); ok {
			supported := codexEffortsForRoute(model, host, route.isCodexBackend)
			enabled, enabledSet := reasoning["enabled"].(bool)
			if len(supported) > 0 && !(enabledSet && !enabled) {
				effort := "medium"
				if pyrepr.Truthy(reasoning["effort"]) {
					effort = pyStr(reasoning["effort"])
				}
				resp["reasoning"] = map[string]any{"effort": clampEffort(effort, supported, nil), "summary": "auto"}
				resp["include"] = []any{"reasoning.encrypted_content"}
			} else if containsString(supported, "none") && !isXAI {
				resp["reasoning"] = map[string]any{"effort": "none"}
			}
		}
	}
	if len(wireTools) > 0 {
		resp["tools"] = wireTools
	}
	if len(wireAliases) > 0 {
		resp["_wire_aliases"] = wireAliases
	}
	resp["_issuer_kind"] = issuerKind
	if _, has := resp["prompt_cache_key"]; !isXAI && route.endpoint.promptCacheKey && !has {
		scopeID := auxRuntimeMainValue(ctx, "cache_scope")
		if !pyrepr.Truthy(scopeID) {
			scopeID = auxRuntimeMainValue(ctx, "session_id")
		}
		scope := ""
		if pyrepr.Truthy(scopeID) {
			scope = cacheScopeFromSessionID(pyStr(scopeID))
		}
		if key := contentCacheKey(instructions, wireTools, scope); key != "" {
			resp["prompt_cache_key"] = key
		}
	}
	if _, has := resp["prompt_cache_retention"]; !has {
		if retention := defaultPromptCacheRetentionForRequest(model, host); retention != "" {
			resp["prompt_cache_retention"] = retention
		}
	}
	sanitizeAstraRequestKwargs(resp, model, host)
	return resp, model, timeout, nil
}

// Positive numeric seconds of a kwargs value, false for anything else
func auxPositiveSeconds(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case bool:
		if n {
			f = 1
		}
	default:
		return 0, false
	}
	return f, f > 0
}

// Whether a guard callback error is watchdog or cancellation control flow that must end the stream
func auxStreamControlFlow(err error) bool {
	var timeout *AuxTimeoutError
	var interrupted *InterruptedError
	var cancelled *AuxiliaryExplicitCancellation
	return errors.As(err, &timeout) || errors.As(err, &interrupted) || errors.As(err, &cancelled)
}

// Folds a Codex event stream into the final Responses object, logging a failing event hook unless it is control flow
func auxConsumeCodexEventStream(ctx context.Context, stream AuxChatStream, model string, onEvent func(event any) error) (map[string]any, error) {
	assembler := newCodexAssembler(model)
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if onEvent != nil {
			if hookErr := onEvent(event); hookErr != nil {
				if auxStreamControlFlow(hookErr) {
					return nil, hookErr
				}
				slog.Debug("Codex stream on_event hook raised", "error", hookErr)
			}
		}
		done, err := assembler.feed(event)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	return assembler.result()
}

// Streams one Codex Responses request under the progress guard and shapes the result like chat completions
func (a *auxCodexCompletionsAdapter) create(ctx context.Context, kwargs map[string]any) (out any, err error) {
	host := a.client.BaseURL()
	if IsActualRoute(auxClientEffectiveProvider(a.client), host) {
		return nil, &ValueError{Message: "Actual requests require Chat Completions; refusing to call /responses."}
	}
	resp, model, timeout, err := a.buildResponsesKwargs(ctx, kwargs)
	if err != nil {
		return nil, err
	}
	wireAliases, _ := resp["_wire_aliases"].(map[string]string)
	delete(resp, "_wire_aliases")
	issuerKind := pyStr(resp["_issuer_kind"])
	delete(resp, "_issuer_kind")
	issuerModel := model
	if pyrepr.Truthy(resp["model"]) {
		issuerModel = pyStr(resp["model"])
	}
	total, hasTotal := auxPositiveSeconds(timeout)
	noProgress, _ := auxPositiveSeconds(kwargs["no_progress_timeout"])
	guard := newAuxCodexStreamGuard(ctx, a.client, total, hasTotal, noProgress)
	defer guard.finish()
	fail := func(cause error) error {
		var cancelled *AuxiliaryExplicitCancellation
		if errors.As(cause, &cancelled) {
			return cause
		}
		if guard.timedOut.Load() {
			return &AuxTimeoutError{Message: guard.timeoutMessage(), Err: cause}
		}
		slog.Debug(fmt.Sprintf("Codex auxiliary Responses API call failed: %v", cause))
		return cause
	}
	if err := guard.begin(); err != nil {
		return nil, fail(err)
	}
	streamKwargs := make(map[string]any, len(resp)+1)
	for k, v := range resp {
		streamKwargs[k] = v
	}
	streamKwargs["stream"] = true
	eventStream, err := a.client.CreateResponse(ctx, streamKwargs)
	if err != nil {
		return nil, fail(err)
	}
	guard.adoptStream(eventStream)
	if guard.timedOut.Load() && guard.cancelRequested() {
		guard.closeAttemptStream("late cancelled attempt stream close failed", true)
	}
	var final map[string]any
	if completed, ok := eventStream.(map[string]any); ok {
		if _, hasOutput := completed["output"]; hasOutput {
			final = completed
		}
	}
	if final == nil {
		stream, ok := eventStream.(AuxChatStream)
		if !ok {
			guard.releaseStream(eventStream)
			return nil, fail(&AuxRuntimeError{Message: "Codex auxiliary Responses stream did not return a final response"})
		}
		final, err = auxConsumeCodexEventStream(ctx, stream, issuerModel, guard.onEvent)
		guard.releaseStream(eventStream)
		if err != nil {
			return nil, fail(err)
		}
	} else {
		guard.releaseStream(eventStream)
	}
	if final == nil {
		return nil, fail(&AuxRuntimeError{Message: "Codex auxiliary Responses stream did not return a final response"})
	}
	textParts, toolCalls, usage, finishReason, err := auxParseCodexFinalResponse(final, issuerKind, issuerModel)
	if err != nil {
		return nil, fail(err)
	}
	var wireCalls []any
	for _, tc := range toolCalls {
		if original, ok := wireAliases[tc.Name]; ok {
			tc.Name = original
		}
		wireCalls = append(wireCalls, auxCodexToolCallWire(tc))
	}
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": nil}
	if text := pyStripSpace(strings.Join(textParts, "")); text != "" {
		message["content"] = text
	}
	if len(wireCalls) > 0 {
		message["tool_calls"] = wireCalls
	}
	var usageOut any
	if usage != nil {
		usageOut = usage
	}
	return map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"model":   model,
		"usage":   usageOut,
	}, nil
}

// Chat completions tool call of a parsed Responses function call
func auxCodexToolCallWire(tc codexToolCall) map[string]any {
	return map[string]any{
		"id": tc.ID, "call_id": tc.CallID, "response_item_id": tc.ResponseItemID, "type": "function",
		"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
	}
}

// OpenAI client compatible wrapper routing chat completions through the Codex Responses API
type CodexAuxiliaryClient struct {
	realClient auxResponsesClient
	adapter    *auxCodexCompletionsAdapter
}

// Wraps the SDK client so chat completions run as Codex Responses streams
func NewCodexAuxiliaryClient(realClient auxResponsesClient, model string) *CodexAuxiliaryClient {
	return &CodexAuxiliaryClient{realClient: realClient, adapter: &auxCodexCompletionsAdapter{client: realClient, model: model}}
}

func (c *CodexAuxiliaryClient) BaseURL() string { return c.realClient.BaseURL() }
func (c *CodexAuxiliaryClient) APIKey() string  { return c.realClient.APIKey() }

// Runs the request through the Codex adapter
func (c *CodexAuxiliaryClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.adapter.create(ctx, kwargs)
}

// Closes the wrapped SDK client
func (c *CodexAuxiliaryClient) Close() error { return c.realClient.Close() }

func (c *CodexAuxiliaryClient) auxRealClient() AuxClient { return c.realClient }

// Async twin of a wire adapter client running the sync body and mirroring its leaf SDK client for cache eviction
type auxAsyncAuxiliaryClientBase struct {
	sync       AuxClient
	apiKey     string
	baseURL    string
	realClient AuxClient
}

func newAuxAsyncAuxiliaryClientBase(sync AuxClient) auxAsyncAuxiliaryClientBase {
	base := auxAsyncAuxiliaryClientBase{sync: sync, apiKey: sync.APIKey(), baseURL: sync.BaseURL()}
	if holder, ok := sync.(interface{ auxRealClient() AuxClient }); ok {
		base.realClient = holder.auxRealClient()
	}
	return base
}

func (c *auxAsyncAuxiliaryClientBase) BaseURL() string { return c.baseURL }
func (c *auxAsyncAuxiliaryClientBase) APIKey() string  { return c.apiKey }

// Runs the sync adapter body
func (c *auxAsyncAuxiliaryClientBase) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.sync.CreateChatCompletion(ctx, kwargs)
}

// The async wrapper owns no transport so closing it releases nothing
func (c *auxAsyncAuxiliaryClientBase) Close() error { return nil }

func (c *auxAsyncAuxiliaryClientBase) auxRealClient() AuxClient { return c.realClient }

// Async twin of the Codex auxiliary client
type AsyncCodexAuxiliaryClient struct{ auxAsyncAuxiliaryClientBase }

// Wraps a sync Codex client for async callers
func NewAsyncCodexAuxiliaryClient(sync *CodexAuxiliaryClient) *AsyncCodexAuxiliaryClient {
	return &AsyncCodexAuxiliaryClient{newAuxAsyncAuxiliaryClientBase(sync)}
}

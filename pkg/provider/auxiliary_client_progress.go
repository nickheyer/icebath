package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Inner completion of a gateway data envelope, the provider message raised for an error envelope
func auxUnwrapDataEnvelope(response any, task string) (any, error) {
	if pyrepr.Truthy(auxField(response, "choices", nil)) {
		return response, nil
	}
	data, ok := auxField(response, "data", nil).(map[string]any)
	if !ok {
		return response, nil
	}
	if success, isBool := auxField(response, "success", nil).(bool); isBool && !success {
		var errVal any = data
		if pyrepr.Truthy(data["error"]) {
			errVal = data["error"]
		}
		var msg any
		if m, isMap := errVal.(map[string]any); isMap {
			msg = m["message"]
		}
		if !pyrepr.Truthy(msg) {
			msg = errVal
		}
		return nil, &AuxRuntimeError{Message: fmt.Sprintf("Auxiliary %s: provider returned error envelope: %s", auxTaskOrCall(task), pyrepr.Str(msg))}
	}
	if !pyrepr.Truthy(data["choices"]) {
		return response, nil
	}
	if auxChatCompletionValidates(data) {
		return auxCloneMap(data), nil
	}
	rawChoices, _ := data["choices"].([]any)
	var choices []any
	for _, ch := range rawChoices {
		chMap, _ := ch.(map[string]any)
		msg, isMap := chMap["message"].(map[string]any)
		if !isMap {
			continue
		}
		finish := chMap["finish_reason"]
		if !pyrepr.Truthy(finish) {
			finish = "stop"
		}
		choices = append(choices, map[string]any{"message": auxCloneMap(msg), "finish_reason": finish})
	}
	if len(choices) == 0 {
		return response, nil
	}
	return map[string]any{
		"id":      auxMapGetDefault(data, "id", ""),
		"model":   auxMapGetDefault(data, "model", ""),
		"object":  auxMapGetDefault(data, "object", "chat.completion"),
		"choices": choices,
		"usage":   data["usage"],
	}, nil
}

// Task label of a log line, call when the task is unset
func auxTaskOrCall(task string) string {
	if task == "" {
		return "call"
	}
	return task
}

// Value of a present key including null, the default when the key is absent
func auxMapGetDefault(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Shallow copy of a wire map
func auxCloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var auxChatFinishReasons = []string{"stop", "length", "tool_calls", "content_filter", "function_call"}

// Reports whether a map passes the SDK ChatCompletion validation of its required fields
func auxChatCompletionValidates(data map[string]any) bool {
	_, okID := data["id"].(string)
	_, okModel := data["model"].(string)
	if !okID || !okModel || data["object"] != "chat.completion" || !auxPydanticInt(data["created"]) {
		return false
	}
	if usage, present := data["usage"]; present && usage != nil {
		u, isMap := usage.(map[string]any)
		if !isMap || !auxPydanticInt(u["prompt_tokens"]) || !auxPydanticInt(u["completion_tokens"]) || !auxPydanticInt(u["total_tokens"]) {
			return false
		}
	}
	choices, ok := data["choices"].([]any)
	if !ok {
		return false
	}
	for _, ch := range choices {
		c, isMap := ch.(map[string]any)
		if !isMap || !auxPydanticInt(c["index"]) {
			return false
		}
		finish, isStr := c["finish_reason"].(string)
		if !isStr || !slices.Contains(auxChatFinishReasons, finish) {
			return false
		}
		msg, isMsg := c["message"].(map[string]any)
		if !isMsg || msg["role"] != "assistant" {
			return false
		}
		if content, present := msg["content"]; present && content != nil {
			if _, isText := content.(string); !isText {
				return false
			}
		}
		if calls, present := msg["tool_calls"]; present && calls != nil {
			if _, isList := calls.([]any); !isList {
				return false
			}
		}
	}
	return true
}

// Lax pydantic int acceptance of a decoded JSON number, whole floats included and booleans refused
func auxPydanticInt(v any) bool {
	switch x := v.(type) {
	case int, int64:
		return true
	case float64:
		return x == math.Trunc(x) && !math.IsInf(x, 0)
	}
	return false
}

// Response given a chat completions choice from Responses style text, nil when it carries none
func auxRecoverAuxResponseMessage(response any) any {
	text := auxExtractAuxResponseText(response)
	if text == "" {
		return nil
	}
	finish := auxField(response, "finish_reason", nil)
	if !pyrepr.Truthy(finish) {
		finish = "stop"
	}
	choice := map[string]any{"message": map[string]any{"content": text}, "finish_reason": finish}
	if m, ok := response.(map[string]any); ok {
		m["choices"] = []any{choice}
		return m
	}
	return map[string]any{"id": "", "model": "", "object": "chat.completion", "choices": []any{choice}, "usage": nil}
}

// Text of a Responses style output_text or the message output content parts
func auxExtractAuxResponseText(response any) string {
	if outputText, ok := auxField(response, "output_text", nil).(string); ok && pystr.Strip(outputText) != "" {
		return pystr.Strip(outputText)
	}
	output, ok := auxField(response, "output", nil).([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range output {
		itemType := auxField(item, "type", nil)
		if pyrepr.Truthy(itemType) && itemType != "message" {
			continue
		}
		content, _ := auxField(item, "content", nil).([]any)
		for _, part := range content {
			switch auxField(part, "type", nil) {
			case "output_text", "text", nil:
				if text, isStr := auxField(part, "text", nil).(string); isStr && pystr.Strip(text) != "" {
					parts = append(parts, pystr.Strip(text))
				}
			}
		}
	}
	return pystr.Strip(strings.Join(parts, "\n"))
}

// Adapters that stream inside create or cannot stream, none accept a stream request from the caller
func auxClientStreamsInternally(client AuxClient) bool {
	switch client.(type) {
	case *CodexAuxiliaryClient, *AnthropicAuxiliaryClient, *BedrockAuxiliaryClient:
		return true
	}
	return false
}

// Async twin of the internal streaming check over the async adapter types
func auxAsyncClientStreamsInternally(client AuxClient) bool {
	switch client.(type) {
	case *AsyncCodexAuxiliaryClient, *AsyncAnthropicAuxiliaryClient, *AsyncBedrockAuxiliaryClient:
		return true
	}
	return false
}

// Stream only providers, Tencent Copilot, configured stream only base URL markers and the managed llama server
func auxProviderRequiresStream(ctx context.Context, baseURL string) (required bool) {
	url := pystr.Lower(baseURL)
	if url == "" {
		return false
	}
	if baseURLHostMatches(url, "copilot.tencent.com") || auxIsManagedLocalEndpoint(url) {
		return true
	}
	defer func() {
		if recover() != nil {
			required = false
		}
	}()
	tree, err := auxLoadConfigFn(ctx)
	if err != nil {
		return false
	}
	var aux any = map[string]any{}
	if v, ok := tree["auxiliary"]; ok {
		aux = v
	}
	auxMap, ok := aux.(map[string]any)
	if !ok {
		return false
	}
	markers := auxMap["stream_only_base_urls"]
	if !pyrepr.Truthy(markers) {
		return false
	}
	list, ok := markers.([]any)
	if !ok {
		return false
	}
	for _, marker := range list {
		if s, isStr := marker.(string); isStr && pystr.Strip(s) != "" && strings.Contains(url, pystr.Lower(pystr.Strip(s))) {
			return true
		}
	}
	return false
}

var auxAffordableTokensRe = regexp.MustCompile(`(?i)can only afford[\t-\r\x{1c}-\x{20}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]+([0-9][0-9,]*)`)

const (
	auxAffordableRetryFloorTokens  = 512
	auxAffordableRetryMarginTokens = 64
)

// Affordable output budget minus the margin from a credit limited 402, false without a usable count
func auxAffordableMaxTokensFromError(err error) (int, bool) {
	if !auxIsPaymentError(err) {
		return 0, false
	}
	match := auxAffordableTokensRe.FindStringSubmatch(err.Error())
	if match == nil {
		return 0, false
	}
	affordable, perr := strconv.ParseInt(strings.ReplaceAll(match[1], ",", ""), 10, 64)
	if perr != nil {
		affordable = math.MaxInt64
	}
	capped := int(affordable) - auxAffordableRetryMarginTokens
	if capped < auxAffordableRetryFloorTokens {
		return 0, false
	}
	return capped, true
}

// Credit aware create, a 402 naming an affordable budget retries once with that lower cap
func auxCreateWithProgress(ctx context.Context, client AuxClient, kwargs map[string]any, task string, forceStream bool) (any, error) {
	resp, err := auxCreateWithProgressOnce(ctx, client, kwargs, task, forceStream)
	if err == nil || auxIsBaseException(err) {
		return resp, err
	}
	affordable, ok := auxAffordableMaxTokensFromError(err)
	if !ok {
		return nil, err
	}
	existing := kwargs["max_tokens"]
	if !pyrepr.Truthy(existing) {
		existing = kwargs["max_completion_tokens"]
	}
	if cap, isNum := auxPyNumber(existing); isNum && cap > 0 && cap <= float64(affordable) {
		return nil, err
	}
	retry := auxCloneMap(kwargs)
	delete(retry, "max_tokens")
	delete(retry, "max_completion_tokens")
	model := ""
	if pyrepr.Truthy(kwargs["model"]) {
		model = auxPyStr(kwargs["model"])
	}
	for k, v := range AuxiliaryMaxTokensParam(ctx, affordable, model) {
		retry[k] = v
	}
	slog.Info(fmt.Sprintf("Auxiliary %s: credit-limited 402 (affordable=%d tokens); retrying once with a clamped output cap instead of failing: %s", auxTaskOrCall(task), affordable, err.Error()))
	return auxCreateWithProgressOnce(ctx, client, retry, task, forceStream)
}

// Python int or float value as a number, booleans counting as integers
func auxPyNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// Stream kwargs, model name and total ceiling of a streamed re-aggregation
func auxStreamRequestPlan(kwargs map[string]any, client AuxClient) (map[string]any, string, float64) {
	stream := auxCloneMap(kwargs)
	stream["stream"] = true
	stream["stream_options"] = map[string]any{"include_usage": true}
	if client != nil && auxIsManagedLocalEndpoint(client.BaseURL()) {
		stream = WithPromptProgressKwargs(stream)
	}
	model := ""
	if pyrepr.Truthy(kwargs["model"]) {
		model = auxPyStr(kwargs["model"])
	}
	timeout := 0.0
	if v := kwargs["timeout"]; v != nil {
		if f, err := pyint.Float(v); err == nil {
			timeout = f
		}
	}
	return stream, model, auxStreamTotalCeiling(timeout)
}

// Errors a streamed request surfaces unchanged instead of retrying without streaming
func auxStreamFailureIsGenuine(err error, forceStream bool) bool {
	return forceStream || auxIsBaseException(err) || auxIsTransientTransportError(err) || auxIsAuthError(err) || auxIsPaymentError(err) || auxIsRateLimitError(err)
}

// Create that streams and re-aggregates under a progress hook or a stream only provider, a plain create otherwise
func auxCreateWithProgressOnce(ctx context.Context, client AuxClient, kwargs map[string]any, task string, forceStream bool) (any, error) {
	kwargs = BypassChatSDKRequestTransform(kwargs, client)
	auxNotifyAuxDispatch(ctx)
	internal := auxClientStreamsInternally(client)
	if (!auxProgressActive(ctx) && !forceStream) || internal {
		resp, err := client.CreateChatCompletion(ctx, kwargs)
		if err != nil {
			return nil, err
		}
		if !internal {
			auxNotifyAuxProviderResponse(ctx)
		}
		return resp, nil
	}
	streamKwargs, model, ceiling := auxStreamRequestPlan(kwargs, client)
	chunks, err := client.CreateChatCompletion(ctx, streamKwargs)
	if err != nil {
		if auxStreamFailureIsGenuine(err, forceStream) {
			return nil, err
		}
		slog.Debug(fmt.Sprintf("Auxiliary %s: streamed request failed (%s); retrying non-streaming", auxTaskOrCall(task), err.Error()))
		auxNotifyAuxDispatch(ctx)
		resp, err := client.CreateChatCompletion(ctx, kwargs)
		if err != nil {
			return nil, err
		}
		auxNotifyAuxProviderResponse(ctx)
		return resp, nil
	}
	stream, ok := chunks.(AuxChatStream)
	if !ok {
		auxNotifyAuxProviderResponse(ctx)
		return chunks, nil
	}
	windows := auxChatStreamWindows(ctx, client, kwargs, task)
	return auxAggregateChatStream(ctx, stream, model, &ceiling, &windows)
}

// Consumes a chunk stream into a complete response, timing out past the ceiling or the silence windows
func auxAggregateChatStream(ctx context.Context, chunks AuxChatStream, model string, totalCeiling *float64, noProgress *auxStreamWindows) (any, error) {
	acc := newAuxChatStreamAccumulator(ctx, model, totalCeiling)
	err := func() error {
		defer auxCloseChunkStream(chunks)
		return auxConsumeChatStream(ctx, chunks, acc, noProgress)
	}()
	if err != nil {
		return nil, err
	}
	return acc.Finish()
}

// Reasoning detail fields whose non empty text counts as forward progress
var auxReasoningDetailTextFields = []string{"summary", "thinking", "content", "text"}

// Per chunk accumulation shared by the sync and async aggregations
type auxChatStreamAccumulator struct {
	ctx              context.Context
	started          time.Time
	promptReading    *PromptReadingClock
	totalCeiling     *float64
	contentParts     []string
	reasoningParts   []string
	reasoningDetails []any
	toolCalls        map[int]*auxToolCallAcc
	finishReason     any
	usage            any
	respID           any
	respModel        any
}

// Merged fragments of one streamed tool call
type auxToolCallAcc struct {
	id        any
	name      any
	arguments []any
}

func newAuxChatStreamAccumulator(ctx context.Context, model string, totalCeiling *float64) *auxChatStreamAccumulator {
	started := promptReadingNow()
	return &auxChatStreamAccumulator{ctx: ctx, started: started, promptReading: NewPromptReadingClock(started), totalCeiling: totalCeiling,
		toolCalls: map[int]*auxToolCallAcc{}, respID: "", respModel: model}
}

// Timeout past the total ceiling or the host deadline, explicit cancellation on a host hard cancel
func (a *auxChatStreamAccumulator) checkDeadlines() error {
	if auxInterruptCancelRequested(a.ctx) {
		return &AuxiliaryExplicitCancellation{}
	}
	now := promptReadingNow()
	if a.totalCeiling != nil && now.Sub(a.started).Seconds()-a.promptReading.Seconds >= *a.totalCeiling {
		return &AuxTimeoutError{Message: fmt.Sprintf("Auxiliary streamed call timed out after %.0fs total ceiling (stream still open but over budget)", *a.totalCeiling)}
	}
	if deadline, ok := auxCurrentAuxStreamDeadline(a.ctx); ok && !now.Before(deadline) {
		return &AuxTimeoutError{Message: fmt.Sprintf("Auxiliary streamed call timed out at the host compression deadline after %.0fs (the caller already stopped waiting; streaming on would only pin its session lease)", promptReadingNow().Sub(a.started).Seconds())}
	}
	return nil
}

// Collects reasoning details, true only when a detail carries text
func (a *auxChatStreamAccumulator) feedReasoningDetails(delta map[string]any) bool {
	details, ok := delta["reasoning_details"].([]any)
	if !ok {
		return false
	}
	progress := false
	for _, detail := range details {
		a.reasoningDetails = append(a.reasoningDetails, detail)
		if d, isMap := detail.(map[string]any); isMap {
			for _, f := range auxReasoningDetailTextFields {
				if s, isStr := d[f].(string); isStr && s != "" {
					progress = true
				}
			}
		}
	}
	return progress
}

// Merges tool call fragments by index, true when any fragment carried data
func (a *auxChatStreamAccumulator) feedToolCalls(delta map[string]any) bool {
	calls, _ := delta["tool_calls"].([]any)
	progress := false
	for _, raw := range calls {
		idx, _ := auxPyInt(auxField(raw, "index", 0))
		acc := a.toolCalls[idx]
		if acc == nil {
			acc = &auxToolCallAcc{id: "", name: ""}
			a.toolCalls[idx] = acc
		}
		if id := auxField(raw, "id", nil); pyrepr.Truthy(id) {
			acc.id = id
			progress = true
		}
		if fn := auxField(raw, "function", nil); fn != nil {
			if name := auxField(fn, "name", nil); pyrepr.Truthy(name) {
				acc.name = name
				progress = true
			}
			if args := auxField(fn, "arguments", nil); pyrepr.Truthy(args) {
				acc.arguments = append(acc.arguments, args)
				progress = true
			}
		}
	}
	return progress
}

// Accumulates one chunk, true when it carried substantive output
func (a *auxChatStreamAccumulator) Feed(chunk map[string]any) (bool, error) {
	auxNotifyAuxTimingResponse(a.ctx)
	progress := promptProgress(chunk)
	readMore := progress != nil && a.promptReading.Tick(a.ctx, progress)
	if err := a.checkDeadlines(); err != nil {
		return false, err
	}
	if id := chunk["id"]; pyrepr.Truthy(id) {
		a.respID = id
	}
	if model := chunk["model"]; pyrepr.Truthy(model) {
		a.respModel = model
	}
	if usage := chunk["usage"]; pyrepr.Truthy(usage) {
		a.usage = usage
	}
	if progress != nil {
		if readMore {
			auxNotifyAuxProgress(a.ctx)
		}
		return false, nil
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return false, nil
	}
	choice := choices[0]
	if finish := auxField(choice, "finish_reason", nil); pyrepr.Truthy(finish) {
		a.finishReason = finish
	}
	delta, ok := auxField(choice, "delta", nil).(map[string]any)
	if !ok {
		return false, nil
	}
	made := false
	if piece := flattenMessageText(delta["content"], ""); piece != "" {
		a.contentParts = append(a.contentParts, piece)
		made = true
	}
	reasoning := delta["reasoning"]
	if !pyrepr.Truthy(reasoning) {
		reasoning = delta["reasoning_content"]
	}
	if piece := flattenMessageText(reasoning, ""); piece != "" {
		a.reasoningParts = append(a.reasoningParts, piece)
		made = true
	}
	if a.feedReasoningDetails(delta) {
		made = true
	}
	if a.feedToolCalls(delta) {
		made = true
	}
	if made {
		auxNotifyAuxProgress(a.ctx)
	}
	return made, nil
}

// Reports whether a finish reason arrived
func (a *auxChatStreamAccumulator) HasFinishReason() bool { return pyrepr.Truthy(a.finishReason) }

// Complete chat completion response assembled from the accumulated chunks
func (a *auxChatStreamAccumulator) Finish() (map[string]any, error) {
	var toolCalls any
	if len(a.toolCalls) > 0 {
		indexes := make([]int, 0, len(a.toolCalls))
		for idx := range a.toolCalls {
			indexes = append(indexes, idx)
		}
		slices.Sort(indexes)
		list := make([]any, 0, len(indexes))
		for _, idx := range indexes {
			acc := a.toolCalls[idx]
			var args strings.Builder
			for i, part := range acc.arguments {
				s, ok := part.(string)
				if !ok {
					return nil, &auxTypeError{Message: fmt.Sprintf("sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(part))}
				}
				args.WriteString(s)
			}
			list = append(list, map[string]any{"id": acc.id, "type": "function", "function": map[string]any{"name": acc.name, "arguments": args.String()}})
		}
		toolCalls = list
	}
	var reasoning, details any
	if joined := strings.Join(a.reasoningParts, ""); joined != "" {
		reasoning = joined
	}
	if len(a.reasoningDetails) > 0 {
		details = a.reasoningDetails
	}
	finish := a.finishReason
	if !pyrepr.Truthy(finish) {
		finish = "stop"
	}
	message := map[string]any{"role": "assistant", "content": strings.Join(a.contentParts, ""), "tool_calls": toolCalls, "reasoning": reasoning, "reasoning_details": details}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": finish}
	return map[string]any{"id": a.respID, "model": a.respModel, "object": "chat.completion", "choices": []any{choice}, "usage": a.usage}, nil
}

// Async mirror of the aggregation without the silence watchdog
func auxAggregateChatStreamAsync(ctx context.Context, chunks AuxChatStream, model string, totalCeiling *float64) (any, error) {
	acc := newAuxChatStreamAccumulator(ctx, model, totalCeiling)
	err := func() error {
		defer auxCloseChunkStream(chunks)
		return auxConsumeChatStream(ctx, chunks, acc, nil)
	}()
	if err != nil {
		return nil, err
	}
	return acc.Finish()
}

// Async create for stream only providers, streaming and aggregating the chunks
func auxAcreateWithStream(ctx context.Context, client AuxClient, kwargs map[string]any) (any, error) {
	streamKwargs, model, ceiling := auxStreamRequestPlan(kwargs, client)
	chunks, err := client.CreateChatCompletion(ctx, streamKwargs)
	if err != nil {
		return nil, err
	}
	stream, ok := chunks.(AuxChatStream)
	if !ok {
		return chunks, nil
	}
	return auxAggregateChatStreamAsync(ctx, stream, model, &ceiling)
}

// Async create that streams and re-aggregates under a progress hook or a stream only provider
func auxAcreateWithProgress(ctx context.Context, client AuxClient, kwargs map[string]any, task string, forceStream bool) (any, error) {
	kwargs = BypassChatSDKRequestTransform(kwargs, client)
	auxNotifyAuxDispatch(ctx)
	internal := auxAsyncClientStreamsInternally(client)
	if (!auxProgressActive(ctx) && !forceStream) || internal {
		resp, err := client.CreateChatCompletion(ctx, kwargs)
		if err != nil {
			return nil, err
		}
		if !internal {
			auxNotifyAuxProviderResponse(ctx)
		}
		return resp, nil
	}
	streamKwargs, model, ceiling := auxStreamRequestPlan(kwargs, client)
	chunks, err := client.CreateChatCompletion(ctx, streamKwargs)
	if err != nil {
		if auxStreamFailureIsGenuine(err, forceStream) {
			return nil, err
		}
		slog.Debug(fmt.Sprintf("Auxiliary %s: streamed async request failed (%s); retrying non-streaming", auxTaskOrCall(task), err.Error()))
		auxNotifyAuxDispatch(ctx)
		resp, err := client.CreateChatCompletion(ctx, kwargs)
		if err != nil {
			return nil, err
		}
		auxNotifyAuxProviderResponse(ctx)
		return resp, nil
	}
	stream, ok := chunks.(AuxChatStream)
	if !ok {
		auxNotifyAuxProviderResponse(ctx)
		return chunks, nil
	}
	return auxAggregateChatStreamAsync(ctx, stream, model, &ceiling)
}

// Client, final model and the resolved and effective providers of one auxiliary route
type auxResolvedAuxRoute struct {
	Client            AuxClient
	FinalModel        string
	ResolvedProvider  string
	EffectiveProvider string
}

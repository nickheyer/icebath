package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type auxRelayCallKey struct{}

// Relay call record bound for the current auxiliary call, nil outside a call scope
func auxRelayAuxCallContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	record, _ := ctx.Value(auxRelayCallKey{}).(map[string]any)
	return record
}

// Context carrying a fresh relay call record for one auxiliary call of the task
func auxRelayAuxCallScope(ctx context.Context, task string) context.Context {
	if task == "" {
		task = "unknown"
	}
	return context.WithValue(ctx, auxRelayCallKey{}, map[string]any{
		"task":           task,
		"request_id":     "aux-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		"attempt_count":  0,
		"provider":       "",
		"model":          "",
		"response_model": nil,
		"api_mode":       "chat_completions",
	})
}

// Runs one auxiliary call under a shared relay identity, closing the record as failed when it raises
func auxRelayAuxiliaryCall(ctx context.Context, task string, callback func(ctx context.Context) (any, error)) (result any, err error) {
	scoped := auxRelayAuxCallScope(ctx, task)
	defer func() {
		if p := recover(); p != nil {
			perr, ok := p.(error)
			if !ok {
				perr = fmt.Errorf("%v", p)
			}
			auxFailRelayAuxiliaryCall(scoped, perr)
			panic(p)
		}
	}()
	result, err = callback(scoped)
	if err != nil {
		auxFailRelayAuxiliaryCall(scoped, err)
	}
	return result, err
}

// Stamps the concrete route on the bound relay call record
func auxSetRelayAuxiliaryRoute(ctx context.Context, provider, model, apiMode string) {
	record := auxRelayAuxCallContext(ctx)
	if record == nil {
		return
	}
	record["provider"] = auxOrDefault(provider, "auxiliary")
	record["model"] = auxOrDefault(model, "unknown")
	record["response_model"] = nil
	record["api_mode"] = auxOrDefault(apiMode, "chat_completions")
}

// Exposes the concrete route selected for one auxiliary call in the caller supplied map
func auxRecordRouteInfo(routeInfo map[string]string, provider, model string) {
	if routeInfo != nil {
		routeInfo["provider"] = auxOrDefault(provider, "auto")
		routeInfo["model"] = auxOrDefault(model, "default")
	}
}

// Counts one physical attempt and returns its provider, model and relay metadata, false outside a call scope
func auxRelayAuxiliaryMetadata(ctx context.Context, provider, apiMode string) (string, string, map[string]any, bool) {
	record := auxRelayAuxCallContext(ctx)
	if record == nil {
		return "", "", nil, false
	}
	attempt := 0
	if n, ok := record["attempt_count"].(int); ok {
		attempt = n
	}
	record["attempt_count"] = attempt + 1
	providerName := provider
	if providerName == "" {
		providerName = auxRelayField(record, "provider", "auxiliary")
	}
	mode := apiMode
	if mode == "" {
		mode = auxRelayField(record, "api_mode", "chat_completions")
	}
	task := pyrepr.Str(record["task"])
	return providerName, auxRelayField(record, "model", "unknown"), map[string]any{
		"api_mode":       mode,
		"api_request_id": pyrepr.Str(record["request_id"]),
		"call_role":      "auxiliary:" + task,
		"retry_count":    attempt,
		"auxiliary_task": task,
	}, true
}

// String of a truthy record field, the default when the field is falsy
func auxRelayField(record map[string]any, key, fallback string) string {
	if v := record[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return fallback
}

// Value unless empty, otherwise the default
func auxOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// Model the request names, the relay record model when the request names none
func auxRelayModelName(kwargs map[string]any, fallback string) string {
	if v := kwargs["model"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return fallback
}

// Hook arguments for one relayed attempt
func auxRelayHookArgs(client AuxClient, kwargs, metadata map[string]any, provider, model string, streaming bool) auxCallHookArgs {
	return auxCallHookArgs{
		AuxTask: auxRelayField(metadata, "auxiliary_task", ""), Metadata: metadata, Client: client, Kwargs: kwargs,
		Provider: provider, Model: model, APIMode: auxRelayField(metadata, "api_mode", ""), Streaming: streaming,
	}
}

// Sends one non streaming attempt through relay with progress streaming and hard cancel isolation
func auxRelaySyncCompletion(ctx context.Context, client AuxClient, kwargs map[string]any, provider, apiMode string, create func(ctx context.Context, request map[string]any) (any, error)) (any, error) {
	kwargs = auxPrepareChatMessages(ctx, client, kwargs)
	record := auxRelayAuxCallContext(ctx)
	if record == nil {
		record = map[string]any{}
	}
	task, _ := record["task"].(string)
	if provider != "" {
		record["stream_provider"] = provider
	} else {
		record["stream_provider"] = record["provider"]
	}
	callback := create
	if callback == nil {
		callback = func(ctx context.Context, request map[string]any) (any, error) {
			return auxCreateWithProgress(ctx, client, request, task, false)
		}
	}
	providerName, fallbackModel, metadata, ok := auxRelayAuxiliaryMetadata(ctx, provider, apiMode)
	if !ok {
		return auxRunProtectedSyncProviderCall(ctx, callback, kwargs)
	}
	modelName := auxRelayModelName(kwargs, fallbackModel)
	out, err := runWithAuxHooks(ctx, func() (any, error) {
		return relayExecuteCurrent(ctx, kwargs, func(ctx context.Context, request map[string]any) (any, error) {
			return auxRunProtectedSyncProviderCall(ctx, callback, request)
		}, providerName, modelName, auxRelayChatMetadata(metadata), true)
	}, auxRelayHookArgs(client, kwargs, metadata, providerName, modelName, false))
	if err != nil {
		auxNoteRelayAuxiliaryError(ctx, err)
	}
	return out, err
}

// Async twin of the relayed completion, awaiting the provider callback without thread isolation
func auxRelayAsyncCompletion(ctx context.Context, client AuxClient, kwargs map[string]any, provider, apiMode string, create func(ctx context.Context, request map[string]any) (any, error)) (any, error) {
	kwargs = auxPrepareChatMessages(ctx, client, kwargs)
	callback := create
	if callback == nil {
		callback = func(ctx context.Context, request map[string]any) (any, error) {
			return auxAcreateWithProgress(ctx, client, request, "", false)
		}
	}
	providerName, fallbackModel, metadata, ok := auxRelayAuxiliaryMetadata(ctx, provider, apiMode)
	if !ok {
		return callback(ctx, kwargs)
	}
	modelName := auxRelayModelName(kwargs, fallbackModel)
	out, err := runWithAuxHooks(ctx, func() (any, error) {
		return relayExecuteCurrent(ctx, kwargs, callback, providerName, modelName, auxRelayChatMetadata(metadata), true)
	}, auxRelayHookArgs(client, kwargs, metadata, providerName, modelName, false))
	if err != nil {
		auxNoteRelayAuxiliaryError(ctx, err)
	}
	return out, err
}

// Streams one attempt through relay, a completed response from a stream ignoring client unwrapped as is
func auxRelaySyncStream(ctx context.Context, client AuxClient, kwargs map[string]any, provider, apiMode string) (any, error) {
	kwargs = auxPrepareChatMessages(ctx, client, kwargs)
	create := func(ctx context.Context, request map[string]any) (any, error) {
		return client.CreateChatCompletion(ctx, BypassChatSDKRequestTransform(request, client))
	}
	providerName, fallbackModel, metadata, ok := auxRelayAuxiliaryMetadata(ctx, provider, apiMode)
	if !ok {
		return create(ctx, kwargs)
	}
	modelName := auxRelayModelName(kwargs, fallbackModel)
	return runWithAuxHooks(ctx, func() (any, error) {
		return relayStreamCurrent(ctx, kwargs, create, providerName, modelName, func() any { return map[string]any{} },
			auxRelayChatMetadata(metadata), false, auxIsCompletedResponse)
	}, auxRelayHookArgs(client, kwargs, metadata, providerName, modelName, true))
}

// True for a whole chat completion response rather than a chunk stream
func auxIsCompletedResponse(value any) bool {
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, has := m["choices"]
	return has
}

// Closes the bound logical call with its outcome and the last recovered error class
func auxCompleteRelayAuxiliaryCall(ctx context.Context, outcome, errorClass string) error {
	record := auxRelayAuxCallContext(ctx)
	if record == nil {
		return nil
	}
	if outcome == "" {
		outcome = "success"
	}
	if errorClass == "" {
		errorClass = auxRelayField(record, "error_class", "none")
	}
	responseModel := ""
	if v := record["response_model"]; v != nil {
		responseModel = pyrepr.Str(v)
	}
	return relayCompleteLogicalCall(ctx, auxRelayField(record, "request_id", ""), outcome,
		auxRelayField(record, "model", "unknown"), auxRelayField(record, "provider", "auxiliary"), responseModel, errorClass)
}

// Remembers one failed attempt's classified reason on the bound logical call
func auxNoteRelayAuxiliaryError(ctx context.Context, err error) {
	record := auxRelayAuxCallContext(ctx)
	if record != nil && !auxIsBaseException(err) {
		record["error_class"] = auxErrorClass(err, auxRelayField(record, "provider", ""), auxRelayField(record, "model", ""))
	}
}

// Closes a call that ended in the error as cancelled for an abort, failed with its reason otherwise
func auxFailRelayAuxiliaryCall(ctx context.Context, err error) {
	var ferr error
	if auxIsCancellation(err) {
		ferr = auxCompleteRelayAuxiliaryCall(ctx, "cancelled", "none")
	} else {
		auxNoteRelayAuxiliaryError(ctx, err)
		ferr = auxCompleteRelayAuxiliaryCall(ctx, "failed", "")
	}
	if ferr != nil {
		slog.Warn("Relay auxiliary failure finalization failed", "error", ferr)
	}
}

package gatewayapi

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Configured route of a model alias with its credentials, nil when the alias is unknown
func (a *APIServerAdapter) ConfiguredModelRoute(alias any) map[string]any {
	route := a.resolveRoute(alias)
	if route == nil {
		return nil
	}
	out := make(map[string]any, len(route))
	for key, value := range route {
		out[key] = value
	}
	return out
}

// Binds a streaming run id to the admission whose shared approval prompt it answers
func (a *APIServerAdapter) BindRunAdmissionAlias(key, admissionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runAdmissionAliases[key] = admissionID
}

// Drops a streaming run id's binding while it still names this admission
func (a *APIServerAdapter) ReleaseRunAdmissionAlias(key, admissionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runAdmissionAliases[key] == admissionID {
		delete(a.runAdmissionAliases, key)
	}
}

// Optional text argument, None for the empty string
func optionalText(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// Text of a positional callback argument, empty when absent or None
func callbackText(args []any, index int) string {
	if index >= len(args) || args[index] == nil {
		return ""
	}
	if text, ok := args[index].(string); ok {
		return text
	}
	return pyrepr.Str(args[index])
}

// Plain mapping of a positional callback argument, nil when absent or None
func callbackMap(args []any, index int) map[string]any {
	if index >= len(args) || args[index] == nil {
		return nil
	}
	switch value := args[index].(type) {
	case *pyjson.Object:
		plain, _ := pyjson.Plain(value).(map[string]any)
		return plain
	case map[string]any:
		return value
	}
	return nil
}

// Callback a request owned observer registers, a panic raised by the sink returned as its error
func observerCallback(callback any) gateway.APIObserverCallback {
	var invoke func(args []any, kwargs map[string]any)
	switch fn := callback.(type) {
	case nil:
		return nil
	case func(string):
		invoke = func(args []any, _ map[string]any) { fn(callbackText(args, 0)) }
	case func(string, any):
		invoke = func(args []any, _ map[string]any) {
			var message any
			if len(args) > 1 {
				message = args[1]
			}
			fn(callbackText(args, 0), message)
		}
	case func(string, bool):
		invoke = func(args []any, kwargs map[string]any) {
			fn(callbackText(args, 0), pyrepr.Truthy(kwargs["already_streamed"]))
		}
	case func(string, string, map[string]any):
		invoke = func(args []any, _ map[string]any) {
			fn(callbackText(args, 0), callbackText(args, 1), callbackMap(args, 2))
		}
	case func(string, string, map[string]any, string):
		invoke = func(args []any, _ map[string]any) {
			fn(callbackText(args, 0), callbackText(args, 1), callbackMap(args, 2), callbackText(args, 3))
		}
	case func(string, string, string, map[string]any, map[string]any):
		invoke = func(args []any, kwargs map[string]any) {
			fn(callbackText(args, 0), callbackText(args, 1), callbackText(args, 2), callbackMap(args, 3), kwargs)
		}
	default:
		panic(fmt.Sprintf("api server callback of unsupported type %T", callback))
	}
	return func(args []any, kwargs map[string]any) (err error) {
		defer func() {
			if r := recover(); r != nil {
				if raised, ok := r.(error); ok {
					err = raised
					return
				}
				err = fmt.Errorf("%v", r)
			}
		}()
		invoke(args, kwargs)
		return nil
	}
}

// Observer callbacks of a turn keyed by the agent callback name each one serves
func observerCallbacks(pairs ...any) map[string]gateway.APIObserverCallback {
	out := map[string]gateway.APIObserverCallback{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if callback := observerCallback(pairs[i+1]); callback != nil {
			out[pairs[i].(string)] = callback
		}
	}
	return out
}

// Keyword arguments the run agent door hands one canonical API turn
func apiTurnKwargs(o runAgentOptions) *gateway.APITurn {
	routeSource := o.routeSource
	if routeSource == "" {
		routeSource = "global"
	}
	var route any
	if o.route != nil {
		plain := make(map[string]any, len(o.route))
		for key, value := range o.route {
			plain[key] = value
		}
		route = plain
	}
	var requestedRuntime, turnAuthor, relayMetadata any
	if o.requestedRuntime != nil {
		requestedRuntime = o.requestedRuntime
	}
	if o.turnAuthor != nil {
		turnAuthor = o.turnAuthor
	}
	if o.relayMetadata != nil {
		relayMetadata = o.relayMetadata
	}
	history := make([]any, len(o.conversationHistory))
	for i, message := range o.conversationHistory {
		history[i] = message
	}
	turn := &gateway.APITurn{UserMessage: o.userMessage, ConversationHistory: history, SessionID: o.sessionID,
		GatewaySessionKey: o.gatewaySessionKey, BindDeclaredConversation: o.bindDeclaredConversaton,
		Settings: map[string]any{"ephemeral_system_prompt": optionalText(o.ephemeralSystemPrompt),
			"requested_model": optionalText(o.requestedModel), "requested_provider": optionalText(o.requestedProvider),
			"model_options": o.modelOptions, "route": route, "session_model": optionalText(o.sessionModel),
			"confirmed_runtime_lock": o.confirmedRuntimeLock, "requested_runtime": requestedRuntime, "route_source": routeSource,
			"session_history_delivery": o.sessionHistoryDelivery},
		HistoryFromSession: o.historyFromSession, RunOwnerScope: o.runOwnerScope, TurnAuthor: turnAuthor,
		ResumeUnansweredTurn: o.resumeUnansweredTurn, RelayMetadata: relayMetadata, RequestID: o.requestID,
		ActiveRunID: o.activeRunID, WalkedFrom: o.walkedFrom, ApprovalSessionKey: o.approvalSessionKey,
		Observers: observerCallbacks("stream_delta_callback", o.streamDeltaCallback, "tool_progress_callback", o.toolProgressCallback,
			"tool_start_callback", o.toolStartCallback, "tool_complete_callback", o.toolCompleteCallback,
			"interim_assistant_callback", o.interimAssistantCb, "reasoning_callback", o.reasoningCallback,
			"status_callback", o.statusCallback)}
	if o.approvalNotifyCallback != nil {
		turn.ApprovalNotifyCallback = o.approvalNotifyCallback
	}
	return turn
}

// Runs one canonical API turn through the routed authority's FIFO
func (a *APIServerAdapter) runAPITurn(ctx context.Context, o runAgentOptions) (map[string]any, map[string]any, error) {
	return gateway.RunAPITurn(ctx, a, apiTurnKwargs(o))
}

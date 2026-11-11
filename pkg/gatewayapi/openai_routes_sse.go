package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Durable admission identity of a chat completions Idempotency-Key in the authenticated namespace
func (a *APIServerAdapter) chatRequestID(ctx context.Context, request *gateway.WebRequest) string {
	key := request.Header.Get("Idempotency-Key")
	if key == "" {
		return ""
	}
	return "chat:" + a.runIdempotencyScope(ctx, request) + ":" + key
}

// Admission identity of a chat completions turn under session authority, nothing before it
func (a *APIServerAdapter) applyAuthorityChatKwargs(run *runAgentOptions, request *gateway.WebRequest, providedSessionID string) {
	if _, ok := a.sessionAuthority(); !ok {
		return
	}
	ctx := context.Background()
	run.requestID = a.chatRequestID(ctx, request)
	run.historyFromSession = providedSessionID != ""
	run.runOwnerScope = a.runIdempotencyScope(ctx, request)
}

// Session a derived id names, a deleted one replaced by a new generation from the owning store
func (a *APIServerAdapter) implicitChatSession(ctx context.Context, request *gateway.WebRequest, sessionID string) (string, *gateway.WebResponse, error) {
	if _, ok := a.sessionAuthority(); !ok {
		return sessionID, nil, nil
	}
	authority, err := gateway.ActiveAuthority(ctx, a.GatewayRunner)
	if err != nil {
		return sessionID, nil, err
	}
	if authority == nil {
		return sessionID, nil, nil
	}
	resolved, err := gateway.ImplicitAPISession(ctx, authority.DB, sessionID, a.chatRequestID(ctx, request))
	if err != nil {
		if isStoreError(err) {
			return sessionID, storeErrorResponse(err), nil
		}
		apiLog().Error(fmt.Sprintf("Error resolving the chat completions session: %v", err))
		return sessionID, errorResponse(fmt.Sprintf("Internal server error: %v", err), http.StatusInternalServerError, errOpts{Type: "server_error"}), nil
	}
	return resolved, nil, nil
}

// Runtime store, SQLite and OS errors a store error response answers
func isStoreError(err error) bool {
	var runtimeErr *state.RuntimeStoreError
	if errors.As(err, &runtimeErr) {
		return true
	}
	var typed interface{ PyTypeName() string }
	if errors.As(err, &typed) {
		switch typed.PyTypeName() {
		case "OperationalError", "DatabaseError", "IntegrityError", "OSError", "sqlite3.Error":
			return true
		}
	}
	return false
}

// Runs compute once per principal scope, route and Idempotency-Key plus body fingerprint
func (a *APIServerAdapter) runIdempotent(ctx context.Context, request *gateway.WebRequest, body *pyjson.Object, compute func() ([2]map[string]any, error), logLabel string, fingerprintKeys []string, route string) ([2]map[string]any, *gateway.WebResponse) {
	key := request.Header.Get("Idempotency-Key")
	var outcome [2]map[string]any
	var err error
	if _, hasAuthority := a.sessionAuthority(); key != "" && !hasAuthority {
		scoped := a.runIdempotencyScope(ctx, request) + "\x00" + route + "\x00" + key
		fp := makeRequestFingerprint(body, fingerprintKeys)
		var value any
		value, err = idemCache.getOrSet(scoped, fp, func() (any, error) { return compute() })
		if err == nil {
			outcome = value.([2]map[string]any)
		}
	} else {
		outcome, err = compute()
	}
	if err == nil {
		return outcome, nil
	}
	var runtimeErr *state.RuntimeStoreError
	if errors.As(err, &runtimeErr) {
		return outcome, storeErrorResponse(err)
	}
	apiLog().Error(fmt.Sprintf("Error running agent for %s: %v", logLabel, err))
	message := fmt.Sprintf("Internal server error: %v", err)
	var suppressed *presentationSuppressedError
	if errors.As(err, &suppressed) {
		message = ""
	}
	return outcome, errorResponse(message, http.StatusInternalServerError, errOpts{Type: "server_error"})
}

// Prepared SSE stream response with CORS and session headers resolved up front
func (a *APIServerAdapter) prepareSSEResponse(request *gateway.WebRequest, sessionID, gatewaySessionKey string) (*gateway.WebStreamResponse, error) {
	headers := a.sseHeaders(request, nil)
	if sessionID != "" {
		headers["X-Hermes-Session-Id"] = sessionID
	}
	if gatewaySessionKey != "" {
		headers["X-Hermes-Session-Key"] = gatewaySessionKey
	}
	response := gateway.NewWebStreamResponse(http.StatusOK, headers)
	if err := response.Prepare(request); err != nil {
		return nil, err
	}
	return response, nil
}

// Streams chat completion chunks from the agent queue, interrupting the agent when the client leaves
func (a *APIServerAdapter) writeSSEChatCompletion(ctx context.Context, request *gateway.WebRequest, completionID string, model any, created int64, q *streamQueue, task *agentTask, ref **agent.AIAgent, outcome *streamAgentOutcome, sessionID, gatewaySessionKey string) (*gateway.WebResponse, error) {
	response, err := a.prepareSSEResponse(request, sessionID, gatewaySessionKey)
	if err != nil {
		return nil, err
	}
	chunk := func(delta *pyjson.Object, finish any, extra ...any) *pyjson.Object {
		out := pyjson.ObjectOf("id", completionID, "object", "chat.completion.chunk", "created", created, "model", model,
			"choices", []any{pyjson.ObjectOf("index", int64(0), "delta", delta, "finish_reason", finish)})
		for i := 0; i+1 < len(extra); i += 2 {
			out.Set(extra[i].(string), extra[i+1])
		}
		return out
	}
	write := func(data any, event string) error {
		frame, err := gateway.APISSEFrame(data, event, true, nil)
		if err != nil {
			return err
		}
		return response.Write(frame)
	}
	contentSent := false
	streamErr := func() error {
		if err := write(chunk(pyjson.ObjectOf("role", "assistant"), nil), ""); err != nil {
			return err
		}
		err := iterStreamItems(ctx, q, task, response, func(item any, eos bool) error {
			if eos {
				return nil
			}
			switch v := item.(type) {
			case streamTagged:
				switch v.tag {
				case "__tool_progress__":
					if !a.toolProgressEvents {
						return nil
					}
					return write(v.payload, "hermes.tool.progress")
				case "__reasoning__":
					return write(chunk(pyjson.ObjectOf("reasoning_content", v.payload.GetOr("text", "")), nil), "")
				case "__status__":
					return write(v.payload, "hermes.status")
				case "__approval__":
					return write(v.payload, "approval.request")
				}
			case string:
				if v != "" {
					contentSent = true
				}
				return write(chunk(pyjson.ObjectOf("content", v), nil), "")
			}
			return nil
		})
		if err != nil {
			return err
		}
		task.Wait()
		usage := map[string]any{"input_tokens": int64(0), "output_tokens": int64(0), "total_tokens": int64(0)}
		result, agentErr := outcome.result, outcome.err
		if agentErr != nil {
			apiLog().Error(fmt.Sprintf("Agent task %s failed during SSE streaming: %v", completionID, agentErr))
		} else if len(outcome.usage) > 0 {
			usage = outcome.usage
		}
		completed, isPartial, isFailed, errMsg := resultFlags(result)
		if agentErr != nil {
			isFailed = true
			if !pyrepr.Truthy(errMsg) {
				errMsg = agentErr.Error()
			}
		}
		exhausted := budgetExhausted(result)
		finish := finishReason(completed, isPartial, isFailed, errMsg, agentErr, exhausted)
		finishChunk := chunk(pyjson.NewObject(), finish, "usage", chatUsagePayload(usage))
		var suppressed *presentationSuppressedError
		muted := (result != nil && result["_notification_presentation_suppressed"] == true) || errors.As(agentErr, &suppressed)
		if !contentSent && !muted && result != nil {
			final, _ := result["final_response"].(string)
			if fallback := gateway.APIResolveMediaToDataURLs(ctx, final); fallback != "" {
				if err := write(chunk(pyjson.ObjectOf("content", fallback), nil), ""); err != nil {
					return err
				}
			}
		} else if !muted {
			if tail, appended := postStreamTransform(result); tail != "" {
				text := tail
				if !appended {
					text = transformedNotice(ctx) + tail
				}
				if err := write(chunk(pyjson.ObjectOf("content", text), nil), ""); err != nil {
					return err
				}
			}
		}
		if finish != "stop" {
			if pyrepr.Truthy(errMsg) && !muted {
				errType := "agent_error"
				if agentErr != nil {
					errType = pyTypeName(agentErr)
				}
				finishChunk.Set("error", pyjson.ObjectOf("message", errMsg, "type", errType))
			}
			var extrasErr any = errMsg
			if muted {
				extrasErr = ""
			}
			finishChunk.Set("hermes", hermesExtras(completed, isPartial, isFailed, extrasErr, finish, exhausted))
		}
		if err := write(finishChunk, ""); err != nil {
			return err
		}
		return response.Write([]byte("data: [DONE]\n\n"))
	}()
	if streamErr != nil {
		if isClientGone(streamErr) {
			abandonAgentTask(ctx, *ref, task, "SSE client disconnected", "api_server_sse_disconnect", true)
			apiLog().Info(fmt.Sprintf("SSE client disconnected; interrupted agent task %s", completionID))
		} else {
			apiLog().Error(fmt.Sprintf("Agent crashed mid-stream for %s: %.300s", completionID, streamErr.Error()))
			if err := write(chunk(pyjson.NewObject(), "error"), ""); err == nil {
				_ = response.Write([]byte("data: [DONE]\n\n"))
			}
		}
	}
	return response.WebResponse, nil
}

// Writes the responses SSE stream, persisting an incomplete snapshot when the client leaves
func (a *APIServerAdapter) writeSSEResponses(ctx context.Context, request *gateway.WebRequest, responseID string, model any, createdAt int64, q *streamQueue, task *agentTask, ref **agent.AIAgent, outcome *streamAgentOutcome,
	history []any, userMessage, instructions any, conversation string, store bool, sessionID, gatewaySessionKey, durableKey, durableFingerprint string, terminalReplay bool) (*gateway.WebResponse, error) {
	response, err := a.prepareSSEResponse(request, sessionID, gatewaySessionKey)
	if err != nil {
		return nil, err
	}
	st, err := newResponsesStream(ctx, a, response, responseID, model, createdAt, history, userMessage, instructions, conversation, store,
		sessionID, durableKey, durableFingerprint, terminalReplay)
	if err != nil {
		return nil, err
	}
	streamErr := func() error {
		if err := func() error { st.mu.Lock(); defer st.mu.Unlock(); return st.emitCreated() }(); err != nil {
			return err
		}
		err := iterStreamItems(ctx, q, task, response, func(item any, eos bool) error {
			if eos {
				st.cancelBatchTimer()
				return st.flushBatch()
			}
			return st.dispatch(item)
		})
		if err != nil {
			return err
		}
		if err := st.flushBatch(); err != nil {
			return err
		}
		task.Wait()
		if err := st.collectResult(outcome.result, outcome.usage, outcome.err); err != nil {
			return err
		}
		if err := st.closeMessageItem(); err != nil {
			return err
		}
		if st.agentError != "" && !(durableKey != "" && st.result != nil) {
			return st.emitFailed()
		}
		_, err = st.emitCompleted()
		return err
	}()
	if streamErr != nil {
		switch {
		case isClientGone(streamErr):
			if err := func() error { st.mu.Lock(); defer st.mu.Unlock(); return st.persistIncompleteIfNeeded() }(); err != nil {
				return nil, err
			}
			abandonAgentTask(ctx, *ref, task, "SSE client disconnected", "api_server_sse_disconnect", true)
			apiLog().Info(fmt.Sprintf("SSE client disconnected; interrupted agent task %s", responseID))
		case errors.Is(streamErr, context.Canceled):
			abandonAgentTask(context.WithoutCancel(ctx), *ref, task, "SSE task cancelled", "api_server_sse_cancelled", false)
			if err := func() error { st.mu.Lock(); defer st.mu.Unlock(); return st.persistIncompleteIfNeeded() }(); err != nil {
				return nil, err
			}
			apiLog().Info(fmt.Sprintf("SSE task cancelled; persisted incomplete snapshot for %s", responseID))
			return nil, streamErr
		default:
			func() { st.mu.Lock(); defer st.mu.Unlock(); _ = st.persistIncompleteIfNeeded() }()
			st.mu.Lock()
			st.agentError = redactAPIErrorText(streamErr.Error()+"\n"+string(debug.Stack()), -1)
			st.mu.Unlock()
			_ = st.emitCrash(streamErr)
			apiLog().Error(fmt.Sprintf("Agent crashed mid-stream for %s: %.300s", responseID, st.agentError))
		}
	}
	return response.WebResponse, nil
}

// Re-emits a settled idempotent record as SSE, its exact frames or created plus its terminal event
func (a *APIServerAdapter) replaySSEResponses(request *gateway.WebRequest, replay *pyjson.Object) (*gateway.WebResponse, error) {
	headers, _ := replay.GetOr("headers", nil).(*pyjson.Object)
	if headers == nil {
		headers = pyjson.NewObject()
	}
	response, err := a.prepareSSEResponse(request, textOf(headers.GetOr("X-Hermes-Session-Id", nil)), textOf(headers.GetOr("X-Hermes-Session-Key", nil)))
	if err != nil {
		return nil, err
	}
	events, _ := replay.GetOr("events", nil).([]any)
	if len(events) == 0 {
		env, _ := replay.GetOr("response", nil).(*pyjson.Object)
		created := pyjson.NewObject()
		for _, k := range []string{"id", "object", "created_at", "model"} {
			v, _ := env.Get(k)
			created.Set(k, v)
		}
		created.Set("status", "in_progress")
		created.Set("output", []any{})
		status := pyrepr.Str(env.GetOr("status", ""))
		events = []any{
			[]any{"response.created", pyjson.ObjectOf("type", "response.created", "response", created, "sequence_number", int64(0))},
			[]any{"response." + status, pyjson.ObjectOf("type", "response."+status, "response", env, "sequence_number", int64(1))},
		}
	}
	for _, raw := range events {
		pair, _ := raw.([]any)
		if len(pair) != 2 {
			continue
		}
		frame, err := gateway.APISSEFrame(pair[1], pyrepr.Str(pair[0]), true, nil)
		if err != nil {
			return nil, err
		}
		if err := response.Write(frame); err != nil {
			return nil, err
		}
	}
	return response.WebResponse, nil
}

// Canonical Idempotency-Key replay of a responses request, the durable key and a replayed answer when settled
func (a *APIServerAdapter) responsesDurableReplay(ctx context.Context, request *gateway.WebRequest, body *pyjson.Object, stream bool) (string, string, string, string, *gateway.WebResponse, error) {
	idempotencyKey := request.Header.Get("Idempotency-Key")
	_, canonical := a.sessionAuthority()
	if !canonical || idempotencyKey == "" {
		return "", "", "", idempotencyKey, nil, nil
	}
	scope := a.runIdempotencyScope(ctx, request)
	durableKey := "idem:" + scope + ":" + idempotencyKey
	fingerprint := makeRequestFingerprint(body, responsesFingerprintKeysFor(body))
	store, err := a.currentResponseStore(ctx)
	if err != nil {
		return "", "", "", "", nil, err
	}
	binding := requestBinding(ctx, a, request, durableKey, fingerprint)
	var matches bool
	var replay *pyjson.Object
	if _, err := store.Atomically(ctx, func(ctx context.Context) (any, error) {
		var err error
		matches, replay, err = matchingReplay(ctx, store, durableKey, binding)
		return nil, err
	}); err != nil {
		return "", "", "", "", nil, err
	}
	conflict := func(code string) *gateway.WebResponse {
		return jsonResponse(openAIError(code, "", nil, code), http.StatusConflict, nil)
	}
	if !matches {
		return durableKey, fingerprint, scope, idempotencyKey, conflict("admission_conflict"), nil
	}
	if replay != nil {
		if fp, _ := replay.Get("fingerprint"); fp != fingerprint {
			return durableKey, fingerprint, scope, idempotencyKey, conflict("admission_conflict"), nil
		}
		if !settled(replay) {
			return durableKey, fingerprint, scope, idempotencyKey, conflict("unknown_execution"), nil
		}
		if stream {
			resp, err := a.replaySSEResponses(request, replay)
			return durableKey, fingerprint, scope, idempotencyKey, resp, err
		}
		headers := map[string]string{}
		if h, ok := replay.GetOr("headers", nil).(*pyjson.Object); ok {
			for _, k := range h.Keys {
				v, _ := h.Get(k)
				headers[k] = pyrepr.Str(v)
			}
		}
		return durableKey, fingerprint, scope, idempotencyKey, jsonResponse(replay.GetOr("response", nil), http.StatusOK, headers), nil
	}
	return durableKey, fingerprint, scope, idempotencyKey, nil, nil
}

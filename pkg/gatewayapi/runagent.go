package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// 429 once the concurrent run cap is reached, the current request's own reservation not counted
func (a *APIServerAdapter) concurrencyLimitedResponse(ctx context.Context) *gateway.WebResponse {
	limit := a.maxConcurrentRuns
	if limit <= 0 {
		return nil
	}
	inflight := a.ActiveAgentWorkCount()
	if reservation := requestReservation(ctx); reservation != nil {
		a.mu.Lock()
		active := reservation.active
		a.mu.Unlock()
		if active {
			inflight--
		}
	}
	if inflight >= limit {
		return errorResponse(fmt.Sprintf("Too many concurrent runs (max %d)", limit), http.StatusTooManyRequests,
			errOpts{Type: "rate_limit_error", Code: "rate_limit_exceeded", Headers: map[string]string{"Retry-After": "1"}})
	}
	return nil
}

// Binds an API turn's session variables with push disabled and history delivery default denied
func bindAPIServerSession(env *tools.Env, chatID, sessionKey, sessionID, profile, sessionHistoryDelivery string) {
	cronSession := ""
	delivery := sessionHistoryDelivery
	tools.SetSessionVars(env, tools.SessionVars{Platform: "api_server", ChatID: chatID, SessionKey: sessionKey,
		SessionID: sessionID, Profile: profile, NoAsyncDelivery: true, CronSession: &cronSession, SessionHistoryDelivery: &delivery})
}

// Sanitized actual and requested runtime of a finished turn, an error when a confirmed lock ran elsewhere
func (a *APIServerAdapter) turnRuntimeMetadata(ag *agent.AIAgent, route map[string]string, requestedRuntime *pyjson.Object, routeSource string, confirmedRuntimeLock bool) (*pyjson.Object, error) {
	runtime := pyjson.NewObject()
	if info, ok := apiRuntimeOf(ag); ok {
		runtime = pyjson.ObjectOf("provider", info.provider, "model", info.model, "route_source", info.routeSource)
	}
	actualProvider := cleanRuntimeID(ag.Provider(), 80)
	actualModel := cleanRuntimeID(ag.Model(), 200)
	resolvedProvider := cleanRuntimeID(runtime.GetOr("provider", nil), 80)
	for _, pair := range [][2]string{{"provider", actualProvider}, {"model", actualModel}} {
		if pair[1] != "" {
			runtime.Set(pair[0], pair[1])
		} else {
			setDefault(runtime, pair[0], "")
		}
	}
	if route == nil {
		route = map[string]string{}
	}
	if requestedRuntime == nil {
		requestedRuntime = pyjson.NewObject()
	}
	if confirmedRuntimeLock {
		reqProvider := route["provider"]
		if reqProvider == "" {
			reqProvider = fmt.Sprint(nonNil(requestedRuntime.GetOr("provider", nil)))
		}
		requestedProvider := cleanRuntimeID(reqProvider, 80)
		expectedProvider := resolvedProvider
		if expectedProvider == "" {
			expectedProvider = requestedProvider
		}
		expectedProvider = cleanRuntimeID(expectedProvider, 80)
		reqModel := route["model"]
		if reqModel == "" {
			reqModel = fmt.Sprint(nonNil(requestedRuntime.GetOr("model", nil)))
		}
		expectedModel := cleanRuntimeID(reqModel, 200)
		if (expectedProvider != "" && actualProvider != expectedProvider) || (expectedModel != "" && actualModel != expectedModel) {
			label := func(v, def string) string {
				if v == "" {
					return def
				}
				return v
			}
			expectLabel := requestedProvider
			if expectLabel == "" {
				expectLabel = expectedProvider
			}
			return nil, &runtimeCauseError{msg: fmt.Sprintf("confirmed model lock runtime mismatch: expected provider=%s model=%s; actual provider=%s model=%s",
				label(expectLabel, "<unspecified>"), label(expectedModel, "<unspecified>"),
				label(actualProvider, "<unknown>"), label(actualModel, "<unknown>"))}
		}
	}
	if requestedRuntime.Len() > 0 {
		m, p := requestedIDs(requestedRuntime)
		runtime.Set("requested", pyjson.ObjectOf("provider", p, "model", m))
	}
	source := routeSource
	if source == "" {
		source = fmt.Sprint(nonNil(runtime.GetOr("route_source", nil)))
	}
	if source == "" {
		source = "global"
	}
	runtime.Set("route_source", source)
	var requested *pyjson.Object
	if requestedRuntime.Len() > 0 {
		requested = requestedRuntime
	}
	lock := ""
	if confirmedRuntimeLock {
		lock = "confirmed"
	}
	if routeSource == "" {
		routeSource = "global"
	}
	return sanitizeRuntimeMetadata(runtime, requested, routeSource, lock), nil
}

// Empty string for nil, the value otherwise
func nonNil(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Usage, effective session id, the compressed flag and runtime metadata attached to a finished turn
func (a *APIServerAdapter) finishTurnResult(ag *agent.AIAgent, result map[string]any, sessionID string, route map[string]string, requestedRuntime *pyjson.Object, routeSource string, confirmedRuntimeLock bool) (map[string]any, map[string]any, error) {
	usage := map[string]any{"input_tokens": int64(ag.SessionPromptTokens()), "output_tokens": int64(ag.SessionCompletionTokens()),
		"total_tokens": int64(ag.SessionTotalTokens())}
	effective := ag.SessionID()
	if effective != "" {
		result["session_id"] = effective
	}
	rotated := effective != "" && sessionID != "" && effective != sessionID
	if ag.LastCompactionInPlace() || rotated {
		result["_compressed"] = true
	}
	if (requestedRuntime != nil && requestedRuntime.Len() > 0) || route != nil || confirmedRuntimeLock || (routeSource != "" && routeSource != "global") {
		runtime, err := a.turnRuntimeMetadata(ag, route, requestedRuntime, routeSource, confirmedRuntimeLock)
		if err != nil {
			return nil, nil, err
		}
		result["runtime"] = runtime
		usage["runtime"] = runtime
	}
	return result, usage, nil
}

// Keyword arguments of one API agent turn
type runAgentOptions struct {
	userMessage             any
	conversationHistory     []map[string]any
	ephemeralSystemPrompt   string
	sessionID               string
	streamDeltaCallback     any
	toolProgressCallback    any
	toolStartCallback       any
	toolCompleteCallback    any
	interimAssistantCb      any
	reasoningCallback       any
	statusCallback          any
	agentRef                **agent.AIAgent
	activeRunID             string
	gatewaySessionKey       string
	requestedModel          string
	requestedProvider       string
	modelOptions            any
	route                   map[string]string
	sessionModel            string
	requestedRuntime        *pyjson.Object
	routeSource             string
	confirmedRuntimeLock    bool
	bindDeclaredConversaton bool
	requestID               string
	historyFromSession      bool
	runOwnerScope           any
	sessionHistoryDelivery  string
	turnAuthor              *pyjson.Object
	relayMetadata           *pyjson.Object
	notificationCategory    string
	resumeUnansweredTurn    bool
	approvalNotifyCallback  tools.GatewayNotifyFunc
	approvalSessionKey      string
	walkedFrom              string
}

// Error whose presentation the HTTP and SSE boundary suppresses for a muted diagnostic turn
type presentationSuppressedError struct{ err error }

func (e *presentationSuppressedError) Error() string { return e.err.Error() }
func (e *presentationSuppressedError) Unwrap() error { return e.err }

// Creates an agent and runs one turn on a counted worker, the session authority admitting it when present
func (a *APIServerAdapter) runAgent(ctx context.Context, o runAgentOptions) (map[string]any, map[string]any, error) {
	if _, ok := a.sessionAuthority(); ok {
		return a.runAPITurn(ctx, o)
	}
	if o.routeSource == "" {
		o.routeSource = "global"
	}
	if o.notificationCategory == "" {
		o.notificationCategory = "result"
	}
	requestProfileName := requestProfile(ctx)
	run := func() (out [2]map[string]any, err error) {
		scoped, err := profileScope(ctx, requestProfileName)
		if err != nil {
			return out, err
		}
		env := &tools.Env{Home: home.Dir(scoped), Config: config.Tree(gateway.LoadGatewayConfigMap(scoped, ""))}
		sessionKey := o.gatewaySessionKey
		if sessionKey == "" {
			sessionKey = o.sessionID
		}
		bindAPIServerSession(env, o.sessionID, sessionKey, o.sessionID, requestProfileName, o.sessionHistoryDelivery)
		defer tools.ClearSessionVars(env)
		muted := config.DiagnosticTurnMuted(scoped, map[string]any{"notification_category": o.notificationCategory}, "api_server", nil)
		var ag *agent.AIAgent
		defer func() {
			if o.activeRunID != "" {
				a.mu.Lock()
				a.forgetActiveRunAgentLocked(o.activeRunID)
				a.mu.Unlock()
			}
			if ag != nil {
				clearTurnProcessOwnership(ag)
				a.mu.Lock()
				delete(a.shutdownInterruptibleAgents, ag)
				a.mu.Unlock()
				a.memorySessions.Checkin(scoped, ag)
				if o.bindDeclaredConversaton {
					sid := ag.SessionID()
					if sid == "" {
						sid = o.sessionID
					}
					a.bindDeclaredConversation(scoped, sid, o.gatewaySessionKey)
				}
				forgetAgent(ag)
			}
		}()
		built, err := a.createAgent(scoped, agentOptions{ephemeralSystemPrompt: o.ephemeralSystemPrompt, sessionID: o.sessionID,
			streamDeltaCallback: o.streamDeltaCallback, toolProgressCallback: o.toolProgressCallback,
			toolStartCallback: o.toolStartCallback, toolCompleteCallback: o.toolCompleteCallback,
			interimAssistantCb: o.interimAssistantCb, reasoningCallback: o.reasoningCallback, statusCallback: o.statusCallback,
			gatewaySessionKey: o.gatewaySessionKey, requestedModel: o.requestedModel, requestedProvider: o.requestedProvider,
			modelOptions: o.modelOptions, route: o.route, sessionModel: o.sessionModel, confirmedRuntimeLock: o.confirmedRuntimeLock,
			env: env})
		if err != nil {
			var authErr *providerAuthResolutionError
			if errors.As(err, &authErr) {
				apiLog().Warn(fmt.Sprintf("Provider resolution failed for session=%s: %v", o.sessionID, authErr))
				result := map[string]any{"final_response": authErr.userText(scoped), "messages": []any{}, "api_calls": int64(0), "tools": []any{}}
				if muted {
					result["_notification_presentation_suppressed"] = true
				}
				return [2]map[string]any{result, {"input_tokens": int64(0), "output_tokens": int64(0), "total_tokens": int64(0)}}, nil
			}
			if muted {
				return out, &presentationSuppressedError{err}
			}
			return out, err
		}
		ag = built
		if o.agentRef != nil {
			*o.agentRef = ag
		}
		history := o.conversationHistory
		if o.resumeUnansweredTurn {
			agent.AdoptUnansweredTurn(&history, o.userMessage, ag)
		}
		if o.activeRunID != "" {
			a.mu.Lock()
			a.setActiveRunAgentLocked(o.activeRunID, ag)
			a.mu.Unlock()
		}
		taskID := o.sessionID
		if taskID == "" {
			taskID = uuid.NewString()
		}
		publishTurnProcessOwnership(ag, taskID)
		a.mu.Lock()
		a.shutdownInterruptibleAgents[ag] = struct{}{}
		a.mu.Unlock()
		runOpts := agent.RunConversationOptions{ConversationHistory: history, TaskID: taskID}
		if o.turnAuthor != nil {
			runOpts.TurnAuthor = pyjson.Plain(o.turnAuthor).(map[string]any)
		}
		if o.relayMetadata != nil && o.relayMetadata.Len() > 0 {
			runOpts.RelayMetadata = pyjson.Plain(o.relayMetadata).(map[string]any)
		}
		approvalBound := o.approvalNotifyCallback != nil && o.approvalSessionKey != ""
		if approvalBound {
			tools.SetCurrentSessionKey(env, o.approvalSessionKey)
			tools.RegisterGatewayNotify(o.approvalSessionKey, o.approvalNotifyCallback)
		}
		result, runErr := func() (map[string]any, error) {
			defer func() {
				if approvalBound {
					unregisterApprovalNotify(o.approvalSessionKey)
					func() {
						defer func() { _ = recover() }()
						tools.ResetCurrentSessionKey(env)
					}()
				}
			}()
			turnCtx, done := ag.NotificationTurn(scoped, muted, o.sessionID)
			defer done()
			return ag.RunConversationContent(turnCtx, o.userMessage, runOpts)
		}()
		if runErr != nil {
			if muted {
				return out, &presentationSuppressedError{runErr}
			}
			return out, runErr
		}
		if result == nil {
			result = map[string]any{}
		}
		result, usage, err := a.finishTurnResult(ag, result, o.sessionID, o.route, o.requestedRuntime, o.routeSource, o.confirmedRuntimeLock)
		if err != nil {
			if muted {
				return out, &presentationSuppressedError{err}
			}
			return out, err
		}
		if muted {
			projected := make(map[string]any, len(result)+1)
			for k, v := range result {
				projected[k] = v
			}
			projected["_notification_presentation_suppressed"] = true
			result = projected
		}
		return [2]map[string]any{result, usage}, nil
	}
	a.activateAdmittedRequest(ctx)
	a.mu.Lock()
	a.inflightAgentRuns++
	a.mu.Unlock()
	started := time.Now()
	var usage map[string]any
	defer func() {
		a.mu.Lock()
		a.inflightAgentRuns--
		a.mu.Unlock()
		if usage != nil {
			a.recordAPIMetrics(ctx, usage, since(started))
		}
	}()
	res := <-submitAPIWorker(run)
	if res.Panic != nil {
		panic(res.Panic)
	}
	if res.Err != nil {
		return nil, nil, res.Err
	}
	usage = res.Value[1]
	return res.Value[0], usage, nil
}

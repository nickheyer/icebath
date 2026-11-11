package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Fingerprint binding a durable Responses request to its wire fingerprint and declared session key
func requestBinding(ctx context.Context, a *APIServerAdapter, request *gateway.WebRequest, durableKey, fingerprint string) string {
	declared, _ := a.parseSessionKeyHeader(request)
	var declaredValue any
	if declared != "" {
		declaredValue = declared
	}
	return makeRequestFingerprint(pyjson.ObjectOf("request", fingerprint, "declared_session_key", declaredValue),
		[]string{"request", "declared_session_key"})
}

// Live and retired API admissions carrying a request id, a lost pointer never erasing an accepted identity
func requestAdmissionRows(ctx context.Context, db *state.SessionDB, requestID string) ([]*pyjson.Object, error) {
	var rows []*pyjson.Object
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		live, err := state.QueryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE principal_id='api' AND request_id=?", requestID)
		if err != nil {
			return err
		}
		retired, err := state.TerminalAPIAdmissions(ctx, conn, requestID)
		if err != nil {
			return err
		}
		rows = live
		for _, record := range retired {
			row, ok := record.(*pyjson.Object)
			if !ok {
				return &gateway.PyError{Kind: "TypeError", Msg: "'" + pyrepr.TypeName(record) + "' object is not iterable"}
			}
			rows = append(rows, row)
		}
		return nil
	})
	return rows, err
}

// Admission a durable Responses key or its exact request id already names, nil when none
func lookupAdmission(ctx context.Context, authority *gateway.SessionAuthority, store *gateway.ResponseStore, durableKey,
	requestID string) (*gateway.APIAdmission, error) {
	admissionID, err := store.RequestAdmission(ctx, durableKey)
	if err != nil {
		return nil, err
	}
	var row *pyjson.Object
	if admissionID != "" {
		if row, err = authority.DB.GetSessionAdmission(ctx, admissionID); err != nil {
			return nil, err
		}
		if row == nil {
			return nil, state.NewRuntimeStoreError("admission_conflict", nil, nil)
		}
	} else {
		rows, err := requestAdmissionRows(ctx, authority.DB, requestID)
		if err != nil {
			return nil, err
		}
		if len(rows) > 1 {
			return nil, state.NewRuntimeStoreError("admission_conflict", nil, nil)
		}
		if len(rows) == 0 {
			return nil, nil
		}
		if row, err = state.AdmissionRow(rows[0]); err != nil {
			return nil, err
		}
	}
	if !pyjson.Equal(row.GetOr("principal_id", nil), "api") || !pyjson.Equal(row.GetOr("request_id", nil), requestID) {
		return nil, state.NewRuntimeStoreError("permission_denied", nil, nil)
	}
	created, err := store.RequestCreatedAt(ctx, durableKey)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, state.NewRuntimeStoreError("admission_conflict", nil, nil)
	}
	if err := store.BindRequestAdmission(ctx, durableKey, rowText(row, "admission_id")); err != nil {
		return nil, err
	}
	return &gateway.APIAdmission{Authority: authority, Ref: gateway.SessionRef{ProfileID: authority.ProfileID,
		SessionID: rowText(row, "target_session_id")}, Row: row}, nil
}

// Found admission with a live one's session restored and checked on the caller
func restoreAdmitted(ctx context.Context, found *gateway.APIAdmission) (*gateway.APIAdmission, error) {
	if found == nil {
		return nil, nil
	}
	if status := rowText(found.Row, "status"); status != "queued" && status != "started" {
		return found, nil
	}
	if err := gateway.RestoreAPISession(ctx, found.Authority, found.Ref.SessionID); err != nil {
		return nil, err
	}
	payload, _ := found.Row.GetOr("payload", nil).(*pyjson.Object)
	if err := gateway.CheckAPITurn(ctx, found.Authority, found.Ref, payload); err != nil {
		return nil, err
	}
	return found, nil
}

// Authority, store and the one lookup unit a found admission is read through
func lookupCall(ctx context.Context, a *APIServerAdapter, durableKey, requestID string) (*gateway.ResponseStore, func(ctx context.Context) (any, error), error) {
	authority, err := gateway.RequireActiveAuthority(ctx, a.GatewayRunner, func() (*state.SessionDB, error) { return a.EnsureSessionDB(ctx) })
	if err != nil {
		return nil, nil, err
	}
	store, err := a.currentResponseStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	return store, func(ctx context.Context) (any, error) {
		return lookupAdmission(ctx, authority, store, durableKey, requestID)
	}, nil
}

// Atomic lookup of an admission through the store
func atomicLookup(ctx context.Context, store *gateway.ResponseStore, lookup func(ctx context.Context) (any, error)) (*gateway.APIAdmission, error) {
	found, err := store.Atomically(ctx, lookup)
	if err != nil {
		return nil, err
	}
	admitted, _ := found.(*gateway.APIAdmission)
	return admitted, nil
}

// Admission a durable Responses request already holds, its session restored, nil when none
func findAdmission(ctx context.Context, a *APIServerAdapter, durableKey, requestID string) (*gateway.APIAdmission, error) {
	store, lookup, err := lookupCall(ctx, a, durableKey, requestID)
	if err != nil {
		return nil, err
	}
	found, err := atomicLookup(ctx, store, lookup)
	if err != nil {
		return nil, err
	}
	return restoreAdmitted(ctx, found)
}

// Responses context of an admitted turn, its result observed from the canonical admission
func admittedContext(ctx context.Context, admitted *gateway.APIAdmission, replay bool) (*responsesContext, error) {
	payload, _ := admitted.Row.GetOr("payload", nil).(*pyjson.Object)
	if payload == nil {
		payload = pyjson.NewObject()
	}
	data, _ := payload.GetOr("api_turn_v1", pyjson.NewObject()).(*pyjson.Object)
	if data == nil {
		data = pyjson.NewObject()
	}
	userMessage, err := gateway.RehydrateAPIImages(ctx, payload.GetOr("text", ""), data.GetOr("media", nil))
	if err != nil {
		return nil, err
	}
	history, _ := data.GetOr("history", nil).([]any)
	if history == nil {
		history = []any{}
	}
	var instructions any
	if settings, ok := data.GetOr("settings", pyjson.NewObject()).(*pyjson.Object); ok && settings != nil {
		instructions = settings.GetOr("ephemeral_system_prompt", nil)
	}
	observe := func(ctx context.Context, run runAgentOptions) (map[string]any, map[string]any, error) {
		return gateway.ObserveAPITurn(ctx, admitted, observerCallbacks("stream_delta_callback", run.streamDeltaCallback,
			"tool_progress_callback", run.toolProgressCallback, "tool_start_callback", run.toolStartCallback,
			"tool_complete_callback", run.toolCompleteCallback, "interim_assistant_callback", run.interimAssistantCb,
			"reasoning_callback", run.reasoningCallback, "status_callback", run.statusCallback))
	}
	return &responsesContext{sessionID: admitted.Ref.SessionID, userMessage: userMessage, conversationHistory: history,
		instructions: instructions, runAgent: observe, terminalReplay: replay || rowText(admitted.Row, "status") == "terminal"}, nil
}

// Admission of a durable Responses turn and whether it replays one the key already admitted
type responsesAdmitted struct {
	*gateway.APIAdmission
	replay bool
}

// Final lookup and admission of a durable Responses turn as one API admission drive
func admitSteps(ctx context.Context, a *APIServerAdapter, durableKey, requestID, binding string, turn *gateway.APITurn, model any) gateway.WriteSteps {
	return func(run gateway.StepRunner) (any, error) {
		store, lookup, err := lookupCall(ctx, a, durableKey, requestID)
		if err != nil {
			return nil, err
		}
		bound, err := run(func(ctx context.Context) (any, error) { return store.BindRequestKey(ctx, durableKey, binding, model) })
		if err != nil {
			return nil, err
		}
		if bound != true {
			return nil, state.NewRuntimeStoreError("admission_conflict", nil, nil)
		}
		lookupStep := func(ctx context.Context) (any, error) { return atomicLookup(ctx, store, lookup) }
		found, err := run(lookupStep)
		if err != nil {
			return nil, err
		}
		existing, err := restoreAdmitted(ctx, found.(*gateway.APIAdmission))
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return &responsesAdmitted{existing, true}, nil
		}
		admittedResult, err := gateway.AdmitAPISteps(ctx, a, turn)(run)
		if err != nil {
			again, lookupErr := run(lookupStep)
			if lookupErr != nil {
				return nil, lookupErr
			}
			restored, restoreErr := restoreAdmitted(ctx, again.(*gateway.APIAdmission))
			if restoreErr != nil {
				return nil, restoreErr
			}
			if restored == nil {
				if _, forgetErr := run(func(ctx context.Context) (any, error) {
					return nil, store.ForgetUnadmittedRequest(ctx, durableKey)
				}); forgetErr != nil {
					return nil, forgetErr
				}
			}
			return nil, err
		}
		admitted := admittedResult.(*gateway.APIAdmission)
		if _, err := run(func(ctx context.Context) (any, error) {
			return nil, store.BindRequestAdmission(ctx, durableKey, rowText(admitted.Row, "admission_id"))
		}); err != nil {
			return nil, err
		}
		return &responsesAdmitted{admitted, false}, nil
	}
}

// Admits a durable Responses turn and answers the context its observation reads
func admitResponses(ctx context.Context, a *APIServerAdapter, durableKey, requestID, binding string, turn *gateway.APITurn, model any) (*responsesContext, error) {
	result, err := gateway.RunAPIStepsOffLoop(ctx, a, admitSteps(context.WithoutCancel(ctx), a, durableKey, requestID, binding, turn, model))
	if err != nil {
		return nil, err
	}
	admitted := result.(*responsesAdmitted)
	return admittedContext(ctx, admitted.APIAdmission, admitted.replay)
}

// Admission owner of a stored response's session, the lineage root the API binding lives on
func storedOwner(ctx context.Context, a *APIServerAdapter, sessionID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	authority, err := gateway.ActiveAuthority(ctx, a.GatewayRunner)
	if err != nil || authority == nil {
		return sessionID, err
	}
	return authority.LogicalOwner(ctx, sessionID)
}

// Binds the session a chain moved to before any response starts, a busy store refused retryably
func bindSuccessor(ctx context.Context, a *APIServerAdapter, sessionID, walkedFrom, gatewaySessionKey string) error {
	authority, err := gateway.ActiveAuthority(ctx, a.GatewayRunner)
	if err != nil || authority == nil {
		return err
	}
	_, err = gateway.RunStepsOffLoop(ctx, authority, gateway.BindAPISteps(ctx, authority, sessionID, nil, gatewaySessionKey, walkedFrom))
	var refusal *state.RuntimeStoreError
	if err == nil || errors.As(err, &refusal) {
		return err
	}
	slog.Warn(fmt.Sprintf("pre-stream bind of %s failed", sessionID), "error", err)
	return state.NewRuntimeStoreError("storage_unavailable", nil, nil)
}

// Validates a first time Responses request, an exact retry answering from its accepted payload and result
func prepareResponsesContext(ctx context.Context, a *APIServerAdapter, request *gateway.WebRequest, body *pyjson.Object, gatewaySessionKey,
	durableKey, durableFP, scope, idempotencyKey string) (*responsesContext, *gateway.WebResponse, error) {
	requestID := ""
	if durableKey != "" {
		requestID = "responses:" + scope + ":" + idempotencyKey
		admitted, err := findAdmission(ctx, a, durableKey, requestID)
		if err != nil {
			return nil, nil, err
		}
		if admitted != nil {
			rc, err := admittedContext(ctx, admitted, true)
			return rc, nil, err
		}
	}
	previous := body.GetOr("previous_response_id", nil)
	responseStore, err := a.currentResponseStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	if conversation := body.GetOr("conversation", nil); pyrepr.Truthy(conversation) {
		named, err := responseStore.Atomically(ctx, func(ctx context.Context) (any, error) {
			return responseStore.GetConversation(ctx, pyrepr.Str(conversation))
		})
		if err != nil {
			return nil, nil, err
		}
		previous = nil
		if text, _ := named.(string); text != "" {
			previous = text
		}
	}
	messages, errResp := parseResponsesInput(body.GetOr("input", nil))
	if errResp != nil {
		return nil, errResp, nil
	}
	history := []any{}
	if raw := body.GetOr("conversation_history", nil); pyrepr.Truthy(raw) {
		if history, errResp = parseConversationHistory(raw); errResp != nil {
			return nil, errResp, nil
		}
	}
	storedSessionID, chainRoot := "", ""
	instructions := body.GetOr("instructions", nil)
	if len(history) == 0 && pyrepr.Truthy(previous) {
		stored, err := responseStore.Atomically(ctx, func(ctx context.Context) (any, error) { return responseStore.Get(ctx, pyrepr.Str(previous)) })
		if err != nil {
			return nil, nil, err
		}
		storedDict, _ := stored.(*pyjson.Object)
		if storedDict == nil {
			return nil, errorResponse("Previous response not found: "+pyrepr.Str(previous), http.StatusNotFound, errOpts{}), nil
		}
		if prior, ok := storedDict.GetOr("conversation_history", []any{}).([]any); ok {
			history = append([]any{}, prior...)
		}
		storedSession, _ := storedDict.GetOr("session_id", nil).(string)
		if chainRoot, err = storedOwner(ctx, a, storedSession); err != nil {
			return nil, nil, err
		}
		resolved, err := gateway.ContinuedAPISession(ctx, a.GatewayRunner, chainRoot, requestID)
		if err != nil {
			if isStoreError(err) {
				return nil, storeErrorResponse(err), nil
			}
			return nil, nil, err
		}
		storedSessionID = resolved
		if instructions == nil {
			instructions = storedDict.GetOr("instructions", nil)
		}
	}
	for _, message := range messages[:max(0, len(messages)-1)] {
		history = append(history, message)
	}
	var userMessage any = ""
	if len(messages) > 0 {
		userMessage = messages[len(messages)-1].GetOr("content", "")
	}
	if !gateway.APIContentHasVisiblePayload(userMessage) {
		return nil, errorResponse("No user message found in input", http.StatusBadRequest, errOpts{}), nil
	}
	if body.GetOr("truncation", nil) == "auto" {
		history = autoTruncateResponseHistory(history, responsesAutoTruncationHistoryLimit)
	}
	declaredSelected := storedSessionID == "" && gatewaySessionKey != ""
	sessionID := storedSessionID
	if sessionID == "" {
		sessionID = a.declaredConversationSession(ctx, gatewaySessionKey)
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	if durableKey != "" && storedSessionID == "" && gatewaySessionKey == "" {
		sum := sha256.Sum256([]byte(scope + "\x00" + idempotencyKey))
		sessionID = "response-" + hex.EncodeToString(sum[:])
	}
	route, overrides, errResp := a.selectRequestRoute(ctx, body, sessionID, gatewaySessionKey, body.GetOr("model", nil))
	if errResp != nil {
		return nil, errResp, nil
	}
	walkedFrom := ""
	if storedSessionID != "" && storedSessionID != chainRoot {
		walkedFrom = chainRoot
	}
	relayMetadata := requestRelayMetadata(body)
	if durableKey != "" {
		var routeValue, relayValue any
		if route != nil {
			plain := make(map[string]any, len(route))
			for key, value := range route {
				plain[key] = value
			}
			routeValue = plain
		}
		if relayMetadata != nil {
			relayValue = relayMetadata
		}
		settings := map[string]any{"route_source": "global", "confirmed_runtime_lock": false, "session_history_delivery": "",
			"ephemeral_system_prompt": instructions, "route": routeValue}
		for _, key := range []string{"requested_model", "requested_provider", "model_options"} {
			if value, ok := overrides[key]; ok {
				settings[key] = value
			}
		}
		history := append([]any{}, history...)
		turn := &gateway.APITurn{UserMessage: userMessage, ConversationHistory: history, SessionID: sessionID,
			GatewaySessionKey: gatewaySessionKey, BindDeclaredConversation: declaredSelected, Settings: settings,
			RelayMetadata: relayValue, RequestID: requestID, WalkedFrom: walkedFrom}
		model := body.GetOr("model", a.modelName)
		rc, err := admitResponses(ctx, a, durableKey, requestID, requestBinding(ctx, a, request, durableKey, durableFP), turn, model)
		return rc, nil, err
	}
	if walkedFrom != "" {
		if err := bindSuccessor(ctx, a, sessionID, walkedFrom, gatewaySessionKey); err != nil {
			return nil, nil, err
		}
	}
	prompt := ""
	if text, ok := instructions.(string); ok {
		prompt = text
	}
	run := runAgentOptions{userMessage: userMessage, conversationHistory: historyMaps(history), ephemeralSystemPrompt: prompt,
		sessionID: sessionID, gatewaySessionKey: gatewaySessionKey, bindDeclaredConversaton: declaredSelected, route: route,
		relayMetadata: relayMetadata, walkedFrom: walkedFrom}
	run.requestedModel, _ = overrides["requested_model"].(string)
	run.requestedProvider, _ = overrides["requested_provider"].(string)
	if options, ok := overrides["model_options"]; ok {
		run.modelOptions = options
	}
	return &responsesContext{sessionID: sessionID, userMessage: userMessage, conversationHistory: history, instructions: instructions,
		run: run, runAgent: a.runAgent}, nil, nil
}

package gatewayapi

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Settings a runs launch hands its admission, None for each override the request did not name
func runLaunchSettings(o agentOptions) map[string]any {
	var route, dispatch, policy any
	if o.route != nil {
		plain := make(map[string]any, len(o.route))
		for key, value := range o.route {
			plain[key] = value
		}
		route = plain
	}
	if o.roomDispatch != nil {
		dispatch = o.roomDispatch
	}
	if o.roomExecutionPolicy != nil {
		policy = o.roomExecutionPolicy
	}
	return map[string]any{"ephemeral_system_prompt": optionalText(o.ephemeralSystemPrompt), "route": route,
		"room_dispatch": dispatch, "room_execution_policy": policy, "requested_model": optionalText(o.requestedModel),
		"requested_provider": optionalText(o.requestedProvider), "model_options": o.modelOptions}
}

// Admits a runs turn to the session authority FIFO, the refusal response when it is refused
func (a *APIServerAdapter) admitRunToAuthority(ctx context.Context, launch *runLaunch, sessionHistoryDelivery, declaredSelected bool,
	scope, idempotencyKey string) (*gateway.WebResponse, error) {
	runID := launch.runID
	a.mu.Lock()
	owner := a.runOwners[runID]
	a.mu.Unlock()
	settings := runLaunchSettings(launch.agentOptions)
	delivery := ""
	if sessionHistoryDelivery {
		delivery = "1"
	}
	settings["session_history_delivery"] = delivery
	var turnAuthor any
	if launch.turnAuthor != nil {
		turnAuthor = launch.turnAuthor
	}
	admitted, err := func() (*gateway.APIAdmission, error) {
		scoped, err := profileScope(withRequestProfile(ctx, launch.requestProfile), launch.requestProfile)
		if err != nil {
			return nil, err
		}
		return gateway.AdmitAPITurnAsync(scoped, a, &gateway.APITurn{UserMessage: launch.userMessage,
			ConversationHistory: launch.conversationHistory, ActiveRunID: runID, RunOwnerScope: owner, TurnAuthor: turnAuthor,
			HistoryFromSession: sessionHistoryDelivery, BindDeclaredConversation: declaredSelected, WalkedFrom: launch.walkedFrom,
			SessionID: launch.agentOptions.sessionID, GatewaySessionKey: launch.agentOptions.gatewaySessionKey, Settings: settings})
	}()
	if err == nil {
		launch.admission = admitted
		return nil, nil
	}
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		owned, ownedErr := runAdmission(context.WithoutCancel(ctx), a, runID)
		if ownedErr != nil {
			return nil, ownedErr
		}
		if owned != nil {
			launch.admission = &gateway.APIAdmission{Authority: owned.authority, Ref: gateway.SessionRef{ProfileID: owned.authority.ProfileID,
				SessionID: rowText(owned.row, "target_session_id")}, Row: owned.row}
			return nil, nil
		}
	}
	a.mu.Lock()
	a.forgetRunLocked(runID, func(id string) { delete(a.runStreams, id) }, func(id string) { delete(a.runStreamsCreated, id) },
		func(id string) { delete(a.runApprovalSessions, id) }, func(id string) { delete(a.runStatuses, id) },
		func(id string) { delete(a.runOwners, id) }, func(id string) { delete(a.runIdempotencyIDs, id) })
	a.mu.Unlock()
	if idempotencyKey != "" {
		if err := a.runIdempotencyStore.Forget(context.WithoutCancel(ctx), scope, idempotencyKey); err != nil {
			return nil, err
		}
	}
	return storeErrorResponse(err), nil
}

// Settled result and usage of a runs admission, its prompts projected to the run stream while it executes
func observeAPIRun(ctx context.Context, admission any, controlCB func(eventType string, prompt *pyjson.Object), textCB func(string),
	toolStart func(callID, name string, args map[string]any), toolComplete func(callID, name string, args map[string]any, result string)) (map[string]any, map[string]any, error) {
	admitted := admission.(*gateway.APIAdmission)
	release, err := gateway.ObserveAPIControls(ctx, admitted, func(eventType string, prompt *pyjson.Object) error {
		controlCB(eventType, prompt)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return gateway.ObserveAPITurn(ctx, admitted, observerCallbacks("stream_delta_callback", textCB, "tool_start_callback", toolStart,
		"tool_complete_callback", toolComplete))
}

package gatewayapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Status of a refused store reason, the documented 409 for every other reason
var storeErrorStatus = map[string]int{"invalid_params": 400, "runtime_draining": 503, "storage_unavailable": 503, "chain_moved": 503}

// HTTP answer of a refused store call, a runtime store error keeping its reason and anything else storage_unavailable
func storeErrorResponse(err error) *gateway.WebResponse {
	reason := "storage_unavailable"
	var runtimeErr *state.RuntimeStoreError
	if errors.As(err, &runtimeErr) {
		reason = runtimeErr.Reason
	}
	if reason == "admission_queued" {
		return jsonResponse(openAIError(err.Error(), "server_error", nil, reason), http.StatusServiceUnavailable,
			map[string]string{"x-should-retry": "false"})
	}
	status, ok := storeErrorStatus[reason]
	if !ok {
		status = http.StatusConflict
	}
	var headers map[string]string
	if status == http.StatusServiceUnavailable {
		headers = map[string]string{"Retry-After": "1"}
	}
	return jsonResponse(openAIError(reason, "", nil, reason), status, headers)
}

// Live continuation of a chain's stored session, a busy or unreadable store the retryable 503
func continuedSession(ctx context.Context, a *APIServerAdapter, sessionID string) (string, *gateway.WebResponse, error) {
	resolved, err := gateway.ContinuedAPISession(ctx, a.GatewayRunner, sessionID, "")
	if err != nil {
		if isStoreError(err) {
			return sessionID, storeErrorResponse(err), nil
		}
		return sessionID, nil, err
	}
	return resolved, nil, nil
}

// Authority and ledger row of one API run, the run id resolving through a streaming alias first
type authorityRunAdmission struct {
	authority *gateway.SessionAuthority
	row       *jsonutil.Object
}

// Canonical admission of a run id under the routed authority, nil when there is none
func runAdmission(ctx context.Context, a *APIServerAdapter, runID string) (*authorityRunAdmission, error) {
	authority, err := gateway.ActiveAuthority(ctx, a.GatewayRunner)
	if err != nil || authority == nil {
		return nil, err
	}
	a.mu.Lock()
	alias := a.runAdmissionAliases[runID]
	a.mu.Unlock()
	column, value := "request_id", runID
	if alias != "" {
		column, value = "admission_id", alias
	}
	var rows []*jsonutil.Object
	err = authority.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var queryErr error
		rows, queryErr = state.QueryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE principal_id='api' AND "+column+"=?", value)
		return queryErr
	})
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, state.NewRuntimeStoreError("admission_conflict", nil, nil)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row, err := state.AdmissionRow(rows[0])
	if err != nil {
		return nil, err
	}
	return &authorityRunAdmission{authority: authority, row: row}, nil
}

// Authority and ledger row of a run's canonical admission, the ownership check's view of the adapter
func (a *APIServerAdapter) RunAdmission(ctx context.Context, runID string) (*gateway.SessionAuthority, *jsonutil.Object, error) {
	owned, err := runAdmission(ctx, a, runID)
	if err != nil || owned == nil {
		return nil, nil, err
	}
	return owned.authority, owned.row, nil
}

// Text of a ledger row field
func rowText(row *jsonutil.Object, key string) string {
	text, _ := row.ValueOr(key, nil).(string)
	return text
}

// Pollable projection of an authority run, its prompts pending while it waits for approval
func runProjection(ctx context.Context, a *APIServerAdapter, runID string) (*jsonutil.Object, error) {
	owned, err := runAdmission(ctx, a, runID)
	if err != nil || owned == nil {
		return nil, err
	}
	authority, row := owned.authority, owned.row
	rowStatus := rowText(row, "status")
	var status any
	switch rowStatus {
	case "queued":
		status = "queued"
	case "started":
		status = "running"
	case "unknown":
		status = "unknown"
	case "terminal":
		status = row.ValueOr("outcome", nil)
	}
	saved, err := gateway.AdmissionResult(ctx, authority.DB, rowText(row, "admission_id"))
	if err != nil {
		return nil, err
	}
	savedDict, _ := saved.(*jsonutil.Object)
	result := jsonutil.NewObject()
	if savedDict != nil {
		if stored, ok := savedDict.ValueOr("result", jsonutil.NewObject()).(*jsonutil.Object); ok {
			result = stored
		}
	}
	if rowStatus == "terminal" {
		outcome := row.ValueOr("outcome", nil)
		switch {
		case anyutil.IsTrue(result.ValueOr("interrupted", nil)) || outcome == "interrupted":
			status = "cancelled"
		case anyutil.IsTrue(result.ValueOr("failed", nil)) || anyutil.IsTrue(result.ValueOr("error", nil)):
			status = "failed"
		case outcome == "completed" && savedDict != nil:
			plain, _ := jsonutil.Plain(result).(map[string]any)
			status, _ = terminalRunStatus(plain)
		}
	}
	pending := []any{}
	sessionID := rowText(row, "target_session_id")
	if rowStatus == "started" {
		var generation *int64
		if value, isInt := row.ValueOr("generation", nil).(int64); isInt {
			generation = &value
		}
		if prompts, live := authority.PendingPrompts(sessionID, generation); live {
			for _, prompt := range prompts {
				pending = append(pending, prompt)
				if prompt.ValueOr("kind", nil) == "approval" {
					status = "waiting_for_approval"
				}
			}
		}
	}
	usage := any(jsonutil.NewObject())
	if savedDict != nil {
		usage = savedDict.ValueOr("usage", jsonutil.NewObject())
	}
	return jsonutil.ObjectOf("pending_controls", pending, "run_id", runID, "status", status, "session_id", sessionID,
		"admission_id", row.ValueOr("admission_id", nil), "execution_generation", row.ValueOr("generation", nil),
		"output", result.ValueOr("final_response", ""), "usage", usage), nil
}

// Acknowledges a clarify question for a running canonical API session, the shared prompt registered after it
func sendClarify(ctx context.Context, a *APIServerAdapter, chatID, clarifyID string) (gateway.SendResult, error) {
	authority, err := gateway.ActiveAuthority(ctx, a.GatewayRunner)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if authority == nil || !authority.HasLiveSession(chatID) {
		return gateway.SendResult{Success: false, Error: "No canonical API session"}, nil
	}
	handle, err := authority.Handle(ctx, gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: chatID})
	if err != nil {
		return gateway.SendResult{}, err
	}
	if handle.ExecutionState != "running" {
		return gateway.SendResult{Success: false, Error: "No active API execution"}, nil
	}
	return gateway.SendResult{Success: true, MessageID: clarifyID}, nil
}

// Capability set of a principal
func capabilities(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// Answers an authority run's pending approval or clarify prompt for its exact running generation
func respondRun(ctx context.Context, a *APIServerAdapter, runID string, body *jsonutil.Object, kind string) (*jsonutil.Object, error) {
	owned, err := runAdmission(ctx, a, runID)
	if err != nil {
		return nil, err
	}
	if owned == nil {
		return nil, state.NewRuntimeStoreError("not_found", nil, nil)
	}
	authority, row := owned.authority, owned.row
	generation := body.ValueOr("execution_generation", nil)
	promptID := body.ValueOr("request_id", nil)
	field := "answer"
	if kind == "approval" {
		field = "choice"
	}
	expected := map[string]bool{"request_id": true, "execution_generation": true, field: true}
	if body.Len() != len(expected) {
		return nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
	}
	for _, key := range body.Keys() {
		if !expected[key] {
			return nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
		}
	}
	stamp, isInt := generation.(int64)
	if rowText(row, "status") != "started" || !isInt || !anyutil.Equal(stamp, row.ValueOr("generation", nil)) {
		return nil, state.NewRuntimeStoreError("stale_generation", nil, nil)
	}
	ref := gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: rowText(row, "target_session_id")}
	actor := gateway.Principal{Subject: "api", ProfileID: authority.ProfileID,
		Capabilities: capabilities("session:read", "session:approve", "session:respond"), TransportID: "api-control:" + hexUUID()}
	snapshot, err := authority.Attach(ctx, actor, ref, false, nil)
	if err != nil {
		return nil, err
	}
	result, err := func() (any, error) {
		pending := false
		for _, prompt := range snapshot.Prompts {
			if anyutil.Equal(prompt["prompt_id"], promptID) && anyutil.Equal(prompt["kind"], kind) {
				pending = true
				break
			}
		}
		if !pending {
			return nil, state.NewRuntimeStoreError("approval_not_pending", nil, nil)
		}
		return authority.Respond(ctx, actor, ref, generation, promptID, jsonutil.ObjectOf(field, body.ValueOr(field, nil)), kind)
	}()
	if detachErr := authority.Detach(context.WithoutCancel(ctx), actor, snapshot.SubscriptionID, "", false); detachErr != nil {
		return nil, detachErr
	}
	if err != nil {
		return nil, err
	}
	answer, _ := result.(*jsonutil.Object)
	return answer, nil
}

// Stops an authority run, cancelling a queued admission or interrupting the exact running claim
func stopRun(ctx context.Context, a *APIServerAdapter, runID string) (*jsonutil.Object, error) {
	owned, err := runAdmission(ctx, a, runID)
	if err != nil {
		return nil, err
	}
	if owned == nil {
		return nil, state.NewRuntimeStoreError("not_found", nil, nil)
	}
	authority, row := owned.authority, owned.row
	ref := gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: rowText(row, "target_session_id")}
	actor := gateway.Principal{Subject: "api", ProfileID: authority.ProfileID,
		Capabilities: capabilities("session:submit", "session:control"), TransportID: "api-run:" + runID}
	if rowText(row, "status") == "queued" {
		_, err := authority.CancelQueued(ctx, actor, ref, rowText(row, "admission_id"))
		var refusal *state.RuntimeStoreError
		switch {
		case err == nil:
			a.mu.Lock()
			a.stoppingRunIDs[runID] = true
			a.mu.Unlock()
			authority.ReleaseAdmissionWaiter(rowText(row, "admission_id"))
			return runProjection(ctx, a, runID)
		case errors.As(err, &refusal) && refusal.Reason == "stale_generation":
			again, err := runAdmission(ctx, a, runID)
			if err != nil {
				return nil, err
			}
			if again == nil {
				return nil, &errorutil.Error{Kind: "TypeError", Msg: "run admission vanished after a stale generation refusal"}
			}
			authority, row = again.authority, again.row
		default:
			return nil, err
		}
	}
	switch rowText(row, "status") {
	case "started":
		generation, _ := row.ValueOr("generation", nil).(int64)
		if _, err := authority.Interrupt(ctx, actor, ref, generation); err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.stoppingRunIDs[runID] = true
		a.mu.Unlock()
		return jsonutil.ObjectOf("run_id", runID, "status", "stopping", "admission_id", row.ValueOr("admission_id", nil)), nil
	case "unknown":
		return nil, state.NewRuntimeStoreError("unknown_execution", nil, nil)
	}
	return runProjection(ctx, a, runID)
}

// Generation a body value names, one no ledger row carries for a value that is not a Python int
func bodyGeneration(value any) int64 {
	if generation, isInt := value.(int64); isInt {
		return generation
	}
	return -1
}

// Acknowledges the exact unknown admission durably bound to an owned API run
func resolveUnknownRun(ctx context.Context, a *APIServerAdapter, runID string, body any) (*jsonutil.Object, error) {
	owned, err := runAdmission(ctx, a, runID)
	if err != nil {
		return nil, err
	}
	if owned == nil {
		return nil, state.NewRuntimeStoreError("not_found", nil, nil)
	}
	authority, row := owned.authority, owned.row
	dict, isDict := body.(*jsonutil.Object)
	if !isDict || dict.Len() != 2 {
		return nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
	}
	admissionID, hasAdmission := dict.Get("admission_id")
	generation, hasGeneration := dict.Get("execution_generation")
	if !hasAdmission || !hasGeneration {
		return nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
	}
	if !anyutil.Equal(admissionID, row.ValueOr("admission_id", nil)) {
		return nil, state.NewRuntimeStoreError("not_found", nil, nil)
	}
	actor := gateway.Principal{Subject: "api", ProfileID: authority.ProfileID,
		Capabilities: capabilities("session:submit", "session:control"), TransportID: "api-run:" + runID}
	receipt, err := authority.ResolveUnknown(ctx, actor, gateway.SessionRef{ProfileID: authority.ProfileID,
		SessionID: rowText(row, "target_session_id")}, rowText(row, "admission_id"), bodyGeneration(generation))
	if err != nil {
		return nil, err
	}
	return gateway.AdmissionReceiptDict(receipt), nil
}

// Agent a steer reaches for an authority run's running generation, or the latch when the turn has not adopted one
func steerAuthorityRun(ctx context.Context, owned *authorityRunAdmission, text string) (*agent.AIAgent, bool, error) {
	authority, row := owned.authority, owned.row
	generation, _ := row.ValueOr("generation", nil).(int64)
	steered, latched, err := authority.SteerRunningGeneration(ctx, rowText(row, "target_session_id"), generation, text)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if gateway.IsKeyError(err) || errors.As(err, &refusal) {
			return nil, false, nil
		}
		return nil, false, err
	}
	ag, _ := steered.(*agent.AIAgent)
	return ag, latched, nil
}

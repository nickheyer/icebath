package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds between internal wake retries while the concurrent run cap is saturated
var wakeRetryDelaysSeconds = []float64{2.0, 5.0, 10.0}

// Releases a run's approval waiter, nothing without a key
func unregisterApprovalNotify(approvalSessionKey string) {
	defer func() { _ = recover() }()
	if approvalSessionKey != "" {
		tools.UnregisterGatewayNotify(approvalSessionKey)
	}
}

// Drops the owner stamp once nothing keyed by the run survives
func (a *APIServerAdapter) releaseRunOwnerIfForgotten(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseRunOwnerIfForgottenLocked(runID)
}

func (a *APIServerAdapter) releaseRunOwnerIfForgottenLocked(runID string) {
	_, s := a.runStatuses[runID]
	_, ag := a.activeRunAgents[runID]
	_, t := a.activeRunTasks[runID]
	_, st := a.runStreams[runID]
	_, ap := a.runApprovalSessions[runID]
	if !s && !ag && !t && !st && !ap {
		delete(a.runOwners, runID)
	}
}

// Whether the request's principal owns a run, in memory first then the durable records
func (a *APIServerAdapter) requestOwnsRun(ctx context.Context, request *gateway.WebRequest, runID string) (bool, error) {
	scope := a.runIdempotencyScope(ctx, request)
	a.mu.Lock()
	owner, ok := a.runOwners[runID]
	a.mu.Unlock()
	if ok {
		return owner == scope, nil
	}
	owns, err := a.runIdempotencyStore.OwnsRun(ctx, scope, runID)
	if err != nil || owns {
		return owns, err
	}
	return gateway.OwnsAPIRun(ctx, a, runID, scope)
}

// Pollable status of a scoped run, hydrated from the durable store with a dead owner failed closed
func (a *APIServerAdapter) durableRunStatus(ctx context.Context, request *gateway.WebRequest, runID string) (*pyjson.Object, error) {
	canonical, err := runProjection(ctx, a, runID)
	if err != nil {
		return nil, err
	}
	if canonical != nil {
		a.mu.Lock()
		retained := a.runStatuses[runID]
		a.mu.Unlock()
		out := pyjson.ObjectOf("object", "hermes.run")
		if retained != nil {
			for _, k := range []string{"model", "runtime", "created_at"} {
				if v, ok := retained.Get(k); ok {
					out.Set(k, v)
				}
			}
		}
		out.Update(canonical)
		return out, nil
	}
	a.mu.Lock()
	status := a.runStatuses[runID]
	idempotent := a.runIdempotencyIDs[runID]
	a.mu.Unlock()
	if status != nil {
		if idempotent {
			if _, err := a.runIdempotencyStore.ExtendRetention(ctx, a.runIdempotencyScope(ctx, request), runID, roomRetentionUntil(ctx)); err != nil {
				return nil, err
			}
		}
		return status, nil
	}
	scope := a.runIdempotencyScope(ctx, request)
	record, err := a.runIdempotencyStore.StatusForRun(ctx, scope, runID, roomRetentionUntil(ctx))
	if err != nil || record == nil {
		return nil, err
	}
	stored, _ := record.Status.(*pyjson.Object)
	if stored == nil {
		stored = pyjson.NewObject()
	}
	status = stored.Copy()
	if !gateway.APIRunTerminalStatus(pyrepr.Str(status.GetOr("status", ""))) && !ownerAlive(record.OwnerPID, record.OwnerStarted) {
		status.Set("status", "interrupted")
		status.Set("error", "The gateway restarted before this run settled.")
		status.Set("last_event", "run.interrupted")
		status.Set("updated_at", apiNow())
		if err := a.runIdempotencyStore.UpdateStatus(ctx, runID, status); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	a.runStatuses[runID] = status
	a.runIdempotencyIDs[runID] = true
	a.runOwners[runID] = scope
	a.mu.Unlock()
	return status, nil
}

// History, instructions and stored session of a runs body, explicit history before a response chain before prior inputs
func (a *APIServerAdapter) resolveConversationHistory(ctx context.Context, body *pyjson.Object, rawInput any) ([]any, any, string, *gateway.WebResponse, error) {
	instructions := body.GetOr("instructions", nil)
	previousResponseID := body.GetOr("previous_response_id", nil)
	history := []any{}
	if rawHistory := body.GetOr("conversation_history", nil); pyrepr.Truthy(rawHistory) {
		list, ok := rawHistory.([]any)
		if !ok {
			return nil, instructions, "", jsonError("'conversation_history' must be an array of message objects", "", http.StatusBadRequest), nil
		}
		for i, raw := range list {
			entry, isObj := raw.(*pyjson.Object)
			hasBoth := false
			if isObj {
				_, r := entry.Get("role")
				_, c := entry.Get("content")
				hasBoth = r && c
			}
			if !hasBoth {
				return nil, instructions, "", jsonError(fmt.Sprintf("conversation_history[%d] must have 'role' and 'content' fields", i), "", http.StatusBadRequest), nil
			}
			role, _ := entry.Get("role")
			content, _ := entry.Get("content")
			history = append(history, pyjson.ObjectOf("role", pyrepr.Str(role), "content", pyrepr.Str(content)))
		}
		if pyrepr.Truthy(previousResponseID) {
			apiLog().Debug("Both conversation_history and previous_response_id provided; using conversation_history")
		}
	}
	storedSessionID := ""
	if len(history) == 0 && pyrepr.Truthy(previousResponseID) {
		store, err := a.currentResponseStore(ctx)
		if err != nil {
			return nil, instructions, "", nil, err
		}
		raw, err := store.Get(ctx, pyrepr.Str(previousResponseID))
		if err != nil {
			return nil, instructions, "", nil, err
		}
		if stored, ok := raw.(*pyjson.Object); ok && stored.Len() > 0 {
			if prior, ok := stored.GetOr("conversation_history", nil).([]any); ok {
				history = append(history, prior...)
			}
			storedSessionID = textOf(stored.GetOr("session_id", nil))
			if instructions == nil {
				instructions = stored.GetOr("instructions", nil)
			}
		}
	}
	if list, ok := rawInput.([]any); ok && len(history) == 0 && len(list) > 1 {
		for _, raw := range list[:len(list)-1] {
			msg, isObj := raw.(*pyjson.Object)
			if !isObj || !pyrepr.Truthy(msg.GetOr("role", nil)) || !pyrepr.Truthy(msg.GetOr("content", nil)) {
				continue
			}
			content, _ := msg.Get("content")
			if parts, isList := content.([]any); isList {
				var texts []string
				for _, p := range parts {
					if part, isPart := p.(*pyjson.Object); isPart {
						if t, _ := part.Get("type"); t == "text" {
							texts = append(texts, pyrepr.Str(part.GetOr("text", "")))
						}
					}
				}
				content = strings.Join(texts, " ")
			}
			role, _ := msg.Get("role")
			history = append(history, pyjson.ObjectOf("role", role, "content", pyrepr.Str(content)))
		}
	}
	return history, instructions, storedSessionID, nil, nil
}

// Chained history with the chain's live session, the walked past stored session when it moved
func (a *APIServerAdapter) resolveChainedHistory(ctx context.Context, body *pyjson.Object, rawInput any) ([]any, any, string, *gateway.WebResponse, string, error) {
	history, instructions, chainRoot, errResp, err := a.resolveConversationHistory(ctx, body, rawInput)
	if err != nil {
		return nil, nil, "", nil, "", err
	}
	stored := chainRoot
	if errResp == nil {
		if stored, errResp, err = continuedSession(ctx, a, chainRoot); err != nil {
			return nil, nil, "", nil, "", err
		}
	}
	walked := ""
	if stored != "" && stored != chainRoot {
		walked = chainRoot
	}
	return history, instructions, stored, errResp, walked, nil
}

// 202 admission answer, a replay flagged with Idempotency-Replayed
func acceptedResponse(runID, status, gatewaySessionKey string, replayed bool) *gateway.WebResponse {
	headers := map[string]string{}
	if replayed {
		headers["Idempotency-Replayed"] = "true"
	}
	if gatewaySessionKey != "" {
		headers["X-Hermes-Session-Key"] = gatewaySessionKey
	}
	return jsonResponse(pyjson.ObjectOf("run_id", runID, "status", status, "replayed", replayed), http.StatusAccepted, headers)
}

// 409 for a fingerprint conflict, else a replay of the admitted run
func (a *APIServerAdapter) replayOrConflict(ctx context.Context, request *gateway.WebRequest, outcome string, record *gateway.RunIdempotencyRecord, gatewaySessionKey string) (*gateway.WebResponse, error) {
	if outcome == "conflict" {
		return jsonError("Idempotency-Key was already used with a different request payload", "idempotency_key_conflict", http.StatusConflict), nil
	}
	originalID := ""
	if record.RunID != nil {
		originalID = *record.RunID
	}
	status, err := a.durableRunStatus(ctx, request, originalID)
	if err != nil {
		return nil, err
	}
	if status == nil {
		status, _ = record.Status.(*pyjson.Object)
	}
	value := "queued"
	if status != nil {
		if s, ok := status.Get("status"); ok {
			value = pyrepr.Str(s)
		}
	}
	return acceptedResponse(originalID, value, gatewaySessionKey, true), nil
}

// Drops a run from the given tables then releases its owner stamp, caller holds the adapter lock
func (a *APIServerAdapter) forgetRunLocked(runID string, drop ...func(string)) {
	for _, d := range drop {
		d(runID)
	}
	a.releaseRunOwnerIfForgottenLocked(runID)
}

// Retires a run's agent, task, approval and stop control state, caller holds the adapter lock
func (a *APIServerAdapter) retireLiveRunLocked(runID string) {
	a.forgetRunLocked(runID, a.forgetActiveRunAgentLocked, func(id string) { delete(a.activeRunTasks, id) },
		func(id string) { delete(a.runApprovalSessions, id) }, func(id string) { delete(a.stoppingRunIDs, id) },
		func(id string) { delete(a.shutdownInterruptedRunIDs, id) })
}

// Drops a run's SSE transport, caller holds the adapter lock
func (a *APIServerAdapter) dropRunTransportLocked(runID string) {
	a.forgetRunLocked(runID, func(id string) { delete(a.runStreams, id) }, func(id string) { delete(a.runStreamsCreated, id) })
}

// Live compression continuation tip of a client addressed session, the id itself when unresolvable
func (a *APIServerAdapter) resolveLiveSessionID(ctx context.Context, sessionID string) string {
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return sessionID
	}
	resolved, err := db.ResolveResumeSessionID(sessionID)
	if err != nil {
		apiLog().Debug(fmt.Sprintf("/v1/runs live-session resolve failed for %s", sessionID), "error", err)
		return sessionID
	}
	if resolved == "" {
		return sessionID
	}
	return resolved
}

// One background wake turn against a raw session id in process under the owning profile
func (a *APIServerAdapter) runInternalSessionTurn(ctx context.Context, sessionID, text, profile, notificationCategory string) error {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return &gateway.PyError{Kind: "ValueError", Msg: "run_internal_session_turn requires the owning profile"}
	}
	if notificationCategory == "" {
		notificationCategory = "result"
	}
	ctx = withRequestProfile(ctx, profile)
	attempts := 1 + len(wakeRetryDelaysSeconds)
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(wakeRetryDelaysSeconds[attempt-1] * float64(time.Second))):
			}
		}
		if a.drainingResponse() != nil {
			return fmt.Errorf("internal wake refused for session %s: the gateway is draining", sessionID)
		}
		if a.concurrencyLimitedResponse(ctx) != nil {
			lastErr = errors.New("internal wake deferred: the API server is at its concurrent-run cap")
			apiLog().Warn(fmt.Sprintf("%v; attempt %d/%d", lastErr, attempt+1, attempts))
			continue
		}
		resolved := a.resolveLiveSessionID(ctx, sessionID)
		session, errResp, err := a.getExistingSessionOr404(ctx, resolved)
		if err != nil {
			return err
		}
		if errResp != nil || session == nil {
			return fmt.Errorf("internal wake target session %s is not in the active profile store", pyrepr.Repr(resolved))
		}
		history := a.conversationHistoryForSession(ctx, resolved)
		route, overrides, routeErr := a.selectRequestRoute(ctx, pyjson.ObjectOf("model", a.modelName), resolved, "", a.modelName)
		if routeErr != nil {
			return fmt.Errorf("internal wake route conflict for session %s", pyrepr.Repr(resolved))
		}
		run := runAgentOptions{userMessage: text, conversationHistory: history, sessionID: resolved, route: route,
			requestedRuntime: pyjson.NewObject(), routeSource: "global", sessionHistoryDelivery: "1", notificationCategory: notificationCategory}
		run.requestedModel, _ = overrides["requested_model"].(string)
		run.requestedProvider, _ = overrides["requested_provider"].(string)
		if options, ok := overrides["model_options"]; ok {
			run.modelOptions = options
		}
		_, _, err = a.runAgent(ctx, run)
		return err
	}
	return fmt.Errorf("internal wake gave up for session %s after %d attempts: %v", sessionID, attempts, lastErr)
}

// Principal scope and request fingerprint of a keyed runs request, empty when unkeyed
func (a *APIServerAdapter) runIdempotencyIdentity(ctx context.Context, request *gateway.WebRequest, body *pyjson.Object, gatewaySessionKey, idempotencyKey string) (string, string, error) {
	if idempotencyKey == "" {
		return "", "", nil
	}
	encoded, err := pyjson.Dumps(pyjson.ObjectOf("body", body, "gateway_session_key", gatewaySessionKey),
		pyjson.SortKeys(true), pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return a.runIdempotencyScope(ctx, request), hex.EncodeToString(sum[:]), nil
}

// User message of a runs input, a 400 when missing or empty
func runUserMessage(rawInput any) (any, *gateway.WebResponse) {
	if !pyrepr.Truthy(rawInput) {
		return nil, jsonError("Missing 'input' field", "", http.StatusBadRequest)
	}
	var userMessage any = ""
	switch v := rawInput.(type) {
	case string:
		userMessage = v
	case []any:
		last, _ := v[len(v)-1].(*pyjson.Object)
		if last == nil {
			panic(&gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v[len(v)-1]))})
		}
		userMessage = last.GetOr("content", "")
	}
	if !pyrepr.Truthy(userMessage) {
		return nil, jsonError("No user message found in input", "", http.StatusBadRequest)
	}
	return userMessage, nil
}

// Expires old run transports and terminal statuses every minute
func (a *APIServerAdapter) sweepOrphanedRuns(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
		a.sweepOrphanedRunsOnce(apiNow())
	}
}

// Expires SSE buffers past their TTL without treating transport age as run age
func (a *APIServerAdapter) sweepOrphanedRunsOnce(now float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for runID, createdAt := range a.runStreamsCreated {
		stream := a.runStreams[runID]
		if now-createdAt <= runStreamTTL || (stream != nil && stream.subscriberCount() > 0) {
			continue
		}
		apiLog().Debug("[api_server] sweeping expired run transport " + runID)
		task := a.activeRunTasks[runID]
		a.dropRunTransportLocked(runID)
		if task == nil || task.Done() {
			unregisterApprovalNotify(a.runApprovalSessions[runID])
			a.retireLiveRunLocked(runID)
		}
	}
	for runID, status := range a.runStatuses {
		switch pyrepr.Str(status.GetOr("status", "")) {
		case "completed", "failed", "cancelled":
			if now-toFloat(firstTruthy(status.GetOr("updated_at", nil), int64(0))) > runStatusTTL {
				a.forgetRunLocked(runID, func(id string) { delete(a.runStatuses, id) }, func(id string) { delete(a.runIdempotencyIDs, id) })
			}
		}
	}
}

// Number of attached subscribers
func (s *runStream) subscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subscribers)
}

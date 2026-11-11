package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// 404 for a run this principal cannot see
func runNotFound(runID string) *gateway.WebResponse {
	return jsonError("Run not found: "+runID, "run_not_found", http.StatusNotFound)
}

// Owned run with its status, live agent and task, an error response when unauthenticated or not found
func (a *APIServerAdapter) loadOwnedRun(ctx context.Context, request *gateway.WebRequest, permission string, activeFallback bool) (string, *pyjson.Object, *agent.AIAgent, *agentTask, *gateway.WebResponse, error) {
	var authErr *gateway.WebResponse
	if permission != "" {
		authErr = a.checkRunAuth(ctx, request, permission)
	} else {
		authErr = a.checkAuth(ctx, request)
	}
	if authErr != nil {
		return "", nil, nil, nil, authErr, nil
	}
	runID := request.MatchInfo["run_id"]
	owns, err := a.requestOwnsRun(ctx, request, runID)
	if err != nil {
		return runID, nil, nil, nil, nil, err
	}
	if !owns {
		return runID, nil, nil, nil, runNotFound(runID), nil
	}
	a.mu.Lock()
	ag, task := a.activeRunAgents[runID], a.activeRunTasks[runID]
	a.mu.Unlock()
	status, err := a.durableRunStatus(ctx, request, runID)
	if err != nil {
		return runID, nil, nil, nil, nil, err
	}
	if status == nil && activeFallback && (ag != nil || task != nil) {
		status = a.setRunStatus(ctx, runID, "running")
	}
	if status == nil {
		return runID, nil, ag, task, runNotFound(runID), nil
	}
	return runID, status, ag, task, nil, nil
}

// Pollable run status for external UIs
func (a *APIServerAdapter) handleGetRun(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	_, status, _, _, errResp, err := a.loadOwnedRun(ctx, request, "status", true)
	if err != nil || errResp != nil {
		return errResp, err
	}
	return jsonResponse(status, http.StatusOK, nil), nil
}

// Streams a run's structured lifecycle events with replay from the last seen sequence
func (a *APIServerAdapter) handleRunEvents(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if authErr := a.checkRunAuth(ctx, request, "status"); authErr != nil {
		return authErr, nil
	}
	runID := request.MatchInfo["run_id"]
	owns, err := a.requestOwnsRun(ctx, request, runID)
	if err != nil {
		return nil, err
	}
	if !owns {
		return runNotFound(runID), nil
	}
	var stream *runStream
	for i := 0; i < 20; i++ {
		a.mu.Lock()
		stream = a.runStreams[runID]
		a.mu.Unlock()
		if stream != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stream == nil {
		return runNotFound(runID), nil
	}
	rawLastSeq := request.Header.Get("Last-Event-ID")
	hasRaw := rawLastSeq != ""
	if !hasRaw {
		if v, ok := request.Query()["last_seq"]; ok && len(v) > 0 {
			rawLastSeq, hasRaw = v[0], true
		}
	}
	lastSeq := int64(-1)
	if hasRaw {
		if n, err := strconv.ParseInt(strings.TrimSpace(rawLastSeq), 10, 64); err == nil {
			lastSeq = max(-1, n)
		}
	}
	sub, replay := stream.attach(lastSeq)
	defer func() {
		stream.detach(sub)
		a.releaseRunOwnerIfForgotten(runID)
	}()
	response := gateway.NewWebStreamResponse(http.StatusOK, a.sseHeaders(request, nil))
	write := func(data []byte) error {
		done := make(chan error, 1)
		go func() { done <- response.Write(data) }()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Duration(runStreamWriteTimeout * float64(time.Second))):
			return &gateway.PyError{Kind: "TimeoutError", Msg: "run event write timed out"}
		}
	}
	writeEvent := func(seq int64, event *pyjson.Object) error {
		payload := event.Copy()
		payload.Set("seq", seq)
		id := int(seq)
		frame, err := gateway.APISSEFrame(payload, "", true, &id)
		if err != nil {
			return err
		}
		return write(frame)
	}
	if err := response.Prepare(request); err != nil {
		return nil, err
	}
	streamErr := func() error {
		if err := write([]byte(": open\n\n")); err != nil {
			return err
		}
		if len(replay) > 0 && replay[0].seq > lastSeq+1 {
			truncation := runEvent(runID, "replay.truncated", "oldest_retained_seq", replay[0].seq)
			if hasRaw {
				truncation.Set("requested_seq", lastSeq)
			}
			frame, err := gateway.APISSEFrame(truncation, "", true, nil)
			if err != nil {
				return err
			}
			if err := write(frame); err != nil {
				return err
			}
		}
		for _, it := range replay {
			if it.event == nil {
				return write([]byte(": stream closed\n\n"))
			}
			if err := writeEvent(it.seq, it.event); err != nil {
				return err
			}
		}
		stream.mu.Lock()
		terminal := stream.terminal
		stream.mu.Unlock()
		if terminal && len(sub.items) == 0 {
			return write([]byte(": stream closed\n\n"))
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case it := <-sub.items:
				if it.overflow {
					apiLog().Debug("[api_server] closing slow SSE subscriber for run " + runID)
					return nil
				}
				if it.event == nil {
					return write([]byte(": stream closed\n\n"))
				}
				if err := writeEvent(it.seq, it.event); err != nil {
					return err
				}
			case <-time.After(time.Duration(chatCompletionsSSEKeepaliveSeconds * float64(time.Second))):
				if err := write([]byte(": keepalive\n\n")); err != nil {
					return err
				}
			}
		}
	}()
	if streamErr != nil {
		apiLog().Debug(fmt.Sprintf("[api_server] SSE stream error for run %s: %v", runID, streamErr))
	}
	return response.WebResponse, nil
}

// Records a control plane event on the run status and its SSE stream
func (a *APIServerAdapter) markRunEvent(ctx context.Context, runID, name string, fields ...any) {
	a.setRunStatus(ctx, runID, "running", "last_event", name)
	a.mu.Lock()
	stream := a.runStreams[runID]
	a.mu.Unlock()
	if stream != nil {
		func() {
			defer func() { _ = recover() }()
			stream.putNowait(runEvent(runID, name, fields...))
		}()
	}
}

var approvalChoiceAliases = map[string]string{"approve": "once", "approved": "once", "allow": "once"}

// Resolves a pending run approval
func (a *APIServerAdapter) handleRunApproval(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	runID, _, _, _, errResp, err := a.loadOwnedRun(ctx, request, "approve", false)
	if err != nil || errResp != nil {
		return errResp, err
	}
	raw, err := requestJSON(request)
	if err != nil {
		return jsonError("Invalid JSON", "", http.StatusBadRequest), nil
	}
	body, _ := raw.(*pyjson.Object)
	if body == nil {
		return nil, &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
	}
	if _, ok := a.sessionAuthority(); ok {
		if choice := body.GetOr("choice", nil); roomGrantToken(request) != "" && choice != "once" && choice != "deny" {
			return jsonError("Room approvals require once or deny", "", http.StatusBadRequest), nil
		}
		return a.respondAuthorityRun(ctx, runID, body, "approval")
	}
	rawChoice := strings.ToLower(strings.TrimSpace(pyrepr.Str(body.GetOr("choice", ""))))
	choice := rawChoice
	if alias, ok := approvalChoiceAliases[rawChoice]; ok {
		choice = alias
	}
	roomScoped := roomGrantToken(request) != ""
	rawRequestID, hasRequestID := body.Get("request_id")
	hasRequestID = hasRequestID && rawRequestID != nil
	requestID := ""
	if s, ok := rawRequestID.(string); ok {
		requestID = strings.TrimSpace(s)
	}
	allowed := map[string]bool{"once": true, "session": true, "always": true, "deny": true}
	if roomScoped {
		allowed = map[string]bool{"once": true, "deny": true}
	}
	resolveAll := gateway.APICoerceRequestBool(body.GetOr("all", nil), false) || gateway.APICoerceRequestBool(body.GetOr("resolve_all", nil), false)
	a.mu.Lock()
	approvalSessionKey := a.runApprovalSessions[runID]
	a.mu.Unlock()
	allowedNames := make([]string, 0, len(allowed))
	for k := range allowed {
		allowedNames = append(allowedNames, k)
	}
	sort.Strings(allowedNames)
	checks := []struct {
		failed        bool
		message, code string
		status        int
	}{
		{hasRequestID && (requestID == "" || len([]rune(requestID)) > 256), "Approval request_id is invalid.", "invalid_approval_request", 400},
		{!allowed[choice], "Invalid approval choice; expected one of: " + strings.Join(allowedNames, ", "), "invalid_approval_choice", 400},
		{roomScoped && resolveAll, "Room approvals can resolve only one exact request", "invalid_approval_scope", 400},
		{roomScoped && requestID == "", "Room approvals require the exact request_id.", "approval_request_required", 400},
		{approvalSessionKey == "", "Run has no active approval session: " + runID, "approval_not_active", 409},
	}
	for _, c := range checks {
		if c.failed {
			return jsonError(c.message, c.code, c.status), nil
		}
	}
	resolved, err := func() (n int, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return tools.ResolveGatewayApproval(approvalSessionKey, choice, resolveAll, "", requestID), nil
	}()
	if err != nil {
		apiLog().Error(fmt.Sprintf("[api_server] approval resolution failed for run %s", runID), "error", err)
		return jsonError(err.Error(), "", http.StatusInternalServerError), nil
	}
	if resolved <= 0 {
		return jsonError("Run has no pending approval: "+runID, "approval_not_pending", http.StatusConflict), nil
	}
	fields := []any{"choice", choice}
	if requestID != "" {
		fields = append(fields, "request_id", requestID)
	}
	fields = append(fields, "resolved", int64(resolved))
	a.markRunEvent(ctx, runID, "approval.responded", fields...)
	out := pyjson.ObjectOf("object", "hermes.run.approval_response", "run_id", runID)
	for i := 0; i+1 < len(fields); i += 2 {
		out.Set(fields[i].(string), fields[i+1])
	}
	return jsonResponse(out, http.StatusOK, nil), nil
}

// Answers an authority owned run's prompt through the session authority
func (a *APIServerAdapter) respondAuthorityRun(ctx context.Context, runID string, body *pyjson.Object, kind string) (*gateway.WebResponse, error) {
	result, err := respondRun(ctx, a, runID, body, kind)
	if err != nil {
		var runtimeErr *state.RuntimeStoreError
		if errors.As(err, &runtimeErr) {
			return storeErrorResponse(err), nil
		}
		return nil, err
	}
	out := pyjson.ObjectOf("run_id", runID)
	out.Update(result)
	return jsonResponse(out, http.StatusOK, nil), nil
}

// Answers an authority run's clarify prompt
func (a *APIServerAdapter) handleRunClarify(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	runID, _, _, _, errResp, err := a.loadOwnedRun(ctx, request, "approve", false)
	if err != nil || errResp != nil {
		return errResp, err
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	return a.respondAuthorityRun(ctx, runID, body, "clarify")
}

// 409 for a run that does not take steer input now
func steerRefused(runID string) *gateway.WebResponse {
	return jsonError("Run is not currently accepting steer input: "+runID, "run_not_accepting_steer", http.StatusConflict)
}

// Injects guidance into a running agent, an authority run steered through its owning session
func (a *APIServerAdapter) handleSteerRun(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	runID, status, ag, _, errResp, err := a.loadOwnedRun(ctx, request, "", false)
	if err != nil || errResp != nil {
		return errResp, err
	}
	var owned *authorityRunAdmission
	if _, ok := a.sessionAuthority(); ok {
		if owned, err = runAdmission(ctx, a, runID); err != nil {
			return nil, err
		}
		a.mu.Lock()
		if a.stoppingRunIDs[runID] {
			owned = nil
		}
		a.mu.Unlock()
		ag = nil
	}
	if s, _ := status.Get("status"); s != "running" || (owned == nil && ag == nil) {
		return steerRefused(runID), nil
	}
	body, errResp := readJSONBody(request)
	if errResp != nil {
		return errResp, nil
	}
	rawText := firstTruthy(body.GetOr("input", nil), body.GetOr("message", nil), body.GetOr("text", nil), "")
	steerText := strings.TrimSpace(gateway.APINormalizeChatContent(rawText))
	if steerText == "" {
		return jsonError("Missing non-empty steer text; expected 'input', 'message', or 'text'.", "invalid_steer_input", http.StatusBadRequest), nil
	}
	if owned != nil {
		steered, latched, err := steerAuthorityRun(ctx, owned, steerText)
		if err != nil {
			return nil, err
		}
		if !latched && steered == nil {
			return steerRefused(runID), nil
		}
		if latched {
			a.markRunEvent(ctx, runID, "run.steered", "accepted", true)
			return jsonResponse(pyjson.ObjectOf("object", "hermes.run.steer", "run_id", runID, "accepted", true), http.StatusOK, nil), nil
		}
		ag = steered
	}
	accepted, err := func() (ok bool, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return ag.Steer(steerText), nil
	}()
	if err != nil {
		apiLog().Error(fmt.Sprintf("[api_server] steer failed for run %s", runID), "error", err)
		return jsonError(redactAPIErrorText(err.Error(), -1), "steer_failed", http.StatusInternalServerError), nil
	}
	if !accepted {
		return jsonError("Run did not accept steer text: "+runID, "steer_not_accepted", http.StatusConflict), nil
	}
	a.markRunEvent(ctx, runID, "run.steered", "accepted", true)
	return jsonResponse(pyjson.ObjectOf("object", "hermes.run.steer", "run_id", runID, "accepted", true), http.StatusOK, nil), nil
}

// Interrupts a running agent, an authority run stopped through the session authority
func (a *APIServerAdapter) handleStopRun(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	runID, status, ag, task, errResp, err := a.loadOwnedRun(ctx, request, "stop", true)
	if err != nil || errResp != nil {
		return errResp, err
	}
	if _, ok := a.sessionAuthority(); ok && !(task != nil && pyrepr.Truthy(status.GetOr("delivery_id", nil))) {
		result, err := stopRun(ctx, a, runID)
		if err != nil {
			var runtimeErr *state.RuntimeStoreError
			if errors.As(err, &runtimeErr) {
				return storeErrorResponse(err), nil
			}
			return nil, err
		}
		return jsonResponse(result, http.StatusOK, nil), nil
	}
	if gateway.APIRunTerminalStatus(pyrepr.Str(status.GetOr("status", ""))) {
		return jsonResponse(status, http.StatusOK, nil), nil
	}
	if ag == nil && task == nil {
		return jsonError("Run is not active in this gateway process: "+runID, "run_not_active", http.StatusConflict), nil
	}
	a.setRunStatus(ctx, runID, "stopping", "last_event", "run.stopping")
	a.mu.Lock()
	a.stoppingRunIDs[runID] = true
	a.mu.Unlock()
	if ag != nil {
		func() {
			defer func() { _ = recover() }()
			agent.RequestHardInterrupt(ag, "Stop requested via API", "")
		}()
		reapDisconnectedAgentProcesses(ctx, ag, "api_server_run_stop")
	}
	return jsonResponse(pyjson.ObjectOf("run_id", runID, "status", "stopping"), http.StatusOK, nil), nil
}

// Acknowledges one lost owner epoch of an authority run
func (a *APIServerAdapter) handleResolveUnknownRun(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	runID, _, _, _, errResp, err := a.loadOwnedRun(ctx, request, "stop", false)
	if err != nil || errResp != nil {
		return errResp, err
	}
	var body any
	if raw, err := requestJSON(request); err == nil {
		body = raw
	}
	result, err := resolveUnknownRun(ctx, a, runID, body)
	if err != nil {
		var runtimeErr *state.RuntimeStoreError
		if errors.As(err, &runtimeErr) {
			return storeErrorResponse(err), nil
		}
		return nil, err
	}
	out := pyjson.ObjectOf("run_id", runID)
	out.Update(result)
	return jsonResponse(out, http.StatusOK, nil), nil
}

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Server request methods a modal of the CLI answers in the response frame
var gatewayServerRequests = map[string]func(a *GatewayAgent, params map[string]any) any{
	"sudo":                func(a *GatewayAgent, p map[string]any) any { return a.askSudo(p) },
	"secret":              func(a *GatewayAgent, p map[string]any) any { return a.askSecret(p) },
	"vault.unlock_prompt": func(a *GatewayAgent, p map[string]any) any { return a.askVaultUnlock(p) },
	"vault.save_login":    func(a *GatewayAgent, p map[string]any) any { return a.askVaultSaveLogin(p) },
	"vault.code":          func(a *GatewayAgent, p map[string]any) any { return a.askVaultCode(p) },
	"clarify":             func(a *GatewayAgent, p map[string]any) any { return a.askClarify(p) },
}

// Submit parameters of one prompt, consuming a one turn model pick
func (a *GatewayAgent) submitParams(userMessage, persistUserMessage any, turnAuthor map[string]any) (map[string]any, error) {
	var attachments []any
	if parts, isList := userMessage.([]any); isList {
		text, staged, err := a.stageContentParts(parts)
		if err != nil {
			return nil, err
		}
		userMessage, attachments = text, staged
		if _, persistList := persistUserMessage.([]any); persistList {
			persistUserMessage = userMessage
		}
	}
	text := userMessage
	if s, isStr := persistUserMessage.(string); isStr {
		text = s
	}
	a.mu.Lock()
	params := map[string]any{"session_id": a.sessionID, "submission_id": uuidHex(), "text": text}
	if len(attachments) > 0 {
		params["attachments"] = attachments
	}
	if s, isStr := userMessage.(string); isStr && !pyjson.Equal(s, text) {
		params["api_text"] = s
	}
	if len(turnAuthor) > 0 {
		params["turn_author"] = turnAuthor
	}
	if a.voiceTurnPending {
		params["voice_turn"] = true
	}
	if staged := a.pendingCLIUserMessage; staged != nil && pyrepr.Truthy(staged["display_kind"]) {
		params["display_kind"] = staged["display_kind"]
	}
	if a.finite {
		params["finite"] = true
	}
	moaPreset := a.moaPreset
	a.mu.Unlock()
	if a.opts.ClarifyCallback != nil {
		params["clarify_batch"] = true
	}
	info := a.stateInfo()
	if pending := a.takeTurnRoute(); pending != nil {
		params["model_override"] = pending
	} else if moaPreset != "" && !(pyjson.Equal(info["provider"], "moa") && pyjson.Equal(info["model"], moaPreset)) {
		params["model_override"] = map[string]any{"model": moaPreset, "provider": "moa"}
	}
	return params, nil
}

// One turn model pick for the admission being built, consumed so a single admission carries it
func (a *GatewayAgent) takeTurnRoute() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, reasoning := a.turnRoute, a.turnReasoning
	a.turnRoute, a.turnReasoning, a.takenTurnRoute = nil, "", nil
	if pending == nil {
		return nil
	}
	a.takenTurnRoute = &gatewayTakenRoute{route: pending, reasoning: reasoning}
	out := copyMap(pending)
	if reasoning != "" {
		out["reasoning"] = reasoning
	}
	return out
}

// Puts back a taken one turn pick when the admission it rode was never admitted
func (a *GatewayAgent) giveBackTurnRoute() {
	a.mu.Lock()
	defer a.mu.Unlock()
	taken := a.takenTurnRoute
	a.takenTurnRoute = nil
	if taken != nil && a.turnRoute == nil {
		a.turnRoute, a.turnReasoning = taken.route, taken.reasoning
	}
}

// Text and owner staged attachments of native content parts, the local image hints dropped
func (a *GatewayAgent) stageContentParts(parts []any) (string, []any, error) {
	var texts []string
	var attachments []any
	for _, raw := range parts {
		part, isDict := pyjson.Plain(raw).(map[string]any)
		if !isDict {
			continue
		}
		switch part["type"] {
		case "text":
			var kept []string
			text := ""
			if pyrepr.Truthy(part["text"]) {
				text = pyrepr.Str(part["text"])
			}
			for _, line := range pystr.Splitlines(text, false) {
				if !strings.HasPrefix(line, "[Image attached") {
					kept = append(kept, line)
				}
			}
			texts = append(texts, pystr.Strip(strings.Join(kept, "\n")))
		case "image_url":
			image, _ := part["image_url"].(map[string]any)
			url, isStr := image["url"].(string)
			if isStr && strings.HasPrefix(url, "data:image/") {
				raw, err := a.connection.Call(a.ctx, "image.attach_bytes", map[string]any{"session_id": a.SessionID(), "content_base64": url})
				if err != nil {
					return "", nil, err
				}
				staged, _ := pyjson.Plain(raw).(map[string]any)
				attachments = append(attachments, map[string]any{"path": requireKey(staged, "path"), "mime": requireKey(staged, "mime")})
			}
		}
	}
	var nonEmpty []string
	for _, text := range texts {
		if text != "" {
			nonEmpty = append(nonEmpty, text)
		}
	}
	return strings.Join(nonEmpty, "\n\n"), attachments, nil
}

// Submits the prompt and replays the gateway's events into the CLI callbacks, returning the turn result
func (a *GatewayAgent) RunConversation(ctx context.Context, userMessage any, o agent.RunConversationOptions) (result map[string]any, err error) {
	defer func() {
		if ctx.Err() != nil {
			result, err = nil, context.Cause(ctx)
		}
	}()
	turn := newGatewayTurn(a.SessionID())
	history := append([]map[string]any{}, o.ConversationHistory...)
	askIndex := len(history)
	a.mu.Lock()
	a.turn = turn
	a.turnCtx = ctx
	a.turnAskIndex = &askIndex
	a.turnAPICalls = nil
	a.turnCallsBase = a.counters["session_api_calls"]
	a.usageStale = true
	a.interruptRequested = false
	a.interruptSentAt = nil
	a.mu.Unlock()
	modelText := userMessage
	if s, isStr := o.PersistUserMessage.(string); isStr {
		modelText = s
	}
	adopted := takeAdmittedTurn(a, modelText)
	var receipt map[string]any
	if adopted != nil {
		receipt = adopted
		turn.gapped = pyrepr.Truthy(adopted["recovered"])
	} else {
		a.drainStale()
		a.mu.Lock()
		a.takenTurnRoute = nil
		a.mu.Unlock()
		raw, failure, err := a.submit(ctx, turn, userMessage, o)
		if err != nil {
			return nil, err
		}
		if failure != nil {
			return failure(history), nil
		}
		receipt, _ = pyjson.Plain(raw).(map[string]any)
		if receipt["voice_stopped"] == true {
			return a.voiceStopped(history), nil
		}
	}
	a.mu.Lock()
	a.epoch = a.connection.Epoch()
	turn.admission = receipt["admission_id"]
	a.admission = turn.admission
	if generation, has := receipt["execution_generation"]; has && generation != nil {
		a.generation = generation
	}
	a.mu.Unlock()
	starts := map[string]time.Time{}
	var outcome any
	func() {
		defer func() {
			if a.owns(turn) {
				a.generationDone()
			}
		}()
		if adopted != nil && adopted["receipt"] != nil && pyrepr.Truthy(adopted["receipt"]) {
			outcome = map[string]any{"_receipt": adopted["receipt"]}
		} else {
			outcome = a.pump(ctx, o.StreamCallback, starts, turn)
		}
	}()
	if lost, isErr := outcome.(error); isErr && a.owns(turn) {
		return a.failed(history, userMessage, firstNonEmpty(lost.Error(), gateway.GatewayDisconnected), "gateway_connection", false), nil
	}
	complete, _ := outcome.(map[string]any)
	if a.owns(turn) {
		result = a.result(ctx, history, userMessage, complete, turn)
	}
	if !a.owns(turn) {
		return a.failed(history, userMessage, "A later turn took over this terminal.", "", false), nil
	}
	if turn.gapped {
		a.paintOwed(turn, result, complete, o.StreamCallback)
	}
	return result, nil
}

// Sends prompt.submit, returning the receipt or the result builder of a refused or unsent submit
func (a *GatewayAgent) submit(ctx context.Context, turn *gatewayTurn, userMessage any, o agent.RunConversationOptions) (any, func([]map[string]any) map[string]any, error) {
	raw, err := func() (any, error) {
		notice, err := a.prepareSubmit(ctx, turn)
		if err != nil {
			return nil, err
		}
		if notice != "" {
			return nil, &gatewayUnadmittedNotice{notice}
		}
		params, err := a.submitParams(userMessage, o.PersistUserMessage, o.TurnAuthor)
		if err != nil {
			return nil, err
		}
		if a.stop(turn)() {
			return nil, &gatewaycli.CallStopped{Method: "prompt.submit"}
		}
		params["session_id"] = turn.sessionID
		return a.connection.Call(ctx, "prompt.submit", params)
	}()
	if err == nil {
		return raw, nil, nil
	}
	var unadmitted *gatewayUnadmittedNotice
	var stopped *gatewaycli.CallStopped
	var refused *gateway.GatewayRPCError
	switch {
	case errors.As(err, &unadmitted):
		return nil, func(history []map[string]any) map[string]any {
			return a.unadmitted(history, userMessage, unadmitted.notice)
		}, nil
	case errors.As(err, &stopped):
		a.giveBackTurnRoute()
		return nil, func(history []map[string]any) map[string]any {
			return a.failed(history, userMessage, "Stopped before the prompt was sent; nothing was sent.", "", false)
		}, nil
	case errors.As(err, &refused):
		busy := refused.Error() == "session_busy"
		reason := ""
		if busy {
			a.giveBackTurnRoute()
			reason = "session_busy"
		}
		return nil, func(history []map[string]any) map[string]any {
			return a.failed(history, userMessage, refused.Error(), reason, busy)
		}, nil
	case isGatewayClientError(err):
		if err.Error() == gateway.GatewayNotSent {
			return nil, func(history []map[string]any) map[string]any { return a.unadmitted(history, userMessage, err.Error()) }, nil
		}
		return nil, func(history []map[string]any) map[string]any {
			return a.failed(history, userMessage, err.Error(), "gateway_connection", false)
		}, nil
	}
	return nil, nil, err
}

// Pre submit notice of a turn lost in a gateway restart, the message not sent
type gatewayUnadmittedNotice struct{ notice string }

func (e *gatewayUnadmittedNotice) Error() string { return e.notice }

// Drops frames left from an earlier turn, remembering a queued admission's start among them
func (a *GatewayAgent) drainStale() {
	events := a.connection.Events
	for {
		frame, ok := events.GetNowait()
		if !ok {
			return
		}
		if _, isErr := frame.(error); isErr {
			if gatewaycli.LostBefore(frame, a.connection.Epoch()) {
				continue
			}
			events.PutNowait(frame)
			return
		}
		params := frameParams(frame)
		if params != nil && params["session_id"] == a.SessionID() {
			missedStart(a.state(), params)
		}
	}
}

// Params dict of a frame, nil when the frame carries none
func frameParams(frame any) map[string]any {
	obj, ok := pyjson.AsObject(frame)
	if !ok {
		return nil
	}
	params, _ := pyjson.Plain(obj.Vals["params"]).(map[string]any)
	return params
}

func (a *GatewayAgent) generationDone() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generation = nil
}

// Whether the settle bound after an interrupt has passed
func (a *GatewayAgent) expired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interruptSentAt != nil && time.Since(*a.interruptSentAt).Seconds() > gatewayInterruptSettleSeconds
}

// Whether turn is still this agent's turn on its own session
func (a *GatewayAgent) owns(turn *gatewayTurn) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turn == turn && a.sessionID == turn.sessionID
}

// Give up poll of a turn's blocking reads, true once it lost the terminal or the settle bound passed
func (a *GatewayAgent) stop(turn *gatewayTurn) func() bool {
	return func() bool { return !a.owns(turn) || a.expired() }
}

// Give up poll of a turn's end of turn reads, true only once it lost the terminal
func (a *GatewayAgent) lost(turn *gatewayTurn) func() bool {
	return func() bool { return !a.owns(turn) }
}

// Reattaches after a replay gap, restarts the connection and returns the notice of a turn lost in a restart
func (a *GatewayAgent) prepareSubmit(ctx context.Context, turn *gatewayTurn) (string, error) {
	state := a.state()
	state.Lock()
	gapped := state.Gaps != state.AttachedGaps
	state.Unlock()
	if gapped {
		if _, err := a.connection.Reattach(ctx, turn.sessionID, a.stop(turn)); err != nil {
			return "", err
		}
	}
	if err := a.connection.Start(ctx, gatewaycli.DefaultStartTimeout); err != nil {
		return "", err
	}
	a.syncShowReasoning()
	if notice := a.lostTurnNotice(); notice != "" {
		return "Your message was not sent.\n" + notice, nil
	}
	return "", nil
}

// Pending lines of the session when one of its admissions was lost in a gateway restart, else empty
func (a *GatewayAgent) lostTurnNotice() string {
	state := a.state()
	state.Lock()
	pending := append([]any{}, state.Pending...)
	state.Unlock()
	for _, raw := range pending {
		row, _ := raw.(map[string]any)
		if requireKey(row, "status") == "unknown" {
			return strings.Join(gatewayPendingLines(pending, true), "\n")
		}
	}
	return ""
}

// Dispatches frames until this admission's message.complete, returning its payload or the transport error
func (a *GatewayAgent) pump(ctx context.Context, streamCallback func(string), starts map[string]time.Time, turn *gatewayTurn) any {
	events := a.connection.Events
	for a.owns(turn) {
		if a.expired() {
			return map[string]any{"status": "interrupted", "outcome": "cancelled", "_settle_timeout": true}
		}
		waitCtx, cancel := context.WithTimeout(ctx, gatewayAgentPoll)
		frame, err := events.Get(waitCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if !a.owns(turn) {
			events.PutFirst(frame)
			break
		}
		if failure, isErr := frame.(error); isErr {
			a.mu.Lock()
			epoch := a.epoch
			a.mu.Unlock()
			if gatewaycli.LostBefore(frame, epoch) {
				continue
			}
			return failure
		}
		params := frameParams(frame)
		if params == nil || params["session_id"] != a.SessionID() {
			continue
		}
		obj, _ := pyjson.AsObject(frame)
		method, _ := obj.Vals["method"].(string)
		if _, isRequest := gatewayServerRequests[method]; isRequest {
			if _, hasID := obj.Get("id"); hasID {
				a.answerRequest(obj, method, params)
				continue
			}
		}
		kind, _ := params["type"].(string)
		payload, _ := params["payload"].(map[string]any)
		if payload == nil {
			payload = map[string]any{}
		}
		admission := params["admission_id"]
		if !pyrepr.Truthy(admission) {
			admission = payload["admission_id"]
		}
		a.mu.Lock()
		current := a.admission
		a.mu.Unlock()
		if pyrepr.Truthy(current) && pyrepr.Truthy(admission) && !pyjson.Equal(admission, current) {
			missedStart(a.state(), params)
			continue
		}
		if generation, has := params["execution_generation"]; has && generation != nil {
			a.mu.Lock()
			a.generation = generation
			a.mu.Unlock()
			state := a.state()
			state.Lock()
			state.ExecutionGeneration = generation
			state.Unlock()
		}
		if kind == "message.complete" {
			return payload
		}
		if kind == "session.replay_gap" {
			if outcome := a.recover(ctx, turn); outcome != nil {
				return outcome
			}
			continue
		}
		a.dispatchSafely(kind, payload, streamCallback, starts)
	}
	return nil
}

// Dispatches one event, a failing handler logged and never ending the turn
func (a *GatewayAgent) dispatchSafely(kind string, payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Debug(fmt.Sprintf("event %s dispatch failed: %v", kind, recovered))
		}
	}()
	a.dispatch(kind, payload, streamCallback, starts)
}

// Reattaches after a replay gap, answers controls the hole hid and settles a turn the snapshot no longer lists
func (a *GatewayAgent) recover(ctx context.Context, turn *gatewayTurn) any {
	lost := &gateway.GatewayClientError{Msg: "session_replay_gap: " + gateway.GatewayDisconnected}
	stop := a.stop(turn)
	raw, err := a.connection.Reattach(ctx, turn.sessionID, stop)
	var receipt map[string]any
	if err == nil {
		snapshot, _ := pyjson.Plain(raw).(map[string]any)
		row := map[string]any{}
		pending, _ := snapshot["pending"].([]any)
		for _, candidate := range pending {
			if r, _ := candidate.(map[string]any); r != nil && pyjson.Equal(r["admission_id"], turn.admission) {
				row = r
				break
			}
		}
		if row["status"] == "unknown" {
			return lost
		}
		turn.gapped = turn.gapped || row["status"] != "queued"
		pendingConnection, _ := snapshot["pending_connection"].(map[string]any)
		a.restoreConnectionCard(pendingConnection)
		prompts, _ := snapshot["prompts"].([]any)
		for _, rawPrompt := range prompts {
			prompt, _ := rawPrompt.(map[string]any)
			if row["status"] == "started" && pyjson.Equal(prompt["execution_generation"], row["execution_generation"]) &&
				!turn.answered[prompt["prompt_id"]] && !stop() {
				turn.answered[prompt["prompt_id"]] = true
				a.dispatchSafely(pyrepr.Str(prompt["kind"])+".request", prompt, nil, map[string]time.Time{})
			}
		}
		if len(row) > 0 || !pyrepr.Truthy(turn.admission) || stop() {
			return nil
		}
		var got any
		got, err = a.connection.CallWith(ctx, "prompt.receipt", map[string]any{"session_id": turn.sessionID, "admission_id": turn.admission,
			"include_result": true}, gatewaycli.CallOptions{Abandon: stop})
		receipt, _ = pyjson.Plain(got).(map[string]any)
	}
	var stopped *gatewaycli.CallStopped
	if errors.As(err, &stopped) {
		return nil
	}
	if err != nil {
		if isGatewayClientError(err) {
			return lost
		}
		panic(err)
	}
	if receipt == nil {
		receipt = map[string]any{}
	}
	verdict := replayGapSettled(receipt)
	switch {
	case verdict == nil:
		return lost
	case *verdict:
		return map[string]any{"_receipt": receipt}
	}
	return nil
}

package tui

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	goalStatusHold   = 6000 * time.Millisecond
	statusNoteHold   = 4000 * time.Millisecond
	protocolPreview  = 120
	thinkingTailSize = 6
	notesTailSize    = 6
	toolsTailSize    = 8
)

var noProviderRe = regexp.MustCompile(`(?i)\bNo (?:LLM|inference) provider configured\b`)

// Records the admission whose completion this view saw last for a session
func (s *tuiStore) noteCanonicalCompletion(sid string, admissionID any) {
	id, isStr := admissionID.(string)
	if sid == "" || !isStr || id == "" {
		return
	}
	if s.lastAdmission == nil {
		s.lastAdmission = map[string]string{}
	}
	s.lastAdmission[sid] = id
}

// Records an explicit wake off so a reconnect does not arm the listener again
func (s *tuiStore) setWakeUserDisabled(disabled bool) { s.wakeUserDisabled = disabled }

// List with x appended unless it is already last, keeping the newest max entries
func pushUnique(xs []string, x string, max int) []string {
	if len(xs) > 0 && xs[len(xs)-1] == x {
		return xs
	}
	next := append(slices.Clone(xs), x)
	if len(next) > max {
		next = next[len(next)-max:]
	}
	return next
}

// Whether a subagent status is final
func isTerminalStatus(s string) bool {
	return s == "completed" || s == "error" || s == "failed" || s == "interrupted" || s == "timeout"
}

// Final status kept, anything else running
func keepTerminalElseRunning(s string) string {
	if isTerminalStatus(s) {
		return s
	}
	return "running"
}

// Labels of a tool event, nil when absent
func toolLabelsOf(v any) []toolLabel {
	list, isList := v.([]any)
	if !isList {
		return nil
	}
	out := make([]toolLabel, 0, len(list))
	for _, raw := range list {
		l, _ := raw.(map[string]any)
		out = append(out, toolLabel{Emoji: paramStr(l["emoji"]), Text: paramStr(l["text"]), Preview: paramStr(l["preview"])})
	}
	return out
}

// Number of a payload field, nil when it is no number
func optionalNumber(p map[string]any, k string) *float64 {
	if n, isNum := p[k].(float64); isNum {
		return &n
	}
	return nil
}

// Text of a truthy payload value with ANSI escapes removed, empty otherwise
func strippedText(v any) string {
	if !truthyJS(v) {
		return ""
	}
	return stripAnsi(jsValueString(v))
}

// Dispatches one gateway event to the store, the turn controller and the app
func (h *eventHandler) handle(ev gatewayEventMsg) {
	sid := h.store.ui.Sid
	if ev.SessionID != "" && sid != "" && ev.SessionID != sid && !strings.HasPrefix(ev.Type, "gateway.") {
		return
	}
	current := h.store.ui.Info
	genericError := isGenericErrorEvent(ev)
	if h.store.fenceLifecycleEvent(ev, genericError) {
		return
	}
	if h.store.settleSharedPrompt(ev) || h.refreshSlashCatalogOnSkillsChange(ev) {
		return
	}
	if h.store.handleVoiceCapture(ev, h.app) {
		return
	}
	p := ev.Payload
	switch ev.Type {
	case "connection.request":
		if p != nil {
			h.store.applyConnectionRequest(p)
		}
	case "connection.update":
		if p != nil {
			for _, line := range h.store.applyConnectionUpdate(p) {
				h.app.sys(line)
			}
		}
	case "gateway.ready":
		skin, _ := p["skin"].(map[string]any)
		h.handleReady(gatewaySkinOf(skin))
	case "skin.changed":
		if p != nil {
			h.queue(h.store.applySkin(gatewaySkinOf(p)))
		}
	case "free_tier.challenge":
		h.showChallenge(p)
	case "session.info":
		h.applySessionInfo(p)
	case "session.updated":
		h.applyDeferredPickOutcome(p, sid)
	case "session.replay_gap":
		if sid != "" {
			h.queue(h.app.resumeByID(sid, nil))
		}
	case "session.usage":
		if usage := usageFromTick(p); usage != nil {
			h.store.ui.Usage = spreadUsage(h.store.ui.Usage, usage)
		}
	case "thinking.delta":
		h.thinkingDelta(p)
	case "session.control.update":
		control, _ := p["control"].(map[string]any)
		h.store.applyControlSnapshot(sid, control)
	case "message.start":
		h.agentsNudgedThisTurn = false
		h.turn.startMessage()
	case "status.update":
		h.statusUpdate(p)
	case "notification.show":
		h.showGatewayNotice(p)
	case "notification.clear":
		h.turn.clearNotice(paramStr(p["key"]))
	case "billing.step_up.verification":
		h.showBillingVerification(p)
	case "gateway.stderr":
		if p != nil {
			if line := jsTemplate(p, "line"); stderrLooksLikeProblem(line) {
				h.turn.pushActivity(stderrProblemActivity(line), "warn", "")
			}
		}
	case "gateway.reconnecting":
		attempt, _ := p["attempt"].(float64)
		h.setStatus(backendReconnecting(int(attempt), optionalNumber(p, "delay_ms")))
	case "browser.progress":
		if message := strings.TrimFunc(stringOr(p["message"], ""), isJSSpace); message != "" {
			h.app.sys(message)
		}
	case "voice.transcript":
		h.handleVoiceTranscript(p)
	case "wake.detected":
		h.handleWakeDetected(p)
	case "gateway.start_timeout":
		h.turn.reportGatewayStartTimeout(paramStr(p["stderr_tail"]), h.setStatus)
	case "gateway.protocol_error":
		h.setStatus(tr("gatewayMsg.status.protocolWarning"))
		h.turn.restoreStatusAfter(statusNoteHold)
		if !h.turn.protocolWarned {
			h.turn.protocolWarned = true
			h.turn.pushActivity(tr("gatewayMsg.protocol.noiseDetected"), "info", "")
		}
		if truthyJS(p["preview"]) {
			h.turn.pushActivity(tr("gatewayMsg.protocol.noise", jsSlice(jsValueString(p["preview"]), 0, protocolPreview)), "info", "")
		}
	case "reasoning.delta":
		if truthyJS(p["text"]) {
			h.turn.recordReasoningDelta(jsValueString(p["text"]), truthyJS(p["verbose"]))
		}
	case "reasoning.available":
		h.turn.recordReasoningAvailable(stringOr(p["text"], ""), truthyJS(p["verbose"]))
	case "moa.reference":
		index, _ := p["index"].(float64)
		count, _ := p["count"].(float64)
		h.turn.recordMoaReference(stringOr(p["label"], "reference"), stringOr(p["text"], ""), int(index), int(count))
	case "moa.aggregating":
	case "moa.progress":
		done, doneNum := p["refs_done"].(float64)
		total, totalNum := p["refs_total"].(float64)
		if doneNum && totalNum {
			h.turn.pushActivity(tr("gatewayMsg.moa.refs", jsString(done), jsString(total)), "info", "MoA")
		}
	case "moa.phase":
		if p["phase"] == "aggregator" {
			h.turn.pushActivity(tr("gatewayMsg.moa.aggregating"), "info", "MoA")
		}
	case "tool.generating":
		if truthyJS(p["name"]) {
			h.turn.pushTrail("drafting " + jsValueString(p["name"]) + "…")
		}
	case "reaction":
		h.store.flashGoodVibes()
		h.store.flashPet("jump", petFlashDefault)
	case "tool.start":
		if p != nil {
			h.turn.recordToolStart(paramStr(p["tool_id"]), stringOr(p["name"], "tool"), stringOr(p["context"], ""), strippedText(p["args_text"]), toolLabelsOf(p["labels"]))
		}
	case "tool.complete":
		h.toolComplete(p)
	case "request.cancel":
		h.requestCancel(p)
	case "clarify.request":
		h.store.openCanonicalClarify(ev, h)
	case "approval.request":
		h.store.openCanonicalApproval(ev, h)
	case "background.complete":
		if p != nil {
			taskID := jsTemplate(p, "task_id")
			delete(h.store.ui.BgTasks, taskID)
			h.app.sys("[bg " + taskID + "] " + jsTemplate(p, "text"))
		}
	case "btw.complete":
		if p != nil {
			question := ""
			if truthyJS(p["question"]) {
				question = ` "` + jsValueString(p["question"]) + `"`
			}
			h.app.sys("[btw" + question + "] " + jsTemplate(p, "text"))
		}
	case "review.summary":
		if text := strings.TrimFunc(stringOr(p["text"], ""), isJSSpace); text != "" {
			h.app.sys(text)
		}
	case "subagent.spawn_requested", "subagent.start":
		if p == nil {
			return
		}
		status := "queued"
		if ev.Type == "subagent.start" {
			status = "running"
		}
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			if !isTerminalStatus(base.Status) {
				next.Status = status
			}
		}, true)
		h.maybeNudgeAgents()
		if ev.Type == "subagent.spawn_requested" {
			h.refreshDelegationStatus(h.store.delegation.MaxSpawnDepth == nil)
		}
	case "subagent.thinking", "subagent.progress":
		if p == nil {
			return
		}
		text := strings.TrimFunc(stringOr(p["text"], ""), isJSSpace)
		if text == "" {
			return
		}
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			next.Status = keepTerminalElseRunning(base.Status)
			if ev.Type == "subagent.thinking" {
				next.Thinking = pushUnique(base.Thinking, text, thinkingTailSize)
			} else {
				next.Notes = pushUnique(base.Notes, text, notesTailSize)
			}
		}, false)
	case "subagent.tool":
		if p == nil {
			return
		}
		preview := p["tool_preview"]
		if preview == nil {
			preview = p["text"]
		}
		line := formatToolCall(stringOr(p["tool_name"], "delegate_task"), stringOr(preview, ""))
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			next.Status = keepTerminalElseRunning(base.Status)
			next.Tools = pushUnique(base.Tools, line, toolsTailSize)
		}, false)
	case "subagent.complete":
		if p == nil {
			return
		}
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			next.DurationSeconds = base.DurationSeconds
			if d := optionalNumber(p, "duration_seconds"); d != nil {
				next.DurationSeconds = d
			}
			next.Status = normalizeSubagentStatus(p["status"], "completed")
			next.Summary = base.Summary
			if truthyJS(p["summary"]) {
				next.Summary = jsValueString(p["summary"])
			} else if truthyJS(p["text"]) {
				next.Summary = jsValueString(p["text"])
			}
		}, false)
	case "message.delta":
		text, _ := p["text"].(string)
		h.turn.recordMessageDelta(text)
	case "message.interim":
		if text, isStr := p["text"].(string); isStr && strings.TrimFunc(text, isJSSpace) != "" {
			h.turn.recordInterimMessage(text)
		}
	case "message.complete":
		h.messageComplete(ev)
	case "error":
		_, stamped := field(current, "execution_generation")
		h.handleErrorEvent(p, genericError && stamped)
	}
}

// Thinking text while busy, batched into the status line and the reasoning stream
func (h *eventHandler) thinkingDelta(p map[string]any) {
	if !h.store.ui.Busy {
		return
	}
	raw, has := field(p, "text")
	if !has {
		return
	}
	value := jsValueString(raw)
	status := value
	if status == "" {
		status = h.store.statusFromBusy()
	}
	h.scheduleThinkingStatus(status)
	if value != "" {
		h.turn.recordReasoningDelta(value, false)
	}
}

// Status line of a status update, goal verdicts, compaction and noted activity
func (h *eventHandler) statusUpdate(p map[string]any) {
	if !truthyJS(p["text"]) {
		return
	}
	text := jsValueString(p["text"])
	kind, _ := p["kind"].(string)
	if kind == "goal" {
		h.app.sys(text)
		brief := "ready"
		switch {
		case strings.HasPrefix(text, "✓"):
			brief = tr("gatewayMsg.goal.complete")
		case strings.HasPrefix(text, "↻"):
			brief = tr("gatewayMsg.goal.continuing")
		case strings.HasPrefix(text, "⏸"):
			brief = tr("gatewayMsg.goal.paused")
		}
		h.setStatus(brief)
		h.turn.restoreStatusAfter(goalStatusHold)
		return
	}
	h.setStatus(text)
	if kind == "compressing" || kind == "compacting" {
		h.app.sys(text)
		h.turn.clearStatusTimer()
		h.store.ui.Compacting = true
		return
	}
	if kind == "compacted" {
		h.store.ui.Compacting = false
	}
	if !truthyJS(p["kind"]) || kind == "status" {
		return
	}
	if h.turn.lastStatusNote != text {
		h.turn.lastStatusNote = text
		tone := "info"
		switch kind {
		case "error":
			tone = "error"
		case "warn", "approval":
			tone = "warn"
		}
		h.turn.pushActivity(text, tone, "")
	}
	h.turn.restoreStatusAfter(statusNoteHold)
}

// Finished tool call, an inline diff segment when the diff is shown, else its trail row
func (h *eventHandler) toolComplete(p map[string]any) {
	if p == nil {
		return
	}
	if p["name"] == "clarify" {
		h.flushAbandonedClarify()
	}
	inlineDiff := ""
	if truthyJS(p["inline_diff"]) && h.store.ui.InlineDiffs {
		inlineDiff = strings.TrimFunc(stripAnsi(jsValueString(p["inline_diff"])), isJSSpace)
	}
	resultText := strippedText(p["result_text"])
	toolID, name := paramStr(p["tool_id"]), paramStr(p["name"])
	duration, labels := optionalNumber(p, "duration_s"), toolLabelsOf(p["labels"])
	if inlineDiff != "" {
		h.turn.recordInlineDiffToolComplete(inlineDiff, toolID, name, duration, resultText, labels)
		return
	}
	var todos any
	if v, has := p["todos"]; has && v != nil {
		todos = v
	}
	h.turn.recordToolComplete(toolID, name, paramStr(p["summary"]), duration, todos, resultText, labels)
}

// Closes the prompt cards of a request the gateway cancelled, with its timeout notice
func (h *eventHandler) requestCancel(p map[string]any) {
	if !truthyJS(p["id"]) {
		return
	}
	id := jsValueString(p["id"])
	if text, has := promptTimeoutNotice(paramStr(p["method"]), paramStr(p["reason"])); has {
		h.app.sys(text)
	}
	h.store.forgetServerRequest(id)
	for _, key := range append([]string{"approval", "clarify"}, sensitivePrompts...) {
		if h.store.overlay.promptRequestID(key) == id {
			h.store.overlay.closePrompt(key)
		}
	}
}

// Completed turn, its final rows, interrupt rewrite, failure copy, bell, usage, spawn tree and billing wall
func (h *eventHandler) messageComplete(ev gatewayEventMsg) {
	p := ev.Payload
	if p == nil {
		p = map[string]any{}
	}
	h.store.noteCanonicalCompletion(ev.SessionID, p["admission_id"])
	done := h.turn.recordMessageComplete(p)
	if len(done.FinishedSubagents) > 0 {
		h.queue(h.persistSpawnTree(done.FinishedSubagents, done.SessionID))
	}
	if reply := done.InterruptedReply; reply != nil && reply.From == nil {
		h.app.appendMessage(msg{Role: "assistant", Text: reply.To})
	} else if reply != nil {
		from, to := *reply.From, reply.To
		h.app.setHistoryItems(func(prev []msg) []msg {
			at := -1
			for i := len(prev) - 1; i >= 0; i-- {
				if prev[i].Role == "assistant" && prev[i].Text == from {
					at = i
					break
				}
			}
			if at < 0 {
				return prev
			}
			next := slices.Clone(prev)
			next[at].Text = to
			return next
		})
	}
	if !done.WasInterrupted {
		errText := jsValueString(p["error"])
		if p["error"] == nil {
			errText = ""
		}
		failed := p["status"] == "error" && !truthyJS(p["partial"]) && isBareErrorText(done.FinalText, errText)
		var msgs []msg
		switch {
		case failed:
			for i, m := range done.FinalMessages {
				if !(i == len(done.FinalMessages)-1 && m.Role == "assistant" && isBareErrorText(m.Text, errText)) {
					msgs = append(msgs, m)
				}
			}
			msgs = append(msgs, msg{Role: "assistant", Text: describeTurnFailure(p)})
		case len(done.FinalMessages) > 0:
			msgs = done.FinalMessages
		default:
			msgs = []msg{{Role: "assistant", Text: done.FinalText}}
		}
		for _, m := range msgs {
			h.app.appendMessage(m)
		}
		beat := "wave"
		if isTodoDone(h.store.turn.Todos) {
			beat = "jump"
		}
		h.store.flashPet(beat, petFlashDefault)
		if h.app.bellOnComplete() {
			h.queue(tea.Raw("\x07"))
		}
	}
	h.setStatus("ready")
	if truthyJS(p["warning"]) {
		h.turn.pushActivity(jsValueString(p["warning"]), "warn", "")
	}
	if usage, _ := p["usage"].(map[string]any); truthyJS(p["usage"]) {
		h.store.ui.Usage = mergeUsageStable(h.store.ui.Usage, usage)
	}
	h.confirmBillingWall(p)
}

// Error event, a versioned generic error printed alone, else a turn failure with setup help when no provider exists
func (h *eventHandler) handleErrorEvent(p map[string]any, versionedGenericError bool) {
	message := tr("gatewayMsg.error.unknown")
	if truthyJS(p["message"]) {
		message = jsValueString(p["message"])
	}
	if versionedGenericError {
		h.app.sys("error: " + message)
		return
	}
	h.turn.recordError()
	h.store.flashPet("failed", petFlashDefault)
	h.turn.pushActivity(message, "error", "")
	if noProviderRe.MatchString(message) {
		h.app.panel(setupRequiredTitle(), buildSetupRequiredSections())
		h.setStatus(tr("session.status.setupRequired"))
		return
	}
	h.app.sys("error: " + describeRPCError(errors.New(message)))
	h.setStatus("ready")
}

// Usage with the fields of a live tick laid over it
func spreadUsage(prev, next map[string]any) map[string]any {
	merged := make(map[string]any, len(prev)+len(next))
	for k, v := range prev {
		merged[k] = v
	}
	for k, v := range next {
		merged[k] = v
	}
	return merged
}

// Session info push, admitted bubbles, running state, stored id and usage, ignored for another profile
func (h *eventHandler) applySessionInfo(incoming map[string]any) {
	current := h.store.ui.Info
	if incoming == nil {
		return
	}
	if truthyJS(incoming["profile_name"]) && truthyJS(current["profile_name"]) && !jsStrictEqual(incoming["profile_name"], current["profile_name"]) {
		return
	}
	info := make(map[string]any, len(current)+len(incoming))
	for k, v := range current {
		info[k] = v
	}
	for k, v := range incoming {
		info[k] = v
	}
	for _, started := range h.store.newlyStartedRows(current["pending_submissions"], incoming["pending_submissions"]) {
		h.store.markBubbleShown(paramStr(started["input_id"]))
		h.app.appendMessage(msg{Role: "user", Text: paramStr(started["user"])})
	}
	if incoming["running"] == true {
		h.store.ui.Busy, h.store.ui.Status = true, "running…"
	}
	if incoming["running"] == false {
		h.turn.clearStatusTimer()
		h.turn.idle(true)
		h.setStatus("ready")
	}
	storedSid := h.store.ui.StoredSid
	if truthyJS(info["stored_session_id"]) {
		storedSid = jsValueString(info["stored_session_id"])
	}
	if storedSid != "" {
		info["stored_session_id"] = storedSid
	}
	h.store.ui.Info = info
	if h.store.ui.Status == tr("session.status.startingAgent") {
		h.store.ui.Status = "ready"
	}
	h.store.ui.StoredSid = storedSid
	if usage, _ := info["usage"].(map[string]any); truthyJS(info["usage"]) {
		h.store.ui.Usage = mergeUsageStable(h.store.ui.Usage, usage)
	}
	h.app.setHistoryItems(func(prev []msg) []msg {
		next := slices.Clone(prev)
		for i := range next {
			if next[i].Kind == "intro" {
				next[i].Info = info
			}
		}
		return next
	})
}

// Model pick that did not apply at the turn boundary, a refusal named or a guarded target asked again
func (h *eventHandler) applyDeferredPickOutcome(update map[string]any, sid string) {
	if update["operation"] != "model" || sid == "" {
		return
	}
	if update["status"] == "refused" {
		if info := h.store.ui.Info; info != nil {
			if model, isStr := update["model"].(string); isStr && model != "" {
				next := make(map[string]any, len(info)+2)
				for k, v := range info {
					next[k] = v
				}
				next["model"] = model
				if provider, isStr := update["provider"].(string); isStr {
					next["provider"] = provider
				}
				h.store.ui.Info = next
			}
		}
		h.app.sys(tr("slashCmd.session.model.deferredRefused", stringOr(update["target_model"], ""), stringOr(update["reason"], "refused")))
		return
	}
	confirm, isStr := update["confirm"].(string)
	if update["status"] != "confirmation_required" || !isStr {
		return
	}
	pick, _ := update["payload"].(map[string]any)
	arg := stringOr(pick["model"], "")
	if truthyJS(pick["provider"]) {
		arg += " --provider " + jsValueString(pick["provider"])
	}
	if truthyJS(pick["reasoning"]) {
		arg += " --reasoning " + jsValueString(pick["reasoning"])
	}
	target := update["target_model"]
	if target == nil {
		target = pick["model"]
	}
	detail, _ := update["confirm_message"].(string)
	h.store.openModelConfirmation(detail, stringOr(target, ""), func() tea.Cmd {
		return h.store.mutateCanonicalSession(h.gw, sid, "model", arg, func() bool { return false }, confirm, func(m *mutationOutcome, err error) tea.Cmd {
			if err != nil {
				h.app.sys("error: " + err.Error())
				return nil
			}
			if m != nil && m.Result["status"] == "confirmation_required" {
				h.app.sys(tr("slashCmd.session.model.confirmStale"))
			}
			return nil
		})
	})
}

// Billing wall of a completed turn, a window plan's limit dialog or the out of credits dialog
func (h *eventHandler) confirmBillingWall(p map[string]any) {
	surface, _ := p["error_surface"].(map[string]any)
	if limit, _ := surface["usage_limit"].(map[string]any); truthyJS(surface["usage_limit"]) {
		h.confirmUsageLimit(limit)
		return
	}
	block, _ := p["billing"].(map[string]any)
	if !truthyJS(p["billing"]) || surface["code"] == "usage_window" {
		return
	}
	copy := billingDialogCopy(block)
	h.store.overlay.Confirm = &confirmReq{CancelLabel: copy.CancelLabel, ConfirmLabel: copy.ConfirmLabel, Detail: copy.Detail, Title: copy.Title, OnConfirm: func() tea.Cmd {
		switch {
		case truthyJS(block["is_nous"]):
			return h.app.submit("/topup")
		case truthyJS(block["billing_url"]):
			openExternalURL(jsValueString(block["billing_url"]))
			return nil
		}
		return h.app.submit("/model")
	}}
}

// Window plan limit dialog, switching this session to the recommended model and sending again, or the top up
func (h *eventHandler) confirmUsageLimit(limit map[string]any) {
	dialog := usageLimitDialog(limit)
	h.store.overlay.Confirm = &confirmReq{CancelLabel: dialog.CancelLabel, ConfirmLabel: dialog.ConfirmLabel, Detail: dialog.Detail, Title: dialog.Title, OnConfirm: func() tea.Cmd {
		if dialog.SwitchValue == "" {
			return h.app.submit("/topup")
		}
		sid := h.store.ui.Sid
		if h.gw.canonical() && sid != "" {
			return h.store.switchCanonicalModel(h.gw, sid, dialog.SwitchValue, h.app.sys, func() tea.Cmd { return h.app.submit("/retry") }, "", func(err error) tea.Cmd {
				if err != nil {
					h.app.sys("error: " + rpcErrorMessage(err))
				}
				return nil
			})
		}
		params := map[string]any{"key": "model", "session_id": nil, "value": dialog.SwitchValue}
		if sid != "" {
			params["session_id"] = sid
		}
		return h.rpc("config.set", params, func(result map[string]any) tea.Cmd {
			if h.store.ui.Sid != sid {
				return nil
			}
			if !truthyJS(result["value"]) {
				h.app.sys("error: model switch failed")
				return nil
			}
			value := jsValueString(result["value"])
			h.app.sys("model → " + value)
			if info := h.store.ui.Info; info != nil {
				next := make(map[string]any, len(info)+1)
				for k, v := range info {
					next[k] = v
				}
				next["model"] = value
				h.store.ui.Info = next
			}
			return h.app.submit("/retry")
		})
	}}
}

package tui

import (
	"errors"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"regexp"
	"slices"
	"strconv"
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
	h.renewOnRedraw()
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
		if ready, ok := eventPayload(h, tuigateway.GatewayReadyEvent, p); ok {
			h.handleReady(gatewaySkinOf(ready.Skin))
		}
	case "skin.changed":
		if skin, ok := eventPayload(h, tuigateway.SkinChangedEvent, p); ok && p != nil {
			h.queue(h.store.applySkin(gatewaySkinOf(&skin)))
		}
	case "session.info":
		h.applySessionInfo(ev)
	case "session.updated":
		if update, ok := eventPayload(h, tuigateway.SessionUpdatedEvent, p); ok {
			h.applyDeferredPickOutcome(update, sid)
		}
	case "session.replay_gap":
		if sid != "" {
			h.queue(h.app.resumeByID(sid, nil))
		}
	case "session.usage":
		if tick, ok := eventPayload(h, tuigateway.SessionUsageEvent, p); ok {
			h.store.ui.Usage = mergeUsageStable(h.store.ui.Usage, usageOfCounters(tick))
		}
	case "thinking.delta":
		h.thinkingDelta(p)
	case "session.control.update":
		if update, ok := eventPayload(h, tuigateway.SessionControlUpdateEvent, p); ok {
			h.store.applyControlSnapshot(sid, update.Control)
		}
	case "message.start":
		h.agentsNudgedThisTurn = false
		h.turn.startMessage()
	case "status.update":
		h.statusUpdate(p)
	case "notification.show":
		h.showGatewayNotice(p)
	case "notification.clear":
		h.turn.clearNotice(paramStr(p["key"]))
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
	case "voice.interrupted":
		h.setStatus(tr("gatewayMsg.status.voiceInterrupted"))
		h.turn.restoreStatusAfter(statusNoteHold)
	case "session.resume_progress":
		h.resumeProgress(sid, p)
	case "session.reclaimed":
		h.sessionReclaimed(p)
	case "display.status", "display.lease":
		h.setStatus(screenEventNote(ev))
		h.turn.restoreStatusAfter(statusNoteHold)
	case "display.install.log", "display.install.done":
		h.app.sys(screenInstallLine(ev))
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
		h.handleErrorEvent(p, genericError && infoOf(current).ExecutionGeneration != nil)
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
	if raw, has := p["usage"]; has && raw != nil {
		var usage tuigateway.Usage
		if err := tuigateway.Loose(raw, &usage); err != nil {
			h.app.sys("error: " + tr("session.common.invalidResponse", "message.complete") + " " + err.Error())
		} else {
			h.store.ui.Usage = mergeUsageStable(h.store.ui.Usage, &usage)
		}
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

// Session info push, admitted bubbles, running state, stored id and usage, ignored for another profile
func (h *eventHandler) applySessionInfo(ev gatewayEventMsg) {
	incoming, ok := eventPayload(h, tuigateway.SessionInfoEvent, ev.Payload)
	if !ok {
		return
	}
	if ev.Epoch != nil {
		incoming.AuthorityEpoch = ev.Epoch
	}
	if ev.Generation != nil {
		incoming.ExecutionGeneration = ev.Generation
	}
	current := h.store.ui.Info
	if name, held := val(incoming.ProfileName), val(infoOf(current).ProfileName); name != "" && held != "" && name != held {
		return
	}
	info := patchedInfo(current, incoming)
	for _, started := range h.store.newlyStartedRows(current, &incoming) {
		h.store.markBubbleShown(paramStr(started["input_id"]))
		row := map[string]any{"role": "user", "text": paramStr(started["user"]),
			"display_kind": started["display_kind"], "display_metadata": started["display_metadata"]}
		for _, shown := range toTranscriptMessages([]any{row}) {
			h.app.appendMessage(shown)
		}
	}
	if running := incoming.Running; running != nil && *running {
		h.store.ui.Busy, h.store.ui.Status = true, "running…"
	}
	if running := incoming.Running; running != nil && !*running {
		h.turn.clearStatusTimer()
		h.turn.idle(true)
		h.setStatus("ready")
	}
	storedSid := h.store.ui.StoredSid
	if stored := val(info.StoredSessionID); stored != "" {
		storedSid = stored
	}
	if storedSid != "" {
		info.StoredSessionID = &storedSid
	}
	h.store.ui.Info = info
	if h.store.ui.Status == tr("session.status.startingAgent") {
		h.store.ui.Status = "ready"
	}
	h.store.ui.StoredSid = storedSid
	if info.Usage != nil {
		h.store.ui.Usage = mergeUsageStable(h.store.ui.Usage, info.Usage)
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

// Typed payload of an event, a misfit printed as an invalid response naming the field
func eventPayload[T any](h *eventHandler, ev tuigateway.Event[T], p map[string]any) (T, bool) {
	payload, err := ev.Payload(p)
	if err != nil {
		h.app.sys("error: " + tr("session.common.invalidResponse", ev.Name()) + " " + err.Error())
		return payload, false
	}
	return payload, true
}

// Model pick that did not apply at the turn boundary, a refusal named or a guarded target asked again
func (h *eventHandler) applyDeferredPickOutcome(update tuigateway.SessionMutateResult, sid string) {
	if update.Operation != "model" || sid == "" {
		return
	}
	status := val(update.Status)
	if status == "refused" {
		if model := val(update.Model); model != "" && h.store.ui.Info != nil {
			h.store.ui.Info = patchedInfo(h.store.ui.Info, tuigateway.SessionInfo{Model: update.Model, Provider: update.Provider})
		}
		reason := val(update.Reason)
		if reason == "" {
			reason = "refused"
		}
		h.app.sys(tr("slashCmd.session.model.deferredRefused", val(update.TargetModel), reason))
		return
	}
	confirm := val(update.Confirm)
	if status != "confirmation_required" || update.Confirm == nil {
		return
	}
	pick := update.Payload
	if pick == nil {
		pick = &tuigateway.ModelPick{}
	}
	arg := pick.Model
	if provider := val(pick.Provider); provider != "" {
		arg += " --provider " + provider
	}
	if reasoning := val(pick.Reasoning); reasoning != "" {
		arg += " --reasoning " + reasoning
	}
	target := pick.Model
	if update.TargetModel != nil {
		target = *update.TargetModel
	}
	h.store.openModelConfirmation(val(update.ConfirmMessage), target, func() tea.Cmd {
		return h.store.mutateCanonicalSession(h.gw, sid, "model", arg, func() bool { return false }, confirm, func(m *mutationOutcome, err error) tea.Cmd {
			if err != nil {
				h.app.sys("error: " + err.Error())
				return nil
			}
			if m != nil && val(m.Result.Status) == "confirmation_required" {
				h.app.sys(tr("slashCmd.session.model.confirmStale"))
			}
			return nil
		})
	})
}

// Out of credits dialog of a turn the provider refused for billing, opening its billing page or the model picker
func (h *eventHandler) confirmBillingWall(p map[string]any) {
	block, _ := p["billing"].(map[string]any)
	if !truthyJS(p["billing"]) {
		return
	}
	copy := billingDialogCopy(block)
	h.store.overlay.Confirm = &confirmReq{CancelLabel: copy.CancelLabel, ConfirmLabel: copy.ConfirmLabel, Detail: copy.Detail, Title: copy.Title, OnConfirm: func() tea.Cmd {
		if truthyJS(block["billing_url"]) {
			openExternalURL(jsValueString(block["billing_url"]))
			return nil
		}
		return h.app.submit("/model")
	}}
}

// Status of a deferred history load of the session in view, loading then the count of messages read
func (h *eventHandler) resumeProgress(sid string, p map[string]any) {
	progress, ok := eventPayload(h, tuigateway.SessionResumeProgressEvent, p)
	if !ok || sid == "" || sid != h.store.ui.Sid {
		return
	}
	switch progress.Status {
	case "loading":
		h.setStatus(tr("gatewayMsg.status.loadingHistory"))
	case "complete":
		h.setStatus(tr("gatewayMsg.status.historyLoaded", strconv.FormatInt(val(progress.MessageCount), 10)))
		h.turn.restoreStatusAfter(statusNoteHold)
	}
}

// Warns that the gateway closed the session in view, naming the reason and the resume that reopens it
func (h *eventHandler) sessionReclaimed(p map[string]any) {
	reclaimed, ok := eventPayload(h, tuigateway.SessionReclaimedEvent, p)
	live, stored := reclaimed.SessionID, reclaimed.StoredSessionID
	if !ok || live == "" || (live != h.store.ui.Sid && (stored == "" || stored != h.store.ui.StoredSid)) {
		return
	}
	if stored == "" {
		stored = live
	}
	h.app.sys("warning: " + tr("session.lifecycle.reclaimed", reclaimed.Reason, stored))
}

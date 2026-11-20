package tui

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"

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
func (s *tuiStore) noteCanonicalCompletion(sid, id string) {
	if sid == "" || id == "" {
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

// Display labels of a tool event, nil when absent
func toolLabelsOf(labels []tuigateway.ToolLabel) []toolLabel {
	if len(labels) == 0 {
		return nil
	}
	out := make([]toolLabel, 0, len(labels))
	for _, l := range labels {
		out = append(out, toolLabel{Emoji: l.Emoji, Text: l.Text, Preview: l.Preview})
	}
	return out
}

// Text of an optional payload field with ANSI escapes removed, empty when absent
func strippedText(v *string) string { return stripAnsi(val(v)) }

// Dispatches one gateway event to the store, the turn controller and the app
func (h *eventHandler) handle(ev gatewayEventMsg) {
	h.renewOnRedraw()
	sid := h.store.ui.Sid
	if ev.SessionID != "" && sid != "" && ev.SessionID != sid && !strings.HasPrefix(ev.Type, "gateway.") {
		h.store.withdrawOutOfView(ev)
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
	case tuigateway.ConnectionRequestEvent.Name():
		if request, ok := eventPayload(h, tuigateway.ConnectionRequestEvent, p); ok {
			h.store.applyConnectionRequest(request)
		}
	case tuigateway.ConnectionUpdateEvent.Name():
		if update, ok := eventPayload(h, tuigateway.ConnectionUpdateEvent, p); ok {
			for _, line := range h.store.applyConnectionUpdate(update) {
				h.app.sys(line)
			}
		}
	case "gateway.ready":
		if ready, ok := eventPayload(h, tuigateway.GatewayReadyEvent, p); ok {
			h.handleReady(ready)
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
		if delta, ok := eventPayload(h, tuigateway.ThinkingDeltaEvent, p); ok {
			h.thinkingDelta(delta)
		}
	case "session.control.update":
		if update, ok := eventPayload(h, tuigateway.SessionControlUpdateEvent, p); ok {
			h.store.applyControlSnapshot(sid, update.Control)
		}
	case "message.start":
		h.agentsNudgedThisTurn = false
		h.turn.startMessage()
	case "status.update":
		if update, ok := eventPayload(h, tuigateway.StatusUpdateEvent, p); ok {
			h.statusUpdate(update)
		}
	case "notification.show":
		if show, ok := eventPayload(h, tuigateway.NotificationShowEvent, p); ok {
			h.showGatewayNotice(show)
		}
	case "notification.clear":
		if clear, ok := eventPayload(h, tuigateway.NotificationClearEvent, p); ok {
			h.turn.clearNotice(clear.Key)
		}
	case "notice":
		if note, ok := eventPayload(h, tuigateway.NoticeEvent, p); ok && note.Message != "" {
			h.app.sys(note.Message)
		}
	case "gateway.stderr":
		if stderr, ok := ev.Local.(gatewayStderr); ok && stderrLooksLikeProblem(stderr.Line) {
			h.turn.pushActivity(stderrProblemActivity(stderr.Line), "warn", "")
		}
	case "gateway.reconnecting":
		if redial, ok := ev.Local.(gatewayReconnecting); ok {
			h.setStatus(backendReconnecting(redial.Attempt, &redial.DelayMs))
		}
	case "browser.progress":
		if progress, ok := eventPayload(h, tuigateway.BrowserProgressEvent, p); ok {
			if message := strings.TrimFunc(progress.Message, isJSSpace); message != "" {
				h.app.sys(message)
			}
		}
	case "voice.transcript":
		if transcript, ok := eventPayload(h, tuigateway.VoiceTranscriptEvent, p); ok {
			h.handleVoiceTranscript(transcript)
		}
	case "voice.interrupted":
		h.setStatus(tr("gatewayMsg.status.voiceInterrupted"))
		h.turn.restoreStatusAfter(statusNoteHold)
		if h.gw.canonical() {
			speech.latch()
			if h.store.ui.Busy {
				h.queue(h.app.interruptTurn())
			}
		}
	case tuigateway.VoiceSpeechEvent.Name():
		if spoken, ok := eventPayload(h, tuigateway.VoiceSpeechEvent, p); ok {
			speech.handle(h.gw, spoken)
		}
	case "session.resume_progress":
		h.resumeProgress(sid, p)
	case "session.reclaimed":
		h.sessionReclaimed(p)
	case "display.status":
		if status, ok := eventPayload(h, tuigateway.DisplayStatusEvent, p); ok {
			h.setStatus(screenRunningNote(status.Running))
			h.turn.restoreStatusAfter(statusNoteHold)
		}
	case "display.lease":
		if lease, ok := eventPayload(h, tuigateway.DisplayLeaseEvent, p); ok {
			h.setStatus(tr("screen.control", screenHolder(lease.Lease)))
			h.turn.restoreStatusAfter(statusNoteHold)
		}
	case "display.install.log":
		if line, ok := eventPayload(h, tuigateway.DisplayInstallLogEvent, p); ok {
			h.app.sys("  " + line.Line)
		}
	case "display.install.done":
		if done, ok := eventPayload(h, tuigateway.DisplayInstallDoneEvent, p); ok {
			h.app.sys(screenInstallDone(done.Code))
		}
	case "wake.detected":
		if wake, ok := eventPayload(h, tuigateway.WakeDetectedEvent, p); ok {
			h.handleWakeDetected(wake)
		}
	case "gateway.start_timeout":
		if timeout, ok := ev.Local.(gatewayStartTimeout); ok {
			h.turn.reportGatewayStartTimeout(timeout.StderrTail, h.setStatus)
		}
	case "gateway.protocol_error":
		h.setStatus(tr("gatewayMsg.status.protocolWarning"))
		h.turn.restoreStatusAfter(statusNoteHold)
		if !h.turn.protocolWarned {
			h.turn.protocolWarned = true
			h.turn.pushActivity(tr("gatewayMsg.protocol.noiseDetected"), "info", "")
		}
		if noise, ok := ev.Local.(gatewayProtocolError); ok && noise.Preview != "" {
			h.turn.pushActivity(tr("gatewayMsg.protocol.noise", jsSlice(noise.Preview, 0, protocolPreview)), "info", "")
		}
	case "reasoning.delta":
		if delta, ok := eventPayload(h, tuigateway.ReasoningDeltaEvent, p); ok && delta.Text != "" {
			h.turn.countReasoning(delta.Text)
			h.turn.recordReasoningDelta(delta.Text, val(delta.Verbose))
		}
	case "reasoning.available":
		if block, ok := eventPayload(h, tuigateway.ReasoningAvailableEvent, p); ok {
			h.turn.recordReasoningAvailable(block.Text, val(block.Verbose))
		}
	case "moa.reference":
		if ref, ok := eventPayload(h, tuigateway.MoaReferenceEvent, p); ok {
			h.turn.recordMoaReference(ref.Label, ref.Text, val(ref.Index), val(ref.Count))
		}
	case "moa.aggregating":
	case "moa.progress":
		if progress, ok := eventPayload(h, tuigateway.MoaProgressEvent, p); ok {
			h.turn.pushActivity(tr("gatewayMsg.moa.refs", progress.RefsDone, progress.RefsTotal), "info", "MoA")
		}
	case "moa.phase":
		if phase, ok := eventPayload(h, tuigateway.MoaPhaseEvent, p); ok && phase.Phase == "aggregator" {
			h.turn.pushActivity(tr("gatewayMsg.moa.aggregating"), "info", "MoA")
		}
	case tuigateway.ToolGeneratingEvent.Name():
		if generating, ok := eventPayload(h, tuigateway.ToolGeneratingEvent, p); ok && generating.Name != "" {
			h.turn.pushTrail(tr("tool.drafting", generating.Name))
		}
	case "reaction":
		h.store.flashGoodVibes()
		h.store.flashPet("jump", petFlashDefault)
	case tuigateway.ToolStartEvent.Name():
		if start, ok := eventPayload(h, tuigateway.ToolStartEvent, p); ok {
			h.turn.recordToolStart(start.ToolID, firstText(start.Name, "tool"), val(start.Context), strippedText(start.ArgsText), toolLabelsOf(start.Labels))
		}
	case tuigateway.ToolCompleteEvent.Name():
		if complete, ok := eventPayload(h, tuigateway.ToolCompleteEvent, p); ok {
			h.toolComplete(complete)
		}
	case tuigateway.ToolProgressEvent.Name():
		if progress, ok := eventPayload(h, tuigateway.ToolProgressEvent, p); ok {
			h.turn.recordToolProgress(progress.ToolID, toolProgressText(progress))
		}
	case tuigateway.ToolOutputRiskEvent.Name():
		if risk, ok := eventPayload(h, tuigateway.ToolOutputRiskEvent, p); ok && risk.Risk != "low" {
			h.app.sys("⚠ " + toolRiskText(risk))
		}
	case tuigateway.TodoUpdatedEvent.Name():
		if todos, ok := eventPayload(h, tuigateway.TodoUpdatedEvent, p); ok {
			h.turn.recordTodos(todos.Todos)
		}
	case "request.cancel":
		if cancel, ok := eventPayload(h, tuigateway.RequestCancelEvent, p); ok {
			h.requestCancel(cancel)
		}
	case "clarify.request":
		h.store.openCanonicalClarify(ev, h)
	case "approval.request":
		h.store.openCanonicalApproval(ev, h)
	case "background.complete":
		if done, ok := eventPayload(h, tuigateway.BackgroundCompleteEvent, p); ok {
			delete(h.store.ui.BgTasks, done.TaskID)
			h.app.sys("[bg " + done.TaskID + "] " + done.Text)
		}
	case "btw.complete":
		if done, ok := eventPayload(h, tuigateway.BtwCompleteEvent, p); ok {
			question := ""
			if asked := val(done.Question); asked != "" {
				question = ` "` + asked + `"`
			}
			h.app.sys("[btw" + question + "] " + done.Text)
		}
	case "review.summary":
		if summary, ok := eventPayload(h, tuigateway.ReviewSummaryEvent, p); ok && strings.TrimFunc(summary.Text, isJSSpace) != "" {
			h.app.sys(strings.TrimFunc(summary.Text, isJSSpace))
		}
	case "subagent.spawn_requested", "subagent.start", "subagent.thinking", "subagent.progress", "subagent.tool", "subagent.complete":
		if p == nil {
			return
		}
		if sub, ok := eventPayload(h, subagentEvents[ev.Type], p); ok {
			h.applySubagentEvent(ev.Type, sub)
		}
	case "message.delta":
		if delta, ok := eventPayload(h, tuigateway.MessageDeltaEvent, p); ok {
			h.turn.recordMessageDelta(val(delta.Text))
		}
	case "message.interim":
		if interim, ok := eventPayload(h, tuigateway.MessageInterimEvent, p); ok && strings.TrimFunc(interim.Text, isJSSpace) != "" {
			h.turn.recordInterimMessage(interim.Text)
		}
	case "message.complete":
		h.messageComplete(ev)
	case "error":
		failure, _ := eventPayload(h, tuigateway.ErrorEvent, p)
		h.handleErrorEvent(failure, genericError && infoOf(current).ExecutionGeneration != nil)
	}
}

// Thinking text while busy, batched into the status line and the reasoning stream
func (h *eventHandler) thinkingDelta(delta tuigateway.StreamDeltaPayload) {
	if !h.store.ui.Busy {
		return
	}
	status := delta.Text
	if status == "" {
		status = h.store.statusFromBusy()
	}
	h.scheduleThinkingStatus(status)
	if delta.Text != "" {
		h.turn.recordReasoningDelta(delta.Text, false)
	}
}

// Status line of a status update, goal verdicts, compaction and noted activity
func (h *eventHandler) statusUpdate(update tuigateway.StatusUpdatePayload) {
	text, kind := update.Text, update.Kind
	if text == "" {
		return
	}
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
	if kind == "" || kind == "status" {
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

// Finished tool call, an inline diff segment when the diff is shown, else its trail row, then the image it made or looked at
func (h *eventHandler) toolComplete(p tuigateway.ToolCompletePayload) {
	if p.Name == "clarify" {
		h.flushAbandonedClarify()
	}
	inlineDiff := ""
	if h.store.ui.InlineDiffs {
		inlineDiff = strings.TrimFunc(strippedText(p.InlineDiff), isJSSpace)
	}
	resultText := strippedText(p.ResultText)
	duration, labels, verdict := p.DurationS, toolLabelsOf(p.Labels), verdictOf(p.IsError, p.Refused, p.Error)
	h.turn.summary.recordTool(p.Name, val(p.Result), val(p.IsError) || val(p.Refused))
	if inlineDiff != "" {
		h.turn.recordInlineDiffToolComplete(inlineDiff, p.ToolID, p.Name, duration, resultText, labels, verdict)
	} else {
		h.turn.recordToolComplete(p.ToolID, p.Name, val(p.Summary), duration, p.Todos, resultText, labels, verdict)
	}
	h.turn.pushImageSegment(val(p.ImagePath), p.ToolID)
}

// Progress line of a running call, the server's message with the percent done when it reports a total
func toolProgressText(p tuigateway.ToolProgressPayload) string {
	message := strings.TrimFunc(stripAnsi(val(p.Message)), isJSSpace)
	if p.Percent == nil {
		return message
	}
	if message == "" {
		return tr("tool.progressPercent", *p.Percent)
	}
	return tr("tool.progressMessage", message, *p.Percent)
}

// Activity line of a tool output the injection scanner flagged, its findings named
func toolRiskText(p tuigateway.ToolOutputRiskPayload) string {
	name := toolTrailLabel(strings.TrimPrefix(p.Name, "mcp__"))
	if len(p.Findings) == 0 {
		return tr("tool.outputRisk", name, p.Risk)
	}
	return tr("tool.outputRiskFindings", name, p.Risk, strings.Join(p.Findings, ", "))
}

// Closes the prompt cards of a request the gateway cancelled, with its timeout notice
func (h *eventHandler) requestCancel(cancel tuigateway.RequestCancelPayload) {
	id := cancel.ID
	if id == "" {
		return
	}
	if text, has := promptTimeoutNotice(cancel.Method, cancel.Reason); has {
		h.app.sys(text)
	}
	h.store.withdrawServerRequest(id)
}

// Completed turn, its final rows, interrupt rewrite, failure copy, bell, usage, spawn tree and billing wall
func (h *eventHandler) messageComplete(ev gatewayEventMsg) {
	p, _ := eventPayload(h, tuigateway.MessageCompleteEvent, ev.Payload)
	h.store.noteCanonicalCompletion(ev.SessionID, val(p.AdmissionID))
	done := h.turn.recordMessageComplete(p)
	if len(done.FinishedSubagents) > 0 {
		h.queue(h.persistSpawnTree(h.store.spawnHistory[0]))
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
		errText := val(p.Error)
		failed := val(p.Status) == "error" && !val(p.Partial) && isBareErrorText(done.FinalText, errText)
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
	if line := h.turn.summaryLine(); line != "" {
		h.app.appendMessage(msg{Kind: "event", Role: "system", Text: line})
	}
	h.setStatus("ready")
	if warning := val(p.Warning); warning != "" {
		h.turn.pushActivity(warning, "warn", "")
	}
	if p.Usage != nil {
		h.store.ui.Usage = mergeUsageStable(h.store.ui.Usage, p.Usage)
	}
	h.confirmBillingWall(p.Billing)
}

// Error event, a versioned generic error printed alone, else a turn failure with setup help when no provider exists
func (h *eventHandler) handleErrorEvent(failure tuigateway.ErrorPayload, versionedGenericError bool) {
	message := failure.Message
	if message == "" {
		message = tr("gatewayMsg.error.unknown")
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
		h.store.markBubbleShown(started.InputID)
		row := tuigateway.TranscriptMessage{Role: "user", Text: &started.User, DisplayKind: started.DisplayKind, DisplayMetadata: started.DisplayMetadata}
		staged, typedHere := h.store.takePastes(started.User)
		for _, shown := range toTranscriptMessages([]tuigateway.TranscriptMessage{row}) {
			if typedHere && shown.Role == "user" && shown.Kind == "" {
				shown.Text, shown.Pastes = staged.display, staged.pastes
			}
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

// Subagent events by wire name
var subagentEvents = map[string]tuigateway.Event[tuigateway.SubagentEventPayload]{
	tuigateway.SubagentSpawnRequestedEvent.Name(): tuigateway.SubagentSpawnRequestedEvent, tuigateway.SubagentStartEvent.Name(): tuigateway.SubagentStartEvent,
	tuigateway.SubagentThinkingEvent.Name(): tuigateway.SubagentThinkingEvent, tuigateway.SubagentProgressEvent.Name(): tuigateway.SubagentProgressEvent,
	tuigateway.SubagentToolEvent.Name(): tuigateway.SubagentToolEvent, tuigateway.SubagentCompleteEvent.Name(): tuigateway.SubagentCompleteEvent,
}

// Moves one subagent of the turn by its lifecycle event
func (h *eventHandler) applySubagentEvent(kind string, p tuigateway.SubagentEventPayload) {
	switch kind {
	case "subagent.spawn_requested", "subagent.start":
		status := "queued"
		if kind == "subagent.start" {
			status = "running"
		}
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			if !isTerminalStatus(base.Status) {
				next.Status = status
			}
		}, true)
		h.maybeNudgeAgents()
		if kind == "subagent.spawn_requested" {
			h.refreshDelegationStatus(h.store.delegation.MaxSpawnDepth == nil)
		}
	case "subagent.thinking", "subagent.progress":
		text := strings.TrimFunc(val(p.Text), isJSSpace)
		if text == "" {
			return
		}
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			next.Status = keepTerminalElseRunning(base.Status)
			if kind == "subagent.thinking" {
				next.Thinking = pushUnique(base.Thinking, text, thinkingTailSize)
			} else {
				next.Notes = pushUnique(base.Notes, text, notesTailSize)
			}
		}, false)
	case "subagent.tool":
		preview := val(p.ToolPreview)
		if p.ToolPreview == nil {
			preview = val(p.Text)
		}
		line := formatToolCall(firstText(val(p.ToolName), "delegate_task"), preview)
		h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			next.Status = keepTerminalElseRunning(base.Status)
			next.Tools = pushUnique(base.Tools, line, toolsTailSize)
		}, false)
	case "subagent.complete":
		finished := h.turn.upsertSubagent(p, func(base subagentProgress, next *subagentProgress) {
			next.DurationSeconds = base.DurationSeconds
			if p.DurationSeconds != nil {
				next.DurationSeconds = p.DurationSeconds
			}
			next.Status = normalizeSubagentStatus(string(val(p.Status)), "completed")
			next.Summary = firstText(val(p.Summary), firstText(val(p.Text), base.Summary))
		}, false)
		if finished >= 0 {
			h.queue(h.persistSpawnTree(h.store.spawnHistory[finished]))
		}
	}
}

// Typed payload of an event, a misfit printed as an invalid response naming the field
func eventPayload[T any](h *eventHandler, ev tuigateway.Event[T], p map[string]any) (T, bool) {
	payload, err := ev.Payload(p)
	if err != nil {
		h.misfit(ev.Name(), err)
		return payload, false
	}
	return payload, true
}

// Prints an event payload or request that does not fit its contract as an invalid response naming the field
func (h *eventHandler) misfit(name string, err error) {
	h.app.sys("error: " + misfitText(tr("session.common.invalidResponse", name)) + " " + err.Error())
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
func (h *eventHandler) confirmBillingWall(block *tuigateway.BillingBlock) {
	if block == nil {
		return
	}
	copy := billingDialogCopy(block)
	h.store.overlay.Confirm = &confirmReq{CancelLabel: copy.CancelLabel, ConfirmLabel: copy.ConfirmLabel, Detail: copy.Detail, Title: copy.Title, OnConfirm: func() tea.Cmd {
		if url := val(block.BillingURL); url != "" {
			openExternalURL(url)
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

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hard interrupt of a running agent, swapped by tests
var requestHardInterrupt = func(a *agent.AIAgent) { agent.RequestHardInterrupt(a, "", "") }

// Sleeps for a number of seconds
func sleepSeconds(s float64) { time.Sleep(time.Duration(s * float64(time.Second))) }

// Current connection card state
func (c *CLI) connectionModal() *connectionModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	st, _ := c.connectionState.(*connectionModalState)
	return st
}

// Reports whether an agent exists and a turn is running
func (c *CLI) agentTurnRunning() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.agentRunning && c.agent != nil
}

// Toggles voice recording while voice mode is on, the heavy work done off the loop
func (c *CLI) tuiHandleVoiceRecord(e *ptEvent) {
	c.voiceLock.Lock()
	voiceMode, recording, processing := c.voiceMode, c.voiceRecording, c.voiceProcessing
	c.voiceLock.Unlock()
	if !voiceMode {
		return
	}
	if recording {
		c.voiceLock.Lock()
		c.voiceContinuous = false
		c.voiceLock.Unlock()
		_ = e.app.Invalidate()
		go c.voiceStopAndTranscribe()
		return
	}
	c.uiMu.Lock()
	agentRunning := c.agentRunning
	blocked := c.clarifyState != nil || c.sudoState != nil || c.approvalState != nil || c.slashConfirmState != nil || c.connectionState != nil
	c.uiMu.Unlock()
	if agentRunning || processing {
		c.voiceLock.Lock()
		c.voiceContinuous = false
		c.voiceLock.Unlock()
		_ = e.app.Invalidate()
		return
	}
	if blocked {
		return
	}
	if c.voiceTTSDone != nil && !c.voiceTTSDone.IsSet() {
		cliLogger.Info("TTS CUT: record key handler cutting TTS")
		media.MarkSpeechInterrupted()
		if c.voiceTTSStop != nil {
			c.voiceTTSStop.Set()
		}
		media.StopPlayback()
		c.voiceTTSDone.Set()
	}
	c.voiceLock.Lock()
	c.voiceContinuous = true
	c.voiceLock.Unlock()
	go func() {
		if err := c.voiceStartRecording(); err != nil {
			cprint("\n" + dimANSI + c.t("cli.tui.voice_recording_failed", map[string]any{"error": err}) + resetANSI)
			return
		}
		if c.app != nil {
			_ = c.app.Invalidate()
		}
	}()
	_ = e.app.Invalidate()
}

// Cancels an active recording, true when one was cancelled
func (c *CLI) tuiCancelVoiceRecording(e *ptEvent) bool {
	c.voiceLock.Lock()
	var rec voiceRecorder
	if c.voiceRecording {
		if rec = c.voiceRecorderHandle(); rec != nil {
			c.voiceRecording, c.voiceContinuous = false, false
		}
	}
	c.voiceLock.Unlock()
	if rec == nil {
		return false
	}
	cprint("\n" + dimANSI + c.t("cli.tui.recording_cancelled", nil) + resetANSI)
	go rec.Cancel()
	_ = e.app.Invalidate()
	return true
}

// tuiCloser pairs a foreground state probe with the function closing it
type tuiCloser struct {
	open  func() bool
	close func()
}

// Closes the first open foreground overlay, true when one closed
func (c *CLI) tuiCancelForegroundUI(e *ptEvent, closers []tuiCloser) bool {
	for _, closer := range closers {
		if closer.open() {
			closer.close()
			e.app.buffer.reset(false)
			_ = e.app.Invalidate()
			return true
		}
	}
	return false
}

// Reports whether a slash confirmation is open
func (c *CLI) slashConfirmOpen() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.slashConfirmState != nil
}

// Reports whether the model picker is open
func (c *CLI) modelPickerOpen() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.modelPickerState != nil
}

// Reports whether the command palette is open
func (c *CLI) commandPaletteOpen() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.commandPaletteState != nil
}

// Clears every overlay that blocks the agent, true when one was open
func (c *CLI) tuiClearBlockingOverlays(e *ptEvent) bool {
	c.uiMu.Lock()
	any := c.sudoState != nil || c.secretState != nil || c.approvalState != nil || c.clarifyState != nil || c.connectionState != nil
	c.uiMu.Unlock()
	if !any {
		return false
	}
	c.clearActiveOverlaysForInterrupt()
	e.app.buffer.reset(false)
	_ = e.app.Invalidate()
	return true
}

// Clears typed text and images, exiting when both are already empty
func (c *CLI) tuiClearOrExit(e *ptEvent) {
	if e.app.buffer.text() != "" || len(c.attachedImages) > 0 {
		e.app.buffer.reset(false)
		c.attachedImages = nil
		_ = e.app.Invalidate()
		return
	}
	c.setShouldExit(false)
	e.app.Exit()
}

// Marks the run as ending, detaching from a running turn when asked
func (c *CLI) setShouldExit(detach bool) {
	c.uiMu.Lock()
	c.shouldExit = true
	if detach {
		c.detachOnExit = c.agentRunning
	}
	c.uiMu.Unlock()
}

// Ctrl C cancels recording, overlays or a draft, then interrupts the turn, a second press within two seconds exiting
func (c *CLI) tuiHandleCtrlC(e *ptEvent) {
	now := wallClock()
	if c.tuiCancelVoiceRecording(e) {
		return
	}
	if c.tuiCancelForegroundUI(e, []tuiCloser{
		{c.slashConfirmOpen, func() { c.submitSlashConfirmResponse("cancel") }},
		{c.modelPickerOpen, c.closeModelPicker},
		{c.commandPaletteOpen, c.closeCommandPalette}}) {
		return
	}
	cleared := c.tuiClearBlockingOverlays(e)
	running := c.agentTurnRunning()
	if cleared && !running {
		return
	}
	if running && e.app.buffer.text() == "" && len(c.attachedImages) == 0 {
		if now-c.lastCtrlCTime < 2.0 {
			fmt.Fprintln(console().Out, "\n"+c.t("cli.tui.force_exiting", nil))
			c.setShouldExit(false)
			e.app.Exit()
			return
		}
		c.lastCtrlCTime = now
		fmt.Fprintln(console().Out, "\n"+c.t("cli.tui.interrupting_again_hint", nil))
		requestHardInterrupt(c.agent)
		return
	}
	c.tuiClearOrExit(e)
}

// Ctrl Q acts like Ctrl C without the double press exit and leaves the palette open
func (c *CLI) tuiHandleCtrlQ(e *ptEvent) {
	if c.tuiCancelVoiceRecording(e) {
		return
	}
	if c.tuiCancelForegroundUI(e, []tuiCloser{
		{c.slashConfirmOpen, func() { c.submitSlashConfirmResponse("cancel") }},
		{c.modelPickerOpen, c.closeModelPicker}}) {
		return
	}
	cleared := c.tuiClearBlockingOverlays(e)
	running := c.agentTurnRunning()
	if cleared && !running {
		return
	}
	if running && e.app.buffer.text() == "" && len(c.attachedImages) == 0 {
		fmt.Fprintln(console().Out, "\n"+c.t("cli.tui.interrupting", nil))
		requestHardInterrupt(c.agent)
		return
	}
	c.tuiClearOrExit(e)
}

// Builds the number key handler picking or toggling clarify choice idx
func (c *CLI) tuiMakeClarifyNumberHandler(idx int) ptHandler {
	return func(e *ptEvent) {
		c.uiMu.Lock()
		state, freetext := c.clarifyState, c.clarifyFreetext
		c.uiMu.Unlock()
		if state == nil || freetext || idx > len(state.Choices) {
			return
		}
		switch {
		case state.MultiSelect:
			if state.SelectedIndices == nil {
				state.SelectedIndices = map[int]bool{}
			}
			if state.SelectedIndices[idx] {
				delete(state.SelectedIndices, idx)
			} else {
				state.SelectedIndices[idx] = true
			}
		case idx == len(state.Choices):
			c.uiMu.Lock()
			c.clarifyFreetext = true
			c.uiMu.Unlock()
		default:
			answer := state.Choices[idx]
			c.clarifyBatchLock(state, &answer, nil)
		}
		_ = e.app.Invalidate()
	}
}

// Puts a popped stash payload back into the composer, its images added to the attached ones
func (c *CLI) tuiRestoreStashPayload(e *ptEvent, payload *config.StashPayload) {
	if payload == nil {
		return
	}
	_ = e.app.buffer.setText(payload.Text)
	e.app.buffer.setCursorPosition(len([]rune(payload.Text)))
	for _, img := range payload.Images {
		path := cliStr(img, "")
		if indexOf(c.attachedImages, path) < 0 {
			c.attachedImages = append(c.attachedImages, path)
		}
	}
}

func (c *CLI) tuiHandleStashPanelUp(e *ptEvent) {
	c.promptStash.MoveCursor(-1)
	_ = e.app.Invalidate()
}

func (c *CLI) tuiHandleStashPanelDown(e *ptEvent) {
	c.promptStash.MoveCursor(1)
	_ = e.app.Invalidate()
}

// Discards the highlighted stashed draft
func (c *CLI) tuiHandleStashPanelDelete(e *ptEvent) {
	c.promptStash.DeleteAtCursor()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiHandleStashPanelClose(e *ptEvent) {
	c.promptStash.ClosePanel()
	_ = e.app.Invalidate()
}

// Tab accepts the open completion, else the ghost suggestion, else starts completions
func (c *CLI) tuiHandleTab(e *ptEvent) {
	buf := e.currentBuffer()
	switch {
	case buf.completeState != nil:
		completion := buf.completeState.current()
		if completion == nil {
			zero := 0
			buf.goToCompletion(&zero)
			if buf.completeState != nil {
				completion = buf.completeState.current()
			}
		}
		if completion == nil {
			return
		}
		buf.applyCompletion(*completion)
	case buf.suggestion != nil && *buf.suggestion != "":
		buf.insertText(*buf.suggestion, false, true, true)
	default:
		buf.startCompletion(false, false, false)
	}
}

// Double escape discards the draft and attached images, the draft kept in history
func (c *CLI) tuiHandleDoubleEscape(e *ptEvent) {
	buf := e.app.buffer
	if buf.text() == "" && len(c.attachedImages) == 0 {
		return
	}
	buf.reset(buf.text() != "")
	c.attachedImages = nil
	_ = e.app.Invalidate()
}

// Consumes ignored terminal sequences, a focus in report scheduling a rate limited full repaint
func (c *CLI) tuiHandleIgnoredTerminalSequence(e *ptEvent) {
	for _, press := range e.keySequence {
		if press.Data == "\x1b[I" {
			c.scheduleFocusRegainRedraw(1.0)
			break
		}
	}
}

// Escape cancels the active connection, secret, sudo or slash confirm prompt
func (c *CLI) tuiHandleEscapeModal(e *ptEvent) {
	c.uiMu.Lock()
	connection, secret, sudo, slash := c.connectionState != nil, c.secretState != nil, c.sudoState, c.slashConfirmState != nil
	c.uiMu.Unlock()
	switch {
	case connection:
		c.connectionCancel()
		e.app.buffer.reset(false)
	case secret:
		c.cancelSecretCapture()
		e.app.buffer.reset(false)
	case sudo != nil:
		offer(sudo.response, "")
		c.uiMu.Lock()
		c.sudoState = nil
		c.uiMu.Unlock()
	case slash:
		c.submitSlashConfirmResponse("cancel")
		e.app.buffer.reset(false)
	default:
		return
	}
	_ = e.app.Invalidate()
}

// Platform and process hooks the suspend handler goes through
var (
	tuiGOOS         = runtime.GOOS
	suspendSelf     = suspendProcessGroup
	suspendNotice   = func(msg string) { _, _ = os.Stdout.WriteString(msg) }
	wallClockFormat = "15:04:05"
)

// Ctrl Z suspends the process, unsupported on Windows
func (c *CLI) tuiHandleCtrlZ(e *ptEvent) {
	if tuiGOOS == "windows" {
		cprint("\n" + dimANSI + c.t("cli.tui.suspend_unsupported", nil) + resetANSI)
		_ = e.app.Invalidate()
		return
	}
	name := config.GetActiveSkin(c.ctx).GetBranding("agent_name", "Icebath Agent")
	msg := "\n" + c.t("cli.tui.suspended", map[string]any{"agent_name": name})
	e.app.RunInTerminal(func() {
		suspendNotice(msg)
		_ = suspendSelf()
	})
}

// Ctrl D deletes under the cursor, exiting only on empty input with no images
func (c *CLI) tuiHandleCtrlD(e *ptEvent) {
	buf := e.app.buffer
	if buf.text() != "" {
		buf.delete(1)
		return
	}
	if len(c.attachedImages) == 0 {
		c.setShouldExit(true)
		e.app.Exit()
	}
}

// Runs a history move with paste collapsing off, the flag cleared when the text did not change
func (c *CLI) tuiRecallWithoutRecollapse(buf *ptBuffer, move func()) {
	before := buf.text()
	c.tui.skipPasteCollapse = true
	move()
	if buf.text() == before {
		c.tui.skipPasteCollapse = false
	}
}

// Alt V attaches a clipboard image
func (c *CLI) tuiHandleAltV(e *ptEvent) {
	if c.tryAttachClipboardImage() {
		_ = e.app.Invalidate()
	}
}

// Ctrl V attaches a clipboard image on terminals without bracketed paste
func (c *CLI) tuiHandleCtrlV(e *ptEvent) {
	if c.tryAttachClipboardImage() {
		_ = e.app.Invalidate()
	}
}

// Ctrl L forces a clean repaint
func (c *CLI) tuiHandleCtrlL(*ptEvent) { c.forceFullRedraw() }

// Inserts a newline for multi line input
func (c *CLI) tuiInsertNewline(e *ptEvent) { e.currentBuffer().insertText("\n", false, true, true) }

// Opens the draft in an external editor
func (c *CLI) tuiHandleOpenInEditor(e *ptEvent) { c.openExternalEditor(e.currentBuffer()) }

// Moves the connection card selection by delta, saving the field being edited
func (c *CLI) tuiConnectionMove(e *ptEvent, delta int) {
	state := c.connectionModal()
	if state == nil {
		return
	}
	switch state.Phase {
	case "form", "failed":
		switch {
		case state.FieldIndex < len(state.Fields):
			name := cliStr(state.Target["name"], "")
			if state.Drafts[name] == nil {
				state.Drafts[name] = map[string]string{}
			}
			state.Drafts[name][cliStr(state.Fields[state.FieldIndex]["name"], "")] = e.app.buffer.text()
			state.FieldIndex = min(len(state.Fields), max(0, state.FieldIndex+delta))
		case delta < 0 && len(state.Fields) > 0:
			state.FieldIndex = len(state.Fields) - 1
		default:
			state.Selected = pyMod(state.Selected+delta, 2)
		}
		c.connectionSyncInputBuffer()
	case "authorized":
		state.Selected = pyMod(state.Selected+delta, 2)
	}
	c.paintNow()
	_ = e.app.Invalidate()
}

// Remainder with the sign of the divisor
func pyMod(a, b int) int { return ((a % b) + b) % b }

func (c *CLI) tuiConnectionUp(e *ptEvent)   { c.tuiConnectionMove(e, -1) }
func (c *CLI) tuiConnectionDown(e *ptEvent) { c.tuiConnectionMove(e, 1) }

// Flips between the two actions of the connection card
func (c *CLI) tuiConnectionSide(e *ptEvent) {
	state := c.connectionModal()
	if state == nil {
		return
	}
	if state.Phase == "authorized" || ((state.Phase == "form" || state.Phase == "failed") && state.FieldIndex >= len(state.Fields)) {
		state.Selected = 1 - min(1, max(0, state.Selected))
		c.paintNow()
		_ = e.app.Invalidate()
	}
}

// Open model picker state
func (c *CLI) modelPicker() map[string]any {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	st, _ := c.modelPickerState.(map[string]any)
	return st
}

// Moves the model picker selection down within its stage
func (c *CLI) tuiModelPickerDown(e *ptEvent) {
	state := c.modelPicker()
	if state == nil {
		return
	}
	var maxIdx int
	switch cliStr(state["stage"], "") {
	case "provider":
		providers, _ := state["providers"].([]map[string]any)
		maxIdx = len(providers)
	case "reasoning":
		maxIdx = len(pickerReasoningRows()) + 1
	default:
		if pairs, ok := state["_filtered_pairs"].([]pickerEntry); ok {
			maxIdx = len(pairs) + 1
		} else {
			modelList, _ := state["model_list"].([]string)
			maxIdx = len(modelList) + 1
		}
	}
	state["selected"] = min(maxIdx, stateInt(state, "selected", 0)+1)
	_ = e.app.Invalidate()
}

// Moves the model picker selection up
func (c *CLI) tuiModelPickerUp(e *ptEvent) {
	if state := c.modelPicker(); state != nil {
		state["selected"] = max(0, stateInt(state, "selected", 0)-1)
		_ = e.app.Invalidate()
	}
}

// Replaces a picker filter and rewinds its selection and viewport
func tuiSetFilter(state map[string]any, value string) {
	state["filter"], state["selected"], state["_scroll_offset"] = value, 0, 0
}

// Escape clears the picker filter, else steps back from the effort stage, else closes the picker
func (c *CLI) tuiModelPickerEscape(e *ptEvent) {
	state := c.modelPicker()
	if state != nil && cliStr(state["stage"], "") == "model" && cliStr(state["filter"], "") != "" {
		tuiSetFilter(state, "")
		_ = e.app.Invalidate()
		return
	}
	if state != nil && cliStr(state["stage"], "") == "reasoning" {
		state["stage"], state["selected"], state["_scroll_offset"], state["switch_result"] = "model", 0, 0, nil
		_ = e.app.Invalidate()
		return
	}
	c.closeModelPicker()
	e.app.buffer.reset(false)
	_ = e.app.Invalidate()
}

// Text without its last character
func dropLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

func (c *CLI) tuiModelPickerFilterBackspace(e *ptEvent) {
	if state := c.modelPicker(); state != nil {
		tuiSetFilter(state, dropLastRune(cliStr(state["filter"], "")))
		_ = e.app.Invalidate()
	}
}

// Builds the handler typing ch into the model picker filter
func (c *CLI) tuiMakeModelFilterCharHandler(ch string) ptHandler {
	return func(e *ptEvent) {
		state := c.modelPicker()
		if state == nil || cliStr(state["stage"], "") != "model" {
			return
		}
		tuiSetFilter(state, cliStr(state["filter"], "")+ch)
		_ = e.app.Invalidate()
	}
}

// Replaces the palette filter and rewinds its selection and viewport
func (s *commandPaletteModalState) setFilter(value string) {
	s.Filter, s.Selected, s.ScrollOffset = value, 0, 0
}

// Builds the handler typing ch into the palette filter
func (c *CLI) tuiMakePaletteCharHandler(ch string) ptHandler {
	return func(e *ptEvent) {
		if state := c.commandPaletteState; state != nil {
			state.setFilter(state.Filter + ch)
			_ = e.app.Invalidate()
		}
	}
}

// Builds the number key handler picking approval choice idx
func (c *CLI) tuiMakeApprovalNumberHandler(idx int) ptHandler {
	return func(e *ptEvent) {
		c.uiMu.Lock()
		state := c.approvalState
		ok := state != nil && idx < len(state.Choices)
		if ok {
			state.Selected = idx
		}
		c.uiMu.Unlock()
		if ok {
			c.handleApprovalSelection()
			_ = e.app.Invalidate()
		}
	}
}

// Builds the number key handler answering a slash confirmation with choice idx
func (c *CLI) tuiMakeSlashConfirmNumberHandler(idx int) ptHandler {
	return func(e *ptEvent) {
		c.uiMu.Lock()
		state := c.slashConfirmState
		c.uiMu.Unlock()
		if state != nil && idx < len(state.Choices) {
			c.submitSlashConfirmResponse(state.Choices[idx].Value)
			e.app.buffer.reset(false)
			_ = e.app.Invalidate()
		}
	}
}

// Current clarify panel state
func (c *CLI) clarifyModal() *clarifyModalState {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.clarifyState
}

// Toggles the checkbox under the clarify cursor
func (c *CLI) tuiClarifyToggle(e *ptEvent) {
	if state := c.clarifyModal(); state != nil {
		if state.SelectedIndices == nil {
			state.SelectedIndices = map[int]bool{}
		}
		if state.SelectedIndices[state.Selected] {
			delete(state.SelectedIndices, state.Selected)
		} else {
			state.SelectedIndices[state.Selected] = true
		}
		_ = e.app.Invalidate()
	}
}

func (c *CLI) tuiClarifyDown(e *ptEvent) {
	if state := c.clarifyModal(); state != nil {
		state.Selected = min(len(state.Choices), state.Selected+1)
		_ = e.app.Invalidate()
	}
}

func (c *CLI) tuiClarifyUp(e *ptEvent) {
	if state := c.clarifyModal(); state != nil {
		state.Selected = max(0, state.Selected-1)
		_ = e.app.Invalidate()
	}
}

// Moves the active clarify question by delta, wrapping around
func (c *CLI) tuiClarifyBatchStep(e *ptEvent, delta int) {
	if state := c.clarifyModal(); state != nil {
		c.clarifyBatchSetActive(state, pyMod(state.Active+delta, len(state.Questions)))
		_ = e.app.Invalidate()
	}
}

func (c *CLI) tuiClarifyBatchTab(e *ptEvent)     { c.tuiClarifyBatchStep(e, 1) }
func (c *CLI) tuiClarifyBatchBacktab(e *ptEvent) { c.tuiClarifyBatchStep(e, -1) }

func (c *CLI) tuiCommandPaletteBackspace(e *ptEvent) {
	if state := c.commandPaletteState; state != nil {
		state.setFilter(dropLastRune(state.Filter))
		_ = e.app.Invalidate()
	}
}

func (c *CLI) tuiCommandPaletteDown(e *ptEvent) {
	if state := c.commandPaletteState; state != nil {
		n := len(c.commandPaletteVisibleEntries())
		if state.VisibleCount != nil {
			n = *state.VisibleCount
		}
		state.Selected = min(max(0, n-1), state.Selected+1)
		_ = e.app.Invalidate()
	}
}

func (c *CLI) tuiCommandPaletteUp(e *ptEvent) {
	if state := c.commandPaletteState; state != nil {
		state.Selected = max(0, state.Selected-1)
		_ = e.app.Invalidate()
	}
}

func (c *CLI) tuiCommandPaletteEnter(e *ptEvent) {
	c.handleCommandPaletteSelection()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiCommandPaletteEscape(e *ptEvent) {
	c.closeCommandPalette()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiOpenCommandPalette(e *ptEvent) {
	c.openCommandPalette()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiSlashConfirmDown(e *ptEvent) {
	c.uiMu.Lock()
	if st := c.slashConfirmState; st != nil {
		st.Selected = min(len(st.Choices)-1, st.Selected+1)
	}
	c.uiMu.Unlock()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiSlashConfirmUp(e *ptEvent) {
	c.uiMu.Lock()
	if st := c.slashConfirmState; st != nil {
		st.Selected = max(0, st.Selected-1)
	}
	c.uiMu.Unlock()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiApprovalDown(e *ptEvent) {
	c.uiMu.Lock()
	if st := c.approvalState; st != nil {
		st.Selected = min(len(st.Choices)-1, st.Selected+1)
	}
	c.uiMu.Unlock()
	_ = e.app.Invalidate()
}

func (c *CLI) tuiApprovalUp(e *ptEvent) {
	c.uiMu.Lock()
	if st := c.approvalState; st != nil {
		st.Selected = max(0, st.Selected-1)
	}
	c.uiMu.Unlock()
	_ = e.app.Invalidate()
}

// Starts the wake word listener off the main goroutine, a failure only logged
func (c *CLI) tuiWakeStartup() {
	if err := c.maybeStartWakeWord(); err != nil {
		cliLogger.Debug("wake-word startup skipped", "error", err)
	}
}

// Reports whether a loop error is one of the known harmless ones of a closing terminal
func (c *CLI) tuiSuppressClosedLoopErrors(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Event loop is closed") || strings.Contains(msg, "is not registered") || isEIO(err)
}

// Enter answers an open overlay, continues a backslash line, treats a burst Enter as a newline, else submits
func (c *CLI) tuiHandleEnter(e *ptEvent) {
	if c.tuiEnterOverlay(e) {
		return
	}
	buf := e.app.buffer
	raw := buf.text()
	if c.tui.multilineShortcuts && buf.cursor == len([]rune(raw)) && isBackslashLineContinuation(raw) {
		continued := applyBackslashLineContinuation(raw)
		_ = buf.setText(continued)
		buf.setCursorPosition(len([]rune(continued)))
		_ = e.app.Invalidate()
		return
	}
	if !handleBurstEnter(c, e, buf) {
		c.tuiSubmitBuffer(e, buf)
	}
}

// Submits the composer through inline commands, busy routing or the pending input queue
func (c *CLI) tuiSubmitBuffer(e *ptEvent, buf *ptBuffer) {
	text := strings.TrimSpace(buf.text())
	hasImages := len(c.attachedImages) > 0
	if text == "" && !hasImages {
		return
	}
	inline := c.tuiEnterInlineCommand
	if c.tui.inlineCommandHook != nil {
		inline = c.tui.inlineCommandHook
	}
	if inline(e, text, hasImages) {
		return
	}
	images := append([]string{}, c.attachedImages...)
	c.attachedImages = nil
	_ = e.app.Invalidate()
	var payload any = text
	if len(images) > 0 {
		payload = submittedInput{Text: text, Images: images}
	}
	local := text != "" && (commands.LooksLikeSlashCommand(text) || strings.HasPrefix(strings.TrimSpace(text), "!"))
	c.uiMu.Lock()
	running := c.agentRunning
	c.uiMu.Unlock()
	if running && !local {
		busy := c.tuiEnterWhileBusy
		if c.tui.enterWhileBusyHook != nil {
			busy = c.tui.enterWhileBusyHook
		}
		busy(text, images, payload, "")
	} else {
		c.pendingInput.Put(payload)
	}
	c.inlinePastes(buf)
	buf.reset(true)
}

// submittedInput is a typed message with its attached images
type submittedInput struct {
	Text   string
	Images []string
}

// Runs model, steer, background and stop commands on the loop, true when the text was one of them
func (c *CLI) tuiEnterInlineCommand(e *ptEvent, text string, hasImages bool) bool {
	switch {
	case c.shouldHandleModelCommandInline(text, hasImages):
		if !c.processCommand(text) {
			c.setShouldExit(false)
			if e.app.IsRunning() {
				e.app.Exit()
			}
		}
	case c.shouldHandleSteerCommandInline(text, hasImages), c.shouldHandleBackgroundCommandInline(text, hasImages), c.shouldHandleStopCommandInline(text, hasImages):
		c.processCommand(text)
	default:
		return false
	}
	e.app.buffer.reset(true)
	_ = e.app.Invalidate()
	return true
}

// First 80 characters of a text with an ellipsis when longer
func preview80(text string) string {
	r := []rune(text)
	if len(r) > 80 {
		return string(r[:80]) + "..."
	}
	return text
}

// Routes a submission typed while the agent runs by busy input mode, steer, interrupt or queue
func (c *CLI) tuiEnterWhileBusy(text string, images []string, payload any, mode string) {
	effective := mode
	if effective == "" {
		effective = c.busyInputMode
	}
	redirected := false
	if effective == "steer" {
		if len(images) > 0 || text == "" {
			effective = "queue"
		} else if c.agent != nil && c.agent.Steer(text) {
			cprint("  " + accentANSI.String() + c.t("cli.tui.steered", map[string]any{"preview": preview80(text)}) + resetANSI)
		} else {
			effective = "queue"
		}
	}
	switch effective {
	case "queue":
		c.pendingInput.Put(payload)
		preview := text
		if preview == "" {
			preview = tn(c.ctx, "cli.tui.images_attached_preview", len(images), nil)
		}
		cprint("  " + c.t("cli.tui.queued_next_turn", map[string]any{"preview": preview80(preview)}))
	case "interrupt":
		if len(images) == 0 && text != "" && c.agent != nil {
			redirected = c.agent.Redirect(text)
		}
		if redirected {
			cprint("  " + accentANSI.String() + c.t("cli.tui.redirected_turn", map[string]any{"preview": preview80(text)}) + resetANSI)
		} else {
			c.interruptQueue.Put(payload)
			c.uiMu.Lock()
			running := c.agentRunning
			c.uiMu.Unlock()
			line := fmt.Sprintf("%s ENTER: queued interrupt msg=%s, agent_running=%s\n", time.Now().Format(wallClockFormat),
				pyReprPrefix(payload, 60), pyBool(running))
			if f, err := os.OpenFile(filepath.Join(home.Dir(c.ctx), "interrupt_debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				_, _ = f.WriteString(line)
				_ = f.Close()
			}
		}
	}
	cfg := CLIConfig(c.ctx)
	if !tools.IsSeen(cfg, tools.OnboardingBusyInputFlag) {
		hintMode := effective
		if redirected {
			hintMode = "redirect"
		}
		cprint("  " + dimANSI + tools.BusyInputHintCLI(hintMode) + resetANSI)
		tools.MarkSeen(c.ctx, home.ConfigPath(home.Dir(c.ctx)), tools.OnboardingBusyInputFlag)
		onboarding, _ := cfg["onboarding"].(map[string]any)
		if onboarding == nil {
			onboarding = map[string]any{}
			cfg["onboarding"] = onboarding
		}
		seen, _ := onboarding["seen"].(map[string]any)
		if seen == nil {
			seen = map[string]any{}
			onboarding["seen"] = seen
		}
		seen[tools.OnboardingBusyInputFlag] = true
	}
}

// Python spelling of a boolean
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Enter while a modal overlay is up submits it, true when one was
func (c *CLI) tuiEnterOverlay(e *ptEvent) bool {
	buf := e.app.buffer
	c.uiMu.Lock()
	sudo, secret, approval, slash := c.sudoState, c.secretState != nil, c.approvalState != nil, c.slashConfirmState
	picker := c.modelPickerState != nil
	clarify, freetext := c.clarifyState != nil, c.clarifyFreetext
	c.uiMu.Unlock()
	if conn := c.connectionModal(); conn != nil {
		if (conn.Phase == "form" || conn.Phase == "failed") && conn.FieldIndex < len(conn.Fields) {
			value := buf.text()
			buf.reset(false)
			c.connectionSetField(value)
		} else {
			c.connectionSubmit()
		}
		_ = e.app.Invalidate()
		return true
	}
	switch {
	case sudo != nil:
		offer(sudo.response, buf.text())
		c.uiMu.Lock()
		c.sudoState = nil
		c.uiMu.Unlock()
	case secret:
		value := buf.text()
		buf.reset(false)
		c.submitSecretResponse(value)
	case approval:
		c.handleApprovalSelection()
	case slash != nil:
		text := strings.TrimSpace(buf.text())
		choice := ""
		if text != "" {
			choice = c.normalizeSlashConfirmChoice(text, slash.Choices)
		}
		if choice == "" && slash.Selected >= 0 && slash.Selected < len(slash.Choices) {
			choice = slash.Choices[slash.Selected].Value
		}
		if choice == "" {
			choice = "cancel"
		}
		c.submitSlashConfirmResponse(choice)
		buf.reset(false)
	case picker:
		persist := provider.ResolvePersistBehavior(c.ctx, false, false, false, "")
		if err := c.handleModelPickerSelection(persist); err != nil {
			cprint("  " + c.t("cli.tui.model_selection_failed", map[string]any{"error": err}))
			c.closeModelPicker()
		}
		buf.reset(false)
	case clarify && freetext:
		c.tuiEnterClarifyFreetext(e)
		return true
	case clarify:
		c.tuiEnterClarifyChoice(e)
		return true
	default:
		return false
	}
	_ = e.app.Invalidate()
	return true
}

// Locks the typed clarify answer, empty input skipping the question
func (c *CLI) tuiEnterClarifyFreetext(e *ptEvent) {
	buf := e.app.buffer
	text := strings.TrimSpace(buf.text())
	state := c.clarifyModal()
	c.uiMu.Lock()
	base := c.clarifyMultiBase
	c.clarifyFreetext, c.clarifyPrefill, c.clarifyMultiBase = false, "", nil
	c.uiMu.Unlock()
	switch {
	case text == "" && base != nil && len(*base) > 0:
		answer := jsonLabels(*base)
		c.clarifyBatchLock(state, &answer, &clarifyAnswerMeta{Kind: "multi", Choices: append([]string{}, *base...)})
	case text == "":
		c.clarifyBatchLock(state, nil, &clarifyAnswerMeta{Kind: "skipped"})
	case base != nil:
		answer := jsonLabels(append(append([]string{}, *base...), text))
		c.clarifyBatchLock(state, &answer, &clarifyAnswerMeta{Kind: "multi", Choices: append([]string{}, *base...), OtherText: text})
	default:
		c.clarifyBatchLock(state, &text, &clarifyAnswerMeta{Kind: "other", OtherText: text})
	}
	buf.reset(false)
	_ = e.app.Invalidate()
}

// Locks the active clarify choice, an earlier typed answer filled back into the composer for editing
func (c *CLI) tuiEnterClarifyChoice(e *ptEvent) {
	c.clarifyBatchEnter(c.clarifyModal())
	c.uiMu.Lock()
	prefill := ""
	if c.clarifyFreetext {
		prefill, c.clarifyPrefill = c.clarifyPrefill, ""
	}
	c.uiMu.Unlock()
	if prefill != "" {
		_ = e.app.buffer.setText(prefill)
		e.app.buffer.setCursorPosition(len([]rune(prefill)))
	}
	_ = e.app.Invalidate()
}

// Saves a large paste under the pastes directory and returns the reference shown in the buffer
func (c *CLI) tuiCollapsePaste(text string, lineCount int, fallback bool) string {
	c.tui.pasteCounter++
	dir := filepath.Join(home.Dir(c.ctx), "pastes")
	_ = os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, fmt.Sprintf("paste_%d_%s.txt", c.tui.pasteCounter, time.Now().Format("150405")))
	_ = os.WriteFile(file, []byte(text), 0o644)
	suffix := ""
	if fallback {
		suffix = " (fallback)"
	}
	cliLogger.Info(fmt.Sprintf("Collapsed paste #%d: %d lines, %d chars -> %s%s", c.tui.pasteCounter, lineCount+1, len([]rune(text)), file, suffix))
	c.tui.pasteJustCollapsed = true
	return fmt.Sprintf("[Pasted text #%d: %d lines → %s]", c.tui.pasteCounter, lineCount+1, file)
}

// Reports whether a paste crosses the line or character collapse threshold
func (c *CLI) tuiPasteOverThreshold(text string, lineCount int, thresholdKey string) bool {
	threshold := intOr(mapValueOr(c.config, thresholdKey, 5), 5)
	charThreshold := intOr(mapValueOr(c.config, "paste_collapse_char_threshold", 2000), 2000)
	return (threshold > 0 && lineCount >= threshold) || (charThreshold > 0 && len([]rune(text)) >= charThreshold)
}

// Seconds a paste handler may take before it is logged as slow
const slowPasteMillis = 500.0

// Bracketed paste strips leaked terminal replies, attaches a clipboard image for an empty paste and collapses large text
func (c *CLI) tuiHandlePaste(e *ptEvent) {
	started := time.Now()
	rawSize := len([]rune(e.data()))
	pasted := strings.ReplaceAll(strings.ReplaceAll(e.data(), "\r\n", "\n"), "\r", "\n")
	pasted = tools.StripLeakedBracketedPasteWrappers(pasted)
	pasted, hadMouse := stripLeakedTerminalResponsesWithMeta(pasted)
	if hadMouse {
		c.recoverTerminalInputModes("mouse reports leaked into bracketed paste payload")
	}
	if shouldAutoAttachClipboardImageOnPaste(pasted) && c.tryAttachClipboardImage() {
		_ = e.app.Invalidate()
	}
	if pasted != "" {
		pasted = state.SanitizeSurrogates(pasted)
		lineCount := strings.Count(pasted, "\n")
		buf := e.currentBuffer()
		if c.tuiPasteOverThreshold(pasted, lineCount, "paste_collapse_threshold") && !strings.HasPrefix(strings.TrimSpace(buf.text()), "/") {
			reference := c.tuiCollapsePaste(pasted, lineCount, false)
			prefix := ""
			if r := []rune(buf.text()); buf.cursor > 0 && r[buf.cursor-1] != '\n' {
				prefix = "\n"
			}
			buf.insertText(prefix+reference, false, true, true)
		} else {
			buf.insertText(pasted, false, true, true)
		}
	}
	if elapsed := float64(time.Since(started).Microseconds()) / 1000.0; elapsed > slowPasteMillis {
		lines := 0
		if pasted != "" {
			lines = strings.Count(pasted, "\n") + 1
		}
		cliLogger.Warn(fmt.Sprintf("Slow bracketed-paste handler: %.1fms to process %d bytes (%d lines) on %s. If the input becomes unresponsive after this, attach this log line to the bug report.",
			elapsed, rawSize, lines, runtime.GOOS))
	}
}

// Collapses a large paste on terminals without bracketed paste, detected by many characters or newlines in one change
func (c *CLI) tuiOnTextChanged(buf *ptBuffer) {
	c.tui.lastTextChange = noteTextChange(c)
	text := tools.StripLeakedBracketedPasteWrappers(buf.text())
	text, hadMouse := stripLeakedTerminalResponsesWithMeta(text)
	if hadMouse {
		c.recoverTerminalInputModes("mouse reports leaked into prompt buffer")
	}
	if text != buf.text() {
		cursor := min(buf.cursor, len([]rune(text)))
		c.tui.pasteJustCollapsed = true
		_ = buf.setText(text)
		buf.setCursorPosition(cursor)
		c.tui.prevTextLen = len([]rune(text))
		c.tui.prevNewlineCount = strings.Count(text, "\n")
		return
	}
	charsAdded := len([]rune(text)) - c.tui.prevTextLen
	c.tui.prevTextLen = len([]rune(text))
	lineCount := strings.Count(text, "\n")
	if c.tui.pasteJustCollapsed || c.tui.skipPasteCollapse {
		c.tui.pasteJustCollapsed, c.tui.skipPasteCollapse = false, false
		c.tui.prevNewlineCount = lineCount
		return
	}
	newlinesAdded := lineCount - c.tui.prevNewlineCount
	c.tui.prevNewlineCount = lineCount
	isPaste := charsAdded > 1 || newlinesAdded >= 4
	if c.tuiPasteOverThreshold(text, lineCount, "paste_collapse_threshold_fallback") && isPaste && !strings.HasPrefix(text, "/") {
		_ = buf.setText(c.tuiCollapsePaste(text, lineCount, true))
		buf.setCursorPosition(len([]rune(buf.text())))
	}
}

// Ctrl S stashes the draft, restores the only stashed one, or opens or closes the stash panel
func (c *CLI) tuiHandlePromptStash(e *ptEvent) {
	buf := e.app.buffer
	images := make([]any, len(c.attachedImages))
	for i, img := range c.attachedImages {
		images[i] = img
	}
	switch action, payload := config.ResolveCtrlS(c.promptStash, buf.text(), images); action {
	case config.ActionStashed:
		buf.reset(false)
		c.attachedImages = nil
	case config.ActionRestored:
		c.tuiRestoreStashPayload(e, payload)
	}
	_ = e.app.Invalidate()
}

// Enter in the stash panel restores the highlighted draft
func (c *CLI) tuiHandleStashPanelRestore(e *ptEvent) {
	if text, images, ok := c.promptStash.RestoreAtCursor(); ok {
		c.tuiRestoreStashPayload(e, &config.StashPayload{Text: text, Images: images})
	}
	_ = e.app.Invalidate()
}

// Up browses history on the first line, else moves the cursor up
func (c *CLI) tuiHistoryUp(e *ptEvent) {
	buf := e.app.buffer
	c.tuiRecallWithoutRecollapse(buf, func() { buf.autoMoveUp(e.arg()) })
}

// Down browses history on the last line, else moves the cursor down
func (c *CLI) tuiHistoryDown(e *ptEvent) {
	buf := e.app.buffer
	c.tuiRecallWithoutRecollapse(buf, func() { buf.autoMoveDown(e.arg()) })
}

// Badge row for the attached images
func (c *CLI) tuiImageBarFragments() []styledFragment {
	if len(c.attachedImages) == 0 {
		return nil
	}
	return []styledFragment{{"class:image-badge", " " + formatImageAttachmentBadges(c.attachedImages, c.imageCounter, 0) + " "}}
}

func (c *CLI) tuiVoiceStatusFragments() []styledFragment { return c.getVoiceStatusFragments() }

// Spinner line fragments, none while nothing spins
func (c *CLI) tuiSpinnerText() []styledFragment {
	if line := c.renderSpinnerText(); line != "" {
		return []styledFragment{{"class:hint", line}}
	}
	return nil
}

func (c *CLI) tuiSpinnerHeight() int { return c.spinnerWidgetHeight() }

// Height of the hint row, one while a modal prompt or command needs it, else the agent spacer
func (c *CLI) tuiHintHeight() int {
	c.uiMu.Lock()
	needed := c.sudoState != nil || c.secretState != nil || c.approvalState != nil || c.connectionState != nil ||
		c.slashConfirmState != nil || c.clarifyState != nil || c.commandRunning
	c.uiMu.Unlock()
	if needed {
		return 1
	}
	return c.agentSpacerHeight()
}

// Enter arriving this soon after a buffer change is part of a burst of input
const rapidInputEnterWindow = 0.05

// Quiet time that ends a burst of input
const burstEndQuiet = 0.25

// Records a buffer change, input inside the quiet window or behind a held Enter keeping the paste burst alive
func noteTextChange(c *CLI) float64 {
	now := monotonic()
	held := c.tui.heldEnter != nil
	if held || (c.tui.pasteBurstAt != nil && now-*c.tui.pasteBurstAt < burstEndQuiet) {
		c.tui.pasteBurstAt = &now
		c.tui.heldEnter = nil
	}
	return now
}

// Handles an Enter arriving mid burst as a newline, submitting later when it ended the burst
func handleBurstEnter(c *CLI, e *ptEvent, buf *ptBuffer) bool {
	now := monotonic()
	if now-c.tui.lastTextChange >= rapidInputEnterWindow {
		return false
	}
	pastedNewlineBefore := c.tui.pasteBurstAt != nil && now-*c.tui.pasteBurstAt < burstEndQuiet
	c.tui.pasteBurstAt = &now
	before := buf.document()
	buf.insertText("\n", false, true, true)
	app := e.app
	if !app.IsRunning() || len(app.processor.inputQueue) > 0 || pastedNewlineBefore {
		return true
	}
	withNewline := buf.document()
	changedAt := c.tui.lastTextChange
	held := &struct{}{}
	c.tui.heldEnter = held
	settle := func() {
		if c.tui.heldEnter == held {
			c.tui.heldEnter = nil
		}
		if !app.IsRunning() || !buf.document().equal(withNewline) || c.tui.lastTextChange != changedAt {
			return
		}
		_ = buf.setDocument(before.text(), before.cursor, true)
		c.tuiSubmitBuffer(e, buf)
		_ = app.Invalidate()
	}
	var settleOnceInputIsRead func()
	settleOnceInputIsRead = func() {
		if app.inputWaiting() {
			app.CallSoonThreadsafe(settleOnceInputIsRead)
			return
		}
		settle()
	}
	app.callLater(time.Duration(burstEndQuiet*float64(time.Second)), func() {
		app.CallSoonThreadsafe(func() { app.CallSoonThreadsafe(settleOnceInputIsRead) })
	})
	return true
}

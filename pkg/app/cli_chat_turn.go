package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const voiceInputPrefix = "[Voice input — respond concisely and conversationally, " +
	"2-3 sentences max. No code blocks or markdown.] "

var (
	settlePaintDelay         = 150 * time.Millisecond
	interruptPollInterval    = 100 * time.Millisecond
	interruptJoinPoll        = 200 * time.Millisecond
	interruptJoinPolls       = 50
	agentJoinTimeout         = 30 * time.Second
	ttsDrainTimeout          = 120 * time.Second
	ttsReleaseTimeout        = 5 * time.Second
	checkTTSRequirements     = media.CheckTTSRequirements
	streamTTSToSpeaker       = media.StreamTTSToSpeaker
	startThinkingSound       = media.StartThinkingSound
	stopThinkingSound        = media.StopThinkingSound
	loadFallbackChainConfig  = config.LoadUserEffectiveFailClosed
	decideImageInputMode     = tools.DecideImageInputMode
	buildNativeContentParts  = tools.BuildNativeContentParts
	cleanupStaleAsyncClients = provider.CleanupStaleAsyncClients
	ensureTurnCredentials    = (*CLI).ensureRuntimeCredentials
	resolveTurnRoute         = (*CLI).resolveTurnAgentConfig
	initTurnAgent            = (*CLI).initAgent
	runTurnConversation      = func(ctx context.Context, a cliAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
		return a.RunConversation(ctx, message, o)
	}
	interruptTurnAgent = func(a cliAgent, message string) { a.Interrupt() }
)

// queuedInput is the composer message bundled with its image attachments, the payload the prompt submits
type queuedInput = submittedInput

// Python str of the bundled message, the tuple repr hermes writes into its interrupt debug log
func (q submittedInput) String() string {
	paths := make([]string, len(q.Images))
	for i, p := range q.Images {
		paths[i] = "PosixPath(" + pyrepr.StrRepr(p) + ")"
	}
	return "(" + pyrepr.StrRepr(q.Text) + ", [" + strings.Join(paths, ", ") + "])"
}

// Result of the turn so far, nil while the agent goroutine has not returned
func (t *chatTurn) getResult() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result
}

func (t *chatTurn) setResult(result map[string]any) {
	t.mu.Lock()
	t.result = result
	t.mu.Unlock()
}

// Reports whether a turn worker finished without blocking
func turnDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// Provider of the running agent, the CLI's own when no agent holds one
func (c *CLI) turnProvider() string {
	if c.agentRef() != nil && c.agentRef().Provider() != "" {
		return c.agentRef().Provider()
	}
	return c.provider
}

// Model of the running agent, the CLI's own when no agent holds one
func (c *CLI) turnModel() string {
	if c.agentRef() != nil && c.agentRef().Model() != "" {
		return c.agentRef().Model()
	}
	return c.model
}

// Adopts fallback_providers edits made while this chat is open, a torn config keeping the last good chain
func (c *CLI) syncFallbackChainWithConfig() {
	cfg, err := loadFallbackChainConfig(home.ConfigPath(home.Dir(c.ctx)), nil)
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("fallback chain sync skipped (keeping current chain): %s", err))
		return
	}
	c.fallbackModel = provider.GetFallbackChain(cfg)
	if applier, ok := c.agentRef().(fallbackChainApplier); ok {
		applier.ApplyFallbackChain(c.fallbackModel)
	}
}

// Agent that keeps its own fallback chain aligned with the configured one, the AIAgent chain owner
type fallbackChainApplier interface {
	ApplyFallbackChain(chain []map[string]any)
}

var _ fallbackChainApplier = (*agent.AIAgent)(nil)

// Runs one user turn and returns the agent's response, false when the turn produced none
func (c *CLI) chat(message any, images []string, voiceInput bool) (response string, ok bool) {
	skills.SetSecretCaptureCallback(c.secretCaptureCallback)
	c.uiMu.Lock()
	c.lastTurnInterrupted, c.lastTurnFailed = false, false
	c.uiMu.Unlock()
	switch m := message.(type) {
	case *seededQueryMessage:
		message = m.Text
	case *voiceInputMessage:
		message = m.Text
	}
	if !ensureTurnCredentials(c) {
		return "", false
	}
	turnRoute := resolveTurnRoute(c, message)
	if turnRoute.signature != c.activeAgentRouteSignature {
		retireAgent(c)
	}
	initializing := ""
	if c.agentRef() == nil {
		initializing = c.t("cli.chat.initializing_agent", nil)
		c.setSpinnerText(initializing)
	}
	built := func() bool {
		defer func() {
			c.uiMu.Lock()
			if initializing != "" && c.spinnerText == initializing {
				c.spinnerText = ""
			}
			c.uiMu.Unlock()
		}()
		return initTurnAgent(c, turnRoute.model, turnRoute.runtime, turnRoute.requestOverrides)
	}()
	if !built {
		return "", false
	}
	a := c.agentRef()
	if a == nil {
		return "", false
	}
	c.syncFallbackChainWithConfig()
	message = c.chatRouteImages(message, images)
	if text, isText := message.(string); isText {
		message = state.SanitizeSurrogates(text)
	}
	displayMetadata := c.chatStageUserMessage(a, message)
	if n, isNotification := message.(tools.TimelineNotification); isNotification {
		message = n.Text
	}
	if !c.modernLayout {
		chatConsolePrint("[" + accentHex() + "]" + strings.Repeat("─", 40) + "[/]")
		cprint("")
	}
	defer a.NotificationPolicySnapshot(agent.NotificationConfigSnapshot(c.ctx))()
	turn := &chatTurn{}
	turn.muteNotificationReply = config.DiagnosticTurnMuted(c.ctx, displayMetadata, "cli", a.NotificationConfig())
	defer c.chatReleaseTurnAudio(turn)
	defer func() {
		if r := recover(); r != nil {
			if _, interrupted := r.(keyboardInterrupt); interrupted {
				panic(r)
			}
			cprint(c.t("gateway.model.error_prefix", map[string]any{"error": panicText(r)}))
			response, ok = "", false
		}
	}()
	c.resetStreamState()
	c.streamMu.Lock()
	c.reasoningShownThisTurn = false
	c.streamedTextThisTurn = ""
	c.streamMu.Unlock()
	c.chatSetupTurnAudio(turn, message, voiceInput)
	started := wallClock()
	c.uiMu.Lock()
	c.promptStartTime = &started
	c.promptDuration = 0
	c.uiMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		turn.workerID.Store(pythread.GoroutineID())
		c.chatRunAgent(turn, message)
	}()
	interruptMsg := c.chatMonitorAgentThread(turn, done)
	c.chatSettleTurn(turn)
	return c.chatRenderTurn(turn, done, interruptMsg)
}

// Stops the thinking sound, sends the TTS end sentinel and cuts playback only after an abnormal exit
func (c *CLI) chatReleaseTurnAudio(turn *chatTurn) {
	if turn.thinkingStarted {
		func() {
			defer func() { _ = recover() }()
			stopThinkingSound()
		}()
	}
	if turn.textQueue != nil {
		turn.textQueue.Put(nil)
	}
	if turn.stopEvent != nil && !turn.ttsNormalExit {
		cliLogger.Info("TTS CUT: exception finally block setting stop_event")
		turn.stopEvent.Set()
	}
	if turn.ttsDone != nil && !turnDone(turn.ttsDone) {
		select {
		case <-turn.ttsDone:
		case <-time.After(ttsReleaseTimeout):
		}
	}
}

// Attaches images as native content parts for a vision model, else prepends their text descriptions
func (c *CLI) chatRouteImages(message any, images []string) any {
	if len(images) == 0 {
		return message
	}
	text, _ := message.(string)
	mode := func() (mode string) {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Debug(fmt.Sprintf("image_routing decision failed, defaulting to text: %v", r))
				mode = "text"
			}
		}()
		return decideImageInputMode(c.ctx, c.env, pystr.Strip(c.provider), pystr.Strip(c.model), pystr.Strip(c.requestedProvider))
	}()
	if mode == "native" {
		parts, skipped, err := buildNativeContentParts(c.env, text, images, nil)
		if err != nil {
			cliLogger.Warn(fmt.Sprintf("native image attach failed, falling back to text: %s", err))
		} else {
			if len(skipped) > 0 {
				cprint("  " + dimANSI + c.t("cli.chat.skipped_unreadable_images", map[string]any{"count": len(skipped)}) + resetANSI)
			}
			for _, part := range parts {
				if p, isMap := part.(map[string]any); isMap && p["type"] == "image_url" {
					names := make([]string, len(images))
					for i, img := range images {
						names[i] = filepath.Base(img)
					}
					cprint("  " + dimANSI + c.t("cli.chat.attaching_images_natively", map[string]any{"count": len(images), "names": strings.Join(names, ", ")}) + resetANSI)
					return parts
				}
			}
		}
	}
	return c.preprocessImagesWithVision(text, images, true)
}

// Appends the staged user row to the transcript under the agent's persist lock and returns its display metadata
func (c *CLI) chatStageUserMessage(a cliAgent, message any) map[string]any {
	staged := state.StampMessageTimestamp(map[string]any{"role": "user", "content": message}, nil)
	var displayMetadata map[string]any
	if n, isNotification := message.(tools.TimelineNotification); isNotification {
		displayMetadata = map[string]any{"display_text": n.DisplayText, "notification_category": n.NotificationCategory}
		staged["content"] = n.Text
		staged["display_kind"] = n.DisplayKind
		staged["display_metadata"] = displayMetadata
	}
	a.StagePendingCLIUserMessage(staged, func() {
		c.conversationHistory = append(append([]map[string]any{}, c.conversationHistory...), staged)
	})
	return displayMetadata
}

// Arms the full duplex listener and the streaming TTS pipeline of this turn in voice mode
func (c *CLI) chatSetupTurnAudio(turn *chatTurn, message any, voiceInput bool) {
	if turn.muteNotificationReply {
		return
	}
	c.voiceLock.Lock()
	voiceMode, continuous, voiceTTS := c.voiceMode, c.voiceContinuous, c.voiceTTS
	c.voiceLock.Unlock()
	if voiceMode && continuous {
		c.voiceLock.Lock()
		c.voiceLastTTSText = ""
		c.voiceLock.Unlock()
		go c.voiceFullDuplexListener()
	}
	if voiceTTS {
		func() {
			defer func() {
				if r := recover(); r != nil {
					cliLogger.Debug("streaming TTS arm check failed", "error", r)
				}
			}()
			turn.useStreamingTTS = checkTTSRequirements(c.ctx)
		}()
	}
	if turn.useStreamingTTS {
		turn.textQueue = &inputQueue{}
		turn.stopEvent = newSyncEvent()
		displayCallback := func(sentence string) {
			if !turn.boxOpened && c.modernLayout {
				turn.boxOpened = true
				cprint(answerLabel(""))
			}
			if !turn.boxOpened {
				turn.boxOpened = true
				label := " ☤ Icebath "
				if c.showTimestamps {
					label = label + c.timestampNow() + " "
				}
				w := scrollbackBoxWidth()
				fill := w - 2 - statusBarDisplayWidth(label)
				cprint("\n" + accentANSI.String() + "╭─" + label + strings.Repeat("─", max(fill-1, 0)) + "╮" + resetANSI)
			}
			cprint(streamPad + pystr.Rstrip(sentence))
		}
		if c.streamingEnabled {
			displayCallback = nil
		}
		turn.ttsDone = make(chan struct{})
		textQueue, stopEvent, ttsDone := turn.textQueue, turn.stopEvent, turn.ttsDone
		go func() {
			defer close(ttsDone)
			streamTTSToSpeaker(c.ctx, textQueue, stopEvent, c.voiceTTSDone, displayCallback, "")
		}()
		c.voiceLock.Lock()
		c.voiceTTSStop = turn.stopEvent
		c.voiceLock.Unlock()
		turn.streamCallback = func(delta string) {
			textQueue.Put(delta)
			c.voiceLock.Lock()
			c.voiceLastTTSText += delta
			c.voiceLock.Unlock()
		}
	}
	if _, isText := message.(string); voiceInput && isText {
		turn.voicePrefix = voiceInputPrefix
	}
}

// Agent goroutine body binding the turn callbacks and approval key, prepending one shot notes and running the turn
func (c *CLI) chatRunAgent(turn *chatTurn, message any) {
	ctx := tools.WithThreadLocals(c.turnCtx)
	ctx = skills.WithTurnSecretCapture(ctx, c.secretCaptureCallback)
	browser.SetUnlockPromptCallback(ctx, c.vaultUnlockCallback)
	browser.SetSaveLoginPromptCallback(ctx, c.vaultSaveLoginCallback)
	browser.SetCodePromptCallback(ctx, c.vaultCodeCallback)
	sessionKey := c.sessionID
	if sessionKey == "" {
		sessionKey = "default"
	}
	tools.SetCurrentSessionKey(c.env, sessionKey)
	a := c.agentRef()
	agentMessage, changed := message, false
	if turn.voicePrefix != "" {
		agentMessage, changed = turn.voicePrefix+message.(string), true
	}
	a.SetVoiceTurnPending(turn.voicePrefix != "")
	c.uiMu.Lock()
	takenModelNote, takenSkillsNote := c.pendingModelSwitchNote, c.pendingSkillsReloadNote
	c.pendingModelSwitchNote, c.pendingSkillsReloadNote = "", ""
	c.uiMu.Unlock()
	for _, note := range []string{takenModelNote, takenSkillsNote} {
		if note != "" {
			agentMessage, changed = prependNoteToMessage(agentMessage, note), true
		}
	}
	if media.TakeSpeechInterrupted() {
		agentMessage, changed = prependNoteToMessage(agentMessage, media.SpeechInterruptedNote), true
	}
	c.uiMu.Lock()
	moaConfig := c.pendingMoaConfig
	c.pendingMoaConfig = nil
	oneTurnModelRestore := c.pendingOneTurnModelRestore
	c.pendingOneTurnModelRestore = nil
	c.uiMu.Unlock()
	var persistCleanUserMessage any
	if changed {
		persistCleanUserMessage = message
	}
	defer func() {
		result := turn.getResult()
		admitted, hasAdmitted := result["admitted"]
		unadmitted := hasAdmitted && admitted == false
		c.uiMu.Lock()
		if unadmitted {
			if takenModelNote != "" {
				c.pendingModelSwitchNote = takenModelNote
			}
			if takenSkillsNote != "" {
				c.pendingSkillsReloadNote = takenSkillsNote
			}
		}
		keepOneTurn := len(oneTurnModelRestore) > 0 && (unadmitted || result["failure_reason"] == "session_busy")
		if keepOneTurn {
			c.pendingOneTurnModelRestore = oneTurnModelRestore
		}
		c.uiMu.Unlock()
		if len(oneTurnModelRestore) > 0 && !keepOneTurn {
			c.restoreModelRuntimeSnapshot(oneTurnModelRestore)
		}
		c.flushCreditNotices()
		browser.SetUnlockPromptCallback(ctx, nil)
		browser.SetSaveLoginPromptCallback(ctx, nil)
		browser.SetCodePromptCallback(ctx, nil)
		tools.ResetCurrentSessionKey(c.env)
	}()
	defer func() {
		if r := recover(); r != nil {
			turn.setResult(c.runConversationFailure(a, fmt.Errorf("%s", panicText(r))))
		}
	}()
	muted := turn.muteNotificationReply
	streamCallback := turn.streamCallback
	if muted {
		streamCallback = nil
	}
	history := append([]map[string]any{}, c.conversationHistory[:max(0, len(c.conversationHistory)-1)]...)
	c.uiMu.Lock()
	turnSessionID := c.sessionID
	c.uiMu.Unlock()
	result, err := func() (map[string]any, error) {
		runCtx, restoreTurn := a.NotificationTurn(ctx, muted, turnSessionID)
		defer restoreTurn()
		return runTurnConversation(runCtx, a, agentMessage, agent.RunConversationOptions{
			ConversationHistory: history, StreamCallback: streamCallback, TaskID: turnSessionID,
			PersistUserMessage: persistCleanUserMessage, MoaConfig: moaConfig, TurnAuthor: c.singleQueryTurnAuthor})
	}()
	if err != nil {
		turn.setResult(c.runConversationFailure(a, err))
		return
	}
	turn.setResult(result)
	c.uiMu.Lock()
	disableMoa := c.pendingMoaDisableAfterTurn
	restoreModel := c.pendingMoaRestoreModel
	c.uiMu.Unlock()
	if disableMoa {
		c.restoreMoaModelFields(restoreModel)
		retireAgent(c)
		c.uiMu.Lock()
		c.pendingMoaRestoreModel = nil
		c.pendingMoaDisableAfterTurn = false
		c.uiMu.Unlock()
	}
}

// Puts back the model routing fields a one shot MoA prompt replaced, fields captured empty left alone
func (c *CLI) restoreMoaModelFields(saved map[string]string) {
	fields := map[string]*string{"requested_provider": &c.requestedProvider, "provider": &c.provider, "model": &c.model,
		"api_key": &c.apiKey, "base_url": &c.baseURL, "api_mode": &c.apiMode}
	for key, value := range saved {
		if target, known := fields[key]; known && value != "" {
			*target = value
		}
	}
}

// Failed turn result carrying the chat error copy for an error run_conversation raised
func (c *CLI) runConversationFailure(a cliAgent, err error) map[string]any {
	cliLogger.Error(fmt.Sprintf("run_conversation raised: %s", err))
	providerName, model := c.provider, c.model
	if a.Provider() != "" {
		providerName = a.Provider()
	}
	if a.Model() != "" {
		model = a.Model()
	}
	return map[string]any{
		"final_response": chatErrorResponse(c.ctx, err, providerName, model, ""),
		"messages":       []map[string]any{}, "api_calls": 0,
		"completed": false, "failed": true, "error": a.SummarizeAPIError(err),
	}
}

// Polls the interrupt queue while the agent goroutine runs and returns the interrupting message, nil for none
func (c *CLI) chatMonitorAgentThread(turn *chatTurn, done <-chan struct{}) any {
	c.voiceLock.Lock()
	voiceMode := c.voiceMode
	c.voiceLock.Unlock()
	if voiceMode {
		func() {
			defer func() {
				if recover() != nil {
					turn.thinkingStarted = false
				}
			}()
			turn.thinkingStarted = startThinkingSound(c.ctx, func() bool {
				c.voiceLock.Lock()
				recording := c.voiceRecording
				c.voiceLock.Unlock()
				return c.voiceTTSDone.IsSet() && !recording && !c.voiceBargeCapture.IsSet()
			})
		}()
	}
	var interruptMsg any
	for !turnDone(done) {
		c.raiseIfTurnCancelled()
		msg, got := c.interruptQueue.Get(interruptPollInterval)
		if !got {
			if err := c.invalidateWithin(0.15); err != nil {
				panic(err)
			}
			continue
		}
		interruptMsg = msg
		if !pyrepr.Truthy(interruptPayloadValue(msg)) {
			continue
		}
		c.uiMu.Lock()
		clarifying := c.clarifyState != nil || c.clarifyFreetext
		c.uiMu.Unlock()
		if clarifying {
			c.pendingInput.Put(msg)
			interruptMsg = nil
			continue
		}
		cprint("\n" + c.t("cli.chat.new_message_interrupting", nil))
		if turn.stopEvent != nil {
			turn.stopEvent.Set()
		}
		interruptTurnAgent(c.agentRef(), interruptText(msg))
		c.clearActiveOverlaysForInterrupt()
		c.writeInterruptDebugLog(msg)
		break
	}
	if interruptMsg != nil {
		for range interruptJoinPolls {
			select {
			case <-done:
			case <-time.After(interruptJoinPoll):
			}
			c.raiseIfTurnCancelled()
			if turnDone(done) || c.exitRequested() {
				break
			}
		}
		if !turnDone(done) {
			cliLogger.Warn(fmt.Sprintf("Agent thread still alive after interrupt (thread %d). Daemon thread will be cleaned up on exit.", turn.workerID.Load()))
		}
	} else {
		select {
		case <-done:
		case <-time.After(agentJoinTimeout):
		}
	}
	return interruptMsg
}

// Unwinds the turn the way a KeyboardInterrupt does once a shutdown signal cancelled the turn context
func (c *CLI) raiseIfTurnCancelled() {
	if c.turnCtx != nil && c.turnCtx.Err() != nil {
		panic(keyboardInterrupt{})
	}
}

// Python truth operand of a queued interrupt item, a bundled message always counting as true
func interruptPayloadValue(msg any) any {
	if q, ok := msg.(queuedInput); ok {
		return []any{q.Text, q.Images}
	}
	return msg
}

// Text the agent receives as the interrupting message
func interruptText(msg any) string {
	if q, ok := msg.(queuedInput); ok {
		return q.Text
	}
	return pyrepr.Str(msg)
}

// Reports whether the user asked the CLI to exit
func (c *CLI) exitRequested() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.shouldExit
}

// Appends the interrupt and the flags of the agent and its children to the home interrupt debug log
func (c *CLI) writeInterruptDebugLog(msg any) {
	defer func() { _ = recover() }()
	f, err := os.OpenFile(filepath.Join(home.Dir(c.ctx), "interrupt_debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s interrupt fired: msg=%s, children=0, parent._interrupt=%s\n", pydatetime.Strftime(time.Now(), "%H:%M:%S"),
		pyrepr.StrRepr(pystr.Slice(pyrepr.Str(msg), 0, 60)), pyrepr.Repr(c.agentRef().InterruptRequested()))
}

// Transcript rows of a turn result, false when the result carries no messages key
func resultMessages(result map[string]any) ([]map[string]any, bool) {
	raw, ok := result["messages"]
	if !ok {
		return nil, false
	}
	switch msgs := raw.(type) {
	case []map[string]any:
		return msgs, true
	case []*state.Message:
		return agent.SessionMessageDicts(msgs), true
	case []any:
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			if row, isRow := m.(map[string]any); isRow {
				out = append(out, row)
			}
		}
		return out, true
	}
	return nil, true
}

// Freezes the timers, flushes the stream, drains TTS and syncs history and session id after the agent ends
func (c *CLI) chatSettleTurn(turn *chatTurn) {
	now := wallClock()
	result := turn.getResult()
	c.uiMu.Lock()
	if c.promptStartTime != nil {
		c.promptDuration = max(0.0, now-*c.promptStartTime)
		c.promptStartTime = nil
	}
	c.lastTurnFinishedAt = &now
	c.lastTurnResult = result
	c.uiMu.Unlock()
	func() {
		defer func() { _ = recover() }()
		cleanupStaleAsyncClients()
	}()
	c.flushStream()
	if turn.useStreamingTTS && turn.textQueue != nil {
		turn.textQueue.Put(nil)
		if turn.ttsDone != nil {
			select {
			case <-turn.ttsDone:
			case <-time.After(ttsDrainTimeout):
			}
			turn.ttsNormalExit = turnDone(turn.ttsDone)
		}
	}
	time.Sleep(settlePaintDelay)
	if len(result) > 0 {
		if msgs, ok := resultMessages(result); ok {
			c.conversationHistory = msgs
		}
	}
	if a := c.agentRef(); a != nil && a.SessionID() != "" && a.SessionID() != c.sessionID {
		tools.TransferSessionYolo(c.sessionID, a.SessionID())
		c.sessionID = a.SessionID()
		c.writeTerminalBreadcrumb()
		c.pendingTitle = ""
	}
}

// Acknowledges a turn the gateway lost in a restart, the in process CLI holding no gateway session
func (c *CLI) handleDiscardCommand(cmdOriginal string) {
	_, rest, _ := strings.Cut(cmdOriginal, " ")
	admissionID := pystr.Strip(rest)
	if admissionID == "" {
		cprint("  Usage: /discard <admission_id>")
		return
	}
	connection := gatewayConnectionFor(c)
	if connection == nil {
		cprint("  /discard: no gateway session is open")
		return
	}
	var lost, snapshot map[string]any
	err := func() error {
		raw, err := connection.Reattach(c.ctx, c.sessionID, nil)
		if err != nil {
			return err
		}
		before, _ := pyjson.Plain(raw).(map[string]any)
		if lost, err = gatewayLostAdmission(before, admissionID); err != nil {
			return err
		}
		if _, err = connection.Call(c.ctx, "prompt.resolve_unknown", map[string]any{"session_id": c.sessionID,
			"admission_id": requireKey(lost, "admission_id"), "execution_generation": requireKey(lost, "execution_generation")}); err != nil {
			return err
		}
		raw, err = connection.Reattach(c.ctx, c.sessionID, nil)
		snapshot, _ = pyjson.Plain(raw).(map[string]any)
		return err
	}()
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		cprint("  /discard: " + err.Error())
		return
	}
	left := 0
	pending, _ := snapshot["pending"].([]any)
	for _, row := range anyRows(pending) {
		if requireKey(row, "status") == "unknown" {
			left++
		}
	}
	tail := "You can send messages again."
	if left > 0 {
		plural := ""
		if left > 1 {
			plural = "s"
		}
		tail = fmt.Sprintf("%d more paused turn%s to /discard first.", left, plural)
	}
	cprint(fmt.Sprintf("  Discarded %s; it was not re-run. %s", pyrepr.Str(lost["admission_id"]), tail))
}

// Post turn display of errors, the interrupt marker, reasoning and response blocks, bell and requeues
func (c *CLI) chatRenderTurn(turn *chatTurn, done <-chan struct{}, interruptMsg any) (string, bool) {
	result := turn.getResult()
	response := ""
	if len(result) > 0 {
		response = cliStr(result["final_response"], "")
	}
	if turn.muteNotificationReply {
		if pending, _ := c.chatResolveInterrupt(turn, done, interruptMsg, response); pyrepr.Truthy(interruptPayloadValue(pending)) {
			c.pendingInput.Put(pending)
		}
		return "", false
	}
	if len(result) > 0 && (cliTruthy(result["failed"]) || cliTruthy(result["partial"])) && response == "" {
		errText := c.t("cli.chat.unknown_error", nil)
		if cliTruthy(result["error"]) {
			errText = pyrepr.Str(result["error"])
		}
		response = chatErrorResponse(c.ctx, errText, c.turnProvider(), c.turnModel(), cliStr(result["failure_reason"], ""))
		c.voiceLock.Lock()
		continuous := c.voiceContinuous
		c.voiceContinuous = false
		c.voiceLock.Unlock()
		if continuous {
			cprint("\n" + dimANSI + c.t("cli.chat.continuous_voice_stopped", nil) + resetANSI)
		}
	}
	pendingMessage, showInterruptMarker := c.chatResolveInterrupt(turn, done, interruptMsg, response)
	c.chatPrintReasoningBox(turn)
	c.chatPrintResponsePanel(turn, response)
	if showInterruptMarker {
		func() {
			defer suspendOutputHistory()()
			cprint("\n" + dimANSI + c.t("cli.chat.interrupted_marker", nil) + resetANSI)
		}()
	}
	func() {
		defer func() { _ = recover() }()
		c.emitFocusRecoveryLine()
	}()
	c.ringBell(false, c.t("cli.modal.bell_turn_complete", nil), "")
	if len(result) > 0 && !cliTruthy(result["completed"]) && !cliTruthy(result["interrupted"]) {
		apiCalls, _ := progressNumber(mapValueOr(result, "api_calls", 0))
		maxIter := 500
		var notificationConfig map[string]any
		if c.agentRef() != nil {
			maxIter = c.agentRef().MaxIterations()
			notificationConfig = c.agentRef().NotificationConfig()
		}
		if int(apiCalls) >= maxIter {
			_, _ = config.RenderNotification(c.ctx, func() error {
				cprint("\n" + dimANSI + c.t("cli.chat.iteration_budget_reached", map[string]any{"used": int(apiCalls), "max": maxIter}) + resetANSI)
				return nil
			}, "cli", false, notificationConfig)
		}
	}
	c.voiceLock.Lock()
	voiceTTS := c.voiceTTS
	c.voiceLock.Unlock()
	if voiceTTS && response != "" && !turn.useStreamingTTS {
		c.voiceSpeakResponseAsync(response)
	}
	if pyrepr.Truthy(interruptPayloadValue(pendingMessage)) {
		c.requeueInterruptedInput(pendingMessage)
	}
	if steer := cliStr(result["pending_steer"], ""); steer != "" {
		preview := pystr.Slice(steer, 0, 60)
		if pystr.Len(steer) > 60 {
			preview += "..."
		}
		cprint("\n" + c.t("cli.chat.delivering_leftover_steer", map[string]any{"preview": preview}))
		c.pendingInput.Put(steer)
	}
	return response, true
}

// Requeues the interrupting message with any that arrived meanwhile as the next prompt
func (c *CLI) requeueInterruptedInput(pendingMessage any) {
	allParts := []any{pendingMessage}
	for {
		extra, got := c.interruptQueue.GetNoWait()
		if !got {
			break
		}
		if pyrepr.Truthy(interruptPayloadValue(extra)) {
			allParts = append(allParts, extra)
		}
	}
	textParts := []string{}
	imageParts := []string{}
	for _, part := range allParts {
		if q, ok := part.(queuedInput); ok {
			textParts = append(textParts, q.Text)
			imageParts = append(imageParts, q.Images...)
			continue
		}
		textParts = append(textParts, pyrepr.Str(part))
	}
	combined := strings.Join(textParts, "\n")
	var payload any = combined
	if len(imageParts) > 0 {
		payload = queuedInput{Text: combined, Images: imageParts}
	}
	preview := pystr.Slice(combined, 0, 50)
	if pystr.Len(combined) > 50 {
		preview += "..."
	}
	if len(allParts) > 1 {
		cprint("\n" + c.t("cli.chat.sending_after_interrupt_multi", map[string]any{"count": len(allParts), "preview": preview}))
	} else {
		cprint("\n" + c.t("cli.chat.sending_after_interrupt", map[string]any{"preview": preview}))
	}
	c.pendingInput.Put(payload)
}

// Returns the message to requeue and whether to print the interrupt marker, clearing a stale agent interrupt flag
func (c *CLI) chatResolveInterrupt(turn *chatTurn, done <-chan struct{}, interruptMsg any, response string) (any, bool) {
	result := turn.getResult()
	interruptedThisTurn := len(result) > 0 && cliTruthy(result["interrupted"])
	c.uiMu.Lock()
	c.lastTurnInterrupted = interruptedThisTurn
	c.lastTurnFailed = len(result) > 0 && cliTruthy(result["failed"])
	c.uiMu.Unlock()
	if interruptedThisTurn {
		var pendingMessage any = interruptMsg
		if acknowledged := result["interrupt_message"]; pyrepr.Truthy(interruptPayloadValue(acknowledged)) {
			pendingMessage = acknowledged
			if q, bundled := interruptMsg.(queuedInput); bundled && pyrepr.Equal(acknowledged, q.Text) {
				pendingMessage = q
			}
		}
		return pendingMessage, response != "" && pyrepr.Truthy(interruptPayloadValue(pendingMessage))
	}
	if pyrepr.Truthy(interruptPayloadValue(interruptMsg)) {
		func() {
			defer func() { _ = recover() }()
			if turnDone(done) && c.agentRef() != nil && c.agentRef().InterruptRequested() {
				c.agentRef().ClearInterrupt()
			}
		}()
		return interruptMsg, false
	}
	return nil, false
}

// Prints the collapsed reasoning block when reasoning is shown and streaming did not already show it
func (c *CLI) chatPrintReasoningBox(turn *chatTurn) {
	result := turn.getResult()
	c.streamMu.Lock()
	shown := c.reasoningShownThisTurn
	c.streamMu.Unlock()
	if !c.showReasoning || len(result) == 0 || shown {
		return
	}
	reasoning := cliStr(result["last_reasoning"], "")
	if reasoning == "" {
		return
	}
	if c.modernLayout {
		for _, line := range thoughtRows(nil, reasoning, c.reasoningFull) {
			cprint(line)
		}
		return
	}
	w := scrollbackBoxWidth()
	label := " " + c.t("cli.chat.reasoning_label", nil) + " "
	top := dimANSI + "┌─" + label + strings.Repeat("─", max(w-3-pystr.Len(label), 0)) + "┐" + resetANSI
	bottom := dimANSI + "└" + strings.Repeat("─", max(w-2, 0)) + "┘" + resetANSI
	lines := pystr.Splitlines(pystr.Strip(reasoning), false)
	display := pystr.Strip(reasoning)
	if len(lines) > 10 && !c.reasoningFull {
		display = strings.Join(lines[:10], "\n") + "\n" + dimANSI + "  " + c.t("cli.chat.reasoning_more_lines", map[string]any{"count": len(lines) - 10}) + resetANSI
	}
	cprint("\n" + top + "\n" + dimANSI + display + resetANSI + "\n" + bottom)
}

// Prints the response block, closing a spoken block or showing a transform suffix, then the billing call to action
func (c *CLI) chatPrintResponsePanel(turn *chatTurn, response string) {
	result := turn.getResult()
	if response == "" || cliTruthy(result["response_previewed"]) {
		return
	}
	label, respColor, respText := "☤ Icebath", "#CD7F32", "#FFF8DC"
	if skin := config.GetActiveSkin(c.ctx); skin != nil {
		label = skin.GetBranding("response_label", "☤ Icebath")
		respColor = skin.GetColor("response_border", "#CD7F32")
		respText = skin.GetColor("banner_text", "#FFF8DC")
	}
	respColor, respText = maybeRemapForLightMode(respColor), maybeRemapForLightMode(respText)
	isErrorResponse := cliTruthy(result["failed"]) || cliTruthy(result["partial"])
	c.streamMu.Lock()
	streamedText, streamStarted, streamBoxOpened := c.streamedTextThisTurn, c.streamStarted, c.streamBoxOpened
	c.streamMu.Unlock()
	c.uiMu.Lock()
	lastTurnInterrupted := c.lastTurnInterrupted
	c.uiMu.Unlock()
	interruptedStreamed := lastTurnInterrupted && pystr.Strip(response) != "" &&
		strings.Contains(strings.Join(pystr.SplitWhitespace(streamedText, -1), " "), strings.Join(pystr.SplitWhitespace(response, -1), " "))
	alreadyStreamed := ((streamStarted && streamBoxOpened) || interruptedStreamed) && !isErrorResponse
	mode := c.finalResponseMarkdown
	if result["failure_reason"] == "gateway_connection" {
		mode = "raw"
	}
	switch {
	case turn.useStreamingTTS && turn.boxOpened && !isErrorResponse:
		if c.modernLayout {
			cprint(closingSeparator(true))
		} else {
			cprint("\n" + accentANSI.String() + "╰" + strings.Repeat("─", max(scrollbackBoxWidth()-2, 0)) + "╯" + resetANSI)
		}
	case alreadyStreamed:
		if text := postStreamTransformOutput(response, result); pystr.Strip(text) != "" {
			cprint(text)
		}
	case c.modernLayout:
		cprint(answerLabel(""))
		if err := defaultChatConsole.PrintRenderableWith(renderFinalAssistantContent(response, mode), pyrich.PrintOptions{Style: respText, SoftWrap: true}); err != nil {
			panic(err)
		}
		cprint(closingSeparator(true))
	default:
		defaultChatConsole.PrintPanel(pyrich.Panel{
			Content: renderFinalAssistantContent(response, mode), Title: "[" + respColor + " bold]" + label + "[/]", TitleAlign: "left",
			BorderStyle: respColor, Style: respText, Box: "horizontals", Padding: &[2]int{1, 0}, Width: scrollbackBoxWidth()})
	}
	if limit, isLimit := result["usage_limit"].(map[string]any); isLimit {
		func() {
			defer func() {
				if r := recover(); r != nil {
					cliLogger.Debug("Usage-limit panel rendering failed", "error", r)
				}
			}()
			defaultChatConsole.PrintPanel(pyrich.Panel{
				Body: provider.LimitNextStepsFromPayload(limit, "/model", "/topup", false, true), Title: "[#CD7F32 bold]⚡ Limit reached[/]",
				TitleAlign: "left", BorderStyle: "#CD7F32", Box: "horizontals", Padding: &[2]int{1, 4}, Width: scrollbackBoxWidth()})
		}()
		return
	}
	if result["failure_reason"] != "billing" || cliTruthy(result["usage_window_message"]) {
		return
	}
	billingBlock, _ := result["billing_block"].(map[string]any)
	var ctaLines []string
	if cliTruthy(billingBlock["is_nous"]) {
		ctaLines = []string{c.t("cli.chat.billing_cta_nous", nil)}
	} else {
		providerLabel := cliStr(billingBlock["provider_label"], "")
		if providerLabel == "" {
			providerLabel = c.t("cli.chat.your_provider", nil)
		}
		tail := "."
		if url := cliStr(billingBlock["billing_url"], ""); url != "" {
			tail = ": [bold]" + url + "[/]"
		}
		ctaLines = []string{c.t("cli.chat.billing_cta_provider", map[string]any{"provider": providerLabel}) + tail}
	}
	ctaLines = append(ctaLines, c.t("cli.chat.billing_cta_switch", nil))
	func() {
		defer func() { _ = recover() }()
		defaultChatConsole.PrintPanel(pyrich.Panel{
			Body: strings.Join(ctaLines, "\n"), Title: "[#CD7F32 bold]" + c.t("cli.chat.out_of_credits_title", nil) + "[/]",
			TitleAlign: "left", BorderStyle: "#CD7F32", Box: "horizontals", Padding: &[2]int{1, 4}, Width: scrollbackBoxWidth()})
	}()
}

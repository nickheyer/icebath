package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Shared state of one turn goroutine, the agent bound eagerly so recovery and release always have one
type turnRun struct {
	agent            *agent.AIAgent
	oneTurnRestore   map[string]any
	terminalCallback func(map[string]any)
	receiptCommitted bool
	sessionEnv       *tools.Env
	homeCtx          context.Context
	result           map[string]any
	ttsQueue         *ttsTurnQueue
	thinkingStarted  bool
	history          []any
	historyVersion   int64
	compressionCount *int
	runOpts          *agent.RunConversationOptions
	errorRetained    bool
	errorDetail      string
	promptText       string
	markerKey        string
	receiptAttempted bool
}

// Optional arguments of a prompt dispatch, a nil image list meaning the staged attachments
type promptSubmitOptions struct {
	displayKind            string
	displayMetadata        map[string]any
	imagePaths             []string
	queuedPromptGeneration *int64
	terminalCallback       func(map[string]any)
	turnAuthor             map[string]any
	silentOwnershipRefusal bool
}

// Folds turns another surface appended to this session into the model facing history before the turn snapshots it
func adoptOutOfBandTurns(ctx context.Context, session *tuigateway.Session) {
	unlock := lockHistory(session)
	history, version := historyList(session.Get("history")), intOr(session.Get("history_version"))
	unlock()
	var seen *int64
	for _, m := range history {
		if msg, ok := m.(map[string]any); ok {
			if rid, has := messageRowID(msg); has && (seen == nil || rid > *seen) {
				v := rid
				seen = &v
			}
		}
	}
	submitRow, _ := session.Get("_submit_user_row").(map[string]any)
	ceiling, hasCeiling := messageRowID(submitRow)
	belowCeiling := func(rid int64, ok bool) bool { return ok && (!hasCeiling || rid < ceiling) }
	if seen == nil {
		adoptStoreTailWithoutRowIDs(ctx, session, history, version, belowCeiling)
		return
	}
	foreign := func(rid int64, ok bool) bool { return belowCeiling(rid, ok) && rid > *seen }
	var newer []*pyjson.Object
	probeFailed := func() bool {
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			return false
		}
		rows, err := db.GetMessages(ctx, submitRowTargetKey(session), state.GetMessagesOptions{AfterID: seen})
		if err != nil {
			logger().Debug("out-of-band history probe failed; turn runs on the in-memory history", "error", err)
			return true
		}
		newer = rows
		return false
	}()
	if probeFailed {
		return
	}
	rewritten, found := false, false
	for _, row := range newer {
		rid, ok := objectRowID(row)
		if !foreign(rid, ok) {
			continue
		}
		found = true
		if v, _ := row.Get("_compressed_summary"); truthy(v) {
			rewritten = true
		}
	}
	if !found {
		return
	}
	keep := foreign
	if rewritten {
		keep = belowCeiling
	}
	var kept []map[string]any
	durable, _ := loadDurableTruncationHistory(ctx, session, "", rewritten)
	for _, m := range durable {
		if keep(messageRowID(m)) {
			kept = append(kept, m)
		}
	}
	tail := agent.CanonicalizeReplayHistory(kept, nil)
	if len(tail) == 0 {
		return
	}
	unlock = lockHistory(session)
	defer unlock()
	if intOr(session.Get("history_version")) != version {
		return
	}
	if rewritten {
		session.Set("history", messagesAsHistory(tail))
	} else {
		session.Set("history", append(append([]any{}, history...), messagesAsHistory(tail)...))
	}
	session.Set("history_version", version+1)
}

// Row id of a decoded store row
func objectRowID(row *pyjson.Object) (int64, bool) {
	v, _ := row.Get("id")
	if _, isBool := v.(bool); isBool {
		return 0, false
	}
	n, err := pyint.Int64(v)
	if v == nil || err != nil {
		return 0, false
	}
	return n, true
}

// Catches a record whose messages carry no durable row id up with a strictly longer matching store lineage
func adoptStoreTailWithoutRowIDs(ctx context.Context, session *tuigateway.Session, history []any, version int64, belowCeiling func(int64, bool) bool) {
	var rows []map[string]any
	durable, _ := loadDurableTruncationHistory(ctx, session, "", true)
	for _, m := range durable {
		if belowCeiling(messageRowID(m)) {
			rows = append(rows, m)
		}
	}
	if len(history) == 0 {
		return
	}
	var known []any
	for _, m := range history {
		if msg, ok := m.(map[string]any); ok && truthy(msg["_compressed_summary"]) {
			known = append(known, msg["content"])
		}
	}
	for _, m := range rows {
		if truthy(m["_compressed_summary"]) && !containsEqual(known, m["content"]) {
			tail := agent.CanonicalizeReplayHistory(rows, nil)
			unlock := lockHistory(session)
			if len(tail) > 0 && intOr(session.Get("history_version")) == version {
				session.Set("history", messagesAsHistory(tail))
				session.Set("history_version", version+1)
			}
			unlock()
			return
		}
	}
	if len(rows) <= len(history) {
		return
	}
	for i, mem := range history {
		msg, ok := mem.(map[string]any)
		if !ok || !pyjson.Equal(msg["role"], rows[i]["role"]) || !pyjson.Equal(msg["content"], rows[i]["content"]) {
			return
		}
	}
	tail := agent.CanonicalizeReplayHistory(rows[len(history):], nil)
	if len(tail) == 0 {
		return
	}
	unlock := lockHistory(session)
	defer unlock()
	if intOr(session.Get("history_version")) != version {
		return
	}
	session.Set("history", append(append([]any{}, history...), messagesAsHistory(tail)...))
	session.Set("history_version", version+1)
}

// Whether a list holds a value equal to v
func containsEqual(list []any, v any) bool {
	for _, item := range list {
		if pyjson.Equal(item, v) {
			return true
		}
	}
	return false
}

// Whether the install already has session rows beyond the current one
func installHasPriorSessions(ctx context.Context, session *tuigateway.Session) bool {
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return false
	}
	ok, err := db.SessionCountGE(ctx, 2)
	if err != nil {
		logger().Debug("session count probe failed for first-contact check", "error", err)
		return false
	}
	return ok
}

// Stages the install's first message onboarding note for this turn as a gateway turn context note
func stageFirstContactOnboardingNote(ctx context.Context, session *tuigateway.Session, a *agent.AIAgent, historyEmpty bool) {
	if failure := catchPanic(func() {
		cfg, err := loadConfig(ctx)
		if err != nil {
			panic(err)
		}
		note := tools.FirstContactTurnNote(ctx, cfg, filepath.Join(home.Dir(ctx), "config.yaml"), historyEmpty,
			installHasPriorSessions(ctx, session), a.ValidToolNames()["setup_beat"])
		if note == nil || *note == "" {
			return
		}
		if prior := a.GatewayTurnContextNotes(); prior != "" {
			a.SetGatewayTurnContextNotes(prior + "\n\n" + *note)
		} else {
			a.SetGatewayTurnContextNotes(*note)
		}
	}); failure != nil {
		logger().Debug("first-contact onboarding note failed", "error", pyrepr.Str(failure))
	}
}

// Daybreak flag of the turn read inside its profile scope, only on the owner's own surfaces
func syncAgentDaybreak(ctx context.Context, a *agent.AIAgent) {
	if a == nil {
		return
	}
	on := false
	if agent.DaybreakSources[a.Platform()] {
		agentCfg, _ := loadCfg(ctx)["agent"].(map[string]any)
		on = agentCfg["daybreak"] == true
	}
	a.SetDaybreak(on)
}

// Binds scopes, syncs the agent, snapshots history and builds the run message, ok false when expansion was refused
func prepareTurnInput(ctx context.Context, sid string, session *tuigateway.Session, st *turnRun, text any, images []string) (context.Context, any, any, int, streamRenderer, bool) {
	sessionKey := strOr(session.Get("session_key"))
	ctx, env := setSessionContext(ctx, sessionKey, nil, sid)
	if env == nil {
		env = &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}}
		ctx = tools.WithToolEnv(ctx, env)
	}
	st.sessionEnv = env
	tools.SetCurrentSessionKey(env, sessionKey)
	st.homeCtx = profileHomeScope(ctx, strOr(session.Get("profile_home")))
	ctx = sessionProfileRuntimeScope(ctx, session, true)
	wireCallbacks(ctx, sid)
	if st.oneTurnRestore == nil {
		applyPendingModelSwitch(ctx, sid, session)
		syncAgentModelWithConfig(ctx, sid, session)
		syncAgentCompressionWithConfig(ctx, sid, session)
	}
	syncAgentFallbackWithConfig(ctx, sid, session)
	syncBotCapabilities(ctx, sid, session)
	adoptOutOfBandTurns(ctx, session)
	a := sessionAgent(session)
	st.agent = a
	syncAgentDaybreak(ctx, a)
	unlock := lockHistory(session)
	st.history = historyList(session.Get("history"))
	st.historyVersion = intOr(session.Get("history_version"))
	unlock()
	stageFirstContactOnboardingNote(ctx, session, a, len(st.history) == 0)
	stageOnboardingTurnNote(ctx, sid, a, len(st.history) == 0)
	cwd := sessionCwd(ctx, session)
	registerSessionCwd(ctx, session)
	cols := 80
	if raw, present := session.Lookup("cols"); present {
		if n, err := pyint.Int64(raw); err == nil {
			cols = int(n)
		}
	}
	streamer := makeStreamRenderer(cols)
	prompt := text
	if s, ok := prompt.(string); ok && strings.Contains(s, "@") {
		model := a.Model()
		if model == "" {
			model = resolveModel(ctx)
		}
		ctxLen := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: model, BaseURL: a.BaseURL(),
			APIKey: a.APIKey(), Provider: a.Provider(), ConfigContextLength: a.ConfigContextLength()})
		allowed := cwd
		refs, err := agent.PreprocessContextReferences(ctx, s, agent.ContextReferenceOptions{Cwd: cwd, AllowedRoot: &allowed, ContextLength: ctxLen})
		if err != nil {
			panic(err)
		}
		if refs.Blocked {
			message := strings.Join(refs.Warnings, "\n")
			if message == "" {
				message = "Context injection refused."
			}
			tuigateway.Emit(ctx, "error", sid, map[string]any{"message": message})
			return ctx, nil, nil, 0, nil, false
		}
		prompt = refs.Message
	}
	st.promptText, _ = prompt.(string)
	runMessage := prompt
	if len(images) > 0 {
		runMessage = routeTurnImages(ctx, a, prompt, images)
	}
	if !agent.EventPresentationMuted(ctx, "message.delta", sid) {
		st.ttsQueue, st.thinkingStarted = startTurnVoice(ctx)
	}
	if media.TakeSpeechInterrupted() {
		runMessage = prependNote(runMessage, media.SpeechInterruptedNote)
	}
	runMessage = prependNote(runMessage, pendingReactionNotes(ctx, session))
	voiceTurn, _ := session.Pop("voice_turn")
	a.SetVoiceTurnPending(truthy(voiceTurn))
	return ctx, prompt, prependNote(runMessage, hudSurfaceNote(ctx, session)), cols, streamer, true
}

// Context carrying only the home override a profile turn binds, kept past the scope release until housekeeping
func profileHomeScope(ctx context.Context, profileHome string) context.Context {
	if profileHome != "" {
		return home.WithOverride(ctx, profileHome)
	}
	if !provider.IsMultiplexActive(ctx) {
		return ctx
	}
	return home.WithOverride(ctx, launchHome())
}

// Scripted opening steps of an onboarding turn fed one by one to the conversation loop
type onboardingPreludeSteps struct {
	steps []tools.OnboardingPreludeStep
	next  int
}

func (p *onboardingPreludeSteps) Next(*string) (agent.PreludeStep, bool) {
	if p.next >= len(p.steps) {
		return agent.PreludeStep{}, false
	}
	step := p.steps[p.next]
	p.next++
	return agent.PreludeStep{Content: step.Text, ToolName: step.Tool, Args: step.Args}, true
}

func (p *onboardingPreludeSteps) Close() {}

// Wires the streaming callbacks and runs the conversation into the turn result
func invokeAgent(ctx context.Context, sid string, session *tuigateway.Session, st *turnRun, prompt, runMessage any, streamer streamRenderer, images []string, displayKind string, displayMetadata, turnAuthor map[string]any, text any) {
	a := st.agent
	if count, ok := a.ContextCompressionCount(); ok {
		st.compressionCount = &count
	}
	var hold *gateway.SilenceMarkerStreamHold
	if isBotModeSession(ctx, session) {
		hold = &gateway.SilenceMarkerStreamHold{}
	}
	emitDelta := func(delta string) {
		unlock := lockHistory(session)
		appendInflightDelta(session, delta)
		unlock()
		payload := map[string]any{"text": delta}
		if streamer != nil {
			if rendered, ok := streamer.Feed(delta); ok {
				payload["rendered"] = rendered
			}
		}
		if st.ttsQueue != nil {
			st.ttsQueue.Put(delta)
		}
		tuigateway.Emit(ctx, "message.delta", sid, payload)
	}
	stream := func(delta string) {
		if a.MuteNotificationReply() {
			return
		}
		if hold != nil {
			delta = hold.Feed(delta)
			if delta == "" {
				return
			}
		}
		emitDelta(delta)
	}
	if loadInterimAssistantMessages(ctx) {
		a.SetInterimAssistantCallback(func(text string, alreadyStreamed bool) {
			if a.MuteNotificationReply() {
				return
			}
			tuigateway.Emit(ctx, "message.interim", sid, map[string]any{"text": text, "already_streamed": alreadyStreamed})
		})
	} else {
		a.SetInterimAssistantCallback(nil)
	}
	var persistUserMessage any = prompt
	if len(images) > 0 {
		promptText, _ := prompt.(string)
		persistUserMessage = tuigateway.BuildPersistUserMessage(promptText, images, runMessage)
	}
	opts := &agent.RunConversationOptions{ConversationHistory: historyAsMessages(st.history), StreamCallback: stream,
		PersistUserMessage: persistUserMessage, TaskID: strOr(session.Get("session_key"))}
	if displayKind != "" {
		opts.PersistUserDisplayKind = displayKind
	}
	if len(displayMetadata) > 0 {
		opts.PersistUserDisplayMetadata = displayMetadata
	}
	if len(turnAuthor) > 0 {
		opts.TurnAuthor = turnAuthor
	}
	if steps := tools.OnboardingPrelude(a, opts.ConversationHistory); steps != nil {
		opts.Prelude = &onboardingPreludeSteps{steps: steps}
	}
	st.runOpts = opts
	adoptSubmitUserRow(ctx, session, a, persistUserMessage, text)
	titleKey := sessionKeyOr(session, sid)
	a.SetOnSessionTitle(func(title, _ string) {
		tuigateway.Emit(ctx, "session.title", sid, map[string]any{"session_id": titleKey, "title": title})
	})
	stopUsage := startUsageTicker(ctx, sid, a, time.Second)
	defer stopUsage()
	turnCtx, endTurn := a.NotificationTurn(ctx, agent.EventPresentationMuted(ctx, "message.delta", sid), sid)
	defer endTurn()
	result, err := a.RunConversationContent(turnCtx, runMessage, *opts)
	if err != nil {
		panic(err)
	}
	st.result = result
	if hold != nil {
		if rest := hold.Finish(); rest != "" && !a.MuteNotificationReply() {
			emitDelta(rest)
		}
	}
}

// Stamps a synthesized turn's display kind, restores a one shot model and commits history, returning the history warning
func absorbTurnResult(ctx context.Context, sid string, session *tuigateway.Session, st *turnRun, text any, displayKind string, displayMetadata map[string]any) string {
	result, a := st.result, st.agent
	messages, hasMessages := resultMessages(result)
	if s, isText := text.(string); displayKind != "" && isText {
		if db := a.SessionDB(); db != nil {
			currentSessionID := a.SessionID()
			if currentSessionID == "" {
				currentSessionID = strOr(session.Get("session_key"))
			}
			var meta any
			if displayMetadata != nil {
				meta = displayMetadata
			}
			if _, err := db.SetLatestMatchingMessageDisplayKind(ctx, currentSessionID, "user", s, displayKind, meta); err != nil {
				logger().Debug("failed to stamp synthetic display kind", "error", err)
			}
		}
		if hasMessages {
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i]["role"] == "user" && messages[i]["content"] == s {
					messages[i]["display_kind"] = displayKind
					if len(displayMetadata) > 0 {
						messages[i]["display_metadata"] = displayMetadata
					}
					break
				}
			}
		}
	}
	if restore, present := session.Lookup("moa_one_shot_restore"); present {
		session.Pop("moa_one_shot_restore")
		switch r := restore.(type) {
		case map[string]any:
			prevOverride := r["override"]
			if prevOverride == nil {
				session.Pop("model_override")
			} else {
				session.Set("model_override", prevOverride)
			}
			if prevModel := r["model"]; truthy(prevModel) {
				raw := pyrepr.Str(prevModel)
				if prevProvider := r["provider"]; truthy(prevProvider) {
					raw += " --provider " + pyrepr.Str(prevProvider)
				}
				if failure := catchPanic(func() {
					if _, err := applyModelSwitch(ctx, sid, session, raw, modelSwitchOptions{pinSessionOverride: truthy(prevOverride),
						persistOverride: new(bool), countSwitch: false, confirmExpensiveModel: false}); err != nil {
						panic(err)
					}
				}); failure != nil {
					logger().Warn("MoA one-shot model restore failed: " + pyrepr.Str(failure))
				}
			}
		case nil:
			session.Pop("model_override")
		default:
			session.Set("model_override", restore)
		}
	}
	statusNote := ""
	if result != nil {
		if hasMessages {
			statusNote = commitTurnHistory(session, messages, st.history, st.historyVersion)
		}
		syncSessionKeyAfterCompress(ctx, sid, session, false, true)
	}
	return statusNote
}

// Message list of a conversation result when it holds one
func resultMessages(result map[string]any) ([]map[string]any, bool) {
	switch m := result["messages"].(type) {
	case []map[string]any:
		return m, true
	case []any:
		return historyAsMessages(m), true
	}
	return nil, false
}

// Committed row addresses of this turn, nil when the result staged no history
func persistedTurnReceiptOf(st *turnRun, raw any, status string) map[string]any {
	if _, ok := resultMessages(st.result); !ok {
		return nil
	}
	unchanged := false
	if st.compressionCount != nil {
		if count, ok := st.agent.ContextCompressionCount(); ok {
			unchanged = *st.compressionCount == count
		}
	}
	return agent.PersistedTurnReceipt(st.result["messages"], st.agent.PersistUserMessageIdx(), historyAsMessages(st.history), raw, status,
		unchanged, func(before, after any) bool {
			b := agent.CommittedRowID(before)
			return b != nil && pyjson.Equal(b, agent.CommittedRowID(after))
		})
}

// Payload, raw text and status of message complete, retaining or clearing the in flight turn
func completeTurnPayload(ctx context.Context, session *tuigateway.Session, st *turnRun, statusNote string, cols int) (map[string]any, any, string) {
	result, a := st.result, st.agent
	var errorSurface map[string]any
	if resultStatus(result) == "error" {
		if failure := catchPanic(func() { errorSurface = agent.BuildErrorSurfaceFromResult(result, a.Provider(), a.Model()) }); failure != nil {
			errorSurface = nil
		}
	}
	raw, status, lastReasoning := turnOutcome(result, errorSurface)
	receipt := persistedTurnReceiptOf(st, raw, status)
	if isBotModeSession(ctx, session) {
		raw = botModeDeliveryText(raw, status == "complete")
	}
	payload := map[string]any{"text": raw, "usage": getUsage(ctx, a), "status": status}
	if len(receipt) > 0 {
		payload["persisted_turn"] = receipt
	}
	if lastReasoning != "" {
		payload["reasoning"] = lastReasoning
	}
	if statusNote != "" {
		payload["warning"] = statusNote
	}
	rawText, rawIsText := raw.(string)
	if truthy(result["response_previewed"]) || (rawIsText && a.InterimTextWasDelivered(rawText)) {
		payload["response_previewed"] = true
	}
	if truthy(raw) && truthy(result["response_reused"]) {
		payload["response_reused"] = true
	}
	if truthy(result["response_transformed"]) {
		payload["response_transformed"] = true
	}
	if billing := result["billing_block"]; truthy(billing) {
		payload["billing"] = billing
		payload["failure_reason"] = result["failure_reason"]
	}
	if rendered, ok := renderMessage(pyrepr.Str(raw), cols); ok && rendered != "" {
		payload["rendered"] = rendered
	}
	errorValue := result["error"]
	finalText, finalIsText := result["final_response"].(string)
	errorText := ""
	if truthy(errorValue) {
		errorText = pyrepr.Str(errorValue)
	}
	hasPartialText := truthy(result["partial"]) && finalIsText && pystr.Strip(finalText) != "" && pystr.Strip(finalText) != pystr.Strip(errorText)
	func() {
		unlock := lockHistory(session)
		defer unlock()
		if status == "error" {
			inflight, _ := session.Get("inflight_turn").(map[string]any)
			if hasPartialText && !truthy(inflight["assistant"]) {
				appendInflightDelta(session, raw)
			}
			failInflightTurn(session, errorValue, errorSurface)
			st.errorRetained = true
			st.errorDetail = turnFailureDetail(errorValue, result["failure_reason"], st.promptText)
		} else {
			clearInflightTurn(session)
		}
	}()
	errorOrRaw := errorText
	if errorOrRaw == "" {
		errorOrRaw = pyrepr.Str(raw)
	}
	if status == "error" {
		payload["error"] = errorOrRaw
		payload["recoverable"] = true
		if hasPartialText {
			payload["partial"] = true
		}
		if len(errorSurface) > 0 {
			payload["error_surface"] = errorSurface
		}
	}
	if st.terminalCallback != nil {
		st.receiptAttempted = true
		receiptStatus := map[string]string{"interrupted": "cancelled", "error": "failed"}[status]
		if receiptStatus == "" {
			receiptStatus = "settled"
		}
		terminal := map[string]any{"status": receiptStatus, "text": pyrepr.Str(raw)}
		if status == "error" {
			terminal["error"] = errorOrRaw
		}
		st.terminalCallback(terminal)
		st.receiptCommitted = true
	}
	if st.receiptCommitted {
		retireTurnMarker(ctx, session, st.markerKey)
	}
	return payload, raw, status
}

// Crash log of the turn dispatcher under the launch home
func crashLogPath() string { return filepath.Join(launchHome(), "logs", "tui_gateway_crash.log") }

// Except path of the turn, the crash log, the history restore and the terminal error frame
func recoverTurnException(ctx context.Context, sid string, session *tuigateway.Session, st *turnRun, failure any) {
	err, ok := failure.(error)
	if !ok {
		err = fmt.Errorf("%s", pyrepr.Str(failure))
	}
	suppress(func() {
		path := crashLogPath()
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o777); mkErr != nil {
			panic(mkErr)
		}
		f, openErr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if openErr != nil {
			panic(openErr)
		}
		defer f.Close()
		fmt.Fprintf(f, "\n=== turn-dispatcher exception · %s · sid=%s ===\n", time.Now().Format("2006-01-02 15:04:05"), sid)
		fmt.Fprintf(f, "%s: %s\n%s", pyrepr.TypeName(err), err, debug.Stack())
	})
	fmt.Fprintf(os.Stderr, "[gateway-turn] %s: %s\n", pyrepr.TypeName(err), err)
	restoreAgentHistoryAfterTurnError(session, st.agent)
	if st.terminalCallback != nil && !st.receiptAttempted {
		st.receiptAttempted = true
		if cbFailure := catchPanic(func() {
			st.terminalCallback(map[string]any{"status": "failed", "text": "", "error": err.Error()})
			st.receiptCommitted = true
		}); cbFailure != nil {
			logger().Error("hosted room terminal receipt commit failed", "error", pyrepr.Str(cbFailure))
		}
	}
	if emitFailure := catchPanic(func() {
		emitTerminalTurnError(ctx, sid, session, err, nil, st.receiptCommitted)
		st.errorRetained = true
		st.errorDetail = turnFailureDetail(err, pyrepr.TypeName(err), st.promptText)
	}); emitFailure != nil {
		fmt.Fprintf(os.Stderr, "[gateway-turn] terminal error emit failed: %s: %s\n", pyrepr.TypeName(emitFailure), pyrepr.Str(emitFailure))
		tuigateway.Emit(ctx, "error", sid, map[string]any{"message": err.Error()})
	}
}

// Finally path before settlement, dropping snapshots, ending turn audio, undoing a one turn model and resetting scopes
func releaseTurnScopes(ctx context.Context, sid string, session *tuigateway.Session, st *turnRun) {
	st.history = st.history[:0]
	if st.runOpts != nil {
		*st.runOpts = agent.RunConversationOptions{}
	}
	if st.thinkingStarted {
		suppress(media.StopThinkingSound)
	}
	if st.ttsQueue != nil {
		st.ttsQueue.Finish()
	}
	if st.oneTurnRestore != nil {
		if failure := catchPanic(func() {
			restoreAgentModelRuntime(ctx, st.agent, st.oneTurnRestore)
			restartSlashWorker(ctx, sid, session)
			persistLiveSessionRuntime(ctx, session)
			persistLiveSessionSystemPrompt(ctx, session)
		}); failure != nil {
			logger().Debug("TUI one-turn model restore failed", "error", pyrepr.Str(failure))
		}
	}
	suppress(func() { tools.ResetCurrentSessionKey(st.sessionEnv) })
	clearSessionContext(st.sessionEnv)
}

// Tail after the turn settled, the memory trim while the profile home is still bound
func postTurnHousekeeping(sid string, st *turnRun) {
	ctx := st.homeCtx
	if failure := catchPanic(func() {
		if sessionsQuiescent(sid) {
			memory.TrimMemory(ctx, configEnv(ctx), memory.TrimOptions{Reason: "tui turn completion", AllocatorOnly: true})
		}
	}); failure != nil {
		logger().Debug("post-turn memory trim failed", "error", pyrepr.Str(failure))
	}
}

const routingReopenPatienceS = 0.5

// Clears an explicit close stamp of a session still registered here and about to run a turn
func reopenRoutedSessionRow(ctx context.Context, db *state.DB, sid string, session *tuigateway.Session) {
	a := sessionAgent(session)
	if db == nil || a == nil || a.SessionID() == "" {
		return
	}
	patience := routingReopenPatienceS
	if _, err := db.ReopenIfExplicitlyClosed(ctx, a.SessionID(), "TUI session "+sid+" is still registered and accepting a turn", &patience); err != nil {
		logger().Debug("routing-provenance reopen failed for "+a.SessionID(), "error", err)
	}
}

// Whether an error is the ValueError of a store write
func isValueError(err error) bool {
	var named pyrepr.TypeNamer
	if errors.As(err, &named) && named.PyTypeName() == "ValueError" {
		return true
	}
	var pyErr *pyint.Error
	return errors.As(err, &pyErr) && pyErr.Kind == "ValueError"
}

// Admits and starts one prompt turn on its own goroutine, false when it never started
func runPromptSubmit(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, o promptSubmitOptions) bool {
	if !ensureSessionDBRow(ctx, session) {
		logger().Warn(fmt.Sprintf("prompt dispatch: session store unavailable for %s — this turn may not persist", sessionKeyOr(session, sid)))
	}
	images, a, admitted := admitPromptTurn(ctx, sid, session, text, o.imagePaths, o.queuedPromptGeneration, o.displayKind, o.displayMetadata, !o.silentOwnershipRefusal)
	if !admitted {
		return false
	}
	displayKind := o.displayKind
	scoped := sessionProfileRuntimeScope(ctx, session, true)
	notificationConfig := agent.NotificationConfigSnapshot(scoped)
	muted := config.DiagnosticTurnMuted(scoped, o.displayMetadata, "tui", notificationConfig)
	if muted {
		displayKind = "hidden"
	}
	started := time.Now()
	kind := displayKind
	if kind == "" {
		kind = "user"
	}
	chars := "-"
	if s, ok := text.(string); ok {
		chars = fmt.Sprint(pystr.Len(s))
	}
	logger().Info(fmt.Sprintf("tui prompt accepted: ui_session=%s session_key=%s agent_session_id=%s kind=%s chars=%s images=%d",
		sid, strOr(session.Get("session_key")), a.SessionID(), kind, chars, len(images)))
	segments := tuigateway.NewTurnSegments()
	session.Set(tuigateway.SessionSegmentsKey, segments)
	if !muted {
		tuigateway.Emit(ctx, "message.start", sid, nil)
	}
	notificationCategory := ""
	if c, ok := o.displayMetadata["notification_category"].(string); ok {
		notificationCategory = c
	}
	runBody := func(ctx context.Context) (result map[string]any, goalFollowup string) {
		ctx = tuigateway.BindTransport(ctx, transportOf(session.Get("transport")))
		ctx = withRuntimeSessionRecord(ctx, session)
		restore, _ := session.Pop("one_turn_model_restore")
		restoreMap, _ := restore.(map[string]any)
		st := &turnRun{agent: sessionAgent(session), oneTurnRestore: restoreMap, terminalCallback: o.terminalCallback,
			receiptCommitted: o.terminalCallback == nil, homeCtx: ctx}
		st.markerKey = recordTurnMarker(ctx, session, text, o.terminalCallback == nil, notificationCategory)
		defer func() {
			defer postTurnHousekeeping(sid, st)
			releaseTurnScopes(ctx, sid, session, st)
			settle := st.homeCtx
			st.agent.SetInterimAssistantCallback(nil)
			func() {
				unlock := lockHistory(session)
				defer unlock()
				session.Set("running", false)
				session.Set("last_active", nowSeconds())
				session.Pop("_turn_user_input")
				if !st.errorRetained {
					clearInflightTurn(session)
				}
				releaseHostedRoomTurnSlot(session)
				if session.Get(tuigateway.SessionSegmentsKey) == segments {
					session.Pop(tuigateway.SessionSegmentsKey)
				}
			}()
			status := "complete"
			if st.result != nil {
				status = resultStatus(st.result)
			} else if st.errorRetained {
				status = "error"
			}
			logger().Info(fmt.Sprintf("tui turn finished: ui_session=%s session_key=%s agent_session_id=%s status=%s error_retained=%s duration=%.1fs%s",
				sid, strOr(session.Get("session_key")), st.agent.SessionID(), status, pyrepr.Str(st.errorRetained), time.Since(started).Seconds(), st.errorDetail))
			if st.receiptCommitted {
				retireTurnMarker(settle, session, st.markerKey)
				unlock := lockHistory(session)
				if session.Get("_active_turn_marker_key") == st.markerKey {
					session.Pop("_active_turn_marker_key")
				}
				session.Pop("_hosted_room_task")
				unlock()
			}
			session.Pop("_auto_continue_scheduled")
			emitSettledSessionInfo(settle, sid, session, st.agent)
			result = st.result
		}()
		failure := catchPanic(func() {
			turnCtx, prompt, runMessage, cols, streamer, ok := prepareTurnInput(ctx, sid, session, st, text, images)
			if !ok {
				if st.terminalCallback != nil && !st.receiptAttempted {
					st.receiptAttempted = true
					st.terminalCallback(map[string]any{"status": "failed", "text": "", "error": "Context injection refused."})
					st.receiptCommitted = true
				}
				return
			}
			invokeAgent(turnCtx, sid, session, st, prompt, runMessage, streamer, images, displayKind, o.displayMetadata, o.turnAuthor, text)
			statusNote := absorbTurnResult(turnCtx, sid, session, st, text, displayKind, o.displayMetadata)
			payload, raw, status := completeTurnPayload(turnCtx, session, st, statusNote, cols)
			if status == "complete" {
				noteTaskDone(turnCtx, session, st.result, st.agent, displayKind)
			}
			tuigateway.Emit(turnCtx, "message.complete", sid, payload)
			goalFollowup = goalFollowupAfterTurn(turnCtx, sid, session, st.result, status, raw)
			if status == "complete" {
				afterCompleteTurn(turnCtx, sid, session, st, raw)
			}
			publishSessionControlSnapshot(turnCtx, sid, session, true, false)
		})
		if failure != nil {
			recoverTurnException(ctx, sid, session, st, failure)
		}
		return st.result, goalFollowup
	}
	run := func(ctx context.Context) {
		restorePolicy := a.NotificationPolicySnapshot("tui", notificationConfig)
		turnCtx, endTurn := a.NotificationTurn(ctx, muted, sid)
		result, goalFollowup := func() (map[string]any, string) {
			defer restorePolicy()
			defer endTurn()
			return runBody(turnCtx)
		}()
		runPostTurnFollowups(ctx, rid, sid, session, result, goalFollowup)
	}
	canStart := false
	func() {
		db, release := routingProvenanceDB(ctx, session)
		defer release()
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		registered := tuigateway.SessionRecord(sid)
		canStart = !truthy(session.Get("_closing")) && (registered == nil || registered == session)
		if canStart {
			if registered == session {
				reopenRoutedSessionRow(ctx, db, sid, session)
			}
			canStart = startSessionWork(ctx, run, "prompt-turn-"+sid, session) != nil
		}
	}()
	if !canStart {
		unlock := lockHistory(session)
		session.Set("running", false)
		if session.Get(tuigateway.SessionSegmentsKey) == segments {
			session.Pop(tuigateway.SessionSegmentsKey)
		}
		unlock()
	}
	return canStart
}

// The session's own store for the routing provenance heal, nil when it cannot be resolved
func routingProvenanceDB(ctx context.Context, session *tuigateway.Session) (db *state.DB, release func()) {
	release = func() {}
	if failure := catchPanic(func() { db, release = sessionDB(ctx, session) }); failure != nil {
		logger().Debug("could not resolve the session db for routing provenance", "error", pyrepr.Str(failure))
		return nil, func() {}
	}
	return db, release
}

// Transport slot of a record, nil when it holds none
func transportOf(v any) tuigateway.Transport {
	t, _ := v.(tuigateway.Transport)
	return t
}

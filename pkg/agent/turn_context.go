package agent

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Index of this turn's user row after compaction, the last exact match else the last user originated row
func reanchorCurrentTurnUserIdx(messages []*state.Message, userMessage any) int {
	fallback := -1
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil || msg.Role != "user" {
			continue
		}
		if reflect.DeepEqual(msg.Content, userMessage) {
			return i
		}
		view := UserOriginatedTurnView(state.MessageToDict(msg))
		if view == nil {
			continue
		}
		if reflect.DeepEqual(view["content"], userMessage) {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

func turnContextLog() *slog.Logger { return slog.Default().With("logger", "agent.turn_context") }

const PreLLMBlockExitReason = "blocked_by_plugin_pre_llm_call"

var untitledPlatforms = map[string]bool{"cron": true}

// Per turn agent slots the prologue resets or binds before the loop reads them
type turnContextState struct {
	persistUserMessagePlatformID                 *string
	currentTaskID                                string
	processOwnerTaskIDs                          map[string]bool
	invalidToolRetries, invalidJSONRetries       int
	incompleteScratchpadRetries                  int
	thinkingPrefillRetries                       int
	postToolEmptyRetried                         bool
	lastContentWithTools                         any
	lastContentToolsAllHousekeeping              bool
	reusedResponseText                           *string
	mutePostResponse                             bool
	unicodeSanitizationPasses                    int
	verificationStopNudges, preVerifyNudges      int
	lspTurnEndNotes                              int
	currentTurnTimestamp                         float64
	compressionWarning                           any
	deferredTitleUpgrade                         any
	reviewAttended                               bool
	turnOrigin                                   string
	gatewayTurnContextNotes, skillsIndexNoteText string
	inflightTurnID                               string
	inflightTurnStarted                          float64
	reviewDeferCompactionBeforeFirstResponse     bool
	turnReceivedProviderResponse                 bool
	titleFailureCallback, onSessionTitle         any
	authMode                                     string
}

// Token estimate for automatic preflight, the provider usage anchor else the native wire else the generic estimate
func preflightRequestTokens(ctx context.Context, a *AIAgent, messages []*state.Message, systemPrompt string) int {
	anchored, ok := provider.AnchoredContextTokens(ctx, messages, a.usageAnchor, true)
	a.requestPressureAnchored = ok
	if ok {
		return anchored
	}
	toolDefs := toolDefsOf(a.tools)
	if native, ok := estimateNativeResponsesPreflightTokens(ctx, a, messages, systemPrompt, toolDefs); ok && native >= 0 {
		return native
	}
	chargeStale := agentStaleThinkingOnWire(a)
	estimate := messages
	if chargeStale && a.apiMode == "anthropic_messages" && provider.NativeAnthropicPreservesPriorThinking(a.baseURL, a.model) {
		dicts := make([]map[string]any, len(messages))
		for i, m := range messages {
			dicts[i] = state.MessageToDict(m)
		}
		applyRejectedThinkingSuppression(a, dicts)
		estimate = make([]*state.Message, len(dicts))
		for i, d := range dicts {
			estimate[i] = state.MessageFromDict(d)
		}
		return provider.EstimateNativeAnthropicRequestTokens(ctx, estimate, systemPrompt, toolDefs)
	}
	return provider.EstimateRequestTokens(ctx, estimate, systemPrompt, toolDefs, chargeStale)
}

// Whether the active route replays stale thinking text on the wire
func agentStaleThinkingOnWire(a *AIAgent) bool {
	return staleThinkingReachesWire(a.apiMode, a.provider, a.model, a.baseURL)
}

// One text block of the turn's memory prefetch and pre_llm_call context, false when nothing is injected
func composeMultimodalContextPart(extPrefetchCache, pluginUserContext string) (string, bool) {
	fenced := ""
	if extPrefetchCache != "" {
		fenced = memory.BuildMemoryContextBlock(extPrefetchCache)
	}
	var parts []string
	for _, p := range []string{fenced, pluginUserContext} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// API bound content of the current string user turn, false when nothing is injected or content is not a string
func composeUserAPIContent(content any, extPrefetchCache, pluginUserContext string) (string, bool) {
	text, ok := content.(string)
	if !ok {
		return "", false
	}
	injection, ok := composeMultimodalContextPart(extPrefetchCache, pluginUserContext)
	if !ok {
		return "", false
	}
	return text + "\n\n" + injection, true
}

// Pops the api_content sidecar into content for user and assistant rows, returning the popped value
func substituteAPIContent(apiMsg map[string]any) any {
	sidecar, had := apiMsg["api_content"]
	delete(apiMsg, "api_content")
	if !had {
		return nil
	}
	if s, ok := sidecar.(string); ok && s != "" && (apiMsg["role"] == "user" || apiMsg["role"] == "assistant") {
		apiMsg["content"] = s
	}
	return sidecar
}

// Drops the api_content sidecar from a message whose content was rewritten
func dropStaleAPIContent(msg map[string]any) { delete(msg, "api_content") }

// The api_content sidecar, false when absent or not a string
func extractAPIContentSidecar(msg map[string]any) (string, bool) {
	s, ok := msg["api_content"].(string)
	return s, ok
}

// Reads and clears a one shot per turn note slot
func popTurnNote(slot *string) string {
	note := *slot
	*slot = ""
	return note
}

func consumeGatewayTurnContextNotes(a *AIAgent) string {
	return popTurnNote(&a.gatewayTurnContextNotes)
}
func consumeSurfaceSwitchNote(a *AIAgent) string { return popTurnNote(&a.surfaceSwitchNote) }
func consumeSkillsIndexNote(a *AIAgent) string   { return popTurnNote(&a.skillsIndexNoteText) }

// Appends must deliver notes as a text part on a multimodal user content list, true when appended
func appendNotesToMultimodalContent(content *any, notes string) bool {
	list, ok := (*content).([]any)
	if notes == "" || !ok {
		return false
	}
	*content = append(list, map[string]any{"type": "text", "text": notes})
	return true
}

// Fires the turn start auto title for the session unless the platform stays untitled
func maybeTitleSessionAtTurnStart(ctx context.Context, a *AIAgent, messages []*state.Message, titleUserMessage *string) {
	if a.sessionDB == nil || a.sessionID == "" {
		return
	}
	platform := strings.ToLower(a.platform)
	if untitledPlatforms[platform] {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			turnContextLog().Debug("Turn-start auto-title dispatch failed", "error", r)
		}
	}()
	userText := ""
	var titlePreview *string
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil || msg.Role != "user" {
			continue
		}
		userText = strings.TrimSpace(state.FlattenMessageText(msg.Content, ""))
		if p, ok := msg.DisplayMetadata["title_preview"].(string); ok {
			titlePreview = &p
		}
		break
	}
	if titleUserMessage != nil {
		userText = strings.TrimSpace(*titleUserMessage)
	}
	if userText == "" {
		return
	}
	if !a.sessionDBCreated {
		if err := a.ensureDBSession(); err != nil {
			turnContextLog().Debug("Turn-start auto-title dispatch failed", "error", err)
			return
		}
		if !a.sessionDBCreated {
			return
		}
	}
	if platform == "subagent" {
		applySubagentTitle(ctx, a.sessionDB, a.sessionID, userText)
		return
	}
	mainRuntime := map[string]any{"model": a.model, "provider": a.provider, "base_url": a.baseURL, "api_key": a.apiKey, "api_mode": a.apiMode, "session_id": a.sessionID}
	failure := a.titleFailureCallback
	if failure == nil {
		failure = a.emitAuxiliaryFailure
	}
	upgrade, started := maybeAutoTitle(ctx, a.sessionDB, a.sessionID, userText, autoTitleOptions{
		ConversationHistory: messages, FailureCallback: failure, MainRuntime: mainRuntime, TitleCallback: a.onSessionTitle,
		RuntimeValidator: func() bool { return a.model == mainRuntime["model"] && a.provider == mainRuntime["provider"] },
		TitlePreview:     titlePreview,
	})
	if upgrade != nil && !started {
		a.deferredTitleUpgrade = upgrade
	}
}

// Starts the title upgrade deferred at turn start
func startDeferredTitleUpgrade(a *AIAgent) {
	upgrade := a.deferredTitleUpgrade
	if upgrade == nil {
		return
	}
	a.deferredTitleUpgrade = nil
	startTitleUpgrade(upgrade)
}

// Stamps turn_id and current_turn_user_idx on a result only when the addressed row is this turn verbatim
func exportCurrentTurnBoundary(a *AIAgent, result map[string]any, userMessage any) map[string]any {
	if result == nil || result["turn_exit_reason"] == "context_compression_timeout" {
		return result
	}
	turnID := a.currentTurnID
	var messages []*state.Message
	switch rows := result["messages"].(type) {
	case []*state.Message:
		messages = rows
	case []map[string]any:
		messages = make([]*state.Message, len(rows))
		for i, r := range rows {
			messages[i] = state.MessageFromDict(r)
		}
	default:
		return result
	}
	if turnID == "" || userMessage == nil {
		return result
	}
	idx := reanchorCurrentTurnUserIdx(messages, userMessage)
	if idx < 0 || idx >= len(messages) {
		return result
	}
	row := messages[idx]
	if row == nil || row.Role != "user" {
		return result
	}
	view := UserOriginatedTurnView(state.MessageToDict(row))
	if !reflect.DeepEqual(row.Content, userMessage) && !(view != nil && reflect.DeepEqual(view["content"], userMessage)) {
		return result
	}
	result["turn_id"] = turnID
	result["current_turn_user_idx"] = idx
	return result
}

// True when a compression pass dropped rows or cut tokens by more than five percent
func compressionMadeProgress(origLen, newLen, origTokens, newTokens int) bool {
	return newLen < origLen || (origTokens > 0 && float64(newTokens) < float64(origTokens)*0.95)
}

// Error raised when preflight compression cannot make the request fit before the provider call
type PreflightCompressionTimedOut struct{ Msg string }

func (e *PreflightCompressionTimedOut) Error() string { return e.Msg }

// Stops an oversized turn whose compression wait timed out unless the request still fits the window
func failClosedAfterPreflightTimeout(a *AIAgent, requestTokens int) error {
	if !contextCompressionTimedOut(a) {
		return nil
	}
	if fits := requestExceedsModelWindow(a, requestTokens); fits != nil && !*fits {
		turnContextLog().Warn(fmt.Sprintf("Preflight compression timed out but the request (~%s tokens) fits the model window (%s); sending it uncompressed this turn",
			groupThousands(requestTokens), groupThousands(a.contextCompressor.Base().ContextLength)))
		return nil
	}
	return &PreflightCompressionTimedOut{Msg: "Context compression timed out before it could commit while the request " +
		fmt.Sprintf("was still approximately %s tokens. The provider call ", groupThousands(requestTokens)) +
		"was not sent. Run /compress and wait for it to finish, then retry."}
}

// Stops an over window turn the moment preflight proves the session cannot shrink
func failClosedOnInsufficientProgress(a *AIAgent, requestTokens int) error {
	if exceeds := requestExceedsModelWindow(a, requestTokens); exceeds == nil || !*exceeds {
		return nil
	}
	if compressionBlockedTransiently(a) {
		return nil
	}
	window := a.contextCompressor.Base().ContextLength
	return &PreflightCompressionTimedOut{Msg: "Context compression could not bring this session under the model's context window " +
		fmt.Sprintf("(~%s tokens vs %s). The provider call was not ", groupThousands(requestTokens), groupThousands(window)) +
		"sent. Start a new session with /new; this session is too large to compress further."}
}

// Whether a detached review fork still waits on its first provider response
func reviewForkFirstRequestPending(a *AIAgent) bool {
	return a.reviewDeferCompactionBeforeFirstResponse && !a.turnReceivedProviderResponse
}

// Another immediate summary only while still over threshold after a pass that cut more than five percent
func compressionWarrantsAnotherPreflightPass(origTokens, newTokens, thresholdTokens int) bool {
	return newTokens >= thresholdTokens && origTokens > 0 && float64(newTokens) < float64(origTokens)*0.95
}

// Cheap gate for the full preflight estimate, row count past the protected ranges or a rough estimate over threshold
func shouldRunPreflightEstimate(ctx context.Context, messages []*state.Message, protectFirstN, protectLastN, thresholdTokens int) bool {
	return len(messages) > protectFirstN+protectLastN+1 || provider.EstimateMessagesTokens(messages, provider.CurrentImageTokenCost(ctx), true) >= thresholdTokens
}

// Idle compaction fires after the configured gap above the floor raised by the last pass, never in cooldown
func shouldIdleCompact(enabled bool, idleAfterSeconds int, idleGapSeconds float64, tokens, floorTokens int, cooldownActive bool, lastCompactionTokens int) bool {
	if !enabled || idleAfterSeconds <= 0 || idleGapSeconds < float64(idleAfterSeconds) || cooldownActive {
		return false
	}
	effective := floorTokens
	if lastCompactionTokens > 0 {
		effective = max(effective, lastCompactionTokens+floorTokens)
	}
	return tokens > effective
}

// Runs fn under the session persist lock, logs failures, then drops staged CLI input unless kept for a retry
func persistUnderLock(a *AIAgent, fn func() error, failureMsg string, pendingCLIMessage map[string]any) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				turnContextLog().Warn(fmt.Sprintf(failureMsg, orNone(a.sessionID)), "error", r)
			}
		}()
		if a.sessionPersistLock != nil {
			a.sessionPersistLock.Lock()
			defer a.sessionPersistLock.Unlock()
		}
		if err := fn(); err != nil {
			turnContextLog().Warn(fmt.Sprintf(failureMsg, orNone(a.sessionID)), "error", err)
		}
	}()
	if pendingCLIMessage == nil || pyTruthy(pendingCLIMessage[state.DBPersistedMarker]) {
		a.pendingCLIUserMessage = nil
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Tells the auxiliary client the live main provider and model for this turn
func publishRuntimeMain(ctx context.Context, a *AIAgent) (out context.Context) {
	out = ctx
	defer func() { _ = recover() }()
	scope := provider.ResolvePromptCacheScopeSafe(a.cacheScopeAgent())
	return provider.SetRuntimeMain(ctx, a.provider, a.model, provider.RuntimeMainOptions{
		RequestedProvider: a.requestedProvider, BaseURL: a.baseURL, APIKey: a.apiKey, APIMode: a.apiMode,
		AuthMode: a.authMode, SessionID: a.sessionID, CacheScope: scope,
	})
}

// Adopts late MCP connections and refreshes the MCP tool snapshot before the first call of the turn
func refreshMCPToolsBetweenTurns(ctx context.Context, a *AIAgent) {
	defer func() {
		if r := recover(); r != nil {
			turnContextLog().Debug("between-turns MCP tool refresh skipped", "error", r)
		}
	}()
	mcp.AdoptLateConnections(ctx, a)
	if !a.skipMCPRefresh && hasRegisteredMCPTools() {
		mcp.RefreshAgentMCPTools(ctx, a, mcp.RefreshOptions{Quiet: true, PreservePrefix: true})
	}
}

// Stages callback and persist overrides on the agent and binds this turn's task and turn ids
func bindTurnIdentity(a *AIAgent, taskID string, streamCallback func(string), persistUserMessage any, persistUserTimestamp *float64, persistUserPlatformID *string) (string, string) {
	a.streamCallback = streamCallback
	a.persistUserMessageIdx = nil
	a.persistUserMessageOverride = persistUserMessage
	a.persistUserMessageTimestamp = nil
	if persistUserTimestamp != nil {
		a.persistUserMessageTimestamp = *persistUserTimestamp
	}
	a.persistUserMessagePlatformID = persistUserPlatformID
	effectiveTaskID := taskID
	if effectiveTaskID == "" {
		effectiveTaskID = uuid.NewString()
	}
	a.currentTaskID = effectiveTaskID
	if a.processOwnerTaskIDs == nil {
		a.processOwnerTaskIDs = map[string]bool{}
	}
	a.processOwnerTaskIDs[effectiveTaskID] = true
	turnID := a.relayPendingTurnID
	if turnID == "" {
		session := a.sessionID
		if session == "" {
			session = "session"
		}
		turnID = fmt.Sprintf("%s:%s:%s", session, effectiveTaskID, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	}
	a.relayPendingTurnID = ""
	a.currentTurnID = turnID
	a.currentAPIRequestID = ""
	noteTurnStart(a, turnID)
	return effectiveTaskID, turnID
}

// Resets the per turn retry counters, guardrails, budgets and scrubbers and replays pending notices
func resetPerTurnAgentState(ctx context.Context, a *AIAgent) {
	a.invalidToolRetries, a.invalidJSONRetries, a.emptyContentRetries = 0, 0, 0
	a.incompleteScratchpadRetries, a.codexIncompleteRetries, a.codexReasoningOnlyStreak = 0, 0, 0
	a.thinkingPrefillRetries, a.postToolEmptyRetried = 0, false
	a.lastContentWithTools, a.lastContentToolsAllHousekeeping = nil, false
	a.reusedResponseText = nil
	a.mutePostResponse, a.unicodeSanitizationPasses = false, 0
	a.toolGuardrailHaltDecision = nil
	observability.ResetHarnessTurn(a)
	a.iterationBudgetWarningInjected = false
	a.runBudgetWrapupInjected, a.verificationStopNudges = false, 0
	a.preVerifyNudges, a.lspTurnEndNotes = 0, 0
	a.turnFailedFileMutations = NewFileMutationFailures()
	a.turnFileMutationPaths = map[string]bool{}
	a.toolGuardrails.ResetForTurn()
	if a.memoryStore != nil {
		a.memoryStore.ResetConsolidationFailures()
	}
	a.currentTurnTimestamp = nowSeconds()
	if a.apiMode != "anthropic_messages" {
		func() {
			defer func() { _ = recover() }()
			if cleanupDeadConnections(a) {
				a.emitDiagnosticStatus("🔌 Detected stale connections from a previous provider issue — cleaned up automatically. Proceeding with fresh connection.")
			}
		}()
	}
	func() {
		defer func() { _ = recover() }()
		retryRecoveredLocalServer(a)
	}()
	if pyTruthy(a.compressionWarning) {
		a.replayCompressionWarning()
		a.compressionWarning = nil
	}
	deliverKeptStartupNotices(ctx, a)
	if a.hasPendingStartupWarnings() {
		a.replayStartupWarnings()
	}
	a.iterationBudget = NewIterationBudget(a.maxIterations)
	a.runBudgetStartedAt = 0
	if a.runBudgetSeconds != 0 {
		a.runBudgetStartedAt = nowSeconds()
	}
	if a.streamContextScrubber != nil {
		a.streamContextScrubber.Reset()
	}
	if a.streamThinkScrubber != nil {
		a.streamThinkScrubber.Reset()
	}
}

// Builds this turn's user row, reusing CLI staged input only when its clean text matches this turn
func stageTurnUserMessage(a *AIAgent, userMessage, persistUserMessage any, persistUserTimestamp *float64, persistUserPlatformID *string, displayKind string, displayMetadata map[string]any) (map[string]any, map[string]any) {
	pending, _ := a.pendingCLIUserMessage.(map[string]any)
	expected := persistUserMessage
	if expected == nil {
		expected = userMessage
	}
	var userMsg map[string]any
	if pending != nil && reflect.DeepEqual(pending["content"], expected) {
		userMsg = pending
		userMsg["content"] = userMessage
	} else {
		userMsg = map[string]any{"role": "user", "content": userMessage}
		if pending != nil {
			a.pendingCLIUserMessage = nil
		}
	}
	state.StampMessageTimestamp(userMsg, persistUserTimestamp)
	if displayKind != "" {
		userMsg["display_kind"] = displayKind
	}
	if len(displayMetadata) > 0 {
		userMsg["display_metadata"] = displayMetadata
	}
	if persistUserPlatformID != nil {
		userMsg["platform_message_id"] = *persistUserPlatformID
	}
	return userMsg, pending
}

// Hydrates the todo store, usage anchor, native checkpoint and turn counters from restored history
func hydrateFromHistory(ctx context.Context, a *AIAgent, history []map[string]any) {
	if len(history) == 0 {
		return
	}
	if !a.todoStore.HasItems() {
		a.hydrateTodoStore(ctx, history)
	}
	if a.userTurnCount != 0 {
		return
	}
	restoreUsageAnchor(a, history)
	if noter, ok := a.contextCompressor.(nativeCompactionCheckpointNoter); ok {
		func() {
			defer func() {
				if r := recover(); r != nil {
					turnContextLog().Debug("restored native checkpoint hydration skipped", "error", r)
				}
			}()
			if hasReplayableNativeCompactionCheckpoint(a, history) {
				noter.NoteNativeCompactionCheckpoint()
				a.setUsageAnchor(nil, false)
			}
		}()
	}
	prior := 0
	for _, m := range history {
		if m["role"] == "user" {
			prior++
		}
	}
	if prior > 0 {
		a.userTurnCount = prior
		if a.memoryNudgeInterval > 0 && a.turnsSinceMemory == 0 {
			a.turnsSinceMemory = prior % a.memoryNudgeInterval
		}
	}
}

type nativeCompactionCheckpointNoter interface{ NoteNativeCompactionCheckpoint() }

// Counts the turn toward the memory nudge interval, true when the post turn review should fire
func tickMemoryNudge(a *AIAgent) bool {
	if a.memoryNudgeInterval > 0 && a.validToolNames["memory"] && a.memoryStore != nil {
		a.turnsSinceMemory++
		if a.turnsSinceMemory >= a.memoryNudgeInterval {
			a.turnsSinceMemory = 0
			return true
		}
	}
	return false
}

// Signals an affection reaction for words the user typed, never for hidden or skill expanded turns
func emitReaction(a *AIAgent, originalUserMessage any, displayKind string) {
	if a.reactionCallback == nil || displayKind == "hidden" {
		return
	}
	defer func() { _ = recover() }()
	if text, ok := originalUserMessage.(string); ok {
		if _, isSkill := skills.DescribeSkillInvocation(text, ""); isSkill {
			return
		}
	}
	if kind := detectReaction(originalUserMessage); kind != "" {
		a.reactionCallback(kind)
	}
}

// Creates the session row before preflight compression so compaction inserts reference it
func ensureSessionRow(a *AIAgent, pendingCLIMessage map[string]any) {
	persistUnderLock(a, a.ensureDBSession, "Turn-start session row creation failed for session=%s", pendingCLIMessage)
}

// Stores a vetoed prompt as the withheld placeholder, keeping the typed text only in display metadata
func withholdBlockedPrompt(ctx context.Context, a *AIAgent, turnUserMsg *state.Message, typed any) {
	content := turnUserMsg.Content
	var withheld any
	row := state.MessageToDict(turnUserMsg)
	if pyTruthy(row[state.CompressedSummaryMetadataKey]) {
		handoff := strings.TrimSpace(state.FlattenMessageText(state.HandoffOnlyContent(content), ""))
		if handoff != "" {
			withheld = handoff + "\n\n" + state.PreLLMBlockWithheldText
		} else {
			withheld = state.PreLLMBlockWithheldText
		}
	} else {
		typed = contentWithTurnOverride(row, content, typed)
		withheld = contentWithTurnOverride(row, content, state.PreLLMBlockWithheldText)
	}
	metadata := map[string]any{}
	for k, v := range turnUserMsg.DisplayMetadata {
		metadata[k] = v
	}
	metadata[state.WithheldPrompt] = pyrepr.Str(pyOr(durableContent(typed), ""))
	a.persistUserMessageOverride = state.PreLLMBlockWithheldText
	defer persistLock(a)()
	turnUserMsg.Content = withheld
	turnUserMsg.APIContent = ""
	turnUserMsg.DisplayMetadata = metadata
	rowID, ok := turnUserMsg.Extra["_row_id"].(int64)
	if a.sessionDB == nil || !ok {
		return
	}
	session, _ := turnUserMsg.Extra["_submit_row_session_id"].(string)
	if session == "" {
		session = a.sessionID
	}
	if _, err := a.sessionDB.SetUserMessageContent(ctx, session, rowID, withheld, metadata); err != nil {
		turnContextLog().Warn(fmt.Sprintf("withheld-prompt rewrite failed for session=%s", orNone(a.sessionID)), "error", err)
	}
}

// Runs pre_llm_call plugins, spilling oversized context and collecting block vetoes into blocks
func collectPreLLMCallContext(ctx context.Context, a *AIAgent, effectiveTaskID, turnID string, originalUserMessage any, messages []*state.Message, conversationHistory []map[string]any, blocks *[]string) (out string) {
	if a.persistDisabled {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			turnContextLog().Warn(fmt.Sprintf("pre_llm_call hook failed: %v", r))
			out = ""
		}
	}()
	history := make([]map[string]any, len(messages))
	for i, m := range messages {
		history[i] = state.MessageToDict(m)
	}
	results := lifecycleInvokeHook(ctx, "pre_llm_call", map[string]any{
		"session_id": a.sessionID, "task_id": effectiveTaskID, "turn_id": turnID, "user_message": originalUserMessage,
		"conversation_history": history, "is_first_turn": len(conversationHistory) == 0, "model": a.model,
		"platform": a.platform, "parent_session_id": a.parentSessionID, "sender_id": a.userID,
	})
	spillCfg := tools.SpillConfigFromEnv(a.env)
	var parts []string
	for _, r := range results {
		var piece string
		if d, ok := r.(map[string]any); ok && d["action"] == "block" {
			if msg, ok := d["message"].(string); ok && blocks != nil && strings.TrimSpace(msg) != "" {
				*blocks = append(*blocks, strings.TrimSpace(msg))
			}
			continue
		}
		if d, ok := r.(map[string]any); ok && pyTruthy(d["context"]) {
			piece = pyrepr.Str(d["context"])
		} else if s, ok := r.(string); ok && strings.TrimSpace(s) != "" {
			piece = s
		} else {
			continue
		}
		piece = tools.SpillIfOversized(a.env, piece, a.sessionID, "plugin hook", &spillCfg)
		parts = append(parts, piece)
	}
	return strings.Join(parts, "\n\n")
}

// Routes gateway notes, plugin notices, surface switch and skills index notes onto the user injection channel
func mergeGatewayNotes(a *AIAgent, messages []*state.Message, currentTurnUserIdx int, pluginUserContext string) string {
	tools.LSPForwardToAgent(a.sessionID, a.currentTaskID, a.queuePluginNotice)
	var parts []string
	for _, p := range []string{consumeGatewayTurnContextNotes(a), popPluginNotices(a), consumeSurfaceSwitchNote(a), consumeSkillsIndexNote(a)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	notes := strings.Join(parts, "\n\n")
	if notes == "" {
		return pluginUserContext
	}
	if currentTurnUserIdx >= 0 && currentTurnUserIdx < len(messages) && messages[currentTurnUserIdx] != nil {
		if _, isList := messages[currentTurnUserIdx].Content.([]any); isList {
			appendNotesToMultimodalContent(&messages[currentTurnUserIdx].Content, notes)
			return pluginUserContext
		}
	}
	if pluginUserContext != "" {
		return pluginUserContext + "\n\n" + notes
	}
	return notes
}

// Binds the turn's worker identity for tool interrupts, clearing stale state but keeping a pending interrupt
func bindInterruptScope(ctx context.Context, a *AIAgent) context.Context {
	id := tools.WorkerOf(ctx)
	if id == 0 {
		id = tools.NewWorkerID()
		ctx = tools.WithWorker(ctx, id)
	}
	a.executionThreadID = int64(id)
	tools.SetInterrupt(id, false, "")
	if a.interruptRequested {
		tools.SetInterrupt(id, true, a.toolInterruptReason)
	} else {
		a.interruptMessage = ""
		a.toolInterruptReason = ""
	}
	a.interruptThreadSignalPending = false
	return ctx
}

// Semantic text of the turn for memory queries, flattened from string or multimodal content
func memoryQueryText(originalUserMessage any) string {
	switch originalUserMessage.(type) {
	case string, []any:
		return state.FlattenMessageText(originalUserMessage, "")
	}
	return ""
}

// Notifies memory providers of the turn and prefetches external memory once unless the prompt is trivial
func memoryTurnStartAndPrefetch(ctx context.Context, a *AIAgent, originalUserMessage any, turnAuthor map[string]any) string {
	if a.memoryManager == nil {
		return ""
	}
	query := memoryQueryText(originalUserMessage)
	authorID, _ := turnAuthor["id"].(string)
	authorName, _ := turnAuthor["name"].(string)
	func() {
		defer func() { _ = recover() }()
		a.memoryManager.OnTurnStart(ctx, a.userTurnCount, query, memory.TurnOptions{AuthorID: authorID, AuthorName: authorName, AuthorIsBot: pyTruthy(turnAuthor["is_bot"])})
	}()
	prefetch := ""
	func() {
		defer func() { _ = recover() }()
		if !memory.IsTrivialPrompt(query) {
			prefetch = a.memoryManager.PrefetchAll(ctx, query, a.sessionID)
		}
	}()
	if prefetch != "" {
		func() {
			defer func() { _ = recover() }()
			if indicator := a.memoryManager.DescribeRecall(ctx); indicator != "" {
				a.emitStatus(indicator)
			}
		}()
	}
	return prefetch
}

// Stamps the exact sent bytes on the live user row as its api_content sidecar and backfills the stored row
func stampAPIContentSidecar(ctx context.Context, a *AIAgent, messages []*state.Message, currentTurnUserIdx int, extPrefetchCache, pluginUserContext string, preflightCompressed bool) {
	turnUserMsg := messages[currentTurnUserIdx]
	live := turnUserMsg.Content
	var composed *string
	if c, ok := composeUserAPIContent(pyOr(live, ""), extPrefetchCache, pluginUserContext); ok {
		composed = &c
	}
	durable, apiContent := durableUserRowContent(a, turnUserMsg, live, composed)
	if apiContent == nil || reflect.DeepEqual(any(*apiContent), durable) {
		return
	}
	turnUserMsg.APIContent = *apiContent
	defer persistLock(a)()
	rowID, hasRow := turnUserMsg.Extra["_row_id"].(int64)
	inPlace := preflightCompressed && a.lastCompactionInPlace
	if a.sessionDB == nil || !(hasRow || inPlace) {
		return
	}
	var err error
	if hasRow {
		_, err = a.sessionDB.SetMessageAPIContent(ctx, a.sessionID, rowID, durable, *apiContent)
	} else {
		_, err = a.sessionDB.SetLatestUserAPIContent(ctx, a.sessionID, durable, *apiContent)
	}
	if err != nil {
		turnContextLog().Warn(fmt.Sprintf("api_content backfill failed for session=%s", orNone(a.sessionID)), "error", err)
	}
}

// Carries the turn's context as a durable text part on a multimodal user row and backfills the stored row
func appendMultimodalContext(ctx context.Context, a *AIAgent, turnUserMsg *state.Message, extPrefetchCache, pluginUserContext string, preflightCompressed bool) {
	part, _ := composeMultimodalContextPart(extPrefetchCache, pluginUserContext)
	if !appendNotesToMultimodalContent(&turnUserMsg.Content, part) {
		return
	}
	defer persistLock(a)()
	rowID, ok := turnUserMsg.Extra["_row_id"].(int64)
	if a.sessionDB == nil || !ok {
		return
	}
	content := turnUserMsg.Content
	if !(preflightCompressed && a.lastCompactionInPlace) {
		content = durableContent(content)
	}
	if _, err := a.sessionDB.SetUserMessageContent(ctx, a.sessionID, rowID, content, nil); err != nil {
		turnContextLog().Warn(fmt.Sprintf("multimodal context backfill failed for session=%s", orNone(a.sessionID)), "error", err)
	}
}

// Persists the inbound user turn once with its final api_content before the first model call
func persistTurnStart(a *AIAgent, messages, conversationHistory []*state.Message, pendingCLIMessage map[string]any) {
	persistUnderLock(a, func() error {
		if err := a.ensureDBSession(); err != nil {
			return err
		}
		return a.persistSession(&messages, conversationHistory)
	}, "Early turn-start session persistence failed for session=%s", pendingCLIMessage)
}

// Runs the once per turn setup and returns the loop's input context
func buildTurnContext(ctx context.Context, a *AIAgent, r TurnContextRequest) (*TurnContext, error) {
	provider.InstallSafeStdio()
	turnAuthor := provider.ParseTurnAuthor(r.TurnAuthor)
	a.turnAuthor = turnAuthor
	conversationHistory := r.ConversationHistory
	if recovered, ok := recoverRotatedCompressionSession(ctx, a); ok {
		conversationHistory = recovered
	}
	setLoggingSessionContext(a.sessionID)
	skills.SetCurrentWriteOrigin(a.env, pyStrOr(a.memoryWriteOrigin, "assistant_tool"))
	skills.SetReviewAttended(a.env, a.reviewAttended)
	a.restorePrimaryRuntime(ctx)
	ctx = publishRuntimeMain(ctx, a)
	refreshMCPToolsBetweenTurns(ctx, a)
	userMessage, persistUserMessage := r.UserMessage, r.PersistUserMessage
	if s, ok := userMessage.(string); ok {
		userMessage = state.SanitizeSurrogates(s)
	}
	if s, ok := persistUserMessage.(string); ok {
		persistUserMessage = state.SanitizeSurrogates(s)
	}
	var platformID *string
	if r.PersistUserPlatformID != "" {
		platformID = &r.PersistUserPlatformID
	}
	effectiveTaskID, turnID := bindTurnIdentity(a, r.TaskID, r.StreamCallback, persistUserMessage, r.PersistUserTimestamp, platformID)
	resetPerTurnAgentState(ctx, a)
	previewText := provider.SummarizeUserMessageForLog(userMessage, " ")
	msgPreview := truncateRunes(previewText, 80)
	if len([]rune(previewText)) > 80 {
		msgPreview += "..."
	}
	line := fmt.Sprintf("conversation turn: session=%s model=%s provider=%s platform=%s history=%d msg=%s",
		orNone(a.sessionID), a.model, pyStrOr(a.provider, "unknown"), pyStrOr(a.platform, "unknown"), len(conversationHistory),
		pyrepr.Repr(strings.ReplaceAll(msgPreview, "\n", " ")))
	if a.turnOrigin != "" {
		line += " origin=" + a.turnOrigin
	}
	turnContextLog().Info(line)
	messages := make([]*state.Message, 0, len(conversationHistory)+1)
	for _, m := range conversationHistory {
		messages = append(messages, state.MessageFromDict(m))
	}
	userMsg, pendingCLI := stageTurnUserMessage(a, userMessage, persistUserMessage, r.PersistUserTimestamp, platformID, r.PersistUserDisplayKind, r.PersistUserDisplayMetadata)
	hydrateFromHistory(ctx, a, conversationHistory)
	ctx = provider.BindImageTokenCost(ctx, a.model, a.baseURL)
	turnRow := state.MessageFromDict(userMsg)
	messages = append(messages, turnRow)
	currentTurnUserIdx := len(messages) - 1
	idx := currentTurnUserIdx
	a.persistUserMessageIdx = &idx
	a.userTurnCount++
	a.isUserInitiatedTurn = true
	originalUserMessage := persistUserMessage
	if originalUserMessage == nil {
		originalUserMessage = userMessage
	}
	shouldReviewMemory := tickMemoryNudge(a)
	emitReaction(a, originalUserMessage, r.PersistUserDisplayKind)
	if !a.quietMode {
		suffix := ""
		if len([]rune(previewText)) > 60 {
			suffix = "..."
		}
		a.safePrint(fmt.Sprintf("💬 Starting conversation: '%s%s'", truncateRunes(previewText, 60), suffix), false)
	}
	if err := RefreshWarmPrompt(ctx, a, r.SystemMessage, conversationHistory); err != nil {
		return nil, err
	}
	if a.cachedSystemPrompt == "" {
		if err := restoreOrBuildSystemPrompt(ctx, a, r.SystemMessage, conversationHistory); err != nil {
			return nil, err
		}
	}
	activeSystemPrompt := a.cachedSystemPrompt
	applyRequestedToolRebuild(ctx, a)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				turnContextLog().Debug("message_agent injection skipped", "error", rec)
			}
		}()
		ensureMessageAgentTool(ctx, a)
	}()
	ensureSessionRow(a, pendingCLI)
	for _, m := range conversationHistory {
		if pyTruthy(m[state.PersistAfterAdmissionInterrupt]) {
			if _, err := a.flushMessagesToSessionDB(conversationHistory); err != nil {
				return nil, err
			}
			break
		}
	}
	var blocks []string
	pluginUserContext := collectPreLLMCallContext(ctx, a, effectiveTaskID, turnID, originalUserMessage, messages, conversationHistory, &blocks)
	pluginBlockMessage := ""
	if len(blocks) > 0 {
		pluginBlockMessage = blocks[0]
	}
	var historyOut []*state.Message
	if conversationHistory != nil {
		historyOut = slices.Clone(messages[:len(conversationHistory)])
	}
	compaction := CompactionOutcome{Messages: messages, ActiveSystemPrompt: activeSystemPrompt, ConversationHistory: historyOut, CurrentTurnUserIdx: currentTurnUserIdx}
	if pluginBlockMessage == "" {
		var err error
		compaction, err = runTurnStartCompaction(ctx, a, turnStartCompactionRequest{
			Messages: messages, SystemMessage: r.SystemMessage, ActiveSystemPrompt: activeSystemPrompt, ConversationHistory: historyOut,
			CurrentTurnUserIdx: currentTurnUserIdx, UserMessage: userMessage, EffectiveTaskID: effectiveTaskID,
		})
		if err != nil {
			return nil, err
		}
	}
	messages, activeSystemPrompt = compaction.Messages, compaction.ActiveSystemPrompt
	historyOut, currentTurnUserIdx = compaction.ConversationHistory, compaction.CurrentTurnUserIdx
	stageSkillsIndexNote(ctx, a, activeSystemPrompt, messages)
	pluginUserContext = mergeGatewayNotes(a, messages, currentTurnUserIdx, pluginUserContext)
	ctx = bindInterruptScope(ctx, a)
	bindTurnClientEnv(ctx, a)
	extPrefetchCache := ""
	if pluginBlockMessage == "" {
		extPrefetchCache = memoryTurnStartAndPrefetch(ctx, a, originalUserMessage, turnAuthorMap(turnAuthor))
		maybeTitleSessionAtTurnStart(ctx, a, messages, r.TitleUserMessage)
	}
	if currentTurnUserIdx >= 0 && currentTurnUserIdx < len(messages) && messages[currentTurnUserIdx].Role == "user" {
		row := messages[currentTurnUserIdx]
		_, isList := row.Content.([]any)
		switch {
		case pluginBlockMessage != "":
			withholdBlockedPrompt(ctx, a, row, originalUserMessage)
		case isList:
			appendMultimodalContext(ctx, a, row, extPrefetchCache, pluginUserContext, compaction.Compressed)
		case !r.MoaActive && a.apiMode != "codex_app_server":
			stampAPIContentSidecar(ctx, a, messages, currentTurnUserIdx, extPrefetchCache, pluginUserContext, compaction.Compressed)
		}
	}
	persistTurnStart(a, messages, historyOut, pendingCLI)
	return &TurnContext{
		Ctx: ctx, UserMessage: userMessage, OriginalUserMessage: originalUserMessage, Messages: messages,
		ConversationHistory: historyOut, ActiveSystemPrompt: activeSystemPrompt, EffectiveTaskID: effectiveTaskID, TurnID: turnID,
		CurrentTurnUserIdx: currentTurnUserIdx, ShouldReviewMemory: shouldReviewMemory, PluginUserContext: pluginUserContext,
		PluginBlockMessage: pluginBlockMessage, ExtPrefetchCache: extPrefetchCache, PreflightCompressionBlocked: compaction.Blocked,
	}, nil
}

// Model name for strict API tool call sanitization, the resolved aggregator in MoA mode
func sanitizeModelFor(a *AIAgent, moaConfig map[string]any) string {
	model := a.model
	if a.provider != "moa" {
		return model
	}
	if len(moaConfig) > 0 {
		if agg, ok := moaConfig["aggregator"].(map[string]any); ok {
			if m, ok := agg["model"].(string); ok && m != "" {
				model = m
			}
		}
	}
	if model == a.model {
		if slot := lastAggregatorSlot(a); slot != nil {
			if m, ok := slot["model"].(string); ok && m != "" {
				model = m
			}
		}
	}
	return model
}

// Wire dict of a message without the Go only row bookkeeping keys hermes dicts never carry
func wireDictOf(m *state.Message) map[string]any {
	d := state.MessageToDict(m)
	for _, k := range []string{"id", "active", "compacted"} {
		if _, own := m.Extra[k]; !own {
			delete(d, k)
		}
	}
	return d
}

// Builds the wire copy of messages for one API call and the effective system message
func buildAPIMessages(ctx context.Context, a *AIAgent, messages []*state.Message, currentTurnUserIdx int, extPrefetchCache, pluginUserContext string, moaConfig map[string]any, activeSystemPrompt string) ([]map[string]any, string) {
	hasCurrent := currentTurnUserIdx >= 0 && currentTurnUserIdx < len(messages)
	var current *state.Message
	split := 0
	if hasCurrent {
		current = messages[currentTurnUserIdx]
		split = currentTurnUserIdx
	}
	canonical := append(canonicalizeReplayHistory(messages[:split], a.currentTurnTimestamp), messages[split:]...)
	apiMessages := make([]map[string]any, 0, len(canonical)+1)
	for idx, msg := range canonical {
		apiMsg := cloneMessageForSend(wireDictOf(msg)).(map[string]any)
		sidecar, _ := apiMsg["api_content"].(string)
		delete(apiMsg, "api_content")
		for key := range state.PersistenceOnlyMessageFields {
			delete(apiMsg, key)
		}
		if msg == current && msg.Role == "user" {
			if sidecar != "" {
				apiMsg["content"] = sidecar
			} else if composed, ok := composeUserAPIContent(dictGetOr(apiMsg, "content", ""), extPrefetchCache, pluginUserContext); ok {
				apiMsg["content"] = composed
			}
		} else if sidecar != "" && (msg.Role == "user" || msg.Role == "assistant") {
			apiMsg["content"] = sidecar
		}
		a.copyReasoningContentForAPI(msg, apiMsg)
		delete(apiMsg, "finish_reason")
		fillEmptyNonFinalWirePayload(apiMsg, idx == len(canonical)-1)
		delete(apiMsg, "_length_continuation_fragment")
		delete(apiMsg, "_length_continuation_nudge")
		if a.shouldSanitizeToolCalls() {
			sanitizeToolCallsForStrictAPI(apiMsg, sanitizeModelFor(a, moaConfig), a.baseURL, a.provider)
		}
		apiMessages = append(apiMessages, apiMsg)
	}
	replayForAgent(ctx, a, apiMessages)
	applyRejectedThinkingSuppression(a, apiMessages)
	effective := activeSystemPrompt
	if a.ephemeralSystemPrompt != "" {
		effective = strings.TrimSpace(effective + "\n\n" + a.ephemeralSystemPrompt)
	}
	if effective != "" {
		apiMessages = append([]map[string]any{{"role": "system", "content": effective}}, apiMessages...)
	}
	return apiMessages, effective
}

// Request inputs of the turn prologue, nil and empty values standing for None
type TurnContextRequest struct {
	UserMessage                any
	SystemMessage              *string
	ConversationHistory        []map[string]any
	TaskID                     string
	StreamCallback             func(string)
	PersistUserMessage         any
	PersistUserTimestamp       *float64
	PersistUserDisplayKind     string
	PersistUserDisplayMetadata map[string]any
	PersistUserPlatformID      string
	TurnAuthor                 map[string]any
	MoaActive                  bool
	TitleUserMessage           *string
}

// Locals the loop reads back from the prologue, Ctx carrying the turn scoped runtime bindings
type TurnContext struct {
	Ctx                         context.Context
	UserMessage                 any
	OriginalUserMessage         any
	ConversationHistory         []*state.Message
	EffectiveTaskID             string
	TurnID                      string
	ShouldReviewMemory          bool
	PluginUserContext           string
	ExtPrefetchCache            string
	Messages                    []*state.Message
	ActiveSystemPrompt          string
	CurrentTurnUserIdx          int
	PreflightCompressionBlocked bool
	PluginBlockMessage          string
}

// Tool schema dicts of the agent tool list
func toolDefsOf(list []any) []map[string]any {
	var out []map[string]any
	for _, t := range list {
		if d, ok := t.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

// Plain dict of the parsed turn author, empty when there is none
func turnAuthorMap(author *pyjson.Object) map[string]any {
	if author == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(author.Vals))
	for k, v := range author.Vals {
		out[k] = v
	}
	return out
}

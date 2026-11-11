package gatewayrun

import (
	"context"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scripted onboarding opening handed to run_conversation one step at a time
type stepsPrelude struct {
	steps []tools.OnboardingPreludeStep
	next  int
}

func (p *stepsPrelude) Next(result *string) (agent.PreludeStep, bool) {
	if p.next >= len(p.steps) {
		return agent.PreludeStep{}, false
	}
	step := p.steps[p.next]
	p.next++
	return agent.PreludeStep{Content: step.Text, ToolName: step.Tool, Args: step.Args}, true
}

func (p *stepsPrelude) Close() {}

// Appends the onboarding turn note and returns the scripted opening for a chat built with setup_beat
func armOnboardingTurn(a *agent.AIAgent, owner *turnApprovalOwner, history []map[string]any) []tools.OnboardingPreludeStep {
	if owner == nil || !tools.BuildsOnboarding(a) {
		return nil
	}
	cardWaiting := false
	if live, ok := owner.authority.Live(owner.sessionID); ok && live != nil {
		cardWaiting = gateway.ServerRequestWaiting(live.EventStream, owner.sessionID, "setup_beat")
	}
	tools.AppendTurnNote(a, tools.OnboardingSessionTurnNote(a, len(history) == 0, cardWaiting))
	return tools.OnboardingPrelude(a, history)
}

// Receipt binding the streamed reply to its stored row, nil for a result that is not a dict
func persistedTurn(a *agent.AIAgent, agentHistory []map[string]any, result map[string]any, compressions *int) map[string]any {
	if result == nil {
		return nil
	}
	status := "complete"
	switch {
	case pyrepr.Truthy(result["interrupted"]):
		status = "interrupted"
	case pyrepr.Truthy(result["failed"]) || pyrepr.Truthy(result["error"]):
		status = "error"
	}
	unchanged := false
	if compressions != nil && a.ContextCompressor() != nil {
		unchanged = *compressions == a.ContextCompressor().Base().CompressionCount
	}
	return agent.PersistedTurnReceipt(result["messages"], a.PersistUserMessageIdx(), agentHistory, result["final_response"], status,
		unchanged, func(before, after any) bool { return sameTranscriptRow(before, after) })
}

// Whether two transcript rows are the same dict object
func sameTranscriptRow(before, after any) bool {
	b, okB := before.(map[string]any)
	c, okC := after.(map[string]any)
	if !okB || !okC || b == nil || c == nil {
		return false
	}
	return reflect.ValueOf(b).UnsafePointer() == reflect.ValueOf(c).UnsafePointer()
}

// Compression count of the agent's compressor, nil without one
func agentCompressionCount(a *agent.AIAgent) *int {
	if a == nil || a.ContextCompressor() == nil {
		return nil
	}
	count := a.ContextCompressor().Base().CompressionCount
	return &count
}

// Rows of a history value carried by an API admission
func historyRows(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return typed
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			switch row := item.(type) {
			case map[string]any:
				out = append(out, row)
			case *pyjson.Object:
				plain, _ := pyjson.Plain(row).(map[string]any)
				out = append(out, plain)
			}
		}
		return out
	}
	return nil
}

func rowsAsAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Replay history, observed group context and media paths already in it, preserving a live cached transcript that outran disk
func (t *TurnRunner) loadTurnHistory(ctx context.Context, a *agent.AIAgent, reusedCachedAgent bool) ([]map[string]any, *string, map[string]struct{}, error) {
	tc := t.ctx
	if api := gateway.APIExecution(ctx); api != nil && api.GetOr("history", nil) != nil {
		history := historyRows(api.GetOr("history", nil))
		var tail map[string]any
		if len(history) > 0 {
			tail = history[len(history)-1]
		}
		if pyrepr.Truthy(api.GetOr("resume_unanswered", nil)) && tail != nil && tail["role"] == "user" {
			history = append([]map[string]any(nil), history...)
			agent.AdoptUnansweredTurn(&history, api.GetOr("content", nil), a)
		}
		media, err := gateway.CollectHistoryMediaPaths(ctx, rowsAsAny(history))
		return history, nil, media, err
	}
	agentHistory, observed, err := buildGatewayAgentHistory(ctx, tc.history, tc.channelPrompt, messageTimestampsEnabled(tc.userConfig))
	if err != nil {
		return nil, nil, nil, err
	}
	if reusedCachedAgent && a.SessionID() == tc.sessionID {
		live := agent.SessionMessageDicts(a.SessionMessages())
		selected, chosen := selectCachedAgentHistory(agentHistory, live)
		r := t.runner
		r.transcriptLagMu.Lock()
		if r.transcriptLagStreaks == nil {
			r.transcriptLagStreaks = map[string]int{}
		}
		if chosen {
			r.transcriptLagStreaks[tc.sessionKey]++
			streak := r.transcriptLagStreaks[tc.sessionKey]
			r.transcriptLagMu.Unlock()
			suffix := ""
			if streak >= transcriptLagEscalationTurns {
				suffix = "; state.db is not receiving this session's writes and needs operator attention"
			}
			msg := "Persisted transcript lagged live cached history for session " + tc.sessionKey + " (disk=" +
				pyrepr.Str(len(agentHistory)) + ", memory=" + pyrepr.Str(len(selected)) + ", consecutive_turns=" +
				pyrepr.Str(streak) + "); preserving live conversation context (possible FTS write corruption)" + suffix
			if streak >= transcriptLagEscalationTurns {
				runLog().Error(msg)
			} else {
				runLog().Warn(msg)
			}
			agentHistory = agent.CanonicalizeReplayHistory(selected, nil)
		} else {
			delete(r.transcriptLagStreaks, tc.sessionKey)
			r.transcriptLagMu.Unlock()
		}
	}
	media, err := gateway.CollectHistoryMediaPaths(ctx, rowsAsAny(agentHistory))
	return agentHistory, observed, media, err
}

// Prepends recovery and notice guidance to the turn message, returning the persisted user text and timestamp overrides
func (t *TurnRunner) prepareTurnMessage(ctx context.Context, agentHistory []map[string]any) (any, *float64) {
	tc := t.ctx
	r := t.runner
	persistOverride := tc.persistUserMessage
	var inbound any
	if tc.inboundMessageID != nil {
		inbound = *tc.inboundMessageID
	}
	apiText, hasAPIText := gateway.CLITurn(ctx, inbound).Get("api_text")
	hasAPIText = hasAPIText && apiText != nil
	var cleanText any
	if hasAPIText {
		cleanText = persistOverride
		if cleanText == nil {
			cleanText = tc.message
		}
		persistOverride = cleanText
		tc.message = pyrepr.Str(apiText)
	}
	t.prependPendingNote(r.pendingModelNotes)
	window := gateway.AutoContinueFreshnessWindow()
	interruptionIsFresh := isFreshGatewayInterruption(lastTranscriptTimestamp(tc.history), nil, window)
	var entry *gateway.SessionEntry
	if tc.sessionKey != "" && r.sessionStore != nil {
		entry = r.sessionStore.LoadedEntry(ctx, tc.sessionKey)
	}
	resumePending := entry != nil && entry.ResumePending
	var resumeReason *string
	if resumePending {
		reason := "restart_timeout"
		if entry.ResumeReason != nil && *entry.ResumeReason != "" {
			reason = *entry.ResumeReason
		}
		resumeReason = &reason
	}
	markIsFresh := false
	if resumePending {
		var marked any
		if stamp, ok := entry.LastResumeMarkedTimestamp(); ok {
			marked = stamp
		}
		markIsFresh = isFreshGatewayInterruption(marked, nil, window)
	}
	switch {
	case resumePending && (interruptionIsFresh || markIsFresh):
		message, override := prepareResumePendingMessage(resumeReason, tc.message, t.resumeNoteInteractive(ctx))
		tc.message, persistOverride = message, override
	case len(agentHistory) > 0 && agentHistory[len(agentHistory)-1]["role"] == "tool" && interruptionIsFresh:
		persistOverride = tc.message
		tc.message = "[System note: A new message has arrived. The conversation " +
			"history contains pending tool outputs from an interrupted turn. " +
			"IGNORE those pending results. Address the user's NEW message " +
			"below FIRST. Do NOT re-execute old tool calls from the history.]\n\n" + tc.message
	}
	t.prependPendingNote(r.pendingSkillsReloadNotes)
	if hasAPIText {
		persistOverride = cleanText
	}
	if strings.TrimSpace(tc.message) == "" && resumePending {
		tc.message = buildResumeRecoveryNote(resumeReason, "", t.resumeNoteInteractive(ctx))
	}
	return persistOverride, tc.persistUserTimestamp
}

// Runs the conversation with the session approval notify registered and pending clarify prompts cleared afterwards
func (t *TurnRunner) runConversationWithApproval(ctx context.Context, a *agent.AIAgent, agentHistory []map[string]any, observed *string, persistOverride any, persistTimestamp *float64) (map[string]any, error) {
	tc := t.ctx
	sessionKey := tc.sessionKey
	env := t.toolsEnv()
	tools.SetCurrentSessionKey(env, sessionKey)
	tools.RegisterGatewayNotify(sessionKey, t.approvalNotifySync)
	defer func() {
		tools.UnregisterGatewayNotify(sessionKey)
		tools.ClearClarifySession(sessionKey)
		tools.ResetCurrentSessionKey(env)
	}()
	apiMessage := wrapCurrentMessageWithObservedContext(t.nativeImageRunMessage(ctx), observed)
	opts := agent.RunConversationOptions{ConversationHistory: agentHistory, TaskID: tc.sessionID}
	api := gateway.APIExecution(ctx)
	if api != nil {
		if relay, ok := api.GetOr("relay_metadata", nil).(map[string]any); ok && len(relay) > 0 {
			opts.RelayMetadata = relay
		} else if relayObj, ok := api.GetOr("relay_metadata", nil).(*pyjson.Object); ok && relayObj.Len() > 0 {
			opts.RelayMetadata, _ = pyjson.Plain(relayObj).(map[string]any)
		}
		opts.TurnAuthor = authorMap(api.GetOr("turn_author", nil))
	} else if author := gateway.AdmissionAuthorOf(ctx); pyrepr.Truthy(author) {
		opts.TurnAuthor = authorMap(author)
	} else {
		var id, name any
		if tc.source.UserID != nil && *tc.source.UserID != "" {
			id = *tc.source.UserID
		}
		if tc.source.UserName != nil && *tc.source.UserName != "" {
			name = *tc.source.UserName
		}
		opts.TurnAuthor = map[string]any{"id": id, "name": name, "is_bot": tc.source.IsBot}
	}
	opts.TitleUserMessage = tc.titleUserMessage
	if persistOverride != nil {
		opts.PersistUserMessage = persistOverride
	} else if observed != nil && *observed != "" {
		opts.PersistUserMessage = tc.message
	}
	if tc.persistUserDisplayKind != nil && *tc.persistUserDisplayKind != "" {
		opts.PersistUserDisplayKind = *tc.persistUserDisplayKind
	}
	if len(tc.persistUserDisplayMetadata) > 0 {
		opts.PersistUserDisplayMetadata = tc.persistUserDisplayMetadata
	}
	if tc.moaConfig != nil {
		opts.MoaConfig = tc.moaConfig
	}
	if steps := armOnboardingTurn(a, t.approvalOwner, agentHistory); steps != nil {
		opts.Prelude = &stepsPrelude{steps: steps}
	}
	if persistTimestamp != nil {
		opts.PersistUserTimestamp = persistTimestamp
	}
	if tc.inboundMessageID != nil {
		opts.PersistUserPlatformID = *tc.inboundMessageID
	}
	captured := gateway.ExecutionResultOf(ctx)
	beforePrompt, beforeCompletion := a.SessionPromptTokens(), a.SessionCompletionTokens()
	turnCtx, restore := a.NotificationTurn(ctx, tc.muteNotificationReply, tc.sessionID)
	var displayKind any
	if tc.persistUserDisplayKind != nil {
		displayKind = *tc.persistUserDisplayKind
	}
	a.SetSilenceMarkerNeedsReply(!gateway.SilenceAllowed(displayKind, tc.replyExpected, nil, nil))
	result, err := func() (map[string]any, error) {
		defer restore()
		defer a.SetSilenceMarkerNeedsReply(false)
		return a.RunConversationContent(turnCtx, apiMessage, opts)
	}()
	if err != nil {
		return nil, err
	}
	if captured != nil {
		incoming := max(0, a.SessionPromptTokens()-beforePrompt)
		outgoing := max(0, a.SessionCompletionTokens()-beforeCompletion)
		usage := map[string]any{"input_tokens": incoming, "output_tokens": outgoing, "total_tokens": incoming + outgoing}
		captured.Set("usage", usage)
		if err := settleAPIRuntimeLock(ctx, a, result, map[string]any{"usage": usage}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Author dict of an admission or API turn
func authorMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case *pyjson.Object:
		plain, _ := pyjson.Plain(typed).(map[string]any)
		return plain
	}
	return nil
}

// Syncs the session binding after compression rotated or compacted it, returning the history offset to persist from
func (t *TurnRunner) syncSessionAfterRun(ctx context.Context, agentHistory []map[string]any) (bool, string, int, error) {
	tc := t.ctx
	r := t.runner
	a := tc.currentAgent()
	compactedInPlace := a != nil && a.LastCompactionInPlace()
	agentSessionID := tc.sessionID
	if a != nil {
		agentSessionID = a.SessionID()
	}
	sessionWasSplit := a != nil && tc.sessionKey != "" && agentSessionID != tc.sessionID
	if sessionWasSplit {
		runLog().Info("Session split detected: " + tc.sessionID + " → " + agentSessionID + " (compression)")
		entry := r.sessionStore.LoadedEntry(ctx, tc.sessionKey)
		persisted := false
		if entry != nil {
			keyLabel := tc.sessionKey
			if keyLabel == "" {
				keyLabel = "?"
			}
			switch {
			case !tc.runStillCurrent():
				generation := "None"
				if tc.runGeneration != nil {
					generation = pyrepr.Str(*tc.runGeneration)
				}
				runLog().Info("Skipping session split sync for stale run " + keyLabel + " — generation " + generation + " is no longer current")
			case entry.SessionID == agentSessionID:
				persisted = true
			case entry.SessionID != tc.sessionID:
				runLog().Info("Skipping session split sync for " + keyLabel + " because the session binding moved from " + tc.sessionID + " to " + entry.SessionID + " before compression finished")
			default:
				entry.SessionID = agentSessionID
				if err := r.sessionStore.Save(ctx); err != nil {
					return false, "", 0, err
				}
				if err := r.sessionStore.RecordGatewaySessionPeer(ctx, agentSessionID, tc.sessionKey, tc.source); err != nil {
					return false, "", 0, err
				}
				persisted = true
			}
		}
		if persisted {
			src := tc.source
			if src.Platform == gateway.PlatformTelegram && src.ChatType == "dm" && src.ThreadID == nil && r.sessionDB(ctx) != nil {
				t.restoreTelegramThreadIDAfterSplit(ctx, agentSessionID)
			}
			r.syncTelegramTopicBinding(ctx, src, entry, "agent-run-compression")
		}
	}
	var inbound any
	if tc.inboundMessageID != nil {
		inbound = *tc.inboundMessageID
	}
	if !pyrepr.Truthy(gateway.CLITurn(ctx, inbound).GetOr("model_override", nil)) {
		r.syncSessionModelFromAgent(ctx, agentSessionID, a)
	}
	offset := len(agentHistory)
	if sessionWasSplit || compactedInPlace {
		offset = 0
	}
	return compactedInPlace, agentSessionID, offset, nil
}

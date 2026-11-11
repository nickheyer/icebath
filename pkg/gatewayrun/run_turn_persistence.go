package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reasoning header catalog key and quote prefixes for blank and non blank lines, per reasoning style
var reasoningQuoteStyles = map[string][3]string{
	"subtext":    {"gateway.reasoning.quote_label_discord", "-# ", "-#"},
	"blockquote": {"gateway.reasoning.quote_label_md", "> ", ">"},
}

// Chat next step catalog keys by HTTP status, the 401 key carrying the relogin command
var statusHints = map[int]string{401: "gateway.errors.hint_auth", 402: "gateway.errors.hint_quota", 529: "gateway.errors.hint_overloaded"}

// Whether the source is an editor launch, a local route with no restored policy counting as none
func acpLaunch(ctx context.Context, r *GatewayRunner, source *gateway.SessionSource) (bool, error) {
	policy, err := gateway.LocalPolicy(ctx, r, source)
	var storeErr *state.RuntimeStoreError
	if errors.As(err, &storeErr) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return policy != nil && policy.Source == "acp", nil
}

// Whether the agent result is an intentional silence, false when the check itself fails
func isIntentionalSilence(agentResult map[string]any, response string) (silent bool) {
	defer func() {
		if recover() != nil {
			silent = false
		}
	}()
	return gateway.IsIntentionalSilenceAgentResult(agentResult, response)
}

// Value of a result key, or the fallback when the key is absent
func resultGet(agentResult map[string]any, key string, fallback any) any {
	if v, ok := agentResult[key]; ok {
		return v
	}
	return fallback
}

// Optional bool of a result value, nil unless it is a bool
func resultOptBool(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

// Binds the room silence receipt to this exact persisted final and active turn token
func (r *GatewayRunner) recordRoomSilenceDecision(ctx context.Context, sessionKey string, agentResult map[string]any, silent bool, displayKind any, replyExpected *bool) {
	kind := resultGet(agentResult, "queued_terminal_display_kind", displayKind)
	expected := replyExpected
	if v, ok := agentResult["queued_terminal_reply_expected"]; ok {
		expected = resultOptBool(v)
	}
	if !silent || sessionKey == "" || gateway.SilenceAllowed(kind, expected, nil, nil) || !gateway.IsIntentionalSilenceResponse(agentResult["final_response"]) {
		return
	}
	store := r.AsyncSessionStore().Store()
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
		}()
		entry := store.LookupBySessionKey(ctx, sessionKey)
		if entry == nil || entry.ActiveTurnToken == nil {
			return nil
		}
		loaded, err := store.LoadTranscript(ctx, entry.SessionID)
		if err != nil {
			return err
		}
		history := transcriptRows(loaded)
		last := map[string]any{}
		for i := len(history) - 1; i >= 0; i-- {
			if role := history[i]["role"]; role != "session_meta" && role != "system" {
				last = history[i]
				break
			}
		}
		if last["role"] != "assistant" || last["timestamp"] == nil || !pyrepr.Equal(last["content"], agentResult["final_response"]) {
			return nil
		}
		_, err = store.SetSessionMetadata(ctx, sessionKey, "room_silence_decision", map[string]any{
			"token": *entry.ActiveTurnToken, "timestamp": last["timestamp"], "silent": silent,
		})
		return err
	}()
	if err != nil {
		runLog().Warn(fmt.Sprintf("Could not record the room silence decision for %s", sessionKey), "error", pyrepr.ExceptionMessage(err))
	}
}

// Outbound text of a raw agent result with silence handling, normalization and identity guarded session id sync
func (r *GatewayRunner) hmwaShapeAgentResponse(ctx context.Context, agentResult map[string]any, source *gateway.SessionSource, history []map[string]any, entry *gateway.SessionEntry, sessionKey, quickKey string, runGeneration int,
	runStartSessionID, platformName string, msgStartTime time.Time, persistUserDisplayKind any, replyExpected *bool) (string, bool, []map[string]any, error) {
	response := ""
	if v := agentResult["final_response"]; pyrepr.Truthy(v) {
		response = pyrepr.Str(v)
	}
	if isGatewayHiddenReasoningIncompleteTurn(agentResult) {
		response = ""
	}
	silent := isIntentionalSilence(agentResult, response)
	silenceKind := resultGet(agentResult, "queued_terminal_display_kind", persistUserDisplayKind)
	silenceReplyExpected := replyExpected
	if v, ok := agentResult["queued_terminal_reply_expected"]; ok {
		silenceReplyExpected = resultOptBool(v)
	}
	silenceSource := source
	if v, ok := agentResult["queued_terminal_source"].(*gateway.SessionSource); ok {
		silenceSource = v
	}
	chatOrUnknown := source.ChatID
	if chatOrUnknown == "" {
		chatOrUnknown = "unknown"
	}
	if silent && !r.silenceAllowedForSource(ctx, silenceSource, silenceKind, silenceReplyExpected, agentResult) {
		runLog().Warn(fmt.Sprintf("silence marker rejected on a user turn: platform=%s chat=%s", platformName, chatOrUnknown))
		silent = false
		response = unexpectedSilenceReply(ctx)
	} else if silent && !gateway.IsMachineryDisplayKind(silenceKind) {
		runLog().Debug(fmt.Sprintf("silence marker suppressed on an unaddressed or opted-in turn: platform=%s chat=%s", platformName, chatOrUnknown))
	}
	if response == "(empty)" && !silent {
		model := pystr.Strip(noneText(agentResult["model"]))
		if model == "" {
			model = i18n.T(ctx, "gateway.errors.empty_response_model_label", "", nil)
		}
		explanation := strings.ReplaceAll(agent.EmptyResponseExplanation, "{model}", model)
		response = i18n.T(ctx, "gateway.shared.warn_passthrough", "", map[string]any{"error": explanation})
	}
	agentMessages := resultRows(resultGet(agentResult, "messages", []any{}))
	sessionOrUnknown := sessionKey
	if sessionOrUnknown == "" {
		sessionOrUnknown = "unknown"
	}
	apiCalls := 0
	if n, err := pyint.Int64(resultGet(agentResult, "api_calls", 0)); err == nil {
		apiCalls = int(n)
	}
	runLog().Info(fmt.Sprintf("response ready: platform=%s chat=%s session=%s time=%.1fs api_calls=%d response=%d chars",
		platformName, chatOrUnknown, sessionOrUnknown, time.Since(msgStartTime).Seconds(), apiCalls, len([]rune(response))))
	if sessionKey != "" && shouldClearResumePendingAfterTurn(agentResult) {
		r.clearRestartFailureCount(ctx, sessionKey)
		if _, err := r.AsyncSessionStore().Store().ClearResumePending(ctx, sessionKey); err != nil {
			runLog().Debug(fmt.Sprintf("clear_resume_pending failed for %s: %s", sessionKey, pyrepr.ExceptionMessage(err)))
		}
	}
	if !silent {
		response = noneText(gateway.StripTrailingSilenceMarker(response))
		response = normalizeEmptyAgentResponse(ctx, agentResult, response, len(history))
		response = sanitizeGatewayFinalResponse(ctx, source.Platform, response)
	}
	if sid := noneText(agentResult["session_id"]); sid != "" && sid != entry.SessionID {
		if entry.SessionID == runStartSessionID {
			entry.SessionID = sid
			r.rebindTurnLease(quickKey, runGeneration, entry.SessionID)
			store := r.AsyncSessionStore().Store()
			if err := store.Save(ctx); err != nil {
				return "", false, nil, err
			}
			if err := store.RecordGatewaySessionPeer(ctx, entry.SessionID, sessionKey, source); err != nil {
				return "", false, nil, err
			}
			r.syncTelegramTopicBinding(ctx, source, entry, "agent-result-compression")
		} else {
			keyOrQ := sessionKey
			if keyOrQ == "" {
				keyOrQ = "?"
			}
			runLog().Info(fmt.Sprintf("Skipping agent-result session split sync for %s because the session binding moved from %s to %s before compression finished",
				keyOrQ, runStartSessionID, entry.SessionID))
		}
	}
	return response, silent, agentMessages, nil
}

// Dict rows of a result message list
func resultRows(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		return transcriptRows(rows)
	}
	return nil
}

// Response with the last reasoning block prepended when show reasoning is on for the platform
func (r *GatewayRunner) hmwaPrependReasoning(ctx context.Context, agentResult map[string]any, response string, source *gateway.SessionSource, silent bool) (string, error) {
	if source.Platform == gateway.PlatformLocal {
		acp, err := acpLaunch(ctx, r, source)
		if err != nil {
			return "", err
		}
		if !acp {
			return response, nil
		}
	}
	show := func() (show bool) {
		defer func() {
			if recover() != nil {
				show = source.Platform != gateway.PlatformMattermost && r.showReasoning
			}
		}()
		return resolveGatewayDisplayBool(ctx, gateway.LoadGatewayConfigMap(ctx, ""), platformConfigKey(source.Platform), "show_reasoning", r.showReasoning, source.Platform, []gateway.Platform{gateway.PlatformMattermost})
	}()
	lastReasoning := noneText(agentResult["last_reasoning"])
	if !(show && response != "" && !silent && lastReasoning != "") {
		return response, nil
	}
	lines := strings.Split(strings.ReplaceAll(pystr.Strip(lastReasoning), "\r\n", "\n"), "\n")
	display := pystr.Strip(lastReasoning)
	if len(lines) > 15 {
		display = strings.Join(lines[:15], "\n") + i18n.T(ctx, "gateway.reasoning.more_lines", "", map[string]any{"count": len(lines) - 15})
	}
	style := func() (style string) {
		defer func() {
			if recover() != nil {
				style = "code"
			}
		}()
		return pyrepr.Str(config.ResolveDisplaySetting(ctx, gateway.LoadGatewayConfigMap(ctx, ""), platformConfigKey(source.Platform), "reasoning_style", "code"))
	}()
	if quote, ok := reasoningQuoteStyles[style]; ok {
		var quoted []string
		for _, ln := range strings.Split(display, "\n") {
			if ln != "" {
				quoted = append(quoted, quote[1]+ln)
			} else {
				quoted = append(quoted, quote[2])
			}
		}
		return i18n.T(ctx, quote[0], "", nil) + "\n" + strings.Join(quoted, "\n") + "\n\n" + response, nil
	}
	return i18n.T(ctx, "gateway.reasoning.block", "", map[string]any{"reasoning": gateway.EscapeCodeFencesForDisplay(display), "response": response}), nil
}

// Runtime metadata footer for the final message, empty when off, failing, or on a finite turn
func (r *GatewayRunner) hmwaRuntimeFooterLine(ctx context.Context, agentResult map[string]any, source *gateway.SessionSource, turnSeconds float64) string {
	if required, ok := gateway.FiniteTurnRequired(ctx).(bool); ok && required {
		return ""
	}
	line, err := func() (line string, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
		}()
		contextTokens := 0
		if n, err := pyint.Int64(agentResult["last_prompt_tokens"]); err == nil {
			contextTokens = int(n)
		}
		contextLength := 0
		if n, err := pyint.Int64(agentResult["context_length"]); err == nil {
			contextLength = int(n)
		}
		cwd, err := terminalScopeCwd(ctx, "")
		if err != nil {
			return "", err
		}
		return gateway.BuildFooterLine(ctx, gateway.LoadGatewayConfigMap(ctx, ""), platformConfigKey(source.Platform), gateway.FooterInputs{
			Model: noneText(agentResult["model"]), ContextTokens: contextTokens, ContextLength: contextLength, Cwd: cwd,
			TurnSeconds: &turnSeconds, RequestedModel: noneText(agentResult["requested_model"]), ServedModel: noneText(agentResult["served_model"]),
		})
	}()
	if err != nil {
		runLog().Debug("runtime_footer build failed: " + pyrepr.ExceptionMessage(err))
		return ""
	}
	return line
}

// Fires the agent end hook, schedules pending process watchers and drains watch notifications
func (r *GatewayRunner) hmwaPostTurnHooks(ctx context.Context, hookCtx map[string]any, agentResult map[string]any, response string) error {
	payload := maps.Clone(hookCtx)
	runes := []rune(response)
	payload["response"] = string(runes[:min(len(runes), 500)])
	payload["model"] = resultGet(agentResult, "model", "")
	payload["provider"] = resultGet(agentResult, "provider", "")
	if err := r.hooks.emit(ctx, "agent:end", payload); err != nil {
		return err
	}
	registry := tools.DefaultProcessRegistry()
	watchers := registry.TakePendingWatchers()
	for _, watcher := range watchers {
		spawnGatewayTask(context.WithoutCancel(ctx), "process-watcher", func(taskCtx context.Context) (any, error) {
			return nil, r.runProcessWatcher(taskCtx, watcher)
		})
	}
	if err := r.drainWatchNotifications(ctx, registry.CompletionQueue); err != nil {
		runLog().Debug("Watch queue drain error: " + pyrepr.ExceptionMessage(err))
	}
	return nil
}

// Failed turn notice appended to the provider specific guidance, or the notice alone
func hmwaAddFailedTurnNotice(response, notice string) string {
	response = pystr.Strip(response)
	if response != "" {
		return response + "\n\n" + notice
	}
	return notice
}

// Retry guidance that never assumes completed tool effects can be safely repeated
func hmwaFailedTurnNotice(agentResult map[string]any) string {
	messages, _ := resultGet(agentResult, "messages", []any{}).([]any)
	if rows, ok := agentResult["messages"].([]map[string]any); ok {
		messages = make([]any, len(rows))
		for i, row := range rows {
			messages[i] = row
		}
	}
	offset := 0
	if n, err := pyint.Int64(resultGet(agentResult, "history_offset", 0)); err == nil {
		offset = int(n)
	}
	turn, _ := gateway.CurrentTurnMessages(messages, offset)
	for _, raw := range turn {
		message, _ := raw.(map[string]any)
		if message["role"] == "tool" || (message["role"] == "assistant" && pyrepr.Truthy(message["tool_calls"])) {
			return agent.PartialFailedTurnNotice
		}
	}
	return agent.FailedTurnNotice
}

// Appends the gateway owned assistant boundary when the durable transcript tail is still open
func (r *GatewayRunner) hmwaCloseFailedTurn(ctx context.Context, sessionID, notice string) error {
	store := r.AsyncSessionStore().Store()
	if !agent.OpenTurnTailRoles[store.TranscriptTailRole(ctx, sessionID)] {
		return nil
	}
	return store.AppendToTranscript(ctx, sessionID, map[string]any{
		"role": "assistant", "content": notice, "timestamp": float64(time.Now().UnixNano()) / 1e9, "display_kind": agent.FailedTurnDisplayKind,
	}, false)
}

// Failure classification for transcript persistence: failed early, hidden reasoning incomplete, context overflow
func hmwaClassifyTurnFailure(agentResult map[string]any, history []map[string]any, entry *gateway.SessionEntry) (bool, bool, bool) {
	failedEarly := pyrepr.Truthy(agentResult["failed"])
	hiddenIncomplete := isGatewayHiddenReasoningIncompleteTurn(agentResult)
	overflow := isContextOverflowFailureResult(agentResult, len(history))
	switch {
	case overflow:
		runLog().Info(fmt.Sprintf("Skipping transcript persistence for context-overflow failure in session %s to prevent session growth loop.", entry.SessionID))
	case failedEarly:
		runLog().Info(fmt.Sprintf("Transient agent failure in session %s — persisting user message so conversation context is preserved on retry.", entry.SessionID))
	case hiddenIncomplete:
		runLog().Warn(fmt.Sprintf("Suppressing hidden-reasoning-only incomplete gateway turn for session %s: %s", entry.SessionID, pyrepr.Str(resultGet(agentResult, "error", "processing incomplete"))))
	}
	return failedEarly, hiddenIncomplete, overflow
}

// Auto reset of a permanently oversized session, never on a lock contended defer or an API or detached route
func (r *GatewayRunner) hmwaCompressionExhaustionReset(ctx context.Context, agentResult map[string]any, response string, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource) (string, *gateway.SessionEntry, error) {
	sidOrQ := "?"
	if entry != nil {
		sidOrQ = entry.SessionID
	}
	switch {
	case pyrepr.Truthy(agentResult["compression_deferred"]):
		runLog().Info(fmt.Sprintf("Compression deferred for session %s — the compression lock is held by a concurrent compressor. Keeping the session intact; the next message retries normally.", sidOrQ))
	case pyrepr.Truthy(agentResult["compression_exhausted"]) && (source != nil && (source.Platform == gateway.PlatformAPIServer || gateway.DetachedRouteOf(source) != nil)):
		runLog().Info(fmt.Sprintf("Keeping session %s after compression exhaustion.", sidOrQ))
		guidance := ""
		if gateway.DetachedRouteOf(source) != nil {
			guidance = contextOverflowReply(ctx)
		}
		if !strings.Contains(response, guidance) {
			if response != "" {
				response = response + "\n\n" + guidance
			} else {
				response = guidance
			}
		}
	case pyrepr.Truthy(agentResult["compression_exhausted"]) && entry != nil && sessionKey != "":
		deferred, err := r.hmwaDeferExhaustionReset(ctx, entry, sessionKey, source)
		if err != nil {
			return "", nil, err
		}
		if !deferred {
			if entry, err = r.hmwaRotateExhaustedSession(ctx, entry, sessionKey, source); err != nil {
				return "", nil, err
			}
		}
		response += i18n.T(ctx, "gateway.session.auto_reset_context_exhausted", "", nil)
	}
	return response, entry, nil
}

// Hands a local session's exhaustion reset to the authority drain, true when deferred
func (r *GatewayRunner) hmwaDeferExhaustionReset(ctx context.Context, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource) (bool, error) {
	if source == nil || source.Platform != gateway.PlatformLocal || !gateway.ExecutingAdmission(ctx) {
		return false, nil
	}
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil || authority == nil {
		return false, err
	}
	authority.DeferReset(sessionKey, func(resetCtx context.Context) error {
		_, err := r.hmwaRotateExhaustedSession(resetCtx, entry, sessionKey, source)
		return err
	})
	return true, nil
}

// Compression exhaustion auto reset, returning the route's fresh entry
func (r *GatewayRunner) hmwaRotateExhaustedSession(ctx context.Context, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource) (*gateway.SessionEntry, error) {
	runLog().Info(fmt.Sprintf("Auto-resetting session %s after compression exhaustion.", entry.SessionID))
	newEntry, releaseFresh, err := gateway.ResetAndRehomeMessages(ctx, r, sessionKey, entry.SessionID)
	if err != nil {
		return nil, err
	}
	defer releaseFresh()
	if err := gateway.MoveRouteWakes(ctx, r, sessionKey, entry.SessionID); err != nil {
		return nil, err
	}
	r.evictCachedAgent(ctx, sessionKey, false, false)
	var queued []*gateway.MessageEvent
	if q := r.overflowQueue(sessionKey); q != nil {
		queued = append(queued, (*q)...)
	}
	r.clearConversationScope(ctx, sessionKey, "compression_exhausted_reset")
	if len(queued) > 0 {
		st := r.sessionState(sessionKey)
		st.conversation.queuedEvents = append(st.conversation.queuedEvents, queued...)
	}
	if newEntry != nil {
		entry = newEntry
		r.syncTelegramTopicBinding(ctx, source, entry, "compression-exhausted-reset")
	}
	return entry, nil
}

// Transcript row of the inbound user turn with its clean text, event time and owner metadata
func hmwaUserTranscriptEntry(ctx context.Context, event *gateway.MessageEvent, prepared *preparedTurn, ts float64) map[string]any {
	content := prepared.persistUserMessage
	if content == nil {
		content = prepared.messageText
	}
	var timestamp any = ts
	if prepared.persistUserTimestamp != nil {
		timestamp = prepared.persistUserTimestamp
	}
	entry := map[string]any{"role": "user", "content": content, "timestamp": timestamp}
	if pyrepr.Truthy(prepared.persistUserDisplayKind) {
		entry["display_kind"] = prepared.persistUserDisplayKind
	}
	displayMetadata := map[string]any{}
	if prepared.persistenceOwner != nil && *prepared.persistenceOwner != "" {
		displayMetadata["gateway_input_owner"] = *prepared.persistenceOwner
	}
	maps.Copy(displayMetadata, gateway.PersistedUserMetadataForEvent(ctx, gateway.DisplayEventOf(event)))
	if len(displayMetadata) > 0 {
		entry["display_metadata"] = displayMetadata
	}
	if event.MessageID != nil && *event.MessageID != "" {
		entry["message_id"] = *event.MessageID
	}
	return entry
}

// Persists this turn, session meta on a first turn, the user row only on a transient failure, nothing on overflow
func (r *GatewayRunner) hmwaPersistTurnTranscript(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey string, agentResult map[string]any,
	agentMessages []map[string]any, prepared *preparedTurn, response string, failedEarly, hiddenIncomplete, overflow bool) error {
	ts := float64(time.Now().UnixNano()) / 1e9
	store := r.AsyncSessionStore().Store()
	sid := entry.SessionID
	history := prepared.history
	agentPersisted := r.sessionDB(ctx) != nil
	if v, ok := agentResult["agent_persisted"]; ok {
		agentPersisted = pyrepr.Truthy(v)
	}
	userRow := hmwaUserTranscriptEntry(ctx, event, prepared, ts)
	if !overflow {
		if len(history) == 0 && source.Platform != gateway.PlatformLocal {
			toolsList := resultGet(agentResult, "tools", []any{})
			if !pyrepr.Truthy(toolsList) {
				toolsList = []any{}
			}
			if err := store.AppendToTranscript(ctx, sid, map[string]any{"role": "session_meta", "tools": toolsList,
				"model": gateway.ResolveGatewayModel(ctx, nil), "platform": string(source.Platform), "timestamp": ts}, false); err != nil {
				return err
			}
		}
		if failedEarly || hiddenIncomplete {
			if event.MessageID != nil && *event.MessageID != "" && store.HasPlatformMessageID(ctx, sid, *event.MessageID) {
				runLog().Info(fmt.Sprintf("Skipping duplicate user turn (message_id=%s) in session %s", *event.MessageID, sid))
			} else if err := store.AppendToTranscript(ctx, sid, userRow, agentPersisted); err != nil {
				return err
			}
			if err := r.hmwaCloseFailedTurn(ctx, sid, hmwaFailedTurnNotice(agentResult)); err != nil {
				return err
			}
		} else {
			historyLen := len(history)
			if n, err := pyint.Int64(agentResult["history_offset"]); err == nil {
				historyLen = int(n)
			}
			var newMessages []map[string]any
			if len(agentMessages) > historyLen {
				newMessages = agentMessages[historyLen:]
			}
			if len(newMessages) == 0 {
				if err := store.AppendToTranscript(ctx, sid, userRow, agentPersisted); err != nil {
					return err
				}
				if response != "" {
					if err := store.AppendToTranscript(ctx, sid, map[string]any{"role": "assistant", "content": response, "timestamp": ts}, agentPersisted); err != nil {
						return err
					}
				}
			} else {
				attached := false
				for _, msg := range newMessages {
					if msg["role"] == "system" {
						continue
					}
					row := maps.Clone(msg)
					row["timestamp"] = ts
					_, hasID := row["message_id"]
					if !attached && msg["role"] == "user" && event.MessageID != nil && *event.MessageID != "" && !hasID {
						row["message_id"] = *event.MessageID
						attached = true
					}
					if err := store.AppendToTranscript(ctx, sid, row, agentPersisted); err != nil {
						return err
					}
				}
			}
		}
	}
	var lastPromptTokens int64
	if n, err := pyint.Int64(resultGet(agentResult, "last_prompt_tokens", 0)); err == nil {
		lastPromptTokens = n
	}
	if err := store.UpdateSession(ctx, entry.SessionKey, &lastPromptTokens, !event.Internal); err != nil {
		return err
	}
	r.refreshAgentCacheMessageCount(ctx, sessionKey, sid)
	return nil
}

// Usage window refusal reworded for a chat, every other reply unchanged
func usageWindowChatWording(agentResult map[string]any, response string) string {
	if !pyrepr.Truthy(agentResult["usage_window_message"]) || response == "" {
		return response
	}
	resetsAt, _ := pyint.Float(agentResult["failure_resets_at"])
	return provider.ChatRefusalWording(response, resetsAt)
}

// Delivers the turn response, then for a usage window refusal schedules the limit offer under it
func (r *GatewayRunner) deliverTurnResponseThenLimitOffer(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey string, runGeneration int,
	agentResult map[string]any, agentMessages []map[string]any, response, footerLine string, silent bool) (*string, error) {
	delivered, err := r.hmwaDeliverTurnResponse(ctx, event, source, entry, sessionKey, runGeneration, agentResult, agentMessages, response, footerLine, silent)
	if err != nil {
		return nil, err
	}
	if limit, ok := agentResult["usage_limit"].(map[string]any); ok {
		generation := runGeneration
		r.scheduleUsageLimitOffer(ctx, event, limit, sessionKey, &generation)
	}
	return delivered, nil
}

// Final delivery decisions for silence, voice reply and streamed media or footer, nil when already delivered
func (r *GatewayRunner) hmwaDeliverTurnResponse(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey string, runGeneration int,
	agentResult map[string]any, agentMessages []map[string]any, response, footerLine string, silent bool) (*string, error) {
	if config.DiagnosticWakeMuted(ctx, event, nil) {
		return nil, nil
	}
	if silent {
		runLog().Info(fmt.Sprintf("Suppressing intentional silence marker for session %s", entry.SessionID))
		response = ""
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	streamingTTSDone := adapter != nil && gateway.StreamingTTSTurnCompleted(adapter, sessionKey, runGeneration)
	if !streamingTTSDone && r.shouldSendVoiceReply(ctx, event, response, agentMessages, pyrepr.Truthy(agentResult["already_sent"])) {
		r.sendVoiceReply(ctx, event, response)
	}
	if pyrepr.Truthy(agentResult["already_sent"]) && !pyrepr.Truthy(agentResult["failed"]) {
		if response != "" && adapter != nil && !pyrepr.Truthy(agentResult["media_already_delivered"]) {
			r.deliverMediaFromResponse(ctx, response, event, adapter, nil)
		}
		if footerLine != "" && adapter != nil && !silent {
			if _, err := adapter.Send(ctx, source.ChatID, footerLine, "", r.eventThreadMetadata(event, source)); err != nil {
				runLog().Debug("trailing footer send failed: " + pyrepr.ExceptionMessage(err))
			}
		}
		event.SetStreamedFinalResponse(response)
		return nil, nil
	}
	return &response, nil
}

// Error reply of a failed agent turn with the inbound user turn persisted once and a sanitized hint
func (r *GatewayRunner) hmwaAgentErrorReply(ctx context.Context, turnErr error, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey string, prepared *preparedTurn) string {
	r.hmwaStopTypingForTurn(ctx, event, source)
	runLog().Error(fmt.Sprintf("Agent error in session %s", sessionKey), "error", pyrepr.ExceptionRepr(turnErr))
	statusCode := 0
	var statusErr interface{ StatusCode() int }
	if errors.As(turnErr, &statusErr) {
		statusCode = statusErr.StatusCode()
	}
	if (statusCode == 400 || statusCode == 500) && len(prepared.history) > 50 {
		return gateway.RecordUnexecutedFailure(ctx, contextOverflowReply(ctx), "", nil)
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("Failed to persist inbound user message after agent exception", "error", pyrepr.Str(rec))
			}
		}()
		if prepared.messageText == nil || entry == nil {
			return
		}
		store := r.AsyncSessionStore().Store()
		owned, err := store.HasInputOwner(ctx, optText(prepared.persistenceSessionID), optText(prepared.persistenceOwner))
		if err != nil {
			runLog().Debug("Failed to persist inbound user message after agent exception", "error", pyrepr.ExceptionMessage(err))
			return
		}
		if !owned {
			if err := store.AppendToTranscript(ctx, entry.SessionID, hmwaUserTranscriptEntry(ctx, event, prepared, float64(time.Now().UnixNano())/1e9), false); err != nil {
				runLog().Debug("Failed to persist inbound user message after agent exception", "error", pyrepr.ExceptionMessage(err))
				return
			}
		}
		if err := r.hmwaCloseFailedTurn(ctx, entry.SessionID, agent.PartialFailedTurnNotice); err != nil {
			runLog().Debug("Failed to persist inbound user message after agent exception", "error", pyrepr.ExceptionMessage(err))
		}
	}()
	t := func(key string, kwargs map[string]any) string { return i18n.T(ctx, key, "", kwargs) }
	hintKey := statusHints[statusCode]
	statusHint := ""
	if hintKey != "" && statusCode != 401 {
		statusHint = t(hintKey, nil)
	}
	switch statusCode {
	case 401:
		providerName := ""
		if st := r.sessionState(sessionKey); st.turn.agent != nil {
			providerName = st.turn.agent.Provider()
		}
		statusHint = t(hintKey, map[string]any{"relogin": provider.ReloginCommandHint(home.Dir(ctx), providerName)})
	case 429:
		errJSON := map[string]any{}
		var bodyErr interface {
			ResponseJSON() (map[string]any, error)
		}
		if errors.As(turnErr, &bodyErr) {
			if body, err := bodyErr.ResponseJSON(); err == nil {
				if inner, ok := body["error"].(map[string]any); ok {
					errJSON = inner
				}
			}
		}
		resetsIn, _ := pyint.Float(errJSON["resets_in_seconds"])
		switch {
		case errJSON["type"] != "usage_limit_reached":
			statusHint = t("gateway.errors.hint_rate_limited", nil)
		case resetsIn > 0:
			statusHint = t("gateway.errors.hint_usage_limit_resets", map[string]any{"hours": int(math.Ceil(resetsIn / 3600))})
		default:
			statusHint = t("gateway.errors.hint_usage_limit", nil)
		}
	case 400:
		statusHint = t("gateway.errors.hint_rejected", nil)
	}
	var reasoned interface{ Reason() string }
	if errors.As(turnErr, &reasoned) && reasoned.Reason() == "launch_credentials_unavailable" {
		return gateway.RecordUnexecutedFailure(ctx, agent.LaunchCredentialsCopy(ctx, turnErr), "launch_credentials_unavailable", nil)
	}
	var localError any
	if source.Platform == gateway.PlatformLocal {
		detail := []rune(pyrepr.ExceptionName(turnErr) + ": " + pyrepr.ExceptionMessage(turnErr))
		localError = string(detail[:min(len(detail), 500)])
	}
	return gateway.RecordUnexecutedFailure(ctx, hmwaAddFailedTurnNotice(t("gateway.errors.generic_failed_with_hint", map[string]any{"hint": statusHint}), agent.PartialFailedTurnNotice), "", localError)
}

// Drops the deferred post delivery callback of a turn a newer run generation superseded
func (r *GatewayRunner) hmwaDiscardStaleResult(ctx context.Context, source *gateway.SessionSource, quickKey string, runGeneration int) {
	keyOrQ := quickKey
	if keyOrQ == "" {
		keyOrQ = "?"
	}
	runLog().Info(fmt.Sprintf("Discarding stale agent result for %s — generation %d is no longer current", keyOrQ, runGeneration))
	adapter, _ := r.deliveryAdapterFor(ctx, source)
	generation := runGeneration
	popPostDeliveryCallback(adapter, quickKey, &generation)
}

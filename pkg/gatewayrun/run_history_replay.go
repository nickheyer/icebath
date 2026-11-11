package gatewayrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Copy of metadata marked as a mid turn send that must not seal a turn final stream
func interimMetadata(metadata map[string]any) map[string]any {
	merged := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		merged[k] = v
	}
	merged["_interim_send"] = true
	return merged
}

// Plain one line text of an agent notice, empty for a malformed or empty one
func renderNoticeLine(notice provider.AgentNotice) string {
	return pystr.Strip(notice.Text)
}

// Classifies an approval prompt send as sent, failed, ambiguous or declined
func approvalSendOutcome(future *concurrentFuture, timeout float64) string {
	if future == nil {
		runLog().Warn("Prompt send failed: no scheduling future (loop unavailable)")
		return "failed"
	}
	value, err := future.resultWithin(timeout)
	if errors.Is(err, errFutureTimeout) {
		return "ambiguous"
	}
	if err != nil {
		runLog().Warn(fmt.Sprintf("Prompt send failed: %s", err))
		return "failed"
	}
	result, _ := value.(gateway.SendResult)
	if result.Success {
		return "sent"
	}
	if raw, ok := result.RawResponse.(map[string]any); ok && pyrepr.Truthy(raw["ambiguous"]) {
		runLog().Warn(fmt.Sprintf("Prompt send AMBIGUOUS (lost ack): %s", pyrepr.Str(raw["error"])))
		return "ambiguous"
	}
	if gateway.DeclinedSend(&result) {
		runLog().Warn(fmt.Sprintf("Prompt send DECLINED by connector egress guard: %s", pyrepr.Str(result.Error)))
		return "declined"
	}
	reason := "unknown error"
	if pyrepr.Truthy(result.Error) {
		reason = pyrepr.Str(result.Error)
	}
	runLog().Warn(fmt.Sprintf("Prompt send failed: %s", reason))
	return "failed"
}

// Text approval prompt with the typed approve steps for a surface without buttons
func formatExecApprovalFallback(ctx context.Context, command, description, commandPrefix string, allowPermanent, allowSession, smartDenied bool) string {
	cmdPreview := gateway.TruncatePreview(command, gateway.DefaultAdapterTraits().EACmdBudget, "...")
	heading := "⚠️ **" + i18n.T(ctx, "gateway.exec_approval.header", "", nil) + "**"
	if smartDenied {
		heading = i18n.T(ctx, "gateway.exec_approval.smart_deny_heading", "", nil)
	}
	prefix := map[string]any{"prefix": commandPrefix}
	choices := []string{i18n.T(ctx, "gateway.exec_approval.text_choice_once", "", prefix)}
	if !smartDenied && allowSession {
		choices = append(choices, i18n.T(ctx, "gateway.exec_approval.text_choice_session", "", prefix))
		if allowPermanent {
			choices = append(choices, i18n.T(ctx, "gateway.exec_approval.text_choice_always", "", prefix))
		}
	}
	choices = append(choices, i18n.T(ctx, "gateway.exec_approval.text_choice_deny", "", prefix))
	body := i18n.T(ctx, "gateway.exec_approval.text_body", "", map[string]any{"heading": heading, "command": cmdPreview,
		"reason_label": i18n.T(ctx, "gateway.exec_approval.reason_label", "", nil), "reason": description})
	joiner := i18n.T(ctx, "gateway.exec_approval.text_choice_joiner", "", nil)
	return body + strings.Join(choices[:len(choices)-1], joiner) +
		i18n.T(ctx, "gateway.exec_approval.text_choice_last", "", map[string]any{"choice": choices[len(choices)-1]}) +
		gateway.FormatApprovalDeadlineLine(ctx, gateway.ApprovalTimeoutSeconds(ctx))
}

// Recovery system note for a turn a gateway restart or shutdown interrupted, an empty message meaning auto resume
func buildResumeRecoveryNote(reason *string, message string, interactive bool) string {
	reasonPhrase := "a gateway interruption"
	if reason != nil && *reason == "restart_timeout" {
		reasonPhrase = "a gateway restart"
	} else if reason != nil && *reason == "shutdown_timeout" {
		reasonPhrase = "a gateway shutdown"
	}
	var resumeGuidance, tailGuidance string
	switch {
	case message != "":
		resumeGuidance = "Address the user's NEW message below FIRST and focus on what the user is asking now."
		tailGuidance = "Do NOT re-execute old tool calls — skip any unfinished work from the conversation history."
	case interactive:
		resumeGuidance = "Report to the user that the session was restored successfully and ask what they would like to do next."
		tailGuidance = "Do NOT re-execute old tool calls — skip any unfinished work from the conversation history."
	default:
		resumeGuidance = "No user is present on this non-interactive platform, so do NOT emit a 'session restored' acknowledgement " +
			"or ask questions. Review the conversation history and CONTINUE the interrupted task to completion."
		tailGuidance = "Do NOT re-run tool calls whose results already appear in the history — resume from the first step that has no recorded result."
	}
	note := "[System note: The previous turn was interrupted by " + reasonPhrase + "; the gateway is now back online. " +
		"Any restart/shutdown command in the history has already run — do NOT re-execute or verify it. " +
		resumeGuidance + " " + tailGuidance + "]"
	if message != "" {
		note += "\n\n" + message
	}
	return note
}

// Recovery message for the model and the user text to persist, the note itself when the original is blank
func prepareResumePendingMessage(reason *string, message string, interactive bool) (string, string) {
	recovery := buildResumeRecoveryNote(reason, message, interactive)
	if pystr.Strip(message) != "" {
		return recovery, message
	}
	return recovery, recovery
}

// Assistant fields that survive replay for reasoning continuity and provider echo
var assistantReplayFields = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items", "finish_reason"}

// Replay entry of a message without tool calls keeping the replay fields, sidecar, identity and durable stamps
func buildReplayEntry(role string, content any, msg map[string]any, preserveTimestamp bool) map[string]any {
	entry := map[string]any{"role": role, "content": content}
	if sidecar, ok := msg["api_content"].(string); ok && sidecar != "" && (role == "user" || role == "assistant") && pyrepr.Equal(content, msg["content"]) {
		entry["api_content"] = sidecar
	}
	if role == "assistant" {
		for _, key := range assistantReplayFields {
			value, present := msg[key]
			if !present {
				continue
			}
			if key == "reasoning_content" {
				if value == nil {
					continue
				}
			} else if !pyrepr.Truthy(value) {
				continue
			}
			entry[key] = value
		}
	}
	if preserveTimestamp && pyrepr.Truthy(msg["timestamp"]) {
		entry["timestamp"] = msg["timestamp"]
	}
	for k, v := range state.ScaffoldingReplayFields(msg) {
		entry[k] = v
	}
	identity := map[string]any{}
	for _, key := range []string{state.MessageUID, state.AbsorbedMessageUIDs} {
		if v, ok := msg[key]; ok {
			identity[key] = v
		}
	}
	state.CopyIdentityFields(identity, entry)
	if pyrepr.Truthy(msg[state.DBPersistedMarker]) {
		entry[state.DBPersistedMarker] = true
	}
	for _, stamp := range []string{state.MergedDurableRows, state.UnnamedDurableRows, state.RetiredDurableRows} {
		if pyrepr.Truthy(msg[stamp]) {
			entry[stamp] = msg[stamp]
		}
	}
	return entry
}

const (
	telegramObservedContextPromptMarker = "observed Telegram group context"
	observedGroupContextHeader          = "[Observed Telegram group context - context only, not requests]"
	currentAddressedMessageHeader       = "[Current addressed message - answer only this unless it explicitly asks you to use the observed context]"
)

// Whether the channel prompt marks a Telegram group turn that may carry observed chatter
func usesTelegramObservedGroupContext(channelPrompt *string) bool {
	return channelPrompt != nil && strings.Contains(*channelPrompt, telegramObservedContextPromptMarker)
}

// Whether an assistant row keeps its reply only in the api_content sidecar
func hasReplayableSidecar(role, content any, msg map[string]any) bool {
	sidecar, ok := msg["api_content"].(string)
	return role == "assistant" && !pyrepr.Truthy(content) && ok && sidecar != ""
}

const (
	autoContinueNotePrefix     = "[System note: Your previous turn"
	autoContinueFallbackPrefix = "[System note: A new message"
)

// Whether user content is a gateway injected auto continue note
func isAutoContinueNoise(content any) bool {
	text, ok := content.(string)
	return ok && (strings.HasPrefix(text, autoContinueNotePrefix) || strings.HasPrefix(text, autoContinueFallbackPrefix))
}

// User text with its leading persisted auto continue notes removed
func stripAutoContinueNoise(content any) any {
	if !isAutoContinueNoise(content) {
		return content
	}
	text := pyrepr.Str(content)
	for isAutoContinueNoise(text) {
		end := strings.Index(text, "]")
		if end < 0 {
			return ""
		}
		text = pystr.Lstrip(text[end+1:])
	}
	return text
}

// Agent replay messages from stored gateway transcript rows and the observed group context kept out of them
func buildGatewayAgentHistory(ctx context.Context, history []map[string]any, channelPrompt *string, injectTimestamps bool) ([]map[string]any, *string, error) {
	tz, _ := config.TimeZone(ctx)
	var agentHistory []map[string]any
	var observed []string
	separateObserved := usesTelegramObservedGroupContext(channelPrompt)
	for _, msg := range history {
		role, _ := msg["role"].(string)
		if role == "" || role == "session_meta" || role == "system" {
			continue
		}
		content := msg["content"]
		if separateObserved && pyrepr.Truthy(msg["observed"]) && role == "user" && pyrepr.Truthy(content) {
			if text, ok := content.(string); ok && injectTimestamps {
				rendered, err := gateway.RenderUserContentWithTimestamp(text, msg["timestamp"], tz)
				if err != nil {
					return nil, nil, err
				}
				content = rendered
			}
			observed = append(observed, pystr.Strip(pyrepr.Str(content)))
			continue
		}
		_, hasCalls := msg["tool_calls"]
		_, hasCallID := msg["tool_call_id"]
		if hasCalls || hasCallID || role == "tool" {
			clean := make(map[string]any, len(msg))
			for k, v := range msg {
				if k != "timestamp" && k != "observed" {
					clean[k] = v
				}
			}
			agentHistory = append(agentHistory, clean)
			continue
		}
		if !pyrepr.Truthy(content) && !hasReplayableSidecar(role, content, msg) {
			continue
		}
		replayTimestamp := msg["timestamp"]
		if role == "user" {
			if text, ok := content.(string); ok {
				body, embedded := gateway.StripLeadingMessageTimestamps(text, tz)
				cleanBody := stripAutoContinueNoise(body)
				if !pyrepr.Equal(cleanBody, body) {
					content = cleanBody
					if embedded != nil {
						replayTimestamp = *embedded
					}
				}
			}
			if !pyrepr.Truthy(content) {
				continue
			}
		}
		entry := buildReplayEntry(role, content, msg, role == "user")
		if text, ok := content.(string); ok && injectTimestamps && role == "user" && !state.IsCompactionScaffolding(msg) {
			rendered, err := gateway.RenderUserContentWithTimestamp(text, replayTimestamp, tz)
			if err != nil {
				return nil, nil, err
			}
			sidecar, _ := entry["api_content"].(string)
			if !pyrepr.Equal(rendered, text) && sidecar != "" {
				renderedText := pyrepr.Str(rendered)
				if !(sidecar == renderedText || strings.HasPrefix(sidecar, renderedText+"\n\n")) {
					delete(entry, "api_content")
				}
			}
			entry["content"] = rendered
		}
		if pyrepr.Truthy(msg["mirror"]) {
			mirrorSrc, present := msg["mirror_source"]
			if !present {
				mirrorSrc = "another session"
			}
			entry["content"] = fmt.Sprintf("[Delivered from %s] %s", pyrepr.Str(mirrorSrc), pyrepr.Str(entry["content"]))
			delete(entry, "api_content")
		}
		agentHistory = append(agentHistory, entry)
	}
	agentHistory = agent.CanonicalizeReplayHistory(agentHistory, nil)
	if joined := pystr.Strip(strings.Join(observed, "\n")); joined != "" {
		return agentHistory, &joined, nil
	}
	return agentHistory, nil, nil
}

// Live cached transcript when it is longer and holds a real unpersisted row, else the persisted one
func selectCachedAgentHistory(persisted []map[string]any, live []map[string]any) ([]map[string]any, bool) {
	if live != nil && len(live) > len(persisted) {
		for _, message := range live {
			if !pyrepr.Truthy(message[state.DBPersistedMarker]) && !state.IsEphemeralScaffolding(message) {
				return slices.Clone(live), true
			}
		}
	}
	return persisted, false
}

// Current user turn with the observed Telegram context prepended for the API only
func wrapCurrentMessageWithObservedContext(message any, observedContext *string) any {
	if observedContext == nil || *observedContext == "" {
		return message
	}
	prefix := observedGroupContextHeader + "\n" + *observedContext + "\n\n" + currentAddressedMessageHeader + "\n"
	switch m := message.(type) {
	case string:
		return prefix + m
	case []any:
		wrapped := make([]any, len(m))
		for i, part := range m {
			if d, ok := part.(map[string]any); ok {
				copied := make(map[string]any, len(d))
				for k, v := range d {
					copied[k] = v
				}
				wrapped[i] = copied
			} else {
				wrapped[i] = part
			}
		}
		for _, part := range wrapped {
			if d, ok := part.(map[string]any); ok && d["type"] == "text" {
				text, present := d["text"]
				if !present {
					text = ""
				}
				d["text"] = prefix + pyrepr.Str(text)
				return wrapped
			}
		}
		return append([]any{map[string]any{"type": "text", "text": pystr.Rstrip(prefix)}}, wrapped...)
	}
	return message
}

// Timestamp of the last usable transcript row, nil when that row carries none
func lastTranscriptTimestamp(history []map[string]any) any {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "" || role == "session_meta" || role == "system" {
			continue
		}
		return msg["timestamp"]
	}
	return nil
}

// Tools whose results may carry deliverable media that auto appends to the reply
var autoAppendMediaToolNames = map[string]bool{"text_to_speech": true, "text_to_speech_tool": true, "image_generate": true}

// Fields of a JSON tool payload that name a local media file
var jsonMediaToolPathFields = []string{"host_image", "image", "agent_visible_image"}

// Extension anchored MEDIA tag matcher of the dispatch site
const toolMediaPattern = `(?i)MEDIA:((?:[A-Za-z]:[/\\]|/|~\/)\S+\.(?:png|jpe?g|gif|webp|` +
	`mp4|mov|avi|mkv|webm|ogg|opus|mp3|wav|m4a|` +
	`flac|epub|pdf|zip|rar|7z|docx?|xlsx?|pptx?|` +
	`txt|csv|apk|ipa))`

var toolMediaRe = pyre.MustCompile(toolMediaPattern)

// Same matcher anchored at both ends, the fullmatch of hermes
var toolMediaFullRe = mustFullmatch(toolMediaPattern)

// Python fullmatch regexp of the pattern, panicking on a pattern Python rejects
func mustFullmatch(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Media tags from this turn's producer tool results and whether one asked for voice delivery
func collectAutoAppendMediaTags(messages []map[string]any, historyOffset int, historyMediaPaths map[string]struct{}) ([]string, bool, error) {
	newMessages := messages
	if historyOffset != 0 && len(messages) >= historyOffset {
		newMessages = messages[historyOffset:]
	}
	asAny := make([]any, len(newMessages))
	for i, m := range newMessages {
		asAny[i] = m
	}
	toolNames, err := gateway.ToolNameByCallID(asAny)
	if err != nil {
		return nil, false, err
	}
	var tags []string
	voice := false
	for _, msg := range newMessages {
		if msg["role"] != "tool" && msg["role"] != "function" {
			continue
		}
		callID := ""
		if pyrepr.Truthy(msg["tool_call_id"]) {
			callID = pyrepr.Str(msg["tool_call_id"])
		} else if pyrepr.Truthy(msg["call_id"]) {
			callID = pyrepr.Str(msg["call_id"])
		}
		toolName, known := toolNames[callID]
		if !known || !autoAppendMediaToolNames[toolName] {
			continue
		}
		content := ""
		if pyrepr.Truthy(msg["content"]) {
			content = pyrepr.Str(msg["content"])
		}
		if toolName == "image_generate" && !strings.Contains(content, "MEDIA:") {
			var payload map[string]any
			if json.Unmarshal([]byte(content), &payload) == nil && payload != nil && pyrepr.Truthy(payload["success"]) {
				for _, field := range jsonMediaToolPathFields {
					path, ok := payload[field].(string)
					if !ok {
						continue
					}
					if full, _ := toolMediaFullRe.MatchString("MEDIA:" + path); !full {
						continue
					}
					if _, seen := historyMediaPaths[path]; seen {
						continue
					}
					tags = append(tags, "MEDIA:"+path)
					break
				}
			}
			continue
		}
		if !strings.Contains(content, "MEDIA:") {
			continue
		}
		match, _ := toolMediaRe.FindStringMatch(content)
		for match != nil {
			path := strings.TrimRight(pystr.Strip(match.GroupByNumber(1).String()), "\",}")
			if _, seen := historyMediaPaths[path]; path != "" && !seen {
				tags = append(tags, "MEDIA:"+path)
			}
			match, _ = toolMediaRe.FindNextMatch(match)
		}
		if strings.Contains(content, "[[audio_as_voice]]") {
			voice = true
		}
	}
	return tags, voice, nil
}

package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var inlineThinkBlockRe = regexp.MustCompile(`(?s)<think>(.*?)</think>`)

// Native carriers replayed verbatim on later turns, in the order the builder copies them
var assistantCarrierAttrs = []string{"anthropic_content_blocks", "bedrock_content_blocks", "codex_reasoning_items", "codex_message_items"}

// Python attribute value of a normalized message, raw JSON carriers decoded and unknown keys read from Extra
func messageAttr(m *state.Message, key string) any {
	var raw json.RawMessage
	switch key {
	case "reasoning_details":
		raw = m.ReasoningDetails
	case "codex_reasoning_items":
		raw = m.CodexReasoningItems
	case "codex_message_items":
		raw = m.CodexMessageItems
	case "anthropic_content_blocks":
		if blocks, ok := provider.AnthropicLiveBlocks(m); ok {
			return blocks
		}
	}
	if len(raw) > 0 {
		var out any
		if json.Unmarshal(raw, &out) == nil {
			return out
		}
	}
	return m.Extra[key]
}

// Structured reasoning or inline think blocks, fired to the reasoning callback when nothing streamed it
func (a *AIAgent) assistantReasoningText(resp *provider.Response) any {
	reasoning := a.extractReasoning(resp)
	if reasoning == "" {
		content := state.FlattenMessageText(resp.Message.Content, "\n")
		var parts []string
		for _, block := range inlineThinkBlockRe.FindAllStringSubmatch(content, -1) {
			if s := pystr.Strip(block[1]); s != "" {
				parts = append(parts, s)
			}
		}
		reasoning = strings.Join(parts, "\n\n")
	}
	if reasoning == "" {
		return nil
	}
	if a.verboseLogging {
		slog.Debug(fmt.Sprintf("Captured reasoning (%d chars): %s", utf8.RuneCountInString(reasoning), reasoning))
	}
	if a.reasoningCallback != nil && a.streamDeltaCallback == nil && a.streamCallback == nil {
		callQuietly(a.reasoningCallback, reasoning)
	}
	return state.SanitizeSurrogates(reasoning)
}

// Storage text with surrogates sanitized, inline think tags stripped and inlined credentials redacted
func (a *AIAgent) assistantContentForStorage(m *state.Message) string {
	content := state.SanitizeSurrogates(state.FlattenMessageText(m.Content, "\n"))
	if content == "" {
		return content
	}
	content = pystr.Strip(a.stripThinkBlocks(content))
	if content != "" {
		content = tools.RedactSensitiveTextWith(a.env, content, tools.RedactOptions{})
	}
	return content
}

// Nonblank string value, empty for anything else
func nonblankString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && pystr.Strip(s) != ""
}

// Replayable tool call dict with resolved call and response item ids, arguments kept unredacted for replay
func (a *AIAgent) assistantToolCallDict(tc state.ToolCall, index int) map[string]any {
	rawID := tc.ID
	callID, ok := nonblankString(tc.Extra["call_id"])
	if !ok {
		callID, _ = provider.SplitResponsesToolID(rawID)
		_, ok = nonblankString(callID)
	}
	if !ok {
		if pystr.Strip(rawID) != "" {
			callID = pystr.Strip(rawID)
		} else {
			callID = tools.DeterministicCallID(tc.Function.Name, tc.Function.Arguments, index)
		}
	}
	callID = pystr.Strip(callID)
	responseItemID, ok := nonblankString(tc.Extra["response_item_id"])
	if !ok {
		_, responseItemID = provider.SplitResponsesToolID(rawID)
	}
	fn := map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments}
	out := map[string]any{"id": callID, "call_id": callID,
		"response_item_id": provider.DeriveResponsesFunctionCallID(callID, responseItemID), "type": tc.Type, "function": fn}
	if extra, has := tc.Extra["extra_content"]; has && extra != nil {
		out["extra_content"] = provider.DumpIfModel(extra)
	}
	sig := tc.Extra["thought_signature"]
	if !pyrepr.Truthy(sig) {
		sig = tc.Function.Extra["thought_signature"]
	}
	if s, ok := sig.(string); ok && s != "" {
		fn["thought_signature"] = s
	}
	return out
}

// Normalized assistant row with reasoning, replay carriers and tool calls, shared by tool call and final paths
func (a *AIAgent) buildAssistantMessageRow(resp *provider.Response, finishReason string) map[string]any {
	m := resp.Message
	reasoningText := a.assistantReasoningText(resp)
	msg := state.StampMessageTimestamp(map[string]any{"role": "assistant",
		"content": a.assistantContentForStorage(m), "reasoning": reasoningText, "finish_reason": finishReason}, nil)
	var rawReasoningContent any
	if m.ReasoningContent != "" {
		rawReasoningContent = m.ReasoningContent
	} else if v, ok := m.Extra["reasoning_content"]; ok {
		rawReasoningContent = v
	}
	switch {
	case rawReasoningContent != nil:
		if s, ok := rawReasoningContent.(string); ok {
			msg["reasoning_content"] = state.SanitizeSurrogates(s)
		} else {
			msg["reasoning_content"] = rawReasoningContent
		}
	case len(m.ToolCalls) > 0 && a.needsThinkingReasoningPad():
		if reasoningText != nil {
			msg["reasoning_content"] = reasoningText
		} else {
			msg["reasoning_content"] = " "
		}
	case reasoningText != nil:
		msg["reasoning_content"] = reasoningText
	}
	if details := messageAttr(m, "reasoning_details"); pyrepr.Truthy(details) {
		var preserved []any
		for _, d := range anyRows(details) {
			switch dv := d.(type) {
			case map[string]any:
				preserved = append(preserved, dv)
			case provider.ModelDumper:
				preserved = append(preserved, provider.DumpIfModel(dv))
			}
		}
		if len(preserved) > 0 {
			msg["reasoning_details"] = preserved
		}
	}
	for _, attr := range assistantCarrierAttrs {
		value := messageAttr(m, attr)
		if !pyrepr.Truthy(value) {
			continue
		}
		msg[attr] = value
		if attr == "codex_reasoning_items" && hasReplayableNativeCompactionCheckpoint(a, []map[string]any{msg}) {
			if noter, ok := a.contextCompressor.(nativeCompactionCheckpointNoter); ok {
				noter.NoteNativeCompactionCheckpoint()
				a.setUsageAnchor(nil, false)
			}
			if a.currentTaskID != "" {
				resetReadDedupCaches(a.homeContext(), a.currentTaskID, a.sessionID)
			}
		}
	}
	if record := provider.CarrierRecord(a.homeContext(), state.MessageToDict(m)); record != nil {
		for k, v := range record {
			if k != "type" {
				msg[k] = v
			}
		}
		msg["reasoning_details"] = append(anyRows(msg["reasoning_details"]), record)
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]any, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			calls[i] = a.assistantToolCallDict(tc, i)
		}
		msg["tool_calls"] = calls
	}
	return msg
}

// Assistant row of the response, a held turn's visible row typed hidden until the final publishes it
func (a *AIAgent) buildAssistantMessage(assistantMessage *provider.Response, finishReason string) map[string]any {
	resp := assistantMessage
	if resp == nil || resp.Message == nil {
		resp = &provider.Response{Message: &state.Message{Role: "assistant"}}
	}
	return withholdHeldRow(a, a.buildAssistantMessageRow(resp, finishReason))
}

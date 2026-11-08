package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	intakeThinkTagAlt     = quotedThinkTagAlternation()
	intakeReasoningTagRE  = regexp.MustCompile(`(?i)</?(?:` + intakeThinkTagAlt + `)>`)
	intakeInlineReasoning = regexp.MustCompile(`(?is)<(?:` + intakeThinkTagAlt + `)>(.*?)</(?:` + intakeThinkTagAlt + `)>`)
)

// One escaped alternation over every think tag the scrubber hides
func quotedThinkTagAlternation() string {
	names := make([]string, len(provider.ThinkTagNames))
	for i, name := range provider.ThinkTagNames {
		names[i] = regexp.QuoteMeta(name)
	}
	return strings.Join(names, "|")
}

// Text of dict or list content from servers that send structured content, multimodal lists keeping their text parts
func coerceContentText(raw any) (any, error) {
	switch c := raw.(type) {
	case map[string]any:
		if text, ok := c["text"]; ok && pyrepr.Truthy(text) {
			return text, nil
		}
		if content, ok := c["content"]; ok && pyrepr.Truthy(content) {
			return content, nil
		}
		text, err := pyjson.Dumps(c)
		return text, err
	case []map[string]any:
		items := make([]any, len(c))
		for i, part := range c {
			items[i] = part
		}
		return coerceContentText(items)
	case []any:
		parts := make([]string, 0, len(c))
		for i, part := range c {
			switch p := part.(type) {
			case string:
				parts = append(parts, p)
			case map[string]any:
				if p["type"] == "text" {
					text, has := p["text"]
					if !has {
						text = ""
					}
					s, ok := text.(string)
					if !ok {
						return nil, &pyrepr.FormatError{Class: "TypeError", Msg: fmt.Sprintf("sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(text))}
					}
					parts = append(parts, s)
				} else if text, has := p["text"]; has {
					parts = append(parts, pyrepr.Str(text))
				}
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	return pyrepr.Str(raw), nil
}

// Fires post_api_request with the response, usage and timing views when a consumer is registered, failures swallowed
func firePostAPIRequestHook(ctx context.Context, a *AIAgent, s *LoopState, assistantMessage *provider.Response, finishReason string) {
	defer func() {
		if r := recover(); r != nil && isUserInterruptPanic(r) {
			panic(r)
		}
	}()
	if !lifecycleHasHook(ctx, "post_api_request") {
		return
	}
	var firstChunkAt, contextLength, finish any
	if a.lastAPIFirstChunkAt != nil {
		firstChunkAt = *a.lastAPIFirstChunkAt
	}
	if a.contextCompressor != nil {
		contextLength = a.contextCompressor.Base().ContextLength
	}
	if finishReason != "" {
		finish = finishReason
	}
	msg := assistantMessage.Message
	if msg == nil {
		msg = &state.Message{}
	}
	contentChars, ok := pySizedLen(msg.Content)
	if !ok {
		return
	}
	hookMessage := state.MessageToDict(msg)
	hookMessage["finish_reason"] = finish
	lifecycleInvokeHook(ctx, "post_api_request", map[string]any{
		"task_id": s.EffectiveTaskID, "turn_id": s.TurnID, "api_request_id": s.APIRequestID,
		"session_id": a.sessionID, "platform": a.platform, "model": a.model, "provider": a.provider,
		"base_url": a.baseURL, "api_mode": a.apiMode, "api_call_count": s.APICallCount,
		"api_duration": s.APIDuration, "started_at": s.APIStartTime, "ended_at": s.APIStartTime + s.APIDuration,
		"first_chunk_at": firstChunkAt, "finish_reason": finish, "message_count": len(s.APIMessages),
		"response_model":    state.ToolCallField(s.Response, "model"),
		"response":          a.apiResponsePayloadForHook(s.Response, assistantMessage, finishReason),
		"usage":             a.usageSummaryForAPIRequestHook(s.Response),
		"context_length":    contextLength,
		"assistant_message": hookMessage, "assistant_content_chars": contentChars,
		"assistant_tool_call_count": len(msg.ToolCalls), "moa_references": moaReferenceMetricsForHook(a),
	})
}

// Python len of value or empty text, false when the value has no length
func pySizedLen(v any) (int, bool) {
	if !pyrepr.Truthy(v) {
		return 0, true
	}
	switch c := v.(type) {
	case string:
		return pystr.Len(c), true
	case []any:
		return len(c), true
	case map[string]any:
		return len(c), true
	}
	return 0, false
}

// Reasoning for the reasoning.available event from the structured extractor, else inline think blocks, never reply text
func reasoningPreview(a *AIAgent, content string, assistantMessage *provider.Response) string {
	if assistantMessage != nil {
		if extracted := pystr.Strip(a.extractReasoning(assistantMessage)); extracted != "" {
			return extracted
		}
	}
	if content == "" {
		return ""
	}
	var blocks []string
	for _, m := range intakeInlineReasoning.FindAllStringSubmatch(content, -1) {
		if block := pystr.Strip(m[1]); block != "" {
			blocks = append(blocks, block)
		}
	}
	return strings.Join(blocks, "\n\n")
}

// Relays a subagent first line preview to the parent or extracted reasoning to a top level progress callback
func relayThinking(a *AIAgent, content any, assistantMessage *provider.Response) error {
	text, isText := content.(string)
	if !isText && pyrepr.Truthy(content) {
		return &pyrepr.FormatError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(content))}
	}
	thinkText := pystr.Strip(intakeReasoningTagRE.ReplaceAllString(pystr.Strip(text), ""))
	firstLine := ""
	if thinkText != "" {
		firstLine = pystr.Slice(strings.SplitN(thinkText, "\n", 2)[0], 0, 80)
	}
	if a.delegateDepth > 0 {
		if firstLine != "" {
			callToolProgress(a, "_thinking", firstLine, nil)
		}
		return nil
	}
	if a.nativeReasoningStreamed {
		return nil
	}
	reasoning := reasoningPreview(a, text, assistantMessage)
	if reasoning == "" {
		return nil
	}
	callToolProgress(a, "reasoning.available", "_thinking", pystr.Slice(reasoning, 0, 500))
	return nil
}

// Calls the progress hook in either shape with no arguments, swallowing any failure but a user interrupt
func callToolProgress(a *AIAgent, event, name string, preview any) {
	defer func() {
		if r := recover(); r != nil && isUserInterruptPanic(r) {
			panic(r)
		}
	}()
	switch cb := progressFn(a.toolProgressCallback).(type) {
	case toolProgressFn:
		text, _ := preview.(string)
		cb(event, name, text, nil, nil)
	case codexProgressFn:
		cb(event, name, preview, nil, nil)
	}
}

// First hundred items of content with an ellipsis past them, the error a subscript of content raises
func assistantContentPreview(content any) (string, error) {
	switch c := content.(type) {
	case string:
		if pystr.Len(c) > 100 {
			return pystr.Slice(c, 0, 100) + "...", nil
		}
		return c, nil
	case []any:
		if len(c) > 100 {
			return pyrepr.Str(c[:100]) + "...", nil
		}
		return pyrepr.Str(c), nil
	case map[string]any:
		return "", &pyrepr.FormatError{Class: "KeyError", Msg: "slice(None, 100, None)"}
	}
	return "", &pyrepr.FormatError{Class: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(content))}
}

// Opened but never closed scratchpad in content or empty text, lists and dicts checked by membership
func contentHasIncompleteScratchpad(content any) (bool, error) {
	const opened, closed = "<REASONING_SCRATCHPAD>", "</REASONING_SCRATCHPAD>"
	if !pyrepr.Truthy(content) {
		return false, nil
	}
	switch c := content.(type) {
	case string:
		return HasIncompleteScratchpad(c), nil
	case []any:
		has := func(want string) bool {
			for _, item := range c {
				if pyrepr.Equal(item, want) {
					return true
				}
			}
			return false
		}
		return has(opened) && !has(closed), nil
	case map[string]any:
		_, open := c[opened]
		_, shut := c[closed]
		return open && !shut, nil
	}
	return false, &pyrepr.FormatError{Class: "TypeError", Msg: fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(content))}
}

// Normalizes the response into a text assistant message then runs the post response hooks and continuation guards in order
func normalizeModelResponse(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	assistantMessage, err := a.normalizeResponseForAgent(s.Response)
	if err != nil {
		return PhaseVerdict{}, err
	}
	finishReason := assistantMessage.FinishReason
	activeSystemPrompt := s.ActiveSystemPrompt
	verdict := func(action string, result map[string]any) (PhaseVerdict, error) {
		s.AssistantMessage, s.FinishReason, s.ActiveSystemPrompt = assistantMessage, finishReason, activeSystemPrompt
		return PhaseVerdict{Action: action, Result: result}, nil
	}

	var content any
	if msg := assistantMessage.Message; msg != nil {
		if _, isText := msg.Content.(string); msg.Content != nil && !isText {
			if msg.Content, err = coerceContentText(msg.Content); err != nil {
				return PhaseVerdict{}, err
			}
		}
		content = msg.Content
	}

	spliceProviderProjection(a, s.Response, &s.Messages)
	firePostAPIRequestHook(ctx, a, s, assistantMessage, finishReason)
	var replyMessage map[string]any
	if assistantMessage.Message != nil {
		replyMessage = state.MessageToDict(assistantMessage.Message)
	}
	observability.RecordReplyContent(ctx, a, s.Response, replyMessage)

	if pyrepr.Truthy(content) && !a.quietMode {
		if a.verboseLogging {
			a.vprint(a.logPrefix+"🤖 Assistant: "+pyrepr.Str(content), false, false)
		} else {
			preview, err := assistantContentPreview(content)
			if err != nil {
				return PhaseVerdict{}, err
			}
			a.vprint(a.logPrefix+"🤖 Assistant: "+preview, false, false)
		}
	}
	if a.toolProgressCallback != nil {
		if err := relayThinking(a, content, assistantMessage); err != nil {
			return PhaseVerdict{}, err
		}
	}

	incomplete, err := contentHasIncompleteScratchpad(content)
	if err != nil {
		return PhaseVerdict{}, err
	}
	if incomplete {
		a.incompleteScratchpadRetries++
		a.bufferVprint("⚠️  Incomplete <REASONING_SCRATCHPAD> detected (opened but never closed)")
		if a.incompleteScratchpadRetries <= 2 {
			a.bufferVprint(fmt.Sprintf("🔄 Retrying API call (%d/2)...", a.incompleteScratchpadRetries))
			return verdict("continue", nil)
		}
		a.flushStatusBuffer()
		a.vprint(a.logPrefix+"❌ Max retries (2) for incomplete scratchpad. Saving as partial.", true, true)
		a.incompleteScratchpadRetries = 0
		rolledBack := a.getMessagesUpToLastAssistant(s.Messages)
		a.cleanupTaskResources(s.EffectiveTaskID)
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
		return verdict("return", PartialResult(rolledBack, s.APICallCount, "Incomplete REASONING_SCRATCHPAD after 2 retries", nil, false, false))
	}
	a.incompleteScratchpadRetries = 0

	if a.apiMode == "codex_responses" && finishReason == "incomplete" {
		result, fellOver, err := a.continueCodexIncomplete(ctx, assistantMessage, finishReason, &s.Messages,
			s.ConversationHistory, s.APICallCount, s.Response)
		if err != nil {
			return PhaseVerdict{}, err
		}
		if fellOver {
			activeSystemPrompt = syncFailoverSystemMessage(a, s.APIMessages, activeSystemPrompt)
			return verdict("continue", nil)
		}
		if result != nil {
			return verdict("return", result)
		}
		return verdict("continue", nil)
	}
	a.codexIncompleteRetries = 0
	a.codexReasoningOnlyStreak = 0
	return verdict("fallthrough", nil)
}

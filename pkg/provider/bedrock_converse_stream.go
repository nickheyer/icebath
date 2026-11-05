package provider

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
)

// Converse stopReason mapped to the OpenAI finish reason, unknown values meaning stop
var bedrockStopReasonToFinish = map[string]string{
	"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls", "max_tokens": "length",
	"content_filtered": "content_filter", "guardrail_intervened": "content_filter",
}

// OpenAI shaped usage of one Converse answer, prompt tokens including cache reads and writes
type bedrockConverseUsage struct {
	PromptTokens             int
	CompletionTokens         int
	TotalTokens              int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
}

// OpenAI shaped Converse answer with the ordered block sidecar and persisted reasoning details
type bedrockConverseResult struct {
	Content          any
	ToolCalls        []state.ToolCall
	ReasoningDetails []any
	ReasoningContent string
	ContentBlocks    []any
	FinishReason     string
	Usage            bedrockConverseUsage
	Model            string
}

// Redacted reasoning payload as base64, bytes encoded and strings passed through
func bedrockEncodeRedacted(redacted any) (string, bool) {
	switch r := redacted.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(r), true
	case string:
		return r, true
	}
	return "", false
}

// Tool call with the input dumped as Python json text
func bedrockToolCall(id, name, input any) state.ToolCall {
	return state.ToolCall{ID: pyStr(id), Type: "function", Function: state.ToolCallFunction{Name: pyStr(name), Arguments: bedrockPyDumps(input)}}
}

// Anthropic shaped thinking and redacted thinking records of the ordered reasoning blocks
func bedrockPersistedReasoningDetails(ordered []any) []any {
	var details []any
	for _, item := range ordered {
		block, ok := asMap(item)
		if !ok {
			continue
		}
		reasoning, ok := asMap(block["reasoningContent"])
		if !ok {
			continue
		}
		if text, ok := reasoning["text"].(string); ok && text != "" {
			d := map[string]any{"type": "thinking", "thinking": text}
			if truthy(reasoning["signature"]) {
				d["signature"] = reasoning["signature"]
			}
			details = append(details, d)
		}
		if truthy(reasoning["redactedContentBase64"]) {
			details = append(details, map[string]any{"type": "redacted_thinking", "data": reasoning["redactedContentBase64"]})
		}
	}
	return details
}

// Accumulator shared by the response and stream normalizers
type bedrockResponseParts struct {
	textParts        []string
	reasoningDetails []any
	toolCalls        []state.ToolCall
}

// Folds a reasoningContent payload into the accumulators and the ordered block, nested or flat
func (p *bedrockResponseParts) absorbReasoning(raw any, block map[string]any, onText func(string) error) error {
	reasoning, ok := asMap(raw)
	if !ok {
		return nil
	}
	if nested, ok := asMap(reasoning["reasoningText"]); ok {
		merged := make(map[string]any, len(reasoning)+len(nested))
		for k, v := range reasoning {
			merged[k] = v
		}
		for k, v := range nested {
			merged[k] = v
		}
		reasoning = merged
	}
	if text := reasoning["text"]; truthy(text) {
		s := pyStr(text)
		if onText != nil {
			if err := onText(s); err != nil {
				return err
			}
		}
		prev, _ := block["text"].(string)
		block["text"] = prev + s
	}
	if sig, ok := reasoning["signature"].(string); ok && sig != "" {
		prev, _ := block["signature"].(string)
		block["signature"] = prev + sig
	}
	if encoded, ok := bedrockEncodeRedacted(reasoning["redactedContent"]); ok && encoded != "" {
		p.reasoningDetails = append(p.reasoningDetails, map[string]any{"type": "redacted_thinking", "data": encoded})
		block["redactedContentBase64"] = encoded
	}
	return nil
}

// Assembles the answer, cache tokens added back into the prompt count
func (p *bedrockResponseParts) build(ordered []any, usage map[string]any, stopReason, model string) *bedrockConverseResult {
	out := &bedrockConverseResult{ToolCalls: p.toolCalls, Model: model}
	if len(p.textParts) > 0 {
		out.Content = strings.Join(p.textParts, "\n")
	}
	if details := bedrockPersistedReasoningDetails(ordered); len(details) > 0 {
		out.ReasoningDetails = details
	} else if len(p.reasoningDetails) > 0 {
		out.ReasoningDetails = p.reasoningDetails
	}
	var texts []string
	for _, item := range ordered {
		block, _ := asMap(item)
		if reasoning, ok := asMap(block["reasoningContent"]); ok && truthy(reasoning["text"]) {
			texts = append(texts, pyStr(reasoning["text"]))
		}
	}
	out.ReasoningContent = strings.Join(texts, "\n\n")
	if len(ordered) > 0 {
		out.ContentBlocks = ordered
	}
	read, write := bedrockUsageInt(usage["cacheReadInputTokens"]), bedrockUsageInt(usage["cacheWriteInputTokens"])
	output, input := bedrockUsageInt(usage["outputTokens"]), bedrockUsageInt(usage["inputTokens"])
	prompt := input + read + write
	out.Usage = bedrockConverseUsage{PromptTokens: prompt, CompletionTokens: output, TotalTokens: prompt + output,
		CacheReadInputTokens: read, CacheCreationInputTokens: write}
	finish, ok := bedrockStopReasonToFinish[stopReason]
	if !ok {
		finish = "stop"
	}
	if len(p.toolCalls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	out.FinishReason = finish
	return out
}

// Token count of a usage value, exact JSON numbers included
func bedrockUsageInt(v any) int {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		f, _ := n.Float64()
		return int(f)
	}
	i, _ := anyInt(v)
	return i
}

// Text of a block value, Python str for anything else
func bedrockTextOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyStr(v)
}

// Converse response as the OpenAI shaped answer with ordered blocks and reasoning details
func normalizeConverseResponse(response map[string]any) *bedrockConverseResult {
	parts := &bedrockResponseParts{}
	var ordered []any
	output, _ := asMap(response["output"])
	message, _ := asMap(output["message"])
	content, _ := asList(message["content"])
	for _, item := range content {
		block, ok := asMap(item)
		if !ok {
			continue
		}
		if text, has := block["text"]; has {
			parts.textParts = append(parts.textParts, bedrockTextOf(text))
			ordered = append(ordered, map[string]any{"text": text})
		} else if reasoning, has := block["reasoningContent"]; has {
			orderedReasoning := map[string]any{}
			_ = parts.absorbReasoning(reasoning, orderedReasoning, nil)
			if len(orderedReasoning) > 0 {
				ordered = append(ordered, map[string]any{"reasoningContent": orderedReasoning})
			}
		} else if raw, has := block["toolUse"]; has {
			tu, _ := asMap(raw)
			id, name, input := pyGetAny(tu, "toolUseId", ""), pyGetAny(tu, "name", ""), pyGetAny(tu, "input", map[string]any{})
			ordered = append(ordered, bedrockToolUseBlock(id, name, input))
			parts.toolCalls = append(parts.toolCalls, bedrockToolCall(id, name, input))
		}
	}
	usage, _ := asMap(response["usage"])
	stop := pyGetStr(response, "stopReason", "end_turn")
	return parts.build(ordered, usage, stop, pyGetStr(response, "modelId", ""))
}

// Callbacks of a streamed Converse answer, an error from any of them aborting the stream
type bedrockStreamCallbacks struct {
	OnTextDelta      func(text string) error
	OnToolStart      func(index int, id, name string) error
	OnToolDone       func(index int, call state.ToolCall) error
	OnReasoningDelta func(text string) error
	OnInterruptCheck func() bool
	OnEvent          func()
}

// Source of decoded ConverseStream events, io.EOF at the end
type bedrockEventSource func() (map[string]any, error)

// Event source over an in memory event list
func bedrockEventList(events []map[string]any) bedrockEventSource {
	i := 0
	return func() (map[string]any, error) {
		if i >= len(events) {
			return nil, io.EOF
		}
		i++
		return events[i-1], nil
	}
}

// Integer contentBlockIndex of an event payload, false when absent or not an integer
func bedrockBlockIndex(payload map[string]any) (int, bool) {
	switch v := payload["contentBlockIndex"].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// Runs the watchdog hook with any panic swallowed so it never aborts the stream
func bedrockSafeOnEvent(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// ConverseStream events consumed into the response shape, blocks keyed by contentBlockIndex
func streamConverseWithCallbacks(next bedrockEventSource, cb bedrockStreamCallbacks) (*bedrockConverseResult, bool, error) {
	parts := &bedrockResponseParts{}
	blocks := map[int]map[string]any{}
	current, hasCurrent := 0, false
	var tool map[string]string
	var textBuffer []string
	hasToolUse, interrupted := false, false
	stopReason, hasStop := "", false
	usage := map[string]any{}
	blockIndex := func(payload map[string]any, newBlock bool) int {
		if idx, ok := bedrockBlockIndex(payload); ok {
			return idx
		}
		if newBlock || !hasCurrent {
			return len(blocks)
		}
		return current
	}
	flushText := func() {
		if len(textBuffer) > 0 {
			parts.textParts = append(parts.textParts, strings.Join(textBuffer, ""))
			textBuffer = nil
		}
	}
	for {
		event, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		if cb.OnEvent != nil {
			bedrockSafeOnEvent(cb.OnEvent)
		}
		if cb.OnInterruptCheck != nil && cb.OnInterruptCheck() {
			interrupted = true
			break
		}
		if raw, has := event["contentBlockStart"]; has {
			startEvent, _ := asMap(raw)
			idx := blockIndex(startEvent, true)
			current, hasCurrent = idx, true
			start, _ := asMap(pyGetAny(startEvent, "start", map[string]any{}))
			if tu, has := start["toolUse"]; has {
				hasToolUse = true
				flushText()
				t, _ := asMap(tu)
				tool = map[string]string{"toolUseId": pyGetStr(t, "toolUseId", ""), "name": pyGetStr(t, "name", ""), "input_json": ""}
				blocks[idx] = bedrockToolUseBlock(tool["toolUseId"], tool["name"], map[string]any{})
				if cb.OnToolStart != nil {
					if err := cb.OnToolStart(len(parts.toolCalls), tool["toolUseId"], tool["name"]); err != nil {
						return nil, false, err
					}
				}
			}
		} else if raw, has := event["contentBlockDelta"]; has {
			deltaEvent, _ := asMap(raw)
			idx := blockIndex(deltaEvent, false)
			current, hasCurrent = idx, true
			delta, _ := asMap(pyGetAny(deltaEvent, "delta", map[string]any{}))
			if rawText, has := delta["text"]; has {
				text := bedrockTextOf(rawText)
				block, ok := blocks[idx]
				if !ok {
					block = map[string]any{"text": ""}
					blocks[idx] = block
				}
				prev, _ := block["text"].(string)
				block["text"] = prev + text
				textBuffer = append(textBuffer, text)
				if cb.OnTextDelta != nil && !hasToolUse {
					if err := cb.OnTextDelta(text); err != nil {
						return nil, false, err
					}
				}
			} else if rawTool, has := delta["toolUse"]; has && tool != nil {
				t, _ := asMap(rawTool)
				tool["input_json"] += pyGetStr(t, "input", "")
			} else if rawReasoning, has := delta["reasoningContent"]; has {
				reasoning, ok := asMap(rawReasoning)
				encoded, _ := bedrockEncodeRedacted(reasoning["redactedContent"])
				if ok && (truthy(pyGetAny(reasoning, "text", "")) || truthy(reasoning["signature"]) || encoded != "") {
					block, ok := blocks[idx]
					if !ok {
						block = map[string]any{"reasoningContent": map[string]any{}}
						blocks[idx] = block
					}
					inner, ok := asMap(block["reasoningContent"])
					if !ok {
						inner = map[string]any{}
						block["reasoningContent"] = inner
					}
					if err := parts.absorbReasoning(reasoning, inner, cb.OnReasoningDelta); err != nil {
						return nil, false, err
					}
				}
			}
		} else if raw, has := event["contentBlockStop"]; has {
			stopEvent, _ := asMap(raw)
			idx := blockIndex(stopEvent, false)
			hasCurrent = false
			if tool != nil {
				input := bedrockParseToolArgs(tool["input_json"])
				call := bedrockToolCall(tool["toolUseId"], tool["name"], input)
				parts.toolCalls = append(parts.toolCalls, call)
				if block, ok := blocks[idx]; ok {
					if tu, ok := asMap(block["toolUse"]); ok {
						tu["input"] = input
					}
				}
				tool = nil
				if cb.OnToolDone != nil {
					if err := cb.OnToolDone(len(parts.toolCalls)-1, call); err != nil {
						return nil, false, err
					}
				}
			} else {
				flushText()
			}
		} else if raw, has := event["messageStop"]; has {
			stop, _ := asMap(raw)
			stopReason, hasStop = pyGetStr(stop, "stopReason", "end_turn"), true
		} else if raw, has := event["metadata"]; has {
			meta, _ := asMap(raw)
			metaUsage, _ := asMap(pyGetAny(meta, "usage", map[string]any{}))
			usage = map[string]any{}
			for _, k := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheWriteInputTokens"} {
				usage[k] = pyGetAny(metaUsage, k, 0)
			}
		}
	}
	if !hasStop && !interrupted {
		return nil, false, &EmptyStreamError{Message: "Bedrock Converse stream ended before messageStop; response is incomplete"}
	}
	flushText()
	keys := make([]int, 0, len(blocks))
	for k := range blocks {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	ordered := make([]any, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, blocks[k])
	}
	if !hasStop {
		stopReason = "end_turn"
	}
	return parts.build(ordered, usage, stopReason, ""), interrupted, nil
}

// Stream events consumed with no callbacks into the response shape
func normalizeConverseStreamEvents(next bedrockEventSource) (*bedrockConverseResult, error) {
	out, _, err := streamConverseWithCallbacks(next, bedrockStreamCallbacks{})
	return out, err
}

// Decoded ConverseStream frames as single key event maps, exception frames turned into stream errors
func bedrockConverseEvents(body io.Reader, headers http.Header) bedrockEventSource {
	reader := newAWSEventStreamReader(body)
	return func() (map[string]any, error) {
		for {
			m, err := reader.Next()
			if err != nil {
				return nil, err
			}
			if exc := awsEventStreamFrameException(m); exc != nil {
				return nil, exc.streamError(headers)
			}
			eventType := m.header(":event-type")
			if eventType == "" {
				continue
			}
			dec := json.NewDecoder(strings.NewReader(string(m.Payload)))
			dec.UseNumber()
			var payload map[string]any
			if err := dec.Decode(&payload); err != nil {
				return nil, providerStreamErrorFromNonJSON(string(m.Payload), headers)
			}
			return map[string]any{eventType: payload}, nil
		}
	}
}

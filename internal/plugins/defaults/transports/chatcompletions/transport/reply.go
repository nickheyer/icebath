package transport

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Whether a reply is a router's timeout sentinel sent as an answer
func shim(content string, calls int, usage map[string]any) bool {
	if strings.TrimSpace(content) != routerShim || calls > 0 {
		return false
	}
	return usageOf(usage, "") == nil || usageOf(usage, "").Output <= 0
}

// Reply of a chat completion payload, a streamed one put together or one that came whole
func (c *client) normalize(p *prepared, payload map[string]any, header http.Header) (*provider.Response, error) {
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return nil, p.refused.Empty("The provider sent back a reply with nothing in it. Send your message again.", nil)
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message = map[string]any{}
	}
	finish := finishOf(choice["finish_reason"])
	finish = cmp.Or(finish, "stop")
	var calls []*sessionsv1.ToolCall
	kept := carried{Calls: map[string]callCarried{}, Fields: map[string]string{}}
	list, _ := message["tool_calls"].([]any)
	for i, raw := range list {
		call, _ := raw.(map[string]any)
		function, _ := call["function"].(map[string]any)
		if function == nil || function["name"] == nil {
			continue
		}
		name, _ := function["name"].(string)
		name = p.unaliased(name)
		arguments := "{}"
		switch a := function["arguments"].(type) {
		case nil:
		case string:
			arguments = a
		default:
			encoded, _ := json.Marshal(a)
			arguments = string(encoded)
		}
		id := strings.TrimSpace(deltaID(call["id"]))
		id = cmp.Or(id, transport.CallID(name, arguments, i))
		calls = append(calls, &sessionsv1.ToolCall{Id: id, Name: name, Arguments: arguments})
		signature, _ := function["thought_signature"].(string)
		if extra := call["extra_content"]; extra != nil || signature != "" {
			kept.Calls[id] = callCarried{Signature: signature, Extra: extra}
		}
	}
	tier, _ := payload["service_tier"].(string)
	usage, _ := payload["usage"].(map[string]any)
	content := message["content"]
	if refusal, _ := message["refusal"].(string); strings.TrimSpace(refusal) != "" && strings.TrimSpace(flatten(content)) == "" && len(calls) == 0 {
		content = refusal
		if finish == "stop" {
			finish = "content_filter"
		}
	}
	text := flatten(content)
	if shim(text, len(calls), usage) {
		return nil, p.refused.Empty("A router in front of the provider gave up waiting and sent its own stand-in instead of a reply: "+routerShim+
			". Send your message again.", nil)
	}
	plain, _ := message[reasoningField].(string)
	if plain == "" {
		for _, key := range p.texts {
			if value, ok := message[key].(string); ok && value != "" {
				plain = value
				break
			}
		}
	}
	reasoning, _ := message[reasoningContent].(string)
	details, _ := message[reasoningDetails].([]any)
	for _, detail := range details {
		if _, ok := detail.(map[string]any); ok {
			kept.Details = append(kept.Details, detail)
		}
	}
	kept.Signature = thoughtSignature(message["extra_content"])
	for _, key := range slices.Concat(p.opaque, p.texts) {
		if value, ok := message[key].(string); ok && value != "" {
			kept.Fields[key] = value
		}
	}
	kept.Reasoning = cmp.Or(reasoning, plain)
	shown := extractReasoning(plain, reasoning, details, content)
	return &provider.Response{Message: c.message(p, text, shown, calls, kept), FinishReason: finish, Usage: usageOf(usage, tier), Header: header}, nil
}

// Reasoning a reply shows, each part once: its plain and reasoning_content fields, the text of its detail entries, else the thinking blocks
// of its content parts, else the reasoning tags in its text
func extractReasoning(plain, reasoning string, details []any, content any) string {
	var parts []string
	add := func(text string) {
		if text != "" && !slices.Contains(parts, text) {
			parts = append(parts, text)
		}
	}
	add(plain)
	add(reasoning)
	for _, d := range details {
		detail, ok := d.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"summary", "thinking", "content", "text"} {
			if !empty(detail[key]) {
				add(flatten(detail[key]))
				break
			}
		}
	}
	if list, ok := content.([]any); ok && len(parts) == 0 {
		for _, b := range list {
			block, ok := b.(map[string]any)
			if !ok || block["type"] != "thinking" {
				continue
			}
			value := block["thinking"]
			if empty(value) {
				value = block["text"]
			}
			add(strings.TrimSpace(flatten(value)))
		}
	}
	if text, ok := content.(string); ok && len(parts) == 0 {
		add(transport.InlineReasoning(text))
	}
	return strings.Join(parts, "\n\n")
}

// Stored message of a reply: the text the person was shown, the reasoning, the calls, and what the provider wants back on replay
func (c *client) message(p *prepared, content, reasoning string, calls []*sessionsv1.ToolCall, kept carried) *sessionsv1.Message {
	scrub := transport.NewScrubber()
	visible := strings.TrimLeft(scrub.Feed(content)+scrub.Flush(), "\n")
	message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: visible, Reasoning: reasoning, ToolCalls: calls}
	if visible != content {
		kept.Content = content
	}
	if kept.Reasoning == reasoning {
		kept.Reasoning = ""
	}
	if len(kept.Calls) == 0 {
		kept.Calls = nil
	}
	if len(kept.Fields) == 0 {
		kept.Fields = nil
	}
	if kept.Content != "" || kept.Reasoning != "" || len(kept.Details) > 0 || kept.Signature != "" || kept.Calls != nil || kept.Fields != nil {
		if raw, err := json.Marshal(kept); err == nil {
			message.Carriers = map[string][]byte{carrier: raw}
		}
	}
	return message
}

// Where each count of a usage report may be found, the first one there winning
var (
	promptCounts     = [][]string{{"prompt_tokens"}, {"input_tokens"}}
	outputCounts     = [][]string{{"completion_tokens"}, {"output_tokens"}}
	cacheReadCounts  = [][]string{{"prompt_tokens_details", "cached_tokens"}, {"cache_read_input_tokens"}, {"prompt_cache_hit_tokens"}, {"cached_tokens"}}
	cacheWriteCounts = [][]string{{"prompt_tokens_details", "cache_write_tokens"}, {"prompt_tokens_details", "cache_creation_input_tokens"},
		{"cache_creation_input_tokens"}, {"cache_write_tokens"}}
	reasoningCounts = [][]string{{"completion_tokens_details", "reasoning_tokens"}, {"output_tokens_details", "reasoning_tokens"}}
)

// Tokens a usage report gives, the input counted without its cached part; nil without a report
func usageOf(report map[string]any, tier string) *provider.Usage {
	if len(report) == 0 {
		return nil
	}
	count := func(paths [][]string) int64 {
		for _, path := range paths {
			var value any = report
			for _, key := range path {
				object, _ := value.(map[string]any)
				value = object[key]
			}
			if n, ok := value.(float64); ok && n != 0 {
				return int64(n)
			}
		}
		return 0
	}
	u := &provider.Usage{Output: count(outputCounts), CacheRead: count(cacheReadCounts), CacheWrite: count(cacheWriteCounts),
		Reasoning: count(reasoningCounts), Tier: strings.ToLower(tier)}
	u.Input = max(count(promptCounts)-u.CacheRead-u.CacheWrite, 0)
	return u
}

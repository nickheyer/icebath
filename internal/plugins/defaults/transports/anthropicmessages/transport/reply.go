package transport

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Stop reasons as finish reasons
var finishes = map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length", "stop_sequence": "stop",
	"refusal": "content_filter", "model_context_window_exceeded": "length"}

// Name a tool call came back under, as the agent knows the tool
func (p *prepared) inbound(name string) string {
	if p.subscription {
		name = fromWire(name, p.known)
	}
	if original, ok := p.aliases[name]; ok {
		return original
	}
	return name
}

// Reply of a Messages payload: its text, its thinking, its tool calls, and the blocks replayed with it. A payload with no content is
// refused unless it ended its turn or was a refusal
func (c *client) normalize(p *prepared, message map[string]any, header http.Header, status int) (*provider.Response, error) {
	content, ok := message["content"].([]any)
	stop := transport.Text(message["stop_reason"])
	if !ok || len(content) == 0 && stop != "end_turn" && stop != "refusal" {
		return nil, p.refused.Empty("Provider returned a message with no content blocks", nil)
	}
	// Blocks before the last fallback block are the refused model's, whose tool calls are not made
	edge := -1
	for i, b := range content {
		if kind(b) == "fallback" {
			edge = i
		}
	}
	var texts, thoughts []string
	var details, ordered []any
	var calls []*sessionsv1.ToolCall
	signed, used := false, false
	for i, b := range content {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		clean := sanitize(block)
		switch transport.Text(block["type"]) {
		case "text":
			texts = append(texts, transport.Text(block["text"]))
		case "thinking", "redacted_thinking":
			if block["type"] == "thinking" {
				thoughts = append(thoughts, transport.Text(block["thinking"]))
			}
			if clean != nil {
				details = append(details, clean)
				signed = signed || transport.Text(clean["signature"]) != "" || transport.Text(clean["data"]) != ""
			}
		case "tool_use":
			if i <= edge {
				clean = nil
				break
			}
			used = true
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			args, err := json.Marshal(input)
			if err != nil {
				args = []byte("{}")
			}
			calls = append(calls, &sessionsv1.ToolCall{Id: transport.Text(block["id"]), Name: p.inbound(transport.Text(block["name"])), Arguments: string(args)})
		}
		if clean != nil {
			ordered = append(ordered, clean)
		}
	}
	finish := "stop"
	if mapped, ok := finishes[stop]; ok {
		finish = mapped
	}
	if strings.EqualFold(transport.Text(message[guardrailAction]), "INTERVENED") {
		finish = "content_filter"
	}
	reply := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: strings.Join(texts, "\n"), Reasoning: strings.Join(thoughts, "\n\n"),
		ToolCalls: calls, FinishReason: finish}
	kept := carried{Details: details}
	if signed && used {
		kept.Blocks = ordered
	}
	if len(kept.Details) > 0 || len(kept.Blocks) > 0 {
		if raw, err := json.Marshal(kept); err == nil {
			reply.Carriers = map[string][]byte{carrier: raw}
		}
	}
	response := &provider.Response{Message: reply, FinishReason: finish, Header: header}
	if usage, ok := message["usage"].(map[string]any); ok {
		count := func(key string) int64 { n, _ := usage[key].(float64); return int64(n) }
		response.Usage = &provider.Usage{Input: count("input_tokens"), CacheRead: count("cache_read_input_tokens"),
			CacheWrite: count("cache_creation_input_tokens"), Output: count("output_tokens")}
		if usage["speed"] == "fast" {
			response.Usage.Tier = "fast"
		}
	}
	return response, nil
}

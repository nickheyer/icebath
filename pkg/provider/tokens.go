package provider

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/nickheyer/icebath/pkg/state"
)

// Rough chars per token ratio for ASCII text
const CharsPerToken = 4

// Flat per image token price before a route calibrates it from usage
const DefaultImageTokenCost = 1500

var cjkDenseRe = regexp.MustCompile("[ᄀ-ᇿ⺀-鿿ꥠ-꥿가-힯豈-﫿＀-￯]")

// Rough token estimate, CJK codepoints one token each and the rest ceil of UTF8 bytes over four
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii := true
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return (len(text) + 3) / CharsPerToken
	}
	dense := 0
	stripped := cjkDenseRe.ReplaceAllStringFunc(text, func(string) string {
		dense++
		return ""
	})
	return dense + (len(stripped)+3)/CharsPerToken
}

var imagePartTypes = map[string]bool{"image": true, "image_url": true, "input_image": true}

var persistenceOnlyFields = map[string]bool{
	"timestamp": true, "display_kind": true, "display_metadata": true, "message_uid": true,
	"absorbed_message_uids": true, "tool_call_uids": true, "tool_call_uid": true, "token_count": true,
}

// Replay carriers priced at zero, opaque signatures, duplicate reasoning copies and ordered sidecars
var unpricedCoreFields = []string{"_anthropic_content_blocks", "reasoning_details", "extra_content", "bedrock_content_blocks", "data_use"}

// Core unpriced carriers plus every message carrier key a registered profile declares
func unpricedReplayFields() map[string]bool {
	out := make(map[string]bool, len(unpricedCoreFields))
	for _, k := range unpricedCoreFields {
		out[k] = true
	}
	for _, p := range ListProviders(context.Background()) {
		if p.MessageCarrierOpaque != "" {
			out[p.MessageCarrierOpaque] = true
		}
		if p.MessageCarrierText != "" {
			out[p.MessageCarrierText] = true
		}
	}
	return out
}

// Message as a wire shadow map holding only what the provider receives, the live ordered sidecar included
func messageShadow(m *state.Message) (map[string]any, int) {
	raw, _ := json.Marshal(m)
	var row map[string]any
	_ = json.Unmarshal(raw, &row)
	if blocks, ok := anthropicLiveBlocksOf(m); ok {
		row["anthropic_content_blocks"] = blocks
	}
	return mapShadow(row)
}

// Shadow of a message row, with the count of image parts it carries
func mapShadow(row map[string]any) (map[string]any, int) {
	images := 0
	sidecar, _ := row["api_content"].(string)
	role, _ := row["role"].(string)
	sidecarWins := sidecar != "" && (role == "user" || role == "assistant")
	rc, _ := row["reasoning_content"].(string)
	dropReasoningDup := len(regexp.MustCompile(`\S`).FindString(rc)) > 0
	unpriced := unpricedReplayFields()
	shadow := map[string]any{}
	for k, v := range row {
		if unpriced[k] || persistenceOnlyFields[k] || (k == "reasoning" && dropReasoningDup) {
			continue
		}
		switch {
		case k == "api_content":
			if sidecarWins {
				shadow["content"] = v
			}
		case k == "content" && sidecarWins:
			continue
		case (k == "content" || k == "output") && isList(v):
			parts := v.([]any)
			out := make([]any, 0, len(parts))
			for _, p := range parts {
				pm, ok := p.(map[string]any)
				if ok && imagePartTypes[stringOf(pm["type"])] {
					images++
					out = append(out, map[string]any{"type": pm["type"], "image": "[stripped]"})
					continue
				}
				out = append(out, p)
			}
			shadow[k] = out
		case k == "codex_reasoning_items":
			shadow[k] = stripOpaqueReplayItems(v)
		case k == "encrypted_content":
			shadow[k] = ""
		default:
			shadow[k] = v
		}
	}
	return shadow, images
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Replay items with their ciphertext blanked so bytes never price what the provider counts itself
func stripOpaqueReplayItems(items any) any {
	list, ok := items.([]any)
	if !ok {
		return items
	}
	out := make([]any, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		c := make(map[string]any, len(m))
		for k, v := range m {
			if k == "encrypted_content" {
				c[k] = ""
			} else {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

// Tokens of one message, text estimated on its shadow and images priced flat
func estimateMessageTokens(m *state.Message, imageCost int) int {
	shadow, images := messageShadow(m)
	b, _ := json.Marshal(shadow)
	return EstimateTokens(string(b)) + images*imageCost
}

// Messages with stale reasoning text removed from every assistant row but the newest
func stripStaleThinking(msgs []*state.Message) []*state.Message {
	newest := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			newest = i
			break
		}
	}
	out := make([]*state.Message, len(msgs))
	for i, m := range msgs {
		if i != newest && m.Role == "assistant" && (m.Reasoning != "" || m.ReasoningContent != "") {
			c := *m
			c.Reasoning, c.ReasoningContent = "", ""
			out[i] = &c
			continue
		}
		out[i] = m
	}
	return out
}

// Rough token estimate of a message list for preflight decisions
func EstimateMessagesTokens(msgs []*state.Message, imageCost int, chargeStaleThinking bool) int {
	if !chargeStaleThinking {
		msgs = stripStaleThinking(msgs)
	}
	total := 0
	for _, m := range msgs {
		total += estimateMessageTokens(m, imageCost)
	}
	return total
}

// Rough token estimate of tool schemas from their names, descriptions and parameters
func EstimateToolsTokens(tools []map[string]any) int {
	chars := 0
	for _, tool := range tools {
		src := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			src = fn
		}
		chars += len(stringOf(src["name"])) + len(stringOf(src["description"]))
		params := src["parameters"]
		if params == nil {
			params = map[string]any{}
		}
		b, err := json.Marshal(params)
		if err != nil {
			chars += len(stringOf(params))
			continue
		}
		chars += len(b)
	}
	return (chars + 3) / 4
}

// Rough token estimate of a whole request, system prompt plus messages plus tool schemas
func EstimateRequestTokens(msgs []*state.Message, system string, tools []map[string]any, imageCost int, chargeStaleThinking bool) int {
	total := 0
	if system != "" {
		total = EstimateTokens(system)
	}
	total += EstimateMessagesTokens(msgs, imageCost, chargeStaleThinking)
	if len(tools) > 0 {
		total += EstimateToolsTokens(tools)
	}
	return total
}

// Char equivalent of a wire value with image parts priced at the learned per image cost
func payloadChars(v any, imageCost int) int {
	switch c := v.(type) {
	case nil:
		return 0
	case map[string]any:
		if t, ok := c["type"].(string); ok && imagePartTypes[t] {
			for _, k := range []string{"image_url", "image", "source", "file_id"} {
				if _, has := c[k]; has {
					return imageCost*4 + len(stringOf(c["text"]))
				}
			}
		}
		sum := 0
		for k, val := range c {
			sum += len(k) + 6 + payloadChars(val, imageCost)
		}
		return sum
	case []any:
		sum := 2 * len(c)
		for _, it := range c {
			sum += payloadChars(it, imageCost)
		}
		return sum
	case string:
		return len(c)
	default:
		b, _ := json.Marshal(c)
		return len(b)
	}
}

// Cheap context estimate of a wire body for the stale call detectors
func estimateBodyContextTokens(body map[string]any, imageCost int) int {
	if msgs, ok := body["messages"].([]any); ok {
		total := 0
		for _, m := range msgs {
			total += payloadChars(m, imageCost)
		}
		if tools, has := body["tools"]; has {
			total += payloadChars(tools, imageCost)
		}
		return total / 4
	}
	if _, has := body["input"]; has {
		return (payloadChars(body["input"], imageCost) + payloadChars(body["instructions"], imageCost) + payloadChars(body["tools"], imageCost)) / 4
	}
	total := 0
	for _, v := range body {
		total += payloadChars(v, imageCost)
	}
	return total / 4
}

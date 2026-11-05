package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf16"
)

// ---- tool use and prompt cache support ----

// Models known to reject toolConfig, unknown models are assumed to take tools
var bedrockNonToolCallingPatterns = []string{"deepseek.r1", "deepseek-r1", "stability.", "cohere.embed", "amazon.titan-embed"}

// Families accepting cachePoint markers, unknown models get none
var bedrockCachePointPatterns = []string{"anthropic.claude", "amazon.nova"}

// Grok on Bedrock rejects temperature and topP
var bedrockXAIGrokNoSamplingRe = regexp.MustCompile(`(?i)^(?:[a-z]+\.)?xai\.grok`)

// Reports a model outside the tool calling denylist
func bedrockModelSupportsToolUse(model string) bool {
	lowered := strings.ToLower(model)
	for _, p := range bedrockNonToolCallingPatterns {
		if strings.Contains(lowered, p) {
			return false
		}
	}
	return true
}

// Reports a cachePoint capable family, an application profile matched on the model it wraps
func bedrockModelSupportsPromptCache(model string, resolve func(string) string) bool {
	if bedrockApplicationProfileRe.MatchString(model) {
		model = bedrockResolveIssuerARN(model, resolve)
	}
	lowered := strings.ToLower(model)
	for _, p := range bedrockCachePointPatterns {
		if strings.Contains(lowered, p) {
			return true
		}
	}
	return false
}

// ---- server verdict cachePoint suppression ----

// BedrockCachePointPlacements lists the Converse sections a cachePoint marker can sit in
var BedrockCachePointPlacements = []string{"tools", "system", "messages"}

var (
	bedrockCachePointMu         sync.Mutex
	bedrockCachePointRejections = map[string]map[string]bool{}
)

var bedrockCachePointPathRe = regexp.MustCompile(`(?i)#/([A-Za-z0-9_./\[\]-]*)`)

// Fresh cachePoint marker block
func bedrockCachePoint() map[string]any {
	return map[string]any{"cachePoint": map[string]any{"type": "default"}}
}

// Converse section whose cachePoint Bedrock refused, tools when the pointer names none, empty when unrelated
func bedrockCachePointRejectionPlacement(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	lowered := strings.ToLower(msg)
	if !strings.Contains(lowered, "cachepoint") || (!strings.Contains(lowered, "not permitted") && !strings.Contains(lowered, "extraneous")) {
		return ""
	}
	path := ""
	if m := bedrockCachePointPathRe.FindStringSubmatch(msg); m != nil {
		path = strings.ToLower(m[1])
	}
	if strings.Contains(path, "toolconfig") || strings.Contains(path, "tools") {
		return "tools"
	}
	for _, p := range []string{"system", "messages"} {
		if strings.Contains(path, p) {
			return p
		}
	}
	return "tools"
}

// Records that a model refuses cachePoint blocks in a placement for the rest of the process
func noteBedrockCachePointRejection(model, placement string) {
	if !containsString(BedrockCachePointPlacements, placement) {
		return
	}
	bedrockCachePointMu.Lock()
	defer bedrockCachePointMu.Unlock()
	key := strings.ToLower(model)
	if bedrockCachePointRejections[key] == nil {
		bedrockCachePointRejections[key] = map[string]bool{}
	}
	bedrockCachePointRejections[key][placement] = true
}

// False once Bedrock has refused the placement for the model
func bedrockCachePointAllowed(model, placement string) bool {
	bedrockCachePointMu.Lock()
	defer bedrockCachePointMu.Unlock()
	return !bedrockCachePointRejections[strings.ToLower(model)][placement]
}

// ResetBedrockCachePointRejections forgets every recorded cachePoint verdict
func ResetBedrockCachePointRejections() {
	bedrockCachePointMu.Lock()
	defer bedrockCachePointMu.Unlock()
	bedrockCachePointRejections = map[string]map[string]bool{}
}

// Blocks minus bare cachePoint entries, false when not a list or nothing was removed
func bedrockWithoutCachePoints(blocks any) ([]any, bool) {
	list, ok := asList(blocks)
	if !ok {
		return nil, false
	}
	cleaned := make([]any, 0, len(list))
	for _, b := range list {
		if m, ok := asMap(b); ok && len(m) == 1 {
			if _, has := m["cachePoint"]; has {
				continue
			}
		}
		cleaned = append(cleaned, b)
	}
	return cleaned, len(cleaned) != len(list)
}

// Copy of the kwargs with the placement's cachePoint removed, false when nothing was stripped
func stripBedrockCachePoints(kwargs map[string]any, placement string) (map[string]any, bool) {
	switch placement {
	case "messages":
		messages, ok := asList(kwargs["messages"])
		if !ok {
			return kwargs, false
		}
		out := make([]any, len(messages))
		changed := false
		for i, msg := range messages {
			out[i] = msg
			m, ok := asMap(msg)
			if !ok {
				continue
			}
			if content, stripped := bedrockWithoutCachePoints(m["content"]); stripped {
				out[i], changed = bedrockWithKey(m, "content", content), true
			}
		}
		if !changed {
			return kwargs, false
		}
		return bedrockWithKey(kwargs, "messages", out), true
	case "system":
		if cleaned, stripped := bedrockWithoutCachePoints(kwargs["system"]); stripped {
			return bedrockWithKey(kwargs, "system", cleaned), true
		}
	case "tools":
		toolConfig, _ := asMap(kwargs["toolConfig"])
		if cleaned, stripped := bedrockWithoutCachePoints(toolConfig["tools"]); stripped {
			return bedrockWithKey(kwargs, "toolConfig", bedrockWithKey(toolConfig, "tools", cleaned)), true
		}
	}
	return kwargs, false
}

// Shallow copy of a map with one key replaced
func bedrockWithKey(m map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

// Records the cachePoint verdict and returns retry kwargs, false when unrelated or the marker was already absent
func recoverFromBedrockCachePointRejection(err error, kwargs map[string]any) (map[string]any, bool) {
	placement := bedrockCachePointRejectionPlacement(err)
	if placement == "" {
		return nil, false
	}
	retry, changed := stripBedrockCachePoints(kwargs, placement)
	if !changed {
		return nil, false
	}
	model := bedrockKwargsModel(kwargs)
	noteBedrockCachePointRejection(model, placement)
	bedrockLog.Warn(fmt.Sprintf("bedrock: %s rejected a cachePoint block in %s — dropping that cache marker for this model and "+
		"retrying. Prompt caching stays active for the remaining sections.", bedrockOr(model, "model"), placement))
	return retry, true
}

// Value unless empty, else the default
func bedrockOr(value, def string) string {
	if value == "" {
		return def
	}
	return value
}

// Model id carried by Converse kwargs, Python str of a non string value
func bedrockKwargsModel(kwargs map[string]any) string {
	v, has := kwargs["modelId"]
	if !has {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyStr(v)
}

// ---- encrypted content redacted reasoning suppression ----

var bedrockRedactedRejectionRe = regexp.MustCompile(`(?is)ValidationException.*(?:redacted|encrypt)`)

// Blocks minus reasoningContent entries carrying redactedContent, false when not a list or nothing was removed
func bedrockWithoutRedactedReasoning(blocks any) ([]any, bool) {
	list, ok := asList(blocks)
	if !ok {
		return nil, false
	}
	cleaned := make([]any, 0, len(list))
	for _, b := range list {
		if m, ok := asMap(b); ok {
			if rc, ok := asMap(m["reasoningContent"]); ok {
				if _, has := rc["redactedContent"]; has {
					continue
				}
			}
		}
		cleaned = append(cleaned, b)
	}
	return cleaned, len(cleaned) != len(list)
}

// Copy of the kwargs without redacted reasoning blocks and the turns they emptied, false when nothing was stripped
func stripBedrockRedactedReasoning(kwargs map[string]any) (map[string]any, bool) {
	messages, ok := asList(kwargs["messages"])
	if !ok {
		return kwargs, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	for _, msg := range messages {
		m, ok := asMap(msg)
		if !ok {
			out = append(out, msg)
			continue
		}
		content, stripped := bedrockWithoutRedactedReasoning(m["content"])
		if !stripped {
			out = append(out, msg)
			continue
		}
		changed = true
		if len(content) > 0 {
			out = append(out, bedrockWithKey(m, "content", content))
		}
	}
	if !changed {
		return kwargs, false
	}
	return bedrockWithKey(kwargs, "messages", out), true
}

// Retry kwargs without redacted reasoning after an encrypted content rejection, false when unrelated or nothing remained
func recoverFromBedrockRedactedReasoningRejection(err error, kwargs map[string]any) (map[string]any, bool) {
	if err == nil || !bedrockRedactedRejectionRe.MatchString(err.Error()) {
		return nil, false
	}
	retry, changed := stripBedrockRedactedReasoning(kwargs)
	if !changed {
		return nil, false
	}
	bedrockLog.Warn(fmt.Sprintf("bedrock: %s rejected replayed redacted reasoning (encrypted content is sealed to the issuing "+
		"model/flow) - stripping redacted blocks and resending once.", bedrockOr(bedrockKwargsModel(kwargs), "model")))
	return retry, true
}

// ---- OpenAI to Converse conversion ----

// OpenAI function tool definitions as Converse toolSpec entries
func convertToolsToConverse(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		fn, has := t["function"]
		if !has {
			fn = map[string]any{}
		}
		f, _ := asMap(fn)
		name, has := f["name"]
		if !has {
			name = ""
		}
		description, has := f["description"]
		if !has {
			description = ""
		}
		params, has := f["parameters"]
		if !has {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"toolSpec": map[string]any{"name": name, "description": description,
			"inputSchema": map[string]any{"json": params}}})
	}
	return out
}

const bedrockEmptyTextPlaceholder = "(empty)"

func bedrockPlaceholderBlock() map[string]any {
	return map[string]any{"text": bedrockEmptyTextPlaceholder}
}

// Text when it has non whitespace content, else the placeholder Converse accepts
func bedrockSafeText(v any) string {
	text := ""
	if v != nil {
		text = pyStr(v)
	}
	if strings.TrimSpace(text) == "" {
		return bedrockEmptyTextPlaceholder
	}
	return text
}

// Python b64decode without validation, characters outside the alphabet discarded
func bedrockLenientB64Decode(data string) ([]byte, error) {
	var b strings.Builder
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			b.WriteByte(c)
		}
	}
	return base64.StdEncoding.DecodeString(b.String())
}

// Data URL as a Converse image block holding the raw decoded bytes
func bedrockImageBlockFromDataURL(url string) map[string]any {
	header, data, _ := strings.Cut(url, ",")
	mediaType := ""
	if strings.HasPrefix(header, "data:") {
		mediaType, _, _ = strings.Cut(header[5:], ";")
	}
	if mediaType == "" {
		mediaType = "image/jpeg"
	}
	raw, err := bedrockLenientB64Decode(data)
	if err != nil {
		raw = []byte(data)
	}
	format := "jpeg"
	if strings.Contains(mediaType, "/") {
		format = mediaType[strings.LastIndex(mediaType, "/")+1:]
	}
	return map[string]any{"image": map[string]any{"format": format, "source": map[string]any{"bytes": raw}}}
}

// OpenAI content as Converse blocks, blank text placeholder filled and remote images as text references
func convertContentToConverse(content any) []any {
	list, ok := asList(content)
	if !ok {
		return []any{map[string]any{"text": bedrockSafeText(content)}}
	}
	var blocks []any
	for _, part := range list {
		if s, ok := part.(string); ok {
			blocks = append(blocks, map[string]any{"text": bedrockSafeText(s)})
			continue
		}
		p, ok := asMap(part)
		if !ok {
			continue
		}
		switch pyGetStr(p, "type", "") {
		case "text":
			text, has := p["text"]
			if !has {
				text = ""
			}
			blocks = append(blocks, map[string]any{"text": bedrockSafeText(text)})
		case "image_url":
			url := ""
			if iu, ok := asMap(p["image_url"]); ok {
				url = pyGetStr(iu, "url", "")
			}
			if strings.HasPrefix(url, "data:") {
				blocks = append(blocks, bedrockImageBlockFromDataURL(url))
			} else {
				blocks = append(blocks, map[string]any{"text": "[Image: " + url + "]"})
			}
		}
	}
	if len(blocks) == 0 {
		return []any{bedrockPlaceholderBlock()}
	}
	return blocks
}

// String value of a key with a default, empty for a present non string value
func pyGetStr(m map[string]any, key, def string) string {
	v, has := m[key]
	if !has {
		return def
	}
	s, _ := v.(string)
	return s
}

// System content as text blocks with blank parts dropped
func bedrockSystemBlocks(content any) []any {
	var parts []any
	if s, ok := content.(string); ok {
		parts = []any{s}
	} else if l, ok := asList(content); ok {
		parts = l
	}
	var out []any
	for _, part := range parts {
		text := part
		if m, ok := asMap(part); ok && m["type"] == "text" {
			text, ok = m["text"]
			if !ok {
				text = ""
			}
		}
		if s, ok := text.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, map[string]any{"text": s})
		}
	}
	return out
}

func bedrockToolUseBlock(id, name, input any) map[string]any {
	return map[string]any{"toolUse": map[string]any{"toolUseId": id, "name": name, "input": input}}
}

// Strict base64 decode, false for empty, non string or undecodable input
func bedrockDecodeRedacted(encoded any) ([]byte, bool) {
	s, ok := encoded.(string)
	if !ok || s == "" || strings.ContainsAny(s, "\r\n") {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// Exact Converse block sequence captured at normalization, undecodable redacted entries skipped
func bedrockReplayOrderedBlocks(ordered []any) []any {
	var out []any
	for _, item := range ordered {
		block, ok := asMap(item)
		if !ok {
			continue
		}
		if text, ok := block["text"].(string); ok {
			out = append(out, map[string]any{"text": text})
			continue
		}
		if _, has := block["text"]; has {
			continue
		}
		if raw, has := block["reasoningContent"]; has {
			reasoning, ok := asMap(raw)
			if !ok {
				continue
			}
			if text, ok := reasoning["text"].(string); ok {
				rt := map[string]any{"text": text}
				if sig, ok := reasoning["signature"].(string); ok && sig != "" {
					rt["signature"] = sig
				}
				out = append(out, map[string]any{"reasoningContent": map[string]any{"reasoningText": rt}})
			}
			if encoded, ok := reasoning["redactedContentBase64"].(string); ok && encoded != "" {
				if redacted, ok := bedrockDecodeRedacted(encoded); ok {
					out = append(out, map[string]any{"reasoningContent": map[string]any{"redactedContent": redacted}})
				}
			}
			continue
		}
		if tu, ok := asMap(block["toolUse"]); ok {
			out = append(out, bedrockToolUseBlock(pyGetAny(tu, "toolUseId", ""), pyGetAny(tu, "name", ""), pyGetAny(tu, "input", map[string]any{})))
		}
	}
	return out
}

// Value of a key with a default when absent
func pyGetAny(m map[string]any, key string, def any) any {
	if v, has := m[key]; has {
		return v
	}
	return def
}

// JSON decoded tool arguments, an empty object when undecodable, non strings passed through
func bedrockParseToolArgs(args any) any {
	s, ok := args.(string)
	if !ok {
		return args
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil || dec.More() {
		return map[string]any{}
	}
	if _, err := dec.Token(); err == nil {
		return map[string]any{}
	}
	return out
}

func bedrockRedactedReasoningBlock(encoded any) map[string]any {
	redacted, ok := bedrockDecodeRedacted(encoded)
	if !ok {
		return nil
	}
	return map[string]any{"reasoningContent": map[string]any{"redactedContent": redacted}}
}

func bedrockReasoningTextBlock(text, signature any) map[string]any {
	s, _ := text.(string)
	rt := map[string]any{"text": s}
	if sig, ok := signature.(string); ok && sig != "" {
		rt["signature"] = sig
	}
	return map[string]any{"reasoningContent": map[string]any{"reasoningText": rt}}
}

// Tool call arguments of an assistant message keyed by call id
func bedrockToolCallArgs(msg map[string]any) map[string]any {
	args := map[string]any{}
	calls, _ := asList(msg["tool_calls"])
	for _, c := range calls {
		tc, ok := asMap(c)
		if !ok {
			continue
		}
		fn, _ := asMap(tc["function"])
		raw, has := fn["arguments"]
		if !has {
			raw = "{}"
		}
		args[pyStr(tc["id"])] = bedrockParseToolArgs(raw)
	}
	return args
}

// Anthropic Messages ordered blocks of a turn as Converse blocks, tool input taken from the tool calls
func bedrockAnthropicOrderedBlocks(msg map[string]any) []any {
	args := bedrockToolCallArgs(msg)
	blocks, _ := asList(msg["anthropic_content_blocks"])
	var out []any
	for _, item := range blocks {
		b, ok := asMap(item)
		if !ok {
			continue
		}
		var converted map[string]any
		switch b["type"] {
		case "thinking":
			converted = bedrockReasoningTextBlock(b["thinking"], b["signature"])
		case "redacted_thinking":
			converted = bedrockRedactedReasoningBlock(b["data"])
		case "text":
			if text, ok := b["text"].(string); ok && strings.TrimSpace(text) != "" {
				converted = map[string]any{"text": text}
			}
		case "tool_use":
			input, has := args[pyStr(b["id"])]
			if !has {
				input = pyGetAny(b, "input", map[string]any{})
			}
			converted = bedrockToolUseBlock(pyGetAny(b, "id", ""), pyGetAny(b, "name", ""), input)
		}
		if converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

// Thinking from persisted reasoning details in stored order, signed for Claude and readable text for other models
func bedrockReasoningDetailsBlocks(msg map[string]any, claude, own bool) []any {
	var blocks []any
	details, _ := asList(msg["reasoning_details"])
	for _, item := range details {
		d, ok := asMap(item)
		if !ok {
			continue
		}
		switch d["type"] {
		case "thinking":
			thinking, ok := d["thinking"].(string)
			if !ok || strings.TrimSpace(thinking) == "" {
				continue
			}
			if (claude || own) && truthy(d["signature"]) {
				blocks = append(blocks, bedrockReasoningTextBlock(thinking, d["signature"]))
			} else if !claude {
				blocks = append(blocks, bedrockReasoningTextBlock(thinking, nil))
			}
		case "redacted_thinking":
			encoded := d["data"]
			if !truthy(encoded) {
				encoded = d["redactedContentBase64"]
			}
			if block := bedrockRedactedReasoningBlock(encoded); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	if text, ok := msg["reasoning_content"].(string); ok && len(blocks) == 0 && !claude && strings.TrimSpace(text) != "" {
		blocks = append(blocks, bedrockReasoningTextBlock(text, nil))
	}
	return blocks
}

// Assistant message as Converse blocks from the first carrier that yields any
func bedrockAssistantBlocks(msg map[string]any, content any, claude, own bool) []any {
	if ordered, ok := asList(msg["bedrock_content_blocks"]); ok {
		if blocks := bedrockReplayOrderedBlocks(ordered); len(blocks) > 0 {
			return blocks
		}
	}
	if claude {
		if blocks := bedrockAnthropicOrderedBlocks(msg); len(blocks) > 0 {
			return blocks
		}
	}
	blocks := bedrockReasoningDetailsBlocks(msg, claude, own)
	if s, ok := content.(string); ok {
		if strings.TrimSpace(s) != "" {
			blocks = append(blocks, map[string]any{"text": s})
		}
	} else if _, ok := asList(content); ok {
		blocks = append(blocks, convertContentToConverse(content)...)
	}
	calls, _ := asList(msg["tool_calls"])
	for _, c := range calls {
		tc, ok := asMap(c)
		if !ok {
			continue
		}
		fn, has := tc["function"]
		if !has {
			fn = map[string]any{}
		}
		f, _ := asMap(fn)
		args, has := f["arguments"]
		if !has {
			args = "{}"
		}
		blocks = append(blocks, bedrockToolUseBlock(pyGetAny(tc, "id", ""), pyGetAny(f, "name", ""), bedrockParseToolArgs(args)))
	}
	return blocks
}

// ---- per model reasoning replay ----

type bedrockReasoningKeep func(block map[string]any) map[string]any

// Readable reasoning text only, nothing for a signature or redacted only block
func bedrockUnsignedReasoning(block map[string]any) map[string]any {
	reasoning, ok := asMap(block["reasoningContent"])
	if !ok {
		return block
	}
	rt, _ := asMap(reasoning["reasoningText"])
	if text, ok := rt["text"].(string); ok && strings.TrimSpace(text) != "" {
		return map[string]any{"reasoningContent": map[string]any{"reasoningText": map[string]any{"text": text}}}
	}
	return nil
}

func bedrockVerbatim(block map[string]any) map[string]any { return block }

func bedrockWithoutReasoning(block map[string]any) map[string]any {
	if _, has := block["reasoningContent"]; has {
		return nil
	}
	return block
}

// Policy to the filters for turns before the in flight tool loop and inside it
var bedrockReasoningFilters = map[string][2]bedrockReasoningKeep{
	"signed":   {bedrockVerbatim, bedrockVerbatim},
	"strip":    {bedrockWithoutReasoning, bedrockVerbatim},
	"unsigned": {bedrockUnsignedReasoning, bedrockVerbatim},
	"issuer":   {bedrockWithoutReasoning, bedrockWithoutReasoning},
}

func bedrockFilterReasoning(blocks []any, keep bedrockReasoningKeep) []any {
	var out []any
	for _, b := range blocks {
		m, ok := asMap(b)
		if !ok {
			out = append(out, b)
			continue
		}
		if kept := keep(m); kept != nil {
			out = append(out, kept)
		}
	}
	return out
}

// Reasoning one assistant turn may replay to the answering model, a foreign stamped turn losing the in loop replay
func bedrockReasoningFilter(policy, issuer string, hasIssuer, own, inLoop bool, resolve func(string) string) bedrockReasoningKeep {
	filters := bedrockReasoningFilters[policy]
	prior, loop := filters[0], filters[1]
	if own {
		if inLoop || policy == "unsigned" || policy == "issuer" {
			return bedrockVerbatim
		}
		return prior
	}
	if hasIssuer {
		if policy == "signed" && bedrockMintedOutsideClaude(issuer, true, resolve) {
			return bedrockWithoutReasoning
		}
		return prior
	}
	if inLoop {
		return loop
	}
	return prior
}

// OpenAI messages as Converse system blocks and strictly alternating turns, no model replaying every carrier verbatim
func convertMessagesToConverse(messages []map[string]any, resolve func(string) string, model ...string) ([]any, []any) {
	hasModel := len(model) > 0
	policy := "signed"
	if hasModel {
		policy = bedrockConverseReplayPolicy(model[0], resolve)
	}
	loopStart := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" {
			loopStart = i
			break
		}
	}
	var system []any
	var turns []any
	appendTurn := func(role string, blocks []any) {
		if n := len(turns); n > 0 {
			last := turns[n-1].(map[string]any)
			if last["role"] == role {
				last["content"] = append(last["content"].([]any), blocks...)
				return
			}
		}
		turns = append(turns, map[string]any{"role": role, "content": blocks})
	}
	for idx, msg := range messages {
		role, _ := msg["role"].(string)
		content := msg["content"]
		switch role {
		case "system":
			system = append(system, bedrockSystemBlocks(content)...)
		case "tool":
			result, ok := content.(string)
			if !ok {
				result = bedrockPyDumps(content)
			}
			appendTurn("user", []any{map[string]any{"toolResult": map[string]any{
				"toolUseId": pyGetAny(msg, "tool_call_id", ""), "content": []any{map[string]any{"text": bedrockSafeText(result)}}}}})
		case "assistant":
			issuer, hasIssuer := "", false
			if hasModel {
				issuer, hasIssuer = bedrockIssuer(msg)
			}
			own := hasIssuer && issuer == model[0]
			keep := bedrockReasoningFilter(policy, issuer, hasIssuer, own, idx >= loopStart, resolve)
			blocks := bedrockFilterReasoning(bedrockAssistantBlocks(msg, content, policy == "signed", own), keep)
			if len(blocks) == 0 {
				blocks = []any{bedrockPlaceholderBlock()}
			}
			appendTurn("assistant", blocks)
		case "user":
			appendTurn("user", convertContentToConverse(content))
		}
	}
	if len(turns) > 0 && turns[0].(map[string]any)["role"] != "user" {
		turns = append([]any{map[string]any{"role": "user", "content": []any{bedrockPlaceholderBlock()}}}, turns...)
	}
	if len(turns) > 0 && turns[len(turns)-1].(map[string]any)["role"] != "user" {
		turns = append(turns, map[string]any{"role": "user", "content": []any{bedrockPlaceholderBlock()}})
	}
	if turns == nil {
		turns = []any{}
	}
	return system, turns
}

// Python json.dumps text with default separators and ASCII escapes, map keys sorted
func bedrockPyDumps(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return pyStr(v)
	}
	compact := bytes.TrimRight(buf.Bytes(), "\n")
	var b strings.Builder
	inString, escaped := false, false
	for _, r := range string(compact) {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
				b.WriteRune(r)
			case r == '\\':
				escaped = true
				b.WriteRune(r)
			case r == '"':
				inString = false
				b.WriteRune(r)
			case r > 0x7e:
				if r > 0xffff {
					hi, lo := utf16.EncodeRune(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		case r == '"':
			inString = true
			b.WriteRune(r)
		case r == ':' || r == ',':
			b.WriteRune(r)
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---- reasoning switches and kwargs ----

// Claude thinking carried in additionalModelRequestFields with the inference settings manual budgets need
func bedrockClaudeConverseReasoning(model string, cfg *anthReasoningConfig, maxTokens *int) (map[string]any, map[string]any) {
	effective := 0
	if maxTokens != nil {
		effective = *maxTokens
	}
	thinking := anthThinkingKwargs(cfg, model, effective)
	fields := map[string]any{}
	for _, k := range []string{"thinking", "output_config"} {
		if v, has := thinking[k]; has {
			fields[k] = v
		}
	}
	inference := map[string]any{}
	if t, ok := asMap(fields["thinking"]); ok && t["type"] == "enabled" {
		if !strings.Contains(strings.ToLower(model), "claude-3") {
			fields["anthropic_beta"] = []any{"interleaved-thinking-2025-05-14"}
		}
		inference["temperature"] = pyGetAny(thinking, "temperature", 1)
		if maxTokens != nil {
			inference["maxTokens"] = pyGetAny(thinking, "max_tokens", *maxTokens)
		}
	}
	return fields, inference
}

var bedrockNovaEffort = map[string]string{"minimal": "low", "low": "low", "medium": "medium"}

// Nova 2 reasoningConfig, unsetting temperature topP and maxTokens at high effort
func bedrockNovaConverseReasoning(_ string, cfg *anthReasoningConfig, _ *int) (map[string]any, map[string]any) {
	if cfg.enabled != nil && !*cfg.enabled {
		return map[string]any{}, map[string]any{}
	}
	requested := cfg.effort
	if requested == "" {
		requested = "medium"
	}
	effort, ok := bedrockNovaEffort[strings.ToLower(requested)]
	if !ok {
		effort = "high"
	}
	fields := map[string]any{"reasoningConfig": map[string]any{"type": "enabled", "maxReasoningEffort": effort}}
	if effort == "high" {
		return fields, map[string]any{"temperature": nil, "topP": nil, "maxTokens": nil}
	}
	return fields, map[string]any{}
}

type bedrockReasoningBuilder func(model string, cfg *anthReasoningConfig, maxTokens *int) (map[string]any, map[string]any)

var bedrockConverseReasoningBuilders = []struct {
	matches func(string) bool
	build   bedrockReasoningBuilder
}{
	{isAnthropicBedrockModel, bedrockClaudeConverseReasoning},
	{func(model string) bool { return strings.Contains(strings.ToLower(model), "amazon.nova-2-lite") }, bedrockNovaConverseReasoning},
}

// Applies the model's reasoning switch to the kwargs and its inference settings, nil values unsetting keys
func applyBedrockConverseReasoning(kwargs, inference map[string]any, model string, cfg *anthReasoningConfig, maxTokens *int) {
	for _, b := range bedrockConverseReasoningBuilders {
		if !b.matches(model) {
			continue
		}
		fields, settings := b.build(model, cfg, maxTokens)
		if len(fields) > 0 {
			kwargs["additionalModelRequestFields"] = fields
		}
		for k, v := range settings {
			if v == nil {
				delete(inference, k)
			} else {
				inference[k] = v
			}
		}
		return
	}
}

// Inputs of one Converse call
type bedrockConverseInput struct {
	Model           string
	Messages        []map[string]any
	Tools           []map[string]any
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
	StopSequences   []string
	GuardrailConfig map[string]any
	Reasoning       *anthReasoningConfig
	NoPromptCache   bool
	Resolve         func(string) string
}

// Default output limit of a Converse call when the caller sets none
const bedrockDefaultMaxTokens = 4096

// Converse and ConverseStream kwargs with cachePoint markers on system, tools and the second newest message
func buildBedrockConverseKwargs(in bedrockConverseInput) map[string]any {
	model := in.Model
	system, turns := convertMessagesToConverse(in.Messages, in.Resolve, model)
	cacheAt := map[string]bool{}
	if !in.NoPromptCache && bedrockModelSupportsPromptCache(model, in.Resolve) {
		for _, p := range BedrockCachePointPlacements {
			if bedrockCachePointAllowed(model, p) {
				cacheAt[p] = true
			}
		}
	}
	inference := map[string]any{}
	if in.MaxTokens != nil {
		inference["maxTokens"] = *in.MaxTokens
	}
	kwargs := map[string]any{"modelId": model, "messages": turns, "inferenceConfig": inference}
	if len(system) > 0 {
		if cacheAt["system"] {
			system = append(system, bedrockCachePoint())
		}
		kwargs["system"] = system
	}
	if !anthForbidsSamplingParams(model) && !bedrockXAIGrokNoSamplingRe.MatchString(model) {
		if in.Temperature != nil {
			inference["temperature"] = *in.Temperature
		}
		if in.TopP != nil {
			inference["topP"] = *in.TopP
		}
	}
	if len(in.StopSequences) > 0 {
		stops := make([]any, 0, len(in.StopSequences))
		for _, s := range in.StopSequences {
			stops = append(stops, s)
		}
		inference["stopSequences"] = stops
	}
	var tools []any
	if len(in.Tools) > 0 {
		tools = convertToolsToConverse(in.Tools)
	}
	if len(tools) > 0 && !bedrockModelSupportsToolUse(model) {
		bedrockLog.Warn(fmt.Sprintf("Model %s does not support tool calling — tools stripped. The agent will operate in text-only mode.", model))
	} else if len(tools) > 0 {
		if cacheAt["tools"] {
			tools = append(tools, bedrockCachePoint())
		}
		kwargs["toolConfig"] = map[string]any{"tools": tools}
	}
	if cacheAt["messages"] && len(turns) >= 2 {
		msg := turns[len(turns)-2].(map[string]any)
		if content, ok := msg["content"].([]any); ok && len(content) > 0 {
			msg["content"] = append(content, bedrockCachePoint())
		}
	}
	if len(in.GuardrailConfig) > 0 {
		kwargs["guardrailConfig"] = in.GuardrailConfig
	}
	if in.Reasoning != nil {
		applyBedrockConverseReasoning(kwargs, inference, model, in.Reasoning, in.MaxTokens)
	}
	if len(inference) == 0 {
		delete(kwargs, "inferenceConfig")
	}
	return kwargs
}

// Converse inputs with the default 4096 token output limit
func newBedrockConverseInput(model string, messages []map[string]any) bedrockConverseInput {
	maxTokens := bedrockDefaultMaxTokens
	return bedrockConverseInput{Model: model, Messages: messages, MaxTokens: &maxTokens}
}

package openai

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Characters of a Mistral tool call id, digits then capitals then small letters
const mistralAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Chat Completions protocol
type ChatProtocol struct{}

// Reports whether a message member is bookkeeping no chat endpoint accepts
func strippedMember(key string) bool {
	switch key {
	case "codex_reasoning_items", "codex_message_items", "tool_name", "effect_disposition", "timestamp",
		"platform_message_id", "message_id", "api_content", "anthropic_content_blocks", "bedrock_content_blocks",
		"message_uid", "data_use", "finish_reason", "codex_reasoning_trimmed":
		return true
	}
	return strings.HasPrefix(key, "_")
}

// Nine alphanumeric characters for a tool call id, the id itself when it already has that form
func mistralToolID(key string) string {
	valid := len(key) == 9
	for i := 0; i < len(key) && valid; i++ {
		valid = strings.IndexByte(mistralAlphabet, key[i]) >= 0
	}
	if valid {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	number := binary.BigEndian.Uint64(sum[:8])
	var out [9]byte
	for i := range out {
		out[i] = mistralAlphabet[number%62]
		number /= 62
	}
	return string(out[:])
}

// Reports whether a tool call's extra_content holds a thought signature worth replaying
func replayableSignature(extra json.RawMessage) bool {
	v, err := wire.ParseValue(extra)
	if err != nil || !v.IsObject() {
		return false
	}
	candidate := v.Get("thought_signature")
	if candidate.Kind() <= wire.KindNull {
		candidate = v.Path("google", "thought_signature")
	}
	return nonblank(candidate)
}

// Pairing key of a tool result id, the call half of a joined id
func resultKey(toolCallID string) string {
	value := strip(toolCallID)
	if value == "" {
		return ""
	}
	head, _, _ := strings.Cut(value, "|")
	if head = strip(head); head != "" {
		return head
	}
	return value
}

// Wire form of one message with every member the route does not read removed
func chatMessage(src *wire.Message, o *wire.ChatOptions, keys wire.ReasoningKeys, first bool) json.RawMessage {
	m := src.Clone()
	m.ToolName, m.FinishReason, m.DataUse = "", "", nil
	m.ResponsesReasoning, m.ResponsesMessages, m.ReasoningTrimmed = nil, nil, false
	m.AnthropicBlocks, m.BedrockBlocks = nil, nil
	if m.Role == wire.RoleAssistant {
		if keys&wire.KeyReasoningContent == 0 {
			m.ReasoningContent = nil
		}
		if keys&wire.KeyReasoning == 0 {
			m.Reasoning = ""
		}
		if keys&wire.KeyReasoningDetails == 0 {
			m.ReasoningDetails = nil
		}
	}
	if len(m.ReasoningDetails) > 0 {
		kept := m.ReasoningDetails[:0:0]
		for _, d := range m.ReasoningDetails {
			typ := wire.DetailType(d)
			if strings.HasSuffix(typ, ".native_assistant") && typ != o.NativeDetailsType {
				continue
			}
			kept = append(kept, d)
		}
		if len(kept) != len(m.ReasoningDetails) {
			m.ReasoningDetails = kept
		}
	}
	if m.Role == wire.RoleTool {
		m.Name = ""
		if key := resultKey(m.ToolCallID); o.MistralToolIDs && key != "" {
			m.ToolCallID = mistralToolID(key)
		}
	}
	for i := range m.ToolCalls {
		c := &m.ToolCalls[i]
		key := ""
		if o.MistralToolIDs {
			key = c.EffectiveID()
		}
		c.CallID, c.ResponseItemID = "", ""
		if len(c.ExtraContent) > 0 && (!o.KeepSignatures || !replayableSignature(c.ExtraContent)) {
			c.ExtraContent = nil
		}
		if key != "" {
			c.ID = mistralToolID(key)
		}
	}
	if first && o.DeveloperRole && m.Role == wire.RoleSystem {
		m.Role = wire.RoleDeveloper
	}
	members := m.Members()
	kept := members[:0]
	for _, mb := range members {
		switch {
		case strippedMember(mb.Key):
		case mb.Key == "name" && m.Role == wire.RoleTool:
		case mb.Key == "tool_calls" && m.Role == wire.RoleAssistant && len(m.ToolCalls) == 0 && emptyOrNull(mb.Value):
		default:
			kept = append(kept, mb)
		}
	}
	return wire.Object(kept)
}

// Reports whether raw JSON is null or an array with no elements
func emptyOrNull(raw json.RawMessage) bool {
	v, err := wire.ParseValue(raw)
	return err != nil || v.Kind() == wire.KindNull || (v.IsArray() && len(v.Elems()) == 0)
}

// Chat tool declarations with reserved names aliased and Moonshot schemas repaired, plus the aliases issued
func chatTools(tools []wire.Tool, o *wire.ChatOptions) ([]json.RawMessage, map[string]string) {
	aliases := map[string]string{}
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].Name
	}
	if o.AliasToolSearch {
		aliasReserved(names, []string{"tool_search"}, aliases)
	}
	out := make([]json.RawMessage, 0, len(tools))
	for i := range tools {
		t := &tools[i]
		d := &wire.Dict{}
		if t.Builtin != "" {
			d.SetString("type", t.Builtin)
		} else {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			if o.MoonshotSchemas {
				params = MoonshotParameters(params)
			}
			fn := (&wire.Dict{}).SetString("name", names[i]).SetString("description", t.Description).Set("parameters", params)
			d.SetString("type", "function").Set("function", fn.JSON())
		}
		if t.Cache != nil {
			d.Set("cache_control", cacheControl(t.Cache))
		}
		out = append(out, d.JSON())
	}
	return out, aliases
}

// Ephemeral cache_control object for a marker
func cacheControl(c *wire.CacheMarker) json.RawMessage {
	d := (&wire.Dict{}).SetString("type", "ephemeral")
	if c.TTL != "" {
		d.SetString("ttl", c.TTL)
	}
	return d.JSON()
}

// Leading system or developer prompt of the wire messages as the text cache routing hashes
func staticInstructions(messages []json.RawMessage) string {
	if len(messages) == 0 {
		return ""
	}
	first, err := wire.ParseValue(messages[0])
	if err != nil {
		return ""
	}
	if role := first.Get("role").Text(); role != "system" && role != "developer" {
		return ""
	}
	content := first.Get("content")
	if s, ok := content.Str(); ok {
		return s
	}
	if !content.Exists() {
		return "null"
	}
	return canonicalJSON(content.Raw())
}

// tool_choice value for a choice, nil when the request leaves the member off
func chatToolChoice(c wire.ToolChoice) json.RawMessage {
	switch {
	case c.Name != "":
		fn := (&wire.Dict{}).SetString("name", c.Name)
		return (&wire.Dict{}).SetString("type", "function").Set("function", fn.JSON()).JSON()
	case c.Mode != "":
		return wire.Quote(c.Mode)
	}
	return nil
}

// response_format value for a structured output request
func chatResponseFormat(f *wire.ResponseFormat) json.RawMessage {
	d := (&wire.Dict{}).SetString("type", f.Type)
	if f.Type == "json_schema" {
		schema := (&wire.Dict{}).SetString("name", f.Name)
		if len(f.Schema) > 0 {
			schema.Set("schema", f.Schema)
		}
		if f.Strict != nil {
			schema.SetBool("strict", *f.Strict)
		}
		d.Set("json_schema", schema.JSON())
	}
	return d.JSON()
}

// Body member the output cap travels in
func maxTokensKey(f wire.MaxTokensField) string {
	switch f {
	case wire.MaxCompletionTokens:
		return "max_completion_tokens"
	case wire.MaxOutputTokens:
		return "max_output_tokens"
	}
	return "max_tokens"
}

// Builds a chat completions call, sanitizing messages for the route and merging the body extras last
func (ChatProtocol) Build(req *wire.Request) (*wire.Call, error) {
	o := &req.Chat
	keys := o.ReplayReasoning
	if o.NativeDetailsType != "" && keys != 0 {
		keys |= wire.KeyReasoningDetails
	}
	messages := make([]json.RawMessage, len(req.Messages))
	for i := range req.Messages {
		messages[i] = chatMessage(&req.Messages[i], o, keys, i == 0)
	}
	body := (&wire.Dict{}).SetString("model", req.Model).Set("messages", wire.Array(messages...))
	if !req.OmitTemperature && req.Temperature != nil {
		body.SetFloat("temperature", *req.Temperature)
	}
	tools, aliases := chatTools(req.Tools, o)
	if len(tools) > 0 {
		body.Set("tools", wire.Array(tools...))
	}
	if req.MaxTokens > 0 {
		body.SetInt(maxTokensKey(req.MaxTokensField), req.MaxTokens)
	}
	if choice := chatToolChoice(req.ToolChoice); choice != nil && len(tools) > 0 {
		body.Set("tool_choice", choice)
	}
	if req.ParallelToolCalls != nil && len(tools) > 0 {
		body.SetBool("parallel_tool_calls", *req.ParallelToolCalls)
	}
	if req.TopP != nil {
		body.SetFloat("top_p", *req.TopP)
	}
	if req.TopK != nil {
		body.SetInt("top_k", *req.TopK)
	}
	if len(req.Stop) > 0 {
		body.Set("stop", wire.Strings(req.Stop))
	}
	if req.ResponseFormat != nil {
		body.Set("response_format", chatResponseFormat(req.ResponseFormat))
	}
	if req.ServiceTier != "" {
		body.SetString("service_tier", req.ServiceTier)
	}
	if req.Verbosity != "" {
		body.SetString("verbosity", req.Verbosity)
	}
	if len(req.Metadata) > 0 {
		body.Set("metadata", req.Metadata)
	}
	if req.Store != nil {
		body.SetBool("store", *req.Store)
	}
	if req.Reasoning != nil {
		if len(req.Reasoning.Payload) > 0 {
			body.Set("reasoning", req.Reasoning.Payload)
		} else if nested, ok := wire.NestedReasoning(req.Reasoning); ok {
			body.Set("reasoning", nested)
		}
	}
	if req.PromptCacheKey != "" {
		body.SetString("prompt_cache_key", req.PromptCacheKey)
	}
	if req.CacheRetention != "" {
		body.SetString("prompt_cache_retention", req.CacheRetention)
	}
	mergeInto(body, req.TopLevel)
	extra := &wire.Dict{}
	mergeInto(extra, req.ExtraBody)
	if body.Has("prompt_cache_key") || extra.Has("prompt_cache_key") {
		boundCacheKeyField(body)
		boundCacheKeyField(extra)
	} else if o.CacheKey {
		if key := contentCacheKey(staticInstructions(messages), tools, cacheScope(req)); key != "" {
			body.SetString("prompt_cache_key", key)
		}
	}
	for _, mb := range extra.Members() {
		body.Set(mb.Key, mb.Value)
	}
	if req.Stream {
		body.SetBool("stream", true)
		if req.IncludeUsage {
			body.Set("stream_options", json.RawMessage(`{"include_usage":true}`))
		}
	}
	return &wire.Call{
		Method: "POST",
		Path:   "/chat/completions",
		Header: callHeader(req, nil),
		Body:   wire.ScrubSurrogates(body.JSON()),
		Stream: req.Stream,
		Decode: &chatDecoder{aliases: aliases, carriers: o.Carriers},
	}, nil
}

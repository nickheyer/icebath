package transport

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Carrier the Anthropic messages wire keeps its replies under, which Claude turns are replayed from here too
const anthropicCarrier = string(provider.ModeAnthropicMessages)

// Record of the model a turn was made by, among its details
const issuerRecord = "bedrock.native_assistant"

// Text that stands in for a block Converse refuses empty
const emptyText = "(empty)"

// What a reply carried that is replayed with it: its reasoning blocks with the record of the model that made it, and every block in order
type carried struct {
	Details []any `json:"details,omitempty"`
	Blocks  []any `json:"blocks,omitempty"`
}

// Families by what Converse takes of them
var (
	// Models known to refuse a toolConfig; unknown models are taken to take tools
	noTools = []string{"deepseek.r1", "deepseek-r1", "stability.", "cohere.embed", "amazon.titan-embed"}
	// Families that take cachePoint markers; unknown models get none
	cacheFamilies = []string{"anthropic.claude", "amazon.nova"}
	// Grok on Bedrock refuses temperature and topP
	grokSampling = regexp.MustCompile(`(?i)^(?:[a-z]+\.)?xai\.grok`)
	// Claude on Bedrock in any regional or global profile form
	claudeModel = regexp.MustCompile(`(?i)^(?:(?:global|us|eu|apac|ap|au|jp|ca|sa|me|af)\.)?anthropic\.claude`)
	// Application inference profiles, which wrap the model they serve
	applicationProfile = regexp.MustCompile(`:application-inference-profile/`)
)

func lowered(model string, parts []string) bool {
	return provider.Contains(strings.ToLower(model), parts...)
}

// How a model's reasoning is replayed to it: signed for Claude, issuer for GPT-6 which reads only its own, strip for R1 and Kimi K3, else
// unsigned text
func policy(model string) string {
	switch m := strings.ToLower(model); {
	case strings.Contains(m, "claude"):
		return "signed"
	case strings.Contains(m, "openai.gpt-6"):
		return "issuer"
	case provider.Contains(m, "deepseek.r1", "deepseek-r1", "moonshotai.kimi-k3"):
		return "strip"
	}
	return "unsigned"
}

func block(kind string, value any) map[string]any { return map[string]any{kind: value} }

func textBlock(text string) map[string]any { return map[string]any{"text": text} }

// Text when it holds more than space, else the placeholder
func safe(text string) string {
	if strings.TrimSpace(text) == "" {
		return emptyText
	}
	return text
}

func toolUse(id, name string, input any) map[string]any {
	return block("toolUse", map[string]any{"toolUseId": id, "name": name, "input": input})
}

func reasoningText(text string, signature string) map[string]any {
	rt := map[string]any{"text": text}
	if signature != "" {
		rt["signature"] = signature
	}
	return block("reasoningContent", map[string]any{"reasoningText": rt})
}

func redacted(data string) map[string]any {
	if data == "" || strings.ContainsAny(data, "\r\n") {
		return nil
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil
	}
	return block("reasoningContent", map[string]any{"redactedContent": data})
}

// Tool arguments decoded, an empty object when they aren't JSON
func arguments(raw string) any {
	var decoded any
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return map[string]any{}
	}
	return decoded
}

// Blocks of a user message: its text and its pictures as image blocks
func userBlocks(m *sessionsv1.Message) []any {
	out := []any{textBlock(safe(m.GetText()))}
	for _, media := range m.GetMedia() {
		mime := media.GetMimeType()
		if !strings.HasPrefix(mime, "image/") {
			noun, _, _ := strings.Cut(mime, "/")
			out = append(out, textBlock("["+noun+" omitted: "+mime+" is not a supported "+noun+" format]"))
			continue
		}
		format := mime[strings.LastIndex(mime, "/")+1:]
		if format == "jpg" {
			format = "jpeg"
		}
		out = append(out, block("image", map[string]any{"format": format, "source": map[string]any{"bytes": base64.StdEncoding.EncodeToString(media.GetData())}}))
	}
	return out
}

// Blocks a turn came in, from the order its reply kept: its own Converse blocks, or for Claude the Anthropic blocks of a turn made over the
// Anthropic API, tool inputs taken from the turn's calls as they stand now
func ordered(m *sessionsv1.Message, claude bool) []any {
	inputs := map[string]any{}
	for _, call := range m.GetToolCalls() {
		inputs[call.GetId()] = arguments(call.GetArguments())
	}
	var out []any
	for _, item := range transport.Carried[carried](m, carrier).Blocks {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case b["text"] != nil:
			if text, ok := b["text"].(string); ok {
				out = append(out, textBlock(text))
			}
		case b["reasoningContent"] != nil:
			reasoning, _ := b["reasoningContent"].(map[string]any)
			if text, ok := reasoning["text"].(string); ok {
				out = append(out, reasoningText(text, transport.Text(reasoning["signature"])))
			}
			if r := redacted(transport.Text(reasoning["redactedContentBase64"])); r != nil {
				out = append(out, r)
			}
		case b["toolUse"] != nil:
			tu, _ := b["toolUse"].(map[string]any)
			input, ok := inputs[transport.Text(tu["toolUseId"])]
			if !ok {
				input = tu["input"]
			}
			out = append(out, toolUse(transport.Text(tu["toolUseId"]), transport.Text(tu["name"]), input))
		}
	}
	if len(out) > 0 || !claude {
		return out
	}
	for _, item := range transport.Carried[carried](m, anthropicCarrier).Blocks {
		b, _ := item.(map[string]any)
		switch b["type"] {
		case "thinking":
			out = append(out, reasoningText(transport.Text(b["thinking"]), transport.Text(b["signature"])))
		case "redacted_thinking":
			if r := redacted(transport.Text(b["data"])); r != nil {
				out = append(out, r)
			}
		case "text":
			if text := transport.Text(b["text"]); strings.TrimSpace(text) != "" {
				out = append(out, textBlock(text))
			}
		case "tool_use":
			input, ok := inputs[transport.Text(b["id"])]
			if !ok {
				input = b["input"]
			}
			out = append(out, toolUse(transport.Text(b["id"]), transport.Text(b["name"]), input))
		}
	}
	return out
}

// Details a turn's reply carried: its own, or those of a turn made over the Anthropic API
func details(m *sessionsv1.Message) []any {
	if own := transport.Carried[carried](m, carrier).Details; len(own) > 0 {
		return own
	}
	return transport.Carried[carried](m, anthropicCarrier).Details
}

// Model a turn's record says made it, false for a turn with no record
func issuer(m *sessionsv1.Message) (string, bool) {
	for _, d := range transport.Carried[carried](m, carrier).Details {
		if detail, ok := d.(map[string]any); ok && detail["type"] == issuerRecord {
			model, ok := detail["model"].(string)
			return model, ok
		}
	}
	return "", false
}

// Reasoning blocks of a turn's details in their order: signed ones for Claude and the model that made them, readable text for others
func reasoningBlocks(m *sessionsv1.Message, claude, own bool) []any {
	var out []any
	for _, item := range details(m) {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch d["type"] {
		case "thinking":
			thinking := transport.Text(d["thinking"])
			if strings.TrimSpace(thinking) == "" {
				continue
			}
			if signature := transport.Text(d["signature"]); (claude || own) && signature != "" {
				out = append(out, reasoningText(thinking, signature))
			} else if !claude {
				out = append(out, reasoningText(thinking, ""))
			}
		case "redacted_thinking":
			data := transport.Text(d["data"])
			data = cmp.Or(data, transport.Text(d["redactedContentBase64"]))
			if r := redacted(data); r != nil {
				out = append(out, r)
			}
		}
	}
	if reasoning := m.GetReasoning(); len(out) == 0 && !claude && strings.TrimSpace(reasoning) != "" {
		out = append(out, reasoningText(reasoning, ""))
	}
	return out
}

// Blocks of an assistant turn from the first carrier that gives any, else its reasoning, text and calls
func assistantBlocks(m *sessionsv1.Message, claude, own bool) []any {
	if blocks := ordered(m, claude); len(blocks) > 0 {
		return blocks
	}
	blocks := reasoningBlocks(m, claude, own)
	if text := m.GetText(); strings.TrimSpace(text) != "" {
		blocks = append(blocks, textBlock(text))
	}
	for _, call := range m.GetToolCalls() {
		blocks = append(blocks, toolUse(call.GetId(), call.GetName(), arguments(call.GetArguments())))
	}
	return blocks
}

// What a turn's reasoning keeps: everything, readable text only, or none
type keep func(b map[string]any) map[string]any

func verbatim(b map[string]any) map[string]any { return b }

func withoutReasoning(b map[string]any) map[string]any {
	if b["reasoningContent"] != nil {
		return nil
	}
	return b
}

func unsigned(b map[string]any) map[string]any {
	reasoning, ok := b["reasoningContent"].(map[string]any)
	if !ok {
		return b
	}
	rt, _ := reasoning["reasoningText"].(map[string]any)
	if text := transport.Text(rt["text"]); strings.TrimSpace(text) != "" {
		return reasoningText(text, "")
	}
	return nil
}

// Filters of each policy for turns before the current tool loop and inside it
var filters = map[string][2]keep{"signed": {verbatim, verbatim}, "strip": {withoutReasoning, verbatim}, "unsigned": {unsigned, verbatim},
	"issuer": {withoutReasoning, withoutReasoning}}

// Reasoning a turn may replay to the model asked: its own turns all of it, a turn another model made per the policy, a Claude turn made by
// another family none of its signatures
func filter(rule, made string, stamped, own, inLoop bool) keep {
	prior, loop := filters[rule][0], filters[rule][1]
	switch {
	case own && (inLoop || rule == "unsigned" || rule == "issuer"):
		return verbatim
	case own:
		return prior
	case stamped && rule == "signed" && !strings.Contains(strings.ToLower(made), "claude") && !applicationProfile.MatchString(made):
		return withoutReasoning
	case stamped:
		return prior
	case inLoop:
		return loop
	}
	return prior
}

// System blocks and strictly alternating turns of the request's messages for the model
func convert(messages []*sessionsv1.Message, model string) ([]any, []any) {
	rule := policy(model)
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].GetRole() == sessionsv1.Role_ROLE_USER {
			start = i
			break
		}
	}
	var system, turns []any
	add := func(role string, blocks []any) {
		if n := len(turns); n > 0 {
			if last := turns[n-1].(map[string]any); last["role"] == role {
				last["content"] = append(last["content"].([]any), blocks...)
				return
			}
		}
		turns = append(turns, map[string]any{"role": role, "content": blocks})
	}
	for i, m := range messages {
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			if strings.TrimSpace(m.GetText()) != "" {
				system = append(system, textBlock(m.GetText()))
			}
		case sessionsv1.Role_ROLE_TOOL:
			add("user", []any{block("toolResult", map[string]any{"toolUseId": m.GetToolCallId(), "content": []any{textBlock(safe(m.GetText()))}})})
		case sessionsv1.Role_ROLE_ASSISTANT:
			made, stamped := issuer(m)
			own := stamped && made == model
			keepFn := filter(rule, made, stamped, own, i >= start)
			var blocks []any
			for _, b := range assistantBlocks(m, rule == "signed", own) {
				if kept := keepFn(b.(map[string]any)); kept != nil {
					blocks = append(blocks, kept)
				}
			}
			if len(blocks) == 0 {
				blocks = []any{textBlock(emptyText)}
			}
			add("assistant", blocks)
		case sessionsv1.Role_ROLE_USER:
			add("user", userBlocks(m))
		}
	}
	if len(turns) > 0 && turns[0].(map[string]any)["role"] != "user" {
		turns = append([]any{map[string]any{"role": "user", "content": []any{textBlock(emptyText)}}}, turns...)
	}
	if n := len(turns); n > 0 && turns[n-1].(map[string]any)["role"] != "user" {
		turns = append(turns, map[string]any{"role": "user", "content": []any{textBlock(emptyText)}})
	}
	if turns == nil {
		turns = []any{}
	}
	return system, turns
}

// OpenAI shaped tools as Converse tool specs
func toolSpecs(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		function, _ := t["function"].(map[string]any)
		params := function["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, block("toolSpec", map[string]any{"name": transport.Text(function["name"]), "description": transport.Text(function["description"]),
			"inputSchema": map[string]any{"json": params}}))
	}
	return out
}

func cachePoint() map[string]any { return block("cachePoint", map[string]any{"type": "default"}) }

// Sections a cachePoint marker can sit in
var placements = []string{"tools", "system", "messages"}

var markerPath = regexp.MustCompile(`(?i)#/([A-Za-z0-9_./\[\]-]*)`)

// Section whose cachePoint Bedrock refused, tools when its pointer names none; empty for an unrelated refusal
func refusedPlacement(message string) string {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "cachepoint") || !strings.Contains(lower, "not permitted") && !strings.Contains(lower, "extraneous") {
		return ""
	}
	path := ""
	if m := markerPath.FindStringSubmatch(message); m != nil {
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

// Blocks without their bare cachePoint entries, and whether any went
func withoutMarkers(blocks any) ([]any, bool) {
	list, ok := blocks.([]any)
	if !ok {
		return nil, false
	}
	out := slices.DeleteFunc(slices.Clone(list), func(b any) bool { m, _ := b.(map[string]any); return len(m) == 1 && m["cachePoint"] != nil })
	return out, len(out) != len(list)
}

// Body without the cachePoint of the section, false when it had none
func stripMarkers(body map[string]any, placement string) (map[string]any, bool) {
	out := shallow(body)
	switch placement {
	case "messages":
		messages, _ := body["messages"].([]any)
		changed := false
		copied := make([]any, len(messages))
		for i, m := range messages {
			copied[i] = m
			message, _ := m.(map[string]any)
			if content, stripped := withoutMarkers(message["content"]); stripped {
				next := shallow(message)
				next["content"] = content
				copied[i], changed = next, true
			}
		}
		out["messages"] = copied
		return out, changed
	case "system":
		system, stripped := withoutMarkers(body["system"])
		out["system"] = system
		return out, stripped
	case "tools":
		config, _ := body["toolConfig"].(map[string]any)
		tools, stripped := withoutMarkers(config["tools"])
		if stripped {
			next := shallow(config)
			next["tools"] = tools
			out["toolConfig"] = next
		}
		return out, stripped
	}
	return body, false
}

func shallow(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var redactedRefusal = regexp.MustCompile(`(?is)ValidationException.*(?:redacted|encrypt)`)

// Body without replayed redacted reasoning and the turns it left empty, false when it had none
func stripRedacted(body map[string]any) (map[string]any, bool) {
	messages, _ := body["messages"].([]any)
	var out []any
	changed := false
	for _, m := range messages {
		message, _ := m.(map[string]any)
		content, _ := message["content"].([]any)
		kept := slices.DeleteFunc(slices.Clone(content), func(b any) bool {
			block, _ := b.(map[string]any)
			reasoning, _ := block["reasoningContent"].(map[string]any)
			return reasoning["redactedContent"] != nil
		})
		if len(kept) == len(content) {
			out = append(out, m)
			continue
		}
		changed = true
		if len(kept) > 0 {
			next := shallow(message)
			next["content"] = kept
			out = append(out, next)
		}
	}
	if !changed {
		return body, false
	}
	next := shallow(body)
	next["messages"] = out
	return next, true
}

var novaEfforts = map[string]string{"minimal": "low", "low": "low", "medium": "medium"}

// Reasoning fields of the model and the inference settings they set, nil ones taken out
func reasoning(model string, ask provider.Ask, maxTokens int) (map[string]any, map[string]any) {
	switch {
	case claudeModel.MatchString(model):
		thinking := provider.ClaudeThinking(ask, model, maxTokens)
		fields := map[string]any{}
		for _, k := range []string{"thinking", "output_config"} {
			if v, ok := thinking[k]; ok {
				fields[k] = v
			}
		}
		inference := map[string]any{}
		if t, _ := fields["thinking"].(map[string]any); t["type"] == "enabled" {
			if !strings.Contains(strings.ToLower(model), "claude-3") {
				fields["anthropic_beta"] = []any{"interleaved-thinking-2025-05-14"}
			}
			inference["temperature"] = thinking["temperature"]
			if maxTokens > 0 {
				inference["maxTokens"] = thinking["max_tokens"]
			}
		}
		return fields, inference
	case strings.Contains(strings.ToLower(model), "amazon.nova-2-lite"):
		if !ask.Set || ask.Off {
			return nil, nil
		}
		effort, ok := novaEfforts[strings.ToLower(cmpOr(ask.Effort, "medium"))]
		if !ok {
			effort = "high"
		}
		fields := map[string]any{"reasoningConfig": map[string]any{"type": "enabled", "maxReasoningEffort": effort}}
		if effort == "high" {
			return fields, map[string]any{"temperature": nil, "topP": nil, "maxTokens": nil}
		}
		return fields, nil
	}
	return nil, nil
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Whether no later request can read a cache of this one: no tools, one user message and no replies
func oneShot(messages []*sessionsv1.Message, tools []map[string]any) bool {
	if len(tools) > 0 {
		return false
	}
	users := 0
	for _, m := range messages {
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_USER:
			users++
		case sessionsv1.Role_ROLE_ASSISTANT, sessionsv1.Role_ROLE_TOOL:
			return false
		}
	}
	return users <= 1
}

// Body of a Converse request: its turns and system blocks, inference settings, tools, the model's reasoning, and cachePoint markers on the
// system prompt, the tools and the second newest turn for the families that take them, those Bedrock refused for the model left off
func (c *client) body(p *prepared, tools []map[string]any) map[string]any {
	model := p.model
	system, turns := convert(p.request.Messages, p.foundation)
	marked := map[string]bool{}
	if p.cache && !oneShot(p.request.Messages, tools) && lowered(p.foundation, cacheFamilies) {
		for _, placement := range placements {
			if _, refused := c.r.markers.Load(strings.ToLower(model) + "\x00" + placement); !refused {
				marked[placement] = true
			}
		}
	}
	inference := map[string]any{}
	if p.request.MaxTokens > 0 {
		inference["maxTokens"] = p.request.MaxTokens
	}
	body := map[string]any{"messages": turns}
	if len(system) > 0 {
		if marked["system"] {
			system = append(system, cachePoint())
		}
		body["system"] = system
	}
	if !provider.ClaudeSamplingForbidden(p.foundation) && !grokSampling.MatchString(model) {
		if temperature := p.spec.Temperature(p.foundation, p.request.Temperature); temperature != nil {
			inference["temperature"] = *temperature
		}
	}
	if len(tools) > 0 {
		if lowered(model, noTools) {
			slog.Warn(fmt.Sprintf("Model %s does not support tool calling — tools stripped. The agent will operate in text-only mode.", model))
		} else {
			specs := toolSpecs(tools)
			if marked["tools"] {
				specs = append(specs, cachePoint())
			}
			body["toolConfig"] = map[string]any{"tools": specs}
		}
	}
	if marked["messages"] && len(turns) >= 2 {
		turn := turns[len(turns)-2].(map[string]any)
		if content, ok := turn["content"].([]any); ok && len(content) > 0 {
			turn["content"] = append(content, cachePoint())
		}
	}
	fields, settings := reasoning(p.foundation, p.call.Ask, p.request.MaxTokens)
	if len(fields) > 0 {
		body["additionalModelRequestFields"] = fields
	}
	for k, v := range settings {
		if v == nil {
			delete(inference, k)
		} else {
			inference[k] = v
		}
	}
	if len(inference) > 0 {
		body["inferenceConfig"] = inference
	}
	return body
}

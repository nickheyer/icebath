package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/state"
)

const emptyTextPlaceholder = "(empty)"

var thinkingBlockTypes = map[string]bool{"thinking": true, "redacted_thinking": true}
var cacheableBlockTypes = map[string]bool{"text": true, "tool_use": true}
var bedrockRegionPrefixes = []string{"global.", "us.", "eu.", "apac.", "ap.", "au.", "jp.", "ca.", "sa.", "me.", "af."}

// Record type stamped by the Bedrock Converse wire naming the model that minted a turn's reasoning
const converseIssuerType = "bedrock.native_assistant"

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asList(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// Recursive copy of JSON shaped data so wire mutation never reaches the caller's values
func deepCopyAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyAny(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyAny(val)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyAny(val)
		}
		return out
	case json.RawMessage:
		var parsed any
		if json.Unmarshal(t, &parsed) == nil {
			return parsed
		}
		return string(t)
	}
	return v
}

// Ordered Messages blocks of live assistant turns, gone once the message is collected and never persisted
var anthropicLiveBlocks sync.Map

// Attaches the ordered Messages block sidecar to the in memory message only
func setAnthropicLiveBlocks(m *state.Message, blocks []any) {
	key := weak.Make(m)
	anthropicLiveBlocks.Store(key, blocks)
	runtime.AddCleanup(m, func(k weak.Pointer[state.Message]) { anthropicLiveBlocks.Delete(k) }, key)
}

// SetAnthropicLiveBlocks replaces the ordered Messages block sidecar of a live message
func SetAnthropicLiveBlocks(m *state.Message, blocks []any) { setAnthropicLiveBlocks(m, blocks) }

// Ordered Messages block sidecar of a live message, absent for a message reloaded from the session store
func anthropicLiveBlocksOf(m *state.Message) ([]any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := anthropicLiveBlocks.Load(weak.Make(m))
	if !ok {
		return nil, false
	}
	blocks, ok := v.([]any)
	return blocks, ok && len(blocks) > 0
}

// AnthropicLiveBlocks is the live ordered Messages content block sidecar of a message, false when it has none
func AnthropicLiveBlocks(m *state.Message) ([]any, bool) { return anthropicLiveBlocksOf(m) }

// OpenAI shaped dict of a transcript row with its stored carriers verbatim and the live ordered sidecar
func messageToDict(m *state.Message) map[string]any {
	d := map[string]any{"role": m.Role}
	if m.Content != nil {
		d["content"] = deepCopyAny(m.Content)
	} else {
		d["content"] = ""
	}
	if m.ToolCallID != "" {
		d["tool_call_id"] = m.ToolCallID
	}
	if m.ToolName != "" {
		d["name"] = m.ToolName
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			calls = append(calls, map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
		}
		d["tool_calls"] = calls
	}
	if m.ReasoningContent != "" {
		d["reasoning_content"] = m.ReasoningContent
	}
	if m.Reasoning != "" {
		d["reasoning"] = m.Reasoning
	}
	if len(m.ReasoningDetails) > 0 {
		var details any
		if json.Unmarshal(m.ReasoningDetails, &details) == nil && details != nil {
			d["reasoning_details"] = details
		}
	}
	if blocks, ok := anthropicLiveBlocksOf(m); ok {
		d["anthropic_content_blocks"] = deepCopyAny(blocks)
	}
	return d
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func blockType(b any) string {
	m, ok := asMap(b)
	if !ok {
		return ""
	}
	return asStr(m["type"])
}

func hasBlockType(blocks []any, types map[string]bool) bool {
	for _, b := range blocks {
		if types[blockType(b)] {
			return true
		}
	}
	return false
}

// A text block whose text is not a non whitespace string
func isBlankTextBlock(b any) bool {
	if blockType(b) != "text" {
		return false
	}
	m, _ := asMap(b)
	s, ok := asString(m["text"])
	return !(ok && strings.TrimSpace(s) != "")
}

func cacheControlOf(b any) map[string]any {
	m, ok := asMap(b)
	if !ok {
		return nil
	}
	cc, ok := asMap(m["cache_control"])
	if !ok {
		return nil
	}
	return cc
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func textBlockWithCitations(text any, cits any) map[string]any {
	block := map[string]any{"type": "text", "text": text}
	if list, ok := asList(cits); ok && len(list) > 0 {
		block["citations"] = list
	}
	return block
}

// JSON decoded tool arguments, non strings pass through, bad JSON becomes an empty object
func parseToolArgs(raw any) any {
	s, ok := raw.(string)
	if !ok {
		return raw
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func stripThinking(blocks []any) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		if !thinkingBlockTypes[blockType(b)] {
			out = append(out, b)
		}
	}
	return out
}

func blockIDs(blocks []any, btype, key string) map[string]bool {
	ids := map[string]bool{}
	for _, b := range blocks {
		if blockType(b) == btype {
			m, _ := asMap(b)
			ids[asStr(m[key])] = true
		}
	}
	return ids
}

func carryCacheControl(out map[string]any, b any, copyIt bool) map[string]any {
	if cc := cacheControlOf(b); cc != nil {
		if copyIt {
			out["cache_control"] = deepCopyAny(cc)
		} else {
			out["cache_control"] = cc
		}
	}
	return out
}

// Kept blocks, the relocated marker of the last dropped blank block, and dropped indexes
func splitBlankTextBlocks(blocks []any) ([]any, map[string]any, []int) {
	var kept []any
	var dropped []int
	var relocated map[string]any
	for i, blk := range blocks {
		if isBlankTextBlock(blk) {
			dropped = append(dropped, i)
			if cc := cacheControlOf(blk); cc != nil {
				relocated = cc
			}
			continue
		}
		kept = append(kept, blk)
	}
	return kept, relocated, dropped
}

func isBedrockModelID(model string) bool {
	lower := strings.ToLower(model)
	for _, p := range append(append([]string{}, bedrockRegionPrefixes...), "anthropic.") {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// Strips the anthropic prefix and hyphenates Claude version dots unless dots are preserved
func normalizeModelName(model string, preserveDots bool) string {
	if strings.HasPrefix(strings.ToLower(model), "anthropic/") {
		model = model[len("anthropic/"):]
	}
	lower := strings.ToLower(model)
	if !preserveDots && !isBedrockModelID(model) && (strings.HasPrefix(lower, "claude-") || strings.HasPrefix(lower, "anthropic/")) {
		model = strings.ReplaceAll(model, ".", "-")
	}
	return model
}

var toolIDBadRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeToolID(id string) string {
	if id == "" {
		return "tool_0"
	}
	out := toolIDBadRe.ReplaceAllString(id, "_")
	if out == "" {
		return "tool_0"
	}
	return out
}

func toolUseBlock(id, name string, input any) map[string]any {
	return map[string]any{"type": "tool_use", "id": sanitizeToolID(id), "name": name, "input": input}
}

var unionKeys = []string{"anyOf", "oneOf"}
var unionMetaKeys = []string{"title", "description", "default", "examples"}

// Bottom up map over a schema tree applying fn to every object node
func rewriteSchema(schema any, fn func(map[string]any) any) any {
	switch t := schema.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = rewriteSchema(item, fn)
		}
		return out
	case map[string]any:
		node := make(map[string]any, len(t))
		for k, v := range t {
			node[k] = rewriteSchema(v, fn)
		}
		return fn(node)
	}
	return schema
}

func isNullBranch(item any) bool {
	m, ok := asMap(item)
	return ok && asStr(m["type"]) == "null"
}

func carryUnionMeta(outer, replacement map[string]any, skipDefaultOnRef bool) {
	for _, key := range unionMetaKeys {
		v, has := outer[key]
		if !has {
			continue
		}
		if _, present := replacement[key]; present {
			continue
		}
		if _, isRef := replacement["$ref"]; skipDefaultOnRef && key == "default" && isRef {
			continue
		}
		replacement[key] = v
	}
}

// Collapses nullable anyOf and oneOf unions to the single non null branch
func stripNullableUnions(schema any, keepNullableHint bool) any {
	var collapse func(map[string]any) any
	collapse = func(stripped map[string]any) any {
		for _, key := range unionKeys {
			variants, ok := asList(stripped[key])
			if !ok {
				continue
			}
			var nonNull []any
			for _, item := range variants {
				if !isNullBranch(item) {
					nonNull = append(nonNull, item)
				}
			}
			if len(nonNull) == 1 && len(nonNull) != len(variants) {
				replacement := map[string]any{}
				if m, ok := asMap(nonNull[0]); ok {
					for k, v := range m {
						replacement[k] = v
					}
				}
				if keepNullableHint {
					if _, has := replacement["nullable"]; !has {
						replacement["nullable"] = true
					}
				}
				carryUnionMeta(stripped, replacement, true)
				return rewriteSchema(replacement, collapse)
			}
		}
		return stripped
	}
	return rewriteSchema(schema, collapse)
}

// Tool schema acceptable to Anthropic with nullable unions collapsed and top level combinators dropped
func normalizeToolInputSchema(schema any) map[string]any {
	var normalized any
	if schema != nil {
		normalized = stripNullableUnions(schema, false)
	}
	m, ok := asMap(normalized)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	banned := false
	for _, k := range []string{"oneOf", "allOf", "anyOf"} {
		if _, has := m[k]; has {
			banned = true
		}
	}
	if banned {
		clean := map[string]any{}
		for k, v := range m {
			if k != "oneOf" && k != "allOf" && k != "anyOf" {
				clean[k] = v
			}
		}
		if _, has := clean["type"]; !has {
			clean["type"] = "object"
		}
		m = clean
	}
	if asStr(m["type"]) == "object" {
		if _, ok := asMap(m["properties"]); !ok {
			copyM := map[string]any{}
			for k, v := range m {
				copyM[k] = v
			}
			copyM["properties"] = map[string]any{}
			m = copyM
		}
	}
	return m
}

// OpenAI tool definitions converted to Anthropic tools with duplicate names dropped
func convertToolsToAnthropic(tools []map[string]any) []map[string]any {
	var result []map[string]any
	seen := map[string]bool{}
	for _, t := range tools {
		fn, _ := asMap(t["function"])
		name := asStr(fn["name"])
		if name != "" && seen[name] {
			continue
		}
		if name != "" {
			seen[name] = true
		}
		var params any
		if fn != nil {
			params = fn["parameters"]
		}
		if params == nil {
			params = map[string]any{}
		}
		tool := map[string]any{"name": name, "description": asStr(fn["description"]), "input_schema": normalizeToolInputSchema(params)}
		result = append(result, carryCacheControl(tool, t, true))
	}
	return result
}

var anthropicSupportedImageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// Media type of an inline data image the wire rejects, empty for accepted rasters and non data URLs
func unsupportedInlineImageMediaType(u string) string {
	header := strings.ToLower(u)
	if i := strings.Index(header, ","); i >= 0 {
		header = header[:i]
	}
	if !strings.HasPrefix(header, "data:image/") {
		return ""
	}
	subtype := strings.TrimSpace(strings.SplitN(header[len("data:image/"):], ";", 2)[0])
	if subtype == "" {
		subtype = "unknown"
	}
	media := "image/" + subtype
	if subtype == "jpg" {
		media = "image/jpeg"
	}
	if anthropicSupportedImageTypes[media] {
		return ""
	}
	return media
}

// PNG data URL rendered from an SVG data URL by rsvg-convert or inkscape, empty when neither works
func rasterizeSVGDataURL(u string) string {
	header, payload, found := strings.Cut(u, ",")
	if !found {
		return ""
	}
	var raw []byte
	if strings.Contains(strings.ToLower(header), ";base64") {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return ""
		}
		raw = decoded
	} else {
		unescaped, err := url.PathUnescape(payload)
		if err != nil {
			return ""
		}
		raw = []byte(unescaped)
	}
	dir, err := os.MkdirTemp("", "icebath-svg-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	svgPath := filepath.Join(dir, "inline.svg")
	pngPath := filepath.Join(dir, "inline.png")
	if err := os.WriteFile(svgPath, raw, 0o600); err != nil {
		return ""
	}
	for _, cmd := range [][]string{
		{"rsvg-convert", "-o", pngPath, svgPath},
		{"inkscape", svgPath, "--export-type=png", "--export-filename=" + pngPath},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		run := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
		run.Stdin = nil
		err := run.Run()
		cancel()
		if err != nil {
			continue
		}
		png, err := os.ReadFile(pngPath)
		if err == nil && len(png) > 0 {
			return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
	}
	return ""
}

// Anthropic image block for an OpenAI image URL, SVG rasterized, other unsupported types become text
func imageBlockFromOpenAIURL(u string) map[string]any {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "data:") {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
	}
	unsupported := unsupportedInlineImageMediaType(u)
	if unsupported == "image/svg+xml" {
		if png := rasterizeSVGDataURL(u); png != "" {
			u, unsupported = png, ""
		}
	}
	if unsupported != "" {
		return textBlock("[image omitted: " + unsupported + " is not a supported image format]")
	}
	header, data, _ := strings.Cut(u, ",")
	mime := strings.TrimSpace(strings.SplitN(header[len("data:"):], ";", 2)[0])
	media := "image/jpeg"
	if strings.HasPrefix(mime, "image/") {
		media = mime
	}
	if strings.ToLower(media) == "image/jpg" {
		media = "image/jpeg"
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}}
}

// One OpenAI content part converted to an Anthropic block, nil when dropped
func convertContentPartToAnthropic(part any) map[string]any {
	if part == nil {
		return nil
	}
	pm, ok := asMap(part)
	if !ok {
		if s, ok := asString(part); ok {
			return textBlock(s)
		}
		b, _ := json.Marshal(part)
		return textBlock(string(b))
	}
	var block map[string]any
	switch ptype := asStr(pm["type"]); ptype {
	case "input_text", "text":
		var cits any
		if ptype == "text" {
			cits = pm["citations"]
		}
		text := pm["text"]
		if text == nil {
			text = ""
		}
		block = textBlockWithCitations(text, cits)
	case "image_url", "input_image":
		var u string
		if iv, ok := asMap(pm["image_url"]); ok {
			u = asStr(iv["url"])
		} else if s, ok := asString(pm["image_url"]); ok {
			u = s
		}
		block = imageBlockFromOpenAIURL(u)
	default:
		block = map[string]any{}
		for k, v := range pm {
			block[k] = v
		}
	}
	if cc := cacheControlOf(pm); cc != nil {
		if _, has := block["cache_control"]; !has {
			block["cache_control"] = deepCopyAny(cc)
		}
	}
	return block
}

func convertContentToAnthropic(content any) any {
	list, ok := asList(content)
	if !ok {
		return content
	}
	out := make([]any, 0, len(list))
	for _, p := range list {
		if b := convertContentPartToAnthropic(p); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// Tool message parts converted to tool_result inner blocks keeping text and image only
func contentPartsToAnthropicBlocks(parts any) []any {
	list, _ := asList(parts)
	var out []any
	for _, p := range list {
		block := convertContentPartToAnthropic(p)
		if block == nil {
			continue
		}
		switch asStr(block["type"]) {
		case "text":
			if s, ok := asString(block["text"]); ok && s != "" {
				out = append(out, textBlock(s))
			}
		case "image":
			if src, ok := asMap(block["source"]); ok && len(src) > 0 {
				out = append(out, map[string]any{"type": "image", "source": src})
			}
		}
	}
	return out
}

// Deep copied thinking blocks of reasoning details, none from a turn minted by a non Claude Converse model
func extractPreservedThinkingBlocks(message map[string]any, opts convertOptions) []any {
	raw, ok := asList(message["reasoning_details"])
	if !ok {
		return nil
	}
	if reasoningMintedOutsideClaude(message, opts.resolveProfileARN) {
		return nil
	}
	var out []any
	for _, d := range raw {
		dm, ok := asMap(d)
		if !ok {
			continue
		}
		if thinkingBlockTypes[strings.ToLower(strings.TrimSpace(asStr(dm["type"])))] {
			out = append(out, deepCopyAny(dm))
		}
	}
	return out
}

func safeText(v any) string {
	s := ""
	if v != nil {
		if str, ok := v.(string); ok {
			s = str
		} else {
			b, _ := json.Marshal(v)
			s = string(b)
		}
	}
	if strings.TrimSpace(s) == "" {
		return emptyTextPlaceholder
	}
	return s
}

func replayText(b map[string]any) map[string]any {
	if isBlankTextBlock(b) {
		return nil
	}
	return carryCacheControl(textBlockWithCitations(b["text"], b["citations"]), b, false)
}

func replayThinking(b map[string]any) map[string]any {
	out := map[string]any{"type": "thinking", "thinking": asStrOr(b["thinking"], "")}
	if sig := asStr(b["signature"]); sig != "" {
		out["signature"] = sig
	}
	return out
}

func asStrOr(v any, def string) any {
	if v == nil {
		return def
	}
	return v
}

func replayRedactedThinking(b map[string]any) map[string]any {
	if data := asStr(b["data"]); data != "" {
		return map[string]any{"type": "redacted_thinking", "data": data}
	}
	return nil
}

func replayToolUse(b map[string]any) map[string]any {
	input := b["input"]
	if input == nil {
		input = map[string]any{}
	}
	return carryCacheControl(toolUseBlock(asStr(b["id"]), asStr(b["name"]), input), b, false)
}

func replayImage(b map[string]any) map[string]any {
	if src, ok := asMap(b["source"]); ok {
		return map[string]any{"type": "image", "source": src}
	}
	return nil
}

// Whitelists a stored Anthropic block so it is valid request input, nil for unknown types
func sanitizeReplayBlock(b any) map[string]any {
	m, ok := asMap(b)
	if !ok {
		return nil
	}
	switch asStr(m["type"]) {
	case "text":
		return replayText(m)
	case "thinking":
		return replayThinking(m)
	case "redacted_thinking":
		return replayRedactedThinking(m)
	case "tool_use":
		return replayToolUse(m)
	case "image":
		return replayImage(m)
	}
	return nil
}

func applyAssistantCacheControlToLastCacheableBlock(blocks []any, cc any) {
	ccm, ok := asMap(cc)
	if !ok || ccm == nil {
		return
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if cacheableBlockTypes[blockType(blocks[i])] {
			bm, _ := asMap(blocks[i])
			if _, has := bm["cache_control"]; !has {
				bm["cache_control"] = deepCopyAny(ccm)
			}
			return
		}
	}
}

// Rebuilds an assistant turn from its verbatim ordered block list with tool inputs re sourced from tool_calls
func replayOrderedBlocks(m map[string]any, ordered []any) []any {
	redactedInput := map[string]any{}
	if calls, ok := asList(m["tool_calls"]); ok {
		for _, tc := range calls {
			tcm, ok := pyjson.Dict(tc)
			if !ok {
				continue
			}
			fn, _ := pyjson.Dict(tcm["function"])
			args := fn["arguments"]
			if args == nil {
				args = "{}"
			}
			redactedInput[sanitizeToolID(asStr(tcm["id"]))] = parseToolArgs(args)
		}
	}
	var replayed []any
	var relocated map[string]any
	droppedBlank := false
	for _, b := range ordered {
		clean := sanitizeReplayBlock(b)
		if clean == nil {
			if blockType(b) == "text" {
				droppedBlank = true
			}
			if cc := cacheControlOf(b); cc != nil {
				relocated = cc
			}
			continue
		}
		if asStr(clean["type"]) == "tool_use" {
			if redacted, has := redactedInput[asStr(clean["id"])]; has {
				clean["input"] = redacted
			}
		}
		replayed = append(replayed, clean)
	}
	if !hasBlockType(replayed, cacheableBlockTypes) && (droppedBlank || relocated != nil) {
		replayed = append(replayed, textBlock(emptyTextPlaceholder))
	}
	if len(replayed) == 0 {
		return nil
	}
	applyAssistantCacheControlToLastCacheableBlock(replayed, relocated)
	applyAssistantCacheControlToLastCacheableBlock(replayed, m["cache_control"])
	if content, ok := asList(m["content"]); ok {
		for _, part := range content {
			if cc := cacheControlOf(part); cc != nil {
				applyAssistantCacheControlToLastCacheableBlock(replayed, cc)
				break
			}
		}
	}
	return replayed
}

// Ordered flag and blocks of the carrier an assistant turn replays from
func assistantReplayCarrier(m map[string]any, opts convertOptions) (bool, []any) {
	if ordered, ok := asList(m["anthropic_content_blocks"]); ok && len(ordered) > 0 {
		if replayed := replayOrderedBlocks(m, ordered); len(replayed) > 0 {
			return true, replayed
		}
	}
	return false, extractPreservedThinkingBlocks(m, opts)
}

// Assistant message converted to Anthropic content blocks
func convertAssistantMessage(m map[string]any, opts convertOptions) map[string]any {
	content := m["content"]
	ordered, blocks := assistantReplayCarrier(m, opts)
	if ordered {
		return map[string]any{"role": "assistant", "content": blocks}
	}
	var relocated map[string]any
	if list, ok := asList(content); ok {
		converted, _ := asList(convertContentToAnthropic(list))
		kept, rel, _ := splitBlankTextBlocks(converted)
		relocated = rel
		blocks = append(blocks, kept...)
	} else if content != nil {
		if s := safeTextRaw(content); strings.TrimSpace(s) != "" {
			blocks = append(blocks, textBlock(s))
		}
	}
	if calls, ok := asList(m["tool_calls"]); ok {
		for _, tc := range calls {
			tcm, ok := pyjson.Dict(tc)
			if !ok || len(tcm) == 0 {
				continue
			}
			fn, _ := pyjson.Dict(tcm["function"])
			args := fn["arguments"]
			if args == nil {
				args = "{}"
			}
			blocks = append(blocks, toolUseBlock(asStr(tcm["id"]), asStr(fn["name"]), parseToolArgs(args)))
		}
	}
	if rc, ok := asString(m["reasoning_content"]); ok && !hasBlockType(blocks, thinkingBlockTypes) {
		blocks = append([]any{map[string]any{"type": "thinking", "thinking": rc}}, blocks...)
	}
	effective := blocks
	if len(effective) == 0 {
		effective = []any{textBlock(emptyTextPlaceholder)}
	}
	applyAssistantCacheControlToLastCacheableBlock(effective, relocated)
	applyAssistantCacheControlToLastCacheableBlock(effective, m["cache_control"])
	return map[string]any{"role": "assistant", "content": effective}
}

func safeTextRaw(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Tool message content resolved into tool_result content blocks or a string
func toolResultContent(m map[string]any) any {
	content := m["content"]
	var multimodal []any
	if cm, ok := asMap(content); ok && cm["_multimodal"] != nil && cm["_multimodal"] != false {
		multimodal = contentPartsToAnthropicBlocks(cm["content"])
		if len(multimodal) == 0 {
			if summary := asStr(cm["text_summary"]); summary != "" {
				multimodal = []any{textBlock(summary)}
			}
		}
	} else if list, ok := asList(content); ok {
		converted := contentPartsToAnthropicBlocks(list)
		if hasBlockType(converted, map[string]bool{"image": true}) {
			multimodal = converted
		}
	}
	if multimodal == nil {
		if stashed, ok := asList(m["_anthropic_content_blocks"]); ok && len(stashed) > 0 {
			if s, ok := asString(content); ok && strings.TrimSpace(s) != "" {
				multimodal = append([]any{textBlock(s)}, stashed...)
			} else {
				multimodal = append([]any{}, stashed...)
			}
		}
	}
	if len(multimodal) > 0 {
		return multimodal
	}
	var text string
	switch c := content.(type) {
	case string:
		text = c
	case nil:
		text = ""
	default:
		if list, ok := asList(c); ok && len(list) == 0 {
			text = ""
		} else if cm, ok := asMap(c); ok && len(cm) == 0 {
			text = ""
		} else {
			b, _ := json.Marshal(c)
			text = string(b)
		}
	}
	if text == "" {
		return "(no output)"
	}
	return text
}

// Appends a tool_result, merging into a trailing tool_result user message
func convertToolMessageToResult(result []map[string]any, m map[string]any) []map[string]any {
	tr := map[string]any{"type": "tool_result", "tool_use_id": sanitizeToolID(asStr(m["tool_call_id"])), "content": toolResultContent(m)}
	carryCacheControl(tr, m, true)
	if len(result) > 0 {
		last := result[len(result)-1]
		if asStr(last["role"]) == "user" {
			if lc, ok := asList(last["content"]); ok && len(lc) > 0 && blockType(lc[0]) == "tool_result" {
				last["content"] = append(lc, tr)
				return result
			}
		}
	}
	return append(result, map[string]any{"role": "user", "content": []any{tr}})
}

func convertUserMessage(content any) map[string]any {
	if list, ok := asList(content); ok {
		converted, _ := asList(convertContentToAnthropic(list))
		return map[string]any{"role": "user", "content": fixBlankTextBlocksInList(converted, "(empty message)")}
	}
	if content == nil {
		return map[string]any{"role": "user", "content": "(empty message)"}
	}
	if s, ok := asString(content); ok && strings.TrimSpace(s) == "" {
		return map[string]any{"role": "user", "content": "(empty message)"}
	}
	return map[string]any{"role": "user", "content": content}
}

func assistantBlockLists(result []map[string]any) []int {
	var idx []int
	for i, m := range result {
		if asStr(m["role"]) == "assistant" {
			if _, ok := asList(m["content"]); ok {
				idx = append(idx, i)
			}
		}
	}
	return idx
}

// Removes tool_use blocks without an adjacent tool_result and tool_results whose tool_use vanished
func stripOrphanedToolBlocks(result []map[string]any) {
	for _, i := range assistantBlockLists(result) {
		m := result[i]
		content, _ := asList(m["content"])
		useIDs := blockIDs(content, "tool_use", "id")
		if len(useIDs) == 0 {
			continue
		}
		adjacent := map[string]bool{}
		if i+1 < len(result) {
			nxt := result[i+1]
			if asStr(nxt["role"]) == "user" {
				if nc, ok := asList(nxt["content"]); ok {
					adjacent = blockIDs(nc, "tool_result", "tool_use_id")
				}
			}
		}
		orphaned := map[string]bool{}
		for id := range useIDs {
			if !adjacent[id] {
				orphaned[id] = true
			}
		}
		if len(orphaned) == 0 {
			continue
		}
		var kept []any
		for _, b := range content {
			bm, _ := asMap(b)
			if blockType(b) == "tool_use" && orphaned[asStr(bm["id"])] {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) != len(content) && hasBlockType(content, thinkingBlockTypes) {
			m["_thinking_signature_invalidated"] = true
		}
		if len(kept) == 0 {
			kept = []any{textBlock("(tool call removed)")}
		}
		m["content"] = kept
	}
	surviving := map[string]bool{}
	for _, i := range assistantBlockLists(result) {
		content, _ := asList(result[i]["content"])
		for id := range blockIDs(content, "tool_use", "id") {
			surviving[id] = true
		}
	}
	for _, m := range result {
		if asStr(m["role"]) != "user" {
			continue
		}
		content, ok := asList(m["content"])
		if !ok {
			continue
		}
		var kept []any
		for _, b := range content {
			bm, _ := asMap(b)
			if blockType(b) == "tool_result" && !surviving[asStr(bm["tool_use_id"])] {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) != len(content) {
			if len(kept) == 0 {
				kept = []any{textBlock("(tool result removed)")}
			}
			m["content"] = kept
		}
	}
}

// Merges two message contents into one block list with each side's blocks kept intact
func concatContent(prev, curr any) []any {
	asBlocks := func(c any) []any {
		if s, ok := asString(c); ok {
			return []any{textBlock(s)}
		}
		list, _ := asList(c)
		return append([]any{}, list...)
	}
	return append(asBlocks(prev), asBlocks(curr)...)
}

// Merges consecutive same role messages to enforce alternation
func mergeConsecutiveRoles(result []map[string]any) []map[string]any {
	var fixed []map[string]any
	for _, m := range result {
		if len(fixed) == 0 || asStr(fixed[len(fixed)-1]["role"]) != asStr(m["role"]) {
			fixed = append(fixed, m)
			continue
		}
		last := fixed[len(fixed)-1]
		if asStr(m["role"]) != "user" {
			if m["_thinking_signature_invalidated"] == true {
				last["_thinking_signature_invalidated"] = true
			}
			if list, ok := asList(m["content"]); ok {
				m["content"] = stripThinking(list)
			}
		}
		last["content"] = concatContent(last["content"], m["content"])
	}
	return fixed
}

// Keeps valid signed thinking and demotes readable unsigned or invalidated thinking to text
func keepValidThinking(content []any, signatureDead bool) []any {
	var out []any
	for _, b := range content {
		if !thinkingBlockTypes[blockType(b)] {
			out = append(out, b)
			continue
		}
		bm, _ := asMap(b)
		redacted := asStr(bm["type"]) == "redacted_thinking"
		var signed string
		if redacted {
			signed = asStr(bm["data"])
		} else {
			signed = asStr(bm["signature"])
		}
		thinking := asStr(bm["thinking"])
		if signed != "" && !signatureDead {
			out = append(out, b)
		} else if (signatureDead || !redacted) && thinking != "" {
			out = append(out, textBlock(thinking))
		}
	}
	return out
}

// Index after the last user message carrying no tool result
func currentToolLoopStart(result []map[string]any) int {
	for i := len(result) - 1; i >= 0; i-- {
		m := result[i]
		if asStr(m["role"]) != "user" {
			continue
		}
		content, ok := asList(m["content"])
		hasResult := false
		if ok {
			for _, b := range content {
				if blockType(b) == "tool_result" {
					hasResult = true
				}
			}
		}
		if !hasResult {
			return i + 1
		}
	}
	return 0
}

// Strips or preserves thinking blocks per endpoint replay contract
func manageThinkingSignatures(result []map[string]any, baseURL, model string) {
	route := anthropicThinkingRoute(baseURL, model)
	loopStart := currentToolLoopStart(result)
	lastAssistant := len(result)
	for i := len(result) - 1; i >= 0; i-- {
		if asStr(result[i]["role"]) == "assistant" {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < loopStart {
		loopStart = lastAssistant
	}
	preservePrior := routeReplaysPriorThinking(route, model)
	for _, idx := range assistantBlockLists(result) {
		m := result[idx]
		content, _ := asList(m["content"])
		switch {
		case route == "kimi":
		case route == "deepseek":
			var kept []any
			for _, b := range content {
				bm, _ := asMap(b)
				if !thinkingBlockTypes[blockType(b)] || (asStr(bm["signature"]) == "" && asStr(bm["data"]) == "") {
					kept = append(kept, b)
				}
			}
			if len(kept) == 0 {
				kept = []any{textBlock(emptyTextPlaceholder)}
			}
			m["content"] = kept
		case route == "third_party" || (idx < loopStart && !preservePrior):
			kept := stripThinking(content)
			if len(kept) == 0 {
				kept = []any{textBlock("(thinking elided)")}
			}
			m["content"] = kept
		default:
			kept := keepValidThinking(content, m["_thinking_signature_invalidated"] == true)
			if len(kept) == 0 {
				kept = []any{textBlock(emptyTextPlaceholder)}
			}
			m["content"] = kept
		}
		content, _ = asList(m["content"])
		for _, b := range content {
			if thinkingBlockTypes[blockType(b)] {
				bm, _ := asMap(b)
				delete(bm, "cache_control")
			}
		}
		delete(m, "_thinking_signature_invalidated")
	}
}

const (
	outboundImageLimit       = 20
	outboundImageHardLimit   = 100
	outboundImageBudgetBytes = 24_000_000
	imageEvictionBatch       = 8
	outboundImageFloor       = 3
	manyImageDimensionLimit  = 2000
	dimensionProbeChars      = 32 * 1024
)

var jpegSOFMarkers = map[byte]bool{0xC0: true, 0xC1: true, 0xC2: true, 0xC3: true, 0xC5: true, 0xC6: true, 0xC7: true, 0xC9: true, 0xCA: true, 0xCB: true, 0xCD: true, 0xCE: true, 0xCF: true}

// Width and height read from PNG or JPEG headers, false when unreadable
func imageDimensionsFromBytes(raw []byte) (int, int, bool) {
	if bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) && len(raw) >= 24 {
		return int(binary.BigEndian.Uint32(raw[16:20])), int(binary.BigEndian.Uint32(raw[20:24])), true
	}
	if bytes.HasPrefix(raw, []byte{0xff, 0xd8}) && len(raw) > 4 {
		i := 2
		for i+9 < len(raw) {
			if raw[i] != 0xFF {
				i++
				continue
			}
			marker := raw[i+1]
			i += 2
			for marker == 0xFF && i < len(raw) {
				marker = raw[i]
				i++
			}
			if marker == 0xD8 || marker == 0xD9 {
				continue
			}
			if marker == 0xDA || i+2 > len(raw) {
				break
			}
			segLen := int(binary.BigEndian.Uint16(raw[i : i+2]))
			if segLen < 2 || i+segLen > len(raw) {
				break
			}
			if jpegSOFMarkers[marker] && segLen >= 7 {
				return int(binary.BigEndian.Uint16(raw[i+5 : i+7])), int(binary.BigEndian.Uint16(raw[i+3 : i+5])), true
			}
			i += segLen
		}
	}
	return 0, 0, false
}

// ImageDimensionsFromBytes is the width and height read from PNG or JPEG headers, false when unreadable
func ImageDimensionsFromBytes(raw []byte) (int, int, bool) { return imageDimensionsFromBytes(raw) }

// Longest side of a data URL or bare base64 image read from its header prefix only
func imageMaxSideFromData(data string) (int, bool) {
	if data == "" {
		return 0, false
	}
	payload := data
	if strings.HasPrefix(payload, "data:") {
		head, rest, found := strings.Cut(payload, ",")
		if !found || !strings.Contains(head, ";base64") {
			return 0, false
		}
		payload = rest
	}
	payload = strings.TrimSpace(payload)
	if len(payload) > dimensionProbeChars {
		payload = payload[:dimensionProbeChars]
	}
	payload = payload[:len(payload)-len(payload)%4]
	if payload == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
		if err != nil && len(raw) == 0 {
			return 0, false
		}
	}
	w, h, ok := imageDimensionsFromBytes(raw)
	if !ok {
		return 0, false
	}
	if w > h {
		return w, true
	}
	return h, true
}

func imageWithinManyImageLimit(data string) bool {
	longest, ok := imageMaxSideFromData(data)
	return ok && longest <= manyImageDimensionLimit
}

func imagesAllWithinManyImageLimit(datas []string) bool {
	for _, d := range datas {
		if !imageWithinManyImageLimit(d) {
			return false
		}
	}
	return true
}

// Whether every image payload fits the per image limit that applies once many images ride one request
func ImagesAllWithinManyImageLimit(datas []string) bool { return imagesAllWithinManyImageLimit(datas) }

func outboundImageBlockLimit(manyImageSafe bool) int {
	if manyImageSafe {
		return outboundImageHardLimit
	}
	return outboundImageLimit
}

// Retire count for one request in isolation, a step function of the overshoot
func retireCountForRequest(carrierBlocks []int, reservedBlocks int, sizes []int, reservedBytes, limit, budget, batch, floor int) int {
	total := len(carrierBlocks)
	blocksKept := make([]int, total+1)
	bytesKept := make([]int, total+1)
	for i := 0; i < total; i++ {
		blocksKept[i+1] = blocksKept[i] + carrierBlocks[i]
		if sizes != nil {
			bytesKept[i+1] = bytesKept[i] + sizes[i]
		}
	}
	bytesFit := func(kept int) bool { return sizes == nil || reservedBytes+bytesKept[kept] <= budget }
	fits := func(kept int) bool { return reservedBlocks+blocksKept[kept] <= limit && bytesFit(kept) }
	if fits(total) {
		return 0
	}
	if floor < 0 {
		floor = 0
	}
	if floor > total {
		floor = total
	}
	if !fits(0) && bytesFit(floor) {
		byteWindow := floor
		for k := floor; k <= total; k++ {
			if bytesFit(k) {
				byteWindow = k
			}
		}
		quantum := batch
		if byteWindow-floor+1 < quantum {
			quantum = byteWindow - floor + 1
		}
		if quantum < 1 {
			quantum = 1
		}
		excess := total - floor
		return excess - excess%quantum
	}
	window := 0
	if fits(0) {
		for k := 0; k <= total; k++ {
			if fits(k) {
				window = k
			}
		}
	}
	quantum := batch
	if window-floor < quantum {
		quantum = window - floor
	}
	if quantum < 1 {
		quantum = 1
	}
	retire := 0
	for retire < total {
		retire += quantum
		if retire > total {
			retire = total
		}
		if fits(total - retire) {
			break
		}
	}
	return retire
}

// How many of the oldest image bearing tool results to retire for the request to fit
func outboundImageRetireCount(carrierBlocksNewestFirst []int, reservedBlocks int, carrierBytesNewestFirst []int, reservedBytes int, manyImageSafe bool) int {
	limit := outboundImageBlockLimit(manyImageSafe)
	total := len(carrierBlocksNewestFirst)
	best := 0
	for m := 1; m <= total; m++ {
		var sizes []int
		if carrierBytesNewestFirst != nil {
			sizes = carrierBytesNewestFirst[total-m:]
		}
		n := retireCountForRequest(carrierBlocksNewestFirst[total-m:], reservedBlocks, sizes, reservedBytes, limit, outboundImageBudgetBytes, imageEvictionBatch, outboundImageFloor)
		if n > best {
			best = n
		}
	}
	return best
}

// Count of newest image carriers to retire so the outbound request stays under the image ceilings
func OutboundImageRetireCount(carrierBlocks []int, reservedBlocks int, carrierBytes []int, reservedBytes int, manyImageSafe bool) int {
	return outboundImageRetireCount(carrierBlocks, reservedBlocks, carrierBytes, reservedBytes, manyImageSafe)
}

// Widens the retire count through the newest earlier retired carrier and records the carriers retired now
func LatchedRetireCount(retire int, carrierIDsNewestFirst []string, retiredIDs map[string]bool) int {
	if retiredIDs == nil {
		return retire
	}
	total := len(carrierIDsNewestFirst)
	for n, cid := range carrierIDsNewestFirst {
		if cid != "" && retiredIDs[cid] {
			retire = max(retire, total-n)
			break
		}
	}
	start := total - retire
	if start < 0 {
		start = max(start+total, 0)
	}
	for _, cid := range carrierIDsNewestFirst[min(start, total):] {
		if cid != "" {
			retiredIDs[cid] = true
		}
	}
	return retire
}

func imageSourceData(block any) (string, bool) {
	bm, ok := asMap(block)
	if !ok {
		return "", false
	}
	src, ok := asMap(bm["source"])
	if !ok {
		return "", false
	}
	data, ok := asString(src["data"])
	return data, ok
}

type imageCarrier struct {
	block  map[string]any
	images []imageRef
}

type imageRef struct {
	data string
	ok   bool
}

// EvictOldScreenshots is the wire pass retiring screenshot payloads past the per request image limit
func EvictOldScreenshots(result []map[string]any) { evictOldScreenshots(result) }

// Retires screenshot payloads once the request would cross the API per request image limit
func evictOldScreenshots(result []map[string]any) {
	var reserved []imageRef
	for _, msg := range result {
		content, _ := asList(msg["content"])
		for _, block := range content {
			if blockType(block) == "image" {
				d, ok := imageSourceData(block)
				reserved = append(reserved, imageRef{d, ok})
			}
		}
	}
	var carriers []imageCarrier
	for mi := len(result) - 1; mi >= 0; mi-- {
		content, _ := asList(result[mi]["content"])
		for bi := len(content) - 1; bi >= 0; bi-- {
			block := content[bi]
			if blockType(block) != "tool_result" {
				continue
			}
			bm, _ := asMap(block)
			inner, ok := asList(bm["content"])
			if !ok || !hasBlockType(inner, map[string]bool{"image": true}) {
				continue
			}
			var images []imageRef
			for _, b := range inner {
				if blockType(b) == "image" {
					d, ok := imageSourceData(b)
					images = append(images, imageRef{d, ok})
				}
			}
			carriers = append(carriers, imageCarrier{bm, images})
		}
	}
	var all []string
	for _, r := range reserved {
		all = append(all, r.data)
	}
	for _, c := range carriers {
		for _, r := range c.images {
			all = append(all, r.data)
		}
	}
	manySafe := imagesAllWithinManyImageLimit(all)
	blocks := make([]int, len(carriers))
	sizes := make([]int, len(carriers))
	for i, c := range carriers {
		blocks[i] = len(c.images)
		for _, r := range c.images {
			if r.ok {
				sizes[i] += len(r.data)
			}
		}
	}
	reservedBytes := 0
	for _, r := range reserved {
		if r.ok {
			reservedBytes += len(r.data)
		}
	}
	retire := outboundImageRetireCount(blocks, len(reserved), sizes, reservedBytes, manySafe)
	for _, c := range carriers[len(carriers)-retire:] {
		inner, _ := asList(c.block["content"])
		replaced := make([]any, 0, len(inner))
		for _, b := range inner {
			if blockType(b) == "image" {
				replaced = append(replaced, textBlock("[screenshot removed to save context]"))
			} else {
				replaced = append(replaced, b)
			}
		}
		c.block["content"] = replaced
	}
}

// Prepends a placeholder user turn when the first message is not a user turn
func ensureLeadingUserTurn(result []map[string]any) []map[string]any {
	if len(result) > 0 && asStr(result[0]["role"]) != "user" {
		return append([]map[string]any{{"role": "user", "content": []any{textBlock(emptyTextPlaceholder)}}}, result...)
	}
	return result
}

// Drops blank text blocks, relocating their marker, substituting one placeholder when nothing survives
func fixBlankTextBlocksInList(blocks []any, placeholder string) []any {
	kept, relocated, _ := splitBlankTextBlocks(blocks)
	if len(kept) == 0 {
		kept = []any{textBlock(placeholder)}
	}
	applyAssistantCacheControlToLastCacheableBlock(kept, relocated)
	return kept
}

// Final guard against blank text blocks including inside tool_result content
func scrubBlankTextBlocks(result []map[string]any) {
	for _, msg := range result {
		role := asStr(msg["role"])
		content, ok := asList(msg["content"])
		if !ok || len(content) == 0 {
			continue
		}
		placeholder := "(empty message)"
		if role == "assistant" {
			placeholder = emptyTextPlaceholder
		}
		fixed := fixBlankTextBlocksInList(content, placeholder)
		msg["content"] = fixed
		for _, blk := range fixed {
			if blockType(blk) != "tool_result" {
				continue
			}
			bm, _ := asMap(blk)
			if inner, ok := asList(bm["content"]); ok && len(inner) > 0 {
				bm["content"] = fixBlankTextBlocksInList(inner, "(no output)")
			}
		}
	}
}

// System content as the system param, a string or a block list when a marker is present
func convertSystemContent(content any) any {
	list, ok := asList(content)
	if !ok {
		return content
	}
	marked := false
	for _, p := range list {
		if pm, ok := asMap(p); ok && pm["cache_control"] != nil {
			marked = true
		}
	}
	if !marked {
		var texts []string
		for _, p := range list {
			if pm, ok := asMap(p); ok && asStr(pm["type"]) == "text" {
				texts = append(texts, asStr(pm["text"]))
			}
		}
		return strings.Join(texts, "\n")
	}
	out := make([]any, 0, len(list))
	for _, p := range list {
		pm, ok := asMap(p)
		if !ok {
			continue
		}
		if _, isStr := asString(pm["text"]); isBlankTextBlock(pm) && isStr {
			copyP := map[string]any{}
			for k, v := range pm {
				copyP[k] = v
			}
			copyP["text"] = emptyTextPlaceholder
			out = append(out, copyP)
			continue
		}
		out = append(out, pm)
	}
	return out
}

// Inputs steering thinking replay policy and Bedrock issuer resolution during conversion
type convertOptions struct {
	baseURL           string
	model             string
	resolveProfileARN func(arn string) string
}

// OpenAI shaped dicts converted to the Anthropic system param and message list
func convertMessagesToAnthropic(messages []map[string]any, opts convertOptions) (any, []map[string]any) {
	var system any
	var result []map[string]any
	for _, m := range messages {
		role := asStr(m["role"])
		if role == "" {
			role = "user"
		}
		switch role {
		case "system":
			system = convertSystemContent(m["content"])
		case "assistant":
			result = append(result, convertAssistantMessage(m, opts))
		case "tool":
			result = convertToolMessageToResult(result, m)
		default:
			result = append(result, convertUserMessage(m["content"]))
		}
	}
	stripOrphanedToolBlocks(result)
	result = mergeConsecutiveRoles(result)
	result = ensureLeadingUserTurn(result)
	manageThinkingSignatures(result, opts.baseURL, opts.model)
	evictOldScreenshots(result)
	scrubBlankTextBlocks(result)
	return system, result
}

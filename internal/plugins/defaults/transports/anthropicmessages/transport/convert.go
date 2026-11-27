package transport

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
)

// Text that stands in for a block the API refuses empty
const emptyText = "(empty)"

var (
	thinkingTypes  = []string{"thinking", "redacted_thinking"}
	cacheableTypes = []string{"text", "tool_use"}
	// Region prefixes of Bedrock inference profiles
	bedrockPrefixes = []string{"global.", "us.", "eu.", "apac.", "ap.", "au.", "jp.", "ca.", "sa.", "me.", "af."}
	imageTypes      = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
)

func kind(b any) string {
	block, _ := b.(map[string]any)
	kind, _ := block["type"].(string)
	return kind
}

func hasKind(blocks []any, kinds []string) bool {
	return slices.ContainsFunc(blocks, func(b any) bool { return slices.Contains(kinds, kind(b)) })
}

// A text block whose text holds nothing but space
func blank(b any) bool {
	if kind(b) != "text" {
		return false
	}
	block, _ := b.(map[string]any)
	s, ok := block["text"].(string)
	return !ok || strings.TrimSpace(s) == ""
}

func marker(b any) map[string]any {
	block, _ := b.(map[string]any)
	m, _ := block["cache_control"].(map[string]any)
	return m
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// Text block keeping the citations the text came with
func cited(s any, citations any) map[string]any {
	block := map[string]any{"type": "text", "text": s}
	if list, ok := citations.([]any); ok && len(list) > 0 {
		block["citations"] = list
	}
	return block
}

// Tool arguments decoded, an empty object for arguments that aren't JSON
func arguments(raw any) any {
	s, ok := raw.(string)
	if !ok {
		return raw
	}
	var decoded any
	if json.Unmarshal([]byte(s), &decoded) != nil {
		return map[string]any{}
	}
	return decoded
}

var badToolID = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// Tool use id the API takes: letters, digits, underscores and dashes
func toolID(id string) string {
	if out := badToolID.ReplaceAllString(id, "_"); out != "" {
		return out
	}
	return "tool_0"
}

func toolUse(id, name string, input any) map[string]any {
	return map[string]any{"type": "tool_use", "id": toolID(id), "name": name, "input": input}
}

func carry(out map[string]any, from any) map[string]any {
	if m := marker(from); m != nil {
		out["cache_control"] = transport.DeepCopy(m)
	}
	return out
}

func isBedrockModel(model string) bool {
	m := strings.ToLower(model)
	return provider.HasPrefix(m, append(slices.Clone(bedrockPrefixes), "anthropic.")...)
}

// Model id as the API takes it: without an anthropic/ prefix, a Claude id's dots as dashes unless kept
func modelName(model string, dots bool) string {
	if strings.HasPrefix(strings.ToLower(model), "anthropic/") {
		model = model[len("anthropic/"):]
	}
	if !dots && !isBedrockModel(model) && strings.HasPrefix(strings.ToLower(model), "claude-") {
		model = strings.ReplaceAll(model, ".", "-")
	}
	return model
}

// Tool schema the API takes: nullable unions collapsed, combinators at its top dropped, an object always with its properties
func inputSchema(schema any) map[string]any {
	m, ok := stripNullable(transport.DeepCopy(schema)).(map[string]any)
	if !ok || len(m) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	banned := false
	for _, k := range []string{"oneOf", "allOf", "anyOf"} {
		if _, has := m[k]; has {
			delete(m, k)
			banned = true
		}
	}
	if _, has := m["type"]; banned && !has {
		m["type"] = "object"
	}
	if m["type"] == "object" {
		if _, ok := m["properties"].(map[string]any); !ok {
			m["properties"] = map[string]any{}
		}
	}
	return m
}

// Metadata of a union kept on the branch it collapses to
var unionMeta = []string{"title", "description", "default", "examples"}

// Schema with every anyOf or oneOf union of one schema and null collapsed to that schema, the union's title, description, default and
// examples carried onto it
func stripNullable(node any) any {
	switch n := node.(type) {
	case []any:
		for i, item := range n {
			n[i] = stripNullable(item)
		}
		return n
	case map[string]any:
		for k, v := range n {
			n[k] = stripNullable(v)
		}
		for _, key := range []string{"anyOf", "oneOf"} {
			branches, ok := n[key].([]any)
			if !ok {
				continue
			}
			kept := slices.DeleteFunc(slices.Clone(branches), func(b any) bool { branch, _ := b.(map[string]any); return branch["type"] == "null" })
			if len(kept) != 1 || len(kept) == len(branches) {
				continue
			}
			replacement, _ := transport.DeepCopy(kept[0]).(map[string]any)
			if replacement == nil {
				replacement = map[string]any{}
			}
			for _, meta := range unionMeta {
				_, ref := replacement["$ref"]
				if value, present := n[meta]; present && replacement[meta] == nil && !(meta == "default" && ref) {
					replacement[meta] = value
				}
			}
			return stripNullable(replacement)
		}
		return n
	}
	return node
}

// OpenAI shaped tools as the API's, a name met again dropped
func convertTools(tools []map[string]any) []map[string]any {
	var out []map[string]any
	seen := map[string]bool{}
	for _, t := range tools {
		function, _ := t["function"].(map[string]any)
		name := transport.Text(function["name"])
		if name != "" && seen[name] {
			continue
		}
		seen[name] = true
		params := function["parameters"]
		if params == nil {
			params = map[string]any{}
		}
		out = append(out, carry(map[string]any{"name": name, "description": transport.Text(function["description"]), "input_schema": inputSchema(params)}, t))
	}
	return out
}

// Media type of an inline image the API refuses, empty for the rasters it takes and for anything that isn't inline
func unsupportedImage(u string) string {
	header, _, _ := strings.Cut(strings.ToLower(u), ",")
	rest, ok := strings.CutPrefix(header, "data:image/")
	if !ok {
		return ""
	}
	subtype, _, _ := strings.Cut(rest, ";")
	subtype = strings.TrimSpace(subtype)
	subtype = cmp.Or(subtype, "unknown")
	media := "image/" + subtype
	if subtype == "jpg" {
		media = "image/jpeg"
	}
	if slices.Contains(imageTypes, media) {
		return ""
	}
	return media
}

// PNG data URL of an SVG data URL, rendered by rsvg-convert or Inkscape; empty when neither is there or both fail
func rasterize(u string) string {
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
	svg, png := filepath.Join(dir, "inline.svg"), filepath.Join(dir, "inline.png")
	if os.WriteFile(svg, raw, 0o600) != nil {
		return ""
	}
	for _, command := range [][]string{{"rsvg-convert", "-o", png, svg}, {"inkscape", svg, "--export-type=png", "--export-filename=" + png}} {
		if _, err := exec.LookPath(command[0]); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := exec.CommandContext(ctx, command[0], command[1:]...).Run()
		cancel()
		if err != nil {
			continue
		}
		if data, err := os.ReadFile(png); err == nil && len(data) > 0 {
			return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
		}
	}
	return ""
}

// Image block of an image URL: a remote one by its URL, an inline one by its data, an SVG rendered first; a note for a format the API
// refuses
func imageBlock(u string) map[string]any {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "data:") {
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
	}
	refused := unsupportedImage(u)
	if refused == "image/svg+xml" {
		if png := rasterize(u); png != "" {
			u, refused = png, ""
		}
	}
	if refused != "" {
		return textBlock("[image omitted: " + refused + " is not a supported image format]")
	}
	header, data, _ := strings.Cut(u, ",")
	mime, _, _ := strings.Cut(strings.TrimPrefix(header, "data:"), ";")
	media := "image/jpeg"
	if strings.HasPrefix(strings.TrimSpace(mime), "image/") {
		media = strings.TrimSpace(mime)
	}
	if strings.EqualFold(media, "image/jpg") {
		media = "image/jpeg"
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}}
}

// One OpenAI content part as an API block, other block types kept as they are
func convertPart(part any) any {
	if part == nil {
		return nil
	}
	p, ok := part.(map[string]any)
	if !ok {
		if s, ok := part.(string); ok {
			return textBlock(s)
		}
		raw, _ := json.Marshal(part)
		return textBlock(string(raw))
	}
	var block map[string]any
	switch kind := transport.Text(p["type"]); kind {
	case "input_text", "text":
		var citations any
		if kind == "text" {
			citations = p["citations"]
		}
		value := p["text"]
		if value == nil {
			value = ""
		}
		block = cited(value, citations)
	case "image_url", "input_image":
		u := transport.Text(p["image_url"])
		if inner, ok := p["image_url"].(map[string]any); ok {
			u = transport.Text(inner["url"])
		}
		block = imageBlock(u)
	default:
		return transport.DeepCopy(p)
	}
	if m := marker(p); m != nil {
		if _, has := block["cache_control"]; !has {
			block["cache_control"] = transport.DeepCopy(m)
		}
	}
	return block
}

func convertContent(content any) any {
	list, ok := content.([]any)
	if !ok {
		return content
	}
	out := make([]any, 0, len(list))
	for _, p := range list {
		if b := convertPart(p); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// Text and image blocks of a tool result's parts
func resultBlocks(parts any) []any {
	list, _ := parts.([]any)
	var out []any
	for _, p := range list {
		block, _ := convertPart(p).(map[string]any)
		switch transport.Text(block["type"]) {
		case "text":
			if s := transport.Text(block["text"]); s != "" {
				out = append(out, textBlock(s))
			}
		case "image":
			if source, ok := block["source"].(map[string]any); ok && len(source) > 0 {
				out = append(out, map[string]any{"type": "image", "source": source})
			}
		}
	}
	return out
}

// Thinking blocks a replayed assistant message carries in its details, none from a turn a model other than Claude made through Bedrock
func preservedThinking(message map[string]any) []any {
	details, _ := message["reasoning_details"].([]any)
	if foreign, _ := message["foreign_issuer"].(bool); foreign {
		return nil
	}
	var out []any
	for _, d := range details {
		if detail, ok := d.(map[string]any); ok && slices.Contains(thinkingTypes, strings.ToLower(strings.TrimSpace(transport.Text(detail["type"])))) {
			out = append(out, transport.DeepCopy(detail))
		}
	}
	return out
}

// A stored block kept to what the API takes as input, nil for a type it doesn't
func sanitize(b any) map[string]any {
	block, ok := b.(map[string]any)
	if !ok {
		return nil
	}
	switch transport.Text(block["type"]) {
	case "text":
		if blank(block) {
			return nil
		}
		return carry(cited(block["text"], block["citations"]), block)
	case "thinking":
		out := map[string]any{"type": "thinking", "thinking": transport.Text(block["thinking"])}
		if signature := transport.Text(block["signature"]); signature != "" {
			out["signature"] = signature
		}
		return out
	case "redacted_thinking":
		if data := transport.Text(block["data"]); data != "" {
			return map[string]any{"type": "redacted_thinking", "data": data}
		}
	case "tool_use":
		input := block["input"]
		if input == nil {
			input = map[string]any{}
		}
		return carry(toolUse(transport.Text(block["id"]), transport.Text(block["name"]), input), block)
	case "image":
		if source, ok := block["source"].(map[string]any); ok {
			return map[string]any{"type": "image", "source": source}
		}
	}
	return nil
}

// Puts the marker on the last block that can carry one, unless it carries one already
func markLast(blocks []any, m any) {
	cc, ok := m.(map[string]any)
	if !ok || cc == nil {
		return
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if slices.Contains(cacheableTypes, kind(blocks[i])) {
			block := blocks[i].(map[string]any)
			if _, has := block["cache_control"]; !has {
				block["cache_control"] = transport.DeepCopy(cc)
			}
			return
		}
	}
}

// Assistant turn rebuilt from the blocks it came in, in their order, tool inputs taken from its tool calls as they stand now
func replayOrdered(message map[string]any, ordered []any) []any {
	inputs := map[string]any{}
	calls, _ := message["tool_calls"].([]any)
	for _, c := range calls {
		call, _ := c.(map[string]any)
		function, _ := call["function"].(map[string]any)
		args := function["arguments"]
		if args == nil {
			args = "{}"
		}
		inputs[toolID(transport.Text(call["id"]))] = arguments(args)
	}
	var out []any
	var relocated map[string]any
	droppedBlank := false
	for _, b := range ordered {
		clean := sanitize(b)
		if clean == nil {
			droppedBlank = droppedBlank || kind(b) == "text"
			if m := marker(b); m != nil {
				relocated = m
			}
			continue
		}
		if clean["type"] == "tool_use" {
			if input, ok := inputs[transport.Text(clean["id"])]; ok {
				clean["input"] = input
			}
		}
		out = append(out, clean)
	}
	if !hasKind(out, cacheableTypes) && (droppedBlank || relocated != nil) {
		out = append(out, textBlock(emptyText))
	}
	if len(out) == 0 {
		return nil
	}
	markLast(out, relocated)
	markLast(out, message["cache_control"])
	if content, ok := message["content"].([]any); ok {
		for _, part := range content {
			if m := marker(part); m != nil {
				markLast(out, m)
				break
			}
		}
	}
	return out
}

// Kept blocks with blank text dropped, and the marker the last dropped one carried
func dropBlank(blocks []any) ([]any, map[string]any) {
	var kept []any
	var relocated map[string]any
	for _, b := range blocks {
		if blank(b) {
			if m := marker(b); m != nil {
				relocated = m
			}
			continue
		}
		kept = append(kept, b)
	}
	return kept, relocated
}

func convertAssistant(message map[string]any) map[string]any {
	if ordered, ok := message["anthropic_content_blocks"].([]any); ok && len(ordered) > 0 {
		if blocks := replayOrdered(message, ordered); len(blocks) > 0 {
			return map[string]any{"role": "assistant", "content": blocks}
		}
	}
	blocks := preservedThinking(message)
	var relocated map[string]any
	switch content := message["content"].(type) {
	case []any:
		converted, _ := convertContent(content).([]any)
		var kept []any
		kept, relocated = dropBlank(converted)
		blocks = append(blocks, kept...)
	case string:
		if strings.TrimSpace(content) != "" {
			blocks = append(blocks, textBlock(content))
		}
	}
	calls, _ := message["tool_calls"].([]any)
	for _, c := range calls {
		call, _ := c.(map[string]any)
		if len(call) == 0 {
			continue
		}
		function, _ := call["function"].(map[string]any)
		args := function["arguments"]
		if args == nil {
			args = "{}"
		}
		blocks = append(blocks, toolUse(transport.Text(call["id"]), transport.Text(function["name"]), arguments(args)))
	}
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" && !hasKind(blocks, thinkingTypes) {
		blocks = append([]any{map[string]any{"type": "thinking", "thinking": reasoning}}, blocks...)
	}
	if len(blocks) == 0 {
		blocks = []any{textBlock(emptyText)}
	}
	markLast(blocks, relocated)
	markLast(blocks, message["cache_control"])
	return map[string]any{"role": "assistant", "content": blocks}
}

// Content of a tool result: its images with its text, else its text, "(no output)" for nothing
func resultContent(message map[string]any) any {
	if list, ok := message["content"].([]any); ok {
		if converted := resultBlocks(list); hasKind(converted, []string{"image"}) {
			return converted
		}
	}
	var s string
	switch c := message["content"].(type) {
	case string:
		s = c
	case nil:
	case []any:
		var parts []string
		for _, p := range c {
			if part, ok := p.(map[string]any); ok && part["type"] == "text" {
				parts = append(parts, transport.Text(part["text"]))
			}
		}
		s = strings.Join(parts, "\n")
	default:
		raw, _ := json.Marshal(c)
		s = string(raw)
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

// Appends a tool result, into the user message of tool results it follows when there is one
func appendResult(result []map[string]any, message map[string]any) []map[string]any {
	tr := carry(map[string]any{"type": "tool_result", "tool_use_id": toolID(transport.Text(message["tool_call_id"])), "content": resultContent(message)}, message)
	if n := len(result); n > 0 && result[n-1]["role"] == "user" {
		if content, ok := result[n-1]["content"].([]any); ok && len(content) > 0 && kind(content[0]) == "tool_result" {
			result[n-1]["content"] = append(content, tr)
			return result
		}
	}
	return append(result, map[string]any{"role": "user", "content": []any{tr}})
}

func convertUser(content any) map[string]any {
	if list, ok := content.([]any); ok {
		converted, _ := convertContent(list).([]any)
		return map[string]any{"role": "user", "content": fillBlank(converted, "(empty message)")}
	}
	if s, ok := content.(string); !ok || strings.TrimSpace(s) == "" {
		return map[string]any{"role": "user", "content": "(empty message)"}
	}
	return map[string]any{"role": "user", "content": content}
}

func ids(blocks []any, kind, field string) map[string]bool {
	out := map[string]bool{}
	for _, b := range blocks {
		if block, ok := b.(map[string]any); ok && block["type"] == kind {
			out[transport.Text(block[field])] = true
		}
	}
	return out
}

// Takes out tool uses no tool result follows, and tool results whose tool use is gone; a turn that lost a block next to signed thinking
// is marked so its thinking is no longer replayed as signed
func dropOrphans(result []map[string]any) {
	for i, message := range result {
		content, ok := message["content"].([]any)
		if message["role"] != "assistant" || !ok {
			continue
		}
		uses := ids(content, "tool_use", "id")
		if len(uses) == 0 {
			continue
		}
		answered := map[string]bool{}
		if i+1 < len(result) && result[i+1]["role"] == "user" {
			if next, ok := result[i+1]["content"].([]any); ok {
				answered = ids(next, "tool_result", "tool_use_id")
			}
		}
		var kept []any
		for _, b := range content {
			if kind(b) == "tool_use" && !answered[transport.Text(b.(map[string]any)["id"])] {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == len(content) {
			continue
		}
		if hasKind(content, thinkingTypes) {
			message["invalidated"] = true
		}
		if len(kept) == 0 {
			kept = []any{textBlock("(tool call removed)")}
		}
		message["content"] = kept
	}
	surviving := map[string]bool{}
	for _, message := range result {
		if content, ok := message["content"].([]any); ok && message["role"] == "assistant" {
			for id := range ids(content, "tool_use", "id") {
				surviving[id] = true
			}
		}
	}
	for _, message := range result {
		content, ok := message["content"].([]any)
		if message["role"] != "user" || !ok {
			continue
		}
		var kept []any
		for _, b := range content {
			if kind(b) == "tool_result" && !surviving[transport.Text(b.(map[string]any)["tool_use_id"])] {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) != len(content) {
			if len(kept) == 0 {
				kept = []any{textBlock("(tool result removed)")}
			}
			message["content"] = kept
		}
	}
}

func blocksOf(content any) []any {
	if s, ok := content.(string); ok {
		return []any{textBlock(s)}
	}
	list, _ := content.([]any)
	return slices.Clone(list)
}

// Messages of one role in a row merged into one, the thinking of a merged assistant turn dropped
func mergeRoles(result []map[string]any) []map[string]any {
	var fixed []map[string]any
	for _, message := range result {
		if len(fixed) == 0 || fixed[len(fixed)-1]["role"] != message["role"] {
			fixed = append(fixed, message)
			continue
		}
		last := fixed[len(fixed)-1]
		if message["role"] != "user" {
			if message["invalidated"] == true {
				last["invalidated"] = true
			}
			if list, ok := message["content"].([]any); ok {
				message["content"] = slices.DeleteFunc(slices.Clone(list), func(b any) bool { return slices.Contains(thinkingTypes, kind(b)) })
			}
		}
		last["content"] = append(blocksOf(last["content"]), blocksOf(message["content"])...)
	}
	return fixed
}

// Signed thinking kept as it is; readable thinking without a signature, or whose signature a dropped block voided, kept as text
func validThinking(content []any, voided bool) []any {
	var out []any
	for _, b := range content {
		if !slices.Contains(thinkingTypes, kind(b)) {
			out = append(out, b)
			continue
		}
		block := b.(map[string]any)
		redacted := block["type"] == "redacted_thinking"
		signed := transport.Text(block["signature"])
		if redacted {
			signed = transport.Text(block["data"])
		}
		thought := transport.Text(block["thinking"])
		switch {
		case signed != "" && !voided:
			out = append(out, b)
		case (voided || !redacted) && thought != "":
			out = append(out, textBlock(thought))
		}
	}
	return out
}

// Index just after the last user message that holds no tool result: where the current tool loop starts
func loopStart(result []map[string]any) int {
	for i := len(result) - 1; i >= 0; i-- {
		content, ok := result[i]["content"].([]any)
		if result[i]["role"] == "user" && !(ok && hasKind(content, []string{"tool_result"})) {
			return i + 1
		}
	}
	return 0
}

// Thinking blocks kept or taken out as the endpoint wants them replayed: Kimi keeps them all, DeepSeek its own unsigned ones, a third party
// none, and Anthropic's own signed ones of the current tool loop, and of earlier turns for models that keep them in view
func manageThinking(result []map[string]any, base, model string) {
	route := thinkingRoute(base, model)
	start := loopStart(result)
	for i := len(result) - 1; i >= 0; i-- {
		if result[i]["role"] == "assistant" {
			start = min(start, i)
			break
		}
	}
	prior := replaysPriorThinking(route, model)
	for i, message := range result {
		content, ok := message["content"].([]any)
		if message["role"] != "assistant" || !ok {
			continue
		}
		switch {
		case route == "kimi":
		case route == "deepseek":
			content = slices.DeleteFunc(slices.Clone(content), func(b any) bool {
				block, _ := b.(map[string]any)
				return slices.Contains(thinkingTypes, kind(b)) && (transport.Text(block["signature"]) != "" || transport.Text(block["data"]) != "")
			})
			if len(content) == 0 {
				content = []any{textBlock(emptyText)}
			}
		case route == "third_party" || i < start && !prior:
			content = slices.DeleteFunc(slices.Clone(content), func(b any) bool { return slices.Contains(thinkingTypes, kind(b)) })
			if len(content) == 0 {
				content = []any{textBlock("(thinking elided)")}
			}
		default:
			content = validThinking(content, message["invalidated"] == true)
			if len(content) == 0 {
				content = []any{textBlock(emptyText)}
			}
		}
		for _, b := range content {
			if slices.Contains(thinkingTypes, kind(b)) {
				delete(b.(map[string]any), "cache_control")
			}
		}
		message["content"] = content
		delete(message, "invalidated")
	}
}

// Image limits of one request: blocks, blocks when every image is small, bytes, how many carriers are retired at once, how many are
// always kept, the longest side of a small image, and how much of an image is read for its size
const (
	imageLimit      = 20
	imageHardLimit  = 100
	imageBudget     = 24_000_000
	retireBatch     = 8
	imageFloor      = 3
	smallImageSide  = 2000
	sizeProbeLength = 32 * 1024
)

var startOfFrame = []byte{0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF}

// Width and height a PNG or JPEG header states
func dimensions(raw []byte) (int, int, bool) {
	if bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) && len(raw) >= 24 {
		return int(binary.BigEndian.Uint32(raw[16:20])), int(binary.BigEndian.Uint32(raw[20:24])), true
	}
	if !bytes.HasPrefix(raw, []byte{0xff, 0xd8}) || len(raw) <= 4 {
		return 0, 0, false
	}
	for i := 2; i+9 < len(raw); {
		if raw[i] != 0xFF {
			i++
			continue
		}
		mark := raw[i+1]
		i += 2
		for mark == 0xFF && i < len(raw) {
			mark = raw[i]
			i++
		}
		if mark == 0xD8 || mark == 0xD9 {
			continue
		}
		if mark == 0xDA || i+2 > len(raw) {
			break
		}
		size := int(binary.BigEndian.Uint16(raw[i : i+2]))
		if size < 2 || i+size > len(raw) {
			break
		}
		if slices.Contains(startOfFrame, mark) && size >= 7 {
			return int(binary.BigEndian.Uint16(raw[i+5 : i+7])), int(binary.BigEndian.Uint16(raw[i+3 : i+5])), true
		}
		i += size
	}
	return 0, 0, false
}

// Whether an image's longest side is within the limit of a request with many images, read from its header
func small(data string) bool {
	payload := strings.TrimSpace(data)
	if len(payload) > sizeProbeLength {
		payload = payload[:sizeProbeLength]
	}
	payload = payload[:len(payload)-len(payload)%4]
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		raw, _ = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
	}
	w, h, ok := dimensions(raw)
	return ok && max(w, h) <= smallImageSide
}

// Carriers to retire, oldest first, for one request whose newest carriers are those given: the overshoot of the block limit and the byte
// budget rounded up to whole batches, the floor of newest carriers always kept when the bytes allow
func retirement(blocks []int, reserved int, sizes []int, reservedBytes, limit int) int {
	total := len(blocks)
	keptBlocks, keptBytes := make([]int, total+1), make([]int, total+1)
	for i := range total {
		keptBlocks[i+1] = keptBlocks[i] + blocks[i]
		keptBytes[i+1] = keptBytes[i] + sizes[i]
	}
	bytesFit := func(n int) bool { return reservedBytes+keptBytes[n] <= imageBudget }
	fits := func(n int) bool { return reserved+keptBlocks[n] <= limit && bytesFit(n) }
	if fits(total) {
		return 0
	}
	floor := min(imageFloor, total)
	if !fits(0) && bytesFit(floor) {
		window := floor
		for k := floor; k <= total; k++ {
			if bytesFit(k) {
				window = k
			}
		}
		quantum := max(min(retireBatch, window-floor+1), 1)
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
	quantum := max(min(retireBatch, window-floor), 1)
	retire := 0
	for retire < total {
		retire = min(retire+quantum, total)
		if fits(total - retire) {
			break
		}
	}
	return retire
}

// Replaces the screenshots of the oldest tool results with a note once the request would carry more images, or more image bytes, than the
// API takes in one request
func evictScreenshots(result []map[string]any) {
	type carrier struct {
		block map[string]any
		data  []string
	}
	var reserved []string
	for _, message := range result {
		content, _ := message["content"].([]any)
		for _, b := range content {
			if kind(b) == "image" {
				source, _ := b.(map[string]any)["source"].(map[string]any)
				reserved = append(reserved, transport.Text(source["data"]))
			}
		}
	}
	var carriers []carrier
	for mi := len(result) - 1; mi >= 0; mi-- {
		content, _ := result[mi]["content"].([]any)
		for bi := len(content) - 1; bi >= 0; bi-- {
			block, ok := content[bi].(map[string]any)
			inner, _ := block["content"].([]any)
			if !ok || block["type"] != "tool_result" || !hasKind(inner, []string{"image"}) {
				continue
			}
			c := carrier{block: block}
			for _, b := range inner {
				if kind(b) == "image" {
					source, _ := b.(map[string]any)["source"].(map[string]any)
					c.data = append(c.data, transport.Text(source["data"]))
				}
			}
			carriers = append(carriers, c)
		}
	}
	every := slices.Clone(reserved)
	for _, c := range carriers {
		every = append(every, c.data...)
	}
	limit := imageLimit
	if !slices.ContainsFunc(every, func(d string) bool { return !small(d) }) {
		limit = imageHardLimit
	}
	blocks, sizes := make([]int, len(carriers)), make([]int, len(carriers))
	for i, c := range carriers {
		blocks[i] = len(c.data)
		for _, d := range c.data {
			sizes[i] += len(d)
		}
	}
	reservedBytes := 0
	for _, d := range reserved {
		reservedBytes += len(d)
	}
	retire := 0
	for m := 1; m <= len(carriers); m++ {
		retire = max(retire, retirement(blocks[len(carriers)-m:], len(reserved), sizes[len(carriers)-m:], reservedBytes, limit))
	}
	for _, c := range carriers[len(carriers)-retire:] {
		inner, _ := c.block["content"].([]any)
		replaced := make([]any, 0, len(inner))
		for _, b := range inner {
			if kind(b) == "image" {
				replaced = append(replaced, textBlock("[screenshot removed to save context]"))
			} else {
				replaced = append(replaced, b)
			}
		}
		c.block["content"] = replaced
	}
}

// Blank text blocks dropped, their marker moved to the last block left, the placeholder in place of a list left empty
func fillBlank(blocks []any, placeholder string) []any {
	kept, relocated := dropBlank(blocks)
	if len(kept) == 0 {
		kept = []any{textBlock(placeholder)}
	}
	markLast(kept, relocated)
	return kept
}

// Last pass taking blank text blocks out of every message and tool result
func scrubBlank(result []map[string]any) {
	for _, message := range result {
		content, ok := message["content"].([]any)
		if !ok || len(content) == 0 {
			continue
		}
		placeholder := "(empty message)"
		if message["role"] == "assistant" {
			placeholder = emptyText
		}
		fixed := fillBlank(content, placeholder)
		message["content"] = fixed
		for _, b := range fixed {
			block, ok := b.(map[string]any)
			if inner, isList := block["content"].([]any); ok && block["type"] == "tool_result" && isList && len(inner) > 0 {
				block["content"] = fillBlank(inner, "(no output)")
			}
		}
	}
}

// System prompt as the system field: its text, or its blocks when one carries a cache marker
func convertSystem(content any) any {
	list, ok := content.([]any)
	if !ok {
		return content
	}
	if !slices.ContainsFunc(list, func(p any) bool { return marker(p) != nil }) {
		var texts []string
		for _, p := range list {
			if kind(p) == "text" {
				texts = append(texts, transport.Text(p.(map[string]any)["text"]))
			}
		}
		return strings.Join(texts, "\n")
	}
	out := make([]any, 0, len(list))
	for _, p := range list {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if _, isText := part["text"].(string); isText && blank(part) {
			part = transport.DeepCopy(part).(map[string]any)
			part["text"] = emptyText
		}
		out = append(out, part)
	}
	return out
}

// OpenAI shaped messages as the system field and the messages of a request to the endpoint
func convert(messages []map[string]any, base, model string) (any, []map[string]any) {
	var system any
	var result []map[string]any
	for _, m := range messages {
		switch transport.Text(m["role"]) {
		case "system":
			system = convertSystem(m["content"])
		case "assistant":
			result = append(result, convertAssistant(m))
		case "tool":
			result = appendResult(result, m)
		default:
			result = append(result, convertUser(m["content"]))
		}
	}
	dropOrphans(result)
	result = mergeRoles(result)
	if len(result) > 0 && result[0]["role"] != "user" {
		result = append([]map[string]any{{"role": "user", "content": []any{textBlock(emptyText)}}}, result...)
	}
	manageThinking(result, base, model)
	evictScreenshots(result)
	scrubBlank(result)
	return system, result
}

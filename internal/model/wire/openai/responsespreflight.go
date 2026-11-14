package openai

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Bar that stands in for the ASCII one so a Harmony token stays readable without being a wire token
const fullwidthBar = "｜"

// Rewrites literal Harmony control tokens, matching through invisible format characters the backend strips first
func neutralizeHarmony(text string) string {
	if !strings.Contains(text, "<") || !strings.Contains(text, "|") {
		return text
	}
	replacement := "<" + fullwidthBar + "${1}" + fullwidthBar + ">"
	if strings.IndexFunc(text, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) < 0 {
		return harmonyToken.ReplaceAllString(text, replacement)
	}
	var visible strings.Builder
	var starts, ends []int
	for i, r := range text {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		for range len(string(r)) {
			starts, ends = append(starts, i), append(ends, i+len(string(r)))
		}
		visible.WriteRune(r)
	}
	var out strings.Builder
	cursor := 0
	for _, m := range harmonyToken.FindAllStringSubmatchIndex(visible.String(), -1) {
		start, end := starts[m[0]], ends[m[1]-1]
		out.WriteString(text[cursor:start])
		out.WriteString("<" + fullwidthBar + visible.String()[m[2]:m[3]] + fullwidthBar + ">")
		cursor = end
	}
	out.WriteString(text[cursor:])
	return out.String()
}

// Neutralizes every string of a JSON value, refusing a reserved token inside an object key
func neutralizeStructure(v wire.Value) (json.RawMessage, error) {
	switch v.Kind() {
	case wire.KindString:
		return wire.Quote(neutralizeHarmony(v.Text())), nil
	case wire.KindArray:
		elems := v.Elems()
		out := make([]json.RawMessage, len(elems))
		for i, e := range elems {
			raw, err := neutralizeStructure(e)
			if err != nil {
				return nil, err
			}
			out[i] = raw
		}
		return wire.Array(out...), nil
	case wire.KindObject:
		d := &wire.Dict{}
		for _, f := range v.Fields() {
			if neutralizeHarmony(f.Key) != f.Key {
				return nil, &RequestError{"Reserved Harmony tokens in a JSON object key cannot be neutralized without changing its contract."}
			}
			raw, err := neutralizeStructure(f.Value)
			if err != nil {
				return nil, err
			}
			d.Set(f.Key, raw)
		}
		return d.JSON(), nil
	}
	return v.Raw(), nil
}

// Tool-call arguments as a non-empty JSON string
func coerceArguments(v wire.Value) string {
	text := "{}"
	switch {
	case v.IsObject():
		text, _ = wire.FormatJSON(v.Raw(), wire.TextLayout{Spaced: true, UTF8: true})
	case v.Exists():
		text = v.Display()
	}
	if text = strip(text); text == "" {
		return "{}"
	}
	return text
}

// Removes every enum whose strings contain a slash, which the xAI grammar compiler rejects
func stripSlashEnum(v wire.Value) json.RawMessage {
	switch v.Kind() {
	case wire.KindArray:
		elems := v.Elems()
		out := make([]json.RawMessage, len(elems))
		for i, e := range elems {
			out[i] = stripSlashEnum(e)
		}
		return wire.Array(out...)
	case wire.KindObject:
		d := &wire.Dict{}
		for _, f := range v.Fields() {
			if f.Key == "enum" && f.Value.IsArray() && slices.ContainsFunc(f.Value.Elems(), func(e wire.Value) bool {
				return strings.Contains(e.Text(), "/")
			}) {
				continue
			}
			d.Set(f.Key, stripSlashEnum(f.Value))
		}
		return d.JSON()
	}
	return v.Raw()
}

// Validation pass over Responses input items with the sanitizer and replay policy of one request
type preflightPass struct {
	conv     *inputConv
	harmony  bool
	dropIDs  bool
	seen     map[string]bool
	sanitize func(string) string
}

// Normalized form of one input entry, nil for an encrypted one with nothing to forward
func (p *preflightPass) item(item wire.Value, idx int) (json.RawMessage, error) {
	if !item.IsObject() {
		return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] must be an object.", idx)}
	}
	switch item.Get("type").Text() {
	case "function_call":
		callID, name := item.Get("call_id"), item.Get("name")
		if !nonblank(callID) {
			return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] function_call is missing call_id.", idx)}
		}
		if !nonblank(name) {
			return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] function_call is missing name.", idx)}
		}
		return (&wire.Dict{}).SetString("type", "function_call").SetString("call_id", strip(callID.Text())).
			SetString("name", wire.CoerceToolName(name.Text(), "fn")).
			SetString("arguments", p.sanitize(coerceArguments(item.Get("arguments")))).JSON(), nil
	case "function_call_output":
		callID := item.Get("call_id")
		if !nonblank(callID) {
			return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] function_call_output is missing call_id.", idx)}
		}
		output := item.Get("output")
		value := wire.Quote(p.sanitize(displayOrEmpty(output)))
		if output.IsArray() {
			var cleaned []json.RawMessage
			for _, part := range output.Elems() {
				switch part.Get("type").Text() {
				case "input_text":
					if text := part.Get("text").Text(); text != "" {
						cleaned = append(cleaned, textPart("input_text", p.sanitize(text)))
					}
				case "input_image":
					if url := part.Get("image_url").Text(); url != "" {
						cleaned = append(cleaned, p.conv.imagePart(url, part.Get("detail").Text(), "user", false))
					}
				}
			}
			value = wire.Quote("")
			if len(cleaned) > 0 {
				value = wire.Array(cleaned...)
			}
		}
		return (&wire.Dict{}).SetString("type", "function_call_output").SetString("call_id", strip(callID.Text())).
			Set("output", value).JSON(), nil
	case "reasoning", "compaction":
		encrypted := item.Get("encrypted_content")
		if encrypted.Text() == "" {
			return nil, nil
		}
		if item.Get("type").Text() == "compaction" {
			return (&wire.Dict{}).SetString("type", "compaction").Set("encrypted_content", encrypted.Raw()).JSON(), nil
		}
		if id := item.Get("id").Text(); id != "" {
			if p.seen[id] {
				return nil, nil
			}
			p.seen[id] = true
		}
		summary := json.RawMessage("[]")
		if s := item.Get("summary"); s.IsArray() {
			summary = s.Raw()
			if p.harmony {
				neutral, err := neutralizeStructure(s)
				if err != nil {
					return nil, err
				}
				summary = neutral
			}
		}
		return (&wire.Dict{}).SetString("type", "reasoning").Set("encrypted_content", encrypted.Raw()).Set("summary", summary).JSON(), nil
	case "message":
		content := item.Get("content")
		if item.Get("role").Text() != "assistant" || !content.IsArray() || !(item.Get("id").Exists() || item.Get("status").Exists()) {
			return p.roleMessage(item, idx)
		}
		var normalized []json.RawMessage
		for n, part := range content.Elems() {
			if !part.IsObject() {
				return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] message content[%d] must be an object.", idx, n)}
			}
			if t := part.Get("type").Text(); t != "output_text" && t != "text" {
				return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] message content[%d] has unsupported type %q.", idx, n, displayOrEmpty(part.Get("type")))}
			}
			normalized = append(normalized, textPart("output_text", p.sanitize(displayOrEmpty(part.Get("text")))))
		}
		if len(normalized) == 0 {
			return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] message item must contain at least one text part.", idx)}
		}
		return assistantMessageItem(item, normalized, p.dropIDs, ""), nil
	}
	return p.roleMessage(item, idx)
}

// Normalized user or assistant message item with string content or Responses parts
func (p *preflightPass) roleMessage(item wire.Value, idx int) (json.RawMessage, error) {
	role := item.Get("role").Text()
	if role != "user" && role != "assistant" {
		return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d] has unsupported item shape (type=%q, role=%q).",
			idx, displayOrEmpty(item.Get("type")), displayOrEmpty(item.Get("role")))}
	}
	content := item.Get("content")
	if !content.IsArray() {
		return roleMessageItem(role, wire.Quote(p.sanitize(displayOrEmpty(content))), item.Get("phase")), nil
	}
	textType := textTypeFor(role)
	validated := []json.RawMessage{}
	for n, part := range content.Elems() {
		t := partType(displayOrEmpty(part.Get("type")))
		switch {
		case part.IsString():
			if part.Text() != "" {
				validated = append(validated, textPart(textType, p.sanitize(part.Text())))
			}
		case !part.IsObject():
			return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d].content[%d] must be an object or string.", idx, n)}
		case textPartType(t):
			validated = append(validated, textPart(textType, p.sanitize(displayOrEmpty(part.Get("text")))))
		case imagePartType(t):
			image, detail := part.Get("image_url"), part.Get("detail").Text()
			if image.IsObject() {
				if d := image.Get("detail"); d.Exists() {
					detail = d.Text()
				}
				image = image.Get("url")
			}
			validated = append(validated, p.conv.imagePart(image.Text(), detail, role, true))
		default:
			return nil, &RequestError{fmt.Sprintf("Codex Responses input[%d].content[%d] has unsupported type %q.", idx, n, displayOrEmpty(part.Get("type")))}
		}
	}
	return roleMessageItem(role, wire.Array(validated...), item.Get("phase")), nil
}

// Normalized tool declaration, a provider-executed built-in passing through as written
func preflightTool(tool wire.Value, idx int) (json.RawMessage, error) {
	if !tool.IsObject() {
		return nil, &RequestError{fmt.Sprintf("Codex Responses tools[%d] must be an object.", idx)}
	}
	switch t := tool.Get("type").Text(); t {
	case "web_search", "web_search_preview", "file_search", "code_interpreter", "image_generation", "computer_use_preview", "local_shell":
		return tool.Raw(), nil
	case "function":
	default:
		return nil, &RequestError{fmt.Sprintf("Codex Responses tools[%d] has unsupported type %q.", idx, displayOrEmpty(tool.Get("type")))}
	}
	if !nonblank(tool.Get("name")) {
		return nil, &RequestError{fmt.Sprintf("Codex Responses tools[%d] is missing a valid name.", idx)}
	}
	if !tool.Get("parameters").IsObject() {
		return nil, &RequestError{fmt.Sprintf("Codex Responses tools[%d] is missing valid parameters.", idx)}
	}
	return (&wire.Dict{}).SetString("type", "function").SetString("name", strip(tool.Get("name").Text())).
		SetString("description", displayOrEmpty(tool.Get("description"))).SetBool("strict", tool.Get("strict").Truthy()).
		Set("parameters", tool.Get("parameters").Raw()).JSON(), nil
}

// Validates a Responses body and returns it with only the members the contract allows, each normalized
func (p *ResponsesProtocol) preflight(body *wire.Dict, req *wire.Request) (*wire.Dict, error) {
	o := &req.Responses
	if !nonblank(body.Get("model")) {
		return nil, &RequestError{"Codex Responses request 'model' must be a non-empty string."}
	}
	sanitize := func(s string) string { return s }
	if o.SanitizeHarmony {
		sanitize = neutralizeHarmony
	}
	instructions := strip(displayOrEmpty(body.Get("instructions")))
	if instructions == "" {
		instructions = o.Identity
	}
	instructions = sanitize(instructions)
	input := body.Get("input")
	if !input.IsArray() {
		return nil, &RequestError{"Codex Responses input must be a list of input items."}
	}
	pass := &preflightPass{conv: &inputConv{p: p, o: o}, harmony: o.SanitizeHarmony, dropIDs: !o.ReplayItemIDs,
		seen: map[string]bool{}, sanitize: sanitize}
	items := []json.RawMessage{}
	for idx, raw := range input.Elems() {
		item, err := pass.item(raw, idx)
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, item)
		}
	}
	out := (&wire.Dict{}).SetString("model", strip(body.Get("model").Text())).SetString("instructions", instructions).
		Set("input", wire.Array(items...)).SetBool("store", false)
	if tools := body.Get("tools"); tools.Kind() > wire.KindNull {
		if !tools.IsArray() {
			return nil, &RequestError{"Codex Responses request 'tools' must be a list when provided."}
		}
		normalized := make([]json.RawMessage, 0, len(tools.Elems()))
		for idx, tool := range tools.Elems() {
			raw, err := preflightTool(tool, idx)
			if err != nil {
				return nil, err
			}
			normalized = append(normalized, raw)
		}
		list := wire.Array(normalized...)
		if o.SanitizeHarmony {
			v, _ := wire.ParseValue(list)
			neutral, err := neutralizeStructure(v)
			if err != nil {
				return nil, err
			}
			list = neutral
		}
		if lower := strings.ToLower(body.Get("model").Text()); len(normalized) > 0 &&
			(strings.HasPrefix(lower, "grok-") || strings.HasPrefix(lower, "x-ai/grok-")) {
			v, _ := wire.ParseValue(list)
			var stripped []json.RawMessage
			for _, tool := range v.Elems() {
				d := wire.DictOf(tool)
				if params := tool.Get("parameters"); params.IsObject() {
					d.Set("parameters", stripSlashEnum(params))
				}
				stripped = append(stripped, d.JSON())
			}
			list = wire.Array(stripped...)
		}
		out.Set("tools", list)
	}
	if store := body.Get("store"); store.Exists() && !store.False() {
		return nil, &RequestError{"Codex Responses contract requires 'store' to be false."}
	}
	for _, mb := range body.Members() {
		v := body.Get(mb.Key)
		number := v.Kind() == wire.KindNumber
		switch mb.Key {
		case "model", "instructions", "input", "tools", "store":
		case "reasoning":
			if v.IsObject() {
				out.Set(mb.Key, v.Raw())
			}
		case "include":
			if v.IsArray() {
				out.Set(mb.Key, v.Raw())
			}
		case "service_tier":
			if nonblank(v) {
				out.SetString(mb.Key, strip(v.Text()))
			}
		case "text":
			if v.IsObject() && len(v.Fields()) > 0 {
				out.Set(mb.Key, v.Raw())
			}
		case "max_output_tokens":
			if n, _ := v.Int(); number && n > 0 {
				out.SetInt(mb.Key, int(n))
			}
		case "temperature":
			if f, _ := v.Float(); number {
				out.SetFloat(mb.Key, f)
			}
		case "tool_choice", "parallel_tool_calls", "prompt_cache_key", "prompt_cache_retention":
			if v.Kind() > wire.KindNull {
				out.Set(mb.Key, v.Raw())
			}
		case "context_management":
			if v.IsArray() && len(v.Elems()) > 0 {
				out.Set(mb.Key, v.Raw())
			}
		default:
			var unexpected []string
			for _, other := range body.Members() {
				switch other.Key {
				case "model", "instructions", "input", "tools", "store", "reasoning", "include", "service_tier", "text",
					"max_output_tokens", "temperature", "tool_choice", "parallel_tool_calls", "prompt_cache_key",
					"prompt_cache_retention", "context_management":
				default:
					unexpected = append(unexpected, other.Key)
				}
			}
			slices.Sort(unexpected)
			return nil, &RequestError{"Codex Responses request has unsupported field(s): " + strings.Join(unexpected, ", ") + "."}
		}
	}
	return out, nil
}

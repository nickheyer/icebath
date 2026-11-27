package transport

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Model turn standing in for one that never came between a tool result and a user's text
	interruptedTurn = "[The previous response was interrupted before it completed.]"
	// Signature of a replayed tool call made by another provider, which Gemini then doesn't check
	skipSignature = "skip_thought_signature_validator"
	// Output cap of a request that asks none, and of one whose thinking needs room
	defaultMaxOutput = provider.GeminiMaxOutput
)

var toolChoiceModes = map[string]string{"auto": "AUTO", "required": "ANY", "none": "NONE"}

var finishReasons = map[string]string{"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter", "RECITATION": "content_filter",
	"OTHER": "stop", "CONTINUATION": "length"}

// Model id without the google/ or gemini/ an aggregator puts before it
func bareModel(model string) string {
	name := strings.TrimSpace(model)
	for _, prefix := range []string{"google/", "gemini/"} {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			if rest := strings.TrimSpace(name[len(prefix):]); rest != "" {
				return rest
			}
		}
	}
	return name
}

// Gemini 3 and later take ids on function calls and responses, which 2.x refuses
func takesIDs(model string) bool {
	major, _ := provider.GeminiVersion(bareModel(model))
	return major >= 3
}

// Text of OpenAI content: a string, or the text parts of a list joined by lines
func contentText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var texts []string
		for _, item := range c {
			switch v := item.(type) {
			case string:
				texts = append(texts, v)
			case map[string]any:
				if v["type"] == "text" {
					if s, ok := v["text"].(string); ok {
						texts = append(texts, s)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	raw, _ := json.Marshal(content)
	return string(raw)
}

// Gemini parts of OpenAI content: its text and its inline pictures
func parts(content any) []any {
	list, ok := content.([]any)
	if !ok {
		if text := contentText(content); text != "" {
			return []any{map[string]any{"text": text}}
		}
		return []any{}
	}
	out := []any{}
	for _, item := range list {
		switch v := item.(type) {
		case string:
			out = append(out, map[string]any{"text": v})
		case map[string]any:
			if v["type"] == "text" {
				if s, _ := v["text"].(string); s != "" {
					out = append(out, map[string]any{"text": s})
				}
				continue
			}
			if part := inline(v); part != nil {
				out = append(out, part)
			}
		}
	}
	return out
}

// inlineData part of an image or video part carrying a data URL, nil for any other
func inline(item map[string]any) map[string]any {
	if item["type"] != "image_url" && item["type"] != "video_url" {
		return nil
	}
	holder, _ := item[transport.Text(item["type"])].(map[string]any)
	address := transport.Text(holder["url"])
	header, encoded, ok := strings.Cut(address, ",")
	if !strings.HasPrefix(address, "data:") || !ok {
		return nil
	}
	clean := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' {
			return r
		}
		return -1
	}, encoded)
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil
	}
	mime, _, _ := strings.Cut(strings.TrimPrefix(header, "data:"), ";")
	return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(raw)}}
}

// Thought signature a replayed message or tool call carries in its extra_content, under google or at its top
func signature(holder map[string]any) string {
	extra, _ := holder["extra_content"].(map[string]any)
	sig := extra["google"]
	if sig == nil {
		sig = extra["thought_signature"]
	}
	if m, ok := sig.(map[string]any); ok {
		sig = m["thought_signature"]
		if sig == nil {
			sig = m["thoughtSignature"]
		}
	}
	return transport.Text(sig)
}

func callID(call map[string]any) string {
	for _, key := range []string{"id", "call_id"} {
		if v := call[key]; v != nil && v != "" {
			return strings.TrimSpace(fmt.Sprint(v))
		}
	}
	return ""
}

// functionCall part of a replayed tool call, with its signature or the one Gemini skips
func functionCall(call map[string]any, ids bool) map[string]any {
	function, _ := call["function"].(map[string]any)
	var args any = map[string]any{}
	if raw := transport.Text(function["arguments"]); raw != "" {
		if json.Unmarshal([]byte(raw), &args) != nil {
			args = map[string]any{"_raw": raw}
		}
	}
	if _, ok := args.(map[string]any); !ok {
		args = map[string]any{"_value": args}
	}
	fc := map[string]any{"name": transport.Text(function["name"]), "args": args}
	if id := callID(call); ids && id != "" {
		fc["id"] = id
	}
	sig := signature(call)
	sig = cmp.Or(sig, skipSignature)
	return map[string]any{"functionCall": fc, "thoughtSignature": sig}
}

// Whether a value holds a local $ref, which a schema has and a tool's result doesn't
func schemaLike(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		for k, value := range v {
			if s, ok := value.(string); k == "$ref" && ok && strings.HasPrefix(s, "#/") || schemaLike(value) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, schemaLike)
	}
	return false
}

// functionResponse part of a tool result, a result that isn't an object, or reads as a schema, sent as text; Gemini 3 gets its pictures
func functionResponse(message map[string]any, names map[string]string, ids, gemini3 bool) map[string]any {
	id := transport.Text(message["tool_call_id"])
	name := names[id]
	for _, fallback := range []string{transport.Text(message["name"]), id, "tool"} {
		name = cmp.Or(name, fallback)
	}
	content := contentText(message["content"])
	response := map[string]any{"name": name, "response": map[string]any{"output": content}}
	if trimmed := strings.TrimSpace(content); strings.HasPrefix(trimmed, "{") {
		var parsed map[string]any
		if json.Unmarshal([]byte(content), &parsed) == nil && !schemaLike(parsed) {
			response["response"] = parsed
		}
	}
	if ids && id != "" {
		response["id"] = id
	}
	if gemini3 {
		var images []any
		for _, p := range parts(message["content"]) {
			if part, _ := p.(map[string]any); part["inlineData"] != nil {
				images = append(images, p)
			}
		}
		if len(images) > 0 {
			response["parts"] = images
		}
	}
	return map[string]any{"functionResponse": response}
}

func answers(content map[string]any) bool {
	list, _ := content["parts"].([]any)
	return slices.ContainsFunc(list, func(p any) bool { part, _ := p.(map[string]any); return part["functionResponse"] != nil })
}

// Turns of one role in a row merged, a model turn put between a tool result and a user's text that follows it
func alternate(contents []map[string]any) []map[string]any {
	merged := []map[string]any{}
	for _, content := range contents {
		if n := len(merged); n > 0 && merged[n-1]["role"] == content["role"] {
			prev := merged[n-1]
			if content["role"] == "user" && answers(prev) != answers(content) {
				merged = append(merged, map[string]any{"role": "model", "parts": []any{map[string]any{"text": interruptedTurn}}})
			} else {
				prev["parts"] = append(prev["parts"].([]any), content["parts"].([]any)...)
				continue
			}
		}
		merged = append(merged, content)
	}
	return merged
}

// Contents and system instruction of OpenAI messages
func contents(messages []any, ids, gemini3 bool) ([]map[string]any, map[string]any) {
	var system []string
	var out []map[string]any
	names := map[string]string{}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := transport.Text(message["role"])
		switch role {
		case "system":
			if text := contentText(message["content"]); text != "" {
				system = append(system, text)
			}
			continue
		case "tool", "function":
			out = append(out, map[string]any{"role": "user", "parts": []any{functionResponse(message, names, ids, gemini3)}})
			continue
		}
		list := parts(message["content"])
		calls, _ := message["tool_calls"].([]any)
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			function, _ := call["function"].(map[string]any)
			if id, name := callID(call), transport.Text(function["name"]); id != "" && name != "" {
				names[id] = name
			}
			list = append(list, functionCall(call, ids))
		}
		if role == "assistant" && len(list) > 0 {
			if last, _ := list[len(list)-1].(map[string]any); last["functionCall"] == nil {
				if sig := signature(message); sig != "" {
					withSig := map[string]any{"thoughtSignature": sig}
					for k, v := range last {
						withSig[k] = v
					}
					list[len(list)-1] = withSig
				}
			}
		}
		if len(list) > 0 {
			kind := "user"
			if role == "assistant" {
				kind = "model"
			}
			out = append(out, map[string]any{"role": kind, "parts": list})
		}
	}
	var instruction map[string]any
	if joined := strings.TrimSpace(strings.Join(system, "\n")); joined != "" {
		instruction = map[string]any{"role": "system", "parts": []any{map[string]any{"text": joined}}}
	}
	return alternate(out), instruction
}

// Function declarations of OpenAI tools, nil when none names a function
func declarations(tools any, jsonSchema bool) []any {
	var list []any
	switch t := tools.(type) {
	case []any:
		list = t
	case []map[string]any:
		for _, m := range t {
			list = append(list, m)
		}
	}
	var decls []any
	for _, t := range list {
		tool, _ := t.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		name := transport.Text(function["name"])
		if name == "" {
			continue
		}
		decl := map[string]any{"name": name}
		if description := transport.Text(function["description"]); description != "" {
			decl["description"] = description
		}
		if params, ok := function["parameters"].(map[string]any); ok {
			if jsonSchema {
				decl["parametersJsonSchema"] = jsonSchemaOf(params)
			} else {
				decl["parameters"] = toolSchema(params)
			}
		}
		decls = append(decls, decl)
	}
	if len(decls) == 0 {
		return nil
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

// Tool config of an OpenAI tool choice, nil when it names nothing Gemini takes
func toolConfig(choice any) map[string]any {
	switch c := choice.(type) {
	case string:
		if mode, ok := toolChoiceModes[c]; ok {
			return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
		}
	case map[string]any:
		function, _ := c["function"].(map[string]any)
		if name := transport.Text(function["name"]); name != "" {
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []any{name}}}
		}
	}
	return nil
}

// Whole number of a JSON value, false for anything that isn't one
func whole(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return n, true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

// Thinking config in camel case from camel or snake keys, nil when nothing of it is valid
func thinkingConfig(raw any) map[string]any {
	config, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	pick := func(key, alias string) any {
		if v, ok := config[key]; ok {
			return v
		}
		return config[alias]
	}
	out := map[string]any{}
	if n, ok := whole(pick("thinkingBudget", "thinking_budget")); ok {
		if _, isText := pick("thinkingBudget", "thinking_budget").(string); !isText {
			out["thinkingBudget"] = n
		}
	}
	if b, ok := pick("includeThoughts", "include_thoughts").(bool); ok {
		out["includeThoughts"] = b
	}
	if s, ok := pick("thinkingLevel", "thinking_level").(string); ok && strings.TrimSpace(s) != "" {
		out["thinkingLevel"] = strings.ToLower(strings.TrimSpace(s))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Whether thinking spends output tokens, which a request's cap then makes room for
func roomForThinking(raw any) bool {
	config := thinkingConfig(raw)
	if include, ok := config["includeThoughts"].(bool); ok && !include {
		return false
	}
	budget, budgeted := config["thinkingBudget"].(int)
	_, level := config["thinkingLevel"]
	return len(config) > 0 && !(budgeted && budget <= 0 && !level)
}

// Output cap of the request: the published ceiling for none, or when thinking needs room above what it asks
func maxOutput(requested any, thinking any) int {
	n, _ := whole(requested)
	if n <= 0 || roomForThinking(thinking) {
		return max(n, defaultMaxOutput)
	}
	return n
}

// Generation fields of an OpenAI response format: JSON output, with its schema when it gives one
func jsonOutput(format any, jsonSchema bool) map[string]any {
	f, ok := format.(map[string]any)
	if !ok || f["type"] != "json_object" && f["type"] != "json_schema" {
		return map[string]any{}
	}
	var schema map[string]any
	if f["type"] == "json_schema" {
		spec, _ := f["json_schema"].(map[string]any)
		schema, _ = spec["schema"].(map[string]any)
	}
	switch {
	case schema == nil:
		return map[string]any{"responseMimeType": "application/json"}
	case jsonSchema:
		return map[string]any{"responseMimeType": "application/json", "responseJsonSchema": jsonSchemaOf(schema)}
	}
	return map[string]any{"responseMimeType": "application/json", "responseSchema": toolSchema(schema)}
}

// generateContent body of a chat completions body
func generateBody(body map[string]any, model string, jsonSchema bool) map[string]any {
	gemini3 := takesIDs(model)
	messages, _ := body["messages"].([]any)
	if typed, ok := body["messages"].([]map[string]any); ok {
		for _, m := range typed {
			messages = append(messages, m)
		}
	}
	turns, system := contents(messages, gemini3, gemini3)
	tools := declarations(body["tools"], jsonSchema)
	config := toolConfig(body["tool_choice"])
	out := map[string]any{"contents": turns}
	if system != nil {
		out["systemInstruction"] = system
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if config != nil {
		out["toolConfig"] = config
	}
	thinking := body["thinking_config"]
	if thinking == nil {
		thinking = body["thinkingConfig"]
	}
	requested := body["max_tokens"]
	if requested == nil {
		requested = body["max_completion_tokens"]
	}
	if budget, ok := whole(body["max_output_tokens"]); ok {
		if current, _ := whole(requested); budget > current {
			requested = budget
		}
	}
	generation := map[string]any{"maxOutputTokens": maxOutput(requested, thinking)}
	if t, ok := body["temperature"]; ok {
		generation["temperature"] = t
	}
	if p, ok := body["top_p"]; ok {
		generation["topP"] = p
	}
	switch stop := body["stop"].(type) {
	case []any:
		if len(stop) > 0 {
			generation["stopSequences"] = stop
		}
	case string:
		if stop != "" {
			generation["stopSequences"] = []any{stop}
		}
	}
	if config := thinkingConfig(thinking); config != nil {
		generation["thinkingConfig"] = config
	}
	output := jsonOutput(body["response_format"], jsonSchema)
	calling, _ := config["functionCallingConfig"].(map[string]any)
	forced := calling["mode"] == "ANY"
	if len(output) > 0 && (forced || len(tools) > 0 && !gemini3) {
		if forced {
			slog.Debug("Gemini: dropping JSON response_format — tool_choice forces function calling (mode ANY)")
		} else {
			slog.Debug("Gemini: dropping JSON response_format — pre-Gemini-3 model with tools")
		}
		output = map[string]any{}
	}
	for k, v := range output {
		generation[k] = v
	}
	out["generationConfig"] = generation
	return out
}

// Keys of the OpenAPI subset the parameters field takes
var schemaKeys = []string{"type", "format", "title", "description", "nullable", "enum", "maxItems", "minItems", "properties", "required",
	"minProperties", "maxProperties", "minLength", "maxLength", "pattern", "example", "anyOf", "propertyOrdering", "default", "items", "minimum",
	"maximum"}

// Keys that belong on a branch of a type union, by the branch's type
var structural = map[string][]string{"array": {"items", "minItems", "maxItems"}, "object": {"properties", "required", "minProperties",
	"maxProperties", "propertyOrdering"}}

// Enum entry as the text Gemini takes, false for one that is dropped
func enumText(item any) (string, bool) {
	switch v := item.(type) {
	case bool:
		return strconv.FormatBool(v), true
	case string:
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", false
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// Schema kept to the subset the parameters field takes, its type unions as anyOf branches with their own constraints, enums of numbers and
// booleans as text, required naming only properties it has
func toolSchema(schema any) map[string]any {
	m, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if cleaned := sanitize(m); len(cleaned) > 0 {
		return cleaned
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func sanitize(m map[string]any) map[string]any {
	cleaned := map[string]any{}
	for key, value := range m {
		if !slices.Contains(schemaKeys, key) {
			continue
		}
		switch key {
		case "properties":
			if props, ok := value.(map[string]any); ok {
				out := map[string]any{}
				for name, prop := range props {
					sub, _ := prop.(map[string]any)
					out[name] = sanitize(sub)
				}
				cleaned[key] = out
			}
		case "items":
			sub, _ := value.(map[string]any)
			cleaned[key] = sanitize(sub)
		case "anyOf":
			if list, ok := value.([]any); ok {
				var out []any
				for _, item := range list {
					if sub, ok := item.(map[string]any); ok {
						out = append(out, sanitize(sub))
					}
				}
				cleaned[key] = out
			}
		default:
			cleaned[key] = value
		}
	}
	types, union := cleaned["type"].([]any)
	if union {
		delete(cleaned, "type")
		unionTypes(types, cleaned)
	}
	if enum, ok := cleaned["enum"].([]any); ok {
		if kind, _ := cleaned["type"].(string); union || kind == "integer" || kind == "number" || kind == "boolean" {
			seen := map[string]bool{}
			texts := []any{}
			for _, v := range enum {
				if s, keep := enumText(v); keep && !seen[s] {
					seen[s] = true
					texts = append(texts, s)
				}
			}
			if len(texts) > 0 {
				cleaned["enum"] = texts
			} else {
				delete(cleaned, "enum")
			}
		}
	}
	if required, ok := cleaned["required"].([]any); ok {
		props, _ := cleaned["properties"].(map[string]any)
		valid := []any{}
		for _, name := range required {
			if s, ok := name.(string); ok && props[s] != nil {
				valid = append(valid, s)
			}
		}
		if len(valid) == 0 {
			delete(cleaned, "required")
		} else {
			cleaned["required"] = valid
		}
	}
	return cleaned
}

// A type union folded into one type or into anyOf branches, each with the constraints of its own type, nullable for a null member
func unionTypes(types []any, cleaned map[string]any) {
	null := false
	var kinds []string
	for _, t := range types {
		if s, ok := t.(string); ok {
			if s == "null" {
				null = true
			} else {
				kinds = append(kinds, s)
			}
		}
	}
	switch len(kinds) {
	case 0:
		cleaned["type"] = "object"
		if null {
			cleaned["type"] = "null"
		}
	case 1:
		cleaned["type"] = kinds[0]
	default:
		moved := map[string]any{}
		for _, kind := range []string{"array", "object"} {
			for _, key := range structural[kind] {
				if v, ok := cleaned[key]; ok {
					moved[key] = v
					delete(cleaned, key)
				}
			}
		}
		var branches []any
		for _, kind := range kinds {
			branch := map[string]any{"type": kind}
			if outer, ok := cleaned["anyOf"]; ok {
				branch["anyOf"] = outer
			}
			for _, key := range structural[kind] {
				if v, ok := moved[key]; ok {
					branch[key] = v
				}
			}
			branches = append(branches, sanitize(branch))
		}
		cleaned["anyOf"] = branches
	}
	if null {
		cleaned["nullable"] = true
	}
}

// Same document refs a schema may expand before inlining gives up
const refExpansions = 256

// Full JSON schema for parametersJsonSchema: its $schema dropped, its local refs inlined, an object at its root
func jsonSchemaOf(parameters map[string]any) map[string]any {
	if len(parameters) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	schema := copyOf(parameters).(map[string]any)
	delete(schema, "$schema")
	budget := refExpansions
	inlined, err := inlineRefs(schema, schema, &budget, nil)
	if err != nil {
		slog.Debug("Gemini tool schema kept as-is ($ref inlining skipped): " + err.Error())
		return schema
	}
	schema = inlined.(map[string]any)
	delete(schema, "$defs")
	delete(schema, "definitions")
	if len(schema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if schema["type"] == "object" && schema["properties"] == nil {
		schema["properties"] = map[string]any{}
	}
	return schema
}

func copyOf(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}

func resolve(root map[string]any, ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var node any = root
	for _, raw := range strings.Split(ref[2:], "/") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		next, ok := m[strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")]
		if !ok {
			return nil
		}
		node = next
	}
	out, _ := node.(map[string]any)
	return out
}

// Schema with its same document refs put in place, failing on one that doesn't resolve, one that refers to itself, or too many
func inlineRefs(node any, root map[string]any, budget *int, stack []string) (any, error) {
	switch t := node.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			x, err := inlineRefs(item, root, budget, stack)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case map[string]any:
		ref, isRef := t["$ref"].(string)
		if !isRef {
			out := map[string]any{}
			for k, v := range t {
				x, err := inlineRefs(v, root, budget, stack)
				if err != nil {
					return nil, err
				}
				out[k] = x
			}
			return out, nil
		}
		if slices.Contains(stack, ref) {
			return nil, fmt.Errorf("circular $ref %q", ref)
		}
		if *budget--; *budget < 0 {
			return nil, fmt.Errorf("$ref expansion budget exhausted")
		}
		target := resolve(root, ref)
		if target == nil {
			return nil, fmt.Errorf("unresolvable $ref %q", ref)
		}
		inlined, err := inlineRefs(target, root, budget, append(slices.Clone(stack), ref))
		if err != nil {
			return nil, err
		}
		siblings := map[string]any{}
		for k, v := range t {
			if k != "$ref" {
				siblings[k] = v
			}
		}
		if len(siblings) == 0 {
			return inlined, nil
		}
		resolved, err := inlineRefs(siblings, root, budget, stack)
		if err != nil {
			return nil, err
		}
		merged := copyOf(inlined).(map[string]any)
		for k, v := range resolved.(map[string]any) {
			merged[k] = v
		}
		return merged, nil
	}
	return node, nil
}

// extra_content carrying a part's thought signature, nil without one
func extraOf(part map[string]any) map[string]any {
	if sig := transport.Text(part["thoughtSignature"]); sig != "" {
		return map[string]any{"google": map[string]any{"thought_signature": sig}}
	}
	return nil
}

// Id of a function call, its own or a new one
func newCallID(fc map[string]any) string {
	if id := transport.Text(fc["id"]); id != "" {
		return id
	}
	return "call_" + strs.Random(6)
}

// Arguments of a function call as JSON text, keys sorted, an empty object for none
func callArguments(fc map[string]any) string {
	args := fc["args"]
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func count(meta map[string]any, key string) int {
	n, _ := whole(meta[key])
	return n
}

// OpenAI usage of usageMetadata, the thoughts counted as completion
func usage(meta map[string]any) map[string]any {
	reasoning := count(meta, "thoughtsTokenCount")
	out := map[string]any{"prompt_tokens": float64(count(meta, "promptTokenCount")),
		"completion_tokens": float64(count(meta, "candidatesTokenCount") + reasoning), "total_tokens": float64(count(meta, "totalTokenCount")),
		"prompt_tokens_details":     map[string]any{"cached_tokens": float64(count(meta, "cachedContentTokenCount"))},
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(reasoning)}}
	return out
}

func envelope(model, object string, choice map[string]any, usage map[string]any) map[string]any {
	out := map[string]any{"id": "chatcmpl-" + strs.Random(6), "object": object, "created": float64(time.Now().Unix()),
		"model": model, "choices": []any{choice}}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

func toolCall(name, arguments string, index int, id string, extra map[string]any) map[string]any {
	call := map[string]any{"id": id, "type": "function", "index": float64(index), "function": map[string]any{"name": name, "arguments": arguments}}
	if extra != nil {
		call["extra_content"] = extra
	}
	return call
}

func candidate(payload map[string]any) map[string]any {
	list, _ := payload["candidates"].([]any)
	if len(list) == 0 {
		return map[string]any{}
	}
	first, _ := list[0].(map[string]any)
	if first == nil {
		return map[string]any{}
	}
	return first
}

// Chat completion of a generateContent answer
func completion(answer map[string]any, model string) map[string]any {
	list, _ := answer["candidates"].([]any)
	cand := candidate(answer)
	content, _ := cand["content"].(map[string]any)
	partList, _ := content["parts"].([]any)
	var texts, thoughts []string
	var calls []any
	signed := ""
	for i, p := range partList {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			if thought, _ := part["thought"].(bool); thought {
				thoughts = append(thoughts, text)
			} else {
				texts = append(texts, text)
			}
			if sig := transport.Text(part["thoughtSignature"]); sig != "" && part["functionCall"] == nil {
				signed = sig
			}
		} else if fc, _ := part["functionCall"].(map[string]any); fc != nil && transport.Text(fc["name"]) != "" {
			calls = append(calls, toolCall(transport.Text(fc["name"]), callArguments(fc), i, newCallID(fc), extraOf(part)))
		}
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	} else if mapped, ok := finishReasons[strings.ToUpper(transport.Text(cand["finishReason"]))]; ok {
		finish = mapped
	}
	message := map[string]any{"role": "assistant"}
	if len(texts) > 0 {
		message["content"] = strings.Join(texts, "")
	} else if len(list) == 0 {
		message["content"] = ""
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	if reasoning := strings.Join(thoughts, ""); reasoning != "" {
		message["reasoning"], message["reasoning_content"] = reasoning, reasoning
	}
	if extra := extraOf(map[string]any{"thoughtSignature": signed}); extra != nil {
		message["extra_content"] = extra
	}
	meta, _ := answer["usageMetadata"].(map[string]any)
	return envelope(model, "chat.completion", map[string]any{"index": float64(0), "message": message, "finish_reason": finish}, usage(meta))
}

// One streamed tool call across the events and hops of a turn
type slot struct {
	index int
	id    string
	last  string
}

// Tool call slots of a turn in the order they opened, by the provider's call id or the call's place
type slots struct {
	keys  []string
	slots map[string]*slot
}

func newSlots() *slots { return &slots{slots: map[string]*slot{}} }

func (s *slots) set(key string, sl *slot) {
	if _, ok := s.slots[key]; !ok {
		s.keys = append(s.keys, key)
	}
	s.slots[key] = sl
}

// Slot of a streamed function call: the provider's id, else its place, name and signature, a resend extending its own slot
func (s *slots) of(fc, part map[string]any, at int, args string) (string, *slot) {
	if id := transport.Text(fc["id"]); id != "" {
		key := "id:" + id
		return key, s.slots[key]
	}
	key := fmt.Sprintf("part:%d:%s:%s", at, transport.Text(fc["name"]), transport.Text(part["thoughtSignature"]))
	existing := s.slots[key]
	if existing == nil || strings.HasPrefix(args, existing.last) {
		return key, existing
	}
	for _, other := range s.keys {
		if strings.HasPrefix(other, key+"#") && strings.HasPrefix(args, s.slots[other].last) {
			return other, s.slots[other]
		}
	}
	return fmt.Sprintf("%s#%d", key, len(s.keys)), nil
}

func chunk(model string, delta map[string]any, finish string) map[string]any {
	d := map[string]any{"role": "assistant"}
	for k, v := range delta {
		d[k] = v
	}
	choice := map[string]any{"index": float64(0), "delta": d}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return envelope(model, "chat.completion.chunk", choice, nil)
}

// Chat completion chunks of one Gemini stream event
func translate(event map[string]any, model string, s *slots) []map[string]any {
	if list, _ := event["candidates"].([]any); len(list) == 0 {
		return nil
	}
	cand := candidate(event)
	content, _ := cand["content"].(map[string]any)
	partList, _ := content["parts"].([]any)
	var out []map[string]any
	for at, p := range partList {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		text, isText := part["text"].(string)
		if thought, _ := part["thought"].(bool); isText && thought {
			out = append(out, chunk(model, map[string]any{"reasoning": text, "reasoning_content": text}, ""))
			continue
		}
		if text != "" {
			out = append(out, chunk(model, map[string]any{"content": text}, ""))
		}
		if sig := transport.Text(part["thoughtSignature"]); sig != "" && part["functionCall"] == nil {
			out = append(out, chunk(model, map[string]any{"extra_content": extraOf(part)}, ""))
		}
		fc, _ := part["functionCall"].(map[string]any)
		if fc == nil || transport.Text(fc["name"]) == "" {
			continue
		}
		args := callArguments(fc)
		key, sl := s.of(fc, part, at, args)
		if sl == nil {
			sl = &slot{index: len(s.keys), id: newCallID(fc)}
			s.set(key, sl)
		}
		piece := strings.TrimPrefix(args, sl.last)
		sl.last = args
		out = append(out, chunk(model, map[string]any{"tool_calls": []any{toolCall(transport.Text(fc["name"]), piece, sl.index, sl.id, extraOf(part))}}, ""))
	}
	if raw := transport.Text(cand["finishReason"]); raw != "" {
		finish := "stop"
		if len(s.keys) > 0 {
			finish = "tool_calls"
		} else if mapped, ok := finishReasons[strings.ToUpper(raw)]; ok {
			finish = mapped
		}
		last := chunk(model, nil, finish)
		if meta, ok := event["usageMetadata"].(map[string]any); ok && len(meta) > 0 {
			last["usage"] = usage(meta)
		}
		out = append(out, last)
	}
	return out
}

// Usage counts summed across the hops of a long decode
var hopCounts = []string{"promptTokenCount", "cachedContentTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "toolUsePromptTokenCount",
	"totalTokenCount"}

// Tokens one hop of a long decode may produce
const hopTokens = 32768

// Hops a request's output cap may take, one past them as the ceiling
func maxHops(request map[string]any) int {
	generation, _ := request["generationConfig"].(map[string]any)
	n, _ := whole(generation["maxOutputTokens"])
	return max(1, int(math.Ceil(float64(n)/hopTokens))) + 1
}

// Token to send again with when the hop ended on CONTINUATION, empty for any other end
func continuation(payload map[string]any) string {
	cand := candidate(payload)
	if token := transport.Text(cand["continuationToken"]); strings.EqualFold(transport.Text(cand["finishReason"]), "CONTINUATION") && token != "" {
		return token
	}
	return ""
}

func mergeUsage(hops []map[string]any) map[string]any {
	merged := map[string]any{}
	for _, key := range hopCounts {
		present, sum := false, 0
		for _, h := range hops {
			if _, ok := h[key]; ok {
				present = true
			}
			sum += count(h, key)
		}
		if present {
			merged[key] = float64(sum)
		}
	}
	return merged
}

// One answer whose model turn joins every hop's parts, ending as the last did
func mergeHops(hops []map[string]any) map[string]any {
	var joined []any
	var usages []map[string]any
	for _, hop := range hops {
		content, _ := candidate(hop)["content"].(map[string]any)
		list, _ := content["parts"].([]any)
		joined = append(joined, list...)
		meta, _ := hop["usageMetadata"].(map[string]any)
		usages = append(usages, meta)
	}
	last := hops[len(hops)-1]
	cand := map[string]any{}
	for k, v := range candidate(last) {
		cand[k] = v
	}
	content := map[string]any{"role": "model", "parts": joined}
	cand["content"] = content
	out := map[string]any{}
	for k, v := range last {
		out[k] = v
	}
	out["candidates"] = []any{cand}
	out["usageMetadata"] = mergeUsage(usages)
	return out
}

// Event to translate and the token to send again with when the stream ends there, the usage of finished hops kept
func split(event map[string]any, hops *[]map[string]any) (map[string]any, string) {
	cand := candidate(event)
	if token := continuation(event); token != "" {
		meta, _ := event["usageMetadata"].(map[string]any)
		*hops = append(*hops, meta)
		trimmed := map[string]any{}
		for k, v := range cand {
			if k != "finishReason" && k != "continuationToken" {
				trimmed[k] = v
			}
		}
		out := map[string]any{}
		for k, v := range event {
			if k != "usageMetadata" {
				out[k] = v
			}
		}
		out["candidates"] = []any{trimmed}
		return out, token
	}
	if transport.Text(cand["finishReason"]) != "" {
		if len(*hops) > 0 {
			meta, _ := event["usageMetadata"].(map[string]any)
			out := map[string]any{}
			for k, v := range event {
				out[k] = v
			}
			out["usageMetadata"] = mergeUsage(append(slices.Clone(*hops), meta))
			return out, ""
		}
		return event, ""
	}
	return event, transport.Text(cand["continuationToken"])
}

package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	vertexExpressBaseURL = "https://aiplatform.googleapis.com/v1beta1/publishers/google"
	// Model turn standing in for one that never arrived between a tool result and a user text turn
	geminiInterruptedTurnText = "[The previous response was interrupted before it completed.]"
	// Sentinel signature for replayed tool calls minted by another provider
	geminiSkipSignature = "skip_thought_signature_validator"
)

var geminiToolChoiceModes = map[string]string{"auto": "AUTO", "required": "ANY", "none": "NONE"}

var geminiFinishReasonMap = map[string]string{
	"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter", "RECITATION": "content_filter", "OTHER": "stop",
	"CONTINUATION": "length",
}

var geminiVersionRe = regexp.MustCompile(`^gemini-(\d+)(?:\.(\d+))?`)

// Model id without the google or gemini aggregator prefix
func bareGeminiModelID(model string) string {
	name := strings.TrimSpace(model)
	for _, prefix := range []string{"google/", "gemini/"} {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			if rest := strings.TrimSpace(name[len(prefix):]); rest != "" {
				return rest
			}
			return name
		}
	}
	return name
}

// Major and minor version of a Gemini model id, zero for non Gemini and unversioned ids
func geminiNativeVersion(model string) (int, int) {
	m := geminiVersionRe.FindStringSubmatch(strings.ToLower(bareGeminiModelID(model)))
	if m == nil {
		return 0, 0
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major, minor
}

// Gemini 3 and later need explicit functionCall and functionResponse ids, 2.x rejects them
func geminiRequiresToolCallIDs(model string) bool {
	major, _ := geminiNativeVersion(model)
	return major >= 3
}

// Text of an OpenAI content item given as a plain string or a text part
func geminiTextOf(item any) (string, bool) {
	if m, ok := item.(map[string]any); ok {
		if m["type"] == "text" {
			if s, ok := m["text"].(string); ok {
				return s, true
			}
		}
		return "", false
	}
	s, ok := item.(string)
	return s, ok
}

// OpenAI content flattened to text, list text parts joined by newlines
func coerceGeminiContentToText(content any) string {
	if list, ok := content.([]any); ok {
		var texts []string
		for _, item := range list {
			if t, ok := geminiTextOf(item); ok {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n")
	}
	if content == nil {
		return ""
	}
	return pyStr(content)
}

// inlineData part for an image_url item carrying a data URL, nil otherwise
func geminiInlineDataPart(item map[string]any) map[string]any {
	img, _ := item["image_url"].(map[string]any)
	url, _ := img["url"].(string)
	if item["type"] != "image_url" || !strings.HasPrefix(url, "data:") {
		return nil
	}
	header, encoded, ok := strings.Cut(url, ",")
	if !ok {
		return nil
	}
	raw, err := pyB64Decode(encoded)
	if err != nil {
		return nil
	}
	_, afterColon, ok := strings.Cut(header, ":")
	if !ok {
		return nil
	}
	mime, _, _ := strings.Cut(afterColon, ";")
	return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(raw)}}
}

// Base64 decode that discards non alphabet characters and requires correct padding
func pyB64Decode(s string) ([]byte, error) {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			b.WriteRune(r)
		}
	}
	return base64.StdEncoding.DecodeString(b.String())
}

// Gemini part for one OpenAI content item, nil when it carries neither text nor inline data
func geminiMultimodalPart(item any) map[string]any {
	text, isText := geminiTextOf(item)
	_, isStr := item.(string)
	if (isText && text != "") || isStr {
		return map[string]any{"text": text}
	}
	if m, ok := item.(map[string]any); ok {
		return geminiInlineDataPart(m)
	}
	return nil
}

// Gemini parts for OpenAI message content
func extractGeminiMultimodalParts(content any) []any {
	if list, ok := content.([]any); ok {
		parts := []any{}
		for _, item := range list {
			if p := geminiMultimodalPart(item); p != nil {
				parts = append(parts, p)
			}
		}
		return parts
	}
	if text := coerceGeminiContentToText(content); text != "" {
		return []any{map[string]any{"text": text}}
	}
	return []any{}
}

// Replayed thoughtSignature from extra_content in the google record or flat form
func toolCallExtraSignature(holder map[string]any) string {
	extra, ok := holder["extra_content"].(map[string]any)
	if !ok {
		return ""
	}
	var sig any = extra["google"]
	if !pyTruthy(sig) {
		sig = extra["thought_signature"]
	}
	if m, ok := sig.(map[string]any); ok {
		sig = m["thought_signature"]
		if !pyTruthy(sig) {
			sig = m["thoughtSignature"]
		}
	}
	s, _ := sig.(string)
	return s
}

// Call id of an OpenAI tool call, id before call_id
func geminiToolCallID(tc map[string]any) string {
	if v := tc["id"]; pyTruthy(v) {
		return pyStr(v)
	}
	if v := tc["call_id"]; pyTruthy(v) {
		return pyStr(v)
	}
	return ""
}

// Decodes JSON text with json.loads rules into maps and json.Number text, functionCall args kept in API key order
func geminiJSONLoads(text string) (any, error) {
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	return geminiPlainJSON(v, false), nil
}

// Plain maps and json.Number numbers for a decoded value, leaving args under functionCall as ordered Python values
func geminiPlainJSON(v any, underCall bool) any {
	switch t := v.(type) {
	case *pyjson.Object:
		m := make(map[string]any, len(t.Keys))
		for _, k := range t.Keys {
			if underCall && k == "args" {
				m[k] = t.Vals[k]
				continue
			}
			m[k] = geminiPlainJSON(t.Vals[k], k == "functionCall")
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = geminiPlainJSON(e, false)
		}
		return t
	}
	return geminiNumber(v)
}

// json.Number in Python spelling for a decoded int or finite float, any other value unchanged
func geminiNumber(v any) any {
	switch t := v.(type) {
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case *big.Int:
		return json.Number(t.String())
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return t
		}
		return json.Number(pyrepr.Float(t))
	}
	return v
}

// Request side decode keeping every object in key order with numbers spelled the way json.dumps writes them
func geminiRequestJSONLoads(text string) (any, error) {
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case *pyjson.Object:
			out := pyjson.NewObject()
			for _, k := range t.Keys {
				out.Set(k, walk(t.Vals[k]))
			}
			return out
		case []any:
			for i, e := range t {
				t[i] = walk(e)
			}
			return t
		}
		return geminiNumber(v)
	}
	return walk(v), nil
}

// Single key ordered object as Python builds the wrapper dicts around non object arguments
func geminiWrap(key string, v any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set(key, v)
	return o
}

// functionCall part with its replayed signature or the skip sentinel
func translateToolCallToGemini(tc map[string]any, includeIDs bool) map[string]any {
	fn, _ := pyjson.Dict(tc["function"])
	var args any = pyjson.NewObject()
	if raw, ok := fn["arguments"].(string); ok && raw != "" {
		if parsed, err := geminiRequestJSONLoads(raw); err == nil {
			args = parsed
		} else {
			args = geminiWrap("_raw", raw)
		}
	}
	if _, ok := args.(*pyjson.Object); !ok {
		args = geminiWrap("_value", args)
	}
	call := map[string]any{"name": pyOrStr(fn["name"]), "args": args}
	if includeIDs {
		if id := geminiToolCallID(tc); id != "" {
			call["id"] = id
		}
	}
	sig := toolCallExtraSignature(tc)
	if sig == "" {
		sig = geminiSkipSignature
	}
	return map[string]any{"functionCall": call, "thoughtSignature": sig}
}

// Reports whether a parsed value holds a local JSON schema ref pointer
func looksLikeJSONSchema(node any) bool {
	switch v := node.(type) {
	case *pyjson.Object:
		for _, k := range v.Keys {
			val := v.Vals[k]
			if s, ok := val.(string); k == "$ref" && ok && strings.HasPrefix(s, "#/") {
				return true
			}
			if looksLikeJSONSchema(val) {
				return true
			}
		}
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); k == "$ref" && ok && strings.HasPrefix(s, "#/") {
				return true
			}
			if looksLikeJSONSchema(val) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if looksLikeJSONSchema(item) {
				return true
			}
		}
	}
	return false
}

// functionResponse part for a tool result, schema bearing or non object results sent as opaque text
func translateToolResultToGemini(msg map[string]any, nameByCallID map[string]string, includeIDs, isGemini3 bool) map[string]any {
	callID := pyOrStr(msg["tool_call_id"])
	name := nameByCallID[callID]
	if name == "" {
		name = pyOrStr(msg["name"])
	}
	if name == "" {
		name = callID
	}
	if name == "" {
		name = "tool"
	}
	raw := msg["content"]
	content := coerceGeminiContentToText(raw)
	var parsed any
	if t := strings.TrimSpace(content); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		parsed, _ = geminiRequestJSONLoads(content)
	}
	pm, isObj := parsed.(*pyjson.Object)
	fr := map[string]any{"name": name}
	if isObj && !looksLikeJSONSchema(pm) {
		fr["response"] = pm
	} else {
		fr["response"] = map[string]any{"output": content}
	}
	if includeIDs && callID != "" {
		fr["id"] = callID
	}
	if isGemini3 {
		var images []any
		for _, p := range extractGeminiMultimodalParts(raw) {
			if _, ok := p.(map[string]any)["inlineData"]; ok {
				images = append(images, p)
			}
		}
		if len(images) > 0 {
			fr["parts"] = images
		}
	}
	return map[string]any{"functionResponse": fr}
}

// Reports whether a content carries a functionResponse part
func hasFunctionResponse(content map[string]any) bool {
	parts, _ := content["parts"].([]any)
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if _, ok := m["functionResponse"]; ok {
				return true
			}
		}
	}
	return false
}

// Merges same role neighbours, splitting a tool result turn from user text with an interrupted model turn
func mergeAlternating(contents []map[string]any) []map[string]any {
	var merged []map[string]any
	for _, content := range contents {
		var prev map[string]any
		if len(merged) > 0 {
			prev = merged[len(merged)-1]
		}
		sameRole := prev != nil && prev["role"] == content["role"]
		if sameRole && content["role"] == "user" && hasFunctionResponse(prev) != hasFunctionResponse(content) {
			sameRole = false
			merged = append(merged, map[string]any{"role": "model", "parts": []any{map[string]any{"text": geminiInterruptedTurnText}}})
		}
		if sameRole {
			last := merged[len(merged)-1]
			lp, _ := last["parts"].([]any)
			cp, _ := content["parts"].([]any)
			last["parts"] = append(lp, cp...)
		} else {
			merged = append(merged, content)
		}
	}
	if merged == nil {
		merged = []map[string]any{}
	}
	return merged
}

// Gemini contents and system instruction for OpenAI messages
func buildGeminiContents(messages []any, includeToolCallIDs, isGemini3 bool) ([]map[string]any, map[string]any) {
	var systemParts []string
	var contents []map[string]any
	nameByCallID := map[string]string{}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := pyOrStr(msg["role"])
		if role == "" {
			role = "user"
		}
		if role == "system" {
			systemParts = append(systemParts, coerceGeminiContentToText(msg["content"]))
			continue
		}
		if role == "tool" || role == "function" {
			part := translateToolResultToGemini(msg, nameByCallID, includeToolCallIDs, isGemini3)
			contents = append(contents, map[string]any{"role": "user", "parts": []any{part}})
			continue
		}
		parts := extractGeminiMultimodalParts(msg["content"])
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, rc := range calls {
				tc, ok := pyjson.Dict(rc)
				if !ok {
					continue
				}
				fn, _ := pyjson.Dict(tc["function"])
				toolName := pyOrStr(fn["name"])
				if id := geminiToolCallID(tc); id != "" && toolName != "" {
					nameByCallID[id] = toolName
				}
				parts = append(parts, translateToolCallToGemini(tc, includeToolCallIDs))
			}
		}
		if role == "assistant" && len(parts) > 0 {
			last := parts[len(parts)-1].(map[string]any)
			if _, isCall := last["functionCall"]; !isCall {
				if sig := toolCallExtraSignature(msg); sig != "" {
					withSig := copyAnyMap(last)
					withSig["thoughtSignature"] = sig
					parts[len(parts)-1] = withSig
				}
			}
		}
		if len(parts) > 0 {
			r := "user"
			if role == "assistant" {
				r = "model"
			}
			contents = append(contents, map[string]any{"role": r, "parts": parts})
		}
	}
	var kept []string
	for _, p := range systemParts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	joined := strings.TrimSpace(strings.Join(kept, "\n"))
	var system map[string]any
	if joined != "" {
		system = map[string]any{"role": "system", "parts": []any{map[string]any{"text": joined}}}
	}
	return mergeAlternating(contents), system
}

// functionDeclaration for one OpenAI tool, nil when it has no name
func geminiFunctionDeclaration(tool any, jsonSchema bool) map[string]any {
	t, ok := tool.(map[string]any)
	if !ok {
		return nil
	}
	fn, ok := pyjson.Dict(t["function"])
	if !ok {
		return nil
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return nil
	}
	decl := map[string]any{"name": name}
	if d, ok := fn["description"].(string); ok && d != "" {
		decl["description"] = d
	}
	if params, ok := fn["parameters"].(map[string]any); ok {
		if jsonSchema {
			decl["parametersJsonSchema"] = prepareGeminiToolParameters(params)
		} else {
			decl["parameters"] = sanitizeGeminiToolParameters(params)
		}
	}
	return decl
}

// Gemini tools block for OpenAI tools, empty when none declare a function
func translateToolsToGemini(tools any, jsonSchema bool) []any {
	list, _ := tools.([]any)
	if maps, ok := tools.([]map[string]any); ok {
		for _, m := range maps {
			list = append(list, m)
		}
	}
	var decls []any
	for _, t := range list {
		if d := geminiFunctionDeclaration(t, jsonSchema); d != nil {
			decls = append(decls, d)
		}
	}
	if len(decls) == 0 {
		return nil
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

// Gemini toolConfig for an OpenAI tool_choice, nil when it names nothing usable
func translateToolChoiceToGemini(toolChoice any) map[string]any {
	if s, ok := toolChoice.(string); ok {
		if mode, ok := geminiToolChoiceModes[s]; ok {
			return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
		}
		return nil
	}
	tc, ok := toolChoice.(map[string]any)
	if !ok {
		return nil
	}
	fn, _ := pyjson.Dict(tc["function"])
	if name, ok := fn["name"].(string); ok && name != "" {
		return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY", "allowedFunctionNames": []any{name}}}
	}
	return nil
}

// Python int of an int or float JSON value, false for other types
func geminiPyNumber(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		if f, err := n.Float64(); err == nil {
			return int(f), true
		}
	}
	return 0, false
}

// thinkingConfig in camelCase from camel or snake keys, nil when nothing valid remains
func normalizeGeminiThinkingConfig(config any) map[string]any {
	cfg, ok := config.(map[string]any)
	if !ok {
		return nil
	}
	pick := func(key, alias string) any {
		if v, ok := cfg[key]; ok {
			return v
		}
		return cfg[alias]
	}
	out := map[string]any{}
	if n, ok := geminiPyNumber(pick("thinkingBudget", "thinking_budget")); ok {
		out["thinkingBudget"] = n
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

// Reports whether thinking spends output tokens and so needs maxOutputTokens headroom
func thinkingRequestsOutputHeadroom(thinkingConfig any) bool {
	normalized := normalizeGeminiThinkingConfig(thinkingConfig)
	if inc, ok := normalized["includeThoughts"].(bool); ok && !inc {
		return false
	}
	budget, isInt := normalized["thinkingBudget"].(int)
	_, hasLevel := normalized["thinkingLevel"]
	return len(normalized) > 0 && !(isInt && budget <= 0 && !hasLevel)
}

// Native maxOutputTokens, the published ceiling for an unset cap or when thinking needs headroom
func effectiveGeminiMaxOutputTokens(maxTokens any, thinkingConfig any) int {
	requested := 0
	switch v := maxTokens.(type) {
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			requested = n
		}
	default:
		requested, _ = geminiPyNumber(v)
	}
	if requested <= 0 || thinkingRequestsOutputHeadroom(thinkingConfig) {
		return max(requested, geminiDefaultMaxOutputTokens)
	}
	return requested
}

// generationConfig JSON output keys for an OpenAI response_format
func translateGeminiResponseFormat(responseFormat any, jsonSchema bool) map[string]any {
	rf, ok := responseFormat.(map[string]any)
	if !ok || (rf["type"] != "json_object" && rf["type"] != "json_schema") {
		return map[string]any{}
	}
	var schema map[string]any
	if rf["type"] == "json_schema" {
		if spec, ok := rf["json_schema"].(map[string]any); ok {
			schema, _ = spec["schema"].(map[string]any)
		}
	}
	if schema == nil {
		return map[string]any{"responseMimeType": "application/json"}
	}
	if jsonSchema {
		return map[string]any{"responseMimeType": "application/json", "responseJsonSchema": prepareGeminiToolParameters(schema)}
	}
	return map[string]any{"responseMimeType": "application/json", "responseSchema": sanitizeGeminiToolParameters(schema)}
}

// OpenAI shaped request arguments a generateContent body is built from
type geminiRequestInput struct {
	Messages          []any
	Tools             any
	ToolChoice        any
	Temperature       any
	MaxTokens         any
	TopP              any
	Stop              any
	ThinkingConfig    any
	ResponseFormat    any
	Model             string
	ToolsAsJSONSchema bool
}

// generateContent body for OpenAI shaped request arguments
func buildGeminiRequest(in geminiRequestInput) map[string]any {
	isGemini3 := geminiRequiresToolCallIDs(in.Model)
	contents, system := buildGeminiContents(in.Messages, isGemini3, isGemini3)
	tools := translateToolsToGemini(in.Tools, in.ToolsAsJSONSchema)
	toolConfig := translateToolChoiceToGemini(in.ToolChoice)
	request := map[string]any{"contents": contents}
	if system != nil {
		request["systemInstruction"] = system
	}
	if len(tools) > 0 {
		request["tools"] = tools
	}
	if toolConfig != nil {
		request["toolConfig"] = toolConfig
	}
	gen := map[string]any{}
	if in.Temperature != nil {
		gen["temperature"] = in.Temperature
	}
	gen["maxOutputTokens"] = effectiveGeminiMaxOutputTokens(in.MaxTokens, in.ThinkingConfig)
	if in.TopP != nil {
		gen["topP"] = in.TopP
	}
	if pyTruthy(in.Stop) {
		switch s := in.Stop.(type) {
		case []any:
			gen["stopSequences"] = s
		case []string:
			list := make([]any, 0, len(s))
			for _, x := range s {
				list = append(list, x)
			}
			gen["stopSequences"] = list
		default:
			gen["stopSequences"] = []any{pyStr(s)}
		}
	}
	if tc := normalizeGeminiThinkingConfig(in.ThinkingConfig); tc != nil {
		gen["thinkingConfig"] = tc
	}
	jsonOutput := translateGeminiResponseFormat(in.ResponseFormat, in.ToolsAsJSONSchema)
	fcc, _ := toolConfig["functionCallingConfig"].(map[string]any)
	forcedCall := fcc["mode"] == "ANY"
	if len(jsonOutput) > 0 && (forcedCall || (len(tools) > 0 && !isGemini3)) {
		if forcedCall {
			catalogLog.Debug("Gemini: dropping JSON response_format — tool_choice forces function calling (mode ANY)")
		} else {
			catalogLog.Debug("Gemini: dropping JSON response_format — pre-Gemini-3 model with tools")
		}
		jsonOutput = map[string]any{}
	}
	for k, v := range jsonOutput {
		gen[k] = v
	}
	request["generationConfig"] = gen
	return request
}

// Wire key order of a generateContent body and its generationConfig
var (
	geminiRequestKeyOrder    = []string{"contents", "systemInstruction", "tools", "toolConfig", "generationConfig", "continuationToken"}
	geminiGenerationKeyOrder = []string{"temperature", "maxOutputTokens", "topP", "stopSequences", "thinkingConfig", "responseMimeType", "responseJsonSchema", "responseSchema"}
)

// JSON body with the request and generationConfig keys in wire order
func geminiRequestJSON(request map[string]any) ([]byte, error) {
	top := newOmap()
	for _, k := range geminiRequestKeyOrder {
		v, ok := request[k]
		if !ok {
			continue
		}
		if gen, isGen := v.(map[string]any); isGen && k == "generationConfig" {
			g := newOmap()
			for _, gk := range geminiGenerationKeyOrder {
				if gv, ok := gen[gk]; ok {
					g.Set(gk, gv)
				}
			}
			for gk, gv := range gen {
				if !g.Has(gk) {
					g.Set(gk, gv)
				}
			}
			top.Set(k, g)
			continue
		}
		top.Set(k, v)
	}
	for k, v := range request {
		if !top.Has(k) {
			top.Set(k, v)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(top); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ---- Gemini to OpenAI response translation ----

// extra_content record carrying a thought signature, nil without one
func toolCallExtraFromPart(part map[string]any) map[string]any {
	if sig, ok := part["thoughtSignature"].(string); ok && sig != "" {
		return map[string]any{"google": map[string]any{"thought_signature": sig}}
	}
	return nil
}

// Provider issued functionCall id, empty when absent
func geminiProviderCallID(fc map[string]any) string {
	id, _ := fc["id"].(string)
	return id
}

// Provider call id when present, else a minted OpenAI style id
func geminiNewCallID(fc map[string]any) string {
	if id := geminiProviderCallID(fc); id != "" {
		return id
	}
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// Function call args as Python json text, an empty object when absent
func geminiDumpCallArgs(fc map[string]any) string { return geminiDumpCallArgsWith(fc, false) }

// Function call args as Python json text with keys sorted when asked, an empty object when absent
func geminiDumpCallArgsWith(fc map[string]any, sortKeys bool) string {
	args := fc["args"]
	if !pyTruthy(args) {
		args = map[string]any{}
	}
	out, err := pyjson.Dumps(args, pyjson.EnsureASCII(false), pyjson.SortKeys(sortKeys))
	if err != nil {
		return "{}"
	}
	return out
}

// OpenAI usage for a Gemini usageMetadata, thoughts folded into completion tokens
func geminiUsageFromMetadata(meta map[string]any) map[string]any {
	reasoning := geminiCount(meta, "thoughtsTokenCount")
	usage := map[string]any{
		"prompt_tokens":             geminiCount(meta, "promptTokenCount"),
		"completion_tokens":         geminiCount(meta, "candidatesTokenCount") + reasoning,
		"total_tokens":              geminiCount(meta, "totalTokenCount"),
		"prompt_tokens_details":     map[string]any{"cached_tokens": geminiCount(meta, "cachedContentTokenCount")},
		"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning},
	}
	if _, ok := meta["initialPromptTokenCount"]; ok {
		usage["initial_prompt_tokens"] = geminiCount(meta, "initialPromptTokenCount")
		usage["initial_cached_tokens"] = geminiCount(meta, "initialCachedContentTokenCount")
	}
	return usage
}

// OpenAI chat.completion or chunk envelope around one choice
func geminiEnvelope(model, object string, choice map[string]any, usage map[string]any) map[string]any {
	env := map[string]any{"id": "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12], "object": object,
		"created": time.Now().Unix(), "model": model, "choices": []any{choice}}
	if usage != nil {
		env["usage"] = usage
	} else {
		env["usage"] = nil
	}
	return env
}

// OpenAI shaped tool call with extra_content attached only when present
func geminiToolCallMap(name, arguments string, index int, callID string, extra map[string]any) map[string]any {
	tc := map[string]any{"id": callID, "type": "function", "index": index, "function": map[string]any{"name": name, "arguments": arguments}}
	if extra != nil {
		tc["extra_content"] = extra
	}
	return tc
}

// Text of a candidate part and whether it is a thought
func geminiPartText(part map[string]any) (string, bool, bool) {
	text, ok := part["text"].(string)
	if !ok {
		return "", false, false
	}
	thought, _ := part["thought"].(bool)
	return text, thought, true
}

// thoughtSignature on a part that is not a functionCall
func geminiTextPartSignature(part map[string]any) string {
	sig, _ := part["thoughtSignature"].(string)
	if _, isCall := part["functionCall"]; isCall {
		return ""
	}
	return sig
}

// functionCall of a part when it names a function
func geminiPartFunctionCall(part map[string]any) map[string]any {
	fc, ok := part["functionCall"].(map[string]any)
	if !ok || !pyTruthy(fc["name"]) {
		return nil
	}
	return fc
}

// OpenAI chat.completion for a generateContent response
func translateGeminiResponse(resp map[string]any, model string) map[string]any {
	candidates, _ := resp["candidates"].([]any)
	var cand map[string]any
	var parts []any
	if len(candidates) > 0 {
		cand, _ = candidates[0].(map[string]any)
		if cand == nil {
			cand = map[string]any{}
		}
		if co, ok := cand["content"].(map[string]any); ok {
			parts, _ = co["parts"].([]any)
		}
	}
	var texts, thoughts []string
	var toolCalls []any
	textSignature := ""
	for index, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text, isThought, ok := geminiPartText(part); ok {
			if isThought {
				thoughts = append(thoughts, text)
			} else {
				texts = append(texts, text)
			}
			if sig := geminiTextPartSignature(part); sig != "" {
				textSignature = sig
			}
		} else if fc := geminiPartFunctionCall(part); fc != nil {
			toolCalls = append(toolCalls, geminiToolCallMap(pyStr(fc["name"]), geminiDumpCallArgs(fc), index, geminiNewCallID(fc), toolCallExtraFromPart(part)))
		}
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	} else if mapped, ok := geminiFinishReasonMap[strings.ToUpper(pyOrStr(cand["finishReason"]))]; ok {
		finish = mapped
	}
	meta := map[string]any{}
	if cand != nil {
		if um, ok := resp["usageMetadata"].(map[string]any); ok {
			meta = um
		}
	}
	message := map[string]any{"role": "assistant", "reasoning_details": nil}
	switch {
	case len(texts) > 0:
		message["content"] = strings.Join(texts, "")
	case cand == nil:
		message["content"] = ""
	default:
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	} else {
		message["tool_calls"] = nil
	}
	if reasoning := strings.Join(thoughts, ""); reasoning != "" {
		message["reasoning"], message["reasoning_content"] = reasoning, reasoning
	} else {
		message["reasoning"], message["reasoning_content"] = nil, nil
	}
	if extra := toolCallExtraFromPart(map[string]any{"thoughtSignature": textSignature}); extra != nil {
		message["extra_content"] = extra
	} else {
		message["extra_content"] = nil
	}
	return geminiEnvelope(model, "chat.completion", map[string]any{"index": 0, "message": message, "finish_reason": finish}, geminiUsageFromMetadata(meta))
}

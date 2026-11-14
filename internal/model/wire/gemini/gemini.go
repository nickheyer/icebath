// Gemini generateContent wire protocol
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Text of the model turn that stands in when a user text turn directly follows a tool result turn
const interruptedPlaceholder = "[The previous response was interrupted before it completed.]"

// Signature sent for a replayed function call that never had one, so thinking models accept foreign history
const skipSignature = "skip_thought_signature_validator"

// Guidance appended to errors an API key of the wrong tier, type or surface causes
const (
	freeTierGuidance = "\n\nYour Google API key is on the free tier (a few hundred requests/day for Gemini Flash models). " +
		"icebath typically makes 3-10 API calls per user turn, so the free tier is exhausted in a handful of " +
		"messages and cannot sustain an agent session. Enable billing on your Google Cloud project and " +
		"regenerate the key in a billing-enabled project: https://aistudio.google.com/apikey"
	standardKeyGuidance = "\n\nGoogle Gemini rejected this API key's type — you do NOT need OAuth. Google began rejecting legacy " +
		"'Standard' Google Cloud keys for the Gemini API on June 19, 2026, and all Standard keys stop working in " +
		"September 2026. Open https://aistudio.google.com/api-keys, check the key's type and status, and create a " +
		"replacement Gemini API key (or, as a temporary bridge, restrict the Standard key to " +
		"generativelanguage.googleapis.com). Then update GEMINI_API_KEY / GOOGLE_API_KEY in ~/.icebath/.env, " +
		"delete any leftover Windows/system copy of those variables, and restart. A stale shell key can hide " +
		"the .env value. Details: https://ai.google.dev/gemini-api/docs/api-key"
	expressKeyOnStudioGuidance = "\n\nGoogle issues 'AQ.' keys for both Google AI Studio and Vertex AI express mode, and each surface " +
		"only accepts its own keys: a Vertex express key always gets 403 PERMISSION_DENIED on " +
		"generativelanguage.googleapis.com. If this key came from Google Cloud / Vertex (express mode), point " +
		"the provider at the express surface, e.g. set base_url / GEMINI_BASE_URL to " +
		"https://aiplatform.googleapis.com/v1beta1."
	studioKeyOnExpressGuidance = "\n\nIf this 'AQ.' key came from Google AI Studio (https://aistudio.google.com/apikey), it only works " +
		"on the default generativelanguage.googleapis.com host — remove the explicit aiplatform base_url so " +
		"the default surface is used."
)

// Native Gemini protocol, Version naming the product release in the client headers
type Protocol struct {
	Version string
}

var _ wire.Protocol = Protocol{}

// Puts a Gemini API key on a request without ever showing it when formatted
type keyAuth struct {
	key string
}

// Authorizer that sends an API key in the header the native API reads
func APIKey(key string) wire.Authorizer { return keyAuth{key} }

func (a keyAuth) Authorize(_ context.Context, r *http.Request, _ []byte) error {
	r.Header.Set("x-goog-api-key", a.key)
	return nil
}

func (keyAuth) String() string { return "gemini api key" }

func (keyAuth) GoString() string { return "gemini api key" }

// Client identity the native API reads for partner attribution
func (p Protocol) client() string { return "icebath/" + p.Version }

// Text of message content, the text items of a list joined by newlines
func contentText(m *wire.Message) string {
	if m.Parts == nil {
		return m.Text
	}
	var items []string
	for i := range m.Parts {
		if text, ok := partText(&m.Parts[i]); ok {
			items = append(items, text)
		}
	}
	return strings.Join(items, "\n")
}

// Text of a content item that is a bare string or a text part
func partText(p *wire.Part) (string, bool) {
	if len(p.Native) > 0 || p.Kind != wire.PartText || (p.TypeName != "" && p.TypeName != "text") {
		return "", false
	}
	return p.Text, true
}

// Inline image part for an image part carrying a data URL, nil for any other part
func inlineData(p *wire.Part) json.RawMessage {
	if len(p.Native) > 0 || p.TypeName != "image_url" || !strings.HasPrefix(p.URL, "data:") {
		return nil
	}
	header, encoded, found := strings.Cut(p.URL, ",")
	if !found {
		return nil
	}
	clean := strings.Map(func(r rune) rune {
		if r == '+' || r == '/' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return -1
	}, encoded)
	pads, tail := strings.Count(encoded, "="), len(clean)%4
	data, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil || (tail == 2 && pads < 2) || (tail == 3 && pads < 1) {
		return nil
	}
	_, mime, hasMime := strings.Cut(header, ":")
	if !hasMime {
		return nil
	}
	mime, _, _ = strings.Cut(mime, ";")
	inner := (&wire.Dict{}).SetString("mimeType", mime).SetString("data", base64.StdEncoding.EncodeToString(data))
	return (&wire.Dict{}).Set("inlineData", inner.JSON()).JSON()
}

// Native parts for message content, text and inline images in order
func multimodalParts(m *wire.Message) []json.RawMessage {
	if m.Parts == nil {
		if m.Text == "" {
			return nil
		}
		return []json.RawMessage{(&wire.Dict{}).SetString("text", m.Text).JSON()}
	}
	var parts []json.RawMessage
	for i := range m.Parts {
		p := &m.Parts[i]
		if text, ok := partText(p); ok && (text != "" || p.TypeName == "") {
			parts = append(parts, (&wire.Dict{}).SetString("text", text).JSON())
		} else if data := inlineData(p); data != nil {
			parts = append(parts, data)
		}
	}
	return parts
}

// Thought signature a replayed tool call carries in its extra content, empty when it has none
func callSignature(extra json.RawMessage) string {
	v, err := wire.ParseValue(extra)
	if err != nil || !v.IsObject() {
		return ""
	}
	sig := v.Get("google")
	if !sig.Truthy() {
		sig = v.Get("thought_signature")
	}
	if sig.IsObject() {
		inner := sig.Get("thought_signature")
		if !inner.Truthy() {
			inner = sig.Get("thoughtSignature")
		}
		sig = inner
	}
	return sig.Text()
}

// functionCall part for a replayed tool call with its signature or the skip sentinel
func functionCallPart(tc *wire.ToolCall, withIDs bool) json.RawMessage {
	args := json.RawMessage("{}")
	if tc.Arguments != "" {
		if v, err := wire.ParseValue([]byte(tc.Arguments)); err != nil {
			args = (&wire.Dict{}).SetString("_raw", tc.Arguments).JSON()
		} else if v.IsObject() {
			args = v.Raw()
		} else {
			args = (&wire.Dict{}).Set("_value", v.Raw()).JSON()
		}
	}
	call := (&wire.Dict{}).SetString("name", tc.Name).Set("args", args)
	if id := callID(tc); withIDs && id != "" {
		call.SetString("id", id)
	}
	sig := callSignature(tc.ExtraContent)
	if sig == "" {
		sig = skipSignature
	}
	return (&wire.Dict{}).Set("functionCall", call.JSON()).SetString("thoughtSignature", sig).JSON()
}

// Id a replayed tool call is known by
func callID(tc *wire.ToolCall) string {
	if tc.ID != "" {
		return tc.ID
	}
	return tc.CallID
}

// Reports whether a parsed value holds a JSON Schema pointer, which Gemini 3 would try to resolve
func looksLikeSchema(v wire.Value) bool {
	for _, f := range v.Fields() {
		if f.Key == "$ref" && strings.HasPrefix(f.Value.Text(), "#/") || looksLikeSchema(f.Value) {
			return true
		}
	}
	for _, e := range v.Elems() {
		if looksLikeSchema(e) {
			return true
		}
	}
	return false
}

// functionResponse part for a tool result, structured when its text is a JSON object and opaque otherwise
func functionResponsePart(m *wire.Message, names map[string]string, withIDs, gemini3 bool) json.RawMessage {
	name := names[m.ToolCallID]
	for _, candidate := range [...]string{m.Name, m.ToolCallID, "tool"} {
		if name == "" {
			name = candidate
		}
	}
	content := contentText(m)
	response := (&wire.Dict{}).SetString("output", content).JSON()
	if trimmed := strip(content); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if parsed, err := wire.ParseValue([]byte(content)); err == nil && parsed.IsObject() && !looksLikeSchema(parsed) {
			response = parsed.Raw()
		}
	}
	fr := (&wire.Dict{}).SetString("name", name).Set("response", response)
	if withIDs && m.ToolCallID != "" {
		fr.SetString("id", m.ToolCallID)
	}
	if gemini3 {
		var images []json.RawMessage
		for i := range m.Parts {
			if data := inlineData(&m.Parts[i]); data != nil {
				images = append(images, data)
			}
		}
		if len(images) > 0 {
			fr.Set("parts", wire.Array(images...))
		}
	}
	return (&wire.Dict{}).Set("functionResponse", fr.JSON()).JSON()
}

// One turn of native contents under construction
type content struct {
	role         string
	parts        []json.RawMessage
	hasResponses bool
}

// Native contents and system instruction for a conversation, adjacent same-role turns merged
func buildContents(messages []wire.Message, withIDs, gemini3 bool) ([]json.RawMessage, json.RawMessage) {
	var system []string
	var turns []content
	names := map[string]string{}
	for i := range messages {
		m := &messages[i]
		switch m.Role {
		case wire.RoleSystem:
			if text := contentText(m); text != "" {
				system = append(system, text)
			}
			continue
		case wire.RoleTool, "function":
			turns = append(turns, content{"user", []json.RawMessage{functionResponsePart(m, names, withIDs, gemini3)}, true})
			continue
		}
		parts := multimodalParts(m)
		lastIsCall := false
		for c := range m.ToolCalls {
			tc := &m.ToolCalls[c]
			if id := callID(tc); id != "" && tc.Name != "" {
				names[id] = tc.Name
			}
			parts, lastIsCall = append(parts, functionCallPart(tc, withIDs)), true
		}
		if m.Role == wire.RoleAssistant && len(parts) > 0 && !lastIsCall && m.ThoughtSignature != "" {
			last := wire.DictOf(mustValue(parts[len(parts)-1]))
			parts[len(parts)-1] = last.SetString("thoughtSignature", m.ThoughtSignature).JSON()
		}
		if len(parts) == 0 {
			continue
		}
		role := "user"
		if m.Role == wire.RoleAssistant {
			role = "model"
		}
		turns = append(turns, content{role, parts, false})
	}
	var merged []content
	for _, turn := range turns {
		same := len(merged) > 0 && merged[len(merged)-1].role == turn.role
		if same && turn.role == "user" && merged[len(merged)-1].hasResponses != turn.hasResponses {
			same = false
			placeholder := (&wire.Dict{}).SetString("text", interruptedPlaceholder).JSON()
			merged = append(merged, content{"model", []json.RawMessage{placeholder}, false})
		}
		if same {
			last := &merged[len(merged)-1]
			last.parts = append(last.parts, turn.parts...)
			last.hasResponses = last.hasResponses || turn.hasResponses
			continue
		}
		merged = append(merged, turn)
	}
	out := make([]json.RawMessage, len(merged))
	for i, turn := range merged {
		out[i] = (&wire.Dict{}).SetString("role", turn.role).Set("parts", wire.Array(turn.parts...)).JSON()
	}
	joined := strip(strings.Join(system, "\n"))
	if joined == "" {
		return out, nil
	}
	text := (&wire.Dict{}).SetString("text", joined).JSON()
	return out, (&wire.Dict{}).SetString("role", "system").Set("parts", wire.Array(text)).JSON()
}

// Native tools member for function tools, nil when the request declares none
func buildTools(tools []wire.Tool, jsonSchema bool) json.RawMessage {
	var declarations []json.RawMessage
	for i := range tools {
		t := &tools[i]
		if t.Name == "" {
			continue
		}
		decl := (&wire.Dict{}).SetString("name", t.Name)
		if t.Description != "" {
			decl.SetString("description", t.Description)
		}
		if params, err := wire.ParseValue(t.Parameters); err == nil && params.IsObject() {
			if jsonSchema {
				decl.Set("parametersJsonSchema", prepareParameters(params))
			} else {
				decl.Set("parameters", sanitizeParameters(params))
			}
		}
		declarations = append(declarations, decl.JSON())
	}
	if len(declarations) == 0 {
		return nil
	}
	return wire.Array((&wire.Dict{}).Set("functionDeclarations", wire.Array(declarations...)).JSON())
}

// Native toolConfig for a tool choice with the calling mode it selects, nil when the choice names nothing known
func buildToolChoice(c wire.ToolChoice) (json.RawMessage, string) {
	config := &wire.Dict{}
	switch {
	case c.Name != "":
		config.SetString("mode", "ANY").Set("allowedFunctionNames", wire.Strings([]string{c.Name}))
	case c.Mode == "auto":
		config.SetString("mode", "AUTO")
	case c.Mode == "required":
		config.SetString("mode", "ANY")
	case c.Mode == "none":
		config.SetString("mode", "NONE")
	default:
		return nil, ""
	}
	return (&wire.Dict{}).Set("functionCallingConfig", config.JSON()).JSON(), config.Get("mode").Text()
}

// generationConfig members that ask for JSON output, nil for no structured output request
func jsonOutput(f *wire.ResponseFormat, jsonSchema bool) []wire.Member {
	if f == nil || (f.Type != "json_object" && f.Type != "json_schema") {
		return nil
	}
	out := []wire.Member{{Key: "responseMimeType", Value: wire.Quote("application/json")}}
	schema, err := wire.ParseValue(f.Schema)
	if f.Type != "json_schema" || err != nil || !schema.IsObject() {
		return out
	}
	if jsonSchema {
		return append(out, wire.Member{Key: "responseJsonSchema", Value: prepareParameters(schema)})
	}
	return append(out, wire.Member{Key: "responseSchema", Value: sanitizeParameters(schema)})
}

// Structured output request the extra body carries when the request itself has none
func extraResponseFormat(v wire.Value) *wire.ResponseFormat {
	if !v.IsObject() {
		return nil
	}
	f := &wire.ResponseFormat{Type: v.Get("type").Text()}
	if spec := v.Get("json_schema"); spec.IsObject() {
		f.Name = spec.Get("name").Text()
		if schema := spec.Get("schema"); schema.IsObject() {
			f.Schema = schema.Raw()
		}
	}
	return f
}

// Builds a generateContent call, the streaming method with server-sent events when the request streams
func (p Protocol) Build(req *wire.Request) (*wire.Call, error) {
	o := &req.Gemini
	extra := func(key string) wire.Value {
		v, err := wire.ParseValue(req.ExtraBody[key])
		if err != nil {
			return wire.Value{}
		}
		return v
	}
	maxTokens := req.MaxTokens
	if budget, ok := extra("max_output_tokens").Int(); ok && int(budget) > maxTokens {
		maxTokens = int(budget)
	}
	thinkingRaw := extra("thinking_config")
	if !thinkingRaw.Truthy() {
		thinkingRaw = extra("thinkingConfig")
	}
	format := req.ResponseFormat
	if format == nil {
		format = extraResponseFormat(extra("response_format"))
	}
	jsonSchema := acceptsJSONSchema(o.BaseURL)
	gemini3 := RequiresToolCallIDs(req.Model)
	contents, system := buildContents(req.Messages, gemini3, gemini3)
	body := (&wire.Dict{}).Set("contents", wire.Array(contents...))
	if system != nil {
		body.Set("systemInstruction", system)
	}
	tools := buildTools(req.Tools, jsonSchema)
	if tools != nil {
		body.Set("tools", tools)
	}
	toolConfig, mode := buildToolChoice(req.ToolChoice)
	if toolConfig != nil {
		body.Set("toolConfig", toolConfig)
	}
	thinking := normalizeThinking(thinkingRaw)
	generation := &wire.Dict{}
	if req.Temperature != nil && !req.OmitTemperature {
		generation.SetFloat("temperature", *req.Temperature)
	}
	limit := effectiveMaxOutputTokens(maxTokens, thinking)
	generation.SetInt("maxOutputTokens", limit)
	if req.TopP != nil {
		generation.SetFloat("topP", *req.TopP)
	}
	if len(req.Stop) > 0 {
		generation.Set("stopSequences", wire.Strings(req.Stop))
	}
	if thinking != nil {
		generation.Set("thinkingConfig", thinking)
	}
	if output := jsonOutput(format, jsonSchema); output != nil && mode != "ANY" && (tools == nil || gemini3) {
		for _, mb := range output {
			generation.Set(mb.Key, mb.Value)
		}
	}
	body.Set("generationConfig", generation.JSON())
	model := BareModelID(req.Model)
	d := &decoder{p: p, req: req, model: model, body: body, maxHops: LongDecodeHops(limit) + 1, slots: map[string]*callSlot{}}
	return d.call(""), nil
}

// Call for one hop of the turn, resending the request with a continuation token after the first
func (d *decoder) call(token string) *wire.Call {
	o := &d.req.Gemini
	body := d.body
	if token != "" {
		body = wire.DictOf(d.body.Value()).SetString("continuationToken", token)
	}
	method, accept := o.GenerateMethod, "application/json"
	if method == "" {
		method = "generateContent"
	}
	if d.req.Stream {
		if method, accept = o.StreamMethod, "text/event-stream"; method == "" {
			method = "streamGenerateContent"
		}
		method += "?alt=sse"
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", accept)
	h.Set("User-Agent", d.p.client()+" (gemini-native)")
	h.Set("X-Goog-Api-Client", d.p.client())
	for k, v := range d.req.ExtraHeaders {
		h.Set(k, v)
	}
	return &wire.Call{Method: http.MethodPost, Path: "/models/" + d.model + ":" + method, Header: h,
		Body: wire.ScrubSurrogates(body.JSON()), Stream: d.req.Stream, Decode: d}
}

// Reason and metadata of the first error info detail of a Google error object
func errorInfo(errObj wire.Value) string {
	for _, detail := range errObj.Get("details").Elems() {
		if strings.HasSuffix(detail.Get("@type").Text(), "/google.rpc.ErrorInfo") {
			if reason, ok := detail.Get("reason").Str(); ok {
				return reason
			}
		}
	}
	return ""
}

// Reports whether Google rejected a legacy Standard Cloud key, by the 401 shape or the later 400 one
func standardKeyError(status int, message, reason, keyKind string) bool {
	msg := strings.ToLower(message)
	if status == 401 && (reason == "ACCESS_TOKEN_TYPE_UNSUPPORTED" || strings.Contains(msg, "expected oauth 2 access token")) {
		return true
	}
	return status == 400 && keyKind == "standard" && (reason == "API_KEY_INVALID" || strings.Contains(msg, "api key not valid"))
}

// Reports whether a Gemini 429 message marks free tier exhaustion
func IsFreeTierQuotaError(message string) bool {
	return strings.Contains(strings.ToLower(message), "free_tier")
}

// Guidance for a 403 where the key and the host belong to different Google surfaces, empty when neither shape fits
func wrongSurfaceGuidance(baseURL, keyKind, errStatus string) string {
	if strings.ToUpper(strip(errStatus)) != "PERMISSION_DENIED" {
		return ""
	}
	normalized := strings.ToLower(strip(baseURL))
	switch {
	case IsVertexExpressBaseURL(normalized):
		return studioKeyOnExpressGuidance
	case strings.Contains(normalized, "generativelanguage.googleapis.com") && keyKind == "auth":
		return expressKeyOnStudioGuidance
	}
	return ""
}

// Typed failure for a 4xx or 5xx reply with the remedy text the key and surface call for
func (d *decoder) Error(status int, header http.Header, body []byte) error {
	o := &d.req.Gemini
	text := string(bytes.ToValidUTF8(body, []byte("�")))
	var errObj wire.Value
	if parsed, err := wire.ParseValue(body); err == nil {
		if errObj = parsed.Get("error"); !errObj.IsObject() {
			errObj = wire.Value{}
		}
	}
	field := func(key string) string {
		if v := errObj.Get(key); v.Truthy() {
			return strip(v.Display())
		}
		return ""
	}
	errStatus, errMessage := field("status"), field("message")
	reason := errorInfo(errObj)
	message := "Gemini returned HTTP " + strconv.Itoa(status) + ": " + firstRunes(text, 500)
	if errMessage != "" {
		label := errStatus
		if label == "" {
			label = "error"
		}
		message = "Gemini HTTP " + strconv.Itoa(status) + " (" + label + "): " + errMessage
	}
	basis := errMessage
	if basis == "" {
		basis = text
	}
	if o.KeyGuidance && status == 429 && IsFreeTierQuotaError(basis) {
		message += freeTierGuidance
	}
	if o.KeyGuidance && standardKeyError(status, basis, reason, o.KeyKind) {
		message += standardKeyGuidance
	}
	if o.KeyGuidance && status == 403 {
		message += wrongSurfaceGuidance(o.BaseURL, o.KeyKind, errStatus)
	}
	code := "gemini_http_" + strconv.Itoa(status)
	switch status {
	case 401:
		code = "gemini_unauthorized"
	case 429:
		code = "gemini_rate_limited"
	case 404:
		code = "gemini_model_not_found"
	}
	return wire.NewAPIErrorDetail(status, header, code, message, body)
}

// First n characters of a text
func firstRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// Tier of a Google AI Studio key from one minimal request, free, paid, or unknown when the probe fails
func (p Protocol) ProbeTier(ctx context.Context, client *http.Client, apiKey, baseURL, model string) string {
	key := strip(apiKey)
	if key == "" {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1}}`
	target := NormalizeBaseURL(baseURL) + "/models/" + model + ":generateContent?key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(payload))
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Client", p.client())
	resp, err := client.Do(req)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if perDay, err := strconv.Atoi(strip(resp.Header.Get("x-ratelimit-limit-requests-per-day"))); err == nil {
		if perDay <= 1000 {
			return "free"
		}
		return "paid"
	}
	switch {
	case resp.StatusCode == 429 && IsFreeTierQuotaError(string(text)):
		return "free"
	case resp.StatusCode == 429, resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "paid"
	}
	return "unknown"
}

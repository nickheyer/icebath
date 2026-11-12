package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ChatAux is the auxiliary client surface sampling needs, an OpenAI shaped chat completion that forwards tools
type ChatAux interface {
	CompleteChat(ctx context.Context, messages []map[string]any, toolDefs []map[string]any, opts tools.AuxOptions) (map[string]any, error)
}

type callEnvKey struct{}

// Binds the in flight tool call's environment so elicitation consent reaches the calling session
func withCallEnv(ctx context.Context, env *tools.Env) context.Context {
	return context.WithValue(ctx, callEnvKey{}, env)
}

// Environment of the in flight tool call bound on the context, nil when none
func callEnvFrom(ctx context.Context) *tools.Env {
	if ctx == nil {
		return nil
	}
	env, _ := ctx.Value(callEnvKey{}).(*tools.Env)
	return env
}

// Wall clock seconds of the sampling rate window, swapped by tests
var samplingNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var stopReasonMap = map[string]string{"stop": "endTurn", "length": "maxTokens", "tool_calls": "toolUse"}

var samplingLogLevels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warning": slog.LevelWarn}

// SDK class name of a content block type for the unsupported block warning
var contentBlockClass = map[string]string{
	"text": "TextContent", "image": "ImageContent", "audio": "AudioContent", "resource": "EmbeddedResource",
	"resource_link": "ResourceLink", "tool_use": "ToolUseContent", "tool_result": "ToolResultContent",
}

// Truthiness of a config value the way is_truthy_value reads it
func isTruthyValue(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case bool:
		return x
	case string:
		return trueWords[strings.ToLower(strings.TrimSpace(x))]
	}
	return config.Truthy(v)
}

// Content blocks of a sampling message, a single block or a list
func samplingBlocks(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	return []json.RawMessage{raw}
}

// Block fields decoded, raw values kept for order preserving re-serialization
func blockFields(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return map[string]json.RawMessage{}
	}
	return m
}

// Decoded string of a raw JSON field, false when absent or not a string
func fieldString(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Tool use id of a tool result block under either spelling
func toolUseID(m map[string]json.RawMessage) (json.RawMessage, bool) {
	if raw, ok := m["tool_use_id"]; ok {
		return raw, true
	}
	raw, ok := m["toolUseId"]
	return raw, ok
}

// Text of a tool result block joining every content item that carries text
func toolResultText(m map[string]json.RawMessage) string {
	raw, ok := m["content"]
	if !ok || string(raw) == "null" {
		return ""
	}
	var parts []string
	for _, item := range samplingBlocks(raw) {
		if text, ok := fieldString(blockFields(item), "text"); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// One OpenAI content part of a text or data block, nil when unsupported
func contentPart(m map[string]json.RawMessage) map[string]any {
	if text, ok := fieldString(m, "text"); ok {
		return map[string]any{"type": "text", "text": text}
	}
	mime, hasMime := fieldString(m, "mimeType")
	if !hasMime {
		mime, hasMime = fieldString(m, "mime_type")
	}
	if data, ok := fieldString(m, "data"); ok && hasMime {
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," + data}}
	}
	kind, _ := fieldString(m, "type")
	name := contentBlockClass[kind]
	if name == "" {
		name = kind
	}
	mcpLogger().Warn(fmt.Sprintf("Unsupported sampling content block type: %s (skipped)", name))
	return nil
}

// OpenAI tool call of a tool use block, dict inputs rendered like json.dumps without ASCII escaping
func toolCallDict(m map[string]json.RawMessage, index int) map[string]any {
	id := fmt.Sprintf("call_%d", index)
	if s, ok := fieldString(m, "id"); ok {
		id = s
	}
	name, _ := fieldString(m, "name")
	raw := m["input"]
	var args string
	if trimmed := strings.TrimSpace(string(raw)); strings.HasPrefix(trimmed, "{") {
		args = pyDumpsRaw(raw)
	} else {
		var v any
		_ = json.Unmarshal(raw, &v)
		args = pyStr(v)
	}
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

// One sampling message to OpenAI messages, tool results first then tool calls or plain content
func convertSamplingMessage(msg SamplingMessage) []map[string]any {
	var results, toolUses, content []map[string]json.RawMessage
	for _, raw := range samplingBlocks(msg.Content) {
		m := blockFields(raw)
		_, isResult := toolUseID(m)
		_, hasName := m["name"]
		_, hasInput := m["input"]
		switch {
		case isResult:
			results = append(results, m)
		case hasName && hasInput:
			toolUses = append(toolUses, m)
		default:
			content = append(content, m)
		}
	}
	var out []map[string]any
	for _, m := range results {
		raw, _ := toolUseID(m)
		var id any
		_ = json.Unmarshal(raw, &id)
		out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": toolResultText(m)})
	}
	switch {
	case len(toolUses) > 0:
		calls := make([]any, len(toolUses))
		for i, m := range toolUses {
			calls[i] = toolCallDict(m, i)
		}
		entry := map[string]any{"role": msg.Role, "tool_calls": calls}
		var texts []string
		for _, m := range content {
			if text, ok := fieldString(m, "text"); ok {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			entry["content"] = strings.Join(texts, "\n")
		}
		out = append(out, entry)
	case len(content) == 1 && hasField(content[0], "text"):
		text, _ := fieldString(content[0], "text")
		out = append(out, map[string]any{"role": msg.Role, "content": text})
	case len(content) > 0:
		var parts []any
		for _, m := range content {
			if p := contentPart(m); p != nil {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": msg.Role, "content": parts})
		}
	}
	return out
}

// Reports whether a decoded block carries a string field
func hasField(m map[string]json.RawMessage, key string) bool {
	_, ok := fieldString(m, key)
	return ok
}

// LLM tool call arguments as a mapping, malformed JSON and non mappings wrapped under _raw
func parseToolCallArguments(serverName string, args any) (any, bool) {
	if s, ok := args.(string); ok {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s': malformed tool_calls arguments from LLM (wrapping as raw): %s", serverName, truncateRunes(s, 100)))
			return map[string]any{"_raw": s}, true
		}
		_, isMap := v.(map[string]any)
		return v, isMap
	}
	if m, ok := args.(map[string]any); ok {
		return m, true
	}
	return map[string]any{"_raw": pyStr(args)}, true
}

// First n code points of a string
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Answers sampling/createMessage for one server through the auxiliary client with rate limit and model allowlist
type samplingHandler struct {
	serverName        string
	env               *tools.Env
	maxRPM            int
	timeout           float64
	maxTokensCap      int
	maxToolRounds     int
	exposeClientTools bool
	modelOverride     any
	allowedModels     any
	auditLevel        slog.Level

	mu             sync.Mutex
	rateTimestamps []float64
	toolLoopCount  int
	metrics        map[string]int
}

// Sampler for one server built from its sampling config block
func newSamplingHandler(name string, env *tools.Env, cfg map[string]any) Sampler {
	timeout, ok := cfg["timeout"]
	if !ok {
		timeout = 30
	}
	level, ok := samplingLogLevels[strings.ToLower(pyStr(cfgOr(cfg, "log_level", "info")))]
	if !ok {
		level = slog.LevelInfo
	}
	allowed, ok := cfg["allowed_models"]
	if !ok {
		allowed = []any{}
	}
	return &samplingHandler{
		serverName: name, env: env,
		maxRPM:            safeInt(cfgOr(cfg, "max_rpm", 10), 10, 1),
		timeout:           safeFloat(timeout, 30, 1),
		maxTokensCap:      safeInt(cfgOr(cfg, "max_tokens_cap", 4096), 4096, 1),
		maxToolRounds:     safeInt(cfgOr(cfg, "max_tool_rounds", 5), 5, 0),
		exposeClientTools: isTruthyValue(cfg["expose_client_tools"], false),
		modelOverride:     cfg["model"], allowedModels: allowed, auditLevel: level,
		metrics: map[string]int{"requests": 0, "errors": 0, "tokens_used": 0, "tool_use_count": 0},
	}
}

// Config value under key or the default when the key is absent
func cfgOr(cfg map[string]any, key string, def any) any {
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

// Sampling capability, with the tools sub capability only when the server opts in
func (h *samplingHandler) Capabilities() map[string]any {
	if h.exposeClientTools {
		return map[string]any{"tools": map[string]any{}}
	}
	return map[string]any{}
}

// Copy of the request, error, token and tool use counters
func (h *samplingHandler) Metrics() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]any, len(h.metrics))
	for k, v := range h.metrics {
		out[k] = v
	}
	return out
}

// Sliding sixty second window, true when the request is admitted
func (h *samplingHandler) checkRateLimit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := samplingNow()
	kept := h.rateTimestamps[:0]
	for _, ts := range h.rateTimestamps {
		if ts > now-60 {
			kept = append(kept, ts)
		}
	}
	h.rateTimestamps = kept
	if len(kept) >= h.maxRPM {
		return false
	}
	h.rateTimestamps = append(h.rateTimestamps, now)
	return true
}

// Config override, else the first named server hint, else empty for the default model
func (h *samplingHandler) resolveModel(prefs *ModelPreferences) string {
	if pyTruthy(h.modelOverride) {
		return pyStr(h.modelOverride)
	}
	if prefs != nil {
		for _, hint := range prefs.Hints {
			if hint.Name != "" {
				return hint.Name
			}
		}
	}
	return ""
}

// Reports whether allowed_models permits the model and the joined list for the refusal
func (h *samplingHandler) modelAllowed(model string) (bool, string, bool) {
	switch list := h.allowedModels.(type) {
	case string:
		chars := strings.Split(list, "")
		return strings.Contains(list, model), strings.Join(chars, ", "), list != ""
	case []string:
		return contains(list, model), strings.Join(list, ", "), len(list) > 0
	case []any:
		names := make([]string, len(list))
		found := false
		for i, v := range list {
			names[i] = pyStr(v)
			if s, ok := v.(string); ok && s == model {
				found = true
			}
		}
		return found, strings.Join(names, ", "), len(list) > 0
	}
	return true, "", pyTruthy(h.allowedModels)
}

// Reports whether a string slice holds the value
func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// MCP error data for a refusal
func samplingError(message string) *RPCError { return &RPCError{Code: -1, Message: message} }

// Counts an error and returns its error data
func (h *samplingHandler) fail(message string) *RPCError {
	h.mu.Lock()
	h.metrics["errors"]++
	h.mu.Unlock()
	return samplingError(message)
}

// Rate limit and model allowlist gate returning the resolved model or the refusal
func (h *samplingHandler) admit(params *CreateMessageParams) (string, *RPCError) {
	if !h.checkRateLimit() {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s' sampling rate limit exceeded (%d/min)", h.serverName, h.maxRPM))
		return "", h.fail(fmt.Sprintf("Sampling rate limit exceeded for server '%s' (%d requests/minute)", h.serverName, h.maxRPM))
	}
	model := h.resolveModel(params.ModelPreferences)
	if allowed, joined, active := h.modelAllowed(model); active && model != "" && !allowed {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s' requested model '%s' not in allowed_models", h.serverName, model))
		return "", h.fail(fmt.Sprintf("Model '%s' not allowed for server '%s'. Allowed: %s", model, h.serverName, joined))
	}
	return model, nil
}

// Sampling params to the chat messages, tool definitions and options of the auxiliary call
func (h *samplingHandler) buildLLMCall(params *CreateMessageParams, model string) ([]map[string]any, []map[string]any, tools.AuxOptions) {
	var messages []map[string]any
	for _, msg := range params.Messages {
		messages = append(messages, convertSamplingMessage(msg)...)
	}
	if params.SystemPrompt != "" {
		messages = append([]map[string]any{{"role": "system", "content": params.SystemPrompt}}, messages...)
	}
	maxTokens := min(params.MaxTokens, h.maxTokensCap)
	var toolDefs []map[string]any
	for _, t := range params.Tools {
		toolDefs = append(toolDefs, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": normalizeInputSchema(t.InputSchema)}})
	}
	mcpLogger().Log(context.Background(), h.auditLevel, fmt.Sprintf("MCP server '%s' sampling request: model=%s, max_tokens=%d, messages=%d",
		h.serverName, model, maxTokens, len(messages)))
	return messages, toolDefs, tools.AuxOptions{Task: "mcp", Model: model, MaxTokens: maxTokens, Temperature: params.Temperature,
		Timeout: time.Duration(h.timeout * float64(time.Second))}
}

// Runs the auxiliary chat call bounded by the sampling timeout
func (h *samplingHandler) callLLM(ctx context.Context, messages, toolDefs []map[string]any, opts tools.AuxOptions) (map[string]any, error, bool) {
	var aux ChatAux
	if h.env != nil {
		aux, _ = h.env.AuxClient().(ChatAux)
	}
	if aux == nil {
		return nil, fmt.Errorf("no auxiliary LLM client with chat completions is configured for task 'mcp'"), false
	}
	callCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	type outcome struct {
		resp map[string]any
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: fmt.Errorf("%v", r)}
			}
		}()
		resp, err := aux.CompleteChat(callCtx, messages, toolDefs, opts)
		done <- outcome{resp, err}
	}()
	select {
	case o := <-done:
		if o.err != nil && callCtx.Err() == context.DeadlineExceeded {
			return nil, o.err, true
		}
		return o.resp, o.err, false
	case <-callCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err(), false
		}
		return nil, callCtx.Err(), true
	}
}

// Logs the sampling response at the configured audit level
func (h *samplingHandler) logResponse(resp map[string]any, suffix string) {
	tokens := "?"
	if usage, ok := resp["usage"].(map[string]any); ok {
		if v, ok := usage["total_tokens"]; ok {
			tokens = pyStr(v)
		}
	}
	mcpLogger().Log(context.Background(), h.auditLevel, fmt.Sprintf("MCP server '%s' sampling response: model=%s, tokens=%s", h.serverName, pyStr(resp["model"]), tokens)+suffix)
}

// Tool use result under max_tool_rounds, zero disabling tool loops
func (h *samplingHandler) buildToolUseResult(calls []any, resp map[string]any) (any, *RPCError) {
	h.mu.Lock()
	h.metrics["tool_use_count"]++
	h.toolLoopCount++
	over := h.maxToolRounds == 0 || h.toolLoopCount > h.maxToolRounds
	if over {
		h.toolLoopCount = 0
	}
	h.mu.Unlock()
	if over {
		if h.maxToolRounds == 0 {
			return nil, samplingError(fmt.Sprintf("Tool loops disabled for server '%s' (max_tool_rounds=0)", h.serverName))
		}
		return nil, samplingError(fmt.Sprintf("Tool loop limit exceeded for server '%s' (max %d rounds)", h.serverName, h.maxToolRounds))
	}
	content := make([]any, 0, len(calls))
	for _, raw := range calls {
		tc, _ := pyjson.Dict(raw)
		fn, _ := pyjson.Dict(tc["function"])
		input, ok := parseToolCallArguments(h.serverName, fn["arguments"])
		if !ok {
			return nil, &RPCError{Code: CodeInternalError, Message: "Input should be a valid dictionary"}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": pyStr(tc["id"]), "name": pyStr(fn["name"]), "input": input})
	}
	h.logResponse(resp, fmt.Sprintf(", tool_calls=%d", len(content)))
	return map[string]any{"role": "assistant", "content": content, "model": pyStr(resp["model"]), "stopReason": "toolUse"}, nil
}

// Text result of a normal response, resetting the tool loop
func (h *samplingHandler) buildTextResult(choice, resp map[string]any) any {
	h.mu.Lock()
	h.toolLoopCount = 0
	h.mu.Unlock()
	h.logResponse(resp, "")
	message, _ := choice["message"].(map[string]any)
	text := ""
	if pyTruthy(message["content"]) {
		text = pyStr(message["content"])
	}
	reason, ok := stopReasonMap[pyStr(choice["finish_reason"])]
	if !ok {
		reason = "endTurn"
	}
	return map[string]any{"role": "assistant", "model": pyStr(resp["model"]),
		"content": map[string]any{"type": "text", "text": sanitizeError(text)}, "stopReason": reason}
}

// Answers one sampling request with a text result, a tool use result or error data
func (h *samplingHandler) CreateMessage(ctx context.Context, params *CreateMessageParams) (any, *RPCError) {
	model, refusal := h.admit(params)
	if refusal != nil {
		return nil, refusal
	}
	messages, toolDefs, opts := h.buildLLMCall(params, model)
	resp, err, timedOut := h.callLLM(ctx, messages, toolDefs, opts)
	if timedOut {
		return nil, h.fail(fmt.Sprintf("Sampling LLM call timed out after %ss for server '%s'", pyFloatRepr(h.timeout), h.serverName))
	}
	if err != nil {
		return nil, h.fail("Sampling LLM call failed: " + sanitizeError(excStr(err)))
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, h.fail(fmt.Sprintf("LLM returned empty response (no choices) for server '%s'", h.serverName))
	}
	choice, _ := choices[0].(map[string]any)
	h.mu.Lock()
	h.metrics["requests"]++
	if usage, ok := resp["usage"].(map[string]any); ok {
		if n, ok := pyInt(usage["total_tokens"]); ok && !isFloatValue(usage["total_tokens"]) {
			h.metrics["tokens_used"] += n
		}
	}
	h.mu.Unlock()
	message, _ := choice["message"].(map[string]any)
	if calls, _ := message["tool_calls"].([]any); pyStr(choice["finish_reason"]) == "tool_calls" && len(calls) > 0 {
		return h.buildToolUseResult(calls, resp)
	}
	return h.buildTextResult(choice, resp), nil
}

// Reports whether a decoded number is a float rather than an integer
func isFloatValue(v any) bool {
	switch x := v.(type) {
	case float64:
		return x != float64(int64(x)) || strings.Contains(fmt.Sprint(x), ".")
	case json.Number:
		return strings.ContainsAny(string(x), ".eE")
	case bool:
		return true
	}
	return false
}

// Readable field list of a flat requested schema so the user sees what they approve
func formatElicitationSchemaSummary(schema map[string]any, raw json.RawMessage, serverName string) string {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return fmt.Sprintf("Approval requested by MCP server '%s'.", serverName)
	}
	lines := []string{fmt.Sprintf("Fields requested by MCP server '%s':", serverName)}
	for _, name := range schemaPropertyOrder(props, raw) {
		spec, _ := props[name].(map[string]any)
		line := "  - " + name
		if t := pyStr(spec["type"]); pyTruthy(spec["type"]) && t != "" {
			line += " (" + t + ")"
		}
		if pyTruthy(spec["description"]) {
			line += ": " + pyStr(spec["description"])
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Property names in the order the server sent them, sorted when the schema was built in process
func schemaPropertyOrder(props map[string]any, raw json.RawMessage) []string {
	var names []string
	seen := map[string]bool{}
	if top, err := orderedObject(raw); err == nil {
		if pr, ok := top.get("properties"); ok {
			if inner, err := orderedObject(pr); err == nil {
				for _, k := range inner.keys {
					if _, ok := props[k]; ok && !seen[k] {
						seen[k] = true
						names = append(names, k)
					}
				}
			}
		}
	}
	for _, k := range orderedKeys(props) {
		if !seen[k] {
			names = append(names, k)
		}
	}
	return names
}

// Grace over the consent's own timeout so the session never blocks indefinitely
var elicitationOuterGrace = 5 * time.Second

// Consent router the elicitation handler asks, swapped by tests
var elicitationConsent = tools.RequestElicitationConsent

// Answers elicitation/create for one server through the approval surface, declining URL mode and failing closed
type elicitationHandler struct {
	serverName  string
	env         *tools.Env
	timeout     float64
	callContext func() context.Context

	mu      sync.Mutex
	metrics map[string]int
}

// Elicitor for one server, the consent reaching the in flight call's session when one is captured
func newElicitationHandler(name string, env *tools.Env, cfg map[string]any, callContext func() context.Context) Elicitor {
	if callContext == nil {
		callContext = func() context.Context { return nil }
	}
	return &elicitationHandler{serverName: name, env: env, timeout: safeFloat(cfgOr(cfg, "timeout", 300), 300, 1),
		callContext: callContext, metrics: map[string]int{"requests": 0, "accepted": 0, "declined": 0, "errors": 0}}
}

// Form mode elicitation capability, URL mode is never advertised
func (h *elicitationHandler) Capabilities() map[string]any { return map[string]any{} }

// Copy of the request, accepted, declined and error counters
func (h *elicitationHandler) Metrics() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]any, len(h.metrics))
	for k, v := range h.metrics {
		out[k] = v
	}
	return out
}

// Counts the metric and returns the action, accept carrying empty content
func (h *elicitationHandler) result(action, metric string) *ElicitResult {
	h.mu.Lock()
	h.metrics[metric]++
	h.mu.Unlock()
	if action == "accept" {
		return &ElicitResult{Action: action, Content: map[string]any{}}
	}
	return &ElicitResult{Action: action}
}

// Asks the session owning the in flight call, else the server's own surface, for consent
func (h *elicitationHandler) Elicit(ctx context.Context, params *ElicitParams) (*ElicitResult, *RPCError) {
	h.mu.Lock()
	h.metrics["requests"]++
	h.mu.Unlock()
	if params.Mode == "url" {
		mcpLogger().Info(fmt.Sprintf("MCP server '%s' requested URL-mode elicitation; declining (URL-mode elicitation not implemented)", h.serverName))
		return h.result("decline", "declined"), nil
	}
	message := params.Message
	if message == "" {
		message = fmt.Sprintf("MCP server '%s' is requesting your approval", h.serverName)
	}
	schema := params.RequestedSchema
	if schema == nil {
		schema = map[string]any{}
	}
	mcpLogger().Info(fmt.Sprintf("MCP server '%s' elicitation request: %s", h.serverName, truncateRunes(sanitizeError(message), 200)))
	description := formatElicitationSchemaSummary(schema, params.RawRequestedSchema, h.serverName)
	base, env := ctx, h.env
	if captured := h.callContext(); captured != nil {
		base = context.WithoutCancel(captured)
		if e := callEnvFrom(captured); e != nil {
			env = e
		}
	}
	consentCtx, cancel := context.WithCancel(base)
	defer cancel()
	type outcome struct {
		answer string
		err    any
	}
	done := make(chan outcome, 1)
	consent := elicitationConsent
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: r}
			}
		}()
		done <- outcome{answer: consent(consentCtx, env, message, description, tools.ElicitationOptions{
			Timeout: time.Duration(int(h.timeout)) * time.Second, Surface: "mcp-elicitation/" + h.serverName})}
	}()
	timer := time.NewTimer(time.Duration(h.timeout*float64(time.Second)) + elicitationOuterGrace)
	defer timer.Stop()
	select {
	case o := <-done:
		if o.err != nil {
			mcpLogger().Error(fmt.Sprintf("MCP server '%s' elicitation failed: %v", h.serverName, o.err))
			return h.result("decline", "errors"), nil
		}
		switch o.answer {
		case "accept":
			return h.result("accept", "accepted"), nil
		case "cancel":
			return h.result("cancel", "errors"), nil
		}
		return h.result("decline", "declined"), nil
	case <-timer.C:
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s' elicitation timed out after %ds", h.serverName, int(h.timeout)))
		return h.result("cancel", "errors"), nil
	case <-ctx.Done():
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s' elicitation timed out after %ds", h.serverName, int(h.timeout)))
		return h.result("cancel", "errors"), nil
	}
}

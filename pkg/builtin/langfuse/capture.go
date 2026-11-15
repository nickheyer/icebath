package langfuse

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	readFileHeadLines = 25
	readFileTailLines = 15
	endUserSetting    = "end_user_attribution"
	defaultCapture    = "sanitized"
)

var (
	readFileLineRe  = pyre.MustCompile(`^\s*(\d+)\|(.*)$`)
	readFileMetaKey = []string{"total_lines", "file_size", "truncated", "is_binary", "is_image", "hint",
		"_warning", "mime_type", "dimensions", "similar_files", "error"}
	captureModes         = []string{"metadata", "sanitized", "full"}
	warnedInvalidCapture atomic.Bool
	warnedInvalidEndUser atomic.Bool
	responsesItemTypes   = map[string]bool{"function_call": true, "function_call_output": true, "reasoning": true, "compaction": true}
	responsesItemKeys    = map[string]bool{"type": true, "call_id": true, "name": true, "arguments": true, "output": true, "summary": true}
	responsesContentKeys = map[string]bool{"arguments": true, "output": true, "summary": true}
)

// Logger of the plugin module
func lfLog() *slog.Logger { return slog.Default().With("logger", "icebath_plugins.langfuse") }

// Stripped process env value, the default stripped when unset
func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return pystr.Strip(v)
	}
	return pystr.Strip(def)
}

// Logs a plugin message at info when ICEBATH_LANGFUSE_DEBUG is on
func debugLog(message string) {
	switch strings.ToLower(envStr("ICEBATH_LANGFUSE_DEBUG", "")) {
	case "1", "true", "yes", "on":
		lfLog().Info("Langfuse tracing: " + message)
	}
}

// Runs fn swallowing a panic into a debug line so telemetry never blocks the turn
func failsafe(label string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			debugLog(fmt.Sprintf("%s failed: %v", label, r))
		}
	}()
	fn()
}

// Active capture mode read per call, an invalid value warning once and meaning sanitized
func captureMode() string {
	value := strings.ToLower(envStr("ICEBATH_LANGFUSE_CAPTURE", ""))
	if value == "" {
		return defaultCapture
	}
	for _, m := range captureModes {
		if m == value {
			return value
		}
	}
	if warnedInvalidCapture.CompareAndSwap(false, true) {
		lfLog().Warn(fmt.Sprintf("Langfuse plugin: invalid ICEBATH_LANGFUSE_CAPTURE=%s, falling back to %s (valid: %s)",
			pyrepr.Repr(value), pyrepr.Repr(defaultCapture), strings.Join(captureModes, ", ")))
	}
	return defaultCapture
}

// Profile opt-in to export the sender as the trace user, anything but a boolean warning once and meaning off
func endUserAttribution(ctx context.Context) bool {
	pc := pluginContext()
	if pc == nil {
		return false
	}
	value, err := pc.GetConfig(ctx, endUserSetting, false)
	if err != nil {
		panic(err)
	}
	if b, ok := value.(bool); ok {
		return b
	}
	if warnedInvalidEndUser.CompareAndSwap(false, true) {
		key := fmt.Sprintf("plugins.entries.%s.settings.%s", pc.PluginID(), endUserSetting)
		lfLog().Warn(fmt.Sprintf("Langfuse plugin: %s=%s is not true or false, so traces get no userId. "+
			"Fix it with `icebath config set %s true` (or false).", key, pyrepr.Repr(value), key))
	}
	return false
}

// Telemetry egress scrub forced on, URL credentials included, failing closed
func redactSecrets(value string) string {
	return tools.RedactForEgress(nil, tools.RedactSensitiveTextWith(nil, value, tools.RedactOptions{Force: true, URLCredentials: true}))
}

// Metadata mode stand in for content, its shape and size and never its payload
func describeContent(value any) any {
	switch t := value.(type) {
	case nil, bool:
		return value
	case []byte:
		return obj("omitted", true, "type", "bytes", "length", len(t))
	case string:
		return obj("omitted", true, "type", "text", "chars", pystr.Len(t))
	}
	if isNumber(value) {
		return obj("omitted", true, "type", "number")
	}
	if items, ok := dictItems(value); ok {
		keys := []any{}
		for i, it := range items {
			if i >= 20 {
				break
			}
			keys = append(keys, it.key)
		}
		return obj("omitted", true, "type", "object", "keys", keys)
	}
	if elems, ok := listItems(value); ok {
		return obj("omitted", true, "type", "array", "items", len(elems))
	}
	return obj("omitted", true, "type", pyrepr.TypeName(value))
}

// Tool name and arguments marking a content value as one tool's result
type toolResult struct {
	name string
	args any
}

// Content value under the active capture mode, tool results JSON parsed and read_file payloads collapsed first
func captureContent(value any, parseJSON bool, result *toolResult) any {
	if captureMode() == "metadata" {
		return describeContent(value)
	}
	if result != nil {
		if s, ok := value.(string); ok {
			value, _ = maybeParseJSONString(s)
		}
		value, _ = normalizePayload(value, result.name, result.args)
		parseJSON = true
	}
	return safeValue(value, parseJSON)
}

// String of value cut to maxChars code points after secret redaction, base64 data URIs replaced by a stub
func truncateText(value string, maxChars int) any {
	prefix := strings.ToLower(pystr.Slice(value, 0, 200))
	if strings.HasPrefix(prefix, "data:") && strings.Contains(prefix, ";base64,") {
		header := "data:"
		if strings.Contains(value, ",") {
			header = strings.SplitN(value, ",", 2)[0]
		}
		var mediaType any
		if strings.HasPrefix(header, "data:") {
			if mt := strings.SplitN(header[5:], ";", 2)[0]; mt != "" {
				mediaType = mt
			}
		}
		return obj("type", "data_uri", "media_type", mediaType, "omitted", true, "length", pystr.Len(value))
	}
	if captureMode() == "sanitized" {
		value = redactSecrets(value)
	}
	over := pystr.Len(value) - maxChars
	if over <= 0 {
		return value
	}
	return pystr.Slice(value, 0, maxChars) + fmt.Sprintf("... [truncated %d chars]", over)
}

// JSON object or array a string holds, trailing text kept under _hint or _trailing_text, false when unchanged
func maybeParseJSONString(value string) (any, bool) {
	stripped := pystr.Strip(value)
	rs := pystr.Runes(stripped)
	if len(rs) < 2 || (rs[0] != '{' && rs[0] != '[') {
		return value, false
	}
	parsed, idx, err := pyjson.RawDecode(stripped)
	if err != nil {
		return value, false
	}
	if !isDict(parsed) {
		if _, ok := listItems(parsed); !ok {
			return value, false
		}
	}
	trailing := pystr.Strip(pystr.FromRunes(rs[idx:]))
	if trailing == "" {
		return parsed, true
	}
	hintKey := "_trailing_text"
	if strings.HasPrefix(trailing, "[Hint:") {
		hintKey = "_hint"
	}
	if items, ok := dictItems(parsed); ok {
		out := pyjson.NewObject()
		for _, it := range items {
			out.Set(it.key, it.val)
		}
		key := hintKey
		if _, present := out.Get(hintKey); present {
			key = "_trailing_text"
		}
		out.Set(key, trailing)
		return out, true
	}
	return obj("data", parsed, hintKey, trailing), true
}

// Compact preview of a read_file result, false when the value is not one
func normalizePayload(value any, toolName string, args any) (any, bool) {
	content, isStr := dget(value, "content").(string)
	if !isDict(value) || !isStr || truthy(dget(value, "error")) {
		return value, false
	}
	for _, k := range []string{"total_lines", "file_size", "is_binary", "is_image"} {
		if _, ok := dictGet(value, k); !ok {
			return value, false
		}
	}
	normalized := pyjson.NewObject()
	if toolName == "read_file" && isDict(args) {
		if p, ok := dget(args, "path").(string); ok && p != "" {
			normalized.Set("path", p)
		}
		for _, k := range []string{"offset", "limit"} {
			if n, ok := pyIntValue(dget(args, k)); ok {
				normalized.Set(k, n)
			}
		}
	}
	var lines []any
	if content != "" {
		raw := pystr.Splitlines(content, false)
		all := len(raw) > 0
		parsed := make([]any, 0, len(raw))
		for _, line := range raw {
			m, err := readFileLineRe.FindStringMatch(line)
			if err != nil || m == nil {
				all = false
				break
			}
			groups := m.Groups()
			n, ok := pyint.Parse(groups[1].String())
			if !ok {
				all = false
				break
			}
			parsed = append(parsed, obj("line", pyint.Native(n), "text", groups[2].String()))
		}
		if all {
			lines = parsed
		}
	}
	if len(lines) > 0 {
		first, _ := lines[0].(*pyjson.Object).Get("line")
		last, _ := lines[len(lines)-1].(*pyjson.Object).Get("line")
		normalized.Set("returned_lines", obj("start", first, "end", last, "count", len(lines)))
		if len(lines) <= readFileHeadLines+readFileTailLines {
			normalized.Set("content_preview", obj("lines", lines))
		} else {
			normalized.Set("content_preview", obj("head", append([]any{}, lines[:readFileHeadLines]...),
				"tail", append([]any{}, lines[len(lines)-readFileTailLines:]...),
				"omitted_line_count", len(lines)-readFileHeadLines-readFileTailLines))
		}
	} else if content != "" {
		normalized.Set("content_preview", obj("text", content))
	}
	for _, k := range readFileMetaKey {
		if v, ok := dictGet(value, k); ok {
			normalized.Set(k, v)
		}
	}
	if b64, ok := dget(value, "base64_content").(string); ok && b64 != "" {
		normalized.Set("base64_content", obj("omitted", true, "length", pystr.Len(b64)))
	}
	return normalized, true
}

// Least recently used memo of parsed depth values so a bad value warns once per distinct value
type depthCache struct {
	mu    sync.Mutex
	order []string
	vals  map[string]int
}

var maxDepthCache = &depthCache{vals: map[string]int{}}

// Parsed ICEBATH_LANGFUSE_MAX_DEPTH, an invalid or negative value warning once and meaning 4
func resolveMaxDepth(configured string) int {
	c := maxDepthCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.vals[configured]; ok {
		for i, k := range c.order {
			if k == configured {
				c.order = append(append(c.order[:i:i], c.order[i+1:]...), configured)
				break
			}
		}
		return v
	}
	depth := 4
	n, ok := pyint.Parse(configured)
	if ok && n.Sign() >= 0 {
		if n.IsInt64() && n.Int64() <= math.MaxInt32 {
			depth = int(n.Int64())
		} else {
			depth = math.MaxInt32
		}
	} else {
		lfLog().Warn(fmt.Sprintf("Invalid ICEBATH_LANGFUSE_MAX_DEPTH=%s; use a non-negative integer. Falling back to 4.", pyrepr.Repr(configured)))
	}
	c.vals[configured] = depth
	c.order = append(c.order, configured)
	if len(c.order) > 8 {
		delete(c.vals, c.order[0])
		c.order = c.order[1:]
	}
	return depth
}

// Configured per field character cap, an invalid value raising like int() does
func maxChars() int {
	text := envStr("ICEBATH_LANGFUSE_MAX_CHARS", "12000")
	if text == "" {
		text = "12000"
	}
	n, ok := pyint.Parse(text)
	if !ok {
		panic(&pyint.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.StrRepr(text)})
	}
	return clampInt(n)
}

// Big integer clamped into the int range
func clampInt(n *big.Int) int {
	if n.IsInt64() && n.Int64() <= math.MaxInt32 && n.Int64() >= math.MinInt32 {
		return int(n.Int64())
	}
	if n.Sign() < 0 {
		return math.MinInt32
	}
	return math.MaxInt32
}

// Capture caps one value walk carries
type safeWalk struct {
	maxChars  int
	maxDepth  int
	parseJSON bool
}

// Value made safe for export with the configured caps, JSON strings parsed when asked
func safeValue(value any, parseJSON bool) any {
	depthText := envStr("ICEBATH_LANGFUSE_MAX_DEPTH", "4")
	if depthText == "" {
		depthText = "4"
	}
	w := safeWalk{maxChars: maxChars(), maxDepth: resolveMaxDepth(depthText), parseJSON: parseJSON}
	return w.walk(value, 0)
}

// Depth capped, collection capped, secret masked and truncated copy of value
func (w safeWalk) walk(value any, depth int) any {
	if depth > w.maxDepth {
		return "<max-depth>"
	}
	switch t := value.(type) {
	case nil, bool:
		return value
	case []byte:
		return obj("type", "bytes", "len", len(t))
	case string:
		if w.parseJSON {
			if parsed, changed := maybeParseJSONString(t); changed {
				return w.walk(parsed, depth)
			}
		}
		return truncateText(t, w.maxChars)
	}
	if isNumber(value) {
		return value
	}
	if items, ok := dictItems(value); ok {
		if normalized, changed := normalizePayload(value, "", nil); changed {
			return w.walk(normalized, depth)
		}
		mask := captureMode() == "sanitized"
		out := pyjson.NewObject()
		for i, it := range items {
			if i >= 50 {
				break
			}
			_, isStr := it.val.(string)
			if mask && (isStr || isNumber(it.val)) && !strEq(it.val, "") && tools.IsSecretFieldName(it.key) {
				out.Set(it.key, "[REDACTED]")
			} else {
				out.Set(it.key, w.walk(it.val, depth+1))
			}
		}
		return out
	}
	if elems, ok := listItems(value); ok {
		out := make([]any, 0, min(len(elems), 50))
		for i, e := range elems {
			if i >= 50 {
				break
			}
			out = append(out, w.walk(e, depth+1))
		}
		return out
	}
	if vars, ok := structVars(value); ok {
		return w.walk(vars, depth+1)
	}
	return truncateText(pyrepr.Repr(value), w.maxChars)
}

// First list among the request, messages and history candidates, else the user message as one turn
func coerceRequestMessages(requestMessages, messages, history, userMessage any) []any {
	for _, candidate := range []any{requestMessages, messages, history} {
		if elems, ok := listItems(candidate); ok {
			return elems
		}
	}
	if userMessage == nil {
		return []any{}
	}
	return []any{obj("role", "user", "content", userMessage)}
}

// System message from a split out system prompt string or Anthropic and Bedrock text blocks, nil when empty
func serializeSystemPrompt(systemPrompt any) any {
	text := ""
	switch t := systemPrompt.(type) {
	case string:
		text = pystr.Strip(t)
	default:
		blocks, ok := listItems(systemPrompt)
		if !ok {
			return nil
		}
		var parts []string
		for _, b := range blocks {
			var piece any = b
			if isDict(b) {
				piece = nil
				_, hasText := dictGet(b, "text")
				if kind := dget(b, "type"); (kind == nil || strEq(kind, "text")) && hasText {
					if v, ok := dictGet(b, "text"); ok {
						piece = v
					}
				}
			}
			if s, ok := piece.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		text = strings.Join(parts, "\n\n")
	}
	if text == "" {
		return nil
	}
	return obj("role", "system", "content", captureContent(text, false, nil))
}

// Generation input with the split out system prompt prepended when the messages carry none
func messagesForLangfuseInput(raw []any, systemPrompt any) []any {
	var systemMsg any
	if len(raw) > 0 {
		if !isDict(raw[0]) {
			panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw[0])))
		}
	}
	if !(len(raw) > 0 && strEq(dget(raw[0], "role"), "system")) {
		systemMsg = serializeSystemPrompt(systemPrompt)
	}
	serialized := serializeMessages(raw)
	if systemMsg == nil {
		return serialized
	}
	return append([]any{systemMsg}, serialized...)
}

// Export shape of one chat message or Responses API input item
func serializeMessage(message any) any {
	if kind, ok := dget(message, "type").(string); ok && responsesItemTypes[kind] {
		out := pyjson.NewObject()
		items, _ := dictItems(message)
		for _, it := range items {
			if !responsesItemKeys[it.key] {
				continue
			}
			if responsesContentKeys[it.key] {
				out.Set(it.key, captureContent(it.val, true, nil))
			} else {
				out.Set(it.key, it.val)
			}
		}
		return out
	}
	role := dget(message, "role")
	isTool := strEq(role, "tool")
	out := obj("role", role, "content", captureContent(dget(message, "content"), isTool, nil))
	if isTool && truthy(dget(message, "tool_call_id")) {
		out.Set("tool_call_id", dget(message, "tool_call_id"))
	}
	if isTool && truthy(dget(message, "name")) {
		out.Set("name", safeValue(dget(message, "name"), false))
	}
	if truthy(dget(message, "tool_calls")) {
		out.Set("tool_calls", captureContent(dget(message, "tool_calls"), true, nil))
	}
	return out
}

// Last twelve dict messages in export shape, empty for anything but a list
func serializeMessages(messages any) []any {
	elems, ok := listItems(messages)
	if !ok {
		return []any{}
	}
	if len(elems) > 12 {
		elems = elems[len(elems)-12:]
	}
	out := []any{}
	for _, m := range elems {
		if isDict(m) {
			out = append(out, serializeMessage(m))
		}
	}
	return out
}

// Export shape of one assistant tool call with its arguments captured once
func serializeToolCall(toolCall any) *pyjson.Object {
	fn := getattr(toolCall, "function")
	name := getattr(fn, "name")
	safeArguments := captureContent(getattr(fn, "arguments"), false, nil)
	kind := getattr(toolCall, "type")
	if !truthy(kind) {
		kind = "function"
	}
	return obj("id", getattr(toolCall, "id"), "type", kind, "name", name, "arguments", safeArguments,
		"function", obj("name", name, "arguments", safeArguments))
}

// Elements Python iteration of a value yields, dict keys and string characters included
func pyIter(v any) []any {
	if elems, ok := listItems(v); ok {
		return elems
	}
	if items, ok := dictItems(v); ok {
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = it.key
		}
		return out
	}
	if s, ok := v.(string); ok {
		rs := pystr.Runes(s)
		out := make([]any, len(rs))
		for i, r := range rs {
			out[i] = pystr.FromRunes([]rune{r})
		}
		return out
	}
	panic(fmt.Sprintf("TypeError: '%s' object is not iterable", pyrepr.TypeName(v)))
}

// Export shape of an assistant message, reasoning from the first present reasoning field
func serializeAssistantMessage(message any) *pyjson.Object {
	var reasoning any
	for _, attr := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
		if v := getattr(message, attr); v != nil {
			reasoning = v
			break
		}
	}
	var capturedReasoning any
	if reasoning != nil {
		capturedReasoning = captureContent(reasoning, false, nil)
	}
	calls := []any{}
	if raw := getattr(message, "tool_calls"); truthy(raw) {
		for _, tc := range pyIter(raw) {
			calls = append(calls, serializeToolCall(tc))
		}
	}
	return obj("content", captureContent(getattr(message, "content"), false, nil), "reasoning", capturedReasoning, "tool_calls", calls)
}

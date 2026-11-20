package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Value under key of a decoded JSON object, nil for a missing key or a non object
func cxGet(v any, key string) any {
	switch o := v.(type) {
	case *pyjson.Object:
		if o == nil {
			return nil
		}
		val, _ := o.Get(key)
		return val
	case map[string]any:
		return o[key]
	}
	return nil
}

func cxHas(v any, key string) bool {
	switch o := v.(type) {
	case *pyjson.Object:
		if o == nil {
			return false
		}
		_, ok := o.Get(key)
		return ok
	case map[string]any:
		_, ok := o[key]
		return ok
	}
	return false
}

func cxIsObject(v any) bool {
	switch o := v.(type) {
	case *pyjson.Object:
		return o != nil
	case map[string]any:
		return o != nil
	}
	return false
}

// Python truth value of a decoded JSON value
func cxTruthy(v any) bool {
	switch o := v.(type) {
	case *pyjson.Object:
		return o != nil && o.Len() > 0
	case map[string]any:
		return len(o) > 0
	case []any:
		return len(o) > 0
	case []map[string]any:
		return len(o) > 0
	}
	return pyrepr.Truthy(v)
}

// The value when truthy, else the fallback, the Python x or fallback idiom
func cxOr(v any, fallback any) any {
	if cxTruthy(v) {
		return v
	}
	return fallback
}

// String under key when truthy, else the fallback, as item.get(key) or fallback reads for text fields
func cxStr(v any, key, fallback string) string {
	val := cxGet(v, key)
	if !cxTruthy(val) {
		return fallback
	}
	if s, ok := val.(string); ok {
		return s
	}
	return pyrepr.Str(val)
}

func cxList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out
	}
	return nil
}

// json.dumps with ensure_ascii False, the text form the codex projections record
func cxDumps(v any) (string, error) {
	return pyjson.Dumps(v, pyjson.EnsureASCII(false))
}

// Stable tool call id from the codex item id, else a hash of the item type
func codexDeterministicCallID(itemType, itemID string) string {
	if itemID == "" {
		sum := sha256.Sum256([]byte(itemType))
		itemID = hex.EncodeToString(sum[:])[:16]
	}
	return "codex_" + itemType + "_" + itemID
}

// Tool arguments as sorted key JSON so replays keep prefix caches valid
func codexFormatToolArgs(d any) string {
	out, err := pyjson.Dumps(d, pyjson.EnsureASCII(false), pyjson.SortKeys(true))
	if err != nil {
		panic(err)
	}
	return out
}

func codexDictArgs(raw any) any {
	args := cxOr(raw, map[string]any{})
	if cxIsObject(args) {
		return args
	}
	return map[string]any{"arguments": args}
}

// CodexProjectionResult is the output of projecting one codex item, no messages for an ignored notification
type CodexProjectionResult struct {
	Messages        []map[string]any
	IsToolIteration bool
	FinalText       *string
}

// CodexEventProjector projects codex app server item notifications into OpenAI shaped messages
type CodexEventProjector struct {
	pendingReasoning []string
}

// Fresh projector with no stashed reasoning
func NewCodexEventProjector() *CodexEventProjector { return &CodexEventProjector{} }

type codexToolSpec func(item any) (idType, name string, args any, content string)

var codexToolProjections = map[string]codexToolSpec{
	"commandExecution": codexCommandSpec,
	"fileChange":       codexFileChangeSpec,
	"mcpToolCall":      codexMCPToolCallSpec,
	"dynamicToolCall":  codexDynamicToolCallSpec,
	"webSearch":        codexWebSearchSpec,
}

// Materializes messages for an item completed notification, every other notification projects to nothing
func (p *CodexEventProjector) Project(notification any) CodexProjectionResult {
	if method, _ := cxGet(notification, "method").(string); method != "item/completed" {
		return CodexProjectionResult{}
	}
	item := cxOr(cxGet(cxOr(cxGet(notification, "params"), map[string]any{}), "item"), map[string]any{})
	itemType := cxStr(item, "type", "")
	itemID := cxStr(item, "id", "")
	switch itemType {
	case "agentMessage":
		text := cxStr(item, "text", "")
		return CodexProjectionResult{Messages: []map[string]any{p.assistantMessage(text, nil)}, FinalText: &text}
	case "reasoning":
		for _, key := range []string{"summary", "content"} {
			for _, part := range cxList(cxOr(cxGet(item, key), []any{})) {
				p.pendingReasoning = append(p.pendingReasoning, pyrepr.Str(part))
			}
		}
		return CodexProjectionResult{}
	case "userMessage":
		return codexProjectUserMessage(item)
	}
	if spec, ok := codexToolProjections[itemType]; ok {
		return p.projectToolItem(item, itemID, spec)
	}
	return codexProjectOpaque(item, itemType)
}

// Assistant message carrying the stashed reasoning, which is consumed
func (p *CodexEventProjector) assistantMessage(content any, extra map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content}
	for k, v := range extra {
		msg[k] = v
	}
	if len(p.pendingReasoning) > 0 {
		msg["reasoning"] = strings.Join(p.pendingReasoning, "\n")
		p.pendingReasoning = nil
	}
	return msg
}

// User message from the text fragments of a userMessage item, non text parts dropped
func codexProjectUserMessage(item any) CodexProjectionResult {
	var parts []string
	for _, fragment := range cxList(cxOr(cxGet(item, "content"), []any{})) {
		if !cxIsObject(fragment) {
			continue
		}
		isText := cxGet(fragment, "type") == "text"
		if !isText && !cxHas(fragment, "text") {
			continue
		}
		if isText {
			parts = append(parts, cxStr(fragment, "text", ""))
		} else {
			parts = append(parts, pyrepr.Str(cxGet(fragment, "text")))
		}
	}
	return CodexProjectionResult{Messages: []map[string]any{{"role": "user", "content": strings.Join(parts, "\n")}}}
}

// The assistant tool call and tool result pair of a tool shaped item
func (p *CodexEventProjector) projectToolItem(item any, itemID string, spec codexToolSpec) CodexProjectionResult {
	idType, name, args, content := spec(item)
	callID := codexDeterministicCallID(idType, itemID)
	assistant := p.assistantMessage(nil, map[string]any{"tool_calls": []any{map[string]any{
		"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": codexFormatToolArgs(args)},
	}}})
	tool := map[string]any{"role": "tool", "tool_call_id": callID, "content": content}
	return CodexProjectionResult{Messages: []map[string]any{assistant, tool}, IsToolIteration: true}
}

func codexCommandSpec(item any) (string, string, any, string) {
	args := map[string]any{"command": cxStr(item, "command", ""), "cwd": cxStr(item, "cwd", "")}
	output := cxStr(item, "aggregatedOutput", "")
	if exitCode := cxGet(item, "exitCode"); exitCode != nil {
		env := pyjson.NewObject()
		env.Set("exit_code", exitCode)
		env.Set("output", output)
		output = cxMustDumps(env)
	}
	return "exec", "exec_command", args, output
}

func codexFileChangeSummary(item any) []any {
	var out []any
	for _, change := range cxList(cxOr(cxGet(item, "changes"), []any{})) {
		out = append(out, map[string]any{
			"kind": cxStr(cxOr(cxGet(change, "kind"), map[string]any{}), "type", "update"),
			"path": cxStr(change, "path", ""),
		})
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func codexFileChangeSpec(item any) (string, string, any, string) {
	changes := codexFileChangeSummary(item)
	status := cxStr(item, "status", "unknown")
	content := "apply_patch status=" + status + ", " + pyrepr.Str(len(changes)) + " change(s)"
	return "apply_patch", "apply_patch", map[string]any{"changes": changes}, content
}

// Dumped JSON truncated to n code points
func cxDumpsCut(v any, n int) string { return pystr.Slice(cxMustDumps(v), 0, n) }

// Dumped JSON, raising like json.dumps for an undumpable value
func cxMustDumps(v any) string {
	text, err := cxDumps(v)
	if err != nil {
		panic(err)
	}
	return text
}

func codexMCPToolCallSpec(item any) (string, string, any, string) {
	server := cxStr(item, "server", "mcp")
	tool := cxStr(item, "tool", "unknown")
	result, errVal := cxGet(item, "result"), cxGet(item, "error")
	content := ""
	if cxTruthy(errVal) {
		content = "[error] " + cxDumpsCut(errVal, 1000)
	} else if result != nil {
		content = cxDumpsCut(result, 4000)
	}
	return "mcp__" + server + "__" + tool, "mcp." + server + "." + tool, codexDictArgs(cxGet(item, "arguments")), content
}

func codexDynamicToolCallSpec(item any) (string, string, any, string) {
	tool := cxStr(item, "tool", "unknown")
	contentItems := cxOr(cxGet(item, "contentItems"), []any{})
	var content string
	if list, ok := contentItems.([]any); ok && len(list) > 0 {
		content = cxDumpsCut(list, 4000)
	} else {
		content = "success=" + pyrepr.Str(cxGet(item, "success"))
	}
	return "dyn_" + tool, tool, codexDictArgs(cxGet(item, "arguments")), content
}

func codexWebSearchSpec(item any) (string, string, any, string) {
	result := pyjson.NewObject()
	result.Set("provider", "codex")
	if status := cxGet(item, "status"); cxTruthy(status) {
		result.Set("status", status)
	}
	content, _ := cxDumps(result)
	return "web_search", "web_search", map[string]any{"query": cxStr(item, "query", "")}, content
}

// Opaque assistant note for an item kind with no tool shape, never a fabricated tool call
func codexProjectOpaque(item any, itemType string) CodexProjectionResult {
	payload, err := cxDumps(item)
	if err != nil {
		payload = pyrepr.Repr(item)
	}
	payload = pystr.Slice(payload, 0, 1500)
	return CodexProjectionResult{Messages: []map[string]any{{"role": "assistant", "content": "[codex " + itemType + "] " + payload}}}
}

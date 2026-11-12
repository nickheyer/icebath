package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	// One tagged tool call block with its JSON body as group one
	ToolCallBlockRe = pyre.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	// One bare OpenAI shaped tool call JSON object
	ToolCallJSONRe = pyre.MustCompile(`(?s)\{\s*\"id\"\s*:\s*\"[^\"]+\"\s*,\s*\"type\"\s*:\s*\"function\"\s*,\s*\"function\"\s*:\s*\{.*?\}\s*\}`)
)

// Prompt rule telling an ACP agent how to emit the tool calls parsed back out of its text
const ToolCallContract = "Available tools (OpenAI function schema). " +
	"When using a tool, emit ONLY <tool_call>{...}</tool_call> with one JSON object " +
	"containing id/type/function{name,arguments}. arguments must be a JSON string."

// Stream chunk list that also carries the response level attributes of the completion it came from
type StreamChunks struct {
	Chunks []map[string]any
	Extra  map[string]any
	next   int
}

// Next chunk in order, io.EOF once every chunk was read
func (s *StreamChunks) Next(ctx context.Context) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.next >= len(s.Chunks) {
		return nil, io.EOF
	}
	chunk := s.Chunks[s.next]
	s.next++
	return chunk, nil
}

// Releases nothing since every chunk is already in memory
func (s *StreamChunks) Close() error { return nil }

// Value of key in a JSON object or a struct shaped response part, present false when absent
func bridgeAttr(v any, key string) (any, bool) {
	switch o := v.(type) {
	case nil:
		return nil, false
	case map[string]any:
		val, ok := o[key]
		return val, ok
	case *pyjson.Object:
		return o.Get(key)
	}
	if val := state.ToolCallField(v, key); val != nil {
		return val, true
	}
	return nil, false
}

// Value of key, an attribute error when the part has no such key
func bridgeRequire(v any, key string) (any, error) {
	if val, ok := bridgeAttr(v, key); ok {
		return val, nil
	}
	return nil, fmt.Errorf("%s object has no attribute %s", pyrepr.StrRepr(fmt.Sprintf("%T", v)), pyrepr.StrRepr(key))
}

// Value of key or def when the part has no such key
func bridgeAttrOr(v any, key string, def any) any {
	if val, ok := bridgeAttr(v, key); ok {
		return val
	}
	return def
}

// Whether v is a JSON object
func bridgeIsDict(v any) bool {
	switch o := v.(type) {
	case map[string]any:
		return o != nil
	case *pyjson.Object:
		return o != nil
	}
	return false
}

// Items of a slice valued part, ok false for anything that is not a slice
func bridgeList(v any) ([]any, bool) {
	if l, ok := v.([]any); ok {
		return l, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// Reshapes a one shot ACP completion as a data chunk then a usage chunk, copying its other attributes
func CompletionToStreamChunks(completion map[string]any) (*StreamChunks, error) {
	rawChoices, err := bridgeRequire(completion, "choices")
	if err != nil {
		return nil, err
	}
	choices, _ := bridgeList(rawChoices)
	if len(choices) == 0 {
		return nil, errors.New("list index out of range")
	}
	choice := choices[0]
	message, err := bridgeRequire(choice, "message")
	if err != nil {
		return nil, err
	}
	tc, err := bridgeRequire(message, "tool_calls")
	if err != nil {
		return nil, err
	}
	var toolCallDeltas any
	if pyrepr.Truthy(tc) {
		calls, _ := bridgeList(tc)
		deltas := make([]any, 0, len(calls))
		for index, toolCall := range calls {
			fn, err := bridgeRequire(toolCall, "function")
			if err != nil {
				return nil, err
			}
			deltas = append(deltas, map[string]any{
				"index": index, "id": bridgeAttrOr(toolCall, "id", nil), "type": bridgeAttrOr(toolCall, "type", "function"),
				"function": map[string]any{"name": bridgeAttrOr(fn, "name", nil), "arguments": bridgeAttrOr(fn, "arguments", nil)},
			})
		}
		toolCallDeltas = deltas
	}
	c, err := bridgeRequire(message, "content")
	if err != nil {
		return nil, err
	}
	var content any
	if pyrepr.Truthy(c) {
		content = c
	}
	delta := map[string]any{
		"role": "assistant", "content": content, "tool_calls": toolCallDeltas,
		"reasoning_content": bridgeAttrOr(message, "reasoning_content", nil), "reasoning": bridgeAttrOr(message, "reasoning", nil),
	}
	finishReason, err := bridgeRequire(choice, "finish_reason")
	if err != nil {
		return nil, err
	}
	model, err := bridgeRequire(completion, "model")
	if err != nil {
		return nil, err
	}
	usage, err := bridgeRequire(completion, "usage")
	if err != nil {
		return nil, err
	}
	dataChunk := map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		"model":   model, "usage": nil,
	}
	usageChunk := map[string]any{"choices": []any{}, "model": model, "usage": usage}
	chunks := &StreamChunks{Chunks: []map[string]any{dataChunk, usageChunk}, Extra: map[string]any{}}
	for key, value := range completion {
		if key != "choices" && key != "usage" && key != "model" {
			chunks.Extra[key] = value
		}
	}
	return chunks, nil
}

// OpenAI compatible tool call for downstream handling
func BuildOpenAIToolCall(callID, name, arguments string) state.ToolCall {
	return state.ToolCall{
		ID: callID, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: arguments},
		Extra: map[string]any{"call_id": callID, "response_item_id": nil},
	}
}

// Function object and stripped name of a container whose function is a dict with a non blank name
func namedFunction(container any) (any, string, bool) {
	var fn any
	if bridgeIsDict(container) {
		fn, _ = bridgeAttr(container, "function")
	}
	if !bridgeIsDict(fn) {
		return nil, "", false
	}
	raw, _ := bridgeAttr(fn, "name")
	name, ok := raw.(string)
	if !ok || pystr.Strip(name) == "" {
		return nil, "", false
	}
	return fn, pystr.Strip(name), true
}

// Flattens OpenAI tools into name, description and parameters specs, skipping malformed entries and those outside a non nil allowlist
func ToolSpecsFromOpenAITools(tools []any, allowlist []string) []*pyjson.Object {
	var allowed map[string]bool
	if allowlist != nil {
		allowed = make(map[string]bool, len(allowlist))
		for _, n := range allowlist {
			allowed[pystr.Strip(n)] = true
		}
	}
	specs := []*pyjson.Object{}
	for _, t := range tools {
		fn, name, ok := namedFunction(t)
		if !ok || (allowed != nil && !allowed[name]) {
			continue
		}
		spec := pyjson.NewObject()
		spec.Set("name", name)
		spec.Set("description", bridgeAttrOr(fn, "description", ""))
		spec.Set("parameters", bridgeAttrOr(fn, "parameters", map[string]any{}))
		specs = append(specs, spec)
	}
	return specs
}

// Prompt sections carrying the forwarded tool schemas and the tool choice hint, empty when neither applies
func RenderToolBridgeSections(tools []any, toolChoice any, allowlist []string) ([]string, error) {
	specs := ToolSpecsFromOpenAITools(tools, allowlist)
	sections := []string{}
	if len(specs) > 0 {
		payload, err := pyjson.Dumps(specs, pyjson.EnsureASCII(false))
		if err != nil {
			return nil, err
		}
		sections = append(sections, ToolCallContract+"\n"+payload)
	}
	if toolChoice != nil {
		hint, err := pyjson.Dumps(toolChoice, pyjson.EnsureASCII(false))
		if err != nil {
			return nil, err
		}
		sections = append(sections, "Tool choice hint: "+hint)
	}
	return sections, nil
}

// One tool call JSON body as a tool call, ok false when malformed, a missing id becoming acp_call_ordinal
func parseBridgeToolCall(rawJSON string, ordinal int) (state.ToolCall, bool, error) {
	obj, err := pyjson.Loads(rawJSON)
	if err != nil {
		return state.ToolCall{}, false, nil
	}
	fn, fnName, ok := namedFunction(obj)
	if !ok {
		return state.ToolCall{}, false, nil
	}
	fnArgs := bridgeAttrOr(fn, "arguments", "{}")
	args, isStr := fnArgs.(string)
	if !isStr {
		if args, err = pyjson.Dumps(fnArgs, pyjson.EnsureASCII(false)); err != nil {
			return state.ToolCall{}, false, err
		}
	}
	rawID, _ := bridgeAttr(obj, "id")
	callID, isStr := rawID.(string)
	if !isStr || pystr.Strip(callID) == "" {
		callID = fmt.Sprintf("acp_call_%d", ordinal)
	}
	return BuildOpenAIToolCall(callID, fnName, args), true, nil
}

// Tool calls of an ACP response and its text minus consumed blocks, bare JSON tried only when no block parsed
func ExtractToolCallsFromText(text string) ([]state.ToolCall, string, error) {
	extracted := []state.ToolCall{}
	if pystr.Strip(text) == "" {
		return extracted, "", nil
	}
	runes := []rune(text)
	type span struct{ start, end int }
	var consumed []span
	for _, p := range []struct {
		re    *regexp2.Regexp
		group int
	}{{ToolCallBlockRe, 1}, {ToolCallJSONRe, 0}} {
		for m, _ := p.re.FindStringMatch(text); m != nil; m, _ = p.re.FindNextMatch(m) {
			call, ok, err := parseBridgeToolCall(m.GroupByNumber(p.group).String(), len(extracted)+1)
			if err != nil {
				return nil, "", err
			}
			if ok {
				extracted = append(extracted, call)
			}
			consumed = append(consumed, span{m.RuneIndex, m.RuneIndex + m.RuneLength})
		}
		if len(extracted) > 0 {
			break
		}
	}
	if len(consumed) == 0 {
		return extracted, pystr.Strip(text), nil
	}
	sort.Slice(consumed, func(i, j int) bool {
		if consumed[i].start != consumed[j].start {
			return consumed[i].start < consumed[j].start
		}
		return consumed[i].end < consumed[j].end
	})
	var merged []span
	for _, s := range consumed {
		if len(merged) == 0 || s.start > merged[len(merged)-1].end {
			merged = append(merged, s)
		} else if s.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = s.end
		}
	}
	var parts []string
	cursor := 0
	for _, s := range merged {
		if cursor < s.start {
			parts = append(parts, string(runes[cursor:s.start]))
		}
		cursor = max(cursor, s.end)
	}
	if cursor < len(runes) {
		parts = append(parts, string(runes[cursor:]))
	}
	kept := []string{}
	for _, p := range parts {
		if stripped := pystr.Strip(p); stripped != "" {
			kept = append(kept, stripped)
		}
	}
	return extracted, pystr.Strip(strings.Join(kept, "\n")), nil
}

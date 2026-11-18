package provider

import (
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Key order the agent builds a chat completions message dict in, from role to the tool call id
var chatMessageKeyOrder = []string{"role", "name", "content", "reasoning", "reasoning_content", "reasoning_details", "tool_calls", "tool_call_id"}

// Key order the agent builds a tool call dict and its function dict in
var (
	chatToolCallKeyOrder         = []string{"id", "call_id", "response_item_id", "type", "function", "extra_content"}
	chatToolCallFunctionKeyOrder = []string{"name", "arguments", "thought_signature"}
)

// Key order of a content part, its type first and the cache marker last
var chatContentPartKeyOrder = []string{"type", "text", "image_url", "input_audio", "file", "video_url", "cache_control"}

// Key order of an image_url part body and of a cache marker
var (
	chatImageURLKeyOrder     = []string{"url", "detail"}
	chatCacheControlKeyOrder = []string{"type", "ttl"}
)

// Key order of a function tool definition
var chatToolKeyOrder = []string{"type", "function", "cache_control"}

// Ordered dict of a plain map, listed keys first and the rest in sorted order, an ordered dict kept as built
func orderedByBuild(v any, build []string) (*pyjson.Object, bool) {
	switch t := v.(type) {
	case *pyjson.Object:
		return t, t != nil
	case map[string]any:
		o := pyjson.NewObject()
		for _, k := range build {
			if val, ok := t[k]; ok {
				o.Set(k, val)
			}
		}
		if o.Len() < len(t) {
			rest := make([]string, 0, len(t)-o.Len())
			for k := range t {
				if _, placed := o.Vals[k]; !placed {
					rest = append(rest, k)
				}
			}
			sort.Strings(rest)
			for _, k := range rest {
				o.Set(k, t[k])
			}
		}
		return o, true
	}
	return nil, false
}

// Ordered dict of a message map whose builder recorded its key order, the carrier key left out
func orderedByRecorded(v any) (*pyjson.Object, bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, false
	}
	keys, ok := recordedWireKeys(m[state.WireKeyOrder])
	if !ok {
		return nil, false
	}
	rest := make(map[string]any, len(m))
	for k, val := range m {
		if k != state.WireKeyOrder {
			rest[k] = val
		}
	}
	o := pyjson.NewObject()
	for _, k := range keys {
		if val, present := rest[k]; present {
			o.Set(k, val)
			delete(rest, k)
		}
	}
	if tail, ok := orderedByBuild(rest, chatMessageKeyOrder); ok {
		for _, k := range tail.Keys {
			o.Set(k, tail.Vals[k])
		}
	}
	return o, true
}

// Key list of a recorded wire order, a JSON round trip through a plugin leaving it a list of strings
func recordedWireKeys(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, len(t))
		for i, k := range t {
			s, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// List of request messages with every message that carries a recorded key order turned into its ordered dict
func OrderRecordedMessages(v any) any {
	items, ok := wireListItems(v)
	if !ok {
		return v
	}
	out := make([]any, len(items))
	changed := false
	for i, item := range items {
		out[i] = item
		if o, ok := orderedByRecorded(item); ok {
			out[i], changed = o, true
		}
	}
	if !changed {
		return v
	}
	return out
}

// Message keys a chat completions body keeps, in the given order
func chatWireSurvivors(msg map[string]any, keys []string) []string {
	var out []string
	for _, k := range keys {
		if k == state.WireKeyOrder || ccStripMsgKeys[k] || strings.HasPrefix(k, "_") || k == "name" && msg["role"] == "tool" {
			continue
		}
		out = append(out, k)
	}
	return out
}

// Whether a builder key order puts the keys a chat completions body keeps in another order than the encoder default
func ChatWireNeedsKeyOrder(msg map[string]any, keys []string) bool {
	plain := make(map[string]any, len(msg))
	for k, val := range msg {
		if k != state.WireKeyOrder {
			plain[k] = val
		}
	}
	def, _ := orderedByBuild(plain, chatMessageKeyOrder)
	return !slices.Equal(chatWireSurvivors(msg, keys), chatWireSurvivors(msg, def.Keys))
}

// List items of a request list value, nil for anything that is not a list
func wireListItems(v any) ([]any, bool) {
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

// Copy of a dict with one nested value replaced, the input when the value is unchanged
func withNested(o *pyjson.Object, key string, value any) *pyjson.Object {
	if cur, ok := o.Vals[key]; ok && sameWireValue(cur, value) {
		return o
	}
	out := o.Copy()
	out.Set(key, value)
	return out
}

// Whether two wire values are the same container or scalar
func sameWireValue(a, b any) bool {
	switch x := a.(type) {
	case *pyjson.Object:
		y, ok := b.(*pyjson.Object)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		return ok && len(x) == len(y) && (len(x) == 0 || &x[0] == &y[0])
	case map[string]any, []map[string]any:
		return false
	}
	return true
}

// Chat completions messages as ordered dicts in the order the agent builds each message, tool call and content part
func chatWireMessages(v any) any {
	items, ok := wireListItems(v)
	if !ok {
		return v
	}
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = chatWireMessage(item)
	}
	return out
}

func chatWireMessage(v any) any {
	msg, ok := orderedByRecorded(v)
	if !ok {
		msg, ok = orderedByBuild(v, chatMessageKeyOrder)
	}
	if !ok {
		return v
	}
	if calls, ok := wireListItems(msg.Vals["tool_calls"]); ok {
		ordered := make([]any, len(calls))
		for i, call := range calls {
			ordered[i] = chatWireToolCall(call)
		}
		msg = withNested(msg, "tool_calls", ordered)
	}
	if parts, ok := wireListItems(msg.Vals["content"]); ok {
		ordered := make([]any, len(parts))
		for i, part := range parts {
			ordered[i] = chatWireContentPart(part)
		}
		msg = withNested(msg, "content", ordered)
	}
	if cc, ok := orderedByBuild(msg.Vals["cache_control"], chatCacheControlKeyOrder); ok {
		msg = withNested(msg, "cache_control", cc)
	}
	return msg
}

func chatWireToolCall(v any) any {
	call, ok := orderedByBuild(v, chatToolCallKeyOrder)
	if !ok {
		return v
	}
	if fn, ok := orderedByBuild(call.Vals["function"], chatToolCallFunctionKeyOrder); ok {
		call = withNested(call, "function", fn)
	}
	return call
}

func chatWireContentPart(v any) any {
	part, ok := orderedByBuild(v, chatContentPartKeyOrder)
	if !ok {
		return v
	}
	if img, ok := orderedByBuild(part.Vals["image_url"], chatImageURLKeyOrder); ok {
		part = withNested(part, "image_url", img)
	}
	if cc, ok := orderedByBuild(part.Vals["cache_control"], chatCacheControlKeyOrder); ok {
		part = withNested(part, "cache_control", cc)
	}
	return part
}

// Function tool definitions as ordered dicts, the envelope in build order around the ordered function schema
func chatWireTools(v any) any {
	items, ok := wireListItems(v)
	if !ok {
		return v
	}
	out := make([]any, len(items))
	for i, item := range items {
		tool, ok := orderedByBuild(item, chatToolKeyOrder)
		if !ok {
			out[i] = item
			continue
		}
		if cc, ok := orderedByBuild(tool.Vals["cache_control"], chatCacheControlKeyOrder); ok {
			tool = withNested(tool, "cache_control", cc)
		}
		out[i] = tool
	}
	return out
}

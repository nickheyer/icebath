package provider

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Body key order of messages.create as the SDK builds its body dict
var anthCreateBodyKeys = []string{"max_tokens", "messages", "model", "cache_control", "container", "inference_geo", "metadata", "output_config",
	"service_tier", "stop_sequences", "stream", "system", "temperature", "thinking", "tool_choice", "tools", "top_k", "top_p"}

// Body key order of the messages.stream helper, the stream flag last
var anthStreamBodyKeys = []string{"max_tokens", "messages", "model", "cache_control", "inference_geo", "metadata", "output_config", "container",
	"service_tier", "stop_sequences", "system", "temperature", "top_k", "top_p", "tools", "thinking", "tool_choice", "stream"}

// Body key order of messages.count_tokens
var anthCountTokensBodyKeys = []string{"messages", "model", "cache_control", "output_config", "system", "thinking", "tool_choice", "tools"}

// Key order each Messages content block type is built in by the converter and the cache marker pass
var anthBlockKeyOrder = map[string][]string{
	"text":              {"type", "text", "citations", "cache_control"},
	"image":             {"type", "source", "cache_control"},
	"tool_use":          {"type", "id", "name", "input", "cache_control"},
	"tool_result":       {"type", "tool_use_id", "content", "cache_control"},
	"thinking":          {"type", "thinking", "signature"},
	"redacted_thinking": {"type", "data"},
}

// Key order of an image block source per source type
var anthImageSourceKeyOrder = map[string][]string{
	"base64": {"type", "media_type", "data"},
	"url":    {"type", "url"},
}

// Key order of a tool definition, schema after the description and the cache and buffering flags appended
var anthToolKeyOrder = []string{"name", "description", "input_schema", "cache_control", "eager_input_streaming"}

// Key order of the thinking, output_config, output format, tool_choice, metadata and message params
var (
	anthThinkingKeyOrder     = []string{"type", "budget_tokens", "display"}
	anthOutputConfigKeyOrder = []string{"effort", "format"}
	anthOutputFormatKeyOrder = []string{"type", "schema"}
	anthToolChoiceKeyOrder   = []string{"type", "name"}
	anthMetadataKeyOrder     = []string{"user_id"}
	anthMessageKeyOrder      = []string{"role", "content"}
)

// SDK params of a Messages body in the order the called method builds them, and the extra_body mapping merged after them
func anthWireParams(body map[string]any, op anthOperation) (*pyjson.Object, *pyjson.Object, error) {
	keys, method := anthCreateBodyKeys, "Messages.create()"
	stream, _ := body["stream"].(bool)
	switch {
	case op == anthOpCountTokens:
		keys, method = anthCountTokensBodyKeys, "Messages.count_tokens()"
	case stream:
		keys, method = anthStreamBodyKeys, "Messages.stream()"
	}
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	for k := range body {
		if k != "extra_body" && !known[k] {
			return nil, nil, &pyjson.TypeError{Msg: fmt.Sprintf("%s got an unexpected keyword argument '%s'", method, k)}
		}
	}
	out := pyjson.NewObject()
	for _, k := range keys {
		if v, has := body[k]; has {
			out.Set(k, anthWireParam(k, v))
		}
	}
	var extra *pyjson.Object
	if v, has := body["extra_body"]; has && v != nil {
		extra, _ = pyjson.AsObject(v)
	}
	return out, extra, nil
}

// Body dict the SDK posts, the params with the extra_body keys merged over them as _merge_mappings does
func anthMergedWireBody(params, extra *pyjson.Object) *pyjson.Object {
	if extra == nil || extra.Len() == 0 {
		return params
	}
	out := params.Copy()
	out.Update(extra)
	return out
}

// Value of one Messages param with every nested dict in the order the adapter builds it
func anthWireParam(key string, v any) any {
	switch key {
	case "messages":
		items, ok := wireListItems(v)
		if !ok {
			return v
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = anthWireMessage(item)
		}
		return out
	case "system":
		return anthWireBlocks(v)
	case "tools":
		items, ok := wireListItems(v)
		if !ok {
			return v
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = anthWireTool(item)
		}
		return out
	case "thinking":
		return anthOrdered(v, anthThinkingKeyOrder)
	case "output_config":
		cfg, ok := orderedByBuild(v, anthOutputConfigKeyOrder)
		if !ok {
			return v
		}
		if f, ok := orderedByBuild(cfg.Vals["format"], anthOutputFormatKeyOrder); ok {
			cfg = withNested(cfg, "format", f)
		}
		return cfg
	case "tool_choice":
		return anthOrdered(v, anthToolChoiceKeyOrder)
	case "metadata":
		return anthOrdered(v, anthMetadataKeyOrder)
	case "cache_control":
		return anthOrdered(v, chatCacheControlKeyOrder)
	}
	return v
}

func anthOrdered(v any, order []string) any {
	if o, ok := orderedByBuild(v, order); ok {
		return o
	}
	return v
}

func anthWireMessage(v any) any {
	msg, ok := orderedByBuild(v, anthMessageKeyOrder)
	if !ok {
		return v
	}
	if content, has := msg.Vals["content"]; has {
		msg = withNested(msg, "content", anthWireBlocks(content))
	}
	return msg
}

// Block list in converter key order, strings and other values unchanged
func anthWireBlocks(v any) any {
	items, ok := wireListItems(v)
	if !ok {
		return v
	}
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = anthWireBlock(item)
	}
	return out
}

func anthWireBlock(v any) any {
	typ := blockType(v)
	order, known := anthBlockKeyOrder[typ]
	if !known {
		order = []string{"type"}
	}
	b, ok := orderedByBuild(v, order)
	if !ok {
		return v
	}
	switch typ {
	case "image":
		src, _ := pyjson.Dict(b.Vals["source"])
		srcOrder, known := anthImageSourceKeyOrder[asStr(src["type"])]
		if !known {
			srcOrder = []string{"type"}
		}
		if s, ok := orderedByBuild(b.Vals["source"], srcOrder); ok {
			b = withNested(b, "source", s)
		}
	case "tool_result":
		if _, isList := wireListItems(b.Vals["content"]); isList {
			b = withNested(b, "content", anthWireBlocks(b.Vals["content"]))
		}
	}
	if cc, ok := orderedByBuild(b.Vals["cache_control"], chatCacheControlKeyOrder); ok {
		b = withNested(b, "cache_control", cc)
	}
	return b
}

func anthWireTool(v any) any {
	tool, ok := orderedByBuild(v, anthToolKeyOrder)
	if !ok {
		return v
	}
	if cc, ok := orderedByBuild(tool.Vals["cache_control"], chatCacheControlKeyOrder); ok {
		tool = withNested(tool, "cache_control", cc)
	}
	return tool
}

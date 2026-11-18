package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Converse body members in the order the adapter inserts them into its kwargs, modelId going to the path
var converseBodyKeys = []string{"messages", "inferenceConfig", "system", "toolConfig", "guardrailConfig", "additionalModelRequestFields"}

// Every member of the Converse and ConverseStream input shapes, the set botocore validates kwargs against
var converseInputMembers = []string{"modelId", "messages", "system", "inferenceConfig", "toolConfig", "guardrailConfig", "additionalModelRequestFields",
	"promptVariables", "additionalModelResponseFieldPaths", "requestMetadata", "performanceConfig", "serviceTier", "outputConfig"}

// Key order of the nested Converse structures the adapter builds
var (
	converseInferenceKeyOrder  = []string{"maxTokens", "temperature", "topP", "stopSequences"}
	converseGuardrailKeyOrder  = []string{"guardrailIdentifier", "guardrailVersion", "streamProcessingMode", "trace"}
	converseMessageKeyOrder    = []string{"role", "content"}
	converseToolUseKeyOrder    = []string{"toolUseId", "name", "input"}
	converseToolResultKeyOrder = []string{"toolUseId", "content"}
	converseImageKeyOrder      = []string{"format", "source"}
	converseToolSpecKeyOrder   = []string{"name", "description", "inputSchema"}
	converseReasoningTextOrder = []string{"text", "signature"}
	converseClaudeFieldsOrder  = []string{"thinking", "output_config", "anthropic_beta"}
	converseNovaReasoningOrder = []string{"type", "maxReasoningEffort"}
)

// Body bytes botocore's rest-json serializer writes for Converse kwargs, members in kwargs order, blobs base64, json.dumps defaults
func converseRequestBody(kwargs map[string]any) ([]byte, error) {
	known := map[string]bool{}
	for _, k := range converseInputMembers {
		known[k] = true
	}
	var unknown []string
	for k := range kwargs {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		lines := make([]string, 0, len(unknown))
		for _, k := range unknown {
			lines = append(lines, fmt.Sprintf("Unknown parameter in input: %q, must be one of: %s", k, strings.Join(converseInputMembers, ", ")))
		}
		return nil, fmt.Errorf("Parameter validation failed:\n%s", strings.Join(lines, "\n"))
	}
	body := pyjson.NewObject()
	for _, k := range converseBodyKeys {
		if v, ok := kwargs[k]; ok {
			body.Set(k, converseWireParam(k, v))
		}
	}
	text, err := pyjson.Dumps(body, pyjson.Default(converseBlobDefault))
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// Base64 text of a blob member as botocore's _get_base64 writes it, ordered dicts for Go marshalers
func converseBlobDefault(v any) (any, error) {
	switch b := v.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(b), nil
	case json.RawMessage:
		return pyjson.LoadsBytes(b)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, &pyjson.TypeError{Msg: fmt.Sprintf("Object of type %T is not JSON serializable", v)}
	}
	return pyjson.LoadsBytes(raw)
}

func converseOrdered(v any, order []string) any {
	if o, ok := orderedByBuild(v, order); ok {
		return o
	}
	return v
}

func converseWireParam(key string, v any) any {
	switch key {
	case "messages":
		items, ok := wireListItems(v)
		if !ok {
			return v
		}
		out := make([]any, len(items))
		for i, item := range items {
			msg, ok := orderedByBuild(item, converseMessageKeyOrder)
			if !ok {
				out[i] = item
				continue
			}
			if content, has := msg.Get("content"); has {
				msg = withNested(msg, "content", converseWireBlocks(content))
			}
			out[i] = msg
		}
		return out
	case "inferenceConfig":
		return converseOrdered(v, converseInferenceKeyOrder)
	case "system":
		return converseWireBlocks(v)
	case "toolConfig":
		cfg, ok := orderedByBuild(v, []string{"tools", "toolChoice"})
		if !ok {
			return v
		}
		if tools, has := cfg.Get("tools"); has {
			cfg = withNested(cfg, "tools", converseWireBlocks(tools))
		}
		return cfg
	case "guardrailConfig":
		return converseOrdered(v, converseGuardrailKeyOrder)
	case "additionalModelRequestFields":
		fields, ok := orderedByBuild(v, append(append([]string{}, converseClaudeFieldsOrder...), "reasoningConfig"))
		if !ok {
			return v
		}
		if th, ok := orderedByBuild(fields.Vals["thinking"], anthThinkingKeyOrder); ok {
			fields = withNested(fields, "thinking", th)
		}
		if oc, ok := orderedByBuild(fields.Vals["output_config"], anthOutputConfigKeyOrder); ok {
			fields = withNested(fields, "output_config", oc)
		}
		if rc, ok := orderedByBuild(fields.Vals["reasoningConfig"], converseNovaReasoningOrder); ok {
			fields = withNested(fields, "reasoningConfig", rc)
		}
		return fields
	}
	return v
}

// Content, system and tool block lists with every union member in build order
func converseWireBlocks(v any) any {
	items, ok := wireListItems(v)
	if !ok {
		return v
	}
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = converseWireBlock(item)
	}
	return out
}

func converseWireBlock(v any) any {
	block, ok := orderedByBuild(v, nil)
	if !ok {
		return v
	}
	if tu, ok := orderedByBuild(block.Vals["toolUse"], converseToolUseKeyOrder); ok {
		block = withNested(block, "toolUse", tu)
	}
	if tr, ok := orderedByBuild(block.Vals["toolResult"], converseToolResultKeyOrder); ok {
		if content, has := tr.Get("content"); has {
			tr = withNested(tr, "content", converseWireBlocks(content))
		}
		block = withNested(block, "toolResult", tr)
	}
	if img, ok := orderedByBuild(block.Vals["image"], converseImageKeyOrder); ok {
		block = withNested(block, "image", img)
	}
	if rc, ok := orderedByBuild(block.Vals["reasoningContent"], nil); ok {
		if rt, ok := orderedByBuild(rc.Vals["reasoningText"], converseReasoningTextOrder); ok {
			rc = withNested(rc, "reasoningText", rt)
		}
		block = withNested(block, "reasoningContent", rc)
	}
	if spec, ok := orderedByBuild(block.Vals["toolSpec"], converseToolSpecKeyOrder); ok {
		block = withNested(block, "toolSpec", spec)
	}
	if cp, ok := orderedByBuild(block.Vals["cachePoint"], []string{"type"}); ok {
		block = withNested(block, "cachePoint", cp)
	}
	return block
}

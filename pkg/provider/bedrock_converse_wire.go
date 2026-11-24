package provider

import (
	"fmt"
	"sort"
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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

// Compact JSON body of Converse kwargs, members in kwargs order and blobs base64
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
	body := jsonutil.NewObject()
	for _, k := range converseBodyKeys {
		if v, ok := kwargs[k]; ok {
			body.Set(k, converseWireParam(k, v))
		}
	}
	text, err := jsonutil.Encode(body)
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
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
		if th, ok := orderedByBuild(fields.Value("thinking"), anthThinkingKeyOrder); ok {
			fields = withNested(fields, "thinking", th)
		}
		if oc, ok := orderedByBuild(fields.Value("output_config"), anthOutputConfigKeyOrder); ok {
			fields = withNested(fields, "output_config", oc)
		}
		if rc, ok := orderedByBuild(fields.Value("reasoningConfig"), converseNovaReasoningOrder); ok {
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
	if tu, ok := orderedByBuild(block.Value("toolUse"), converseToolUseKeyOrder); ok {
		block = withNested(block, "toolUse", tu)
	}
	if tr, ok := orderedByBuild(block.Value("toolResult"), converseToolResultKeyOrder); ok {
		if content, has := tr.Get("content"); has {
			tr = withNested(tr, "content", converseWireBlocks(content))
		}
		block = withNested(block, "toolResult", tr)
	}
	if img, ok := orderedByBuild(block.Value("image"), converseImageKeyOrder); ok {
		block = withNested(block, "image", img)
	}
	if rc, ok := orderedByBuild(block.Value("reasoningContent"), nil); ok {
		if rt, ok := orderedByBuild(rc.Value("reasoningText"), converseReasoningTextOrder); ok {
			rc = withNested(rc, "reasoningText", rt)
		}
		block = withNested(block, "reasoningContent", rc)
	}
	if spec, ok := orderedByBuild(block.Value("toolSpec"), converseToolSpecKeyOrder); ok {
		block = withNested(block, "toolSpec", spec)
	}
	if cp, ok := orderedByBuild(block.Value("cachePoint"), []string{"type"}); ok {
		block = withNested(block, "cachePoint", cp)
	}
	return block
}

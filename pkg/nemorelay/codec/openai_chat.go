package codec

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// OpenAIChatCodec translates OpenAI Chat Completions requests and responses
type OpenAIChatCodec struct{}

// Returns the OpenAI Chat Completions codec
func NewOpenAIChatCodec() *OpenAIChatCodec {
	return &OpenAIChatCodec{}
}

var openAIChatProviderSurface = providerSurfaceDescriptor{
	surface: ProviderSurfaceOpenAIChat,
	detectRequest: func(obj map[string]any, _ *string) bool {
		_, ok := obj["messages"]
		return ok
	},
	detectResponse: func(obj map[string]any) bool {
		_, ok := obj["choices"].([]any)
		return ok
	},
	decodeRequest: func(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
		return NewOpenAIChatCodec().Decode(request)
	},
	decodeResponse: func(response any) (*types.AnnotatedLLMResponse, error) {
		return NewOpenAIChatCodec().DecodeResponse(response)
	},
	codecName:      "openai_chat",
	requestCodec:   func() types.LLMCodec { return NewOpenAIChatCodec() },
	responseCodec:  func() types.LLMResponseCodec { return NewOpenAIChatCodec() },
	streamingCodec: func() StreamingCodec { return NewOpenAIChatStreamingCodec() },
}

var chatModeledRequestKeys = []string{
	"messages", "model", "temperature", "max_tokens", "max_completion_tokens", "top_p", "stop",
	"tools", "tool_choice", "store", "user", "metadata", "service_tier", "parallel_tool_calls",
	"top_logprobs", "stream", "audio", "frequency_penalty", "function_call", "functions",
	"logit_bias", "logprobs", "modalities", "moderation", "n", "prediction", "presence_penalty",
	"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "reasoning_effort",
	"response_format", "safety_identifier", "seed", "stream_options", "verbosity", "web_search_options",
}

var chatAPISpecificKeys = []string{
	"audio", "frequency_penalty", "function_call", "functions", "logit_bias", "logprobs",
	"modalities", "moderation", "n", "prediction", "presence_penalty", "prompt_cache_key",
	"prompt_cache_options", "prompt_cache_retention", "reasoning_effort", "response_format",
	"safety_identifier", "seed", "stream_options", "verbosity", "web_search_options",
}

// Identifies the builtin OpenAI Chat codec
func (*OpenAIChatCodec) CodecIdentity() types.LLMCodecIdentity {
	return types.LLMCodecIdentity{Kind: types.LLMCodecIdentityBuiltIn, Builtin: types.BuiltinLLMCodecOpenAIChat}
}

// Maps an OpenAI Chat finish reason to the normalized finish reason
func mapChatFinishReason(reason string) types.FinishReason {
	switch reason {
	case "stop":
		return types.FinishReason{Kind: types.FinishReasonComplete}
	case "length":
		return types.FinishReason{Kind: types.FinishReasonLength}
	case "tool_calls", "function_call":
		return types.FinishReason{Kind: types.FinishReasonToolUse}
	case "content_filter":
		return types.FinishReason{Kind: types.FinishReasonContentFilter}
	}
	return types.FinishReason{Kind: types.FinishReasonUnknown, Unknown: reason}
}

// Wraps a raw OpenAI Chat component as a provider native component
func chatNative(kind string, value any) types.ProviderNativeComponent {
	return types.ProviderNativeComponent{Provider: "openai_chat", Kind: kind, Value: cloneValue(value)}
}

// Decodes OpenAI Chat message content from a string or part array
func decodeChatContent(value any) (types.MessageContent, error) {
	if text, ok := value.(string); ok {
		return types.MessageContentText(text), nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat message content must be a string or array")
	}
	out := make(types.MessageContentParts, 0, len(parts))
	for _, part := range parts {
		decoded, err := decodeChatContentPart(part)
		if err != nil {
			return nil, err
		}
		out = append(out, decoded)
	}
	return out, nil
}

// Decodes an OpenAI image URL object with Rust deserialization errors
func decodeOpenAIImageURL(value any) (types.OpenAIImageURL, error) {
	var out types.OpenAIImageURL
	err := serdeStruct(value, "OpenAiImageUrl", []serdeField{
		{key: "url", set: func(v any) error {
			s, err := serdeString(v)
			out.URL = s
			return err
		}},
		{key: "detail", optional: true, set: func(v any) error {
			s, err := serdeString(v)
			out.Detail = &s
			return err
		}},
	}, false, nil)
	return out, err
}

// Decodes one OpenAI Chat content part, keeping unknown part types native
func decodeChatContentPart(value any) (types.ContentPart, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat content part must be an object")
	}
	kind, ok := obj["type"].(string)
	if !ok {
		kind = "unknown"
	}
	switch kind {
	case "text":
		text, ok := obj["text"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat text part is missing text")
		}
		return types.ContentPartText{Text: text, Extra: objectWithout(obj, "type", "text")}, nil
	case "image_url":
		raw, ok := obj["image_url"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat image part is missing image_url")
		}
		imageURL, err := decodeOpenAIImageURL(raw)
		if err != nil {
			return nil, types.InvalidArgumentf("invalid OpenAI Chat image_url: %v", err)
		}
		return types.ContentPartImageURL{ImageURL: imageURL, Extra: objectWithout(obj, "type", "image_url")}, nil
	case "input_audio":
		audio, ok := obj["input_audio"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat audio part is missing input_audio")
		}
		return types.ContentPartAudio{Audio: cloneValue(audio), Extra: objectWithout(obj, "type", "input_audio")}, nil
	case "file":
		file, ok := obj["file"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat file part is missing file")
		}
		return types.ContentPartFile{File: cloneValue(file), Extra: objectWithout(obj, "type", "file")}, nil
	case "refusal":
		refusal, ok := obj["refusal"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat refusal part is missing refusal")
		}
		return types.ContentPartRefusal{Refusal: refusal, Extra: objectWithout(obj, "type", "refusal")}, nil
	}
	return types.ContentPartProviderNative{Provider: "openai_chat", Kind: kind, Value: cloneValue(value)}, nil
}

// Reads an optional string field of an OpenAI Chat message
func optionalChatString(obj map[string]any, key, context string) (*string, error) {
	switch value := obj[key].(type) {
	case nil:
		return nil, nil
	case string:
		return &value, nil
	}
	return nil, types.InvalidArgumentf("OpenAI Chat %s %s must be a string or null", context, key)
}

// Decodes a plain function tool call, returning nil when the call carries provider extensions
func decodeChatToolCall(value any) (*types.ToolCall, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat tool call must be an object")
	}
	if kind, _ := obj["type"].(string); kind != "function" {
		return nil, nil
	}
	if !onlyKeys(obj, "id", "type", "function") {
		return nil, nil
	}
	function, ok := obj["function"].(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat function tool call is missing function")
	}
	if !onlyKeys(function, "name", "arguments") {
		return nil, nil
	}
	id, ok := obj["id"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat function tool call is missing id")
	}
	name, ok := function["name"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat function tool call is missing name")
	}
	arguments, ok := function["arguments"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat function tool call is missing arguments")
	}
	return &types.ToolCall{ID: id, CallType: "function", Function: types.FunctionCall{Name: name, Arguments: arguments}}, nil
}

// Decodes one OpenAI Chat message, keeping messages with extension fields native
func decodeChatMessage(value any) (types.Message, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat message must be an object")
	}
	role, ok := obj["role"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat message is missing role")
	}
	native := func() types.Message {
		return types.MessageProviderNative{Provider: "openai_chat", Kind: role, Value: cloneValue(value)}
	}
	switch role {
	case "system", "developer", "user":
		if !onlyKeys(obj, "role", "content", "name") {
			return native(), nil
		}
		raw, ok := obj["content"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat message is missing content")
		}
		content, err := decodeChatContent(raw)
		if err != nil {
			return nil, err
		}
		name, err := optionalChatString(obj, "name", "message")
		if err != nil {
			return nil, err
		}
		switch role {
		case "system":
			return types.MessageSystem{Content: content, Name: name}, nil
		case "developer":
			return types.MessageDeveloper{Content: content, Name: name}, nil
		}
		return types.MessageUser{Content: content, Name: name}, nil
	case "assistant":
		if !onlyKeys(obj, "role", "content", "tool_calls", "name") {
			return native(), nil
		}
		var content types.MessageContent
		if raw := obj["content"]; raw != nil {
			decoded, err := decodeChatContent(raw)
			if err != nil {
				return nil, err
			}
			content = decoded
		}
		var toolCalls []types.ToolCall
		switch calls := obj["tool_calls"].(type) {
		case nil:
		case []any:
			decoded := make([]*types.ToolCall, 0, len(calls))
			for _, call := range calls {
				toolCall, err := decodeChatToolCall(call)
				if err != nil {
					return nil, err
				}
				decoded = append(decoded, toolCall)
			}
			toolCalls = make([]types.ToolCall, 0, len(decoded))
			for _, toolCall := range decoded {
				if toolCall == nil {
					return native(), nil
				}
				toolCalls = append(toolCalls, *toolCall)
			}
		default:
			return nil, types.InvalidArgumentf("OpenAI Chat assistant tool_calls must be an array or null")
		}
		name, err := optionalChatString(obj, "name", "assistant message")
		if err != nil {
			return nil, err
		}
		return types.MessageAssistant{Content: content, ToolCalls: toolCalls, Name: name}, nil
	case "tool":
		if !onlyKeys(obj, "role", "content", "tool_call_id") {
			return native(), nil
		}
		raw, ok := obj["content"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat tool message is missing content")
		}
		content, err := decodeChatContent(raw)
		if err != nil {
			return nil, err
		}
		toolCallID, ok := obj["tool_call_id"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat tool message is missing tool_call_id")
		}
		return types.MessageTool{Content: content, ToolCallID: toolCallID}, nil
	case "function":
		if !onlyKeys(obj, "role", "content", "name") {
			return native(), nil
		}
		content, err := optionalChatString(obj, "content", "function message")
		if err != nil {
			return nil, err
		}
		name, ok := obj["name"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat function message is missing name")
		}
		return types.MessageFunction{Content: content, Name: name}, nil
	}
	return native(), nil
}

// Encodes message content into OpenAI Chat content
func encodeChatContent(content types.MessageContent) (any, error) {
	switch c := content.(type) {
	case types.MessageContentText:
		return string(c), nil
	case types.MessageContentParts:
		out := make([]any, 0, len(c))
		for _, part := range c {
			encoded, err := encodeChatContentPart(part)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		}
		return out, nil
	}
	return nil, types.InvalidArgumentf("OpenAI Chat message content must be a string or array")
}

// Encodes one content part into an OpenAI Chat content part
func encodeChatContentPart(part types.ContentPart) (any, error) {
	switch p := part.(type) {
	case types.ContentPartText:
		obj := cloneObject(p.Extra)
		obj["type"] = "text"
		obj["text"] = p.Text
		return obj, nil
	case types.ContentPartImageURL:
		obj := cloneObject(p.Extra)
		obj["type"] = "image_url"
		imageURL, err := types.ToJSONValue(p.ImageURL)
		if err != nil {
			return nil, types.Internalf("OpenAI Chat image URL encode: %v", err)
		}
		obj["image_url"] = imageURL
		return obj, nil
	case types.ContentPartAudio:
		obj := cloneObject(p.Extra)
		obj["type"] = "input_audio"
		obj["input_audio"] = cloneValue(p.Audio)
		return obj, nil
	case types.ContentPartFile:
		obj := cloneObject(p.Extra)
		obj["type"] = "file"
		obj["file"] = cloneValue(p.File)
		return obj, nil
	case types.ContentPartRefusal:
		obj := cloneObject(p.Extra)
		obj["type"] = "refusal"
		obj["refusal"] = p.Refusal
		return obj, nil
	case types.ContentPartProviderNative:
		if p.Provider == "openai_chat" {
			return cloneValue(p.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("content part %s cannot be encoded for OpenAI Chat", types.RustDebug(part))
}

// Builds an OpenAI Chat message object with role, content and optional name
func chatMessageWithContent(role string, content types.MessageContent, name *string) (any, error) {
	encoded, err := encodeChatContent(content)
	if err != nil {
		return nil, err
	}
	obj := map[string]any{"role": role, "content": encoded}
	if name != nil {
		obj["name"] = *name
	}
	return obj, nil
}

// Encodes one normalized message into an OpenAI Chat message
func encodeChatMessage(message types.Message) (any, error) {
	switch m := message.(type) {
	case types.MessageSystem:
		return chatMessageWithContent("system", m.Content, m.Name)
	case types.MessageDeveloper:
		return chatMessageWithContent("developer", m.Content, m.Name)
	case types.MessageUser:
		return chatMessageWithContent("user", m.Content, m.Name)
	case types.MessageAssistant:
		obj := map[string]any{"role": "assistant"}
		if m.Content != nil {
			encoded, err := encodeChatContent(m.Content)
			if err != nil {
				return nil, err
			}
			obj["content"] = encoded
		}
		if m.ToolCalls != nil {
			toolCalls, err := types.ToJSONValue(m.ToolCalls)
			if err != nil {
				return nil, types.Internalf("OpenAI Chat tool calls encode: %v", err)
			}
			obj["tool_calls"] = toolCalls
		}
		if m.Name != nil {
			obj["name"] = *m.Name
		}
		return obj, nil
	case types.MessageTool:
		encoded, err := encodeChatContent(m.Content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"role": "tool", "content": encoded, "tool_call_id": m.ToolCallID}, nil
	case types.MessageFunction:
		var content any
		if m.Content != nil {
			content = *m.Content
		}
		return map[string]any{"role": "function", "content": content, "name": m.Name}, nil
	case types.MessageProviderNative:
		if m.Provider == "openai_chat" {
			return cloneValue(m.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("message %s cannot be encoded for OpenAI Chat", types.RustDebug(message))
}

// Decodes one OpenAI Chat tool, keeping non-function tools native
func decodeChatTool(value any) (types.ToolDefinition, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat tool must be an object")
	}
	if kind, isString := obj["type"].(string); kind != "function" {
		if !isString {
			kind = "unknown"
		}
		native := chatNative(kind, value)
		return types.ToolDefinitionProviderNative{Provider: native.Provider, Kind: native.Kind, Value: native.Value}, nil
	}
	function, ok := obj["function"].(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat function tool is missing function")
	}
	name, ok := function["name"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat function tool is missing name")
	}
	description, err := optionalString(function, "description", "OpenAI Chat function tool")
	if err != nil {
		return nil, err
	}
	strict, err := optionalBool(function, "strict", "OpenAI Chat function tool")
	if err != nil {
		return nil, err
	}
	parameters, parametersPresent := function["parameters"]
	return types.ToolDefinitionFunction{
		Function: types.FunctionDefinition{
			Name:              name,
			Description:       description,
			Parameters:        cloneValue(parameters),
			ParametersPresent: parametersPresent,
			Strict:            strict,
			Extra:             objectWithout(function, "name", "description", "parameters", "strict"),
		},
		Extra: objectWithout(obj, "type", "function"),
	}, nil
}

// Encodes one normalized tool into an OpenAI Chat tool
func encodeChatTool(tool types.ToolDefinition) (any, error) {
	switch t := tool.(type) {
	case types.ToolDefinitionFunction:
		function := cloneObject(t.Function.Extra)
		function["name"] = t.Function.Name
		if t.Function.Description != nil {
			function["description"] = *t.Function.Description
		}
		if t.Function.ParametersPresent || t.Function.Parameters != nil {
			function["parameters"] = cloneValue(t.Function.Parameters)
		}
		if t.Function.Strict != nil {
			function["strict"] = *t.Function.Strict
		}
		obj := cloneObject(t.Extra)
		obj["type"] = "function"
		obj["function"] = function
		return obj, nil
	case types.ToolDefinitionProviderNative:
		if t.Provider == "openai_chat" {
			return cloneValue(t.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("tool %s cannot be encoded for OpenAI Chat", types.RustDebug(tool))
}

// Decodes an OpenAI Chat tool choice, keeping unrecognized shapes native
func decodeChatToolChoice(value any) types.ToolChoice {
	switch value {
	case "auto":
		return types.ToolChoiceAuto
	case "none":
		return types.ToolChoiceNone
	case "required":
		return types.ToolChoiceRequired
	}
	if obj, ok := value.(map[string]any); ok {
		if kind, _ := obj["type"].(string); kind == "function" {
			if function, ok := obj["function"].(map[string]any); ok {
				if name, ok := function["name"].(string); ok {
					return types.ToolChoiceSpecific(types.ToolChoiceFunction{
						ChoiceType: "function",
						Function:   types.ToolChoiceFunctionName{Name: name},
					})
				}
			}
		}
	}
	return types.ToolChoiceProviderNative(chatNative("tool_choice", value))
}

// Encodes a normalized tool choice into an OpenAI Chat tool choice
func encodeChatToolChoice(choice types.ToolChoice) (any, error) {
	switch c := choice.(type) {
	case types.ToolChoiceMode:
		switch c {
		case types.ToolChoiceAuto:
			return "auto", nil
		case types.ToolChoiceNone:
			return "none", nil
		case types.ToolChoiceRequired:
			return "required", nil
		}
	case types.ToolChoiceSpecific:
		return map[string]any{"type": "function", "function": map[string]any{"name": c.Function.Name}}, nil
	case types.ToolChoiceProviderNative:
		if c.Provider == "openai_chat" {
			return cloneValue(c.Value), nil
		}
		return nil, types.InvalidArgumentf("tool choice for %s cannot be encoded for OpenAI Chat", c.Provider)
	}
	return nil, types.InvalidArgumentf("tool choice %s cannot be encoded for OpenAI Chat", types.RustDebug(choice))
}

// Writes edited messages and rejects edits to fields OpenAI Chat cannot carry
func patchChatMessagesAndValidate(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqSlice(annotated.Messages, baseline.Messages) {
		original, hasOriginal := obj["messages"].([]any)
		messages, err := encodeChangedItems(annotated.Messages, baseline.Messages, original, hasOriginal, encodeChatMessage)
		if err != nil {
			return err
		}
		obj["messages"] = messages
	}
	unsupported := !types.Equal(annotated.Instructions, baseline.Instructions) ||
		!eqStrPtr(annotated.PreviousResponseID, baseline.PreviousResponseID) ||
		!eqOptJSON(annotated.Truncation, annotated.TruncationPresent, baseline.Truncation, baseline.TruncationPresent) ||
		!eqOptJSON(annotated.Reasoning, annotated.ReasoningPresent, baseline.Reasoning, baseline.ReasoningPresent) ||
		!eqOptJSON(annotated.Include, annotated.IncludePresent, baseline.Include, baseline.IncludePresent) ||
		!eqU64Ptr(annotated.MaxOutputTokens, baseline.MaxOutputTokens) ||
		!eqU64Ptr(annotated.MaxToolCalls, baseline.MaxToolCalls)
	if unsupported {
		return types.InvalidArgumentf("request contains fields that cannot be encoded for OpenAI Chat")
	}
	return nil
}

// Writes edited model and generation parameters
func patchChatModelAndParams(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) {
	if !eqStrPtr(annotated.Model, baseline.Model) {
		value, present := optStringJSON(annotated.Model)
		setOrRemoveJSON(obj, "model", value, present)
	}
	if eqParams(annotated.Params, baseline.Params) {
		return
	}
	patchParamFloats(obj, annotated.Params, baseline.Params)
	patchChatMaxTokens(obj, annotated.Params, baseline.Params)
	patchChatStopSequences(obj, annotated.Params, baseline.Params)
}

// Writes an edited token limit under the key the original request used
func patchChatMaxTokens(obj map[string]any, edited, before *types.GenerationParams) {
	maxTokens := paramMaxTokens(edited)
	if eqU64Ptr(maxTokens, paramMaxTokens(before)) {
		return
	}
	if maxTokens == nil {
		delete(obj, "max_completion_tokens")
		delete(obj, "max_tokens")
		return
	}
	key := "max_tokens"
	_, hasCompletion := obj["max_completion_tokens"]
	_, hasMax := obj["max_tokens"]
	if hasCompletion || !hasMax {
		key = "max_completion_tokens"
	}
	obj[key] = *maxTokens
}

// Writes edited stop sequences, keeping a single string when the original used one
func patchChatStopSequences(obj map[string]any, edited, before *types.GenerationParams) {
	stop := paramStop(edited)
	if eqStrings(stop, paramStop(before)) {
		return
	}
	if stop == nil {
		delete(obj, "stop")
		return
	}
	if _, isString := obj["stop"].(string); isString && len(stop) == 1 {
		obj["stop"] = stop[0]
		return
	}
	value, _ := optStringsJSON(stop)
	obj["stop"] = value
}

// Writes edited tools and tool choice
func patchChatTools(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqOptSlice(annotated.Tools, baseline.Tools) {
		if annotated.Tools == nil {
			delete(obj, "tools")
		} else {
			original, hasOriginal := obj["tools"].([]any)
			tools, err := encodeChangedItems(annotated.Tools, baseline.Tools, original, hasOriginal, encodeChatTool)
			if err != nil {
				return err
			}
			obj["tools"] = tools
		}
	}
	if !types.Equal(annotated.ToolChoice, baseline.ToolChoice) {
		if annotated.ToolChoice == nil {
			delete(obj, "tool_choice")
			return nil
		}
		edited, err := encodeChatToolChoice(annotated.ToolChoice)
		if err != nil {
			return err
		}
		if baseline.ToolChoice != nil {
			before, err := encodeChatToolChoice(baseline.ToolChoice)
			if err != nil {
				return err
			}
			if original, ok := obj["tool_choice"]; ok {
				edited, err = patchChangedJSON(original, before, edited)
				if err != nil {
					return err
				}
			}
		}
		obj["tool_choice"] = edited
	}
	return nil
}

// Writes edited metadata, flags and identity fields shared by chat requests
func patchChatCommonFields(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) {
	if !eqOptJSON(annotated.Metadata, annotated.MetadataPresent, baseline.Metadata, baseline.MetadataPresent) {
		setOrRemoveJSON(obj, "metadata", cloneValue(annotated.Metadata), annotated.MetadataPresent || annotated.Metadata != nil)
	}
	for _, field := range []struct {
		key          string
		edited, prev *bool
	}{
		{"store", annotated.Store, baseline.Store},
		{"parallel_tool_calls", annotated.ParallelToolCalls, baseline.ParallelToolCalls},
		{"stream", annotated.Stream, baseline.Stream},
	} {
		if !eqBoolPtr(field.edited, field.prev) {
			value, present := optBoolJSON(field.edited)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
	for _, field := range []struct {
		key          string
		edited, prev *string
	}{
		{"user", annotated.User, baseline.User},
		{"service_tier", annotated.ServiceTier, baseline.ServiceTier},
	} {
		if !eqStrPtr(field.edited, field.prev) {
			value, present := optStringJSON(field.edited)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
	if !eqU64Ptr(annotated.TopLogprobs, baseline.TopLogprobs) {
		value, present := optU64JSON(annotated.TopLogprobs)
		setOrRemoveJSON(obj, "top_logprobs", value, present)
	}
}

// Writes edited OpenAI Chat specific controls or removes them when the section was dropped
func patchChatAPISpecific(obj map[string]any, edited, baseline types.APISpecificRequest) error {
	editedChat, editedIsChat := edited.(types.APISpecificRequestOpenAIChat)
	baselineChat, baselineIsChat := baseline.(types.APISpecificRequestOpenAIChat)
	switch {
	case editedIsChat && baselineIsChat:
		patchChatAPIFields(obj, editedChat, baselineChat)
		return nil
	case edited == nil && baselineIsChat:
		for _, key := range chatAPISpecificKeys {
			delete(obj, key)
		}
		return nil
	case edited != nil || baseline != nil:
		return types.InvalidArgumentf("api_specific provider does not match OpenAI Chat")
	}
	return nil
}

// Writes each OpenAI Chat specific control that changed
func patchChatAPIFields(obj map[string]any, edited, baseline types.APISpecificRequestOpenAIChat) {
	for _, field := range []struct {
		key                  string
		value                any
		valuePresent         bool
		old                  any
		oldPresent           bool
	}{
		{"audio", edited.Audio, edited.AudioPresent, baseline.Audio, baseline.AudioPresent},
		{"function_call", edited.FunctionCall, edited.FunctionCallPresent, baseline.FunctionCall, baseline.FunctionCallPresent},
		{"logit_bias", edited.LogitBias, edited.LogitBiasPresent, baseline.LogitBias, baseline.LogitBiasPresent},
		{"moderation", edited.Moderation, edited.ModerationPresent, baseline.Moderation, baseline.ModerationPresent},
		{"prediction", edited.Prediction, edited.PredictionPresent, baseline.Prediction, baseline.PredictionPresent},
		{"prompt_cache_options", edited.PromptCacheOptions, edited.PromptCacheOptionsPresent, baseline.PromptCacheOptions, baseline.PromptCacheOptionsPresent},
		{"response_format", edited.ResponseFormat, edited.ResponseFormatPresent, baseline.ResponseFormat, baseline.ResponseFormatPresent},
		{"stream_options", edited.StreamOptions, edited.StreamOptionsPresent, baseline.StreamOptions, baseline.StreamOptionsPresent},
		{"web_search_options", edited.WebSearchOptions, edited.WebSearchOptionsPresent, baseline.WebSearchOptions, baseline.WebSearchOptionsPresent},
	} {
		if !eqOptJSON(field.value, field.valuePresent, field.old, field.oldPresent) {
			setOrRemoveJSON(obj, field.key, cloneValue(field.value), field.valuePresent || field.value != nil)
		}
	}
	for _, field := range []struct {
		key        string
		value, old *float64
	}{
		{"frequency_penalty", edited.FrequencyPenalty, baseline.FrequencyPenalty},
		{"presence_penalty", edited.PresencePenalty, baseline.PresencePenalty},
	} {
		if !eqF64Ptr(field.value, field.old) {
			value, present := optF64JSON(field.value)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
	for _, field := range []struct {
		key        string
		value, old *string
	}{
		{"prompt_cache_key", edited.PromptCacheKey, baseline.PromptCacheKey},
		{"prompt_cache_retention", edited.PromptCacheRetention, baseline.PromptCacheRetention},
		{"reasoning_effort", edited.ReasoningEffort, baseline.ReasoningEffort},
		{"safety_identifier", edited.SafetyIdentifier, baseline.SafetyIdentifier},
		{"verbosity", edited.Verbosity, baseline.Verbosity},
	} {
		if !eqStrPtr(field.value, field.old) {
			value, present := optStringJSON(field.value)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
	if !eqOptJSONSlice(edited.Functions, baseline.Functions) {
		setOrRemoveJSON(obj, "functions", cloneSlice(edited.Functions), edited.Functions != nil)
	}
	if !eqStrings(edited.Modalities, baseline.Modalities) {
		value, present := optStringsJSON(edited.Modalities)
		setOrRemoveJSON(obj, "modalities", value, present)
	}
	if !eqBoolPtr(edited.Logprobs, baseline.Logprobs) {
		value, present := optBoolJSON(edited.Logprobs)
		setOrRemoveJSON(obj, "logprobs", value, present)
	}
	if !eqU64Ptr(edited.N, baseline.N) {
		value, present := optU64JSON(edited.N)
		setOrRemoveJSON(obj, "n", value, present)
	}
	if !eqI64Ptr(edited.Seed, baseline.Seed) {
		if edited.Seed == nil {
			delete(obj, "seed")
		} else {
			obj["seed"] = *edited.Seed
		}
	}
}

// Raw OpenAI Chat completion fields read from a response
type rawChatCompletion struct {
	id                *string
	model             *string
	choices           []rawChatChoice
	usage             *rawChatUsage
	systemFingerprint *string
	serviceTier       *string
	extra             map[string]any
}

// Raw OpenAI Chat choice fields
type rawChatChoice struct {
	message      *rawChatMessage
	finishReason *string
	logprobs     any
}

// Raw OpenAI Chat message fields
type rawChatMessage struct {
	content      *string
	toolCalls    []rawChatToolCall
	hasToolCalls bool
}

// Raw OpenAI Chat tool call fields
type rawChatToolCall struct {
	id       *string
	function *rawChatFunction
}

// Raw OpenAI Chat function call fields
type rawChatFunction struct {
	name      *string
	arguments *string
}

// Raw OpenAI Chat usage fields
type rawChatUsage struct {
	promptTokens        *uint64
	completionTokens    *uint64
	totalTokens         *uint64
	cachedTokens        *uint64
	providerCost        *float64
	scalarCost          *float64
	detailedCost        *rawUsageCost
}

// Decodes an OpenAI Chat completion with Rust deserialization semantics
func decodeRawChatCompletion(value any) (rawChatCompletion, error) {
	raw := rawChatCompletion{extra: map[string]any{}}
	err := serdeStruct(value, "RawChatCompletion", []serdeField{
		{key: "id", optional: true, set: func(v any) (err error) { raw.id, err = serdeOptString(v); return }},
		{key: "model", optional: true, set: func(v any) (err error) { raw.model, err = serdeOptString(v); return }},
		{key: "choices", optional: true, set: func(v any) error {
			raw.choices = []rawChatChoice{}
			return serdeSeq(v, func(item any) error {
				choice, err := decodeRawChatChoice(item)
				raw.choices = append(raw.choices, choice)
				return err
			})
		}},
		{key: "usage", optional: true, set: func(v any) error {
			usage, err := decodeRawChatUsage(v)
			raw.usage = &usage
			return err
		}},
		{key: "system_fingerprint", optional: true, set: func(v any) (err error) { raw.systemFingerprint, err = serdeOptString(v); return }},
		{key: "service_tier", optional: true, set: func(v any) (err error) { raw.serviceTier, err = serdeOptString(v); return }},
	}, true, raw.extra)
	for key, v := range raw.extra {
		raw.extra[key] = cloneValue(v)
	}
	return raw, err
}

// Decodes an OpenAI Chat choice with Rust deserialization semantics
func decodeRawChatChoice(value any) (rawChatChoice, error) {
	var choice rawChatChoice
	err := serdeStruct(value, "RawChoice", []serdeField{
		{key: "message", optional: true, set: func(v any) error {
			message, err := decodeRawChatMessage(v)
			choice.message = &message
			return err
		}},
		{key: "finish_reason", optional: true, set: func(v any) (err error) { choice.finishReason, err = serdeOptString(v); return }},
		{key: "logprobs", optional: true, set: func(v any) error { choice.logprobs = cloneValue(v); return nil }},
	}, false, nil)
	return choice, err
}

// Decodes an OpenAI Chat response message with Rust deserialization semantics
func decodeRawChatMessage(value any) (rawChatMessage, error) {
	var message rawChatMessage
	err := serdeStruct(value, "RawMessage", []serdeField{
		{key: "content", optional: true, set: func(v any) (err error) { message.content, err = serdeOptString(v); return }},
		{key: "tool_calls", optional: true, set: func(v any) error {
			message.hasToolCalls = true
			message.toolCalls = []rawChatToolCall{}
			return serdeSeq(v, func(item any) error {
				call, err := decodeRawChatToolCall(item)
				message.toolCalls = append(message.toolCalls, call)
				return err
			})
		}},
	}, false, nil)
	return message, err
}

// Decodes an OpenAI Chat response tool call with Rust deserialization semantics
func decodeRawChatToolCall(value any) (rawChatToolCall, error) {
	var call rawChatToolCall
	err := serdeStruct(value, "RawToolCall", []serdeField{
		{key: "id", optional: true, set: func(v any) (err error) { call.id, err = serdeOptString(v); return }},
		{key: "function", optional: true, set: func(v any) error {
			var function rawChatFunction
			err := serdeStruct(v, "RawFunction", []serdeField{
				{key: "name", optional: true, set: func(v any) (err error) { function.name, err = serdeOptString(v); return }},
				{key: "arguments", optional: true, set: func(v any) (err error) { function.arguments, err = serdeOptString(v); return }},
			}, false, nil)
			call.function = &function
			return err
		}},
	}, false, nil)
	return call, err
}

// Decodes OpenAI Chat usage with Rust deserialization semantics
func decodeRawChatUsage(value any) (rawChatUsage, error) {
	var usage rawChatUsage
	err := serdeStruct(value, "RawChatUsage", []serdeField{
		{key: "prompt_tokens", optional: true, set: func(v any) (err error) { usage.promptTokens, err = serdeOptU64(v); return }},
		{key: "completion_tokens", optional: true, set: func(v any) (err error) { usage.completionTokens, err = serdeOptU64(v); return }},
		{key: "total_tokens", optional: true, set: func(v any) (err error) { usage.totalTokens, err = serdeOptU64(v); return }},
		{key: "prompt_tokens_details", optional: true, set: func(v any) error {
			return serdeStruct(v, "RawPromptTokensDetails", []serdeField{
				{key: "cached_tokens", optional: true, set: func(v any) (err error) { usage.cachedTokens, err = serdeOptU64(v); return }},
			}, false, nil)
		}},
		{key: "cost_usd", optional: true, set: func(v any) (err error) { usage.providerCost, err = serdeOptF64(v); return }},
		{key: "cost", optional: true, set: func(v any) error {
			if serdeNumberOf(v).kind != serdeNumNone {
				f, _ := serdeF64(v)
				usage.scalarCost = &f
				return nil
			}
			detailed, err := decodeRawUsageCost(v)
			if err != nil {
				return errors.New("data did not match any variant of untagged enum RawChatUsageCost")
			}
			usage.detailedCost = &detailed
			return nil
		}},
	}, false, nil)
	return usage, err
}

// Decodes an OpenAI Chat completion into the normalized response
func (*OpenAIChatCodec) DecodeResponse(response any) (*types.AnnotatedLLMResponse, error) {
	raw, err := decodeRawChatCompletion(response)
	if err != nil {
		return nil, types.Internalf("OpenAI Chat response decode: %v", err)
	}
	var choice *rawChatChoice
	if len(raw.choices) > 0 {
		choice = &raw.choices[0]
	}
	var message types.MessageContent
	var toolCalls []types.ResponseToolCall
	if choice != nil && choice.message != nil {
		if choice.message.content != nil {
			message = types.MessageContentText(*choice.message.content)
		}
		if choice.message.hasToolCalls {
			toolCalls = make([]types.ResponseToolCall, 0, len(choice.message.toolCalls))
			for _, call := range choice.message.toolCalls {
				if call.function == nil || call.function.name == nil {
					continue
				}
				id := ""
				if call.id != nil {
					id = *call.id
				}
				var arguments any = map[string]any{}
				if call.function.arguments != nil {
					arguments = parseArguments(*call.function.arguments)
				}
				toolCalls = append(toolCalls, types.ResponseToolCall{ID: id, Name: *call.function.name, Arguments: arguments})
			}
		}
	}
	var finishReason *types.FinishReason
	if choice != nil && choice.finishReason != nil {
		reason := mapChatFinishReason(*choice.finishReason)
		finishReason = &reason
	}
	modelProvider := InferModelProvider("openai", raw.model)
	var usage *types.Usage
	if raw.usage != nil {
		u := raw.usage
		usage = &types.Usage{
			PromptTokens:     u.promptTokens,
			CompletionTokens: u.completionTokens,
			TotalTokens:      u.totalTokens,
			CacheReadTokens:  u.cachedTokens,
		}
		if u.promptTokens != nil && u.cachedTokens != nil && *u.promptTokens >= *u.cachedTokens {
			usage.UncachedInputTokens = u64Ptr(*u.promptTokens - *u.cachedTokens)
		}
		providerCost := u.providerCost
		if providerCost == nil {
			providerCost = u.scalarCost
		}
		usage.Cost = providerReportedCost(providerCost, u.detailedCost)
		if usage.Cost == nil && raw.model != nil {
			usage.Cost = EstimateCostForProvider(modelProvider, *raw.model, usage)
		}
	}
	var logprobs any
	if choice != nil {
		logprobs = choice.logprobs
	}
	return &types.AnnotatedLLMResponse{
		ID:           raw.id,
		Model:        raw.model,
		Message:      message,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		APISpecific: types.APISpecificResponseOpenAIChat{
			Logprobs:          logprobs,
			SystemFingerprint: raw.systemFingerprint,
			ServiceTier:       raw.serviceTier,
		},
		Extra: raw.extra,
	}, nil
}

// Decodes an OpenAI Chat Completions request into the normalized request
func (*OpenAIChatCodec) Decode(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
	obj, ok := request.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("request content is not an object")
	}
	rawMessages, ok := obj["messages"]
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat request is missing messages")
	}
	messageValues, ok := rawMessages.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Chat messages must be an array")
	}
	messages := make([]types.Message, 0, len(messageValues))
	for _, value := range messageValues {
		message, err := decodeChatMessage(value)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	const surface = "OpenAI Chat"
	model, err := optionalString(obj, "model", surface)
	if err != nil {
		return nil, err
	}
	temperature, err := optionalF64(obj, "temperature", surface)
	if err != nil {
		return nil, err
	}
	topP, err := optionalF64(obj, "top_p", surface)
	if err != nil {
		return nil, err
	}
	var stop []string
	switch value := obj["stop"].(type) {
	case nil:
	case string:
		stop = []string{value}
	case []any:
		stop, err = serdeStrings(value)
		if err != nil {
			return nil, types.InvalidArgumentf("invalid OpenAI Chat stop value: %v", err)
		}
	default:
		return nil, types.InvalidArgumentf("OpenAI Chat stop must be a string, array, or null")
	}
	maxCompletionTokens, err := optionalU64(obj, "max_completion_tokens", surface)
	if err != nil {
		return nil, err
	}
	maxTokens, err := optionalU64(obj, "max_tokens", surface)
	if err != nil {
		return nil, err
	}
	if maxCompletionTokens != nil {
		maxTokens = maxCompletionTokens
	}
	var params *types.GenerationParams
	if temperature != nil || maxTokens != nil || topP != nil || stop != nil {
		params = &types.GenerationParams{Temperature: temperature, MaxTokens: maxTokens, TopP: topP, Stop: stop}
	}
	var tools []types.ToolDefinition
	if value, ok := obj["tools"]; ok {
		items, ok := value.([]any)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Chat tools must be an array")
		}
		tools = make([]types.ToolDefinition, 0, len(items))
		for _, item := range items {
			tool, err := decodeChatTool(item)
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
	}
	var toolChoice types.ToolChoice
	if value, ok := obj["tool_choice"]; ok {
		toolChoice = decodeChatToolChoice(value)
	}
	store, err := optionalBool(obj, "store", surface)
	if err != nil {
		return nil, err
	}
	user, err := optionalString(obj, "user", surface)
	if err != nil {
		return nil, err
	}
	serviceTier, err := optionalString(obj, "service_tier", surface)
	if err != nil {
		return nil, err
	}
	parallelToolCalls, err := optionalBool(obj, "parallel_tool_calls", surface)
	if err != nil {
		return nil, err
	}
	topLogprobs, err := optionalU64(obj, "top_logprobs", surface)
	if err != nil {
		return nil, err
	}
	stream, err := optionalBool(obj, "stream", surface)
	if err != nil {
		return nil, err
	}
	frequencyPenalty, err := optionalF64(obj, "frequency_penalty", surface)
	if err != nil {
		return nil, err
	}
	var functions []any
	switch value := obj["functions"].(type) {
	case nil:
	case []any:
		functions = cloneSlice(value)
	default:
		return nil, types.InvalidArgumentf("OpenAI Chat functions must be an array or null")
	}
	logprobs, err := optionalBool(obj, "logprobs", surface)
	if err != nil {
		return nil, err
	}
	var modalities []string
	if value := obj["modalities"]; value != nil {
		modalities, err = serdeStrings(value)
		if err != nil {
			return nil, types.InvalidArgumentf("invalid OpenAI Chat modalities: %v", err)
		}
	}
	n, err := optionalU64(obj, "n", surface)
	if err != nil {
		return nil, err
	}
	presencePenalty, err := optionalF64(obj, "presence_penalty", surface)
	if err != nil {
		return nil, err
	}
	stringFields := map[string]*string{}
	for _, key := range []string{"prompt_cache_key", "prompt_cache_retention", "reasoning_effort", "safety_identifier"} {
		value, err := optionalString(obj, key, surface)
		if err != nil {
			return nil, err
		}
		stringFields[key] = value
	}
	seed, err := optionalI64(obj, "seed", surface)
	if err != nil {
		return nil, err
	}
	verbosity, err := optionalString(obj, "verbosity", surface)
	if err != nil {
		return nil, err
	}
	objects := map[string]any{}
	for _, key := range []string{"metadata", "audio", "logit_bias", "moderation", "prediction", "prompt_cache_options", "response_format", "stream_options", "web_search_options"} {
		value, err := optionalObject(obj, key, surface)
		if err != nil {
			return nil, err
		}
		objects[key] = cloneValue(value)
	}
	functionCall, functionCallPresent := obj["function_call"]
	return &types.AnnotatedLLMRequest{
		Messages:          messages,
		Model:             model,
		Params:            params,
		Tools:             tools,
		ToolChoice:        toolChoice,
		Store:             store,
		User:              user,
		Metadata:          objects["metadata"],
		ServiceTier:       serviceTier,
		ParallelToolCalls: parallelToolCalls,
		TopLogprobs:       topLogprobs,
		Stream:            stream,
		APISpecific: types.APISpecificRequestOpenAIChat{
			Audio:                objects["audio"],
			FrequencyPenalty:     frequencyPenalty,
			FunctionCall:         cloneValue(functionCall),
			FunctionCallPresent:  functionCallPresent,
			Functions:            functions,
			LogitBias:            objects["logit_bias"],
			Logprobs:             logprobs,
			Modalities:           modalities,
			Moderation:           objects["moderation"],
			N:                    n,
			Prediction:           objects["prediction"],
			PresencePenalty:      presencePenalty,
			PromptCacheKey:       stringFields["prompt_cache_key"],
			PromptCacheOptions:   objects["prompt_cache_options"],
			PromptCacheRetention: stringFields["prompt_cache_retention"],
			ReasoningEffort:      stringFields["reasoning_effort"],
			ResponseFormat:       objects["response_format"],
			SafetyIdentifier:     stringFields["safety_identifier"],
			Seed:                 seed,
			StreamOptions:        objects["stream_options"],
			Verbosity:            verbosity,
			WebSearchOptions:     objects["web_search_options"],
		},
		Extra: objectWithoutList(obj, chatModeledRequestKeys),
	}, nil
}

// Re-decodes the original request as the baseline and merges the edits into it
func (c *OpenAIChatCodec) Encode(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest) (*types.LLMRequest, error) {
	baseline, err := c.Decode(original)
	if err != nil {
		return nil, err
	}
	return c.EncodeWithBaseline(annotated, original, baseline)
}

// Merges the edits between baseline and annotated into a copy of the original request
func (*OpenAIChatCodec) EncodeWithBaseline(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest, baseline *types.AnnotatedLLMRequest) (*types.LLMRequest, error) {
	out := original.Clone()
	obj, ok := out.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("original content is not an object")
	}
	if err := patchChatMessagesAndValidate(obj, annotated, baseline); err != nil {
		return nil, err
	}
	patchChatModelAndParams(obj, annotated, baseline)
	if err := patchChatTools(obj, annotated, baseline); err != nil {
		return nil, err
	}
	patchChatCommonFields(obj, annotated, baseline)
	if err := patchChatAPISpecific(obj, annotated.APISpecific, baseline.APISpecific); err != nil {
		return nil, err
	}
	patchExtraFields(obj, baseline.Extra, annotated.Extra)
	return &out, nil
}

// OpenAIChatStreamingCodec assembles streamed chat completion chunks into one completion
type OpenAIChatStreamingCodec struct {
	mu    sync.Mutex
	state openAIChatStreamingState
}

// Returns an OpenAI Chat streaming codec with empty state
func NewOpenAIChatStreamingCodec() *OpenAIChatStreamingCodec {
	return &OpenAIChatStreamingCodec{}
}

// Returns a collector that folds each chunk into the shared state
func (c *OpenAIChatStreamingCodec) Collector() types.LLMCollectorFn {
	return func(chunk any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.observe(chunk)
		return nil
	}
}

// Returns a collector that reads each chunk without taking ownership
func (c *OpenAIChatStreamingCodec) BorrowedCollector() types.LLMBorrowedCollectorFn {
	return func(chunk any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.observe(chunk)
		return nil
	}
}

// Returns a finalizer that emits the assembled completion and resets the state
func (c *OpenAIChatStreamingCodec) Finalizer() types.LLMFinalizerFn {
	return func() any {
		c.mu.Lock()
		state := c.state
		c.state = openAIChatStreamingState{}
		c.mu.Unlock()
		return state.finalize()
	}
}

// Accumulated chat completion stream state
type openAIChatStreamingState struct {
	id      *string
	object  *string
	created *uint64
	model   *string
	choices map[uint64]*chatChoiceState
	usage   any
}

// Accumulated state of one streamed choice
type chatChoiceState struct {
	role         *string
	content      strings.Builder
	hasContent   bool
	toolCalls    map[uint64]*chatToolCallState
	finishReason *string
}

// Accumulated state of one streamed tool call
type chatToolCallState struct {
	id        *string
	kind      *string
	name      *string
	arguments strings.Builder
}

// Folds one stream chunk into the state
func (s *openAIChatStreamingState) observe(chunk any) {
	if id, ok := getString(chunk, "id"); s.id == nil && ok && id != "" {
		s.id = strPtr(id)
	}
	if object, ok := getString(chunk, "object"); s.object == nil && ok && object != "" {
		s.object = strPtr(object)
	}
	if created, ok := getU64(chunk, "created"); s.created == nil && ok && created != 0 {
		s.created = u64Ptr(created)
	}
	if model, ok := getString(chunk, "model"); s.model == nil && ok && model != "" {
		s.model = strPtr(model)
	}
	if usage, ok := getValue(chunk, "usage"); ok && usage != nil {
		s.usage = cloneValue(usage)
	}
	value, _ := getValue(chunk, "choices")
	choices, ok := value.([]any)
	if !ok {
		return
	}
	for _, choice := range choices {
		s.observeChoice(choice)
	}
}

// Folds one streamed choice delta into its indexed choice state
func (s *openAIChatStreamingState) observeChoice(choice any) {
	index, _ := getU64(choice, "index")
	if s.choices == nil {
		s.choices = map[uint64]*chatChoiceState{}
	}
	entry, ok := s.choices[index]
	if !ok {
		entry = &chatChoiceState{}
		s.choices[index] = entry
	}
	if reason, ok := getString(choice, "finish_reason"); ok {
		entry.finishReason = strPtr(reason)
	}
	if delta, ok := getValue(choice, "delta"); ok {
		entry.observeDelta(delta)
	}
}

// Builds the assembled chat completion object
func (s *openAIChatStreamingState) finalize() any {
	output := map[string]any{}
	if s.id != nil {
		output["id"] = *s.id
	}
	if s.object != nil {
		output["object"] = strings.TrimSuffix(*s.object, ".chunk")
	}
	if s.created != nil {
		output["created"] = *s.created
	}
	if s.model != nil {
		output["model"] = *s.model
	}
	choices := []any{}
	for _, index := range sortedU64Keys(s.choices) {
		choices = append(choices, s.choices[index].finalize(index))
	}
	output["choices"] = choices
	if s.usage != nil {
		output["usage"] = s.usage
	}
	return output
}

// Folds role, content and tool call fragments of a delta into the choice
func (c *chatChoiceState) observeDelta(delta any) {
	if role, ok := getString(delta, "role"); ok {
		c.role = strPtr(role)
	}
	if content, ok := getString(delta, "content"); ok {
		c.content.WriteString(content)
		c.hasContent = true
	}
	value, _ := getValue(delta, "tool_calls")
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for _, call := range calls {
		index, _ := getU64(call, "index")
		if c.toolCalls == nil {
			c.toolCalls = map[uint64]*chatToolCallState{}
		}
		state, ok := c.toolCalls[index]
		if !ok {
			state = &chatToolCallState{}
			c.toolCalls[index] = state
		}
		if id, ok := getString(call, "id"); ok {
			state.id = strPtr(id)
		}
		if kind, ok := getString(call, "type"); ok {
			state.kind = strPtr(kind)
		}
		if function, ok := getValue(call, "function"); ok {
			if name, ok := getString(function, "name"); ok {
				state.name = strPtr(name)
			}
			if arguments, ok := getString(function, "arguments"); ok {
				state.arguments.WriteString(arguments)
			}
		}
	}
}

// Builds the assembled choice object
func (c *chatChoiceState) finalize(index uint64) any {
	role := "assistant"
	if c.role != nil {
		role = *c.role
	}
	message := map[string]any{"role": role, "content": nil}
	if c.hasContent {
		message["content"] = c.content.String()
	}
	if len(c.toolCalls) > 0 {
		calls := []any{}
		for _, key := range sortedU64Keys(c.toolCalls) {
			calls = append(calls, c.toolCalls[key].finalize())
		}
		message["tool_calls"] = calls
	}
	choice := map[string]any{"index": index, "message": message, "finish_reason": nil}
	if c.finishReason != nil {
		choice["finish_reason"] = *c.finishReason
	}
	return choice
}

// Builds the assembled tool call object
func (t *chatToolCallState) finalize() any {
	name := ""
	if t.name != nil {
		name = *t.name
	}
	kind := "function"
	if t.kind != nil {
		kind = *t.kind
	}
	call := map[string]any{
		"type":     kind,
		"function": map[string]any{"name": name, "arguments": t.arguments.String()},
	}
	if t.id != nil {
		call["id"] = *t.id
	}
	return call
}

// Returns the keys of an index map in ascending order
func sortedU64Keys[V any](m map[uint64]V) []uint64 {
	keys := make([]uint64, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

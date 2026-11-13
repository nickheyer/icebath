package codec

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/nemorelay/logging"
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// OpenAIResponsesCodec translates OpenAI Responses requests and responses
type OpenAIResponsesCodec struct{}

// Returns the OpenAI Responses codec
func NewOpenAIResponsesCodec() *OpenAIResponsesCodec {
	return &OpenAIResponsesCodec{}
}

var openAIResponsesProviderSurface = providerSurfaceDescriptor{
	surface: ProviderSurfaceOpenAIResponses,
	detectRequest: func(obj map[string]any, _ *string) bool {
		_, hasInput := obj["input"]
		_, hasInstructions := obj["instructions"]
		return hasInput || hasInstructions
	},
	detectResponse: func(obj map[string]any) bool {
		_, isArray := obj["output"].([]any)
		_, isString := obj["output_text"].(string)
		return isArray || isString
	},
	decodeRequest: func(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
		return NewOpenAIResponsesCodec().Decode(request)
	},
	decodeResponse: func(response any) (*types.AnnotatedLLMResponse, error) {
		return NewOpenAIResponsesCodec().DecodeResponse(response)
	},
	codecName:      "openai_responses",
	requestCodec:   func() types.LLMCodec { return NewOpenAIResponsesCodec() },
	responseCodec:  func() types.LLMResponseCodec { return NewOpenAIResponsesCodec() },
	streamingCodec: func() StreamingCodec { return NewOpenAIResponsesStreamingCodec() },
}

var responsesModeledRequestKeys = []string{
	"input", "instructions", "model", "max_output_tokens", "temperature", "top_p", "tools",
	"tool_choice", "store", "previous_response_id", "truncation", "reasoning", "include", "user",
	"metadata", "service_tier", "parallel_tool_calls", "max_tool_calls", "top_logprobs", "stream",
	"background", "context_management", "conversation", "moderation", "prompt", "prompt_cache_key",
	"prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options", "text",
}

var responsesAPISpecificKeys = []string{
	"background", "context_management", "conversation", "moderation", "prompt", "prompt_cache_key",
	"prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options", "text",
}

// Identifies the builtin OpenAI Responses codec
func (*OpenAIResponsesCodec) CodecIdentity() types.LLMCodecIdentity {
	return types.LLMCodecIdentity{Kind: types.LLMCodecIdentityBuiltIn, Builtin: types.BuiltinLLMCodecOpenAIResponses}
}

// Maps a Responses status and incomplete details to the normalized finish reason
func mapResponsesFinishReason(status *string, incompleteDetails any) *types.FinishReason {
	if status == nil {
		return nil
	}
	reason := func(kind types.FinishReasonKind, unknown string) *types.FinishReason {
		return &types.FinishReason{Kind: kind, Unknown: unknown}
	}
	switch *status {
	case "completed":
		return reason(types.FinishReasonComplete, "")
	case "incomplete":
		incomplete, ok := getString(incompleteDetails, "reason")
		if !ok {
			return reason(types.FinishReasonUnknown, "incomplete")
		}
		switch incomplete {
		case "max_output_tokens":
			return reason(types.FinishReasonLength, "")
		case "content_filter":
			return reason(types.FinishReasonContentFilter, "")
		}
		return reason(types.FinishReasonUnknown, incomplete)
	}
	return reason(types.FinishReasonUnknown, *status)
}

// Collects output text and function calls from Responses output items
func collectResponsesOutputParts(items []any) ([]string, []types.ResponseToolCall) {
	var textParts []string
	var toolCalls []types.ResponseToolCall
	for _, item := range items {
		kind, _ := getString(item, "type")
		switch kind {
		case "message":
			value, _ := getValue(item, "content")
			blocks, _ := value.([]any)
			for _, block := range blocks {
				if text, ok := responsesOutputTextBlock(block); ok {
					textParts = append(textParts, text)
				}
			}
		case "output_text":
			if text, ok := responsesOutputTextBlock(item); ok {
				textParts = append(textParts, text)
			}
		case "function_call":
			toolCalls = append(toolCalls, parseResponsesFunctionCall(item))
		}
	}
	return textParts, toolCalls
}

// Returns the text of an output_text block
func responsesOutputTextBlock(block any) (string, bool) {
	if kind, _ := getString(block, "type"); kind != "output_text" {
		return "", false
	}
	return getString(block, "text")
}

// Converts a function_call output item to a response tool call
func parseResponsesFunctionCall(item any) types.ResponseToolCall {
	id, _ := getString(item, "call_id")
	name, _ := getString(item, "name")
	var arguments any = map[string]any{}
	if raw, ok := getString(item, "arguments"); ok {
		arguments = parseArguments(raw)
	}
	return types.ResponseToolCall{ID: id, Name: name, Arguments: arguments}
}

// Joins collected output text into message content
func responsesMessageFromTextParts(textParts []string) types.MessageContent {
	if len(textParts) == 0 {
		return nil
	}
	return types.MessageContentText(strings.Join(textParts, "\n"))
}

// Wraps a raw Responses component as a provider native component
func responsesNative(kind string, value any) types.ProviderNativeComponent {
	return types.ProviderNativeComponent{Provider: "openai_responses", Kind: kind, Value: cloneValue(value)}
}

// Decodes Responses message content from a string or part array
func decodeResponsesContent(value any) (types.MessageContent, error) {
	if text, ok := value.(string); ok {
		return types.MessageContentText(text), nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Responses message content must be a string or array")
	}
	out := make(types.MessageContentParts, 0, len(parts))
	for _, part := range parts {
		decoded, err := decodeResponsesContentPart(part)
		if err != nil {
			return nil, err
		}
		out = append(out, decoded)
	}
	return out, nil
}

// Copies only the listed keys of an object
func objectOnly(obj map[string]any, keep ...string) map[string]any {
	out := map[string]any{}
	for key, value := range obj {
		if containsString(keep, key) {
			out[key] = cloneValue(value)
		}
	}
	return out
}

// Decodes one Responses content part, keeping unknown part types native
func decodeResponsesContentPart(value any) (types.ContentPart, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Responses content part must be an object")
	}
	kind, ok := obj["type"].(string)
	if !ok {
		kind = "unknown"
	}
	switch kind {
	case "input_text", "output_text":
		text, ok := obj["text"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses text part is missing text")
		}
		return types.ContentPartText{Text: text, Extra: objectWithout(obj, "type", "text")}, nil
	case "input_image":
		return types.ContentPartImage{
			Image: objectOnly(obj, "image_url", "file_id", "detail"),
			Extra: objectWithout(obj, "type", "image_url", "file_id", "detail"),
		}, nil
	case "input_file":
		return types.ContentPartFile{
			File:  objectOnly(obj, "file_data", "file_id", "file_url", "filename"),
			Extra: objectWithout(obj, "type", "file_data", "file_id", "file_url", "filename"),
		}, nil
	case "refusal":
		refusal, ok := obj["refusal"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses refusal part is missing refusal")
		}
		return types.ContentPartRefusal{Refusal: refusal, Extra: objectWithout(obj, "type", "refusal")}, nil
	}
	return types.ContentPartProviderNative{Provider: "openai_responses", Kind: kind, Value: cloneValue(value)}, nil
}

// Reads an optional string id of a Responses input item
func responsesItemID(obj map[string]any, message string) (*string, error) {
	switch id := obj["id"].(type) {
	case nil:
		return nil, nil
	case string:
		return &id, nil
	}
	return nil, types.InvalidArgumentf("%s", message)
}

// Decodes one Responses input item into a normalized message
func decodeResponsesInputItem(value any, codexDialect bool) (types.Message, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Responses input item must be an object")
	}
	if role, ok := obj["role"].(string); ok {
		native := types.MessageProviderNative{Provider: "openai_responses", Kind: "message", Value: cloneValue(value)}
		if !onlyKeys(obj, "type", "role", "content") {
			return native, nil
		}
		raw, ok := obj["content"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses message is missing content")
		}
		content, err := decodeResponsesContent(raw)
		if err != nil {
			return nil, err
		}
		switch role {
		case "user":
			return types.MessageUser{Content: content}, nil
		case "system":
			return types.MessageSystem{Content: content}, nil
		case "developer":
			return types.MessageDeveloper{Content: content}, nil
		case "assistant":
			return types.MessageAssistant{Content: content}, nil
		}
		return native, nil
	}
	kind, ok := obj["type"].(string)
	if !ok {
		kind = "unknown"
	}
	switch kind {
	case "function_call":
		callID, ok := obj["call_id"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses function_call is missing call_id")
		}
		name, ok := obj["name"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses function_call is missing name")
		}
		arguments, ok := obj["arguments"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses function_call is missing arguments")
		}
		id, err := responsesItemID(obj, "OpenAI Responses function_call id must be a string or null")
		if err != nil {
			return nil, err
		}
		return types.MessageToolCallItem{
			ID:        id,
			CallID:    callID,
			Name:      name,
			Arguments: parseArguments(arguments),
			Extra:     objectWithout(obj, "type", "id", "call_id", "name", "arguments"),
		}, nil
	case "function_call_output":
		if codexDialect && obj["call_id"] == nil {
			name, _ := obj["name"].(string)
			namespace, _ := obj["namespace"].(string)
			_, hasOutput := obj["output"]
			if name != "" && namespace != "" && hasOutput {
				return types.MessageProviderNative{Provider: "openai_responses", Kind: kind, Value: cloneValue(value)}, nil
			}
		}
		callID, ok := obj["call_id"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses function_call_output is missing call_id")
		}
		output, ok := obj["output"]
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses function_call_output is missing output")
		}
		id, err := responsesItemID(obj, "OpenAI Responses function_call_output id must be a string or null")
		if err != nil {
			return nil, err
		}
		return types.MessageToolResultItem{
			ID:     id,
			CallID: callID,
			Output: cloneValue(output),
			Extra:  objectWithout(obj, "type", "id", "call_id", "output"),
		}, nil
	}
	return types.MessageProviderNative{Provider: "openai_responses", Kind: kind, Value: cloneValue(value)}, nil
}

// Encodes message content into Responses content, using output parts for assistant turns
func encodeResponsesContent(content types.MessageContent, assistant bool) (any, error) {
	switch c := content.(type) {
	case types.MessageContentText:
		return string(c), nil
	case types.MessageContentParts:
		out := make([]any, 0, len(c))
		for _, part := range c {
			encoded, err := encodeResponsesContentPart(part, assistant)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		}
		return out, nil
	}
	return nil, types.InvalidArgumentf("OpenAI Responses message content must be a string or array")
}

// Encodes one content part into a Responses content part
func encodeResponsesContentPart(part types.ContentPart, assistant bool) (any, error) {
	switch p := part.(type) {
	case types.ContentPartText:
		obj := cloneObject(p.Extra)
		obj["type"] = "input_text"
		if assistant {
			obj["type"] = "output_text"
		}
		obj["text"] = p.Text
		return obj, nil
	case types.ContentPartImageURL:
		obj := cloneObject(p.Extra)
		obj["type"] = "input_image"
		obj["image_url"] = p.ImageURL.URL
		if p.ImageURL.Detail != nil {
			obj["detail"] = *p.ImageURL.Detail
		}
		return obj, nil
	case types.ContentPartImage:
		image, ok := p.Image.(map[string]any)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses image content must be an object")
		}
		obj := cloneObject(image)
		for key, value := range p.Extra {
			obj[key] = cloneValue(value)
		}
		obj["type"] = "input_image"
		return obj, nil
	case types.ContentPartFile:
		file, ok := p.File.(map[string]any)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses file content must be an object")
		}
		obj := cloneObject(file)
		for key, value := range p.Extra {
			obj[key] = cloneValue(value)
		}
		obj["type"] = "input_file"
		return obj, nil
	case types.ContentPartRefusal:
		if assistant {
			obj := cloneObject(p.Extra)
			obj["type"] = "refusal"
			obj["refusal"] = p.Refusal
			return obj, nil
		}
	case types.ContentPartProviderNative:
		if p.Provider == "openai_responses" {
			return cloneValue(p.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("content part %s cannot be encoded for OpenAI Responses", types.RustDebug(part))
}

// Encodes one normalized message into a Responses input item
func encodeResponsesInputItem(message types.Message) (any, error) {
	roleMessage := func(role string, content types.MessageContent, assistant bool) (any, error) {
		encoded, err := encodeResponsesContent(content, assistant)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "message", "role": role, "content": encoded}, nil
	}
	switch m := message.(type) {
	case types.MessageUser:
		return roleMessage("user", m.Content, false)
	case types.MessageSystem:
		return roleMessage("system", m.Content, false)
	case types.MessageDeveloper:
		return roleMessage("developer", m.Content, false)
	case types.MessageAssistant:
		if m.Content != nil {
			return roleMessage("assistant", m.Content, true)
		}
	case types.MessageToolCallItem:
		obj := cloneObject(m.Extra)
		obj["type"] = "function_call"
		if m.ID != nil {
			obj["id"] = *m.ID
		}
		obj["call_id"] = m.CallID
		obj["name"] = m.Name
		arguments, isString := m.Arguments.(string)
		if !isString {
			encoded, err := types.EncodeJSONString(m.Arguments)
			if err != nil {
				return nil, types.Internalf("OpenAI Responses function arguments encode: %v", err)
			}
			arguments = encoded
		}
		obj["arguments"] = arguments
		return obj, nil
	case types.MessageToolResultItem:
		obj := cloneObject(m.Extra)
		obj["type"] = "function_call_output"
		if m.ID != nil {
			obj["id"] = *m.ID
		}
		obj["call_id"] = m.CallID
		obj["output"] = cloneValue(m.Output)
		return obj, nil
	case types.MessageProviderNative:
		if m.Provider == "openai_responses" {
			return cloneValue(m.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("message %s cannot be encoded for OpenAI Responses", types.RustDebug(message))
}

// Decodes one Responses tool, accepting flat and nested function shapes
func decodeResponsesTool(value any) (types.ToolDefinition, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Responses tool must be an object")
	}
	if kind, isString := obj["type"].(string); kind != "function" {
		if !isString {
			kind = "unknown"
		}
		return types.ToolDefinitionProviderNative{Provider: "openai_responses", Kind: kind, Value: cloneValue(value)}, nil
	}
	function := obj
	wrapperExtra := map[string]any{}
	if nested, ok := obj["function"].(map[string]any); ok {
		function = nested
		wrapperExtra = objectWithout(obj, "type", "function")
	}
	name, ok := function["name"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Responses function tool is missing name")
	}
	description, err := optionalString(function, "description", "OpenAI Responses function tool")
	if err != nil {
		return nil, err
	}
	strict, err := optionalBool(function, "strict", "OpenAI Responses function tool")
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
			Extra:             objectWithout(function, "type", "name", "description", "parameters", "strict", "function"),
		},
		Extra: wrapperExtra,
	}, nil
}

// Encodes one normalized tool into a flat Responses function tool
func encodeResponsesTool(tool types.ToolDefinition) (any, error) {
	switch t := tool.(type) {
	case types.ToolDefinitionFunction:
		obj := cloneObject(t.Extra)
		for key, value := range encodeResponsesFunction(t.Function) {
			obj[key] = value
		}
		obj["type"] = "function"
		return obj, nil
	case types.ToolDefinitionProviderNative:
		if t.Provider == "openai_responses" {
			return cloneValue(t.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("tool %s cannot be encoded for OpenAI Responses", types.RustDebug(tool))
}

// Encodes a function definition into its Responses fields
func encodeResponsesFunction(function types.FunctionDefinition) map[string]any {
	obj := cloneObject(function.Extra)
	obj["name"] = function.Name
	if function.Description != nil {
		obj["description"] = *function.Description
	}
	if function.ParametersPresent || function.Parameters != nil {
		obj["parameters"] = cloneValue(function.Parameters)
	}
	if function.Strict != nil {
		obj["strict"] = *function.Strict
	}
	return obj
}

// Patches an edited tool into its original form, keeping a nested function wrapper
func patchResponsesTool(original any, baseline, edited types.ToolDefinition, baselineValue, editedValue any) (any, error) {
	originalObj, isObject := original.(map[string]any)
	baselineFunction, baselineIsFunction := baseline.(types.ToolDefinitionFunction)
	editedFunction, editedIsFunction := edited.(types.ToolDefinitionFunction)
	if isObject && baselineIsFunction && editedIsFunction {
		if originalFunction, ok := originalObj["function"]; ok {
			patched := cloneObject(originalObj)
			patchExtraFields(patched, baselineFunction.Extra, editedFunction.Extra)
			function, err := patchChangedJSON(
				originalFunction,
				encodeResponsesFunction(baselineFunction.Function),
				encodeResponsesFunction(editedFunction.Function),
			)
			if err != nil {
				return nil, err
			}
			patched["function"] = function
			return patched, nil
		}
	}
	return patchChangedJSON(original, baselineValue, editedValue)
}

// Decodes a tool choice in OpenAI or Anthropic shape, keeping unrecognized shapes native
func decodeOpenAIOrAnthropicToolChoice(value any) types.ToolChoice {
	switch value {
	case "auto":
		return types.ToolChoiceAuto
	case "none":
		return types.ToolChoiceNone
	case "required":
		return types.ToolChoiceRequired
	}
	if obj, ok := value.(map[string]any); ok {
		if choiceType, ok := obj["type"].(string); ok {
			switch choiceType {
			case "auto":
				return types.ToolChoiceAuto
			case "any":
				return types.ToolChoiceRequired
			case "none":
				return types.ToolChoiceNone
			case "tool", "function":
				name, ok := obj["name"].(string)
				if !ok {
					name, ok = getString(obj["function"], "name")
				}
				if ok {
					return types.ToolChoiceSpecific(types.ToolChoiceFunction{
						ChoiceType: "function",
						Function:   types.ToolChoiceFunctionName{Name: name},
					})
				}
			}
		}
	}
	return types.ToolChoiceProviderNative(responsesNative("tool_choice", value))
}

// Encodes a normalized tool choice into a Responses tool choice
func encodeResponsesToolChoice(choice types.ToolChoice) (any, error) {
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
		return map[string]any{"type": "function", "name": c.Function.Name}, nil
	case types.ToolChoiceProviderNative:
		if c.Provider == "openai_responses" {
			return cloneValue(c.Value), nil
		}
		return nil, types.InvalidArgumentf("tool choice for %s cannot be encoded for OpenAI Responses", c.Provider)
	}
	return nil, types.InvalidArgumentf("tool choice %s cannot be encoded for OpenAI Responses", types.RustDebug(choice))
}

// Writes edited Responses specific controls or removes them when the section was dropped
func patchResponsesAPISpecific(obj map[string]any, edited, baseline types.APISpecificRequest) error {
	e, editedIsResponses := edited.(types.APISpecificRequestOpenAIResponses)
	b, baselineIsResponses := baseline.(types.APISpecificRequestOpenAIResponses)
	switch {
	case editedIsResponses && baselineIsResponses:
		if !eqBoolPtr(e.Background, b.Background) {
			value, present := optBoolJSON(e.Background)
			setOrRemoveJSON(obj, "background", value, present)
		}
		for _, field := range []struct {
			key                    string
			value                  any
			valuePresent           bool
			old                    any
			oldPresent             bool
		}{
			{"context_management", e.ContextManagement, e.ContextManagementPresent, b.ContextManagement, b.ContextManagementPresent},
			{"conversation", e.Conversation, e.ConversationPresent, b.Conversation, b.ConversationPresent},
			{"moderation", e.Moderation, e.ModerationPresent, b.Moderation, b.ModerationPresent},
			{"prompt", e.Prompt, e.PromptPresent, b.Prompt, b.PromptPresent},
			{"prompt_cache_options", e.PromptCacheOptions, e.PromptCacheOptionsPresent, b.PromptCacheOptions, b.PromptCacheOptionsPresent},
			{"stream_options", e.StreamOptions, e.StreamOptionsPresent, b.StreamOptions, b.StreamOptionsPresent},
			{"text", e.Text, e.TextPresent, b.Text, b.TextPresent},
		} {
			if !eqOptJSON(field.value, field.valuePresent, field.old, field.oldPresent) {
				setOrRemoveJSON(obj, field.key, cloneValue(field.value), field.valuePresent || field.value != nil)
			}
		}
		for _, field := range []struct {
			key        string
			value, old *string
		}{
			{"prompt_cache_key", e.PromptCacheKey, b.PromptCacheKey},
			{"prompt_cache_retention", e.PromptCacheRetention, b.PromptCacheRetention},
			{"safety_identifier", e.SafetyIdentifier, b.SafetyIdentifier},
		} {
			if !eqStrPtr(field.value, field.old) {
				value, present := optStringJSON(field.value)
				setOrRemoveJSON(obj, field.key, value, present)
			}
		}
		return nil
	case edited == nil && baselineIsResponses:
		for _, key := range responsesAPISpecificKeys {
			delete(obj, key)
		}
		return nil
	case edited != nil || baseline != nil:
		return types.InvalidArgumentf("api_specific provider does not match OpenAI Responses")
	}
	return nil
}

// Reads parallel tool calls from the request or from the Anthropic style tool choice flag
func decodeOpenAIOrAnthropicParallelToolCalls(obj map[string]any) (*bool, error) {
	value, err := optionalBool(obj, "parallel_tool_calls", "OpenAI Responses")
	if err != nil || value != nil {
		return value, err
	}
	toolChoice, ok := obj["tool_choice"].(map[string]any)
	if !ok {
		return nil, nil
	}
	disabled, err := optionalBool(toolChoice, "disable_parallel_tool_use", "OpenAI Responses tool_choice")
	if err != nil || disabled == nil {
		return nil, err
	}
	return boolPtr(!*disabled), nil
}

// Writes edited input items and instructions
func patchResponsesMessages(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqSlice(annotated.Messages, baseline.Messages) {
		var input any
		_, inputIsString := obj["input"].(string)
		single, singleText := responsesSingleUserText(annotated.Messages)
		if inputIsString && singleText {
			input = single
		} else {
			original, hasOriginal := obj["input"].([]any)
			items, err := encodeChangedItems(annotated.Messages, baseline.Messages, original, hasOriginal, encodeResponsesInputItem)
			if err != nil {
				return err
			}
			input = items
		}
		obj["input"] = input
	}
	if !types.Equal(annotated.Instructions, baseline.Instructions) {
		switch instructions := annotated.Instructions.(type) {
		case nil:
			delete(obj, "instructions")
		case types.MessageContentText:
			obj["instructions"] = string(instructions)
		default:
			return types.InvalidArgumentf("OpenAI Responses instructions cannot contain content parts")
		}
	}
	return nil
}

// Returns the text of a message list that is exactly one unnamed plain text user turn
func responsesSingleUserText(messages []types.Message) (string, bool) {
	if len(messages) != 1 {
		return "", false
	}
	user, ok := messages[0].(types.MessageUser)
	if !ok || user.Name != nil {
		return "", false
	}
	text, ok := user.Content.(types.MessageContentText)
	return string(text), ok
}

// Writes edited model and generation parameters, rejecting stop sequences
func patchResponsesModelAndParams(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqStrPtr(annotated.Model, baseline.Model) {
		value, present := optStringJSON(annotated.Model)
		setOrRemoveJSON(obj, "model", value, present)
	}
	if eqParams(annotated.Params, baseline.Params) {
		return nil
	}
	stop := paramStop(annotated.Params)
	if !eqStrings(stop, paramStop(baseline.Params)) && stop != nil {
		return types.InvalidArgumentf("OpenAI Responses does not support stop sequences")
	}
	patchParamFloats(obj, annotated.Params, baseline.Params)
	maxTokens := paramMaxTokens(annotated.Params)
	if !eqU64Ptr(maxTokens, paramMaxTokens(baseline.Params)) {
		value, present := optU64JSON(maxTokens)
		setOrRemoveJSON(obj, "max_output_tokens", value, present)
	}
	return nil
}

// Writes edited tools and tool choice
func patchResponsesTools(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqOptSlice(annotated.Tools, baseline.Tools) {
		if annotated.Tools == nil {
			delete(obj, "tools")
		} else {
			original, hasOriginal := obj["tools"].([]any)
			tools, err := encodeChangedItemsWithPatch(annotated.Tools, baseline.Tools, original, hasOriginal, encodeResponsesTool, patchResponsesTool)
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
		edited, err := encodeResponsesToolChoice(annotated.ToolChoice)
		if err != nil {
			return err
		}
		if baseline.ToolChoice != nil {
			before, err := encodeResponsesToolChoice(baseline.ToolChoice)
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

// Writes edited top level Responses fields
func patchResponsesCommonFields(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) {
	for _, field := range []struct {
		key                  string
		value                any
		valuePresent         bool
		old                  any
		oldPresent           bool
	}{
		{"truncation", annotated.Truncation, annotated.TruncationPresent, baseline.Truncation, baseline.TruncationPresent},
		{"reasoning", annotated.Reasoning, annotated.ReasoningPresent, baseline.Reasoning, baseline.ReasoningPresent},
		{"include", annotated.Include, annotated.IncludePresent, baseline.Include, baseline.IncludePresent},
		{"metadata", annotated.Metadata, annotated.MetadataPresent, baseline.Metadata, baseline.MetadataPresent},
	} {
		if !eqOptJSON(field.value, field.valuePresent, field.old, field.oldPresent) {
			setOrRemoveJSON(obj, field.key, cloneValue(field.value), field.valuePresent || field.value != nil)
		}
	}
	for _, field := range []struct {
		key        string
		value, old *string
	}{
		{"previous_response_id", annotated.PreviousResponseID, baseline.PreviousResponseID},
		{"user", annotated.User, baseline.User},
		{"service_tier", annotated.ServiceTier, baseline.ServiceTier},
	} {
		if !eqStrPtr(field.value, field.old) {
			value, present := optStringJSON(field.value)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
	for _, field := range []struct {
		key        string
		value, old *bool
	}{
		{"store", annotated.Store, baseline.Store},
		{"parallel_tool_calls", annotated.ParallelToolCalls, baseline.ParallelToolCalls},
		{"stream", annotated.Stream, baseline.Stream},
	} {
		if !eqBoolPtr(field.value, field.old) {
			value, present := optBoolJSON(field.value)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
	for _, field := range []struct {
		key        string
		value, old *uint64
	}{
		{"max_output_tokens", annotated.MaxOutputTokens, baseline.MaxOutputTokens},
		{"max_tool_calls", annotated.MaxToolCalls, baseline.MaxToolCalls},
		{"top_logprobs", annotated.TopLogprobs, baseline.TopLogprobs},
	} {
		if !eqU64Ptr(field.value, field.old) {
			value, present := optU64JSON(field.value)
			setOrRemoveJSON(obj, field.key, value, present)
		}
	}
}

// Raw Responses response fields read from a response
type rawResponsesResponse struct {
	id                 *string
	model              *string
	status             *string
	output             []any
	usage              *rawResponsesUsage
	incompleteDetails  any
	previousResponseID *string
	store              *bool
	serviceTier        *string
	truncation         any
	reasoning          any
	extra              map[string]any
}

// Raw Responses usage fields
type rawResponsesUsage struct {
	inputTokens         *uint64
	outputTokens        *uint64
	totalTokens         *uint64
	inputTokensDetails  *rawTokensDetails
	outputTokensDetails *rawTokensDetails
	providerCost        *float64
	cost                *rawUsageCost
}

// Raw token detail object with one modeled counter and flattened extras
type rawTokensDetails struct {
	key   string
	count *uint64
	extra map[string]any
}

// Decodes a token details object with one modeled counter and flattened extras
func decodeRawTokensDetails(value any, name, key string) (*rawTokensDetails, error) {
	details := &rawTokensDetails{key: key, extra: map[string]any{}}
	err := serdeStruct(value, name, []serdeField{
		{key: key, optional: true, set: func(v any) (err error) { details.count, err = serdeOptU64(v); return }},
	}, true, details.extra)
	for k, v := range details.extra {
		details.extra[k] = cloneValue(v)
	}
	return details, err
}

// Rebuilds a token details object from its counter and extras
func (d *rawTokensDetails) toJSON() any {
	obj := map[string]any{}
	if d.count != nil {
		obj[d.key] = *d.count
	}
	for key, value := range d.extra {
		obj[key] = cloneValue(value)
	}
	return obj
}

// Decodes Responses usage with Rust deserialization semantics
func decodeRawResponsesUsage(value any) (rawResponsesUsage, error) {
	var usage rawResponsesUsage
	err := serdeStruct(value, "RawResponsesUsage", []serdeField{
		{key: "input_tokens", optional: true, set: func(v any) (err error) { usage.inputTokens, err = serdeOptU64(v); return }},
		{key: "output_tokens", optional: true, set: func(v any) (err error) { usage.outputTokens, err = serdeOptU64(v); return }},
		{key: "total_tokens", optional: true, set: func(v any) (err error) { usage.totalTokens, err = serdeOptU64(v); return }},
		{key: "input_tokens_details", optional: true, set: func(v any) (err error) {
			usage.inputTokensDetails, err = decodeRawTokensDetails(v, "RawInputTokensDetails", "cached_tokens")
			return
		}},
		{key: "output_tokens_details", optional: true, set: func(v any) (err error) {
			usage.outputTokensDetails, err = decodeRawTokensDetails(v, "RawOutputTokensDetails", "reasoning_tokens")
			return
		}},
		{key: "cost_usd", optional: true, set: func(v any) (err error) { usage.providerCost, err = serdeOptF64(v); return }},
		{key: "cost", optional: true, set: func(v any) error {
			cost, err := decodeRawUsageCost(v)
			usage.cost = &cost
			return err
		}},
	}, false, nil)
	return usage, err
}

// Decodes a Responses response with Rust deserialization semantics, reading output separately
func decodeRawResponsesResponse(response any) (rawResponsesResponse, error) {
	raw := rawResponsesResponse{extra: map[string]any{}}
	fields := []serdeField{
		{key: "id", optional: true, set: func(v any) (err error) { raw.id, err = serdeOptString(v); return }},
		{key: "model", optional: true, set: func(v any) (err error) { raw.model, err = serdeOptString(v); return }},
		{key: "status", optional: true, set: func(v any) (err error) { raw.status, err = serdeOptString(v); return }},
		{key: "output", optional: true, set: func(v any) (err error) { raw.output, err = serdeValues(v); return }},
		{key: "usage", optional: true, set: func(v any) error {
			usage, err := decodeRawResponsesUsage(v)
			raw.usage = &usage
			return err
		}},
		{key: "incomplete_details", optional: true, set: func(v any) error { raw.incompleteDetails = cloneValue(v); return nil }},
		{key: "previous_response_id", optional: true, set: func(v any) (err error) { raw.previousResponseID, err = serdeOptString(v); return }},
		{key: "store", optional: true, set: func(v any) error {
			b, err := serdeBool(v)
			raw.store = &b
			return err
		}},
		{key: "service_tier", optional: true, set: func(v any) (err error) { raw.serviceTier, err = serdeOptString(v); return }},
		{key: "truncation", optional: true, set: func(v any) error { raw.truncation = cloneValue(v); return nil }},
		{key: "reasoning", optional: true, set: func(v any) error { raw.reasoning = cloneValue(v); return nil }},
	}
	obj, ok := response.(map[string]any)
	if !ok {
		return raw, serdeStruct(response, "RawResponsesResponse", fields, true, raw.extra)
	}
	if err := serdeStruct(objectWithoutShallow(obj, "output"), "RawResponsesResponse", fields, true, raw.extra); err != nil {
		return raw, err
	}
	for key, value := range raw.extra {
		raw.extra[key] = cloneValue(value)
	}
	switch output := obj["output"].(type) {
	case nil:
	case []any:
		raw.output = cloneSlice(output)
	default:
		values, err := serdeValues(output)
		if err != nil {
			return raw, err
		}
		raw.output = values
	}
	return raw, nil
}

// Copies the top level entries of an object except the listed keys without copying values
func objectWithoutShallow(obj map[string]any, skip ...string) map[string]any {
	out := make(map[string]any, len(obj))
	for key, value := range obj {
		if !containsString(skip, key) {
			out[key] = value
		}
	}
	return out
}

// Decodes a Responses response into the normalized response
func (*OpenAIResponsesCodec) DecodeResponse(response any) (*types.AnnotatedLLMResponse, error) {
	raw, err := decodeRawResponsesResponse(response)
	if err != nil {
		return nil, types.Internalf("OpenAI Responses response decode: %v", err)
	}
	textParts, toolCalls := collectResponsesOutputParts(raw.output)
	message := responsesMessageFromTextParts(textParts)
	if message == nil {
		if text, ok := getString(response, "output_text"); ok && text != "" {
			message = types.MessageContentText(text)
		}
	}
	if len(toolCalls) == 0 {
		toolCalls = nil
	}
	finishReason := mapResponsesFinishReason(raw.status, raw.incompleteDetails)
	var inputTokensDetails, outputTokensDetails any
	modelProvider := InferModelProvider("openai", raw.model)
	var usage *types.Usage
	if raw.usage != nil {
		u := raw.usage
		var cacheRead *uint64
		if u.inputTokensDetails != nil {
			inputTokensDetails = u.inputTokensDetails.toJSON()
			cacheRead = u.inputTokensDetails.count
		}
		if u.outputTokensDetails != nil {
			outputTokensDetails = u.outputTokensDetails.toJSON()
		}
		usage = &types.Usage{
			PromptTokens:     u.inputTokens,
			CompletionTokens: u.outputTokens,
			TotalTokens:      u.totalTokens,
			CacheReadTokens:  cacheRead,
			Cost:             providerReportedCost(u.providerCost, u.cost),
		}
		if u.inputTokens != nil && cacheRead != nil && *u.inputTokens >= *cacheRead {
			usage.UncachedInputTokens = u64Ptr(*u.inputTokens - *cacheRead)
		}
		if usage.Cost == nil && raw.model != nil {
			usage.Cost = EstimateCostForProvider(modelProvider, *raw.model, usage)
		}
	}
	return &types.AnnotatedLLMResponse{
		ID:           raw.id,
		Model:        raw.model,
		Message:      message,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		APISpecific: types.APISpecificResponseOpenAIResponses{
			OutputItems:         raw.output,
			Status:              raw.status,
			IncompleteDetails:   raw.incompleteDetails,
			PreviousResponseID:  raw.previousResponseID,
			Store:               raw.store,
			ServiceTier:         raw.serviceTier,
			Truncation:          raw.truncation,
			Reasoning:           raw.reasoning,
			InputTokensDetails:  inputTokensDetails,
			OutputTokensDetails: outputTokensDetails,
		},
		Extra: raw.extra,
	}, nil
}

// Decodes an OpenAI Responses request into the normalized request
func (*OpenAIResponsesCodec) Decode(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
	obj, ok := request.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("request content is not an object")
	}
	input, ok := obj["input"]
	if !ok {
		return nil, types.InvalidArgumentf("OpenAI Responses request is missing input")
	}
	installationID, _ := getString(obj["client_metadata"], "x-codex-installation-id")
	codexDialect := installationID != ""
	var messages []types.Message
	if text, ok := input.(string); ok {
		messages = []types.Message{types.MessageUser{Content: types.MessageContentText(text)}}
	} else {
		items, ok := input.([]any)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses input must be a string or an array")
		}
		messages = make([]types.Message, 0, len(items))
		for _, item := range items {
			message, err := decodeResponsesInputItem(item, codexDialect)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
		}
	}
	var instructions types.MessageContent
	switch value := obj["instructions"].(type) {
	case nil:
	case string:
		instructions = types.MessageContentText(value)
	default:
		return nil, types.InvalidArgumentf("OpenAI Responses instructions must be a string or null")
	}
	const surface = "OpenAI Responses"
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
	maxTokens, err := optionalU64(obj, "max_output_tokens", surface)
	if err != nil {
		return nil, err
	}
	var params *types.GenerationParams
	if temperature != nil || maxTokens != nil || topP != nil {
		params = &types.GenerationParams{Temperature: temperature, MaxTokens: maxTokens, TopP: topP}
	}
	var tools []types.ToolDefinition
	if value, ok := obj["tools"]; ok {
		items, ok := value.([]any)
		if !ok {
			return nil, types.InvalidArgumentf("OpenAI Responses tools must be an array")
		}
		tools = make([]types.ToolDefinition, 0, len(items))
		for _, item := range items {
			tool, err := decodeResponsesTool(item)
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
	}
	var toolChoice types.ToolChoice
	if value, ok := obj["tool_choice"]; ok {
		toolChoice = decodeOpenAIOrAnthropicToolChoice(value)
	}
	store, err := optionalBool(obj, "store", surface)
	if err != nil {
		return nil, err
	}
	previousResponseID, err := optionalString(obj, "previous_response_id", surface)
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
	parallelToolCalls, err := decodeOpenAIOrAnthropicParallelToolCalls(obj)
	if err != nil {
		return nil, err
	}
	maxToolCalls, err := optionalU64(obj, "max_tool_calls", surface)
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
	background, err := optionalBool(obj, "background", surface)
	if err != nil {
		return nil, err
	}
	stringFields := map[string]*string{}
	for _, key := range []string{"prompt_cache_key", "prompt_cache_retention", "safety_identifier"} {
		value, err := optionalString(obj, key, surface)
		if err != nil {
			return nil, err
		}
		stringFields[key] = value
	}
	jsonFields := map[string]any{}
	for _, field := range []struct {
		key   string
		array bool
	}{
		{"reasoning", false}, {"include", true}, {"metadata", false}, {"context_management", true},
		{"moderation", false}, {"prompt", false}, {"prompt_cache_options", false},
		{"stream_options", false}, {"text", false},
	} {
		read := optionalObject
		if field.array {
			read = optionalArray
		}
		value, err := read(obj, field.key, surface)
		if err != nil {
			return nil, err
		}
		jsonFields[field.key] = cloneValue(value)
	}
	truncation, truncationPresent := obj["truncation"]
	conversation, conversationPresent := obj["conversation"]
	return &types.AnnotatedLLMRequest{
		Messages:           messages,
		Instructions:       instructions,
		Model:              model,
		Params:             params,
		Tools:              tools,
		ToolChoice:         toolChoice,
		Store:              store,
		PreviousResponseID: previousResponseID,
		Truncation:         cloneValue(truncation),
		TruncationPresent:  truncationPresent,
		Reasoning:          jsonFields["reasoning"],
		Include:            jsonFields["include"],
		User:               user,
		Metadata:           jsonFields["metadata"],
		ServiceTier:        serviceTier,
		ParallelToolCalls:  parallelToolCalls,
		MaxOutputTokens:    maxTokens,
		MaxToolCalls:       maxToolCalls,
		TopLogprobs:        topLogprobs,
		Stream:             stream,
		APISpecific: types.APISpecificRequestOpenAIResponses{
			Background:           background,
			ContextManagement:    jsonFields["context_management"],
			Conversation:         cloneValue(conversation),
			ConversationPresent:  conversationPresent,
			Moderation:           jsonFields["moderation"],
			Prompt:               jsonFields["prompt"],
			PromptCacheKey:       stringFields["prompt_cache_key"],
			PromptCacheOptions:   jsonFields["prompt_cache_options"],
			PromptCacheRetention: stringFields["prompt_cache_retention"],
			SafetyIdentifier:     stringFields["safety_identifier"],
			StreamOptions:        jsonFields["stream_options"],
			Text:                 jsonFields["text"],
		},
		Extra: objectWithoutList(obj, responsesModeledRequestKeys),
	}, nil
}

// Re-decodes the original request as the baseline and merges the edits into it
func (c *OpenAIResponsesCodec) Encode(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest) (*types.LLMRequest, error) {
	baseline, err := c.Decode(original)
	if err != nil {
		return nil, err
	}
	return c.EncodeWithBaseline(annotated, original, baseline)
}

// Merges the edits between baseline and annotated into a copy of the original request
func (*OpenAIResponsesCodec) EncodeWithBaseline(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest, baseline *types.AnnotatedLLMRequest) (*types.LLMRequest, error) {
	out := original.Clone()
	obj, ok := out.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("original content is not an object")
	}
	if err := patchResponsesMessages(obj, annotated, baseline); err != nil {
		return nil, err
	}
	if err := patchResponsesModelAndParams(obj, annotated, baseline); err != nil {
		return nil, err
	}
	if err := patchResponsesTools(obj, annotated, baseline); err != nil {
		return nil, err
	}
	patchResponsesCommonFields(obj, annotated, baseline)
	if err := patchResponsesAPISpecific(obj, annotated.APISpecific, baseline.APISpecific); err != nil {
		return nil, err
	}
	patchExtraFields(obj, baseline.Extra, annotated.Extra)
	return &out, nil
}

// OpenAIResponsesStreamingCodec assembles streamed Responses events into one response
type OpenAIResponsesStreamingCodec struct {
	state *responsesStreamingShared
}

// Streaming state shared by the codec, its collectors and its validating stream
type responsesStreamingShared struct {
	mu    sync.Mutex
	state openAIResponsesStreamingState
}

// Returns an OpenAI Responses streaming codec with empty state
func NewOpenAIResponsesStreamingCodec() *OpenAIResponsesStreamingCodec {
	return &OpenAIResponsesStreamingCodec{state: &responsesStreamingShared{}}
}

// Wraps a provider stream so it fails when it ends before a terminal response event
func (c *OpenAIResponsesStreamingCodec) ValidateStream(stream types.LLMJSONStream) types.LLMJSONStream {
	return &responsesCompletionStream{inner: stream, shared: c.state}
}

// Returns a collector that folds each event into the shared state
func (c *OpenAIResponsesStreamingCodec) Collector() types.LLMCollectorFn {
	return func(event any) error {
		c.state.mu.Lock()
		defer c.state.mu.Unlock()
		c.state.state.observe(event)
		return nil
	}
}

// Returns a collector that reads each event without taking ownership
func (c *OpenAIResponsesStreamingCodec) BorrowedCollector() types.LLMBorrowedCollectorFn {
	return func(event any) error {
		c.state.mu.Lock()
		defer c.state.mu.Unlock()
		c.state.state.observe(event)
		return nil
	}
}

// Returns a finalizer that emits the assembled response and resets the state
func (c *OpenAIResponsesStreamingCodec) Finalizer() types.LLMFinalizerFn {
	return func() any {
		c.state.mu.Lock()
		state := c.state.state
		c.state.state = openAIResponsesStreamingState{}
		c.state.mu.Unlock()
		return state.finalize()
	}
}

// Provider stream wrapper that reports a missing terminal response event
type responsesCompletionStream struct {
	inner  types.LLMJSONStream
	shared *responsesStreamingShared
	mu     sync.Mutex
	ended  bool
}

// Yields the next event, failing once when the stream ends without a terminal response
func (s *responsesCompletionStream) Next(ctx context.Context) (any, error) {
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended {
		return nil, io.EOF
	}
	event, err := s.inner.Next(ctx)
	if err == nil {
		return event, nil
	}
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	if !errors.Is(err, io.EOF) {
		return nil, err
	}
	s.shared.mu.Lock()
	completed := s.shared.state.terminalResponseReceived
	s.shared.mu.Unlock()
	if completed {
		return nil, io.EOF
	}
	logging.Warn("nemo_relay.runtime", "Responses stream ended before its final response event", map[string]any{
		"event":  "llm_stream_incomplete",
		"reason": "missing_terminal_response",
	})
	return nil, types.Internalf("stream ended before its final response event")
}

// Marks the stream ended and releases the inner stream lifecycle guards
func (s *responsesCompletionStream) Terminalize() {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	if terminalizer, ok := s.inner.(types.LLMStreamTerminalizer); ok {
		terminalizer.Terminalize()
	}
}

// Marks the stream ended and closes the inner stream
func (s *responsesCompletionStream) Close(ctx context.Context) error {
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
	return s.inner.Close(ctx)
}

// Accumulated Responses stream state
type openAIResponsesStreamingState struct {
	response                 map[string]any
	terminalResponseReceived bool
	hasTerminalOutput        bool
	items                    map[uint64]any
}

// Folds one stream event into the state
func (s *openAIResponsesStreamingState) observe(event any) {
	kind, _ := getString(event, "type")
	switch kind {
	case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete":
		s.observeResponseSnapshot(event)
	case "response.output_item.added", "response.output_item.done":
		s.observeOutputItem(event)
	}
}

// Records a response snapshot until a terminal snapshot arrives
func (s *openAIResponsesStreamingState) observeResponseSnapshot(event any) {
	if s.terminalResponseReceived {
		return
	}
	value, _ := getValue(event, "response")
	response, ok := value.(map[string]any)
	if !ok {
		return
	}
	s.terminalResponseReceived = isTerminalResponseEvent(event)
	output, _ := response["output"].([]any)
	s.hasTerminalOutput = s.terminalResponseReceived && len(output) > 0
	if s.hasTerminalOutput {
		s.items = nil
	}
	s.response = cloneObject(response)
}

// Records an output item by its index until a terminal snapshot carries the output
func (s *openAIResponsesStreamingState) observeOutputItem(event any) {
	if s.hasTerminalOutput {
		return
	}
	index, ok := getU64(event, "output_index")
	if !ok {
		return
	}
	item, ok := getValue(event, "item")
	if !ok {
		return
	}
	if s.items == nil {
		s.items = map[uint64]any{}
	}
	s.items[index] = cloneValue(item)
}

// Builds the assembled response, filling output from items when the snapshot has none
func (s *openAIResponsesStreamingState) finalize() any {
	output := s.response
	if output == nil {
		output = map[string]any{}
	}
	snapshotOutput, _ := output["output"].([]any)
	if len(snapshotOutput) == 0 && len(s.items) > 0 {
		items := make([]any, 0, len(s.items))
		for _, index := range sortedU64Keys(s.items) {
			items = append(items, s.items[index])
		}
		output["output"] = items
	}
	return output
}

// Reports whether an event is a terminal response event carrying a response object
func isTerminalResponseEvent(event any) bool {
	kind, _ := getString(event, "type")
	if kind != "response.completed" && kind != "response.failed" && kind != "response.incomplete" {
		return false
	}
	response, _ := getValue(event, "response")
	_, ok := response.(map[string]any)
	return ok
}

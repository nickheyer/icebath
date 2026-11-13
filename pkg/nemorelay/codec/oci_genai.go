package codec

import (
	"fmt"
	"strings"
	"sync"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// OCIGenAIChatCodec translates OCI Generative AI chat requests and responses
type OCIGenAIChatCodec struct{}

// Returns the OCI Generative AI chat codec
func NewOCIGenAIChatCodec() *OCIGenAIChatCodec {
	return &OCIGenAIChatCodec{}
}

var ociGenAIProviderSurface = providerSurfaceDescriptor{
	surface: ProviderSurfaceOCIGenAI,
	detectRequest: func(obj map[string]any, hint *string) bool {
		_, hasChatRequest := obj["chatRequest"].(map[string]any)
		_, hasServingMode := obj["servingMode"]
		_, hasCompartment := obj["compartmentId"]
		_, hasAPIFormat := obj["apiFormat"]
		hinted := hint != nil && (*hint == "oci" || *hint == "oci.genai")
		return (hasChatRequest && (hasServingMode || hasCompartment)) || hasAPIFormat || (hinted && hasChatRequest)
	},
	detectResponse: func(obj map[string]any) bool {
		if chatResponse, ok := obj["chatResponse"].(map[string]any); ok {
			_, has := chatResponse["apiFormat"]
			return has
		}
		_, has := obj["apiFormat"]
		return has
	},
	decodeRequest: func(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
		return NewOCIGenAIChatCodec().Decode(request)
	},
	decodeResponse: func(response any) (*types.AnnotatedLLMResponse, error) {
		return NewOCIGenAIChatCodec().DecodeResponse(response)
	},
	codecName:      "oci_genai",
	requestCodec:   func() types.LLMCodec { return NewOCIGenAIChatCodec() },
	responseCodec:  func() types.LLMResponseCodec { return NewOCIGenAIChatCodec() },
	streamingCodec: func() StreamingCodec { return NewOCIGenAIStreamingCodec() },
}

var ociModeledGenericRequestKeys = []string{
	"apiFormat", "messages", "maxTokens", "temperature", "topP", "stop", "tools", "toolChoice",
}

var ociModeledCohereRequestKeys = []string{
	"apiFormat", "message", "chatHistory", "preambleOverride", "maxTokens", "temperature", "topP",
	"stopSequences", "tools", "toolChoice",
}

var ociModeledCohereV2RequestKeys = []string{
	"apiFormat", "messages", "maxTokens", "temperature", "topP", "stopSequences", "tools", "toolChoice",
}

var ociModeledChoiceKeys = []string{"message", "finishReason", "index"}

var ociModeledMessageKeys = []string{"role", "content", "toolCalls"}

// Identifies the builtin OCI Generative AI codec
func (*OCIGenAIChatCodec) CodecIdentity() types.LLMCodecIdentity {
	return types.LLMCodecIdentity{Kind: types.LLMCodecIdentityBuiltIn, Builtin: types.BuiltinLLMCodecOCIGenAI}
}

// Lowercases text with full Unicode case mapping
func unicodeLower(s string) string {
	return cases.Lower(language.Und).String(s)
}

// Uppercases text with full Unicode case mapping
func unicodeUpper(s string) string {
	return cases.Upper(language.Und).String(s)
}

// Reads an optional string array field with deserialization errors
func ociOptionalStringList(obj map[string]any, key, surface string) ([]string, error) {
	value := obj[key]
	if value == nil {
		return nil, nil
	}
	list, err := serdeStrings(value)
	if err != nil {
		return nil, types.InvalidArgumentf("%s %s must be a string array: %v", surface, key, err)
	}
	return list, nil
}

// Maps an OCI finish reason to the normalized finish reason
func mapOCIFinishReason(reason string) types.FinishReason {
	switch reason {
	case "stop", "COMPLETE", "STOP_SEQUENCE":
		return types.FinishReason{Kind: types.FinishReasonComplete}
	case "length", "max_tokens", "MAX_TOKENS":
		return types.FinishReason{Kind: types.FinishReasonLength}
	case "tool_calls", "TOOL_CALL":
		return types.FinishReason{Kind: types.FinishReasonToolUse}
	case "content_filter":
		return types.FinishReason{Kind: types.FinishReasonContentFilter}
	}
	return types.FinishReason{Kind: types.FinishReasonUnknown, Unknown: reason}
}

// Wraps a raw OCI component as provider native, naming it after its type field
func ociNativeComponent(value any) types.ProviderNativeComponent {
	return anthropicNativeComponent("oci_genai", value)
}

// Decodes GENERIC message content, flattening all-text part lists into plain text
func decodeOCIGenericContent(value any, present bool) (types.MessageContent, error) {
	if !present || value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return types.MessageContentText(text), nil
	}
	parts, ok := value.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("OCI GenAI GENERIC message content must be a string, an array, or null")
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if text, ok := ociFlattenAllTextParts(parts); ok {
		return types.MessageContentText(text), nil
	}
	out := make(types.MessageContentParts, 0, len(parts))
	for _, part := range parts {
		decoded, err := decodeOCIGenericContentPart(part)
		if err != nil {
			return nil, err
		}
		out = append(out, decoded)
	}
	return out, nil
}

// Joins the text of a part list when every part is a TEXT part
func ociFlattenAllTextParts(parts []any) (string, bool) {
	var text strings.Builder
	for _, part := range parts {
		obj, ok := part.(map[string]any)
		if !ok {
			return "", false
		}
		if kind, _ := obj["type"].(string); kind != "TEXT" {
			return "", false
		}
		switch partText := obj["text"].(type) {
		case nil:
		case string:
			text.WriteString(partText)
		default:
			return "", false
		}
	}
	return text.String(), true
}

// Decodes one GENERIC content part, keeping non-text parts native
func decodeOCIGenericContentPart(value any) (types.ContentPart, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OCI GenAI GENERIC content part must be an object")
	}
	if kind, _ := obj["type"].(string); kind == "TEXT" {
		raw, present := obj["text"]
		text, isString := raw.(string)
		if !present || isString {
			return types.ContentPartText{Text: text, Extra: objectWithout(obj, "type", "text")}, nil
		}
	}
	native := ociNativeComponent(value)
	return types.ContentPartProviderNative{Provider: native.Provider, Kind: native.Kind, Value: native.Value}, nil
}

// Encodes message content into GENERIC content parts
func encodeOCIGenericContent(content types.MessageContent) (any, error) {
	switch c := content.(type) {
	case types.MessageContentText:
		return []any{map[string]any{"type": "TEXT", "text": string(c)}}, nil
	case types.MessageContentParts:
		out := make([]any, 0, len(c))
		for _, part := range c {
			encoded, err := encodeOCIGenericContentPart(part)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		}
		return out, nil
	}
	return nil, types.InvalidArgumentf("OCI GenAI GENERIC message content must be a string, an array, or null")
}

// Encodes one content part into a GENERIC content part
func encodeOCIGenericContentPart(part types.ContentPart) (any, error) {
	switch p := part.(type) {
	case types.ContentPartText:
		obj := cloneObject(p.Extra)
		obj["type"] = "TEXT"
		obj["text"] = p.Text
		return obj, nil
	case types.ContentPartProviderNative:
		if p.Provider == "oci_genai" {
			return cloneValue(p.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("content part %s cannot be encoded for OCI GenAI", types.RustDebug(part))
}

// Decodes one request tool call from flat or nested function shape
func decodeOCIToolCall(value any) (types.ToolCall, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return types.ToolCall{}, types.InvalidArgumentf("OCI GenAI toolCalls entry must be an object")
	}
	body := obj
	if function, ok := obj["function"].(map[string]any); ok {
		body = function
	}
	id, _ := obj["id"].(string)
	name, _ := body["name"].(string)
	arguments := ""
	if raw, ok := body["arguments"]; ok {
		if text, isString := raw.(string); isString {
			arguments = text
		} else {
			encoded, err := types.EncodeJSONString(raw)
			if err != nil {
				return types.ToolCall{}, types.Internalf("%v", err)
			}
			arguments = encoded
		}
	}
	return types.ToolCall{ID: id, CallType: "function", Function: types.FunctionCall{Name: name, Arguments: arguments}}, nil
}

// Encodes a tool call in the nested function shape used by COHEREV2
func encodeOCIToolCallNested(toolCall types.ToolCall) any {
	obj := map[string]any{
		"type":     "FUNCTION",
		"function": map[string]any{"name": toolCall.Function.Name, "arguments": toolCall.Function.Arguments},
	}
	if toolCall.ID != "" {
		obj["id"] = toolCall.ID
	}
	return obj
}

// Encodes a tool call in the flat shape used by GENERIC
func encodeOCIToolCall(toolCall types.ToolCall) any {
	obj := map[string]any{"type": "FUNCTION", "name": toolCall.Function.Name, "arguments": toolCall.Function.Arguments}
	if toolCall.ID != "" {
		obj["id"] = toolCall.ID
	}
	return obj
}

// Wraps a raw OCI message as a provider native message
func ociProviderNativeMessage(kind string, value any) types.Message {
	return types.MessageProviderNative{Provider: "oci_genai", Kind: kind, Value: cloneValue(value)}
}

// Decodes one GENERIC message, keeping incomplete or unknown roles native
func decodeOCIGenericMessage(value any) (types.Message, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OCI GenAI GENERIC message must be an object")
	}
	role, ok := obj["role"].(string)
	if !ok {
		role = "USER"
	}
	role = unicodeLower(role)
	rawContent, contentPresent := obj["content"]
	content, err := decodeOCIGenericContent(rawContent, contentPresent)
	if err != nil {
		return nil, err
	}
	var toolCalls []types.ToolCall
	switch calls := obj["toolCalls"].(type) {
	case nil:
	case []any:
		toolCalls = make([]types.ToolCall, 0, len(calls))
		for _, call := range calls {
			decoded, err := decodeOCIToolCall(call)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, decoded)
		}
	default:
		return nil, types.InvalidArgumentf("OCI GenAI GENERIC toolCalls must be an array")
	}
	toolCallID, hasToolCallID := obj["toolCallId"].(string)
	switch role {
	case "system":
		if content != nil {
			return types.MessageSystem{Content: content}, nil
		}
	case "user":
		if content != nil {
			return types.MessageUser{Content: content}, nil
		}
	case "assistant":
		return types.MessageAssistant{Content: content, ToolCalls: toolCalls}, nil
	case "tool":
		if content != nil && hasToolCallID {
			return types.MessageTool{Content: content, ToolCallID: toolCallID}, nil
		}
	}
	return ociProviderNativeMessage(role, value), nil
}

// Encodes one normalized message into a GENERIC or COHEREV2 message
func encodeOCIGenericMessage(message types.Message, apiFormat string) (any, error) {
	obj := map[string]any{}
	switch m := message.(type) {
	case types.MessageSystem:
		content, err := encodeOCIGenericContent(m.Content)
		if err != nil {
			return nil, err
		}
		obj["role"] = "SYSTEM"
		obj["content"] = content
	case types.MessageUser:
		content, err := encodeOCIGenericContent(m.Content)
		if err != nil {
			return nil, err
		}
		obj["role"] = "USER"
		obj["content"] = content
	case types.MessageAssistant:
		obj["role"] = "ASSISTANT"
		var content any = []any{}
		if m.Content != nil {
			encoded, err := encodeOCIGenericContent(m.Content)
			if err != nil {
				return nil, err
			}
			content = encoded
		}
		obj["content"] = content
		if m.ToolCalls != nil {
			encode := encodeOCIToolCall
			if apiFormat == "COHEREV2" {
				encode = encodeOCIToolCallNested
			}
			calls := make([]any, 0, len(m.ToolCalls))
			for _, call := range m.ToolCalls {
				calls = append(calls, encode(call))
			}
			obj["toolCalls"] = calls
		}
	case types.MessageTool:
		content, err := encodeOCIGenericContent(m.Content)
		if err != nil {
			return nil, err
		}
		obj["role"] = "TOOL"
		obj["content"] = content
		obj["toolCallId"] = m.ToolCallID
	case types.MessageProviderNative:
		if m.Provider == "oci_genai" {
			return cloneValue(m.Value), nil
		}
		return nil, types.InvalidArgumentf("message %s cannot be encoded for OCI GenAI", types.RustDebug(message))
	default:
		return nil, types.InvalidArgumentf("message %s cannot be encoded for OCI GenAI", types.RustDebug(message))
	}
	return obj, nil
}

// Writes edited GENERIC or COHEREV2 messages over the original message list
func patchOCIGenericMessages(chatRequest map[string]any, edited, baseline []types.Message, apiFormat string) error {
	raw, _ := chatRequest["messages"].([]any)
	if raw == nil {
		raw = []any{}
	}
	patched, err := encodeChangedItems(edited, baseline, raw, true, func(message types.Message) (any, error) {
		return encodeOCIGenericMessage(message, apiFormat)
	})
	if err != nil {
		return err
	}
	chatRequest["messages"] = patched
	return nil
}

// Decodes the COHERE preamble, chat history and current message into messages
func decodeOCICohereMessages(chatRequest map[string]any) ([]types.Message, error) {
	messages := []types.Message{}
	if preamble, ok := chatRequest["preambleOverride"].(string); ok && preamble != "" {
		messages = append(messages, types.MessageSystem{Content: types.MessageContentText(preamble)})
	}
	if history, ok := chatRequest["chatHistory"]; ok {
		turns, ok := history.([]any)
		if !ok {
			return nil, types.InvalidArgumentf("OCI GenAI COHERE chatHistory must be an array")
		}
		for _, turn := range turns {
			message, err := decodeOCICohereTurn(turn)
			if err != nil {
				return nil, err
			}
			messages = append(messages, message)
		}
	}
	if current, ok := chatRequest["message"].(string); ok {
		messages = append(messages, types.MessageUser{Content: types.MessageContentText(current)})
	}
	return messages, nil
}

// Decodes one COHERE chat history turn, keeping turns without text native
func decodeOCICohereTurn(turn any) (types.Message, error) {
	obj, ok := turn.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("OCI GenAI COHERE chatHistory turn must be an object")
	}
	role, ok := obj["role"].(string)
	if !ok {
		role = "USER"
	}
	role = unicodeUpper(role)
	text, ok := obj["message"].(string)
	if !ok {
		return ociProviderNativeMessage(role, turn), nil
	}
	content := types.MessageContentText(text)
	switch role {
	case "USER":
		return types.MessageUser{Content: content}, nil
	case "CHATBOT":
		return types.MessageAssistant{Content: content}, nil
	case "SYSTEM":
		return types.MessageSystem{Content: content}, nil
	}
	return ociProviderNativeMessage(role, turn), nil
}

// Returns the plain text of content the COHERE format can carry
func ociCohereText(content types.MessageContent) (string, error) {
	if text, ok := content.(types.MessageContentText); ok {
		return string(text), nil
	}
	return "", types.InvalidArgumentf("multimodal content cannot be encoded for the OCI GenAI COHERE format")
}

// Rebuilds the COHERE preamble, chat history and current message from edited messages
func encodeOCICohereMessages(chatRequest map[string]any, messages, baseline []types.Message) error {
	remaining := messages
	baselineHistory := baseline
	if preamble, ok := chatRequest["preambleOverride"].(string); ok && preamble != "" && len(baselineHistory) > 0 {
		baselineHistory = baselineHistory[1:]
	}
	if _, ok := chatRequest["message"].(string); ok && len(baselineHistory) > 0 {
		baselineHistory = baselineHistory[:len(baselineHistory)-1]
	}
	if len(remaining) > 0 {
		if system, ok := remaining[0].(types.MessageSystem); ok {
			preamble, err := ociCohereText(system.Content)
			if err != nil {
				return err
			}
			chatRequest["preambleOverride"] = preamble
			remaining = remaining[1:]
		} else {
			delete(chatRequest, "preambleOverride")
		}
	} else {
		delete(chatRequest, "preambleOverride")
	}
	var last types.MessageUser
	isUser := false
	if len(remaining) > 0 {
		last, isUser = remaining[len(remaining)-1].(types.MessageUser)
	}
	if !isUser {
		return types.InvalidArgumentf("OCI GenAI COHERE requests require the last message to be a user message")
	}
	current, err := ociCohereText(last.Content)
	if err != nil {
		return err
	}
	remaining = remaining[:len(remaining)-1]
	original, hasOriginal := chatRequest["chatHistory"].([]any)
	history, err := encodeChangedItems(remaining, baselineHistory, original, hasOriginal, encodeOCICohereTurn)
	if err != nil {
		return err
	}
	chatRequest["message"] = current
	if _, has := chatRequest["chatHistory"]; len(history) > 0 || has {
		chatRequest["chatHistory"] = history
	}
	return nil
}

// Encodes one normalized message as a COHERE chat history turn
func encodeOCICohereTurn(message types.Message) (any, error) {
	var role string
	var content types.MessageContent
	switch m := message.(type) {
	case types.MessageUser:
		role, content = "USER", m.Content
	case types.MessageAssistant:
		if m.Content == nil {
			return nil, types.InvalidArgumentf("message %s cannot be encoded as an OCI GenAI COHERE chatHistory turn", types.RustDebug(message))
		}
		role, content = "CHATBOT", m.Content
	case types.MessageSystem:
		role, content = "SYSTEM", m.Content
	case types.MessageTool:
		return nil, types.InvalidArgumentf("OCI GenAI COHERE tool results cannot be rebuilt from a normalized tool message")
	case types.MessageProviderNative:
		if m.Provider == "oci_genai" {
			return cloneValue(m.Value), nil
		}
		return nil, types.InvalidArgumentf("message %s cannot be encoded as an OCI GenAI COHERE chatHistory turn", types.RustDebug(message))
	default:
		return nil, types.InvalidArgumentf("message %s cannot be encoded as an OCI GenAI COHERE chatHistory turn", types.RustDebug(message))
	}
	text, err := ociCohereText(content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"role": role, "message": text}, nil
}

// Returns the stop sequence key of an API format
func ociStopKey(apiFormat string) string {
	if strings.HasPrefix(apiFormat, "COHERE") {
		return "stopSequences"
	}
	return "stop"
}

// Decodes generation parameters of a chat request
func decodeOCIParams(chatRequest map[string]any, apiFormat string) (*types.GenerationParams, error) {
	const surface = "OCI GenAI"
	temperature, err := optionalF64(chatRequest, "temperature", surface)
	if err != nil {
		return nil, err
	}
	maxTokens, err := optionalU64(chatRequest, "maxTokens", surface)
	if err != nil {
		return nil, err
	}
	topP, err := optionalF64(chatRequest, "topP", surface)
	if err != nil {
		return nil, err
	}
	stop, err := ociOptionalStringList(chatRequest, ociStopKey(apiFormat), surface)
	if err != nil {
		return nil, err
	}
	if temperature == nil && maxTokens == nil && topP == nil && stop == nil {
		return nil, nil
	}
	return &types.GenerationParams{Temperature: temperature, MaxTokens: maxTokens, TopP: topP, Stop: stop}, nil
}

// Writes edited generation parameters into a chat request
func patchOCIParams(chatRequest map[string]any, edited, baseline *types.GenerationParams, apiFormat string) {
	if eqParams(edited, baseline) {
		return
	}
	for _, field := range []struct {
		key        string
		value, old *float64
	}{
		{"temperature", paramTemperature(edited), paramTemperature(baseline)},
		{"topP", paramTopP(edited), paramTopP(baseline)},
	} {
		if !eqF64Ptr(field.value, field.old) {
			value, present := optF64JSON(field.value)
			setOrRemoveJSON(chatRequest, field.key, value, present)
		}
	}
	maxTokens := paramMaxTokens(edited)
	if !eqU64Ptr(maxTokens, paramMaxTokens(baseline)) {
		value, present := optU64JSON(maxTokens)
		setOrRemoveJSON(chatRequest, "maxTokens", value, present)
	}
	stop := paramStop(edited)
	if !eqStrings(stop, paramStop(baseline)) {
		value, present := optStringsJSON(stop)
		setOrRemoveJSON(chatRequest, ociStopKey(apiFormat), value, present)
	}
}

// Decodes request tools, keeping every OCI tool native
func decodeOCITools(chatRequest map[string]any) ([]types.ToolDefinition, error) {
	switch tools := chatRequest["tools"].(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]types.ToolDefinition, 0, len(tools))
		for _, tool := range tools {
			native := ociNativeComponent(tool)
			out = append(out, types.ToolDefinitionProviderNative{Provider: native.Provider, Kind: native.Kind, Value: native.Value})
		}
		return out, nil
	}
	return nil, types.InvalidArgumentf("OCI GenAI tools must be an array")
}

// Encodes one normalized tool into an OCI function tool
func encodeOCITool(tool types.ToolDefinition) (any, error) {
	switch t := tool.(type) {
	case types.ToolDefinitionProviderNative:
		if t.Provider == "oci_genai" {
			return cloneValue(t.Value), nil
		}
	case types.ToolDefinitionFunction:
		obj := cloneObject(t.Extra)
		obj["type"] = "FUNCTION"
		obj["name"] = t.Function.Name
		if t.Function.Description != nil {
			obj["description"] = *t.Function.Description
		}
		if t.Function.ParametersPresent || t.Function.Parameters != nil {
			obj["parameters"] = cloneValue(t.Function.Parameters)
		}
		for key, value := range t.Function.Extra {
			obj[key] = cloneValue(value)
		}
		return obj, nil
	}
	return nil, types.InvalidArgumentf("tool %s cannot be encoded for OCI GenAI", types.RustDebug(tool))
}

// Encodes a tool choice, accepting only OCI native choices
func encodeOCIToolChoice(choice types.ToolChoice) (any, error) {
	if native, ok := choice.(types.ToolChoiceProviderNative); ok && native.Provider == "oci_genai" {
		return cloneValue(native.Value), nil
	}
	return nil, types.InvalidArgumentf("tool choice %s cannot be encoded for OCI GenAI", types.RustDebug(choice))
}

// Returns the model or endpoint id of the serving mode in a ChatDetails envelope
func ociModelFromEnvelope(envelope map[string]any) *string {
	servingMode, ok := envelope["servingMode"].(map[string]any)
	if !ok {
		return nil
	}
	value, ok := servingMode["modelId"]
	if !ok {
		value = servingMode["endpointId"]
	}
	if text, ok := value.(string); ok {
		return &text
	}
	return nil
}

// Splits a ChatDetails envelope from its chat request when one is present
func ociSplitEnvelope(obj map[string]any) (map[string]any, map[string]any) {
	if chatRequest, ok := obj["chatRequest"].(map[string]any); ok {
		return obj, chatRequest
	}
	return nil, obj
}

// Returns the uppercased API format of a chat request or response
func ociAPIFormat(chat map[string]any) string {
	format, ok := chat["apiFormat"].(string)
	if !ok {
		format = "GENERIC"
	}
	return unicodeUpper(format)
}

// Rejects edits to fields OCI Generative AI cannot carry
func validateOCISupportedFields(annotated, baseline *types.AnnotatedLLMRequest) error {
	unsupported := !eqStrPtr(annotated.Model, baseline.Model) ||
		!types.Equal(annotated.Instructions, baseline.Instructions) ||
		!eqBoolPtr(annotated.Store, baseline.Store) ||
		!eqStrPtr(annotated.PreviousResponseID, baseline.PreviousResponseID) ||
		!eqOptJSON(annotated.Truncation, annotated.TruncationPresent, baseline.Truncation, baseline.TruncationPresent) ||
		!eqOptJSON(annotated.Reasoning, annotated.ReasoningPresent, baseline.Reasoning, baseline.ReasoningPresent) ||
		!eqOptJSON(annotated.Include, annotated.IncludePresent, baseline.Include, baseline.IncludePresent) ||
		!eqStrPtr(annotated.User, baseline.User) ||
		!eqOptJSON(annotated.Metadata, annotated.MetadataPresent, baseline.Metadata, baseline.MetadataPresent) ||
		!eqStrPtr(annotated.ServiceTier, baseline.ServiceTier) ||
		!eqBoolPtr(annotated.ParallelToolCalls, baseline.ParallelToolCalls) ||
		!eqU64Ptr(annotated.MaxOutputTokens, baseline.MaxOutputTokens) ||
		!eqU64Ptr(annotated.MaxToolCalls, baseline.MaxToolCalls) ||
		!eqU64Ptr(annotated.TopLogprobs, baseline.TopLogprobs) ||
		!eqBoolPtr(annotated.Stream, baseline.Stream)
	if unsupported {
		return types.InvalidArgumentf("request contains fields that cannot be encoded for OCI GenAI")
	}
	return nil
}

// Writes edited compartment and serving mode into the ChatDetails envelope
func patchOCIAPISpecific(envelope map[string]any, edited, baseline types.APISpecificRequest) error {
	if edited == nil {
		return nil
	}
	e, editedIsOCI := edited.(types.APISpecificRequestOCIGenAI)
	b, baselineIsOCI := baseline.(types.APISpecificRequestOCIGenAI)
	if !editedIsOCI || !baselineIsOCI {
		return types.InvalidArgumentf("api_specific provider does not match OCI GenAI")
	}
	if !eqStrPtr(e.APIFormat, b.APIFormat) {
		return types.InvalidArgumentf("the OCI GenAI api_format cannot be edited")
	}
	compartmentChanged := !eqStrPtr(e.CompartmentID, b.CompartmentID)
	servingModeChanged := !eqOptJSON(e.ServingMode, e.ServingModePresent, b.ServingMode, b.ServingModePresent)
	if !compartmentChanged && !servingModeChanged {
		return nil
	}
	if envelope == nil {
		return types.InvalidArgumentf("compartmentId and servingMode edits require a ChatDetails envelope")
	}
	if compartmentChanged {
		value, present := optStringJSON(e.CompartmentID)
		setOrRemoveJSON(envelope, "compartmentId", value, present)
	}
	if servingModeChanged {
		setOrRemoveJSON(envelope, "servingMode", cloneValue(e.ServingMode), e.ServingModePresent || e.ServingMode != nil)
	}
	return nil
}

// Decodes an OCI chat request in GENERIC, COHERE or COHEREV2 format
func (*OCIGenAIChatCodec) Decode(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
	obj, ok := request.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("request content is not an object")
	}
	envelope, chatRequest := ociSplitEnvelope(obj)
	apiFormat := ociAPIFormat(chatRequest)
	var messages []types.Message
	if apiFormat == "COHERE" {
		decoded, err := decodeOCICohereMessages(chatRequest)
		if err != nil {
			return nil, err
		}
		messages = decoded
	} else {
		switch values := chatRequest["messages"].(type) {
		case nil:
			messages = []types.Message{}
		case []any:
			messages = make([]types.Message, 0, len(values))
			for _, value := range values {
				message, err := decodeOCIGenericMessage(value)
				if err != nil {
					return nil, err
				}
				messages = append(messages, message)
			}
		default:
			return nil, types.InvalidArgumentf("OCI GenAI GENERIC messages must be an array")
		}
	}
	params, err := decodeOCIParams(chatRequest, apiFormat)
	if err != nil {
		return nil, err
	}
	tools, err := decodeOCITools(chatRequest)
	if err != nil {
		return nil, err
	}
	var toolChoice types.ToolChoice
	if value := chatRequest["toolChoice"]; value != nil {
		toolChoice = types.ToolChoiceProviderNative(ociNativeComponent(value))
	}
	modeled := ociModeledGenericRequestKeys
	switch apiFormat {
	case "COHERE":
		modeled = ociModeledCohereRequestKeys
	case "COHEREV2":
		modeled = ociModeledCohereV2RequestKeys
	}
	apiSpecific := types.APISpecificRequestOCIGenAI{APIFormat: strPtr(apiFormat)}
	var model *string
	if envelope != nil {
		model = ociModelFromEnvelope(envelope)
		if compartment, ok := envelope["compartmentId"].(string); ok {
			apiSpecific.CompartmentID = &compartment
		}
		if servingMode, ok := envelope["servingMode"]; ok {
			apiSpecific.ServingMode = cloneValue(servingMode)
			apiSpecific.ServingModePresent = true
		}
	}
	return &types.AnnotatedLLMRequest{
		Messages:    messages,
		Model:       model,
		Params:      params,
		Tools:       tools,
		ToolChoice:  toolChoice,
		APISpecific: apiSpecific,
		Extra:       objectWithoutList(chatRequest, modeled),
	}, nil
}

// Re-decodes the original request as the baseline and merges the edits into it
func (c *OCIGenAIChatCodec) Encode(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest) (*types.LLMRequest, error) {
	baseline, err := c.Decode(original)
	if err != nil {
		return nil, err
	}
	return c.EncodeWithBaseline(annotated, original, baseline)
}

// Merges the edits between baseline and annotated into a copy of the original request
func (*OCIGenAIChatCodec) EncodeWithBaseline(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest, baseline *types.AnnotatedLLMRequest) (*types.LLMRequest, error) {
	out := original.Clone()
	obj, ok := out.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("original content is not an object")
	}
	chatRequest, isEnvelope := obj["chatRequest"].(map[string]any)
	if !isEnvelope {
		chatRequest = obj
	}
	apiFormat := ociAPIFormat(chatRequest)
	if err := validateOCISupportedFields(annotated, baseline); err != nil {
		return nil, err
	}
	if !eqSlice(annotated.Messages, baseline.Messages) {
		if apiFormat == "COHERE" {
			if err := encodeOCICohereMessages(chatRequest, annotated.Messages, baseline.Messages); err != nil {
				return nil, err
			}
		} else if err := patchOCIGenericMessages(chatRequest, annotated.Messages, baseline.Messages, apiFormat); err != nil {
			return nil, err
		}
	}
	patchOCIParams(chatRequest, annotated.Params, baseline.Params, apiFormat)
	if !eqOptSlice(annotated.Tools, baseline.Tools) {
		if annotated.Tools == nil {
			delete(chatRequest, "tools")
		} else {
			original, hasOriginal := chatRequest["tools"].([]any)
			tools, err := encodeChangedItems(annotated.Tools, baseline.Tools, original, hasOriginal, encodeOCITool)
			if err != nil {
				return nil, err
			}
			chatRequest["tools"] = tools
		}
	}
	if !types.Equal(annotated.ToolChoice, baseline.ToolChoice) {
		if annotated.ToolChoice == nil {
			delete(chatRequest, "toolChoice")
		} else {
			choice, err := encodeOCIToolChoice(annotated.ToolChoice)
			if err != nil {
				return nil, err
			}
			chatRequest["toolChoice"] = choice
		}
	}
	patchExtraFields(chatRequest, baseline.Extra, annotated.Extra)
	var envelope map[string]any
	if isEnvelope {
		envelope = obj
	}
	if err := patchOCIAPISpecific(envelope, annotated.APISpecific, baseline.APISpecific); err != nil {
		return nil, err
	}
	return &out, nil
}

// Copies the entries of an object whose keys are not modeled
func ociUnmodeledFields(obj map[string]any, modeled []string) map[string]any {
	return objectWithoutList(obj, modeled)
}

// Stores the unmodeled fields of a nested object under a key when any exist
func ociNestUnmodeledFields(extra map[string]any, key string, obj map[string]any, modeled []string) {
	if unmodeled := ociUnmodeledFields(obj, modeled); len(unmodeled) > 0 {
		extra[key] = unmodeled
	}
}

// Decoded parts of an OCI response body
type ociResponseBody struct {
	message      types.MessageContent
	toolCalls    []types.ResponseToolCall
	finishReason *string
	nestedExtra  map[string]any
}

// Decodes the first choice of a GENERIC response body
func decodeOCIGenericResponseBody(chatResponse map[string]any) (ociResponseBody, error) {
	body := ociResponseBody{nestedExtra: map[string]any{}}
	choices, _ := chatResponse["choices"].([]any)
	if len(choices) == 0 {
		return body, nil
	}
	firstChoice, ok := choices[0].(map[string]any)
	if !ok {
		return body, nil
	}
	ociNestUnmodeledFields(body.nestedExtra, "choice", firstChoice, ociModeledChoiceKeys)
	if reason, ok := firstChoice["finishReason"].(string); ok {
		body.finishReason = &reason
	}
	rawMessage, ok := firstChoice["message"].(map[string]any)
	if !ok {
		return body, nil
	}
	return decodeOCIResponseMessage(body, rawMessage)
}

// Decodes the message content and tool calls of a response message object
func decodeOCIResponseMessage(body ociResponseBody, rawMessage map[string]any) (ociResponseBody, error) {
	ociNestUnmodeledFields(body.nestedExtra, "message", rawMessage, ociModeledMessageKeys)
	rawContent, present := rawMessage["content"]
	message, err := decodeOCIGenericContent(rawContent, present)
	if err != nil {
		return body, err
	}
	body.message = message
	if calls, ok := rawMessage["toolCalls"].([]any); ok {
		if decoded := decodeOCIResponseToolCalls(calls); len(decoded) > 0 {
			body.toolCalls = decoded
		}
	}
	return body, nil
}

// Decodes a COHERE response body
func decodeOCICohereResponseBody(chatResponse map[string]any) ociResponseBody {
	body := ociResponseBody{nestedExtra: map[string]any{}}
	if text, ok := chatResponse["text"].(string); ok {
		body.message = types.MessageContentText(text)
	}
	if calls, ok := chatResponse["toolCalls"].([]any); ok {
		if decoded := decodeOCIResponseToolCalls(calls); len(decoded) > 0 {
			body.toolCalls = decoded
		}
	}
	if reason, ok := chatResponse["finishReason"].(string); ok {
		body.finishReason = &reason
	}
	return body
}

// Decodes a COHEREV2 response body
func decodeOCICohereV2ResponseBody(chatResponse map[string]any) (ociResponseBody, error) {
	body := ociResponseBody{nestedExtra: map[string]any{}}
	if reason, ok := chatResponse["finishReason"].(string); ok {
		body.finishReason = &reason
	}
	rawMessage, ok := chatResponse["message"].(map[string]any)
	if !ok {
		return body, nil
	}
	return decodeOCIResponseMessage(body, rawMessage)
}

// Decodes response tool calls, numbering calls without ids by position
func decodeOCIResponseToolCalls(calls []any) []types.ResponseToolCall {
	out := []types.ResponseToolCall{}
	for index, call := range calls {
		obj, ok := call.(map[string]any)
		if !ok {
			continue
		}
		body := obj
		if function, ok := obj["function"].(map[string]any); ok {
			body = function
		}
		name, ok := body["name"].(string)
		if !ok {
			continue
		}
		var arguments any
		if raw, ok := body["arguments"]; ok {
			if text, isString := raw.(string); isString {
				arguments = parseArguments(text)
			} else {
				arguments = cloneValue(raw)
			}
		} else {
			arguments = cloneValue(body["parameters"])
		}
		id, ok := obj["id"].(string)
		if !ok {
			id = fmt.Sprintf("call_%d", index)
		}
		out = append(out, types.ResponseToolCall{ID: id, Name: name, Arguments: arguments})
	}
	return out
}

// Decodes OCI usage counters
func decodeOCIUsage(usage map[string]any) *types.Usage {
	u64 := func(v any) *uint64 {
		if n, ok := types.JSONUint64(v); ok {
			return &n
		}
		return nil
	}
	promptTokens := u64(usage["promptTokens"])
	var cacheRead *uint64
	if details, ok := usage["promptTokensDetails"].(map[string]any); ok {
		cacheRead = u64(details["cachedTokens"])
	}
	out := &types.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: u64(usage["completionTokens"]),
		TotalTokens:      u64(usage["totalTokens"]),
		CacheReadTokens:  cacheRead,
	}
	if promptTokens != nil && cacheRead != nil && *promptTokens >= *cacheRead {
		out.UncachedInputTokens = u64Ptr(*promptTokens - *cacheRead)
	}
	return out
}

// Decodes an OCI chat response, with or without its ChatDetails envelope
func (*OCIGenAIChatCodec) DecodeResponse(response any) (*types.AnnotatedLLMResponse, error) {
	obj, ok := response.(map[string]any)
	if !ok {
		return &types.AnnotatedLLMResponse{Extra: map[string]any{"raw": cloneValue(response)}}, nil
	}
	var envelope map[string]any
	chatResponse := obj
	if inner, ok := obj["chatResponse"].(map[string]any); ok {
		envelope = obj
		chatResponse = inner
	}
	var model, modelVersion *string
	if envelope != nil {
		if id, ok := envelope["modelId"].(string); ok {
			model = &id
		}
		if version, ok := envelope["modelVersion"].(string); ok {
			modelVersion = &version
		}
	}
	apiFormat := ociAPIFormat(chatResponse)
	var body ociResponseBody
	var err error
	var modeledResponseKeys []string
	switch apiFormat {
	case "COHERE":
		body = decodeOCICohereResponseBody(chatResponse)
		modeledResponseKeys = []string{"apiFormat", "text", "finishReason", "toolCalls", "usage"}
	case "COHEREV2":
		body, err = decodeOCICohereV2ResponseBody(chatResponse)
		modeledResponseKeys = []string{"apiFormat", "id", "message", "finishReason", "usage"}
	default:
		body, err = decodeOCIGenericResponseBody(chatResponse)
		modeledResponseKeys = []string{"apiFormat", "choices", "usage"}
	}
	if err != nil {
		return nil, err
	}
	var id *string
	if apiFormat == "COHEREV2" {
		if value, ok := chatResponse["id"].(string); ok {
			id = &value
		}
	}
	var usage *types.Usage
	if raw, ok := chatResponse["usage"].(map[string]any); ok {
		usage = decodeOCIUsage(raw)
	}
	extra := map[string]any{}
	if envelope != nil {
		extra = ociUnmodeledFields(envelope, []string{"chatResponse", "modelId", "modelVersion"})
	}
	for key, value := range ociUnmodeledFields(chatResponse, modeledResponseKeys) {
		extra[key] = value
	}
	for key, value := range body.nestedExtra {
		extra[key] = value
	}
	var finishReason *types.FinishReason
	if body.finishReason != nil {
		reason := mapOCIFinishReason(*body.finishReason)
		finishReason = &reason
	}
	return &types.AnnotatedLLMResponse{
		ID:           id,
		Model:        model,
		Message:      body.message,
		ToolCalls:    body.toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
		APISpecific:  types.APISpecificResponseOCIGenAI{APIFormat: strPtr(apiFormat), ModelVersion: modelVersion},
		Extra:        extra,
	}, nil
}

// OCIGenAIStreamingCodec assembles streamed OCI chat events into one chat response
type OCIGenAIStreamingCodec struct {
	mu    sync.Mutex
	state ociStreamingState
}

// Returns an OCI streaming codec with empty state
func NewOCIGenAIStreamingCodec() *OCIGenAIStreamingCodec {
	return &OCIGenAIStreamingCodec{}
}

// Returns a collector that folds each event into the shared state
func (c *OCIGenAIStreamingCodec) Collector() types.LLMCollectorFn {
	return func(event any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.observe(event)
		return nil
	}
}

// Returns a collector that reads each event without taking ownership
func (c *OCIGenAIStreamingCodec) BorrowedCollector() types.LLMBorrowedCollectorFn {
	return func(event any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.observe(event)
		return nil
	}
}

// Returns a finalizer that emits the assembled chat response and resets the state
func (c *OCIGenAIStreamingCodec) Finalizer() types.LLMFinalizerFn {
	return func() any {
		c.mu.Lock()
		state := c.state
		c.state = ociStreamingState{}
		c.mu.Unlock()
		return state.finalize()
	}
}

// Accumulated OCI stream state
type ociStreamingState struct {
	modelID            *string
	apiFormat          *string
	usage              any
	choices            map[uint64]*ociChoiceState
	cohereText         string
	cohereFinishReason *string
}

// Accumulated state of one streamed OCI choice
type ociChoiceState struct {
	role         *string
	parts        []any
	toolCalls    []*ociToolCallState
	finishReason *string
}

// Accumulated state of one streamed OCI tool call
type ociToolCallState struct {
	id        *string
	kind      *string
	name      *string
	arguments strings.Builder
}

// Folds one stream event into the state
func (s *ociStreamingState) observe(event any) {
	obj, ok := event.(map[string]any)
	if !ok {
		return
	}
	if modelID, ok := obj["modelId"].(string); ok {
		s.modelID = strPtr(modelID)
	}
	if inner, ok := obj["chatResponse"].(map[string]any); ok {
		obj = inner
	}
	if s.apiFormat == nil {
		if format, ok := obj["apiFormat"].(string); ok {
			s.apiFormat = strPtr(unicodeUpper(format))
		} else {
			s.apiFormat = ociInferAPIFormat(obj)
		}
	}
	if usage, ok := obj["usage"]; ok && usage != nil {
		s.usage = cloneValue(usage)
	}
	if s.apiFormat != nil && *s.apiFormat == "COHERE" {
		finishReason, hasFinish := obj["finishReason"].(string)
		if text, ok := obj["text"].(string); ok {
			if hasFinish && text != "" {
				s.cohereText = text
			} else {
				s.cohereText += text
			}
		}
		if hasFinish {
			s.cohereFinishReason = strPtr(finishReason)
		}
		return
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, choice := range choices {
			if choice, ok := choice.(map[string]any); ok {
				s.observeGenericChoice(choice)
			}
		}
		return
	}
	s.observeGenericChoice(obj)
}

// Infers the API format of a stream event that does not name one
func ociInferAPIFormat(obj map[string]any) *string {
	_, hasMessage := obj["message"]
	_, hasChoices := obj["choices"]
	_, hasIndex := obj["index"]
	if hasMessage || hasChoices || hasIndex {
		return strPtr("GENERIC")
	}
	if _, hasText := obj["text"]; hasText {
		return strPtr("COHERE")
	}
	return nil
}

// Folds one streamed choice into its indexed choice state
func (s *ociStreamingState) observeGenericChoice(choice map[string]any) {
	index, _ := types.JSONUint64(choice["index"])
	if s.choices == nil {
		s.choices = map[uint64]*ociChoiceState{}
	}
	entry, ok := s.choices[index]
	if !ok {
		entry = &ociChoiceState{}
		s.choices[index] = entry
	}
	if reason, ok := choice["finishReason"].(string); ok {
		entry.finishReason = strPtr(reason)
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return
	}
	if role, ok := message["role"].(string); ok {
		entry.role = strPtr(role)
	}
	if parts, ok := message["content"].([]any); ok {
		for _, part := range parts {
			entry.observeContentPart(part)
		}
	}
	if calls, ok := message["toolCalls"].([]any); ok {
		for position, call := range calls {
			if call, ok := call.(map[string]any); ok {
				entry.observeToolCall(position, call)
			}
		}
	}
}

// Builds the assembled chat response in the observed API format
func (s *ociStreamingState) finalize() any {
	apiFormat := "GENERIC"
	if s.apiFormat != nil {
		apiFormat = *s.apiFormat
	}
	chatResponse := map[string]any{"apiFormat": apiFormat}
	switch apiFormat {
	case "COHERE":
		chatResponse["text"] = s.cohereText
		if s.cohereFinishReason != nil {
			chatResponse["finishReason"] = *s.cohereFinishReason
		}
	case "COHEREV2":
		choice := &ociChoiceState{}
		if keys := sortedU64Keys(s.choices); len(keys) > 0 {
			choice = s.choices[keys[0]]
		}
		chatResponse["message"] = choice.finalizeV2Message()
		if choice.finishReason != nil {
			chatResponse["finishReason"] = *choice.finishReason
		}
	default:
		choices := []any{}
		for _, index := range sortedU64Keys(s.choices) {
			choices = append(choices, s.choices[index].finalize(index))
		}
		chatResponse["choices"] = choices
	}
	if s.usage != nil {
		chatResponse["usage"] = s.usage
	}
	output := map[string]any{"chatResponse": chatResponse}
	if s.modelID != nil {
		output["modelId"] = *s.modelID
	}
	return output
}

// Appends a streamed content part, merging consecutive TEXT parts
func (c *ociChoiceState) observeContentPart(part any) {
	if kind, _ := getString(part, "type"); kind == "TEXT" {
		text, ok := getString(part, "text")
		if !ok {
			return
		}
		if len(c.parts) > 0 {
			if last, ok := c.parts[len(c.parts)-1].(map[string]any); ok {
				if lastKind, _ := last["type"].(string); lastKind == "TEXT" {
					previous, _ := last["text"].(string)
					last["text"] = previous + text
					return
				}
			}
		}
		c.parts = append(c.parts, map[string]any{"type": "TEXT", "text": text})
		return
	}
	if obj, ok := part.(map[string]any); ok {
		c.parts = append(c.parts, cloneObject(obj))
	}
}

// Drops empty TEXT parts from accumulated content
func ociContentParts(parts []any) []any {
	out := []any{}
	for _, part := range parts {
		kind, _ := getString(part, "type")
		text, isString := getString(part, "text")
		if kind != "TEXT" || (isString && text != "") {
			out = append(out, part)
		}
	}
	return out
}

// Folds a streamed tool call fragment into the call it continues
func (c *ociChoiceState) observeToolCall(position int, call map[string]any) {
	slot := -1
	if id, ok := call["id"].(string); ok {
		for i, state := range c.toolCalls {
			if state.id != nil && *state.id == id {
				slot = i
				break
			}
		}
	} else if position < len(c.toolCalls) {
		slot = position
	}
	if slot < 0 {
		c.toolCalls = append(c.toolCalls, &ociToolCallState{})
		slot = len(c.toolCalls) - 1
	}
	state := c.toolCalls[slot]
	if id, ok := call["id"].(string); ok {
		state.id = strPtr(id)
	}
	if kind, ok := call["type"].(string); ok {
		state.kind = strPtr(kind)
	}
	body := call
	if function, ok := call["function"].(map[string]any); ok {
		body = function
	}
	if name, ok := body["name"].(string); ok {
		state.name = strPtr(name)
	}
	if arguments, ok := body["arguments"].(string); ok {
		state.arguments.WriteString(arguments)
	}
}

// Builds the assembled message of a choice with role, content and tool calls
func (c *ociChoiceState) message(finalizeCall func(*ociToolCallState) any) map[string]any {
	role := "ASSISTANT"
	if c.role != nil {
		role = *c.role
	}
	message := map[string]any{"role": role, "content": ociContentParts(c.parts)}
	if len(c.toolCalls) > 0 {
		calls := make([]any, 0, len(c.toolCalls))
		for _, call := range c.toolCalls {
			calls = append(calls, finalizeCall(call))
		}
		message["toolCalls"] = calls
	}
	return message
}

// Builds the assembled COHEREV2 message with nested tool calls
func (c *ociChoiceState) finalizeV2Message() any {
	return c.message((*ociToolCallState).finalizeV2)
}

// Builds the assembled GENERIC choice with flat tool calls
func (c *ociChoiceState) finalize(index uint64) any {
	choice := map[string]any{"index": index, "message": c.message((*ociToolCallState).finalize)}
	if c.finishReason != nil {
		choice["finishReason"] = *c.finishReason
	}
	return choice
}

// Builds a nested COHEREV2 tool call
func (t *ociToolCallState) finalizeV2() any {
	name := ""
	if t.name != nil {
		name = *t.name
	}
	kind := "FUNCTION"
	if t.kind != nil {
		kind = *t.kind
	}
	call := map[string]any{"type": kind, "function": map[string]any{"name": name, "arguments": t.arguments.String()}}
	if t.id != nil {
		call["id"] = *t.id
	}
	return call
}

// Builds a flat GENERIC tool call
func (t *ociToolCallState) finalize() any {
	name := ""
	if t.name != nil {
		name = *t.name
	}
	kind := "FUNCTION"
	if t.kind != nil {
		kind = *t.kind
	}
	call := map[string]any{"type": kind, "name": name, "arguments": t.arguments.String()}
	if t.id != nil {
		call["id"] = *t.id
	}
	return call
}

package codec

import (
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

// AnthropicMessagesCodec translates Anthropic Messages requests and responses
type AnthropicMessagesCodec struct{}

// Returns the Anthropic Messages codec
func NewAnthropicMessagesCodec() *AnthropicMessagesCodec {
	return &AnthropicMessagesCodec{}
}

var anthropicProviderSurface = providerSurfaceDescriptor{
	surface: ProviderSurfaceAnthropicMessages,
	detectRequest: func(obj map[string]any, hint *string) bool {
		hinted := hint != nil && (*hint == "anthropic" || *hint == "anthropic.messages")
		_, hasSystem := obj["system"]
		_, hasMessages := obj["messages"]
		return hasSystem || (hinted && hasMessages)
	},
	detectResponse: func(obj map[string]any) bool {
		kind, _ := obj["type"].(string)
		_, isArray := obj["content"].([]any)
		return kind == "message" && isArray
	},
	decodeRequest: func(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
		return NewAnthropicMessagesCodec().Decode(request)
	},
	decodeResponse: func(response any) (*types.AnnotatedLLMResponse, error) {
		return NewAnthropicMessagesCodec().DecodeResponse(response)
	},
	codecName:      "anthropic_messages",
	requestCodec:   func() types.LLMCodec { return NewAnthropicMessagesCodec() },
	responseCodec:  func() types.LLMResponseCodec { return NewAnthropicMessagesCodec() },
	streamingCodec: func() StreamingCodec { return NewAnthropicMessagesStreamingCodec() },
}

var anthropicModeledRequestKeys = []string{
	"system", "messages", "model", "max_tokens", "temperature", "top_p", "stop_sequences", "tools",
	"tool_choice", "metadata", "service_tier", "stream", "cache_control", "container", "inference_geo",
	"output_config", "thinking", "top_k", "anthropic-user-profile-id",
}

var anthropicAPISpecificKeys = []string{
	"cache_control", "container", "inference_geo", "output_config", "thinking", "top_k", "anthropic-user-profile-id",
}

// Identifies the builtin Anthropic Messages codec
func (*AnthropicMessagesCodec) CodecIdentity() types.LLMCodecIdentity {
	return types.LLMCodecIdentity{Kind: types.LLMCodecIdentityBuiltIn, Builtin: types.BuiltinLLMCodecAnthropicMessages}
}

// Maps an Anthropic stop reason to the normalized finish reason
func mapAnthropicStopReason(reason string) types.FinishReason {
	switch reason {
	case "end_turn":
		return types.FinishReason{Kind: types.FinishReasonComplete}
	case "max_tokens":
		return types.FinishReason{Kind: types.FinishReasonLength}
	case "tool_use":
		return types.FinishReason{Kind: types.FinishReasonToolUse}
	}
	return types.FinishReason{Kind: types.FinishReasonUnknown, Unknown: reason}
}

// Decodes an Anthropic tool choice object, returning nil for unrecognized shapes
func decodeAnthropicToolChoice(value any) types.ToolChoice {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	kind, ok := obj["type"].(string)
	if !ok {
		return nil
	}
	switch kind {
	case "auto":
		return types.ToolChoiceAuto
	case "any":
		return types.ToolChoiceRequired
	case "none":
		return types.ToolChoiceNone
	case "tool":
		name, ok := obj["name"].(string)
		if !ok {
			return nil
		}
		return types.ToolChoiceSpecific(types.ToolChoiceFunction{
			ChoiceType: "function",
			Function:   types.ToolChoiceFunctionName{Name: name},
		})
	}
	return nil
}

// Reads the parallel tool use flag carried inside an Anthropic tool choice
func decodeAnthropicParallelToolCalls(value any) (*bool, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	disabled, err := optionalBool(obj, "disable_parallel_tool_use", "Anthropic Messages tool_choice")
	if err != nil || disabled == nil {
		return nil, err
	}
	return boolPtr(!*disabled), nil
}

// Encodes a normalized tool choice into an Anthropic tool choice object
func encodeAnthropicToolChoice(choice types.ToolChoice) (any, error) {
	switch c := choice.(type) {
	case types.ToolChoiceMode:
		switch c {
		case types.ToolChoiceAuto:
			return map[string]any{"type": "auto"}, nil
		case types.ToolChoiceRequired:
			return map[string]any{"type": "any"}, nil
		case types.ToolChoiceNone:
			return map[string]any{"type": "none"}, nil
		}
	case types.ToolChoiceSpecific:
		return map[string]any{"type": "tool", "name": c.Function.Name}, nil
	case types.ToolChoiceProviderNative:
		if c.Provider == "anthropic_messages" {
			return cloneValue(c.Value), nil
		}
		return nil, types.InvalidArgumentf("tool choice for %s cannot be encoded for Anthropic Messages", c.Provider)
	}
	return nil, types.InvalidArgumentf("tool choice %s cannot be encoded for Anthropic Messages", types.RustDebug(choice))
}

// Encodes a tool choice and carries the parallel tool use hint inside it
func encodeAnthropicToolChoiceWithParallelHint(choice types.ToolChoice, parallelToolCalls *bool) (any, error) {
	value, err := encodeAnthropicToolChoice(choice)
	if err != nil {
		return nil, err
	}
	if obj, ok := value.(map[string]any); ok && parallelToolCalls != nil {
		obj["disable_parallel_tool_use"] = !*parallelToolCalls
	}
	return value, nil
}

// Wraps a raw component as provider native, naming it after its type field
func anthropicNativeComponent(provider string, value any) types.ProviderNativeComponent {
	kind, ok := getString(value, "type")
	if !ok {
		kind = "unknown"
	}
	return types.ProviderNativeComponent{Provider: provider, Kind: kind, Value: cloneValue(value)}
}

// Decodes Anthropic message content from a string or block array
func decodeAnthropicContent(value any) (types.MessageContent, error) {
	if text, ok := value.(string); ok {
		return types.MessageContentText(text), nil
	}
	blocks, ok := value.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages content must be a string or an array")
	}
	parts := make(types.MessageContentParts, 0, len(blocks))
	for _, block := range blocks {
		part, err := decodeAnthropicContentPart(block)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// Decodes one Anthropic content block, keeping unknown block types native
func decodeAnthropicContentPart(value any) (types.ContentPart, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages content block must be an object")
	}
	kind, ok := obj["type"].(string)
	if !ok {
		kind = "unknown"
	}
	switch kind {
	case "text":
		text, ok := obj["text"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic text block is missing text")
		}
		return types.ContentPartText{Text: text, Extra: objectWithout(obj, "type", "text")}, nil
	case "image":
		return types.ContentPartImage{Image: objectWithout(obj, "type"), Extra: map[string]any{}}, nil
	case "document":
		return types.ContentPartFile{File: objectWithout(obj, "type"), Extra: map[string]any{}}, nil
	case "tool_use":
		id, ok := obj["id"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic tool_use block is missing id")
		}
		name, ok := obj["name"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic tool_use block is missing name")
		}
		input, ok := obj["input"]
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic tool_use block is missing input")
		}
		return types.ContentPartToolUse{
			ID:    id,
			Name:  name,
			Input: cloneValue(input),
			Extra: objectWithout(obj, "type", "id", "name", "input"),
		}, nil
	case "tool_result":
		toolUseID, ok := obj["tool_use_id"].(string)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic tool_result block is missing tool_use_id")
		}
		content, ok := obj["content"]
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic tool_result block is missing content")
		}
		var isError *bool
		if raw := obj["is_error"]; raw != nil {
			flag, ok := raw.(bool)
			if !ok {
				return nil, types.InvalidArgumentf("Anthropic tool_result is_error must be a boolean")
			}
			isError = &flag
		}
		return types.ContentPartToolResult{
			ToolUseID: toolUseID,
			Content:   cloneValue(content),
			IsError:   isError,
			Extra:     objectWithout(obj, "type", "tool_use_id", "content", "is_error"),
		}, nil
	}
	native := anthropicNativeComponent("anthropic_messages", value)
	return types.ContentPartProviderNative{Provider: native.Provider, Kind: native.Kind, Value: native.Value}, nil
}

// Decodes one Anthropic message, keeping messages with extension fields native
func decodeAnthropicMessage(value any) (types.Message, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages message must be an object")
	}
	role, ok := obj["role"].(string)
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages message is missing role")
	}
	raw, ok := obj["content"]
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages message is missing content")
	}
	native := types.MessageProviderNative{Provider: "anthropic_messages", Kind: role, Value: cloneValue(value)}
	if !onlyKeys(obj, "role", "content") {
		return native, nil
	}
	content, err := decodeAnthropicContent(raw)
	if err != nil {
		return nil, err
	}
	switch role {
	case "user":
		return types.MessageUser{Content: content}, nil
	case "assistant":
		return types.MessageAssistant{Content: content}, nil
	case "system":
		return types.MessageSystem{Content: content}, nil
	}
	return native, nil
}

// Encodes message content into Anthropic content
func encodeAnthropicContent(content types.MessageContent) (any, error) {
	switch c := content.(type) {
	case types.MessageContentText:
		return string(c), nil
	case types.MessageContentParts:
		out := make([]any, 0, len(c))
		for _, part := range c {
			encoded, err := encodeAnthropicContentPart(part)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded)
		}
		return out, nil
	}
	return nil, types.InvalidArgumentf("Anthropic Messages content must be a string or an array")
}

// Encodes one content part into an Anthropic content block
func encodeAnthropicContentPart(part types.ContentPart) (any, error) {
	switch p := part.(type) {
	case types.ContentPartText:
		obj := cloneObject(p.Extra)
		obj["type"] = "text"
		obj["text"] = p.Text
		return obj, nil
	case types.ContentPartImage:
		image, ok := p.Image.(map[string]any)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic image payload must be an object")
		}
		obj := cloneObject(image)
		for key, value := range p.Extra {
			obj[key] = cloneValue(value)
		}
		obj["type"] = "image"
		return obj, nil
	case types.ContentPartFile:
		file, ok := p.File.(map[string]any)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic document payload must be an object")
		}
		obj := cloneObject(file)
		for key, value := range p.Extra {
			obj[key] = cloneValue(value)
		}
		obj["type"] = "document"
		return obj, nil
	case types.ContentPartToolUse:
		obj := cloneObject(p.Extra)
		obj["type"] = "tool_use"
		obj["id"] = p.ID
		obj["name"] = p.Name
		obj["input"] = cloneValue(p.Input)
		return obj, nil
	case types.ContentPartToolResult:
		obj := cloneObject(p.Extra)
		obj["type"] = "tool_result"
		obj["tool_use_id"] = p.ToolUseID
		obj["content"] = cloneValue(p.Content)
		if p.IsError != nil {
			obj["is_error"] = *p.IsError
		}
		return obj, nil
	case types.ContentPartProviderNative:
		if p.Provider == "anthropic_messages" {
			return cloneValue(p.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("content part %s cannot be encoded for Anthropic Messages", types.RustDebug(part))
}

// Encodes one normalized message into an Anthropic message
func encodeAnthropicMessage(message types.Message) (any, error) {
	switch m := message.(type) {
	case types.MessageUser:
		content, err := encodeAnthropicContent(m.Content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"role": "user", "content": content}, nil
	case types.MessageSystem:
		content, err := encodeAnthropicContent(m.Content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"role": "system", "content": content}, nil
	case types.MessageAssistant:
		var content any = []any{}
		if m.Content != nil {
			encoded, err := encodeAnthropicContent(m.Content)
			if err != nil {
				return nil, err
			}
			content = encoded
		}
		return map[string]any{"role": "assistant", "content": content}, nil
	case types.MessageProviderNative:
		if m.Provider == "anthropic_messages" {
			return cloneValue(m.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("message %s cannot be encoded for Anthropic Messages", types.RustDebug(message))
}

// Encodes one normalized tool into an Anthropic client tool
func encodeAnthropicTool(tool types.ToolDefinition) (any, error) {
	switch t := tool.(type) {
	case types.ToolDefinitionFunction:
		obj := cloneObject(t.Extra)
		obj["name"] = t.Function.Name
		if t.Function.Description != nil {
			obj["description"] = *t.Function.Description
		}
		if t.Function.ParametersPresent || t.Function.Parameters != nil {
			obj["input_schema"] = cloneValue(t.Function.Parameters)
		}
		if t.Function.Strict != nil {
			obj["strict"] = *t.Function.Strict
		}
		for key, value := range t.Function.Extra {
			obj[key] = cloneValue(value)
		}
		return obj, nil
	case types.ToolDefinitionProviderNative:
		if t.Provider == "anthropic_messages" {
			return cloneValue(t.Value), nil
		}
	}
	return nil, types.InvalidArgumentf("tool %s cannot be encoded for Anthropic Messages", types.RustDebug(tool))
}

// Decodes one Anthropic tool, keeping server and typed tools native
func decodeAnthropicTool(value any) (types.ToolDefinition, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages tool must be an object")
	}
	_, hasType := obj["type"]
	_, hasName := obj["name"]
	_, hasSchema := obj["input_schema"]
	if hasType || !hasName || !hasSchema {
		native := anthropicNativeComponent("anthropic_messages", value)
		return types.ToolDefinitionProviderNative{Provider: native.Provider, Kind: native.Kind, Value: native.Value}, nil
	}
	wrapperExtra := objectWithout(obj, "name", "description", "input_schema", "strict")
	name, err := optionalString(obj, "name", "Anthropic Messages tool")
	if err != nil {
		return nil, err
	}
	if name == nil {
		return nil, types.InvalidArgumentf("Anthropic Messages tool is missing name")
	}
	description, err := optionalString(obj, "description", "Anthropic Messages tool")
	if err != nil {
		return nil, err
	}
	strict, err := optionalBool(obj, "strict", "Anthropic Messages tool")
	if err != nil {
		return nil, err
	}
	parameters, parametersPresent := obj["input_schema"]
	return types.ToolDefinitionFunction{
		Function: types.FunctionDefinition{
			Name:              *name,
			Description:       description,
			Parameters:        cloneValue(parameters),
			ParametersPresent: parametersPresent,
			Strict:            strict,
			Extra:             map[string]any{},
		},
		Extra: wrapperExtra,
	}, nil
}

// Writes edited messages, system prompt and model
func patchAnthropicMessagesAndModel(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqSlice(annotated.Messages, baseline.Messages) {
		original, hasOriginal := obj["messages"].([]any)
		messages, err := encodeChangedItems(annotated.Messages, baseline.Messages, original, hasOriginal, encodeAnthropicMessage)
		if err != nil {
			return err
		}
		obj["messages"] = messages
	}
	if !types.Equal(annotated.Instructions, baseline.Instructions) {
		if annotated.Instructions == nil {
			delete(obj, "system")
		} else {
			system, err := encodeAnthropicContent(annotated.Instructions)
			if err != nil {
				return err
			}
			obj["system"] = system
		}
	}
	if !eqStrPtr(annotated.Model, baseline.Model) {
		value, present := optStringJSON(annotated.Model)
		setOrRemoveJSON(obj, "model", value, present)
	}
	return nil
}

// Writes edited sampling parameters, token limit and stop sequences
func patchAnthropicParams(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) {
	if eqParams(annotated.Params, baseline.Params) {
		return
	}
	patchParamFloats(obj, annotated.Params, baseline.Params)
	maxTokens := paramMaxTokens(annotated.Params)
	if !eqU64Ptr(maxTokens, paramMaxTokens(baseline.Params)) {
		value, present := optU64JSON(maxTokens)
		setOrRemoveJSON(obj, "max_tokens", value, present)
	}
	stop := paramStop(annotated.Params)
	if !eqStrings(stop, paramStop(baseline.Params)) {
		value, present := optStringsJSON(stop)
		setOrRemoveJSON(obj, "stop_sequences", value, present)
	}
}

// Writes edited tools and the tool choice carrying the parallel tool use hint
func patchAnthropicTools(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) error {
	if !eqOptSlice(annotated.Tools, baseline.Tools) {
		if annotated.Tools == nil {
			delete(obj, "tools")
		} else {
			original, hasOriginal := obj["tools"].([]any)
			tools, err := encodeChangedItems(annotated.Tools, baseline.Tools, original, hasOriginal, encodeAnthropicTool)
			if err != nil {
				return err
			}
			obj["tools"] = tools
		}
	}
	if types.Equal(annotated.ToolChoice, baseline.ToolChoice) && eqBoolPtr(annotated.ParallelToolCalls, baseline.ParallelToolCalls) {
		return nil
	}
	var toolChoice any
	present := true
	switch {
	case annotated.ToolChoice != nil && baseline.ToolChoice != nil:
		edited, err := encodeAnthropicToolChoiceWithParallelHint(annotated.ToolChoice, annotated.ParallelToolCalls)
		if err != nil {
			return err
		}
		before, err := encodeAnthropicToolChoiceWithParallelHint(baseline.ToolChoice, baseline.ParallelToolCalls)
		if err != nil {
			return err
		}
		toolChoice = edited
		if original, ok := obj["tool_choice"]; ok {
			toolChoice, err = patchChangedJSON(original, before, edited)
			if err != nil {
				return err
			}
		}
	case annotated.ToolChoice != nil:
		edited, err := encodeAnthropicToolChoiceWithParallelHint(annotated.ToolChoice, annotated.ParallelToolCalls)
		if err != nil {
			return err
		}
		toolChoice = edited
	case annotated.ParallelToolCalls != nil:
		edited, err := encodeAnthropicToolChoiceWithParallelHint(types.ToolChoiceAuto, annotated.ParallelToolCalls)
		if err != nil {
			return err
		}
		toolChoice = edited
	default:
		present = false
	}
	setOrRemoveJSON(obj, "tool_choice", toolChoice, present)
	return nil
}

// Writes edited metadata, service tier and stream flag
func patchAnthropicCommonFields(obj map[string]any, annotated, baseline *types.AnnotatedLLMRequest) {
	if !eqOptJSON(annotated.Metadata, annotated.MetadataPresent, baseline.Metadata, baseline.MetadataPresent) {
		setOrRemoveJSON(obj, "metadata", cloneValue(annotated.Metadata), annotated.MetadataPresent || annotated.Metadata != nil)
	}
	if !eqStrPtr(annotated.ServiceTier, baseline.ServiceTier) {
		value, present := optStringJSON(annotated.ServiceTier)
		setOrRemoveJSON(obj, "service_tier", value, present)
	}
	if !eqBoolPtr(annotated.Stream, baseline.Stream) {
		value, present := optBoolJSON(annotated.Stream)
		setOrRemoveJSON(obj, "stream", value, present)
	}
}

// Rejects edits to fields Anthropic Messages cannot carry
func validateAnthropicSupportedFields(annotated, baseline *types.AnnotatedLLMRequest) error {
	unsupported := !eqBoolPtr(annotated.Store, baseline.Store) ||
		!eqStrPtr(annotated.PreviousResponseID, baseline.PreviousResponseID) ||
		!eqOptJSON(annotated.Truncation, annotated.TruncationPresent, baseline.Truncation, baseline.TruncationPresent) ||
		!eqOptJSON(annotated.Reasoning, annotated.ReasoningPresent, baseline.Reasoning, baseline.ReasoningPresent) ||
		!eqOptJSON(annotated.Include, annotated.IncludePresent, baseline.Include, baseline.IncludePresent) ||
		!eqStrPtr(annotated.User, baseline.User) ||
		!eqU64Ptr(annotated.MaxOutputTokens, baseline.MaxOutputTokens) ||
		!eqU64Ptr(annotated.MaxToolCalls, baseline.MaxToolCalls) ||
		!eqU64Ptr(annotated.TopLogprobs, baseline.TopLogprobs)
	if unsupported {
		return types.InvalidArgumentf("request contains fields that cannot be encoded for Anthropic Messages")
	}
	return nil
}

// Writes edited Anthropic specific controls or removes them when the section was dropped
func patchAnthropicAPISpecific(obj map[string]any, edited, baseline types.APISpecificRequest) error {
	e, editedIsAnthropic := edited.(types.APISpecificRequestAnthropicMessages)
	b, baselineIsAnthropic := baseline.(types.APISpecificRequestAnthropicMessages)
	switch {
	case editedIsAnthropic && baselineIsAnthropic:
		if !eqOptJSON(e.CacheControl, e.CacheControlPresent, b.CacheControl, b.CacheControlPresent) {
			setOrRemoveJSON(obj, "cache_control", cloneValue(e.CacheControl), e.CacheControlPresent || e.CacheControl != nil)
		}
		for _, field := range []struct {
			key        string
			value, old *string
		}{
			{"container", e.Container, b.Container},
			{"inference_geo", e.InferenceGeo, b.InferenceGeo},
			{"anthropic-user-profile-id", e.UserProfileID, b.UserProfileID},
		} {
			if !eqStrPtr(field.value, field.old) {
				value, present := optStringJSON(field.value)
				setOrRemoveJSON(obj, field.key, value, present)
			}
		}
		for _, field := range []struct {
			key          string
			value        any
			valuePresent bool
			old          any
			oldPresent   bool
		}{
			{"output_config", e.OutputConfig, e.OutputConfigPresent, b.OutputConfig, b.OutputConfigPresent},
			{"thinking", e.Thinking, e.ThinkingPresent, b.Thinking, b.ThinkingPresent},
		} {
			if !eqOptJSON(field.value, field.valuePresent, field.old, field.oldPresent) {
				setOrRemoveJSON(obj, field.key, cloneValue(field.value), field.valuePresent || field.value != nil)
			}
		}
		if !eqU64Ptr(e.TopK, b.TopK) {
			value, present := optU64JSON(e.TopK)
			setOrRemoveJSON(obj, "top_k", value, present)
		}
		return nil
	case edited == nil && baselineIsAnthropic:
		for _, key := range anthropicAPISpecificKeys {
			delete(obj, key)
		}
		return nil
	case edited != nil || baseline != nil:
		return types.InvalidArgumentf("api_specific provider does not match Anthropic Messages")
	}
	return nil
}

// Joins the text blocks of a response into message content
func anthropicTextMessage(blocks []any) types.MessageContent {
	var parts []string
	for _, block := range blocks {
		if kind, _ := getString(block, "type"); kind != "text" {
			continue
		}
		if text, ok := getString(block, "text"); ok {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return types.MessageContentText(strings.Join(parts, "\n"))
}

// Collects the tool_use blocks of a response as tool calls
func anthropicToolCalls(blocks []any) []types.ResponseToolCall {
	var calls []types.ResponseToolCall
	for _, block := range blocks {
		if kind, _ := getString(block, "type"); kind != "tool_use" {
			continue
		}
		id, ok := getString(block, "id")
		if !ok {
			continue
		}
		name, ok := getString(block, "name")
		if !ok {
			continue
		}
		input, ok := getValue(block, "input")
		if !ok {
			continue
		}
		calls = append(calls, types.ResponseToolCall{ID: id, Name: name, Arguments: cloneValue(input)})
	}
	return calls
}

// Raw Anthropic response fields read from a response
type rawAnthropicResponse struct {
	id           *string
	objectType   *string
	role         *string
	model        *string
	content      []any
	stopReason   *string
	stopSequence *string
	serviceTier  *string
	container    any
	usage        *rawAnthropicUsage
	extra        map[string]any
}

// Raw Anthropic usage fields
type rawAnthropicUsage struct {
	inputTokens              *uint64
	outputTokens             *uint64
	cacheReadInputTokens     *uint64
	cacheCreationInputTokens *uint64
	iterations               []rawAnthropicIterationUsage
	providerCost             *float64
	cost                     *rawUsageCost
}

// Raw Anthropic per-iteration usage fields
type rawAnthropicIterationUsage struct {
	model                    *string
	inputTokens              *uint64
	outputTokens             *uint64
	cacheReadInputTokens     *uint64
	cacheCreationInputTokens *uint64
}

// Decodes Anthropic per-iteration usage with Rust deserialization semantics
func decodeRawAnthropicIterationUsage(value any) (rawAnthropicIterationUsage, error) {
	var it rawAnthropicIterationUsage
	err := serdeStruct(value, "RawAnthropicIterationUsage", []serdeField{
		{key: "model", optional: true, set: func(v any) (err error) { it.model, err = serdeOptString(v); return }},
		{key: "input_tokens", optional: true, set: func(v any) (err error) { it.inputTokens, err = serdeOptU64(v); return }},
		{key: "output_tokens", optional: true, set: func(v any) (err error) { it.outputTokens, err = serdeOptU64(v); return }},
		{key: "cache_read_input_tokens", optional: true, set: func(v any) (err error) { it.cacheReadInputTokens, err = serdeOptU64(v); return }},
		{key: "cache_creation_input_tokens", optional: true, set: func(v any) (err error) { it.cacheCreationInputTokens, err = serdeOptU64(v); return }},
	}, false, nil)
	return it, err
}

// Decodes Anthropic usage with Rust deserialization semantics
func decodeRawAnthropicUsage(value any) (rawAnthropicUsage, error) {
	var usage rawAnthropicUsage
	err := serdeStruct(value, "RawAnthropicUsage", []serdeField{
		{key: "input_tokens", optional: true, set: func(v any) (err error) { usage.inputTokens, err = serdeOptU64(v); return }},
		{key: "output_tokens", optional: true, set: func(v any) (err error) { usage.outputTokens, err = serdeOptU64(v); return }},
		{key: "cache_read_input_tokens", optional: true, set: func(v any) (err error) { usage.cacheReadInputTokens, err = serdeOptU64(v); return }},
		{key: "cache_creation_input_tokens", optional: true, set: func(v any) (err error) { usage.cacheCreationInputTokens, err = serdeOptU64(v); return }},
		{key: "iterations", optional: true, set: func(v any) error {
			usage.iterations = []rawAnthropicIterationUsage{}
			return serdeSeq(v, func(item any) error {
				it, err := decodeRawAnthropicIterationUsage(item)
				usage.iterations = append(usage.iterations, it)
				return err
			})
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

// Decodes an Anthropic response with Rust deserialization semantics, reading content separately
func decodeRawAnthropicResponse(response any) (rawAnthropicResponse, error) {
	raw := rawAnthropicResponse{extra: map[string]any{}}
	fields := []serdeField{
		{key: "id", optional: true, set: func(v any) (err error) { raw.id, err = serdeOptString(v); return }},
		{key: "type", optional: true, set: func(v any) (err error) { raw.objectType, err = serdeOptString(v); return }},
		{key: "role", optional: true, set: func(v any) (err error) { raw.role, err = serdeOptString(v); return }},
		{key: "model", optional: true, set: func(v any) (err error) { raw.model, err = serdeOptString(v); return }},
		{key: "content", optional: true, set: func(v any) (err error) { raw.content, err = serdeValues(v); return }},
		{key: "stop_reason", optional: true, set: func(v any) (err error) { raw.stopReason, err = serdeOptString(v); return }},
		{key: "stop_sequence", optional: true, set: func(v any) (err error) { raw.stopSequence, err = serdeOptString(v); return }},
		{key: "service_tier", optional: true, set: func(v any) (err error) { raw.serviceTier, err = serdeOptString(v); return }},
		{key: "container", optional: true, set: func(v any) error { raw.container = cloneValue(v); return nil }},
		{key: "usage", optional: true, set: func(v any) error {
			usage, err := decodeRawAnthropicUsage(v)
			raw.usage = &usage
			return err
		}},
	}
	obj, ok := response.(map[string]any)
	if !ok {
		return raw, serdeStruct(response, "RawAnthropicResponse", fields, true, raw.extra)
	}
	if err := serdeStruct(objectWithoutShallow(obj, "content"), "RawAnthropicResponse", fields, true, raw.extra); err != nil {
		return raw, err
	}
	for key, value := range raw.extra {
		raw.extra[key] = cloneValue(value)
	}
	switch content := obj["content"].(type) {
	case nil:
	case []any:
		raw.content = cloneSlice(content)
	default:
		values, err := serdeValues(content)
		if err != nil {
			return raw, err
		}
		raw.content = values
	}
	return raw, nil
}

// Sums one iteration counter, returning nil when no iteration reports it
func sumAnthropicIterationField(iterations []rawAnthropicIterationUsage, field func(rawAnthropicIterationUsage) *uint64) *uint64 {
	var total *uint64
	for _, it := range iterations {
		value := field(it)
		if value == nil {
			continue
		}
		if total == nil {
			total = u64Ptr(*value)
		} else {
			*total = saturatingAdd(*total, *value)
		}
	}
	return total
}

// Reports whether every iteration that names a model names the response model
func anthropicIterationModelsMatchResponse(iterations []rawAnthropicIterationUsage, responseModel *string) bool {
	for _, it := range iterations {
		if it.model != nil && !eqStrPtr(it.model, responseModel) {
			return false
		}
	}
	return true
}

// Builds normalized usage from Anthropic usage, summing iterations when present
func anthropicUsage(raw *rawAnthropicUsage, model *string) *types.Usage {
	if raw == nil {
		return nil
	}
	modelProvider := InferModelProvider("anthropic", model)
	iterations := raw.iterations
	if len(iterations) == 0 {
		iterations = nil
	}
	pick := func(field func(rawAnthropicIterationUsage) *uint64, fallback *uint64) *uint64 {
		if iterations != nil {
			if sum := sumAnthropicIterationField(iterations, field); sum != nil {
				return sum
			}
		}
		return fallback
	}
	prompt := pick(func(it rawAnthropicIterationUsage) *uint64 { return it.inputTokens }, raw.inputTokens)
	completion := pick(func(it rawAnthropicIterationUsage) *uint64 { return it.outputTokens }, raw.outputTokens)
	cacheRead := pick(func(it rawAnthropicIterationUsage) *uint64 { return it.cacheReadInputTokens }, raw.cacheReadInputTokens)
	cacheWrite := pick(func(it rawAnthropicIterationUsage) *uint64 { return it.cacheCreationInputTokens }, raw.cacheCreationInputTokens)
	usage := &types.Usage{
		PromptTokens:        prompt,
		CompletionTokens:    completion,
		CacheReadTokens:     cacheRead,
		CacheWriteTokens:    cacheWrite,
		UncachedInputTokens: prompt,
		Cost:                providerReportedCost(raw.providerCost, raw.cost),
	}
	if prompt != nil && completion != nil {
		usage.TotalTokens = u64Ptr(saturatingAdd(*prompt, *completion))
	}
	if usage.Cost == nil && anthropicIterationModelsMatchResponse(iterations, model) && model != nil {
		usage.Cost = EstimateCostForProvider(modelProvider, *model, usage)
	}
	return usage
}

// Reports whether estimated pricing applies because every iteration ran on the response model
func (*AnthropicMessagesCodec) AllowsEstimatedCost(response any) bool {
	var model *string
	var iterations []*string
	hasUsage := false
	hasIterations := false
	err := serdeStruct(response, "RawAnthropicPricingEligibility", []serdeField{
		{key: "model", optional: true, set: func(v any) (err error) { model, err = serdeOptString(v); return }},
		{key: "usage", optional: true, set: func(v any) error {
			hasUsage = true
			return serdeStruct(v, "RawAnthropicPricingUsage", []serdeField{
				{key: "iterations", optional: true, set: func(v any) error {
					hasIterations = true
					return serdeSeq(v, func(item any) error {
						var iterationModel *string
						err := serdeStruct(item, "RawAnthropicIterationModel", []serdeField{
							{key: "model", optional: true, set: func(v any) (err error) { iterationModel, err = serdeOptString(v); return }},
						}, false, nil)
						iterations = append(iterations, iterationModel)
						return err
					})
				}},
			}, false, nil)
		}},
	}, false, nil)
	if err != nil {
		return false
	}
	if !hasUsage || !hasIterations {
		return true
	}
	for _, iterationModel := range iterations {
		if iterationModel != nil && !eqStrPtr(iterationModel, model) {
			return false
		}
	}
	return true
}

// Decodes an Anthropic Messages response into the normalized response
func (*AnthropicMessagesCodec) DecodeResponse(response any) (*types.AnnotatedLLMResponse, error) {
	raw, err := decodeRawAnthropicResponse(response)
	if err != nil {
		return nil, types.Internalf("Anthropic Messages response decode: %v", err)
	}
	var finishReason *types.FinishReason
	if raw.stopReason != nil {
		reason := mapAnthropicStopReason(*raw.stopReason)
		finishReason = &reason
	}
	return &types.AnnotatedLLMResponse{
		ID:           raw.id,
		Model:        raw.model,
		Message:      anthropicTextMessage(raw.content),
		ToolCalls:    anthropicToolCalls(raw.content),
		FinishReason: finishReason,
		Usage:        anthropicUsage(raw.usage, raw.model),
		APISpecific: types.APISpecificResponseAnthropicMessages{
			ObjectType:    raw.objectType,
			Role:          raw.role,
			StopReason:    raw.stopReason,
			StopSequence:  raw.stopSequence,
			ServiceTier:   raw.serviceTier,
			Container:     raw.container,
			ContentBlocks: raw.content,
		},
		Extra: raw.extra,
	}, nil
}

// Decodes an Anthropic Messages request into the normalized request
func (*AnthropicMessagesCodec) Decode(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
	obj, ok := request.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("request content is not an object")
	}
	rawMessages, ok := obj["messages"]
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages request is missing messages")
	}
	messageValues, ok := rawMessages.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("Anthropic Messages messages must be an array")
	}
	messages := make([]types.Message, 0, len(messageValues))
	for _, value := range messageValues {
		message, err := decodeAnthropicMessage(value)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	var instructions types.MessageContent
	if system, ok := obj["system"]; ok {
		decoded, err := decodeAnthropicContent(system)
		if err != nil {
			return nil, err
		}
		instructions = decoded
	}
	const surface = "Anthropic Messages"
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
	maxTokens, err := optionalU64(obj, "max_tokens", surface)
	if err != nil {
		return nil, err
	}
	var stop []string
	if value := obj["stop_sequences"]; value != nil {
		stop, err = serdeStrings(value)
		if err != nil {
			return nil, types.InvalidArgumentf("invalid Anthropic stop_sequences value: %v", err)
		}
	}
	var params *types.GenerationParams
	if temperature != nil || maxTokens != nil || topP != nil || stop != nil {
		params = &types.GenerationParams{Temperature: temperature, MaxTokens: maxTokens, TopP: topP, Stop: stop}
	}
	var tools []types.ToolDefinition
	if value, ok := obj["tools"]; ok {
		items, ok := value.([]any)
		if !ok {
			return nil, types.InvalidArgumentf("Anthropic Messages tools must be an array")
		}
		tools = make([]types.ToolDefinition, 0, len(items))
		for _, item := range items {
			tool, err := decodeAnthropicTool(item)
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
	}
	var toolChoice types.ToolChoice
	var parallelToolCalls *bool
	if value, ok := obj["tool_choice"]; ok {
		toolChoice = decodeAnthropicToolChoice(value)
		if toolChoice == nil {
			toolChoice = types.ToolChoiceProviderNative(anthropicNativeComponent("anthropic_messages", value))
		}
		parallelToolCalls, err = decodeAnthropicParallelToolCalls(value)
		if err != nil {
			return nil, err
		}
	}
	serviceTier, err := optionalString(obj, "service_tier", surface)
	if err != nil {
		return nil, err
	}
	stream, err := optionalBool(obj, "stream", surface)
	if err != nil {
		return nil, err
	}
	container, err := optionalString(obj, "container", surface)
	if err != nil {
		return nil, err
	}
	inferenceGeo, err := optionalString(obj, "inference_geo", surface)
	if err != nil {
		return nil, err
	}
	topK, err := optionalU64(obj, "top_k", surface)
	if err != nil {
		return nil, err
	}
	userProfileID, err := optionalString(obj, "anthropic-user-profile-id", surface)
	if err != nil {
		return nil, err
	}
	objects := map[string]any{}
	for _, key := range []string{"metadata", "cache_control", "output_config", "thinking"} {
		value, err := optionalObject(obj, key, surface)
		if err != nil {
			return nil, err
		}
		objects[key] = cloneValue(value)
	}
	return &types.AnnotatedLLMRequest{
		Messages:          messages,
		Instructions:      instructions,
		Model:             model,
		Params:            params,
		Tools:             tools,
		ToolChoice:        toolChoice,
		Metadata:          objects["metadata"],
		ServiceTier:       serviceTier,
		ParallelToolCalls: parallelToolCalls,
		Stream:            stream,
		APISpecific: types.APISpecificRequestAnthropicMessages{
			CacheControl:  objects["cache_control"],
			Container:     container,
			InferenceGeo:  inferenceGeo,
			OutputConfig:  objects["output_config"],
			Thinking:      objects["thinking"],
			TopK:          topK,
			UserProfileID: userProfileID,
		},
		Extra: objectWithoutList(obj, anthropicModeledRequestKeys),
	}, nil
}

// Re-decodes the original request as the baseline and merges the edits into it
func (c *AnthropicMessagesCodec) Encode(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest) (*types.LLMRequest, error) {
	baseline, err := c.Decode(original)
	if err != nil {
		return nil, err
	}
	return c.EncodeWithBaseline(annotated, original, baseline)
}

// Merges the edits between baseline and annotated into a copy of the original request
func (*AnthropicMessagesCodec) EncodeWithBaseline(annotated *types.AnnotatedLLMRequest, original *types.LLMRequest, baseline *types.AnnotatedLLMRequest) (*types.LLMRequest, error) {
	out := original.Clone()
	obj, ok := out.Content.(map[string]any)
	if !ok {
		return nil, types.Internalf("original content is not an object")
	}
	if err := patchAnthropicMessagesAndModel(obj, annotated, baseline); err != nil {
		return nil, err
	}
	patchAnthropicParams(obj, annotated, baseline)
	if err := patchAnthropicTools(obj, annotated, baseline); err != nil {
		return nil, err
	}
	patchAnthropicCommonFields(obj, annotated, baseline)
	if err := validateAnthropicSupportedFields(annotated, baseline); err != nil {
		return nil, err
	}
	if err := patchAnthropicAPISpecific(obj, annotated.APISpecific, baseline.APISpecific); err != nil {
		return nil, err
	}
	patchExtraFields(obj, baseline.Extra, annotated.Extra)
	return &out, nil
}

// AnthropicMessagesStreamingCodec assembles streamed Anthropic events into one message
type AnthropicMessagesStreamingCodec struct {
	mu    sync.Mutex
	state anthropicStreamingState
}

// Returns an Anthropic Messages streaming codec with empty state
func NewAnthropicMessagesStreamingCodec() *AnthropicMessagesStreamingCodec {
	return &AnthropicMessagesStreamingCodec{}
}

// Returns a collector that folds each event into the shared state
func (c *AnthropicMessagesStreamingCodec) Collector() types.LLMCollectorFn {
	return func(event any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.observe(event)
		return nil
	}
}

// Returns a collector that reads each event without taking ownership
func (c *AnthropicMessagesStreamingCodec) BorrowedCollector() types.LLMBorrowedCollectorFn {
	return func(event any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.observe(event)
		return nil
	}
}

// Returns a finalizer that emits the assembled message and resets the state
func (c *AnthropicMessagesStreamingCodec) Finalizer() types.LLMFinalizerFn {
	return func() any {
		c.mu.Lock()
		state := c.state
		c.state = anthropicStreamingState{}
		c.mu.Unlock()
		return state.finalize()
	}
}

// Optional JSON value that distinguishes a present null from absence
type optionalJSON struct {
	value   any
	present bool
}

// Accumulated Anthropic stream state
type anthropicStreamingState struct {
	id                *string
	kind              *string
	role              *string
	model             *string
	container         optionalJSON
	stopDetails       optionalJSON
	diagnostics       optionalJSON
	serviceTier       optionalJSON
	usage             optionalJSON
	stopReason        *string
	stopSequence      optionalJSON
	contextManagement optionalJSON
	blocks            map[uint64]*anthropicStreamingBlock
}

// Accumulated state of one streamed content block
type anthropicStreamingBlock struct {
	skeleton         map[string]any
	text             strings.Builder
	hasText          bool
	partialJSON      strings.Builder
	hasPartialJSON   bool
	citations        []any
	hasCitations     bool
}

// Folds one stream event into the state
func (s *anthropicStreamingState) observe(event any) {
	kind, _ := getString(event, "type")
	switch kind {
	case "message_start":
		s.observeMessageStart(event)
	case "content_block_start":
		s.observeContentBlockStart(event)
	case "content_block_delta":
		s.observeContentBlockDelta(event)
	case "message_delta":
		s.observeMessageDelta(event)
	}
}

// Records message identity, metadata and initial usage
func (s *anthropicStreamingState) observeMessageStart(event any) {
	message, ok := getValue(event, "message")
	if !ok {
		return
	}
	if id, ok := getString(message, "id"); ok {
		s.id = strPtr(id)
	}
	if model, ok := getString(message, "model"); ok {
		s.model = strPtr(model)
	}
	if role, ok := getString(message, "role"); ok {
		s.role = strPtr(role)
	}
	if kind, ok := getString(message, "type"); ok {
		s.kind = strPtr(kind)
	}
	for _, field := range []struct {
		key    string
		target *optionalJSON
	}{
		{"container", &s.container},
		{"stop_details", &s.stopDetails},
		{"diagnostics", &s.diagnostics},
		{"service_tier", &s.serviceTier},
	} {
		if value, ok := getValue(message, field.key); ok {
			*field.target = optionalJSON{value: cloneValue(value), present: true}
		}
	}
	if usage, ok := getValue(message, "usage"); ok {
		s.observeUsage(usage)
	}
}

// Starts a content block from its skeleton
func (s *anthropicStreamingState) observeContentBlockStart(event any) {
	index, ok := getU64(event, "index")
	if !ok {
		return
	}
	value, ok := getValue(event, "content_block")
	if !ok {
		return
	}
	skeleton, ok := value.(map[string]any)
	if !ok {
		return
	}
	if s.blocks == nil {
		s.blocks = map[uint64]*anthropicStreamingBlock{}
	}
	s.blocks[index] = &anthropicStreamingBlock{skeleton: cloneObject(skeleton)}
}

// Appends a delta to the content block it targets
func (s *anthropicStreamingState) observeContentBlockDelta(event any) {
	index, ok := getU64(event, "index")
	if !ok {
		return
	}
	delta, ok := getValue(event, "delta")
	if !ok {
		return
	}
	deltaType, _ := getString(delta, "type")
	block, ok := s.blocks[index]
	if !ok {
		return
	}
	switch deltaType {
	case "text_delta":
		if text, ok := getString(delta, "text"); ok {
			block.text.WriteString(text)
			block.hasText = true
		}
	case "input_json_delta":
		if partial, ok := getString(delta, "partial_json"); ok {
			block.partialJSON.WriteString(partial)
			block.hasPartialJSON = true
		}
	case "citations_delta":
		if citation, ok := getValue(delta, "citation"); ok {
			block.citations = append(block.citations, cloneValue(citation))
			block.hasCitations = true
		}
	case "compaction_delta":
		if kind, _ := block.skeleton["type"].(string); kind == "compaction" {
			for _, field := range []string{"content", "encrypted_content"} {
				if value, ok := getValue(delta, field); ok {
					block.skeleton[field] = cloneValue(value)
				}
			}
		}
	}
}

// Records stop reason, stop details, usage and context management updates
func (s *anthropicStreamingState) observeMessageDelta(event any) {
	if delta, ok := getValue(event, "delta"); ok {
		if reason, ok := getString(delta, "stop_reason"); ok {
			s.stopReason = strPtr(reason)
		}
		for _, field := range []struct {
			key    string
			target *optionalJSON
		}{
			{"stop_sequence", &s.stopSequence},
			{"container", &s.container},
			{"stop_details", &s.stopDetails},
		} {
			if value, ok := getValue(delta, field.key); ok {
				*field.target = optionalJSON{value: cloneValue(value), present: true}
			}
		}
	}
	if usage, ok := getValue(event, "usage"); ok {
		s.observeUsage(usage)
	}
	if contextManagement, ok := getValue(event, "context_management"); ok {
		s.contextManagement = optionalJSON{value: cloneValue(contextManagement), present: true}
	}
}

// Merges a usage object into the accumulated usage, replacing non-object values
func (s *anthropicStreamingState) observeUsage(usage any) {
	current, currentIsObject := s.usage.value.(map[string]any)
	update, updateIsObject := usage.(map[string]any)
	if s.usage.present && currentIsObject && updateIsObject {
		for key, value := range update {
			current[key] = cloneValue(value)
		}
		return
	}
	s.usage = optionalJSON{value: cloneValue(usage), present: true}
}

// Builds the assembled message object
func (s *anthropicStreamingState) finalize() any {
	output := map[string]any{}
	if s.id != nil {
		output["id"] = *s.id
	}
	if s.kind != nil {
		output["type"] = *s.kind
	}
	if s.role != nil {
		output["role"] = *s.role
	}
	if s.model != nil {
		output["model"] = *s.model
	}
	for _, field := range []struct {
		key   string
		value optionalJSON
	}{
		{"container", s.container},
		{"stop_details", s.stopDetails},
		{"diagnostics", s.diagnostics},
		{"service_tier", s.serviceTier},
	} {
		if field.value.present {
			output[field.key] = field.value.value
		}
	}
	content := []any{}
	for _, index := range sortedU64Keys(s.blocks) {
		content = append(content, s.blocks[index].finalize())
	}
	output["content"] = content
	if s.stopReason != nil {
		output["stop_reason"] = *s.stopReason
	}
	if s.stopSequence.present {
		output["stop_sequence"] = s.stopSequence.value
	}
	if s.usage.present {
		output["usage"] = s.usage.value
	}
	if s.contextManagement.present {
		output["context_management"] = s.contextManagement.value
	}
	return output
}

// Builds the assembled content block from its skeleton and accumulated deltas
func (b *anthropicStreamingBlock) finalize() any {
	if b.hasText {
		b.skeleton["text"] = b.text.String()
	}
	if b.hasPartialJSON && b.partialJSON.Len() > 0 {
		partial := b.partialJSON.String()
		if parsed, err := types.DecodeJSON([]byte(partial)); err == nil {
			b.skeleton["input"] = parsed
		} else {
			b.skeleton["input"] = partial
		}
	}
	if b.hasCitations {
		b.skeleton["citations"] = b.citations
	}
	return b.skeleton
}

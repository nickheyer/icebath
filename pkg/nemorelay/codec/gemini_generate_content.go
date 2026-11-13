package codec

import (
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const geminiProvider = "gemini"

// GeminiGenerateContentCodec translates Gemini generateContent requests and responses
type GeminiGenerateContentCodec struct{}

// Returns the Gemini generateContent codec
func NewGeminiGenerateContentCodec() *GeminiGenerateContentCodec {
	return &GeminiGenerateContentCodec{}
}

var geminiGenerateContentProviderSurface = providerSurfaceDescriptor{
	surface: ProviderSurfaceGeminiGenerateContent,
	detectRequest: func(obj map[string]any, _ *string) bool {
		_, ok := obj["contents"]
		return ok
	},
	detectResponse: detectGeminiResponse,
	decodeRequest: func(request *types.LLMRequest) (*types.AnnotatedLLMRequest, error) {
		return NewGeminiGenerateContentCodec().Decode(request)
	},
	decodeResponse: func(response any) (*types.AnnotatedLLMResponse, error) {
		return NewGeminiGenerateContentCodec().DecodeResponse(response)
	},
	codecName:      "gemini_generate_content",
	requestCodec:   func() types.LLMCodec { return NewGeminiGenerateContentCodec() },
	responseCodec:  func() types.LLMResponseCodec { return NewGeminiGenerateContentCodec() },
	streamingCodec: func() StreamingCodec { return NewGeminiGenerateContentStreamingCodec() },
}

var geminiModeledRequestKeys = []string{"contents", "systemInstruction", "tools", "generationConfig", "model"}

// Identifies the builtin Gemini generateContent codec
func (*GeminiGenerateContentCodec) CodecIdentity() types.LLMCodecIdentity {
	return types.LLMCodecIdentity{Kind: types.LLMCodecIdentityBuiltIn, Builtin: types.BuiltinLLMCodecGeminiGenerateContent}
}

// Raw Gemini response fields read from a response
type rawGeminiResponse struct {
	candidates    []rawGeminiCandidate
	usageMetadata *rawGeminiUsageMetadata
	modelVersion  *string
	responseID    *string
	extra         map[string]any
}

// Raw Gemini candidate fields
type rawGeminiCandidate struct {
	content      *rawGeminiContent
	finishReason *string
	extra        map[string]any
}

// Raw Gemini candidate content fields
type rawGeminiContent struct {
	parts    []any
	hasParts bool
}

// Raw Gemini usage metadata counters
type rawGeminiUsageMetadata struct {
	promptTokenCount        *uint64
	candidatesTokenCount    *uint64
	totalTokenCount         *uint64
	cachedContentTokenCount *uint64
	thoughtsTokenCount      *uint64
	toolUsePromptTokenCount *uint64
}

// Decodes a token count that may arrive as an integral float
func decodeGeminiTokenCount(value any) (*uint64, error) {
	if value == nil {
		return nil, nil
	}
	n := serdeNumberOf(value)
	switch n.kind {
	case serdeNumNone:
		return nil, serdeInvalidType(value, "a JSON number")
	case serdeNumPosInt:
		return u64Ptr(n.u), nil
	}
	if !math.IsInf(n.f, 0) && !math.IsNaN(n.f) && n.f >= 0 && n.f < float64(math.MaxUint64) && n.f == math.Trunc(n.f) {
		return u64Ptr(uint64(n.f)), nil
	}
	return nil, errors.New("token count must be an integer in the u64 range")
}

// Decodes Gemini usage metadata with Rust deserialization semantics
func decodeRawGeminiUsageMetadata(value any) (*rawGeminiUsageMetadata, error) {
	meta := &rawGeminiUsageMetadata{}
	counter := func(key string, target **uint64) serdeField {
		return serdeField{key: key, optional: true, defaulted: true, set: func(v any) (err error) {
			*target, err = decodeGeminiTokenCount(v)
			return
		}}
	}
	err := serdeStruct(value, "RawUsageMetadata", []serdeField{
		counter("promptTokenCount", &meta.promptTokenCount),
		counter("candidatesTokenCount", &meta.candidatesTokenCount),
		counter("totalTokenCount", &meta.totalTokenCount),
		counter("cachedContentTokenCount", &meta.cachedContentTokenCount),
		counter("thoughtsTokenCount", &meta.thoughtsTokenCount),
		counter("toolUsePromptTokenCount", &meta.toolUsePromptTokenCount),
	}, false, nil)
	return meta, err
}

// Decodes a Gemini candidate with Rust deserialization semantics
func decodeRawGeminiCandidate(value any) (rawGeminiCandidate, error) {
	candidate := rawGeminiCandidate{extra: map[string]any{}}
	err := serdeStruct(value, "RawCandidate", []serdeField{
		{key: "content", optional: true, set: func(v any) error {
			content := &rawGeminiContent{}
			candidate.content = content
			return serdeStruct(v, "RawContent", []serdeField{
				{key: "parts", optional: true, set: func(v any) (err error) {
					content.hasParts = true
					content.parts, err = serdeValues(v)
					return
				}},
			}, false, nil)
		}},
		{key: "finishReason", optional: true, set: func(v any) (err error) { candidate.finishReason, err = serdeOptString(v); return }},
	}, true, candidate.extra)
	for key, v := range candidate.extra {
		candidate.extra[key] = cloneValue(v)
	}
	if candidate.content != nil {
		candidate.content.parts = cloneSlice(candidate.content.parts)
	}
	return candidate, err
}

// Decodes a Gemini response with Rust deserialization semantics
func decodeRawGeminiResponse(value any) (rawGeminiResponse, error) {
	raw := rawGeminiResponse{extra: map[string]any{}}
	err := serdeStruct(value, "RawGeminiGenerateContentResponse", []serdeField{
		{key: "candidates", optional: true, set: func(v any) error {
			raw.candidates = []rawGeminiCandidate{}
			return serdeSeq(v, func(item any) error {
				candidate, err := decodeRawGeminiCandidate(item)
				raw.candidates = append(raw.candidates, candidate)
				return err
			})
		}},
		{key: "usageMetadata", optional: true, set: func(v any) (err error) {
			raw.usageMetadata, err = decodeRawGeminiUsageMetadata(v)
			return
		}},
		{key: "modelVersion", optional: true, set: func(v any) (err error) { raw.modelVersion, err = serdeOptString(v); return }},
		{key: "responseId", optional: true, set: func(v any) (err error) { raw.responseID, err = serdeOptString(v); return }},
	}, true, raw.extra)
	for key, v := range raw.extra {
		raw.extra[key] = cloneValue(v)
	}
	return raw, err
}

// Maps a candidate finish reason, treating function calls as tool use
func mapGeminiFinishReason(reason *string, hasToolCalls bool) *types.FinishReason {
	kind := func(k types.FinishReasonKind) *types.FinishReason { return &types.FinishReason{Kind: k} }
	if reason == nil || *reason == "" || *reason == "FINISH_REASON_UNSPECIFIED" {
		if hasToolCalls {
			return kind(types.FinishReasonToolUse)
		}
		return nil
	}
	switch *reason {
	case "STOP":
		if hasToolCalls {
			return kind(types.FinishReasonToolUse)
		}
		return kind(types.FinishReasonComplete)
	case "TOOL_CODE":
		return kind(types.FinishReasonToolUse)
	case "MAX_TOKENS":
		return kind(types.FinishReasonLength)
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "LANGUAGE",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "ESCALATION":
		return kind(types.FinishReasonContentFilter)
	}
	return &types.FinishReason{Kind: types.FinishReasonUnknown, Unknown: *reason}
}

// Maps a prompt feedback block reason to a finish reason
func mapGeminiPromptBlockReason(reason *string) *types.FinishReason {
	if reason == nil || *reason == "" || *reason == "BLOCK_REASON_UNSPECIFIED" {
		return nil
	}
	switch *reason {
	case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY":
		return &types.FinishReason{Kind: types.FinishReasonContentFilter}
	}
	return &types.FinishReason{Kind: types.FinishReasonUnknown, Unknown: *reason}
}

// Reports whether an object looks like a Gemini generateContent response
func detectGeminiResponse(obj map[string]any) bool {
	if _, ok := obj["candidates"].([]any); ok {
		return true
	}
	feedback, ok := obj["promptFeedback"].(map[string]any)
	if !ok {
		return false
	}
	_, hasBlockReason := feedback["blockReason"]
	_, hasSafetyRatings := feedback["safetyRatings"]
	return hasBlockReason || hasSafetyRatings
}

// Reads the prompt feedback block reason from response extras
func geminiPromptFeedbackBlockReason(extra map[string]any) (*string, error) {
	value, ok := extra["promptFeedback"]
	if !ok {
		return nil, nil
	}
	feedback, ok := value.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("Gemini response promptFeedback must be an object")
	}
	reason, ok := feedback["blockReason"]
	if !ok {
		return nil, nil
	}
	text, ok := reason.(string)
	if !ok {
		return nil, types.InvalidArgumentf("Gemini response promptFeedback.blockReason must be a string")
	}
	return &text, nil
}

// Reads the tool call id of a serialized normalized tool message
func geminiExtractToolCallID(obj map[string]any) (string, error) {
	value, ok := obj["tool_call_id"]
	if !ok {
		return "", types.Internalf("Message::Tool has no tool_call_id")
	}
	id, isString := value.(string)
	if !isString {
		return "", types.InvalidArgumentf("Gemini encoder: Message::Tool tool_call_id must be a string")
	}
	if id == "" {
		return "", types.InvalidArgumentf("Gemini encoder: Message::Tool has an empty tool_call_id")
	}
	return id, nil
}

// Reads an optional non-empty function id
func geminiParseOptionalID(obj map[string]any, context string) (*string, error) {
	value, ok := obj["id"]
	if !ok {
		return nil, nil
	}
	id, isString := value.(string)
	if !isString {
		return nil, types.InvalidArgumentf("Gemini generateContent %s.id must be a string", context)
	}
	if id == "" {
		return nil, types.InvalidArgumentf("Gemini generateContent %s.id must be a non-empty string", context)
	}
	return &id, nil
}

// Records a function id, rejecting duplicates within one content item
func ensureUniqueGeminiFunctionID(seen map[string]bool, id, partKind string) error {
	if !seen[id] {
		seen[id] = true
		return nil
	}
	return types.InvalidArgumentf("Gemini %s parts have duplicate effective id '%s'; distinct ids are required to correlate function responses", partKind, id)
}

// Reports whether a part key carries part data
func isGeminiPartDataKey(key string) bool {
	switch key {
	case "text", "inlineData", "fileData", "functionCall", "functionResponse", "executableCode", "codeExecutionResult":
		return true
	}
	return false
}

// Returns the sorted data keys present in a part
func geminiPartDataKeys(obj map[string]any) []string {
	var keys []string
	for _, key := range sortedKeys(obj) {
		if isGeminiPartDataKey(key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// Returns the single data key of a part, rejecting parts with several
func validateSingleGeminiPartDataField(obj map[string]any, context string) (string, bool, error) {
	keys := geminiPartDataKeys(obj)
	if len(keys) > 1 {
		return "", false, types.InvalidArgumentf("Gemini generateContent %s part must not contain multiple data fields: %s", context, strings.Join(keys, ", "))
	}
	if len(keys) == 0 {
		return "", false, nil
	}
	return keys[0], true, nil
}

// Converts parts to message content, skipping thoughts and function parts
func geminiPartsToMessageContent(parts []any, context string) (types.MessageContent, error) {
	var texts []string
	contentParts := types.MessageContentParts{}
	requiresParts := false
	for _, part := range parts {
		obj, ok := part.(map[string]any)
		if !ok {
			return nil, types.InvalidArgumentf("Gemini generateContent %s parts entry must be an object", context)
		}
		if thought, _ := obj["thought"].(bool); thought {
			continue
		}
		dataKey, has, err := validateSingleGeminiPartDataField(obj, context)
		if err != nil {
			return nil, err
		}
		if !has {
			requiresParts = true
			contentParts = append(contentParts, types.ContentPartProviderNative{Provider: geminiProvider, Kind: "unknown", Value: cloneValue(part)})
			continue
		}
		switch dataKey {
		case "functionCall", "functionResponse":
			continue
		case "text":
			text, ok := obj["text"].(string)
			if !ok {
				return nil, types.InvalidArgumentf("Gemini generateContent %s parts[].text must be a string", context)
			}
			extra := objectWithout(obj, "text")
			if len(extra) > 0 {
				requiresParts = true
			}
			texts = append(texts, text)
			contentParts = append(contentParts, types.ContentPartText{Text: text, Extra: extra})
		default:
			requiresParts = true
			contentParts = append(contentParts, types.ContentPartProviderNative{Provider: geminiProvider, Kind: dataKey, Value: cloneValue(part)})
		}
	}
	switch {
	case len(contentParts) == 0:
		return nil, nil
	case requiresParts:
		return contentParts, nil
	}
	return types.MessageContentText(strings.Join(texts, "\n")), nil
}

// Validates one functionResponse.parts entry and returns its data kind
func validateGeminiNestedFunctionResponsePart(part any) (string, error) {
	obj, ok := part.(map[string]any)
	if !ok {
		return "", types.InvalidArgumentf("Gemini functionResponse.parts entry must be an object")
	}
	dataKey, has, err := validateSingleGeminiPartDataField(obj, "functionResponse.parts")
	if err != nil {
		return "", err
	}
	if has && (dataKey == "functionCall" || dataKey == "functionResponse") {
		return "", types.InvalidArgumentf("Gemini functionResponse.parts must not contain nested functionCall/functionResponse")
	}
	if has && dataKey == "text" {
		if value, present := obj["text"]; present {
			if _, isString := value.(string); !isString {
				return "", types.InvalidArgumentf("Gemini functionResponse.parts[].text must be a string")
			}
		}
	}
	if !has {
		return "unknown", nil
	}
	return dataKey, nil
}

// Converts a functionResponse into tool message content with its nested parts
func geminiFunctionResponseToMessageContent(fr any) (types.MessageContent, error) {
	response, ok := getValue(fr, "response")
	if !ok {
		return nil, types.InvalidArgumentf("Gemini functionResponse is missing required 'response'")
	}
	contentStr, err := types.EncodeJSONString(response)
	if err != nil {
		contentStr = "{}"
	}
	partsValue, ok := getValue(fr, "parts")
	if !ok {
		return types.MessageContentText(contentStr), nil
	}
	nested, ok := partsValue.([]any)
	if !ok {
		return nil, types.InvalidArgumentf("Gemini functionResponse.parts must be an array")
	}
	parts := make(types.MessageContentParts, 0, len(nested)+1)
	parts = append(parts, types.ContentPartText{Text: contentStr, Extra: map[string]any{}})
	for _, nestedPart := range nested {
		kind, err := validateGeminiNestedFunctionResponsePart(nestedPart)
		if err != nil {
			return nil, err
		}
		parts = append(parts, types.ContentPartProviderNative{Provider: geminiProvider, Kind: kind, Value: cloneValue(nestedPart)})
	}
	return parts, nil
}

// Converts response parts to message content, rejecting function responses
func geminiExtractPartsMessageContent(parts []any) (types.MessageContent, error) {
	for _, part := range parts {
		if _, ok := getValue(part, "functionResponse"); ok {
			return nil, types.InvalidArgumentf("Gemini response parts must not contain functionResponse")
		}
	}
	return geminiPartsToMessageContent(parts, "response")
}

// Collects response function calls as tool calls with unique effective ids
func geminiExtractPartsToolCalls(parts []any) ([]types.ResponseToolCall, error) {
	var calls []types.ResponseToolCall
	seen := map[string]bool{}
	for _, part := range parts {
		fc, ok := getValue(part, "functionCall")
		if !ok {
			continue
		}
		fcObj, ok := fc.(map[string]any)
		if !ok {
			return nil, types.InvalidArgumentf("Gemini response functionCall must be an object")
		}
		rawName, ok := fcObj["name"]
		if !ok {
			return nil, types.InvalidArgumentf("Gemini response functionCall is missing 'name'")
		}
		name, isString := rawName.(string)
		if !isString {
			return nil, types.InvalidArgumentf("Gemini response functionCall.name must be a string")
		}
		if name == "" {
			return nil, types.InvalidArgumentf("Gemini response functionCall.name must be a non-empty string")
		}
		idPtr, err := geminiParseOptionalID(fcObj, "response functionCall")
		if err != nil {
			return nil, err
		}
		id := name
		if idPtr != nil {
			id = *idPtr
		}
		if err := ensureUniqueGeminiFunctionID(seen, id, "functionCall"); err != nil {
			return nil, err
		}
		var arguments any = map[string]any{}
		if args, ok := fcObj["args"]; ok {
			argsObj, isObject := args.(map[string]any)
			if !isObject {
				return nil, types.InvalidArgumentf("Gemini response functionCall.args must be an object")
			}
			arguments = cloneObject(argsObj)
		}
		calls = append(calls, types.ResponseToolCall{ID: id, Name: name, Arguments: arguments})
	}
	return calls, nil
}

// Builds normalized usage and the separately billed counters from usage metadata
func mapGeminiUsage(meta *rawGeminiUsageMetadata, model *string, toolUseBilling toolUsePromptBilling) (*types.Usage, separateUsageCounts) {
	modelProvider := InferModelProvider("google", model)
	if meta == nil {
		return nil, separateUsageCounts{}
	}
	separate := separateUsageCounts{thoughtsTokens: meta.thoughtsTokenCount, toolUsePromptTokens: meta.toolUsePromptTokenCount}
	prompt := meta.promptTokenCount
	completion := meta.candidatesTokenCount
	total := meta.totalTokenCount
	if total == nil {
		counts := []*uint64{prompt, completion, separate.thoughtsTokens, separate.toolUsePromptTokens}
		var sum uint64
		anyCount := false
		overflow := false
		for _, count := range counts {
			if count == nil {
				continue
			}
			anyCount = true
			if sum > math.MaxUint64-*count {
				overflow = true
				break
			}
			sum += *count
		}
		if anyCount && !overflow {
			total = u64Ptr(sum)
		}
	}
	usage := &types.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		CacheReadTokens:  meta.cachedContentTokenCount,
	}
	if prompt != nil && meta.cachedContentTokenCount != nil && *prompt >= *meta.cachedContentTokenCount {
		usage.UncachedInputTokens = u64Ptr(*prompt - *meta.cachedContentTokenCount)
	}
	if model != nil {
		billed, err := computeBilledUsage(usage, &geminiBilling{separate: separate, toolUseBilling: toolUseBilling})
		if err == nil {
			usage.Cost = estimateBilledCostForProvider(modelProvider, *model, billed)
		}
	}
	return usage, separate
}

// Validates a systemInstruction object holding only text parts
func validateGeminiSystemInstruction(value any) error {
	obj, ok := value.(map[string]any)
	if !ok {
		return types.InvalidArgumentf("Gemini systemInstruction must be an object")
	}
	if role, ok := obj["role"]; ok {
		if _, isString := role.(string); !isString {
			return types.InvalidArgumentf("Gemini systemInstruction.role must be a string")
		}
	}
	rawParts, ok := obj["parts"]
	if !ok {
		return types.InvalidArgumentf("Gemini systemInstruction must have a 'parts' field")
	}
	parts, ok := rawParts.([]any)
	if !ok {
		return types.InvalidArgumentf("Gemini systemInstruction.parts must be an array")
	}
	for _, part := range parts {
		partObj, ok := part.(map[string]any)
		if !ok {
			return types.InvalidArgumentf("Gemini systemInstruction.parts entry must be an object")
		}
		dataKey, has, err := validateSingleGeminiPartDataField(partObj, "systemInstruction")
		if err != nil {
			return err
		}
		if !has || dataKey != "text" {
			return types.InvalidArgumentf("Gemini systemInstruction.parts entries must be text parts")
		}
		if _, isString := partObj["text"].(string); !isString {
			return types.InvalidArgumentf("Gemini systemInstruction.parts[].text must be a string")
		}
	}
	return nil
}

// Joins the text parts of a systemInstruction, returning nil when empty
func geminiSystemInstructionText(value any) *string {
	rawParts, _ := getValue(value, "parts")
	parts, ok := rawParts.([]any)
	if !ok {
		return nil
	}
	var texts []string
	for _, part := range parts {
		if text, ok := getString(part, "text"); ok {
			texts = append(texts, text)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" {
		return nil
	}
	return &text
}

// Converts one contents item into normalized messages
func geminiContentToMessages(content any) ([]types.Message, error) {
	obj, ok := content.(map[string]any)
	if !ok {
		return nil, types.InvalidArgumentf("Gemini contents item must be an object")
	}
	role, err := geminiContentRole(obj)
	if err != nil {
		return nil, err
	}
	parts, ok := obj["parts"].([]any)
	if !ok {
		return nil, types.InvalidArgumentf("Gemini contents item must have an array 'parts' field")
	}
	responses, calls, err := validateGeminiContentParts(parts)
	if err != nil {
		return nil, err
	}
	if err := validateGeminiContentRoles(role, responses, calls); err != nil {
		return nil, err
	}
	if len(responses) > 0 {
		return geminiFunctionResponseMessages(parts, responses)
	}
	messageContent, err := geminiPartsToMessageContent(parts, "request")
	if err != nil {
		return nil, err
	}
	if len(calls) > 0 {
		return geminiFunctionCallMessages(messageContent, calls)
	}
	return []types.Message{geminiPlainMessage(role, messageContent)}, nil
}

// Returns the role of a contents item, defaulting to user
func geminiContentRole(obj map[string]any) (string, error) {
	value, ok := obj["role"]
	if !ok {
		return "user", nil
	}
	role, isString := value.(string)
	if !isString {
		return "", types.InvalidArgumentf("Gemini contents item 'role' must be a string")
	}
	if role != "user" && role != "model" {
		return "", types.InvalidArgumentf("Gemini contents item has unsupported role '%s'; expected 'user' or 'model'", role)
	}
	return role, nil
}

// Validates content parts and splits out function response and function call parts
func validateGeminiContentParts(parts []any) ([]any, []any, error) {
	var responses, calls []any
	for _, part := range parts {
		obj, ok := part.(map[string]any)
		if !ok {
			return nil, nil, types.InvalidArgumentf("Gemini parts item must be an object")
		}
		dataKey, has, err := validateSingleGeminiPartDataField(obj, "request")
		if err != nil {
			return nil, nil, err
		}
		if !has {
			continue
		}
		switch dataKey {
		case "functionResponse":
			if err := validateGeminiFunctionResponsePart(part); err != nil {
				return nil, nil, err
			}
			responses = append(responses, part)
		case "functionCall":
			if err := validateGeminiFunctionCallPart(part); err != nil {
				return nil, nil, err
			}
			calls = append(calls, part)
		case "text":
			if _, isString := obj["text"].(string); !isString {
				return nil, nil, types.InvalidArgumentf("Gemini parts item 'text' must be a string")
			}
		}
	}
	if len(responses) > 0 && len(calls) > 0 {
		return nil, nil, types.InvalidArgumentf("Gemini contents item must not contain both functionResponse and functionCall parts")
	}
	return responses, calls, nil
}

// Validates a functionResponse part
func validateGeminiFunctionResponsePart(part any) error {
	value, _ := getValue(part, "functionResponse")
	response, ok := value.(map[string]any)
	if !ok {
		return types.InvalidArgumentf("Gemini functionResponse must be an object")
	}
	if name, _ := response["name"].(string); name == "" {
		return types.InvalidArgumentf("Gemini functionResponse is missing a non-empty 'name'")
	}
	body, ok := response["response"]
	if !ok {
		return types.InvalidArgumentf("Gemini functionResponse is missing required 'response'")
	}
	if _, isObject := body.(map[string]any); !isObject {
		return types.InvalidArgumentf("Gemini functionResponse.response must be an object")
	}
	if rawParts, ok := response["parts"]; ok {
		parts, ok := rawParts.([]any)
		if !ok {
			return types.InvalidArgumentf("Gemini functionResponse.parts must be an array")
		}
		for _, nested := range parts {
			if _, err := validateGeminiNestedFunctionResponsePart(nested); err != nil {
				return err
			}
		}
	}
	_, err := geminiParseOptionalID(response, "functionResponse")
	return err
}

// Validates a functionCall part
func validateGeminiFunctionCallPart(part any) error {
	value, _ := getValue(part, "functionCall")
	call, ok := value.(map[string]any)
	if !ok {
		return types.InvalidArgumentf("Gemini functionCall must be an object")
	}
	if name, _ := call["name"].(string); name == "" {
		return types.InvalidArgumentf("Gemini functionCall is missing a non-empty 'name'")
	}
	if args, ok := call["args"]; ok {
		if _, isObject := args.(map[string]any); !isObject {
			return types.InvalidArgumentf("Gemini functionCall.args must be an object")
		}
	}
	return nil
}

// Requires function responses in user items and function calls in model items
func validateGeminiContentRoles(role string, responses, calls []any) error {
	if len(responses) > 0 && role != "user" {
		return types.InvalidArgumentf("Gemini functionResponse parts must be in a 'user' role content item, got '%s'", role)
	}
	if len(calls) > 0 && role != "model" {
		return types.InvalidArgumentf("Gemini functionCall parts must be in a 'model' role content item, got '%s'", role)
	}
	return nil
}

// Converts a user item with function responses into an optional user message and tool messages
func geminiFunctionResponseMessages(parts []any, responses []any) ([]types.Message, error) {
	messages := make([]types.Message, 0, len(responses)+1)
	content, err := geminiPartsToMessageContent(parts, "request")
	if err != nil {
		return nil, err
	}
	if content != nil {
		messages = append(messages, types.MessageUser{Content: content})
	}
	seen := map[string]bool{}
	for _, part := range responses {
		value, _ := getValue(part, "functionResponse")
		response := value.(map[string]any)
		name, _ := response["name"].(string)
		idPtr, err := geminiParseOptionalID(response, "functionResponse")
		if err != nil {
			return nil, err
		}
		id := name
		if idPtr != nil {
			id = *idPtr
		}
		if err := ensureUniqueGeminiFunctionID(seen, id, "functionResponse"); err != nil {
			return nil, err
		}
		toolContent, err := geminiFunctionResponseToMessageContent(value)
		if err != nil {
			return nil, err
		}
		messages = append(messages, types.MessageTool{Content: toolContent, ToolCallID: id})
	}
	return messages, nil
}

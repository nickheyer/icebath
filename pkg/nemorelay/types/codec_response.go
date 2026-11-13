package types

import (
	"fmt"
	"strings"
)

// AnnotatedLLMResponse is the provider neutral view of an LLM response
type AnnotatedLLMResponse struct {
	ID                  *string                 `json:"id" serde:"skip_none"`
	Model               *string                 `json:"model" serde:"skip_none"`
	Message             MessageContent          `json:"message" serde:"option,skip_none"`
	ToolCalls           []ResponseToolCall      `json:"tool_calls" serde:"option,skip_none"`
	FinishReason        *FinishReason           `json:"finish_reason" serde:"skip_none"`
	Usage               *Usage                  `json:"usage" serde:"skip_none"`
	OptimizationSummary *LLMOptimizationSummary `json:"optimization_summary" serde:"skip_none"`
	APISpecific         APISpecificResponse     `json:"api_specific" serde:"option,skip_none"`
	Extra               map[string]any          `json:"extra" serde:"flatten"`
}

func (AnnotatedLLMResponse) rustName() string { return "AnnotatedLlmResponse" }

// MarshalJSON writes the serde form
func (r AnnotatedLLMResponse) MarshalJSON() ([]byte, error) { return EncodeJSON(r) }

// UnmarshalJSON reads the serde form
func (r *AnnotatedLLMResponse) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, r) }

// Clone deep copies the response through its data model
func (r *AnnotatedLLMResponse) Clone() *AnnotatedLLMResponse {
	if r == nil {
		return nil
	}
	out := cloneDataModel(*r)
	return &out
}

// ResponseText returns the first text-bearing part of the response message
func (r *AnnotatedLLMResponse) ResponseText() (string, bool) {
	if r.Message == nil {
		return "", false
	}
	return firstContentText(r.Message)
}

// HasToolCalls reports whether the response carries at least one tool call
func (r *AnnotatedLLMResponse) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// Usage holds token counts and an optional cost estimate
type Usage struct {
	PromptTokens        *uint64       `json:"prompt_tokens" serde:"skip_none"`
	CompletionTokens    *uint64       `json:"completion_tokens" serde:"skip_none"`
	TotalTokens         *uint64       `json:"total_tokens" serde:"skip_none"`
	CacheReadTokens     *uint64       `json:"cache_read_tokens" serde:"skip_none"`
	CacheWriteTokens    *uint64       `json:"cache_write_tokens" serde:"skip_none"`
	UncachedInputTokens *uint64       `json:"uncached_input_tokens" serde:"skip_none"`
	Cost                *CostEstimate `json:"cost" serde:"skip_none"`
}

// Clone deep copies the usage
func (u *Usage) Clone() *Usage {
	if u == nil {
		return nil
	}
	out := cloneDataModel(*u)
	return &out
}

// MarshalJSON writes the serde form
func (u Usage) MarshalJSON() ([]byte, error) { return EncodeJSON(u) }

// UnmarshalJSON reads the serde form
func (u *Usage) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, u) }

// CostSource says where a cost figure came from
type CostSource uint8

const (
	CostSourceModelPricing CostSource = iota + 1
	CostSourceProviderReported
)

var costSourceNames = []string{"model_pricing", "provider_reported"}

// String returns the snake_case name
func (s CostSource) String() string {
	if s < 1 || int(s) > len(costSourceNames) {
		return ""
	}
	return costSourceNames[s-1]
}

func (s CostSource) rustDebug(b *strings.Builder) {
	switch s {
	case CostSourceModelPricing:
		b.WriteString("ModelPricing")
	case CostSourceProviderReported:
		b.WriteString("ProviderReported")
	}
}

func (s CostSource) serdeSerialize() (any, error) { return s.String(), nil }

func (s *CostSource) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, costSourceNames)
	if err != nil {
		return err
	}
	*s = CostSource(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (s CostSource) MarshalJSON() ([]byte, error) { return EncodeJSON(s) }

// UnmarshalJSON reads the serde form
func (s *CostSource) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, s) }

// CostEstimate is a monetary cost with component breakdown and pricing provenance
type CostEstimate struct {
	Total           *float64   `json:"total" serde:"skip_none"`
	Currency        string     `json:"currency" serde:"default=USD"`
	Input           *float64   `json:"input" serde:"skip_none"`
	Output          *float64   `json:"output" serde:"skip_none"`
	CacheRead       *float64   `json:"cache_read" serde:"skip_none"`
	CacheWrite      *float64   `json:"cache_write" serde:"skip_none"`
	Source          CostSource `json:"source"`
	PricingProvider *string    `json:"pricing_provider" serde:"skip_none"`
	PricingModel    *string    `json:"pricing_model" serde:"skip_none"`
	PricingAsOf     *string    `json:"pricing_as_of" serde:"skip_none"`
	PricingSource   *string    `json:"pricing_source" serde:"skip_none"`
}

// MarshalJSON writes the serde form
func (c CostEstimate) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *CostEstimate) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// TotalOrComponentSum returns the total, or for provider reported costs the sum of present components
func (c *CostEstimate) TotalOrComponentSum() *float64 {
	if c.Total != nil {
		total := *c.Total
		return &total
	}
	if c.Source != CostSourceProviderReported {
		return nil
	}
	has := false
	total := 0.0
	for _, part := range []*float64{c.Input, c.Output, c.CacheRead, c.CacheWrite} {
		if part != nil {
			has = true
			total += *part
		}
	}
	if !has {
		return nil
	}
	return &total
}

// TotalForCurrency returns the total when the currency matches ignoring ASCII case
func (c *CostEstimate) TotalForCurrency(currency string) *float64 {
	if asciiLower(c.Currency) != asciiLower(currency) || c.Total == nil {
		return nil
	}
	total := *c.Total
	return &total
}

// TotalOrComponentSumForCurrency returns TotalOrComponentSum when the currency matches ignoring ASCII case
func (c *CostEstimate) TotalOrComponentSumForCurrency(currency string) *float64 {
	if asciiLower(c.Currency) != asciiLower(currency) {
		return nil
	}
	return c.TotalOrComponentSum()
}

// Lowercases only ASCII letters as Rust eq_ignore_ascii_case compares
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// EqualFoldASCII compares strings ignoring ASCII case only
func EqualFoldASCII(a, b string) bool {
	return asciiLower(a) == asciiLower(b)
}

// FinishReasonKind selects the finish reason variant
type FinishReasonKind uint8

const (
	FinishReasonComplete FinishReasonKind = iota + 1
	FinishReasonLength
	FinishReasonToolUse
	FinishReasonContentFilter
	FinishReasonUnknown
)

// FinishReason says why generation stopped, Unknown carrying the provider text
type FinishReason struct {
	Kind    FinishReasonKind
	Unknown string
}

// NewFinishReason builds a unit finish reason
func NewFinishReason(kind FinishReasonKind) *FinishReason {
	return &FinishReason{Kind: kind}
}

// UnknownFinishReason builds the Unknown finish reason carrying provider text
func UnknownFinishReason(text string) *FinishReason {
	return &FinishReason{Kind: FinishReasonUnknown, Unknown: text}
}

// IsComplete reports whether generation finished normally
func (r FinishReason) IsComplete() bool { return r.Kind == FinishReasonComplete }

var finishReasonNames = []string{"complete", "length", "tool_use", "content_filter", "unknown"}

func (r FinishReason) serdeSerialize() (any, error) {
	if r.Kind == FinishReasonUnknown {
		return orderedObject{}.with("unknown", r.Unknown), nil
	}
	if r.Kind < 1 || int(r.Kind) > len(finishReasonNames) {
		return nil, fmt.Errorf("finish reason holds no variant")
	}
	return finishReasonNames[r.Kind-1], nil
}

func (r *FinishReason) serdeDeserialize(v any) error {
	name, content, has, err := splitExternalTag(v)
	if err != nil {
		return err
	}
	idx, err := variantIndex(name, finishReasonNames)
	if err != nil {
		return err
	}
	kind := FinishReasonKind(idx + 1)
	if kind == FinishReasonUnknown {
		if !has {
			return fmt.Errorf("invalid type: unit variant, expected newtype variant")
		}
		s, ok := content.(string)
		if !ok {
			return InvalidTypeError(content, "a string")
		}
		*r = FinishReason{Kind: kind, Unknown: s}
		return nil
	}
	if err := unitVariantContent(content, has); err != nil {
		return err
	}
	*r = FinishReason{Kind: kind}
	return nil
}

func (r FinishReason) rustDebug(b *strings.Builder) {
	switch r.Kind {
	case FinishReasonComplete:
		b.WriteString("Complete")
	case FinishReasonLength:
		b.WriteString("Length")
	case FinishReasonToolUse:
		b.WriteString("ToolUse")
	case FinishReasonContentFilter:
		b.WriteString("ContentFilter")
	case FinishReasonUnknown:
		b.WriteString("Unknown(" + RustStrDebug(r.Unknown) + ")")
	}
}

// MarshalJSON writes the serde form
func (r FinishReason) MarshalJSON() ([]byte, error) { return EncodeJSON(r) }

// UnmarshalJSON reads the serde form
func (r *FinishReason) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, r) }

// ResponseToolCall is a tool call the model produced
type ResponseToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

// MarshalJSON writes the serde form
func (c ResponseToolCall) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *ResponseToolCall) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// APISpecificResponse carries response fields only one provider API knows, tagged by api
type APISpecificResponse interface {
	isAPISpecificResponse()
}

// APISpecificResponseOpenAIChat holds OpenAI Chat only response fields
type APISpecificResponseOpenAIChat struct {
	Logprobs          any     `json:"logprobs" serde:"option,skip_none"`
	LogprobsPresent   bool    `json:"-"`
	SystemFingerprint *string `json:"system_fingerprint" serde:"skip_none"`
	ServiceTier       *string `json:"service_tier" serde:"skip_none"`
}

// APISpecificResponseOpenAIResponses holds OpenAI Responses only response fields
type APISpecificResponseOpenAIResponses struct {
	OutputItems                []any   `json:"output_items" serde:"option,skip_none"`
	Status                     *string `json:"status" serde:"skip_none"`
	IncompleteDetails          any     `json:"incomplete_details" serde:"option,skip_none"`
	IncompleteDetailsPresent   bool    `json:"-"`
	PreviousResponseID         *string `json:"previous_response_id" serde:"skip_none"`
	Store                      *bool   `json:"store" serde:"skip_none"`
	ServiceTier                *string `json:"service_tier" serde:"skip_none"`
	Truncation                 any     `json:"truncation" serde:"option,skip_none"`
	TruncationPresent          bool    `json:"-"`
	Reasoning                  any     `json:"reasoning" serde:"option,skip_none"`
	ReasoningPresent           bool    `json:"-"`
	InputTokensDetails         any     `json:"input_tokens_details" serde:"option,skip_none"`
	InputTokensDetailsPresent  bool    `json:"-"`
	OutputTokensDetails        any     `json:"output_tokens_details" serde:"option,skip_none"`
	OutputTokensDetailsPresent bool    `json:"-"`
}

// APISpecificResponseAnthropicMessages holds Anthropic Messages only response fields
type APISpecificResponseAnthropicMessages struct {
	ObjectType       *string `json:"object_type" serde:"skip_none"`
	Role             *string `json:"role" serde:"skip_none"`
	StopReason       *string `json:"stop_reason" serde:"skip_none"`
	StopSequence     *string `json:"stop_sequence" serde:"skip_none"`
	ServiceTier      *string `json:"service_tier" serde:"skip_none"`
	Container        any     `json:"container" serde:"option,skip_none"`
	ContainerPresent bool    `json:"-"`
	ContentBlocks    []any   `json:"content_blocks" serde:"option,skip_none"`
}

// APISpecificResponseOCIGenAI holds OCI Generative AI only response fields
type APISpecificResponseOCIGenAI struct {
	APIFormat    *string `json:"api_format" serde:"skip_none"`
	ModelVersion *string `json:"model_version" serde:"skip_none"`
}

// APISpecificResponseGeminiGenerateContent holds Gemini generateContent only response fields
type APISpecificResponseGeminiGenerateContent struct {
	ThoughtsTokens            *uint64        `json:"thoughts_tokens" serde:"skip_none"`
	ToolUsePromptTokens       *uint64        `json:"tool_use_prompt_tokens" serde:"skip_none"`
	SafetyRatings             any            `json:"safety_ratings" serde:"option,skip_none"`
	SafetyRatingsPresent      bool           `json:"-"`
	GroundingMetadata         any            `json:"grounding_metadata" serde:"option,skip_none"`
	GroundingMetadataPresent  bool           `json:"-"`
	CitationMetadata          any            `json:"citation_metadata" serde:"option,skip_none"`
	CitationMetadataPresent   bool           `json:"-"`
	Extra                     map[string]any `json:"extra" serde:"default,flatten"`
}

// APISpecificResponseCustom holds response fields of a custom API
type APISpecificResponseCustom struct {
	APIName string `json:"api_name"`
	Data    any    `json:"data"`
}

func (APISpecificResponseOpenAIChat) isAPISpecificResponse()            {}
func (APISpecificResponseOpenAIResponses) isAPISpecificResponse()       {}
func (APISpecificResponseAnthropicMessages) isAPISpecificResponse()     {}
func (APISpecificResponseOCIGenAI) isAPISpecificResponse()              {}
func (APISpecificResponseGeminiGenerateContent) isAPISpecificResponse() {}
func (APISpecificResponseCustom) isAPISpecificResponse()                {}

func (APISpecificResponseOpenAIChat) rustName() string            { return "OpenAIChat" }
func (APISpecificResponseOpenAIResponses) rustName() string       { return "OpenAIResponses" }
func (APISpecificResponseAnthropicMessages) rustName() string     { return "AnthropicMessages" }
func (APISpecificResponseOCIGenAI) rustName() string              { return "OCIGenAI" }
func (APISpecificResponseGeminiGenerateContent) rustName() string { return "GeminiGenerateContent" }
func (APISpecificResponseCustom) rustName() string                { return "Custom" }

func (a APISpecificResponseOpenAIChat) serdeSerialize() (any, error) {
	return taggedStruct{"api", "openai_chat", a}, nil
}
func (a APISpecificResponseOpenAIResponses) serdeSerialize() (any, error) {
	return taggedStruct{"api", "openai_responses", a}, nil
}
func (a APISpecificResponseAnthropicMessages) serdeSerialize() (any, error) {
	return taggedStruct{"api", "anthropic_messages", a}, nil
}
func (a APISpecificResponseOCIGenAI) serdeSerialize() (any, error) {
	return taggedStruct{"api", "oci_genai", a}, nil
}

// Serializes the Gemini extension dropping any api key from the flattened extra map
func (a APISpecificResponseGeminiGenerateContent) serdeSerialize() (any, error) {
	filtered := make(map[string]any, len(a.Extra))
	for k, v := range a.Extra {
		if k != "api" {
			filtered[k] = v
		}
	}
	a.Extra = filtered
	return taggedStruct{"api", "gemini_generate_content", a}, nil
}
func (a APISpecificResponseCustom) serdeSerialize() (any, error) {
	return taggedStruct{"api", "custom", a}, nil
}

func (a APISpecificResponseOpenAIChat) MarshalJSON() ([]byte, error)            { return EncodeJSON(a) }
func (a APISpecificResponseOpenAIResponses) MarshalJSON() ([]byte, error)       { return EncodeJSON(a) }
func (a APISpecificResponseAnthropicMessages) MarshalJSON() ([]byte, error)     { return EncodeJSON(a) }
func (a APISpecificResponseOCIGenAI) MarshalJSON() ([]byte, error)              { return EncodeJSON(a) }
func (a APISpecificResponseGeminiGenerateContent) MarshalJSON() ([]byte, error) { return EncodeJSON(a) }
func (a APISpecificResponseCustom) MarshalJSON() ([]byte, error)                { return EncodeJSON(a) }

var apiSpecificResponseNames = []string{"openai_chat", "openai_responses", "anthropic_messages", "oci_genai", "gemini_generate_content", "custom"}

// DecodeAPISpecificResponse decodes the api tagged response extension
func DecodeAPISpecificResponse(v any) (APISpecificResponse, error) {
	tag, rest, err := splitInternalTag(v, "api", "internally tagged enum ApiSpecificResponse")
	if err != nil {
		return nil, err
	}
	idx, err := variantIndex(tag, apiSpecificResponseNames)
	if err != nil {
		return nil, err
	}
	switch idx {
	case 0:
		var a APISpecificResponseOpenAIChat
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificResponse::OpenAIChat")
		return a, err
	case 1:
		var a APISpecificResponseOpenAIResponses
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificResponse::OpenAIResponses")
		return a, err
	case 2:
		var a APISpecificResponseAnthropicMessages
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificResponse::AnthropicMessages")
		return a, err
	case 3:
		var a APISpecificResponseOCIGenAI
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificResponse::OCIGenAI")
		return a, err
	case 4:
		var a APISpecificResponseGeminiGenerateContent
		err := decodeVariantStruct(rest, &a, "struct variant ApiSpecificResponse::GeminiGenerateContent")
		delete(a.Extra, "api")
		return a, err
	}
	var a APISpecificResponseCustom
	err = decodeVariantStruct(rest, &a, "struct variant ApiSpecificResponse::Custom")
	return a, err
}

func init() {
	registerInterfaceDecoder(DecodeAPISpecificResponse)
}

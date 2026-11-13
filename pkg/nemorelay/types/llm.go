package types

import "strings"

const LLMRequestInterceptOutcomeSchema = "nemo.relay.LlmRequestInterceptOutcome@2"

// LLMAttributes are bit flags describing an LLM call
type LLMAttributes uint32

const (
	LLMAttributesStateful  LLMAttributes = 0b01
	LLMAttributesStreaming LLMAttributes = 0b10
)

var llmAttributeFlags = []flagDef{{"STATEFUL", 0b01}, {"STREAMING", 0b10}}

// Contains reports whether every flag of other is set
func (a LLMAttributes) Contains(other LLMAttributes) bool { return a&other == other }

// String writes the bitflags text form
func (a LLMAttributes) String() string { return flagsText(uint32(a), llmAttributeFlags) }

func (a LLMAttributes) rustDebug(b *strings.Builder) {
	writeFlagsDebug(b, "LlmAttributes", uint32(a), llmAttributeFlags)
}

func (a LLMAttributes) serdeSerialize() (any, error) { return a.String(), nil }

func (a *LLMAttributes) serdeDeserialize(v any) error {
	bits, err := decodeFlags(v, llmAttributeFlags)
	*a = LLMAttributes(bits)
	return err
}

// MarshalJSON writes the serde form
func (a LLMAttributes) MarshalJSON() ([]byte, error) { return EncodeJSON(a) }

// UnmarshalJSON reads the serde form
func (a *LLMAttributes) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, a) }

// LLMRequest is a provider request as headers plus JSON content
type LLMRequest struct {
	Headers map[string]any `json:"headers"`
	Content any            `json:"content"`
}

func (LLMRequest) rustName() string { return "LlmRequest" }

// Clone deep copies headers and content
func (r LLMRequest) Clone() LLMRequest {
	headers := CloneJSONObject(r.Headers)
	if headers == nil {
		headers = map[string]any{}
	}
	return LLMRequest{Headers: headers, Content: CloneJSON(r.Content)}
}

// MarshalJSON writes the serde form
func (r LLMRequest) MarshalJSON() ([]byte, error) { return EncodeJSON(r) }

// UnmarshalJSON reads the serde form
func (r *LLMRequest) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, r) }

// LLMRequestInterceptOutcome is what an LLM request intercept returns
type LLMRequestInterceptOutcome struct {
	Request                   LLMRequest                     `json:"request"`
	AnnotatedRequest          *AnnotatedLLMRequest           `json:"annotated_request" serde:"default"`
	PendingMarks              []PendingMarkSpec              `json:"pending_marks" serde:"default"`
	OptimizationContributions []LLMOptimizationContribution `json:"optimization_contributions" serde:"default"`
}

func (LLMRequestInterceptOutcome) rustName() string { return "LlmRequestInterceptOutcome" }

// NewLLMRequestInterceptOutcome builds an outcome without marks or contributions
func NewLLMRequestInterceptOutcome(request LLMRequest, annotated *AnnotatedLLMRequest) *LLMRequestInterceptOutcome {
	return &LLMRequestInterceptOutcome{
		Request:                   request,
		AnnotatedRequest:          annotated,
		PendingMarks:              []PendingMarkSpec{},
		OptimizationContributions: []LLMOptimizationContribution{},
	}
}

// WithPendingMark returns a copy with one more pending mark
func (o LLMRequestInterceptOutcome) WithPendingMark(mark PendingMarkSpec) *LLMRequestInterceptOutcome {
	o.PendingMarks = append(append([]PendingMarkSpec{}, o.PendingMarks...), mark)
	return &o
}

// WithOptimizationContribution returns a copy with one more optimization contribution
func (o LLMRequestInterceptOutcome) WithOptimizationContribution(c LLMOptimizationContribution) *LLMRequestInterceptOutcome {
	o.OptimizationContributions = append(append([]LLMOptimizationContribution{}, o.OptimizationContributions...), c)
	return &o
}

// MarshalJSON writes the serde form
func (o LLMRequestInterceptOutcome) MarshalJSON() ([]byte, error) { return EncodeJSON(o) }

// UnmarshalJSON reads the serde form
func (o *LLMRequestInterceptOutcome) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, o) }

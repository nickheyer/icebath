package types

import "strings"

const (
	ToolExecutionResultSchema           = "nemo.relay.ToolExecutionResult@1"
	ToolExecutionInterceptOutcomeSchema = "nemo.relay.ToolExecutionInterceptOutcome@2"
)

// ToolAttributes are bit flags describing a tool call
type ToolAttributes uint32

const ToolAttributesRemote ToolAttributes = 0b01

var toolAttributeFlags = []flagDef{{"REMOTE", 0b01}}

// Contains reports whether every flag of other is set
func (a ToolAttributes) Contains(other ToolAttributes) bool { return a&other == other }

// String writes the bitflags text form
func (a ToolAttributes) String() string { return flagsText(uint32(a), toolAttributeFlags) }

func (a ToolAttributes) rustDebug(b *strings.Builder) {
	writeFlagsDebug(b, "ToolAttributes", uint32(a), toolAttributeFlags)
}

func (a ToolAttributes) serdeSerialize() (any, error) { return a.String(), nil }

func (a *ToolAttributes) serdeDeserialize(v any) error {
	bits, err := decodeFlags(v, toolAttributeFlags)
	*a = ToolAttributes(bits)
	return err
}

// MarshalJSON writes the serde form
func (a ToolAttributes) MarshalJSON() ([]byte, error) { return EncodeJSON(a) }

// UnmarshalJSON reads the serde form
func (a *ToolAttributes) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, a) }

// ToolExecutionResult is a tool result with an optional annotation where null means absent
type ToolExecutionResult struct {
	Result     any `json:"result"`
	Annotation any `json:"annotation" serde:"default,skip_null"`
}

// NewToolExecutionResult wraps a result without annotation
func NewToolExecutionResult(result any) *ToolExecutionResult {
	return &ToolExecutionResult{Result: result}
}

// AnnotatedToolExecutionResult wraps a result with an annotation
func AnnotatedToolExecutionResult(result, annotation any) *ToolExecutionResult {
	return &ToolExecutionResult{Result: result, Annotation: annotation}
}

// WithAnnotation returns a copy carrying the annotation
func (r ToolExecutionResult) WithAnnotation(annotation any) *ToolExecutionResult {
	r.Annotation = annotation
	return &r
}

// WithoutAnnotation returns a copy without annotation
func (r ToolExecutionResult) WithoutAnnotation() *ToolExecutionResult {
	r.Annotation = nil
	return &r
}

// Clone deep copies the result
func (r *ToolExecutionResult) Clone() *ToolExecutionResult {
	if r == nil {
		return nil
	}
	return &ToolExecutionResult{Result: CloneJSON(r.Result), Annotation: CloneJSON(r.Annotation)}
}

// MarshalJSON writes the serde form
func (r ToolExecutionResult) MarshalJSON() ([]byte, error) { return EncodeJSON(r) }

// UnmarshalJSON reads the serde form
func (r *ToolExecutionResult) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, r) }

// ToolExecutionInterceptOutcome is what a tool execution intercept returns
type ToolExecutionInterceptOutcome struct {
	Result       any               `json:"result"`
	Annotation   any               `json:"annotation" serde:"default,skip_null"`
	PendingMarks []PendingMarkSpec `json:"pending_marks" serde:"default"`
}

// NewToolExecutionInterceptOutcome wraps a result without annotation or marks
func NewToolExecutionInterceptOutcome(result any) *ToolExecutionInterceptOutcome {
	return &ToolExecutionInterceptOutcome{Result: result, PendingMarks: []PendingMarkSpec{}}
}

// AnnotatedToolExecutionInterceptOutcome wraps a result with an annotation
func AnnotatedToolExecutionInterceptOutcome(result, annotation any) *ToolExecutionInterceptOutcome {
	return &ToolExecutionInterceptOutcome{Result: result, Annotation: annotation, PendingMarks: []PendingMarkSpec{}}
}

// ToolExecutionInterceptOutcomeFromResult converts an execution result into an outcome
func ToolExecutionInterceptOutcomeFromResult(r *ToolExecutionResult) *ToolExecutionInterceptOutcome {
	return &ToolExecutionInterceptOutcome{Result: r.Result, Annotation: r.Annotation, PendingMarks: []PendingMarkSpec{}}
}

// WithAnnotation returns a copy carrying the annotation
func (o ToolExecutionInterceptOutcome) WithAnnotation(annotation any) *ToolExecutionInterceptOutcome {
	o.Annotation = annotation
	return &o
}

// WithoutAnnotation returns a copy without annotation
func (o ToolExecutionInterceptOutcome) WithoutAnnotation() *ToolExecutionInterceptOutcome {
	o.Annotation = nil
	return &o
}

// WithPendingMark returns a copy with one more pending mark
func (o ToolExecutionInterceptOutcome) WithPendingMark(mark PendingMarkSpec) *ToolExecutionInterceptOutcome {
	o.PendingMarks = append(append([]PendingMarkSpec{}, o.PendingMarks...), mark)
	return &o
}

// IntoExecutionResult drops pending marks and keeps result and annotation
func (o *ToolExecutionInterceptOutcome) IntoExecutionResult() *ToolExecutionResult {
	return &ToolExecutionResult{Result: o.Result, Annotation: o.Annotation}
}

// MarshalJSON writes the serde form
func (o ToolExecutionInterceptOutcome) MarshalJSON() ([]byte, error) { return EncodeJSON(o) }

// UnmarshalJSON reads the serde form
func (o *ToolExecutionInterceptOutcome) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, o) }

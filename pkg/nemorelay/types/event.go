package types

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ATOFVersion             = "0.1"
	LogSeverityMetadataKey  = "nemo_relay.log.severity"
	MetricDataSchemaName    = "nemo.relay.metric_measurements"
	MetricDataSchemaVersion = "1"
)

// LogSeverity orders log events from trace to error
type LogSeverity uint8

const (
	LogSeverityTrace LogSeverity = iota + 1
	LogSeverityDebug
	LogSeverityInfo
	LogSeverityWarn
	LogSeverityError
)

// DefaultLogSeverity is the severity used when none is given
const DefaultLogSeverity = LogSeverityInfo

var logSeverityNames = []string{"trace", "debug", "info", "warn", "error"}

// AsStr returns the lowercase name
func (s LogSeverity) AsStr() string {
	if s < 1 || int(s) > len(logSeverityNames) {
		return "info"
	}
	return logSeverityNames[s-1]
}

// String returns the lowercase name
func (s LogSeverity) String() string { return s.AsStr() }

// ParseLogSeverity reads a severity name ignoring case and surrounding space, warning meaning warn
func ParseLogSeverity(value string) (LogSeverity, error) {
	switch asciiLower(strings.TrimSpace(value)) {
	case "trace":
		return LogSeverityTrace, nil
	case "debug":
		return LogSeverityDebug, nil
	case "info":
		return LogSeverityInfo, nil
	case "warn", "warning":
		return LogSeverityWarn, nil
	case "error":
		return LogSeverityError, nil
	}
	return 0, fmt.Errorf("invalid log severity %s; expected trace, debug, info, warn, warning, or error", RustStrDebug(value))
}

func (s LogSeverity) rustDebug(b *strings.Builder) {
	switch s {
	case LogSeverityTrace:
		b.WriteString("Trace")
	case LogSeverityDebug:
		b.WriteString("Debug")
	case LogSeverityInfo:
		b.WriteString("Info")
	case LogSeverityWarn:
		b.WriteString("Warn")
	case LogSeverityError:
		b.WriteString("Error")
	}
}

func (s LogSeverity) serdeSerialize() (any, error) { return s.AsStr(), nil }

func (s *LogSeverity) serdeDeserialize(v any) error {
	text, ok := v.(string)
	if !ok {
		return InvalidTypeError(v, "a string")
	}
	parsed, err := ParseLogSeverity(text)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// MarshalJSON writes the serde form
func (s LogSeverity) MarshalJSON() ([]byte, error) { return EncodeJSON(s) }

// UnmarshalJSON reads the serde form
func (s *LogSeverity) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, s) }

// DataSchema names the schema and version of event data
type DataSchema struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MarshalJSON writes the serde form
func (d DataSchema) MarshalJSON() ([]byte, error) { return EncodeJSON(d) }

// UnmarshalJSON reads the serde form
func (d *DataSchema) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, d) }

// EventCategory is the open-ended category of an event
type EventCategory string

// NewEventCategory wraps any category text
func NewEventCategory(value string) EventCategory { return EventCategory(value) }

// EventCategoryAgent is the agent category
func EventCategoryAgent() EventCategory { return "agent" }

// EventCategoryFunction is the function category
func EventCategoryFunction() EventCategory { return "function" }

// EventCategoryLLM is the llm category
func EventCategoryLLM() EventCategory { return "llm" }

// EventCategoryTool is the tool category
func EventCategoryTool() EventCategory { return "tool" }

// EventCategoryRetriever is the retriever category
func EventCategoryRetriever() EventCategory { return "retriever" }

// EventCategoryEmbedder is the embedder category
func EventCategoryEmbedder() EventCategory { return "embedder" }

// EventCategoryReranker is the reranker category
func EventCategoryReranker() EventCategory { return "reranker" }

// EventCategoryGuardrail is the guardrail category
func EventCategoryGuardrail() EventCategory { return "guardrail" }

// EventCategoryEvaluator is the evaluator category
func EventCategoryEvaluator() EventCategory { return "evaluator" }

// EventCategoryCustom is the custom category
func EventCategoryCustom() EventCategory { return "custom" }

// EventCategoryUnknown is the unknown category
func EventCategoryUnknown() EventCategory { return "unknown" }

// EventCategoryFromScopeType maps a scope type to its category
func EventCategoryFromScopeType(t ScopeType) EventCategory { return EventCategory(t.AsStr()) }

// AsStr returns the category text
func (c EventCategory) AsStr() string { return string(c) }

// String returns the category text
func (c EventCategory) String() string { return string(c) }

// ToScopeType maps known category names to scope types and others to Unknown
func (c EventCategory) ToScopeType() ScopeType {
	for i, n := range scopeTypeNames[:len(scopeTypeNames)-1] {
		if n == string(c) {
			return ScopeType(i + 1)
		}
	}
	return ScopeTypeUnknown
}

func (c EventCategory) rustDebug(b *strings.Builder) {
	b.WriteString("EventCategory(" + RustStrDebug(string(c)) + ")")
}

// ScopeCategory marks a scope event as start or end
type ScopeCategory uint8

const (
	ScopeCategoryStart ScopeCategory = iota + 1
	ScopeCategoryEnd
)

// String returns the lowercase name
func (c ScopeCategory) String() string {
	if c == ScopeCategoryEnd {
		return "end"
	}
	return "start"
}

func (c ScopeCategory) rustDebug(b *strings.Builder) {
	if c == ScopeCategoryEnd {
		b.WriteString("End")
	} else {
		b.WriteString("Start")
	}
}

func (c ScopeCategory) serdeSerialize() (any, error) { return c.String(), nil }

func (c *ScopeCategory) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, []string{"start", "end"})
	if err != nil {
		return err
	}
	*c = ScopeCategory(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (c ScopeCategory) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *ScopeCategory) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// CategoryProfile holds category specific event details with unknown keys kept in Extra
type CategoryProfile struct {
	ModelName            *string               `json:"model_name" serde:"default,skip_none"`
	ToolCallID           *string               `json:"tool_call_id" serde:"default,skip_none"`
	Subtype              *string               `json:"subtype" serde:"default,skip_none"`
	ToolResultAnnotation any                   `json:"tool_result_annotation" serde:"default,skip_null"`
	Extra                map[string]any        `json:"extra" serde:"flatten"`
	AnnotatedRequest     *AnnotatedLLMRequest  `json:"annotated_request" serde:"default,skip_none"`
	AnnotatedResponse    *AnnotatedLLMResponse `json:"annotated_response" serde:"default,skip_none"`
	TimeToFirstChunk     *float64              `json:"time_to_first_chunk" serde:"default,skip_none"`
}

// IsWireEmpty reports whether the profile serializes to an empty object
func (p *CategoryProfile) IsWireEmpty() bool {
	return p.ModelName == nil && p.ToolCallID == nil && p.Subtype == nil && p.ToolResultAnnotation == nil &&
		p.AnnotatedRequest == nil && p.AnnotatedResponse == nil && p.TimeToFirstChunk == nil && len(p.Extra) == 0
}

// Clone deep copies the profile
func (p *CategoryProfile) Clone() *CategoryProfile {
	if p == nil {
		return nil
	}
	out := cloneDataModel(*p)
	return &out
}

// MarshalJSON writes the serde form
func (p CategoryProfile) MarshalJSON() ([]byte, error) { return EncodeJSON(p) }

// UnmarshalJSON reads the serde form
func (p *CategoryProfile) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, p) }

// BaseEvent holds the fields every event shares
type BaseEvent struct {
	AtofVersion            string      `json:"atof_version"`
	ParentUUID             *uuid.UUID  `json:"parent_uuid"`
	PropagationRootUUID    *uuid.UUID  `json:"propagation_root_uuid" serde:"default,skip_none"`
	PropagationParentUUID  *uuid.UUID  `json:"-" rust:"propagation_parent_uuid"`
	PropagationTraceparent *string     `json:"-" rust:"propagation_traceparent"`
	PropagationTracestate  *string     `json:"-" rust:"propagation_tracestate"`
	UUID                   uuid.UUID   `json:"uuid"`
	Timestamp              time.Time   `json:"timestamp" serde:"timestamp"`
	TimestampLeap          bool        `json:"-"`
	Name                   string      `json:"name"`
	Data                   any         `json:"data" serde:"option"`
	DataSchema             *DataSchema `json:"data_schema"`
	Metadata               any         `json:"metadata" serde:"option"`
}

// NewBaseEvent builds a base event with the current version, a fresh v7 UUID and the current time
func NewBaseEvent(name string) BaseEvent {
	return BaseEvent{AtofVersion: ATOFVersion, UUID: newV7(), Timestamp: time.Now().UTC(), Name: name}
}

// Generates a time ordered v7 UUID
func newV7() uuid.UUID {
	u, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return u
}

// NewUUIDv7 generates a time ordered v7 UUID as Uuid now_v7 does
func NewUUIDv7() uuid.UUID { return newV7() }

// MarshalJSON writes the serde form
func (b BaseEvent) MarshalJSON() ([]byte, error) { return EncodeJSON(b) }

// UnmarshalJSON reads the serde form
func (b *BaseEvent) UnmarshalJSON(data []byte) error { return SerdeUnmarshal(data, b) }

// ScopeEvent is a scope start or end event
type ScopeEvent struct {
	Base            BaseEvent        `json:"base" serde:"flatten"`
	ScopeCategory   ScopeCategory    `json:"scope_category"`
	Attributes      []string         `json:"attributes"`
	Category        EventCategory    `json:"category"`
	CategoryProfile *CategoryProfile `json:"category_profile"`
}

// NewScopeEvent builds a scope event with attributes sorted and deduplicated
func NewScopeEvent(base BaseEvent, scopeCategory ScopeCategory, attributes []string, category EventCategory, profile *CategoryProfile) *ScopeEvent {
	return &ScopeEvent{Base: base, ScopeCategory: scopeCategory, Attributes: canonicalizeAttributes(attributes), Category: category, CategoryProfile: profile}
}

func canonicalizeAttributes(attributes []string) []string {
	out := append([]string{}, attributes...)
	sort.Strings(out)
	dedup := out[:0]
	for i, a := range out {
		if i == 0 || a != out[i-1] {
			dedup = append(dedup, a)
		}
	}
	return dedup
}

// MarshalJSON writes the serde form
func (e ScopeEvent) MarshalJSON() ([]byte, error) { return EncodeJSON(e) }

// UnmarshalJSON reads the serde form
func (e *ScopeEvent) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, e) }

// MarkEvent is a point-in-time event
type MarkEvent struct {
	Base            BaseEvent        `json:"base" serde:"flatten"`
	Category        *EventCategory   `json:"category"`
	CategoryProfile *CategoryProfile `json:"category_profile"`
}

// NewMarkEvent builds a mark event
func NewMarkEvent(base BaseEvent, category *EventCategory, profile *CategoryProfile) *MarkEvent {
	return &MarkEvent{Base: base, Category: category, CategoryProfile: profile}
}

// MarshalJSON writes the serde form
func (e MarkEvent) MarshalJSON() ([]byte, error) { return EncodeJSON(e) }

// UnmarshalJSON reads the serde form
func (e *MarkEvent) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, e) }

// EventSanitizeFields are the event fields a sanitize guardrail may rewrite
type EventSanitizeFields struct {
	Data            any              `json:"data" serde:"option"`
	CategoryProfile *CategoryProfile `json:"category_profile"`
	Metadata        any              `json:"metadata" serde:"option"`
}

// MarshalJSON writes the serde form
func (f EventSanitizeFields) MarshalJSON() ([]byte, error) { return EncodeJSON(f) }

// UnmarshalJSON reads the serde form
func (f *EventSanitizeFields) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, f) }

// PendingMarkSpec describes a mark event to emit later
type PendingMarkSpec struct {
	Name            string           `json:"name"`
	Category        *EventCategory   `json:"category"`
	CategoryProfile *CategoryProfile `json:"category_profile"`
	Data            any              `json:"data" serde:"option"`
	DataSchema      *DataSchema      `json:"data_schema"`
	Metadata        any              `json:"metadata" serde:"option"`
	Severity        *LogSeverity     `json:"severity"`
}

// MarshalJSON writes the serde form
func (p PendingMarkSpec) MarshalJSON() ([]byte, error) { return EncodeJSON(p) }

// UnmarshalJSON reads the serde form
func (p *PendingMarkSpec) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, p) }

// Event holds exactly one of a scope event or a mark event
type Event struct {
	Scope *ScopeEvent
	Mark  *MarkEvent
}

// ScopeEventValue wraps a scope event
func ScopeEventValue(e *ScopeEvent) *Event { return &Event{Scope: e} }

// MarkEventValue wraps a mark event
func MarkEventValue(e *MarkEvent) *Event { return &Event{Mark: e} }

func (e Event) serdeSerialize() (any, error) {
	switch {
	case e.Scope != nil:
		return taggedStruct{"kind", "scope", *e.Scope}, nil
	case e.Mark != nil:
		return taggedStruct{"kind", "mark", *e.Mark}, nil
	}
	return nil, fmt.Errorf("event holds neither a scope nor a mark")
}

func (e *Event) serdeDeserialize(v any) error {
	tag, rest, err := splitInternalTag(v, "kind", "internally tagged enum Event")
	if err != nil {
		return err
	}
	idx, err := variantIndex(tag, []string{"scope", "mark"})
	if err != nil {
		return err
	}
	*e = Event{}
	if idx == 0 {
		var s ScopeEvent
		if err := decodeInto(rest, reflectValueOf(&s)); err != nil {
			return err
		}
		e.Scope = &s
		return nil
	}
	var m MarkEvent
	if err := decodeInto(rest, reflectValueOf(&m)); err != nil {
		return err
	}
	e.Mark = &m
	return nil
}

func (e Event) rustDebug(b *strings.Builder) {
	switch {
	case e.Scope != nil:
		b.WriteString("Scope(" + RustDebug(*e.Scope) + ")")
	case e.Mark != nil:
		b.WriteString("Mark(" + RustDebug(*e.Mark) + ")")
	}
}

// MarshalJSON writes the internally tagged serde form
func (e Event) MarshalJSON() ([]byte, error) { return EncodeJSON(e) }

// UnmarshalJSON reads the internally tagged serde form
func (e *Event) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, e) }

// Clone deep copies the event
func (e *Event) Clone() *Event {
	if e == nil {
		return nil
	}
	out := cloneDataModel(*e)
	return &out
}

// Kind returns scope or mark
func (e *Event) Kind() string {
	if e.Scope != nil {
		return "scope"
	}
	return "mark"
}

// ToJSONValue converts the event into decoded JSON
func (e *Event) ToJSONValue() (any, error) { return ToJSONValue(*e) }

// ToJSONString serializes the event through its JSON value so object keys come out sorted
func (e *Event) ToJSONString() (string, error) {
	v, err := e.ToJSONValue()
	if err != nil {
		return "", err
	}
	return EncodeJSONString(v)
}

// Base returns the shared base fields
func (e *Event) Base() *BaseEvent {
	if e.Scope != nil {
		return &e.Scope.Base
	}
	return &e.Mark.Base
}

// ScopeCategory returns the scope category of scope events
func (e *Event) ScopeCategory() *ScopeCategory {
	if e.Scope == nil {
		return nil
	}
	c := e.Scope.ScopeCategory
	return &c
}

// Category returns the event category
func (e *Event) Category() *EventCategory {
	if e.Scope != nil {
		return &e.Scope.Category
	}
	return e.Mark.Category
}

// CategoryProfile returns the category profile for reading or in-place mutation
func (e *Event) CategoryProfile() *CategoryProfile {
	if e.Scope != nil {
		return e.Scope.CategoryProfile
	}
	return e.Mark.CategoryProfile
}

// ToolResultAnnotation returns a non-null tool result annotation
func (e *Event) ToolResultAnnotation() any {
	p := e.CategoryProfile()
	if p == nil {
		return nil
	}
	return p.ToolResultAnnotation
}

// ParentUUID returns the parent scope UUID
func (e *Event) ParentUUID() *uuid.UUID { return e.Base().ParentUUID }

// PropagationRootUUID returns the propagation root UUID
func (e *Event) PropagationRootUUID() *uuid.UUID { return e.Base().PropagationRootUUID }

// SetPropagationRootUUID sets the propagation root UUID
func (e *Event) SetPropagationRootUUID(id *uuid.UUID) { e.Base().PropagationRootUUID = id }

// PropagationParentUUID returns the propagation parent UUID
func (e *Event) PropagationParentUUID() *uuid.UUID { return e.Base().PropagationParentUUID }

// SetPropagationParentUUID sets the propagation parent UUID
func (e *Event) SetPropagationParentUUID(id *uuid.UUID) { e.Base().PropagationParentUUID = id }

// PropagationTraceparent returns the propagated traceparent
func (e *Event) PropagationTraceparent() *string { return e.Base().PropagationTraceparent }

// SetPropagationTraceparent sets the propagated traceparent
func (e *Event) SetPropagationTraceparent(v *string) { e.Base().PropagationTraceparent = v }

// PropagationTracestate returns the propagated tracestate
func (e *Event) PropagationTracestate() *string { return e.Base().PropagationTracestate }

// SetPropagationTracestate sets the propagated tracestate
func (e *Event) SetPropagationTracestate(v *string) { e.Base().PropagationTracestate = v }

// UUID returns the event UUID
func (e *Event) UUID() uuid.UUID { return e.Base().UUID }

// Timestamp returns the event time
func (e *Event) Timestamp() time.Time { return e.Base().Timestamp }

// TimestampString returns the event time as to_rfc3339 writes it
func (e *Event) TimestampString() string {
	b := e.Base()
	return formatRFC3339(b.Timestamp, false, b.TimestampLeap)
}

// Name returns the event name
func (e *Event) Name() string { return e.Base().Name }

// Data returns the event data
func (e *Event) Data() any { return e.Base().Data }

// DataSchema returns the event data schema
func (e *Event) DataSchema() *DataSchema { return e.Base().DataSchema }

// Metadata returns the event metadata
func (e *Event) Metadata() any { return e.Base().Metadata }

// SanitizeFields copies the fields a sanitize guardrail may rewrite
func (e *Event) SanitizeFields() EventSanitizeFields {
	b := e.Base()
	return EventSanitizeFields{Data: CloneJSON(b.Data), CategoryProfile: e.CategoryProfile().Clone(), Metadata: CloneJSON(b.Metadata)}
}

// ApplySanitizeFields writes rewritten sanitize fields back into the event
func (e *Event) ApplySanitizeFields(f EventSanitizeFields) {
	b := e.Base()
	b.Data = f.Data
	b.Metadata = f.Metadata
	if e.Scope != nil {
		e.Scope.CategoryProfile = f.CategoryProfile
	} else {
		e.Mark.CategoryProfile = f.CategoryProfile
	}
}

// Attributes returns the scope attributes of scope events
func (e *Event) Attributes() ([]string, bool) {
	if e.Scope == nil {
		return nil, false
	}
	return e.Scope.Attributes, true
}

// ScopeType maps the category to a scope type
func (e *Event) ScopeType() *ScopeType {
	c := e.Category()
	if c == nil {
		return nil
	}
	t := c.ToScopeType()
	return &t
}

// Input returns the data of scope start events
func (e *Event) Input() any {
	if e.Scope != nil && e.Scope.ScopeCategory == ScopeCategoryStart {
		return e.Scope.Base.Data
	}
	return nil
}

// Output returns the data of scope end events
func (e *Event) Output() any {
	if e.Scope != nil && e.Scope.ScopeCategory == ScopeCategoryEnd {
		return e.Scope.Base.Data
	}
	return nil
}

// ModelName returns the profile model name
func (e *Event) ModelName() *string {
	if p := e.CategoryProfile(); p != nil {
		return p.ModelName
	}
	return nil
}

// ToolCallID returns the profile tool call id
func (e *Event) ToolCallID() *string {
	if p := e.CategoryProfile(); p != nil {
		return p.ToolCallID
	}
	return nil
}

// AnnotatedRequest returns the profile annotated request
func (e *Event) AnnotatedRequest() *AnnotatedLLMRequest {
	if p := e.CategoryProfile(); p != nil {
		return p.AnnotatedRequest
	}
	return nil
}

// AnnotatedResponse returns the profile annotated response
func (e *Event) AnnotatedResponse() *AnnotatedLLMResponse {
	if p := e.CategoryProfile(); p != nil {
		return p.AnnotatedResponse
	}
	return nil
}

// TimeToFirstChunk returns the profile time to first chunk in seconds
func (e *Event) TimeToFirstChunk() *float64 {
	if p := e.CategoryProfile(); p != nil {
		return p.TimeToFirstChunk
	}
	return nil
}

// IsScopeStart reports whether the event starts a scope
func (e *Event) IsScopeStart() bool {
	return e.Scope != nil && e.Scope.ScopeCategory == ScopeCategoryStart
}

// IsScopeEnd reports whether the event ends a scope
func (e *Event) IsScopeEnd() bool {
	return e.Scope != nil && e.Scope.ScopeCategory == ScopeCategoryEnd
}

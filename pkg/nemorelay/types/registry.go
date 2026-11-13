package types

import "strings"

// RuntimeRegistrationKind names a runtime registration slot
type RuntimeRegistrationKind uint8

const (
	RuntimeRegistrationKindSubscriber RuntimeRegistrationKind = iota + 1
	RuntimeRegistrationKindEventMetadataInjector
	RuntimeRegistrationKindMarkSanitizeGuardrail
	RuntimeRegistrationKindScopeSanitizeStartGuardrail
	RuntimeRegistrationKindScopeSanitizeEndGuardrail
	RuntimeRegistrationKindToolSanitizeRequestGuardrail
	RuntimeRegistrationKindToolSanitizeResponseGuardrail
	RuntimeRegistrationKindToolConditionalExecutionGuardrail
	RuntimeRegistrationKindToolRequestIntercept
	RuntimeRegistrationKindToolExecutionIntercept
	RuntimeRegistrationKindLLMSanitizeRequestGuardrail
	RuntimeRegistrationKindLLMSanitizeResponseGuardrail
	RuntimeRegistrationKindLLMConditionalExecutionGuardrail
	RuntimeRegistrationKindLLMRequestIntercept
	RuntimeRegistrationKindLLMExecutionIntercept
	RuntimeRegistrationKindLLMStreamExecutionIntercept
)

var runtimeRegistrationKindNames = []string{
	"subscriber", "event_metadata_injector", "mark_sanitize_guardrail", "scope_sanitize_start_guardrail",
	"scope_sanitize_end_guardrail", "tool_sanitize_request_guardrail", "tool_sanitize_response_guardrail",
	"tool_conditional_execution_guardrail", "tool_request_intercept", "tool_execution_intercept",
	"llm_sanitize_request_guardrail", "llm_sanitize_response_guardrail", "llm_conditional_execution_guardrail",
	"llm_request_intercept", "llm_execution_intercept", "llm_stream_execution_intercept",
}

var runtimeRegistrationKindDebug = []string{
	"Subscriber", "EventMetadataInjector", "MarkSanitizeGuardrail", "ScopeSanitizeStartGuardrail",
	"ScopeSanitizeEndGuardrail", "ToolSanitizeRequestGuardrail", "ToolSanitizeResponseGuardrail",
	"ToolConditionalExecutionGuardrail", "ToolRequestIntercept", "ToolExecutionIntercept",
	"LlmSanitizeRequestGuardrail", "LlmSanitizeResponseGuardrail", "LlmConditionalExecutionGuardrail",
	"LlmRequestIntercept", "LlmExecutionIntercept", "LlmStreamExecutionIntercept",
}

// AllRuntimeRegistrationKinds lists every kind in declaration order
var AllRuntimeRegistrationKinds = func() []RuntimeRegistrationKind {
	out := make([]RuntimeRegistrationKind, len(runtimeRegistrationKindNames))
	for i := range out {
		out[i] = RuntimeRegistrationKind(i + 1)
	}
	return out
}()

// String returns the snake_case name
func (k RuntimeRegistrationKind) String() string {
	if k < 1 || int(k) > len(runtimeRegistrationKindNames) {
		return ""
	}
	return runtimeRegistrationKindNames[k-1]
}

// AsStr returns the snake_case name as the Rust as_str does
func (k RuntimeRegistrationKind) AsStr() string { return k.String() }

func (k RuntimeRegistrationKind) rustDebug(b *strings.Builder) {
	if k >= 1 && int(k) <= len(runtimeRegistrationKindDebug) {
		b.WriteString(runtimeRegistrationKindDebug[k-1])
	}
}

func (k RuntimeRegistrationKind) serdeSerialize() (any, error) { return k.String(), nil }

func (k *RuntimeRegistrationKind) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, runtimeRegistrationKindNames)
	if err != nil {
		return err
	}
	*k = RuntimeRegistrationKind(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (k RuntimeRegistrationKind) MarshalJSON() ([]byte, error) { return EncodeJSON(k) }

// UnmarshalJSON reads the serde form
func (k *RuntimeRegistrationKind) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, k) }

// RuntimeRegistrationOwnerKind names who owns a registration
type RuntimeRegistrationOwnerKind uint8

const (
	RuntimeRegistrationOwnerKindCore RuntimeRegistrationOwnerKind = iota + 1
	RuntimeRegistrationOwnerKindGlobalAPI
	RuntimeRegistrationOwnerKindPlugin
)

var runtimeRegistrationOwnerKindNames = []string{"core", "global_api", "plugin"}

// String returns the snake_case name
func (k RuntimeRegistrationOwnerKind) String() string {
	if k < 1 || int(k) > len(runtimeRegistrationOwnerKindNames) {
		return ""
	}
	return runtimeRegistrationOwnerKindNames[k-1]
}

func (k RuntimeRegistrationOwnerKind) rustDebug(b *strings.Builder) {
	switch k {
	case RuntimeRegistrationOwnerKindCore:
		b.WriteString("Core")
	case RuntimeRegistrationOwnerKindGlobalAPI:
		b.WriteString("GlobalApi")
	case RuntimeRegistrationOwnerKindPlugin:
		b.WriteString("Plugin")
	}
}

func (k RuntimeRegistrationOwnerKind) serdeSerialize() (any, error) { return k.String(), nil }

func (k *RuntimeRegistrationOwnerKind) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, runtimeRegistrationOwnerKindNames)
	if err != nil {
		return err
	}
	*k = RuntimeRegistrationOwnerKind(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (k RuntimeRegistrationOwnerKind) MarshalJSON() ([]byte, error) { return EncodeJSON(k) }

// UnmarshalJSON reads the serde form
func (k *RuntimeRegistrationOwnerKind) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, k) }

// RuntimeRegistrationOwner identifies the owner of a registration
type RuntimeRegistrationOwner struct {
	Kind             RuntimeRegistrationOwnerKind `json:"kind"`
	PluginKind       *string                      `json:"plugin_kind"`
	ComponentOrdinal *uint32                      `json:"component_ordinal"`
}

// MarshalJSON writes the serde form
func (o RuntimeRegistrationOwner) MarshalJSON() ([]byte, error) { return EncodeJSON(o) }

// UnmarshalJSON reads the serde form
func (o *RuntimeRegistrationOwner) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, o) }

// RuntimeRegistrationIdentity identifies one registration by kind, names and owner
type RuntimeRegistrationIdentity struct {
	Kind          RuntimeRegistrationKind  `json:"kind"`
	LocalName     string                   `json:"local_name"`
	EffectiveName string                   `json:"effective_name"`
	Owner         RuntimeRegistrationOwner `json:"owner"`
}

// MarshalJSON writes the serde form
func (i RuntimeRegistrationIdentity) MarshalJSON() ([]byte, error) { return EncodeJSON(i) }

// UnmarshalJSON reads the serde form
func (i *RuntimeRegistrationIdentity) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, i) }

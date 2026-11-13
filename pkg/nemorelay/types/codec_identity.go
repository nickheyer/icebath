package types

import "strings"

// BuiltinLLMCodec names a codec compiled into the runtime
type BuiltinLLMCodec uint8

const (
	BuiltinLLMCodecOpenAIChat BuiltinLLMCodec = iota + 1
	BuiltinLLMCodecOpenAIResponses
	BuiltinLLMCodecAnthropicMessages
	BuiltinLLMCodecOCIGenAI
	BuiltinLLMCodecGeminiGenerateContent
)

var builtinLLMCodecIDs = []string{"openai_chat", "openai_responses", "anthropic_messages", "oci_genai", "gemini_generate_content"}

var builtinLLMCodecDebug = []string{"OpenAiChat", "OpenAiResponses", "AnthropicMessages", "OCIGenAI", "GeminiGenerateContent"}

// AllBuiltinLLMCodecs lists every builtin codec in declaration order
var AllBuiltinLLMCodecs = []BuiltinLLMCodec{
	BuiltinLLMCodecOpenAIChat, BuiltinLLMCodecOpenAIResponses, BuiltinLLMCodecAnthropicMessages,
	BuiltinLLMCodecOCIGenAI, BuiltinLLMCodecGeminiGenerateContent,
}

// ID returns the stable codec identifier
func (c BuiltinLLMCodec) ID() string {
	if c < 1 || int(c) > len(builtinLLMCodecIDs) {
		return ""
	}
	return builtinLLMCodecIDs[c-1]
}

// String returns the stable codec identifier
func (c BuiltinLLMCodec) String() string { return c.ID() }

// BuiltinLLMCodecFromID resolves a stable codec identifier
func BuiltinLLMCodecFromID(id string) (BuiltinLLMCodec, bool) {
	for i, known := range builtinLLMCodecIDs {
		if known == id {
			return BuiltinLLMCodec(i + 1), true
		}
	}
	return 0, false
}

func (c BuiltinLLMCodec) rustDebug(b *strings.Builder) {
	if c >= 1 && int(c) <= len(builtinLLMCodecDebug) {
		b.WriteString(builtinLLMCodecDebug[c-1])
	}
}

func (c BuiltinLLMCodec) serdeSerialize() (any, error) { return c.ID(), nil }

func (c *BuiltinLLMCodec) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, builtinLLMCodecIDs)
	if err != nil {
		return err
	}
	*c = BuiltinLLMCodec(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (c BuiltinLLMCodec) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *BuiltinLLMCodec) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// LLMCodecIdentityKind selects the codec identity variant
type LLMCodecIdentityKind uint8

const (
	LLMCodecIdentityNone LLMCodecIdentityKind = iota
	LLMCodecIdentityBuiltIn
	LLMCodecIdentityRuntime
	LLMCodecIdentityOpaque
)

// LLMCodecIdentity identifies which codec produced annotated data, zero value meaning none
type LLMCodecIdentity struct {
	Kind    LLMCodecIdentityKind
	Builtin BuiltinLLMCodec
	Runtime string
}

// BuiltinCodecIdentity identifies a builtin codec
func BuiltinCodecIdentity(c BuiltinLLMCodec) LLMCodecIdentity {
	return LLMCodecIdentity{Kind: LLMCodecIdentityBuiltIn, Builtin: c}
}

// RuntimeCodecIdentity identifies a codec registered at runtime by name
func RuntimeCodecIdentity(name string) LLMCodecIdentity {
	return LLMCodecIdentity{Kind: LLMCodecIdentityRuntime, Runtime: name}
}

// OpaqueCodecIdentity identifies a codec that declares no identity
func OpaqueCodecIdentity() LLMCodecIdentity {
	return LLMCodecIdentity{Kind: LLMCodecIdentityOpaque}
}

var llmCodecIdentityKinds = []string{"none", "builtin", "runtime", "opaque"}

func (id LLMCodecIdentity) serdeSerialize() (any, error) {
	switch id.Kind {
	case LLMCodecIdentityBuiltIn:
		return orderedObject{}.with("kind", "builtin").with("id", id.Builtin), nil
	case LLMCodecIdentityRuntime:
		return orderedObject{}.with("kind", "runtime").with("id", id.Runtime), nil
	case LLMCodecIdentityOpaque:
		return orderedObject{}.with("kind", "opaque"), nil
	}
	return orderedObject{}.with("kind", "none"), nil
}

// Decodes the adjacently tagged kind and id form
func (id *LLMCodecIdentity) serdeDeserialize(v any) error {
	const expect = "adjacently tagged enum LlmCodecIdentity"
	var tag, content any
	var hasTag, hasContent bool
	switch x := normalizeJSON(v).(type) {
	case map[string]any:
		tag, hasTag = x["kind"]
		content, hasContent = x["id"]
	case []any:
		if len(x) == 0 {
			return InvalidLengthError(0, expect)
		}
		tag, hasTag = x[0], true
		if len(x) > 1 {
			content, hasContent = x[1], true
		}
		if len(x) > 2 {
			return InvalidLengthError(3, "adjacently tagged enum LlmCodecIdentity")
		}
	default:
		return InvalidTypeError(v, expect)
	}
	if !hasTag {
		return MissingFieldError("kind")
	}
	idx, err := variantIndex(tag, llmCodecIdentityKinds)
	if err != nil {
		return err
	}
	switch LLMCodecIdentityKind(idx) {
	case LLMCodecIdentityBuiltIn:
		if !hasContent {
			return MissingFieldError("id")
		}
		var c BuiltinLLMCodec
		if err := c.serdeDeserialize(content); err != nil {
			return err
		}
		*id = BuiltinCodecIdentity(c)
	case LLMCodecIdentityRuntime:
		if !hasContent {
			return MissingFieldError("id")
		}
		s, ok := content.(string)
		if !ok {
			return InvalidTypeError(content, "a string")
		}
		*id = RuntimeCodecIdentity(s)
	default:
		if hasContent && content != nil {
			return InvalidTypeError(content, "unit")
		}
		*id = LLMCodecIdentity{Kind: LLMCodecIdentityKind(idx)}
	}
	return nil
}

func (id LLMCodecIdentity) rustDebug(b *strings.Builder) {
	switch id.Kind {
	case LLMCodecIdentityBuiltIn:
		b.WriteString("BuiltIn(")
		id.Builtin.rustDebug(b)
		b.WriteByte(')')
	case LLMCodecIdentityRuntime:
		b.WriteString("Runtime(" + RustStrDebug(id.Runtime) + ")")
	case LLMCodecIdentityOpaque:
		b.WriteString("Opaque")
	default:
		b.WriteString("None")
	}
}

// String renders the identity as Rust Debug output
func (id LLMCodecIdentity) String() string {
	var b strings.Builder
	id.rustDebug(&b)
	return b.String()
}

// MarshalJSON writes the serde form
func (id LLMCodecIdentity) MarshalJSON() ([]byte, error) { return EncodeJSON(id) }

// UnmarshalJSON reads the serde form
func (id *LLMCodecIdentity) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, id) }

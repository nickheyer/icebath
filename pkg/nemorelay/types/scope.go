package types

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type flagDef struct {
	name string
	bit  uint32
}

// Writes set flags as bitflags 2 does, names joined by bars then unknown bits in hex
func flagsText(bits uint32, defs []flagDef) string {
	var parts []string
	remaining := bits
	for _, d := range defs {
		if bits&d.bit == d.bit && remaining&d.bit != 0 {
			parts = append(parts, d.name)
			remaining &^= d.bit
		}
	}
	if remaining != 0 {
		parts = append(parts, "0x"+strconv.FormatUint(uint64(remaining), 16))
	}
	return strings.Join(parts, " | ")
}

// Parses bar separated flag names and hex bits as bitflags 2 from_str does
func parseFlagsText(input string, defs []flagDef) (uint32, error) {
	var out uint32
	if strings.TrimSpace(input) == "" {
		return 0, nil
	}
	for _, flag := range strings.Split(input, "|") {
		flag = strings.TrimSpace(flag)
		if flag == "" {
			return 0, fmt.Errorf("encountered empty flag")
		}
		if hex, ok := strings.CutPrefix(flag, "0x"); ok {
			bits, err := strconv.ParseUint(hex, 16, 32)
			if err != nil || hex == "" || hex[0] == '+' || hex[0] == '-' {
				return 0, fmt.Errorf("invalid hex flag `%s`", hex)
			}
			out |= uint32(bits)
			continue
		}
		found := false
		for _, d := range defs {
			if d.name == flag {
				out |= d.bit
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("unrecognized named flag `%s`", flag)
		}
	}
	return out, nil
}

func decodeFlags(v any, defs []flagDef) (uint32, error) {
	s, ok := v.(string)
	if !ok {
		return 0, InvalidTypeError(v, "a string value of `|` separated flags")
	}
	return parseFlagsText(s, defs)
}

func writeFlagsDebug(b *strings.Builder, typeName string, bits uint32, defs []flagDef) {
	b.WriteString(typeName)
	b.WriteByte('(')
	if bits == 0 {
		b.WriteString("0x0")
	} else {
		b.WriteString(flagsText(bits, defs))
	}
	b.WriteByte(')')
}

// ScopeAttributes are bit flags describing a scope
type ScopeAttributes uint32

const (
	ScopeAttributesParallel    ScopeAttributes = 0b01
	ScopeAttributesRelocatable ScopeAttributes = 0b10
)

var scopeAttributeFlags = []flagDef{{"PARALLEL", 0b01}, {"RELOCATABLE", 0b10}}

// Contains reports whether every flag of other is set
func (a ScopeAttributes) Contains(other ScopeAttributes) bool { return a&other == other }

// String writes the bitflags text form
func (a ScopeAttributes) String() string { return flagsText(uint32(a), scopeAttributeFlags) }

func (a ScopeAttributes) rustDebug(b *strings.Builder) {
	writeFlagsDebug(b, "ScopeAttributes", uint32(a), scopeAttributeFlags)
}

func (a ScopeAttributes) serdeSerialize() (any, error) { return a.String(), nil }

func (a *ScopeAttributes) serdeDeserialize(v any) error {
	bits, err := decodeFlags(v, scopeAttributeFlags)
	*a = ScopeAttributes(bits)
	return err
}

// MarshalJSON writes the serde form
func (a ScopeAttributes) MarshalJSON() ([]byte, error) { return EncodeJSON(a) }

// UnmarshalJSON reads the serde form
func (a *ScopeAttributes) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, a) }

// ScopeType classifies a scope
type ScopeType uint8

const (
	ScopeTypeAgent ScopeType = iota + 1
	ScopeTypeFunction
	ScopeTypeTool
	ScopeTypeLLM
	ScopeTypeRetriever
	ScopeTypeEmbedder
	ScopeTypeReranker
	ScopeTypeGuardrail
	ScopeTypeEvaluator
	ScopeTypeCustom
	ScopeTypeUnknown
)

var scopeTypeNames = []string{"agent", "function", "tool", "llm", "retriever", "embedder", "reranker", "guardrail", "evaluator", "custom", "unknown"}

var scopeTypeDebug = []string{"Agent", "Function", "Tool", "Llm", "Retriever", "Embedder", "Reranker", "Guardrail", "Evaluator", "Custom", "Unknown"}

// AsStr returns the lowercase name
func (s ScopeType) AsStr() string {
	if s < 1 || int(s) > len(scopeTypeNames) {
		return "unknown"
	}
	return scopeTypeNames[s-1]
}

// String returns the lowercase name
func (s ScopeType) String() string { return s.AsStr() }

func (s ScopeType) rustDebug(b *strings.Builder) {
	if s >= 1 && int(s) <= len(scopeTypeDebug) {
		b.WriteString(scopeTypeDebug[s-1])
	}
}

func (s ScopeType) serdeSerialize() (any, error) { return s.AsStr(), nil }

// Decodes a scope type mapping unknown names to Unknown as serde other does
func (s *ScopeType) serdeDeserialize(v any) error {
	name, content, has, err := splitExternalTag(v)
	if err != nil {
		return err
	}
	idx := -1
	for i, n := range scopeTypeNames {
		if n == name {
			idx = i
		}
	}
	if idx < 0 {
		idx = int(ScopeTypeUnknown) - 1
	}
	if err := unitVariantContent(content, has); err != nil {
		return err
	}
	*s = ScopeType(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (s ScopeType) MarshalJSON() ([]byte, error) { return EncodeJSON(s) }

// UnmarshalJSON reads the serde form
func (s *ScopeType) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, s) }

// ScopeStackEntry is one scope of a stack snapshot
type ScopeStackEntry struct {
	ScopeID   uuid.UUID `json:"scope_id"`
	ScopeType ScopeType `json:"scope_type"`
}

// MarshalJSON writes the serde form
func (e ScopeStackEntry) MarshalJSON() ([]byte, error) { return EncodeJSON(e) }

// UnmarshalJSON reads the serde form
func (e *ScopeStackEntry) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, e) }

// ScopeStackSnapshot is the ordered list of open scopes
type ScopeStackSnapshot struct {
	Scopes []ScopeStackEntry `json:"scopes"`
}

// MarshalJSON writes the serde form
func (s ScopeStackSnapshot) MarshalJSON() ([]byte, error) { return EncodeJSON(s) }

// UnmarshalJSON reads the serde form
func (s *ScopeStackSnapshot) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, s) }

// HandleAttributes holds exactly one of scope, tool or LLM attributes
type HandleAttributes struct {
	Scope *ScopeAttributes
	Tool  *ToolAttributes
	LLM   *LLMAttributes
}

// ScopeHandleAttributes wraps scope attributes
func ScopeHandleAttributes(a ScopeAttributes) HandleAttributes { return HandleAttributes{Scope: &a} }

// ToolHandleAttributes wraps tool attributes
func ToolHandleAttributes(a ToolAttributes) HandleAttributes { return HandleAttributes{Tool: &a} }

// LLMHandleAttributes wraps LLM attributes
func LLMHandleAttributes(a LLMAttributes) HandleAttributes { return HandleAttributes{LLM: &a} }

func (h HandleAttributes) serdeSerialize() (any, error) {
	switch {
	case h.Scope != nil:
		return orderedObject{}.with("Scope", *h.Scope), nil
	case h.Tool != nil:
		return orderedObject{}.with("Tool", *h.Tool), nil
	case h.LLM != nil:
		return orderedObject{}.with("Llm", *h.LLM), nil
	}
	return nil, fmt.Errorf("handle attributes hold no variant")
}

func (h *HandleAttributes) serdeDeserialize(v any) error {
	name, content, has, err := splitExternalTag(v)
	if err != nil {
		return err
	}
	idx, err := variantIndex(name, []string{"Scope", "Tool", "Llm"})
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("invalid type: unit variant, expected newtype variant")
	}
	*h = HandleAttributes{}
	switch idx {
	case 0:
		var a ScopeAttributes
		if err := a.serdeDeserialize(content); err != nil {
			return err
		}
		h.Scope = &a
	case 1:
		var a ToolAttributes
		if err := a.serdeDeserialize(content); err != nil {
			return err
		}
		h.Tool = &a
	default:
		var a LLMAttributes
		if err := a.serdeDeserialize(content); err != nil {
			return err
		}
		h.LLM = &a
	}
	return nil
}

func (h HandleAttributes) rustDebug(b *strings.Builder) {
	switch {
	case h.Scope != nil:
		b.WriteString("Scope(")
		h.Scope.rustDebug(b)
	case h.Tool != nil:
		b.WriteString("Tool(")
		h.Tool.rustDebug(b)
	case h.LLM != nil:
		b.WriteString("Llm(")
		h.LLM.rustDebug(b)
	default:
		return
	}
	b.WriteByte(')')
}

// MarshalJSON writes the serde form
func (h HandleAttributes) MarshalJSON() ([]byte, error) { return EncodeJSON(h) }

// UnmarshalJSON reads the serde form
func (h *HandleAttributes) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, h) }

// AttributesFromHandle lists the lowercase attribute names set on a handle
func AttributesFromHandle(h HandleAttributes) []string {
	switch {
	case h.Scope != nil:
		return ScopeAttributesToStrings(*h.Scope)
	case h.Tool != nil:
		return ToolAttributesToStrings(*h.Tool)
	case h.LLM != nil:
		return LLMAttributesToStrings(*h.LLM)
	}
	return []string{}
}

// ScopeAttributesToStrings lists the lowercase names of set scope attributes
func ScopeAttributesToStrings(a ScopeAttributes) []string {
	out := []string{}
	if a.Contains(ScopeAttributesParallel) {
		out = append(out, "parallel")
	}
	if a.Contains(ScopeAttributesRelocatable) {
		out = append(out, "relocatable")
	}
	return out
}

// ToolAttributesToStrings lists the lowercase names of set tool attributes
func ToolAttributesToStrings(a ToolAttributes) []string {
	out := []string{}
	if a.Contains(ToolAttributesRemote) {
		out = append(out, "remote")
	}
	return out
}

// LLMAttributesToStrings lists the lowercase names of set LLM attributes
func LLMAttributesToStrings(a LLMAttributes) []string {
	out := []string{}
	if a.Contains(LLMAttributesStateful) {
		out = append(out, "stateful")
	}
	if a.Contains(LLMAttributesStreaming) {
		out = append(out, "streaming")
	}
	return out
}

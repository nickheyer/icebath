package wire

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

// One conversation entry in the form every protocol converts from, Parts non-nil selecting list content
type Message struct {
	Role               Role
	Text               string
	Parts              []Part
	Name               string
	ToolCalls          []ToolCall
	ToolCallID         string
	ToolName           string
	FinishReason       FinishReason
	Reasoning          string
	ReasoningContent   *string
	ReasoningDetails   []json.RawMessage
	ResponsesReasoning []json.RawMessage
	ResponsesMessages  []json.RawMessage
	ReasoningTrimmed   bool
	AnthropicBlocks    []json.RawMessage
	BedrockBlocks      []json.RawMessage
	ThoughtSignature   string
	Carriers           map[string]string
	Refusal            string
	Cache              *CacheMarker
	DataUse            []string

	src *messageSource
}

// Decoded dictionary a message came from and the field values it held then
type messageSource struct {
	shape *shape
	snap  *Message
}

// One member of a message dictionary
type Member struct {
	Key   string
	Value json.RawMessage
}

// Returned when text handed to a decoder is not valid JSON of the kind the type holds
var ErrInvalidJSON = errors.New("wire: JSON text is not a value this type holds")

// Independent copy whose slices and maps can be edited without touching the original
func (m Message) Clone() Message {
	m.Parts = cloneParts(m.Parts)
	m.ToolCalls = slices.Clone(m.ToolCalls)
	if m.ReasoningContent != nil {
		v := *m.ReasoningContent
		m.ReasoningContent = &v
	}
	m.ReasoningDetails = slices.Clone(m.ReasoningDetails)
	m.ResponsesReasoning = slices.Clone(m.ResponsesReasoning)
	m.ResponsesMessages = slices.Clone(m.ResponsesMessages)
	m.AnthropicBlocks = slices.Clone(m.AnthropicBlocks)
	m.BedrockBlocks = slices.Clone(m.BedrockBlocks)
	m.Carriers = maps.Clone(m.Carriers)
	if m.Cache != nil {
		c := *m.Cache
		m.Cache = &c
	}
	m.DataUse = slices.Clone(m.DataUse)
	return m
}

// Copies a part list along with each part's cache marker
func cloneParts(parts []Part) []Part {
	out := slices.Clone(parts)
	for i := range out {
		if out[i].Cache != nil {
			c := *out[i].Cache
			out[i].Cache = &c
		}
	}
	return out
}

// Returned when a message, part or tool call reaches the standard encoder, which re-spaces and re-escapes replayed bytes
var ErrInexactEncoder = errors.New("wire: the standard JSON encoder rewrites replayed bytes, encode with AppendJSON")

// Refuses the standard encoder so the byte-exact writers are the only way to encode a message
func (m Message) MarshalJSON() ([]byte, error) {
	return nil, ErrInexactEncoder
}

// Appends the stored dictionary, members untouched since decoding coming back byte for byte
func (m *Message) AppendJSON(dst []byte) []byte {
	return m.appendJSON(dst, 0)
}

// Stored dictionary as text
func (m *Message) JSON() json.RawMessage {
	return m.appendJSON(nil, 0)
}

// Appends the messages as one JSON array, each written byte-exact
func AppendMessages(dst []byte, messages []Message) []byte {
	return appendArray(dst, len(messages), 0, func(dst []byte, i int) []byte { return messages[i].appendJSON(dst, 0) })
}

// Reads a stored dictionary, remembering member order, unknown members and every value it cannot rebuild
func (m *Message) UnmarshalJSON(data []byte) error {
	data = trimJSON(data)
	if string(data) == "null" {
		return nil
	}
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		return ErrInvalidJSON
	}
	*m = Message{}
	m.decode(data)
	return nil
}

// Dictionary form as ordered members, the view a protocol filters when it builds a wire message
func (m *Message) Members() []Member {
	var out []Member
	l := layout(0)
	sh := m.sourceShape()
	if sh != nil {
		l = sh.layout
	}
	sh.walk(m.fields(), func(key string, raw []byte, f *field) bool {
		if f != nil {
			var ok bool
			if raw, ok = f.enc(nil, l); !ok {
				return false
			}
		}
		out = append(out, Member{key, raw})
		return true
	})
	return out
}

// Compact JSON object holding the members in order
func Object(members []Member) json.RawMessage {
	dst := []byte{'{'}
	for i, mb := range members {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(appendString(dst, mb.Key, 0), ':')
		dst = append(dst, mb.Value...)
	}
	return append(dst, '}')
}

// Decoded shape behind the message, nil for one built in code
func (m *Message) sourceShape() *shape {
	if m.src == nil {
		return nil
	}
	return m.src.shape
}

// Appends the dictionary in its own layout, or the ambient one for a message built in code
func (m *Message) appendJSON(dst []byte, ambient layout) []byte {
	return m.sourceShape().render(dst, m.fields(), ambient)
}

// Fills the fields from a valid object and settles which member values must be kept verbatim
func (m *Message) decode(data []byte) {
	sh, _ := readShape(data)
	for _, mb := range sh.members {
		m.setField(mb.key, mb.raw)
	}
	if keys := carrierRecordKeys(m.ReasoningDetails); len(keys) > 0 {
		for _, mb := range sh.members {
			if _, named := keys[mb.key]; !named {
				continue
			}
			if s, ok := stringValue(mb.raw); ok {
				if m.Carriers == nil {
					m.Carriers = map[string]string{}
				}
				m.Carriers[mb.key] = s
			}
		}
	}
	snap := m.Clone()
	m.src = &messageSource{shape: sh, snap: &snap}
	if !sh.settle(m.fields()) {
		m.src.snap = nil
	}
}

// Stores one known member into its field, leaving unknown keys and unusable values to the shape
func (m *Message) setField(key string, raw []byte) {
	switch key {
	case "role":
		s, _ := stringValue(raw)
		m.Role = Role(s)
	case "content":
		m.Text, m.Parts = "", nil
		if s, ok := stringValue(raw); ok {
			m.Text = s
		} else if len(raw) > 0 && raw[0] == '[' {
			spans := splitArray(raw)
			m.Parts = make([]Part, len(spans))
			for i, sp := range spans {
				m.Parts[i].decode(raw[sp.start:sp.end])
			}
		}
	case "name":
		m.Name, _ = stringValue(raw)
	case "tool_calls":
		m.ToolCalls = decodeToolCalls(raw)
	case "tool_call_id":
		m.ToolCallID, _ = stringValue(raw)
	case "tool_name":
		m.ToolName, _ = stringValue(raw)
	case "finish_reason":
		s, _ := stringValue(raw)
		m.FinishReason = FinishReason(s)
	case "reasoning":
		m.Reasoning, _ = stringValue(raw)
	case "reasoning_content":
		m.ReasoningContent = nil
		if s, ok := stringValue(raw); ok {
			m.ReasoningContent = &s
		}
	case "reasoning_details":
		m.ReasoningDetails, _ = rawElements(raw)
	case "codex_reasoning_items":
		m.ResponsesReasoning, _ = rawElements(raw)
	case "codex_message_items":
		m.ResponsesMessages, _ = rawElements(raw)
	case "codex_reasoning_trimmed":
		m.ReasoningTrimmed = string(raw) == "true"
	case "anthropic_content_blocks":
		m.AnthropicBlocks, _ = rawElements(raw)
	case "bedrock_content_blocks":
		m.BedrockBlocks, _ = rawElements(raw)
	case "extra_content":
		m.ThoughtSignature = ThoughtSignature(raw)
	case "cache_control":
		m.Cache = parseCache(raw)
	case "data_use":
		m.DataUse = nil
		if len(raw) > 0 && raw[0] == '[' {
			for _, sp := range splitArray(raw) {
				s, ok := stringValue(raw[sp.start:sp.end])
				if !ok {
					m.DataUse = nil
					break
				}
				m.DataUse = append(m.DataUse, s)
			}
		}
	}
}

// Tool calls of a tool_calls value, nil unless every element is an object
func decodeToolCalls(raw []byte) []ToolCall {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	spans := splitArray(raw)
	for _, sp := range spans {
		if raw[sp.start] != '{' {
			return nil
		}
	}
	calls := make([]ToolCall, len(spans))
	for i, sp := range spans {
		calls[i].decode(raw[sp.start:sp.end])
	}
	return calls
}

// Field table of the message in the order a message built in code is written
func (m *Message) fields() []field {
	old, fresh := &Message{}, true
	if m.src != nil && m.src.snap != nil {
		old, fresh = m.src.snap, false
	}
	fields := []field{
		textField("role", (*string)(&m.Role), (*string)(&old.Role)),
		textField("name", &m.Name, &old.Name),
		textField("tool_name", &m.ToolName, &old.ToolName),
		{
			key:  "content",
			zero: `""`,
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if m.Parts != nil {
					return appendArray(dst, len(m.Parts), l, func(dst []byte, i int) []byte {
						return m.Parts[i].appendJSON(dst, l)
					}), true
				}
				if m.Text == "" {
					return dst, false
				}
				return appendString(dst, m.Text, l), true
			},
			same: func() bool { return m.Text == old.Text && sameParts(m.Parts, old.Parts) },
		},
		textField("tool_call_id", &m.ToolCallID, &old.ToolCallID),
		textField("reasoning", &m.Reasoning, &old.Reasoning),
		textField("finish_reason", (*string)(&m.FinishReason), (*string)(&old.FinishReason)),
		{
			key: "reasoning_content",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if m.ReasoningContent == nil {
					return dst, false
				}
				return appendString(dst, *m.ReasoningContent, l), true
			},
			same: func() bool {
				if m.ReasoningContent == nil || old.ReasoningContent == nil {
					return m.ReasoningContent == old.ReasoningContent
				}
				return *m.ReasoningContent == *old.ReasoningContent
			},
		},
		rawsField("reasoning_details", &m.ReasoningDetails, &old.ReasoningDetails),
		rawsField("anthropic_content_blocks", &m.AnthropicBlocks, &old.AnthropicBlocks),
		rawsField("bedrock_content_blocks", &m.BedrockBlocks, &old.BedrockBlocks),
		rawsField("codex_reasoning_items", &m.ResponsesReasoning, &old.ResponsesReasoning),
		rawsField("codex_message_items", &m.ResponsesMessages, &old.ResponsesMessages),
		{
			key: "extra_content",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if m.ThoughtSignature == "" {
					return dst, false
				}
				return appendSignature(dst, m.ThoughtSignature, l), true
			},
			same: func() bool { return m.ThoughtSignature == old.ThoughtSignature },
		},
	}
	for _, key := range carrierKeys(m.Carriers, old.Carriers) {
		if reservedKey(key) {
			continue
		}
		fields = append(fields, field{
			key: key,
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if m.Carriers[key] == "" {
					return dst, false
				}
				return appendString(dst, m.Carriers[key], l), true
			},
			same: func() bool { return m.Carriers[key] == old.Carriers[key] },
		})
	}
	fields = append(fields,
		field{
			key: "tool_calls",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if len(m.ToolCalls) == 0 {
					return dst, false
				}
				return appendArray(dst, len(m.ToolCalls), l, func(dst []byte, i int) []byte {
					return m.ToolCalls[i].appendJSON(dst, l)
				}), true
			},
			same: func() bool { return sameCalls(m.ToolCalls, old.ToolCalls) },
		},
		field{
			key: "data_use",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if len(m.DataUse) == 0 {
					return dst, false
				}
				return appendArray(dst, len(m.DataUse), l, func(dst []byte, i int) []byte {
					return appendString(dst, m.DataUse[i], l)
				}), true
			},
			same: func() bool { return slices.Equal(m.DataUse, old.DataUse) },
		},
		field{
			key: "codex_reasoning_trimmed",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				if !m.ReasoningTrimmed {
					return dst, false
				}
				return append(dst, "true"...), true
			},
			same: func() bool { return m.ReasoningTrimmed == old.ReasoningTrimmed },
		},
		cacheField(&m.Cache, &old.Cache),
	)
	if fresh {
		return withoutSame(fields)
	}
	return fields
}

// Sorted union of the carrier keys two maps hold
func carrierKeys(a, b map[string]string) []string {
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, dup := a[k]; !dup {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// Reports whether two part lists hold the same parts in the same content form
func sameParts(a, b []Part) bool {
	if len(a) != len(b) || (a == nil) != (b == nil) {
		return false
	}
	for i := range a {
		if !a[i].equal(&b[i]) {
			return false
		}
	}
	return true
}

// Reports whether two tool call lists hold the same calls
func sameCalls(a, b []ToolCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].equal(&b[i]) {
			return false
		}
	}
	return true
}

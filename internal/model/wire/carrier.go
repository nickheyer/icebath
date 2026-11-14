package wire

import (
	"encoding/json"
	"slices"
	"strings"
)

// Type word of the private reasoning_details record that persists message carriers
const CarrierType = "icebath.native_assistant"

// Assistant message keys the provider profiles declare, opaque blobs and readable text
type CarrierKeys struct {
	Opaque []string
	Text   []string
}

// Carriers gathered across the deltas of one stream, the latest opaque value and the joined text per key
type StreamCarriers struct {
	keys      CarrierKeys
	signature string
	opaque    map[string]string
	text      map[string]*strings.Builder
}

// Gemini thought signature inside an extra_content value, under google or flat, empty when blank
func ThoughtSignature(extra []byte) string {
	var sig json.RawMessage
	if google, ok := objectMember(extra, "google"); ok && len(google) > 0 && google[0] == '{' {
		sig, _ = objectMember(google, "thought_signature")
	} else {
		sig, _ = objectMember(extra, "thought_signature")
	}
	s, _ := stringValue(sig)
	if trimSpace(s) == "" {
		return ""
	}
	return s
}

// Appends the extra_content object that carries a thought signature
func appendSignature(dst []byte, sig string, l layout) []byte {
	dst = l.colon(append(dst, `{"google"`...))
	dst = l.colon(append(dst, `{"thought_signature"`...))
	return append(appendString(dst, sig, l), '}', '}')
}

// Type word of a reasoning_details entry, empty when it has none
func DetailType(detail []byte) string {
	raw, _ := objectMember(detail, "type")
	s, _ := stringValue(raw)
	return s
}

// First carrier record among reasoning details
func carrierRecord(details []json.RawMessage) (json.RawMessage, bool) {
	for _, d := range details {
		if DetailType(d) == CarrierType {
			return trimJSON(d), true
		}
	}
	return nil, false
}

// Profile-declared keys the carrier record among the details names
func carrierRecordKeys(details []json.RawMessage) map[string]struct{} {
	record, ok := carrierRecord(details)
	if !ok {
		return nil
	}
	keys := map[string]struct{}{}
	spans, _, _ := splitObject(record)
	for _, sp := range spans {
		if _, isText := stringValue(record[sp.start:sp.end]); isText && !reservedKey(sp.key) {
			keys[sp.key] = struct{}{}
		}
	}
	return keys
}

// Reports whether a key belongs to the message dictionary itself and can never name a carrier
func reservedKey(key string) bool {
	switch key {
	case "type", "role", "content", "name", "tool_calls", "tool_call_id", "tool_name", "finish_reason", "reasoning",
		"reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items",
		"codex_reasoning_trimmed", "anthropic_content_blocks", "bedrock_content_blocks", "extra_content",
		"cache_control", "data_use":
		return true
	}
	return false
}

// Record that persists the message's thought signature and carriers, nil when it has neither
func CarrierRecord(m *Message) json.RawMessage {
	dst := append([]byte(`{"type":`), `"`+CarrierType+`"`...)
	empty := len(dst)
	if m.ThoughtSignature != "" {
		dst = appendSignature(append(dst, `,"extra_content":`...), m.ThoughtSignature, 0)
	}
	for _, key := range carrierKeys(m.Carriers, nil) {
		if m.Carriers[key] == "" || reservedKey(key) {
			continue
		}
		dst = append(appendString(append(dst, ','), key, 0), ':')
		dst = appendString(dst, m.Carriers[key], 0)
	}
	if len(dst) == empty {
		return nil
	}
	return append(dst, '}')
}

// Fills the thought signature and carriers a message lacks from its persisted record, false when it has none
func LiftCarriers(m *Message) bool {
	record, ok := carrierRecord(m.ReasoningDetails)
	if !ok {
		return false
	}
	spans, _, _ := splitObject(record)
	for _, sp := range spans {
		raw := record[sp.start:sp.end]
		if sp.key == "extra_content" {
			if m.ThoughtSignature == "" {
				m.ThoughtSignature = ThoughtSignature(raw)
			}
			continue
		}
		s, isText := stringValue(raw)
		if !isText || reservedKey(sp.key) {
			continue
		}
		if _, live := m.Carriers[sp.key]; !live {
			if m.Carriers == nil {
				m.Carriers = map[string]string{}
			}
			m.Carriers[sp.key] = s
		}
	}
	return true
}

// Copies the signature and declared carriers of a provider message object onto m, returning its first text carrier
func (k CarrierKeys) Capture(m *Message, message []byte) string {
	if extra, ok := objectMember(message, "extra_content"); ok {
		if sig := ThoughtSignature(extra); sig != "" {
			m.ThoughtSignature = sig
		}
	}
	readable := ""
	for i, key := range slices.Concat(k.Opaque, k.Text) {
		raw, _ := objectMember(message, key)
		s, _ := stringValue(raw)
		if s == "" || reservedKey(key) {
			continue
		}
		if m.Carriers == nil {
			m.Carriers = map[string]string{}
		}
		m.Carriers[key] = s
		if i >= len(k.Opaque) && readable == "" {
			readable = s
		}
	}
	return readable
}

// Starts gathering these carriers across the deltas of one stream
func (k CarrierKeys) Stream() *StreamCarriers {
	return &StreamCarriers{keys: k, opaque: map[string]string{}, text: map[string]*strings.Builder{}}
}

// Absorbs one delta object, returning its first text carrier fragment as readable reasoning
func (s *StreamCarriers) Feed(delta []byte) string {
	if extra, ok := objectMember(delta, "extra_content"); ok {
		if sig := ThoughtSignature(extra); sig != "" {
			s.signature = sig
		}
	}
	for _, key := range s.keys.Opaque {
		raw, _ := objectMember(delta, key)
		if v, _ := stringValue(raw); v != "" {
			s.opaque[key] = v
		}
	}
	readable := ""
	for _, key := range s.keys.Text {
		raw, _ := objectMember(delta, key)
		v, _ := stringValue(raw)
		if v == "" {
			continue
		}
		b := s.text[key]
		if b == nil {
			b = &strings.Builder{}
			s.text[key] = b
		}
		b.WriteString(v)
		if readable == "" {
			readable = v
		}
	}
	return readable
}

// Puts the gathered signature and carriers on the assembled message
func (s *StreamCarriers) Apply(m *Message) {
	if s.signature != "" {
		m.ThoughtSignature = s.signature
	}
	set := func(key, value string) {
		if reservedKey(key) {
			return
		}
		if m.Carriers == nil {
			m.Carriers = map[string]string{}
		}
		m.Carriers[key] = value
	}
	for key, value := range s.opaque {
		set(key, value)
	}
	for key, b := range s.text {
		set(key, b.String())
	}
}

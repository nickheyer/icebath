package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reports whether r is whitespace, the Unicode spaces plus the four information separators
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Trims whitespace of every script from both ends
func trimSpace(s string) string {
	return strings.TrimFunc(s, isSpace)
}

// Reports whether a part type word names media that carries no text
func mediaType(typeName string) bool {
	switch strings.ToLower(trimSpace(typeName)) {
	case "image", "image_url", "input_image", "audio", "input_audio":
		return true
	}
	return false
}

// Members a content part may keep its visible text under, in the order they are tried
func textKeys() [5]string {
	return [5]string{"text", "content", "input_text", "output_text", "summary_text"}
}

// Visible text of one raw content part, empty for media and for anything that is not a string or object
func rawPartText(raw []byte) string {
	raw = trimJSON(raw)
	if s, ok := stringValue(raw); ok {
		return s
	}
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	if typ, ok := objectMember(raw, "type"); ok {
		if s, isText := stringValue(typ); isText && mediaType(s) {
			return ""
		}
	}
	for _, key := range textKeys() {
		if value, ok := objectMember(raw, key); ok {
			if s, isText := stringValue(value); isText {
				return s
			}
		}
	}
	return ""
}

// Visible text of raw message content, parts joined with sep and any other value shown as its JSON text
func FlattenContent(content []byte, sep string) string {
	content = trimJSON(content)
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	if s, ok := stringValue(content); ok {
		return s
	}
	if content[0] == '[' {
		var chunks []string
		for _, sp := range splitArray(content) {
			if chunk := rawPartText(content[sp.start:sp.end]); chunk != "" {
				chunks = append(chunks, chunk)
			}
		}
		return strings.Join(chunks, sep)
	}
	if text := rawPartText(content); text != "" {
		return text
	}
	return string(content)
}

// Visible text of the message content, list parts joined with sep
func (m *Message) FlatText(sep string) string {
	if m.Parts != nil {
		var chunks []string
		for i := range m.Parts {
			if chunk := m.Parts[i].flatText(); chunk != "" {
				chunks = append(chunks, chunk)
			}
		}
		return strings.Join(chunks, sep)
	}
	if m.Text == "" && m.src != nil && m.src.snap != nil && m.src.snap.Text == "" && m.src.snap.Parts == nil {
		if raw, ok := m.src.shape.last("content"); ok && raw != nil {
			return FlattenContent(raw, sep)
		}
	}
	return m.Text
}

// Visible text of one part
func (p *Part) flatText() string {
	if len(p.Native) > 0 {
		return rawPartText(p.Native)
	}
	textual := p.Kind == PartText || p.Kind == 0
	if mediaType(p.TypeName) || (p.TypeName == "" && !textual) {
		return ""
	}
	own := "text"
	var sh *shape
	if p.src != nil {
		if p.src.textKey != "" {
			own = p.src.textKey
		}
		sh = p.src.shape
	}
	if sh == nil {
		if textual {
			return p.Text
		}
		return ""
	}
	for _, key := range textKeys() {
		if key == own && textual {
			return p.Text
		}
		if raw, ok := sh.last(key); ok && raw != nil {
			if s, isText := stringValue(raw); isText {
				return s
			}
		}
	}
	return ""
}

// Returns delta with a paragraph break in front when a closed bold heading butts against previous text
func SeparateGluedReasoning(previous, delta string) string {
	if previous == "" || !strings.HasPrefix(delta, "**") || !strings.Contains(delta[2:], "**") {
		return delta
	}
	if last, _ := utf8.DecodeLastRuneInString(previous); isSpace(last) {
		return delta
	}
	return "\n\n" + delta
}

// Member a streamed reasoning_details entry of a mergeable type keeps its text under
func detailTextKey(typ string) string {
	switch typ {
	case "reasoning.text":
		return "text"
	case "reasoning.summary":
		return "summary"
	}
	return ""
}

// Readable text of one streamed reasoning_details entry, empty for opaque replay material
func DetailText(detail []byte) string {
	key := detailTextKey(DetailType(detail))
	if key == "" {
		return ""
	}
	raw, _ := objectMember(detail, key)
	s, _ := stringValue(raw)
	return s
}

// Collects streamed reasoning_details, joining consecutive text fragments of one logical block
type DetailAccumulator struct {
	entries []detailEntry
}

// One collected entry, its text member held apart once fragments were joined onto it
type detailEntry struct {
	members []member
	typ     string
	joined  bool
	text    strings.Builder
}

// Adds one streamed entry and returns its readable text, ignoring values that are not objects
func (a *DetailAccumulator) Add(detail []byte) string {
	detail = trimJSON(detail)
	if len(detail) == 0 || detail[0] != '{' || !json.Valid(detail) {
		return ""
	}
	sh, _ := readShape(bytes.Clone(detail))
	typ := ""
	if raw, ok := sh.last("type"); ok {
		typ, _ = stringValue(raw)
	}
	key := detailTextKey(typ)
	fragment, mergeable := "", false
	if raw, ok := sh.last(key); ok && key != "" {
		fragment, mergeable = stringValue(raw)
	}
	if n := len(a.entries); n > 0 && mergeable && a.entries[n-1].typ == typ {
		a.entries[n-1].join(key, fragment, sh)
		return fragment
	}
	a.entries = append(a.entries, detailEntry{typ: typ, members: uniqueMembers(sh.members)})
	return fragment
}

// Members with each repeated key folded onto its first position holding its last value
func uniqueMembers(members []member) []member {
	at := make(map[string]int, len(members))
	out := members[:0]
	for _, mb := range members {
		if i, dup := at[mb.key]; dup {
			out[i].raw = mb.raw
			continue
		}
		at[mb.key] = len(out)
		out = append(out, mb)
	}
	return out
}

// Appends a fragment to the entry text and backfills replay members the entry still lacks
func (e *detailEntry) join(key, fragment string, from *shape) {
	if !e.joined {
		e.joined = true
		if raw, ok := e.value(key); ok {
			s, _ := stringValue(raw)
			e.text.WriteString(s)
		}
	}
	e.text.WriteString(fragment)
	for _, backfill := range [...]string{"signature", "id", "format", "index"} {
		have, _ := e.value(backfill)
		give, _ := from.last(backfill)
		if blank(have) && !blank(give) {
			e.set(backfill, give)
		}
	}
}

// Reports whether a member is absent, null or the empty string
func blank(raw []byte) bool {
	return len(raw) == 0 || string(raw) == "null" || string(raw) == `""`
}

// Last raw JSON the entry holds under a key
func (e *detailEntry) value(key string) (json.RawMessage, bool) {
	for i := len(e.members) - 1; i >= 0; i-- {
		if e.members[i].key == key {
			return e.members[i].raw, true
		}
	}
	return nil, false
}

// Replaces the value under a key in place, appending the member when the entry lacks it
func (e *detailEntry) set(key string, raw json.RawMessage) {
	for i := len(e.members) - 1; i >= 0; i-- {
		if e.members[i].key == key {
			e.members[i].raw = raw
			return
		}
	}
	e.members = append(e.members, member{key, raw})
}

// Collected entries as reasoning_details values, nil when nothing was added
func (a *DetailAccumulator) Details() []json.RawMessage {
	if len(a.entries) == 0 {
		return nil
	}
	out := make([]json.RawMessage, len(a.entries))
	for i := range a.entries {
		e := &a.entries[i]
		if e.joined {
			e.set(detailTextKey(e.typ), appendString(nil, e.text.String(), 0))
		}
		members := make([]Member, len(e.members))
		for n, mb := range e.members {
			members[n] = Member{mb.key, mb.raw}
		}
		out[i] = Object(members)
	}
	return out
}

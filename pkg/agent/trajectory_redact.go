package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Tags whose bodies a trajectory turn carries as JSON
var trajectoryJSONTags = []string{"tool_call", "tool_response"}

// Byte offsets of one tagged JSON block, the body between the opening tag with its whitespace and the whitespace before the closing tag
type trajectoryJSONBlock struct{ start, bodyStart, bodyEnd, end int }

// Redaction of text a durable file keeps whatever the display toggle says
func redactTrajectoryPlain(text string) string {
	return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
}

// Offset of the first tool tag opening at or after from and its tag name, -1 when none opens
func nextTrajectoryJSONTag(text string, from int) (int, string) {
	at, tag := -1, ""
	for _, name := range trajectoryJSONTags {
		if i := strings.Index(text[from:], "<"+name+">"); i >= 0 && (at < 0 || from+i < at) {
			at, tag = from+i, name
		}
	}
	return at, tag
}

// Tagged JSON blocks of a turn in order, an opening tag without its closing tag skipped
func trajectoryJSONBlocks(text string) []trajectoryJSONBlock {
	var blocks []trajectoryJSONBlock
	for from := 0; from < len(text); {
		open, tag := nextTrajectoryJSONTag(text, from)
		if open < 0 {
			break
		}
		bodyStart := open + len(tag) + 2
		for bodyStart < len(text) {
			r, size := utf8.DecodeRuneInString(text[bodyStart:])
			if !unicode.IsSpace(r) {
				break
			}
			bodyStart += size
		}
		closeTag := "</" + tag + ">"
		rel := strings.Index(text[bodyStart:], closeTag)
		if rel < 0 {
			from = open + 1
			continue
		}
		closeAt := bodyStart + rel
		bodyEnd := closeAt
		for bodyEnd > bodyStart {
			r, size := utf8.DecodeLastRuneInString(text[bodyStart:bodyEnd])
			if !unicode.IsSpace(r) {
				break
			}
			bodyEnd -= size
		}
		end := closeAt + len(closeTag)
		blocks = append(blocks, trajectoryJSONBlock{open, bodyStart, bodyEnd, end})
		from = end
	}
	return blocks
}

// Redacts one turn text, tool call and tool response JSON decoded first so a marker never eats its quotes or braces
func redactTrajectoryText(text string) string {
	var b strings.Builder
	pos := 0
	for _, block := range trajectoryJSONBlocks(text) {
		decoded, err := jsonutil.Decode(text[block.bodyStart:block.bodyEnd])
		if err != nil {
			continue
		}
		body, err := jsonutil.Encode(redactTrajectoryJSON(decoded), jsonutil.Spaced())
		if err != nil {
			continue
		}
		b.WriteString(redactTrajectoryPlain(text[pos:block.start]))
		b.WriteString(text[block.start:block.bodyStart])
		b.WriteString(body)
		b.WriteString(text[block.bodyEnd:block.end])
		pos = block.end
	}
	b.WriteString(redactTrajectoryPlain(text[pos:]))
	return b.String()
}

// Redacts decoded tool JSON, a string field going through the keyed pass that sees the field name
func redactTrajectoryJSON(value any) any {
	field := func(key string, item any) any {
		if s, ok := item.(string); ok {
			return redactTrajectoryField(key, s)
		}
		return redactTrajectoryJSON(item)
	}
	switch v := value.(type) {
	case *jsonutil.Object:
		if v == nil {
			return value
		}
		out := jsonutil.NewObject()
		for _, key := range v.Keys() {
			out.Set(key, field(key, v.Value(key)))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = field(key, item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactTrajectoryJSON(item)
		}
		return out
	}
	return RedactTrajectoryValue(value)
}

// Redacts one string field, retried as a one field document when the plain pass leaves it unchanged because decoding dropped the key the field rules match on
func redactTrajectoryField(key, value string) any {
	if cleaned := redactTrajectoryText(value); cleaned != value {
		return cleaned
	}
	doc, err := jsonutil.Encode(jsonutil.ObjectOf(key, value), jsonutil.Spaced())
	if err != nil {
		return "***"
	}
	decoded, err := jsonutil.Decode(redactTrajectoryPlain(doc))
	if err != nil {
		return "***"
	}
	fields, ok := jsonutil.AsMap(decoded)
	if !ok {
		return "***"
	}
	redacted, ok := fields[key]
	if !ok {
		return "***"
	}
	return redacted
}

// RedactTrajectoryValue redacts every string leaf of a trajectory value without letting a marker change its JSON syntax
func RedactTrajectoryValue(value any) any {
	switch v := value.(type) {
	case string:
		return redactTrajectoryText(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RedactTrajectoryValue(item)
		}
		return out
	case *jsonutil.Object:
		if v == nil {
			return value
		}
		out := jsonutil.NewObject()
		for _, key := range v.Keys() {
			out.Set(key, RedactTrajectoryValue(v.Value(key)))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = RedactTrajectoryValue(item)
		}
		return out
	}
	return value
}

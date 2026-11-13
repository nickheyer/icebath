package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// One operational record before a sink renders it
type record struct {
	time    time.Time
	level   Level
	target  string
	message string
	fields  map[string]any
}

// Event name and remaining fields of a record, the message field dropped
type collectedFields struct {
	eventName string
	fields    map[string]any
}

// Splits the event field out and normalizes each value the way rendered key values parse
func collectFields(fields map[string]any) collectedFields {
	out := collectedFields{fields: map[string]any{}}
	for name, value := range fields {
		switch name {
		case "event":
			out.eventName = renderedEventName(value)
		case "message":
		default:
			out.fields[name] = renderedFieldValue(value)
		}
	}
	return out
}

func renderedEventName(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// Display text of a field value parsed back as JSON, kept as a string when it does not parse
func renderedFieldValue(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return s
	}
	if _, err := decoder.Token(); err != io.EOF {
		return s
	}
	return parsed
}

// Rendered line of a record in the sink's format
func renderRecord(format LogFormat, rootRelayID string, r record) []byte {
	timestamp := r.time.UTC().Format("2006-01-02T15:04:05.000Z")
	fields := collectFields(r.fields)
	if format == LogFormatJSONL {
		body := map[string]any{
			"timestamp": timestamp, "level": jsonLevelName(r.level), "root_relay_id": rootRelayID, "target": r.target,
			"event": fields.eventName, "message": r.message, "fields": fields.fields,
		}
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(body); err != nil {
			return []byte(`{"timestamp":"","level":"error","root_relay_id":"","target":"","event":"format_error","message":"failed to serialize log record","fields":{}}` + "\n")
		}
		return buf.Bytes()
	}
	var line strings.Builder
	line.WriteString(timestamp + " " + humanLevelName(r.level) + " root=" + shortRootID(rootRelayID) + " target=" + r.target)
	if fields.eventName != "" {
		line.WriteString(" event=" + fields.eventName)
	}
	keys := make([]string, 0, len(fields.fields))
	for k := range fields.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line.WriteString(" " + k + "=" + formatHumanValue(fields.fields[k]))
	}
	if r.message != "" {
		line.WriteString(" " + r.message)
	}
	line.WriteString("\n")
	return []byte(line.String())
}

func jsonLevelName(level Level) string {
	if level <= LevelError {
		return "error"
	}
	return level.String()
}

func humanLevelName(level Level) string {
	return strings.ToUpper(jsonLevelName(level))
}

func shortRootID(rootRelayID string) string {
	head, _, _ := strings.Cut(rootRelayID, "-")
	return head
}

func formatHumanValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

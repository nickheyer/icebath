package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Export format or filter name no session exporter accepts, its message the one the export commands print
type FormatError struct {
	msg string
}

// Message naming the rejected format
func (e *FormatError) Error() string { return e.msg }

// Oldest and newest epoch second a stored timestamp may hold before it counts as corrupt
const (
	epochMin = 0.0
	epochMax = 4_200_000_000.0
)

// Decoded JSON value keeping object members in document order and numbers as written
type jv struct {
	kind byte
	str  string
	b    bool
	keys []string
	vals []*jv
}

// Session document of the session store export decoded in member order
func parseDoc(data []byte) (*jv, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeJV(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("session export document: trailing data after the top level value")
	}
	return v, nil
}

// Session documents decoded in order, the first malformed one failing the call
func parseDocs(docs [][]byte) ([]*jv, error) {
	out := make([]*jv, 0, len(docs))
	for _, d := range docs {
		v, err := parseDoc(d)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Next value of the decoder with its members in document order
func decodeJV(dec *json.Decoder) (*jv, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		v := &jv{kind: 'a'}
		if t == '{' {
			v.kind = 'o'
		}
		for dec.More() {
			if v.kind == 'o' {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := k.(string)
				v.keys = append(v.keys, key)
			}
			child, err := decodeJV(dec)
			if err != nil {
				return nil, err
			}
			v.vals = append(v.vals, child)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return v, nil
	case string:
		return jvString(t), nil
	case json.Number:
		return &jv{kind: 'n', str: string(t)}, nil
	case bool:
		return &jv{kind: 'b', b: t}, nil
	}
	return &jv{kind: 'z'}, nil
}

// String value s
func jvString(s string) *jv { return &jv{kind: 's', str: s} }

// Number value of an integer
func jvInt(n int) *jv { return &jv{kind: 'n', str: strconv.Itoa(n)} }

// Shape of the JSON text an export writes, one line unless indented, members in order unless sorted
type jsonLayout struct {
	indent int
	sorted bool
}

// JSON of the value with raw UTF-8 and the separators the reference writes
func (v *jv) text(l jsonLayout) string {
	var b strings.Builder
	v.render(&b, l, 0)
	return b.String()
}

// Appends the JSON text of the value at nesting depth in the layout
func (v *jv) render(b *strings.Builder, l jsonLayout, depth int) {
	switch v.kind {
	case 'o', 'a':
		open, closing := "[", "]"
		if v.kind == 'o' {
			open, closing = "{", "}"
		}
		if len(v.vals) == 0 {
			b.WriteString(open + closing)
			return
		}
		order := make([]int, len(v.vals))
		for i := range order {
			order[i] = i
		}
		if l.sorted && v.kind == 'o' {
			slices.SortStableFunc(order, func(a, c int) int { return strings.Compare(v.keys[a], v.keys[c]) })
		}
		b.WriteString(open)
		for i, idx := range order {
			switch {
			case i > 0 && l.indent > 0:
				b.WriteString(",")
			case i > 0:
				b.WriteString(", ")
			}
			if l.indent > 0 {
				b.WriteString("\n" + strings.Repeat(" ", l.indent*(depth+1)))
			}
			if v.kind == 'o' {
				quoteJSON(b, v.keys[idx])
				b.WriteString(": ")
			}
			v.vals[idx].render(b, l, depth+1)
		}
		if l.indent > 0 {
			b.WriteString("\n" + strings.Repeat(" ", l.indent*depth))
		}
		b.WriteString(closing)
	case 's':
		quoteJSON(b, v.str)
	case 'n':
		b.WriteString(v.str)
	case 'b':
		b.WriteString(strconv.FormatBool(v.b))
	default:
		b.WriteString("null")
	}
}

// Appends s as a JSON string with quotes, backslashes and control characters escaped and the rest raw
func quoteJSON(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Number value of a float written in the shortest round trip digits with a fractional part
func jvFloat(f float64) *jv {
	text := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return &jv{kind: 'n', str: text}
}

// Member key of an object, nil when absent or when the value is no object
func (v *jv) get(key string) *jv {
	if v == nil || v.kind != 'o' {
		return nil
	}
	for i, k := range v.keys {
		if k == key {
			return v.vals[i]
		}
	}
	return nil
}

// Copy of the object whose key member is val, replaced in place or appended at the end
func (v *jv) with(key string, val *jv) *jv {
	out := &jv{kind: 'o', keys: append([]string(nil), v.keys...), vals: append([]*jv(nil), v.vals...)}
	for i, k := range out.keys {
		if k == key {
			out.vals[i] = val
			return out
		}
	}
	out.keys = append(out.keys, key)
	out.vals = append(out.vals, val)
	return out
}

// Reports whether the value is missing or the JSON literal for nothing
func (v *jv) null() bool { return v == nil || v.kind == 'z' }

// Reports whether the value is present and not null, false, zero, an empty string or an empty container
func (v *jv) truthy() bool {
	switch {
	case v.null():
		return false
	case v.kind == 's':
		return v.str != ""
	case v.kind == 'b':
		return v.b
	case v.kind == 'n':
		f, err := strconv.ParseFloat(v.str, 64)
		return err != nil || f != 0
	}
	return len(v.vals) > 0
}

// First value that is truthy, else the last one
func first(vals ...*jv) *jv {
	for _, v := range vals[:len(vals)-1] {
		if v.truthy() {
			return v
		}
	}
	return vals[len(vals)-1]
}

// Text the exports show for a value, strings as they are, scalars in their reference spelling, containers as JSON
func (v *jv) display() string {
	switch {
	case v.null():
		return "None"
	case v.kind == 's', v.kind == 'n':
		return v.str
	case v.kind == 'b' && v.b:
		return "True"
	case v.kind == 'b':
		return "False"
	}
	return v.text(jsonLayout{})
}

// Object elements of an array value
func (v *jv) objects() []*jv {
	if v == nil || v.kind != 'a' {
		return nil
	}
	var out []*jv
	for _, child := range v.vals {
		if child.kind == 'o' {
			out = append(out, child)
		}
	}
	return out
}

// Stored timestamp as epoch seconds, false for an unset cell or a corrupt one warned about with its session
func coerceEpoch(v *jv, session, field string) (float64, bool) {
	if v.null() || (v.kind == 's' && v.str == "") {
		return 0, false
	}
	ts := math.NaN()
	switch v.kind {
	case 'n':
		ts, _ = strconv.ParseFloat(v.str, 64)
	case 's':
		if f, err := strconv.ParseFloat(strings.TrimSpace(v.str), 64); err == nil {
			ts = f
		}
	case 'b':
		ts = 0
		if v.b {
			ts = 1
		}
	}
	if !(ts >= epochMin && ts <= epochMax) {
		suffix := ""
		if session != "" {
			suffix = " on session " + session
		}
		shown := v.display()
		if v.kind == 's' {
			shown = strconv.Quote(v.str)
		}
		Named(slog.Default(), "cli.export").Warn("Ignoring corrupt " + field + " " + shown + suffix)
		return 0, false
	}
	return ts, true
}

// Time of epoch seconds rounded to the microsecond half to even
func epochTime(ts float64) time.Time {
	sec := math.Floor(ts)
	us := math.RoundToEven((ts - sec) * 1e6)
	if us >= 1e6 {
		sec++
		us -= 1e6
	}
	return time.Unix(int64(sec), int64(us)*int64(time.Microsecond))
}

// UTC timestamp in whole seconds with a Z, the raw text of a corrupt cell, false for an unset one
func exportTime(v *jv, session string) (string, bool) {
	if v.null() {
		return "", false
	}
	ts, ok := coerceEpoch(v, session, "timestamp")
	if !ok {
		if v.kind == 's' {
			return v.str, true
		}
		return v.display(), true
	}
	return epochTime(ts).UTC().Format("2006-01-02T15:04:05Z"), true
}

// UTC timestamp with any microseconds and a Z, raw text for a corrupt cell, empty when unset
func isoMicro(v *jv) string {
	if v.null() || (v.kind == 's' && v.str == "") {
		return ""
	}
	ts, ok := coerceEpoch(v, "", "timestamp")
	if !ok {
		return v.display()
	}
	return isoSeconds(ts)
}

// UTC timestamp of epoch seconds with microseconds when present and a Z
func isoSeconds(ts float64) string {
	t := epochTime(ts).UTC()
	if t.Nanosecond() != 0 {
		return t.Format("2006-01-02T15:04:05.000000Z")
	}
	return t.Format("2006-01-02T15:04:05Z")
}

// Lines of s split at every line boundary the reference splits on, without their endings
func textLines(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if i < start {
			continue
		}
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			start = i + utf8.RuneLen(r)
		case '\r':
			out = append(out, s[start:i])
			start = i + 1
			if strings.HasPrefix(s[start:], "\n") {
				start++
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Text on one line, its line breaks turned into spaces and the ends trimmed, unknown when nothing is left
func headingText(s string) string {
	if t := strings.TrimSpace(strings.Join(textLines(s), " ")); t != "" {
		return t
	}
	return "unknown"
}

// Text with the first rune title cased and the rest lower cased
func capitalized(s string) string {
	for i, r := range s {
		return string(unicode.ToTitle(r)) + strings.ToLower(s[i+len(string(r)):])
	}
	return s
}

// Text with the first letter of every run of cased letters title cased and the rest lower cased
func titled(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if prevCased {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(unicode.ToTitle(r))
		}
		prevCased = unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
	}
	return b.String()
}

// Text with ampersands, angle brackets and both quotes written as HTML entities, single quotes as hex
func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

// Text in a fence longer than any backtick run inside it
func fenced(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "text\n" + text + "\n" + fence
}

// Message objects of a session document
func messagesOf(s *jv) []*jv { return s.get("messages").objects() }

// Id a session document is shown under, unknown when it has none
func sessionID(s *jv) string {
	return first(s.get("id"), s.get("session_id"), jvString("unknown")).display()
}

// Text of one content part, its text or content string, else its sorted JSON
func partText(p *jv) string {
	if p.kind != 'o' {
		if p.kind == 's' {
			return p.str
		}
		return p.display()
	}
	for _, key := range []string{"text", "content"} {
		if v := p.get(key); v != nil && v.kind == 's' {
			return v.str
		}
	}
	return p.text(jsonLayout{sorted: true})
}

// Text of message content, list parts joined by newlines and an unset content empty
func messageText(content *jv) string {
	if content.null() {
		return ""
	}
	if content.kind != 'a' {
		return partText(content)
	}
	var parts []string
	for _, p := range content.vals {
		if t := partText(p); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// One user authored message of an exported session in the shape the user message exports list
type prompt struct {
	session   string
	index     int
	createdAt string
	created   bool
	text      string
	messageID *jv
	eventID   *jv
}

// JSON record of the entry, members in export order
func (p prompt) object() *jv {
	created := &jv{kind: 'z'}
	if p.created {
		created = jvString(p.createdAt)
	}
	o := &jv{kind: 'o',
		keys: []string{"session_id", "index", "created_at", "role", "text"},
		vals: []*jv{jvString(p.session), jvInt(p.index), created, jvString("user"), jvString(p.text)}}
	if p.messageID != nil {
		o = o.with("message_id", p.messageID)
	}
	if p.eventID != nil {
		o = o.with("event_id", p.eventID)
	}
	return o
}

// Every user authored prompt of the sessions in order, numbered per session
func promptsOf(sessions []*jv) []prompt {
	var out []prompt
	for _, s := range sessions {
		sid := first(s.get("id"), s.get("session_id"), jvString("")).display()
		index := 0
		for _, m := range messagesOf(s) {
			if role := m.get("role"); role == nil || role.kind != 's' || role.str != "user" {
				continue
			}
			index++
			p := prompt{session: sid, index: index, text: messageText(m.get("content"))}
			p.createdAt, p.created = exportTime(m.get("timestamp"), sid)
			if id := m.get("id"); !id.null() {
				p.messageID = id
			}
			if ev := first(m.get("platform_message_id"), m.get("event_id")); ev.truthy() {
				p.eventID = ev
			}
			out = append(out, p)
		}
	}
	return out
}

// Canonical export format of a name, jsonl for an empty one, markdown for md
func ExportFormat(name string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		key = "jsonl"
	}
	switch key {
	case "jsonl":
		return "jsonl", nil
	case "markdown", "md":
		return "markdown", nil
	}
	return "", &FormatError{msg: "Unsupported session export format: " + name}
}

// Reports whether an export filter selects user prompts only, an empty filter selecting whole sessions
func ExportPromptsOnly(only string) (bool, error) {
	if only == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(only)) {
	case "user", "prompts", "user-prompts", "user_prompts":
		return true, nil
	}
	return false, &FormatError{msg: "Unsupported session export filter: " + only}
}

// Exported sessions as JSONL objects or Markdown, whole sessions or one record per user prompt
func RenderExport(sessions [][]byte, format, only string) (string, error) {
	docs, err := parseDocs(sessions)
	if err != nil {
		return "", err
	}
	exportFormat, err := ExportFormat(format)
	if err != nil {
		return "", err
	}
	promptsOnly, err := ExportPromptsOnly(only)
	if err != nil {
		return "", err
	}
	if exportFormat == "jsonl" {
		var lines []string
		if promptsOnly {
			for _, p := range promptsOf(docs) {
				lines = append(lines, p.object().text(jsonLayout{}))
			}
		} else {
			for _, d := range docs {
				lines = append(lines, d.text(jsonLayout{}))
			}
		}
		if len(lines) == 0 {
			return "", nil
		}
		return strings.Join(lines, "\n") + "\n", nil
	}
	return renderMarkdownExport(docs, promptsOnly), nil
}

// Markdown of the sessions, one session under its own top heading and several under a shared one
func renderMarkdownExport(docs []*jv, promptsOnly bool) string {
	multiTitle := "icebath sessions export"
	heading := func(s *jv, level int) string {
		title := ""
		if t := s.get("title"); t.truthy() {
			title = strings.TrimSpace(t.display())
		}
		if title == "" {
			title = sessionID(s)
		}
		return "Session: " + headingText(title)
	}
	body := appendSessionMessages
	if promptsOnly {
		multiTitle = "User prompts export"
		heading = func(s *jv, level int) string {
			if level == 1 {
				return "User prompts for session " + headingText(sessionID(s))
			}
			return "Session " + headingText(sessionID(s))
		}
		body = appendPromptRecords
	}
	var lines []string
	single := len(docs) == 1
	if !single {
		lines = append(lines, "# "+multiTitle, "")
	}
	for _, s := range docs {
		level := 2
		if single {
			level = 1
		}
		lines = append(lines, strings.Repeat("#", level)+" "+heading(s, level))
		lines = append(lines, metadataLines(s)...)
		lines = append(lines, "")
		lines = body(lines, s, level+1)
	}
	if promptsOnly && len(docs) == 0 {
		lines = append(lines, "_No user prompts found._", "")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n") + "\n"
}

// Bullet lines of the id, source, model, title, start and message count a session heading carries
func metadataLines(s *jv) []string {
	sid := sessionID(s)
	lines := []string{"- Session ID: `" + sid + "`"}
	for _, f := range [...]struct{ key, label string }{{"source", "Source"}, {"model", "Model"}} {
		if v := s.get(f.key); v.truthy() {
			lines = append(lines, "- "+f.label+": `"+v.display()+"`")
		}
	}
	if title := s.get("title"); title.truthy() {
		lines = append(lines, "- Title: "+strings.TrimSpace(strings.Join(textLines(title.display()), " ")))
	}
	if started, ok := exportTime(s.get("started_at"), sid); ok && started != "" {
		lines = append(lines, "- Started: "+started)
	}
	if count := s.get("message_count"); !count.null() {
		lines = append(lines, "- Messages: "+count.display())
	}
	return lines
}

// Appends every message but the system prompt as a heading and its text, tool results folded in a details block
func appendSessionMessages(lines []string, s *jv, level int) []string {
	marker := strings.Repeat("#", level)
	var visible []*jv
	for _, m := range messagesOf(s) {
		if role := m.get("role"); role == nil || role.kind != 's' || role.str != "system" {
			visible = append(visible, m)
		}
	}
	if len(visible) == 0 {
		return append(lines, "_No messages found._", "")
	}
	for _, m := range visible {
		role := first(m.get("role"), jvString("unknown")).display()
		suffix := ""
		if ts, ok := exportTime(m.get("timestamp"), sessionID(s)); ok && ts != "" {
			suffix = " - " + ts
		}
		text := messageText(m.get("content"))
		if role == "tool" {
			name := first(m.get("tool_name"), m.get("name"), jvString("tool")).display()
			lines = append(lines, marker+" Tool: "+headingText(name)+suffix, "",
				"<details><summary>"+escapeMarkup(name)+"</summary>", "", fenced(text), "", "</details>", "")
			continue
		}
		label, ok := map[string]string{"user": "User", "assistant": "Assistant"}[role]
		if !ok {
			label = titled(role)
		}
		lines = append(lines, marker+" "+label+suffix, "", text, "")
	}
	return lines
}

// Appends every user prompt of the session as a numbered heading with its message id and text
func appendPromptRecords(lines []string, s *jv, level int) []string {
	prompts := promptsOf([]*jv{s})
	if len(prompts) == 0 {
		return append(lines, "_No user prompts found._", "")
	}
	marker := strings.Repeat("#", level)
	for _, p := range prompts {
		created := p.createdAt
		if created == "" {
			created = "timestamp unavailable"
		}
		lines = append(lines, marker+" "+strconv.Itoa(p.index)+". "+created)
		if p.messageID != nil {
			lines = append(lines, "Message ID: `"+p.messageID.display()+"`", "")
		}
		lines = append(lines, p.text, "")
	}
	return lines
}

// Count of what an export holds and its noun, prompts for a prompt export and sessions otherwise
func ExportCount(sessions [][]byte, only string) (int, string, error) {
	promptsOnly, err := ExportPromptsOnly(only)
	if err != nil {
		return 0, "", err
	}
	if !promptsOnly {
		return len(sessions), "session", nil
	}
	docs, err := parseDocs(sessions)
	if err != nil {
		return 0, "", err
	}
	return len(promptsOf(docs)), "prompt", nil
}

// Help text of the save command
const SaveUsage = `/save — export the current session to a file
Usage: /save <format> [filename] [redact]

Formats:
  json    full session snapshot (canonical export shape)
  md      readable Markdown transcript
  html    standalone single-file HTML page (shareable, no dependencies)

Options:
  filename   optional output name/path (default: auto-named;
             CLI saves under ~/.icebath/sessions/saved/)
  redact     scrub API keys, tokens, and credentials before writing

Examples:
  /save json
  /save html
  /save md notes.md
  /save html session.html redact`

// Canonical save format of a typed token, json for an empty one
func SaveFormat(name string) (string, error) {
	token := strings.ToLower(strings.TrimSpace(name))
	if token == "" {
		token = "json"
	}
	switch token {
	case "json", "snapshot":
		return "json", nil
	case "md", "markdown":
		return "md", nil
	case "html":
		return "html", nil
	}
	return "", &FormatError{msg: "Unknown format '" + token + "' — expected one of: json, md, html"}
}

// Reports whether a save format is a transcript of what the user saw rather than a restorable snapshot
func TranscriptFormat(format string) bool { return format == "md" || format == "html" }

// One exported session rendered in a save format, an indented JSON snapshot, a Markdown transcript or an HTML page
func RenderSave(session []byte, format string) (string, error) {
	switch format {
	case "json":
		doc, err := parseDoc(session)
		if err != nil {
			return "", err
		}
		return doc.text(jsonLayout{indent: 2}), nil
	case "md":
		return RenderExport([][]byte{session}, "markdown", "")
	case "html":
		return RenderHTML([][]byte{session})
	}
	return "", &FormatError{msg: "Unknown save format: '" + format + "'"}
}

// Default file name a save of the session takes, its id kept to letters, digits, dashes and underscores
func SaveFilename(sessionID, format string) string {
	var b strings.Builder
	for _, r := range sessionID {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	safe := b.String()
	if safe == "" {
		safe = "session"
	}
	return "icebath_session_" + safe + "." + format
}

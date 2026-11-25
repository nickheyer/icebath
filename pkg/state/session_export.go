package state

import (
	"context"
	"strings"
	"unicode"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

var (
	sessionExportFormats     = map[string]string{"jsonl": "jsonl", "markdown": "markdown", "md": "markdown"}
	sessionExportOnlyAliases = map[string]bool{"user": true, "prompts": true, "user-prompts": true, "user_prompts": true}
)

// Canonical export format name, jsonl for an empty one, a ValueError for an unknown one
func NormalizeExportFormat(format string) (string, error) {
	name := format
	if name == "" {
		name = "jsonl"
	}
	value, ok := sessionExportFormats[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return "", &errorutil.Error{Kind: "ValueError", Msg: "Unsupported session export format: " + format}
	}
	return value, nil
}

// Canonical export filter name, empty for None, a ValueError for an unknown filter
func NormalizeExportOnly(only any) (string, error) {
	if only == nil {
		return "", nil
	}
	text := anyutil.String(only)
	if sessionExportOnlyAliases[strings.ToLower(strings.TrimSpace(text))] {
		return "user-prompts", nil
	}
	return "", &errorutil.Error{Kind: "ValueError", Msg: "Unsupported session export filter: " + text}
}

// Renders exported sessions as jsonl or markdown, whole sessions or one record per user prompt
func RenderSessionsExport(sessions []*jsonutil.Object, format string, only any) (string, error) {
	exportFormat, err := NormalizeExportFormat(format)
	if err != nil {
		return "", err
	}
	filter, err := NormalizeExportOnly(only)
	if err != nil {
		return "", err
	}
	promptsOnly := filter == "user-prompts"
	if exportFormat == "jsonl" {
		rows := sessions
		if promptsOnly {
			rows = IterUserPromptRecords(sessions)
		}
		var lines []string
		for _, row := range rows {
			text, err := jsonutil.Encode(exportJSONValue(row), jsonutil.Spaced())
			if err != nil {
				return "", err
			}
			lines = append(lines, text)
		}
		if len(lines) == 0 {
			return "", nil
		}
		return strings.Join(lines, "\n") + "\n", nil
	}
	multiTitle, appendBody := "Icebath sessions export", appendExportSessionMessages
	sessionHeading := func(s *jsonutil.Object) string { return "Session: " + exportHeadingText(exportSessionTitleOrID(s)) }
	headings := [2]func(*jsonutil.Object) string{sessionHeading, sessionHeading}
	if promptsOnly {
		multiTitle, appendBody = "User prompts export", appendExportPromptRecords
		headings = [2]func(*jsonutil.Object) string{
			func(s *jsonutil.Object) string {
				return "User prompts for session " + exportHeadingText(exportSessionID(s))
			},
			func(s *jsonutil.Object) string { return "Session " + exportHeadingText(exportSessionID(s)) },
		}
	}
	var lines []string
	single := len(sessions) == 1
	if !single {
		lines = append(lines, "# "+multiTitle, "")
	}
	for _, session := range sessions {
		level := 2
		if single {
			level = 1
		}
		lines = append(lines, strings.Repeat("#", level)+" "+headings[level-1](session))
		lines = append(lines, exportSessionMetadataLines(session)...)
		lines = append(lines, "")
		var err error
		if lines, err = appendBody(lines, session, level+1); err != nil {
			return "", err
		}
	}
	if promptsOnly && len(sessions) == 0 {
		lines = append(lines, "_No user prompts found._", "")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// Count and noun of an export for its status message, prompts for a prompt view and sessions otherwise
func ExportRecordCount(sessions []*jsonutil.Object, only any) (int, string, error) {
	filter, err := NormalizeExportOnly(only)
	if err != nil {
		return 0, "", err
	}
	if filter == "user-prompts" {
		return len(IterUserPromptRecords(sessions)), "prompt", nil
	}
	return len(sessions), "session", nil
}

// One normalized record for each user authored prompt of the sessions
func IterUserPromptRecords(sessions []*jsonutil.Object) []*jsonutil.Object {
	records := []*jsonutil.Object{}
	for _, session := range sessions {
		sessionID := ""
		for _, key := range []string{"id", "session_id"} {
			if v := rowValue(session, key); anyutil.IsTrue(v) {
				sessionID = anyutil.String(v)
				break
			}
		}
		index := 0
		for _, message := range exportMessageRows(session) {
			if rowValue(message, "role") != "user" {
				continue
			}
			index++
			var createdAt any
			if stamp, ok := formatExportTimestamp(rowValue(message, "timestamp"), sessionID); ok {
				createdAt = stamp
			}
			text, _ := exportMessageText(rowValue(message, "content"))
			record := jsonutil.ObjectOf("session_id", sessionID, "index", index, "created_at", createdAt, "role", "user", "text", text)
			if messageID := rowValue(message, "id"); messageID != nil {
				record.Set("message_id", messageID)
			}
			eventID := rowValue(message, "platform_message_id")
			if !anyutil.IsTrue(eventID) {
				eventID = rowValue(message, "event_id")
			}
			if anyutil.IsTrue(eventID) {
				record.Set("event_id", eventID)
			}
			records = append(records, record)
		}
	}
	return records
}

// Appends the numbered user prompts of a session under headings of the given level
func appendExportPromptRecords(lines []string, session *jsonutil.Object, headingLevel int) ([]string, error) {
	prompts := IterUserPromptRecords([]*jsonutil.Object{session})
	if len(prompts) == 0 {
		return append(lines, "_No user prompts found._", ""), nil
	}
	marker := strings.Repeat("#", headingLevel)
	for _, prompt := range prompts {
		createdAt := "timestamp unavailable"
		if v := rowValue(prompt, "created_at"); anyutil.IsTrue(v) {
			createdAt = anyutil.String(v)
		}
		lines = append(lines, marker+" "+anyutil.String(rowValue(prompt, "index"))+". "+createdAt)
		if messageID := rowValue(prompt, "message_id"); messageID != nil {
			lines = append(lines, "Message ID: `"+anyutil.String(messageID)+"`", "")
		}
		lines = append(lines, exportTextOr(prompt, "text", ""), "")
	}
	return lines, nil
}

// Python html.escape with quotes escaped
func exportHTMLEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

// Appends every non system message of a session under headings of the given level, tool output folded and shell lines among them by time
func appendExportSessionMessages(lines []string, session *jsonutil.Object, headingLevel int) ([]string, error) {
	marker := strings.Repeat("#", headingLevel)
	var visible []*jsonutil.Object
	for _, message := range exportDisplayRows(session) {
		if rowValue(message, "role") != "system" {
			visible = append(visible, message)
		}
	}
	if len(visible) == 0 {
		return append(lines, "_No messages found._", ""), nil
	}
	for _, message := range visible {
		role := exportTextOr(message, "role", "unknown")
		suffix := ""
		if stamp, ok := formatExportTimestamp(rowValue(message, "timestamp"), exportSessionID(session)); ok && stamp != "" {
			suffix = " - " + stamp
		}
		text, err := exportMessageText(rowValue(message, "content"))
		if err != nil {
			return nil, err
		}
		if role == shellLineRole {
			lines = append(lines, marker+" Shell"+suffix, "", exportFencedText(text, "text"), "")
			continue
		}
		if role == "tool" {
			toolName := exportTextOr(message, "tool_name", "")
			if toolName == "" {
				toolName = exportTextOr(message, "name", "tool")
			}
			lines = append(lines, marker+" Tool: "+exportHeadingText(toolName)+suffix, "",
				"<details><summary>"+exportHTMLEscape(toolName)+"</summary>", "",
				exportFencedText(text, "text"), "", "</details>", "")
			continue
		}
		label, ok := map[string]string{"user": "User", "assistant": "Assistant"}[role]
		if !ok {
			label = textutil.Title(role)
		}
		lines = append(lines, marker+" "+label+suffix, "", text, "")
	}
	return lines, nil
}

// Text of a message content, the parts of a list joined by newlines and None as empty
func exportMessageText(content any) (string, error) {
	var list []any
	switch v := content.(type) {
	case nil:
		return "", nil
	case []any:
		list = v
	case []*jsonutil.Object:
		for _, item := range v {
			list = append(list, item)
		}
	default:
		return exportContentPartText(content)
	}
	var parts []string
	for _, item := range list {
		part, err := exportContentPartText(item)
		if err != nil {
			return "", err
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// Text of one content part, its text or content string or else its sorted JSON
func exportContentPartText(part any) (string, error) {
	var obj *jsonutil.Object
	switch v := exportJSONValue(part).(type) {
	case *jsonutil.Object:
		obj = v
	case map[string]any:
		obj = jsonutil.NewObject()
		for k, value := range v {
			obj.Set(k, value)
		}
	case string:
		return v, nil
	default:
		return anyutil.String(part), nil
	}
	for _, key := range []string{"text", "content"} {
		if value, ok := rowValue(obj, key).(string); ok {
			return value, nil
		}
	}
	return jsonutil.Encode(obj, jsonutil.SortKeys(), jsonutil.Spaced())
}

// UTC ISO second of a stored timestamp, the raw text of a corrupt cell, false for None
func formatExportTimestamp(value any, sessionID string) (string, bool) {
	if value == nil {
		return "", false
	}
	ts, ok := timeutil.StoredEpoch(value, sessionID, "timestamp")
	if !ok {
		return anyutil.String(value), true
	}
	return timeutil.FromEpoch(ts).UTC().Format("2006-01-02T15:04:05") + "Z", true
}

// Bullet lines naming the id, source, model, title, start and message count of a session
func exportSessionMetadataLines(session *jsonutil.Object) []string {
	lines := []string{"- Session ID: `" + exportSessionID(session) + "`"}
	for _, field := range [][2]string{{"source", "Source"}, {"model", "Model"}} {
		if v := rowValue(session, field[0]); anyutil.IsTrue(v) {
			lines = append(lines, "- "+field[1]+": `"+anyutil.String(v)+"`")
		}
	}
	if title := rowValue(session, "title"); anyutil.IsTrue(title) {
		lines = append(lines, "- Title: "+strings.TrimSpace(strings.Join(textutil.Lines(anyutil.String(title)), " ")))
	}
	if started, ok := formatExportTimestamp(rowValue(session, "started_at"), exportSessionID(session)); ok && started != "" {
		lines = append(lines, "- Started: "+started)
	}
	if count := rowValue(session, "message_count"); count != nil {
		lines = append(lines, "- Messages: "+anyutil.String(count))
	}
	return lines
}

// Id of an exported session, unknown when it carries none
func exportSessionID(session *jsonutil.Object) string {
	for _, key := range []string{"id", "session_id"} {
		if v := rowValue(session, key); anyutil.IsTrue(v) {
			return anyutil.String(v)
		}
	}
	return "unknown"
}

// Stripped title of an exported session, its id when untitled
func exportSessionTitleOrID(session *jsonutil.Object) string {
	if title := rowValue(session, "title"); anyutil.IsTrue(title) {
		if text := strings.TrimSpace(anyutil.String(title)); text != "" {
			return text
		}
	}
	return exportSessionID(session)
}

// One line heading text, unknown when nothing is left
func exportHeadingText(value string) string {
	if text := strings.TrimSpace(strings.Join(textutil.Lines(value), " ")); text != "" {
		return text
	}
	return "unknown"
}

// Text inside a code fence longer than any backtick run the text contains
func exportFencedText(text, language string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + language + "\n" + text + "\n" + fence
}

var SaveFormats = []string{"json", "md", "html"}

// Whether a save format is a transcript of what the user saw, markdown or html
func IsSaveTranscriptFormat(format string) bool { return format == "md" || format == "html" }

// Export flags of a transcript, compaction archived turns shown, or of a snapshot carrying every stored row
func ExportProjection(transcript bool) (includeCompacted, includeInactive bool) {
	return transcript, !transcript
}

// Drops the rows removed by undo, rewind or edit from a snapshot and rebuilds its timings from the rest
func DropUndoneRows(export *jsonutil.Object) *jsonutil.Object {
	kept := []*jsonutil.Object{}
	for _, m := range exportMessageRows(export) {
		if anyutil.IsTrue(rowValue(m, "active")) || anyutil.IsTrue(rowValue(m, "compacted")) {
			kept = append(kept, m)
		}
	}
	rows := make([]any, 0, len(kept))
	for _, m := range kept {
		rows = append(rows, m)
	}
	export.Set("messages", rows)
	sessionID := ""
	if id := rowValue(export, "id"); id != nil {
		sessionID = anyutil.String(id)
	}
	export.Set("timings", exportTimings(kept, sessionID))
	return export
}

// Stored session a save of the format writes, nil when it has no row, refused past the export message cap
func LoadSaveSnapshot(ctx context.Context, db *SessionDB, sessionID, format string) (*jsonutil.Object, error) {
	transcript := IsSaveTranscriptFormat(format)
	if !transcript {
		if _, err := db.AssertExportSafe(ctx, sessionID, nil); err != nil {
			return nil, err
		}
	}
	includeCompacted, includeInactive := ExportProjection(transcript)
	export, err := db.ExportSession(ctx, sessionID, includeCompacted, includeInactive)
	if err != nil {
		return nil, err
	}
	if export != nil && export.Len() > 0 && !transcript {
		DropUndoneRows(export)
	}
	return export, nil
}

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

var sessionSaveFormatAliases = map[string]string{"json": "json", "snapshot": "json", "md": "md", "markdown": "md", "html": "html"}

// Canonical save format of a typed token, json for an empty one, a ValueError for an unknown one
func NormalizeSaveFormat(format string) (string, error) {
	token := format
	if token == "" {
		token = "json"
	}
	token = strings.ToLower(strings.TrimSpace(token))
	canonical, ok := sessionSaveFormatAliases[token]
	if !ok {
		return "", &errorutil.Error{Kind: "ValueError", Msg: "Unknown format " + anyutil.Quote(token) + " — expected one of: json, md, html"}
	}
	return canonical, nil
}

// Standalone HTML page of one exported session
func renderHTMLForSave(session *jsonutil.Object) (string, error) { return GenerateHTMLExport(session) }

// Renders one exported session for a save in the given canonical format
func RenderSessionForSave(session *jsonutil.Object, format string) (string, error) {
	switch format {
	case "json":
		return jsonutil.Encode(exportJSONValue(session), jsonutil.Indent(2), jsonutil.Convert(func(v any) (any, error) { return anyutil.String(v), nil }))
	case "md":
		return RenderSessionsExport([]*jsonutil.Object{session}, "markdown", nil)
	case "html":
		return renderHTMLForSave(session)
	}
	return "", &errorutil.Error{Kind: "ValueError", Msg: "Unknown save format: " + anyutil.Quote(format)}
}

// Default file name of a saved session, the id reduced to letters, digits, dashes and underscores
func DefaultSaveFilename(sessionID, format string) string {
	var safe strings.Builder
	for _, ch := range sessionID {
		if (unicode.IsLetter(ch) || unicode.IsNumber(ch)) || ch == '-' || ch == '_' {
			safe.WriteRune(ch)
		}
	}
	if safe.Len() == 0 {
		return "icebath_session_session." + format
	}
	return "icebath_session_" + safe.String() + "." + format
}

// Messages of an exported session as a list of rows, entries that are not rows dropped
func exportMessageRows(session *jsonutil.Object) []*jsonutil.Object {
	switch list := rowValue(session, "messages").(type) {
	case []*jsonutil.Object:
		return list
	case []any:
		out := make([]*jsonutil.Object, 0, len(list))
		for _, item := range list {
			if row, ok := item.(*jsonutil.Object); ok {
				out = append(out, row)
			}
		}
		return out
	}
	return nil
}

// Text of a row value, the fallback when it is missing, None or empty
func exportTextOr(row *jsonutil.Object, key, fallback string) string {
	if v := rowValue(row, key); anyutil.IsTrue(v) {
		return anyutil.String(v)
	}
	return fallback
}

// Decoded store JSON with every ordered object turned into a pyjson Object and every list copied
func exportJSONValue(v any) any {
	switch x := v.(type) {
	case orderedObject:
		obj := jsonutil.NewObject()
		for _, kv := range x {
			obj.Set(kv.Key, exportJSONValue(kv.Value))
		}
		return obj
	case *jsonutil.Object:
		if x == nil {
			return nil
		}
		obj := jsonutil.NewObject()
		for _, k := range x.Keys() {
			obj.Set(k, exportJSONValue(x.Value(k)))
		}
		return obj
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			out[k] = exportJSONValue(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = exportJSONValue(item)
		}
		return out
	case []*jsonutil.Object:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = exportJSONValue(item)
		}
		return out
	}
	return v
}

// One exported session as a JSON line, non ASCII text kept as written
func DumpsExportedSession(session *jsonutil.Object) (string, error) {
	return jsonutil.Encode(exportJSONValue(session), jsonutil.Spaced())
}

// Decoded store JSON as pyjson values for callers outside this package
func ExportJSONValue(v any) any { return exportJSONValue(v) }

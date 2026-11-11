package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	ExporterVersion          = "icebath sessions export (md/qmd) v1"
	exportSHAMarker          = "__SHA256_PLACEHOLDER__"
	exportVerificationHeader = "## Export verification"
)

var (
	exportSHALineRE = regexp.MustCompile("- SHA256 of exported body: `([0-9a-f]{64})`")
	exportSlugRE    = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// ExportExistsError is the FileExistsError of an export file that is already there, its text the path
type ExportExistsError struct{ Path string }

func (e *ExportExistsError) Error() string { return e.Path }

// Lets the error match fs.ErrExist
func (e *ExportExistsError) Is(target error) bool { return target == fs.ErrExist }

// Value of a session or message key, nil when the row or the key is missing
func exportGet(row *pyjson.Object, key string) any {
	if row == nil {
		return nil
	}
	v, _ := row.Get(key)
	return v
}

// First truthy value among the keys of a row, nil when none is
func exportFirst(row *pyjson.Object, keys ...string) any {
	var last any
	for _, key := range keys {
		last = exportGet(row, key)
		if pyrepr.Truthy(last) {
			return last
		}
	}
	return last
}

// UTC ISO text of a stored timestamp, empty when unset, the raw text of a corrupt cell
func isoTimestamp(value any) string {
	if value == nil || value == "" {
		return ""
	}
	ts, ok := pydatetime.CoerceEpoch(value, "", "timestamp")
	if !ok {
		return pyrepr.Str(value)
	}
	t := pydatetime.FromTimestamp(ts).UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "Z"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "Z"
}

// One frontmatter line, null and booleans bare, numbers and lists as JSON, anything else as a JSON string
func frontmatterLine(key string, value any) (string, error) {
	var shown string
	switch v := value.(type) {
	case nil:
		shown = "null"
	case bool:
		shown = "false"
		if v {
			shown = "true"
		}
	default:
		dumped := state.ExportJSONValue(value)
		switch dumped.(type) {
		case int, int64, float64, []any, []string:
		default:
			dumped = pyrepr.Str(value)
		}
		text, err := pyjson.Dumps(dumped, pyjson.EnsureASCII(false))
		if err != nil {
			return "", err
		}
		shown = text
	}
	return key + ": " + shown, nil
}

// Fenced JSON block of a value indented by two spaces
func jsonBlock(value any) (string, error) {
	text, err := pyjson.Dumps(state.ExportJSONValue(value), pyjson.EnsureASCII(false), pyjson.Indent(2))
	if err != nil {
		return "", err
	}
	return "```json\n" + text + "\n```", nil
}

// Level three heading of a message, its role or tool name followed by its timestamp
func messageHeading(message *pyjson.Object) string {
	role := "message"
	if v := exportGet(message, "role"); pyrepr.Truthy(v) {
		role = pyrepr.Str(v)
	}
	label := pystr.Capitalize(role)
	if name := exportFirst(message, "name", "tool_name"); role == "tool" && pyrepr.Truthy(name) {
		label = "Tool — " + pyrepr.Str(name)
	}
	if timestamp := isoTimestamp(exportFirst(message, "created_at", "timestamp")); timestamp != "" {
		return "### " + label + " — " + timestamp
	}
	return "### " + label
}

// Id of an exported session, unknown-session when it carries none
func exportedSessionID(session *pyjson.Object) string {
	if v := exportFirst(session, "id", "session_id"); pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return "unknown-session"
}

// Rows of a list value, entries that are not objects dropped
func exportRows(value any) []*pyjson.Object {
	switch list := value.(type) {
	case []*pyjson.Object:
		return list
	case []any:
		out := make([]*pyjson.Object, 0, len(list))
		for _, item := range list {
			if row, ok := state.ExportJSONValue(item).(*pyjson.Object); ok {
				out = append(out, row)
			}
		}
		return out
	}
	return nil
}

// Compression segments of a lineage export, the session itself when it has none
func exportSegments(session *pyjson.Object) []*pyjson.Object {
	raw := exportGet(session, "segments")
	_, isList := raw.([]any)
	_, isRows := raw.([]*pyjson.Object)
	if (isList || isRows) && pyrepr.Truthy(raw) {
		return exportRows(raw)
	}
	return []*pyjson.Object{session}
}

// Number of entries of a messages list
func exportListLen(value any) int {
	switch list := value.(type) {
	case []*pyjson.Object:
		return len(list)
	case []any:
		return len(list)
	}
	return 0
}

// Messages over every segment of an exported session
func exportedMessageCount(session *pyjson.Object) int {
	n := 0
	for _, segment := range exportSegments(session) {
		n += exportListLen(exportGet(segment, "messages"))
	}
	return n
}

// Messages section of an export, each message under its heading with its tool calls as JSON
func renderMessages(session *pyjson.Object) (string, error) {
	segments := exportSegments(session)
	if exportedMessageCount(session) == 0 {
		return "## Messages\n\n_No messages in this session._\n", nil
	}
	parts := []string{"## Messages\n"}
	for _, segment := range segments {
		if len(segments) > 1 {
			parts = append(parts, "## Compression segment: "+exportedSessionID(segment)+"\n")
		}
		for _, message := range exportRows(exportGet(segment, "messages")) {
			parts = append(parts, messageHeading(message)+"\n")
			rendered := ""
			switch content := exportGet(message, "content").(type) {
			case nil:
			case string:
				rendered = pystr.Rstrip(content)
			default:
				block, err := jsonBlock(content)
				if err != nil {
					return "", err
				}
				rendered = block
			}
			if rendered != "" {
				parts = append(parts, rendered+"\n")
			}
			if toolCalls := exportGet(message, "tool_calls"); pyrepr.Truthy(toolCalls) {
				block, err := jsonBlock(toolCalls)
				if err != nil {
					return "", err
				}
				parts = append(parts, "\n\n## Tool calls\n\n"+block+"\n")
			}
			parts = append(parts, "")
		}
	}
	return pystr.Rstrip(strings.Join(parts, "\n")) + "\n", nil
}

// Export text with the frontmatter, the messages and a verification section whose digest is a marker
func exportBodyWithoutHash(session *pyjson.Object, format string, exportedAt float64) (string, error) {
	sessionID := exportedSessionID(session)
	exportedISO := isoTimestamp(exportedAt)
	messageCount := exportedMessageCount(session)
	toolCallCount := exportGet(session, "tool_call_count")
	if !pyrepr.Truthy(toolCallCount) {
		toolCallCount = 0
	}
	type field struct {
		key   string
		value any
	}
	fields := []field{
		{"session_id", sessionID}, {"title", exportGet(session, "title")}, {"source", exportGet(session, "source")},
		{"created_at", isoTimestamp(exportFirst(session, "started_at", "created_at"))},
		{"updated_at", isoTimestamp(exportFirst(session, "last_active", "updated_at"))},
		{"ended_at", isoTimestamp(exportGet(session, "ended_at"))}, {"model", exportGet(session, "model")},
		{"provider", exportFirst(session, "billing_provider", "provider")}, {"cwd", exportGet(session, "cwd")},
		{"archived", pyrepr.Truthy(exportGet(session, "archived"))}, {"message_count", messageCount}, {"tool_call_count", toolCallCount},
	}
	if lineage := exportGet(session, "lineage_session_ids"); pyrepr.Truthy(lineage) {
		fields = append(fields, field{"lineage_session_ids", lineage})
	}
	fields = append(fields, field{"format", format}, field{"exported_at", exportedISO}, field{"exporter", ExporterVersion})
	front := []string{"---"}
	for _, f := range fields {
		line, err := frontmatterLine(f.key, f.value)
		if err != nil {
			return "", err
		}
		front = append(front, line)
	}
	front = append(front, "---", "")
	title := sessionID
	if v := exportGet(session, "title"); pyrepr.Truthy(v) {
		title = pyrepr.Str(v)
	}
	parts := []string{strings.Join(front, "\n"), "# " + title + "\n", "Session ID: `" + sessionID + "`\n"}
	if source := exportGet(session, "source"); pyrepr.Truthy(source) {
		parts = append(parts, "Source: `"+pyrepr.Str(source)+"`\n")
	}
	if cwd := exportGet(session, "cwd"); pyrepr.Truthy(cwd) {
		parts = append(parts, "Working directory: `"+pyrepr.Str(cwd)+"`\n")
	}
	messages, err := renderMessages(session)
	if err != nil {
		return "", err
	}
	sourceCount := pyrepr.Str(messageCount)
	if v, ok := session.Get("message_count"); ok {
		sourceCount = pyrepr.Str(v)
	}
	parts = append(parts, messages, exportVerificationHeader+"\n", "- Session id: `"+sessionID+"`",
		"- Exported messages: `"+pyrepr.Str(messageCount)+"`", "- Source DB message count at export: `"+sourceCount+"`",
		"- Exported at: `"+exportedISO+"`", "- SHA256 of exported body: `"+exportSHAMarker+"`")
	return pystr.Rstrip(strings.Join(parts, "\n")) + "\n", nil
}

// ValueError for an export format other than md or qmd
func checkExportFormat(format string) error {
	if format != "md" && format != "qmd" {
		return &pyint.Error{Kind: "ValueError", Msg: "fmt must be 'md' or 'qmd'"}
	}
	return nil
}

// Hex SHA256 of a text
func textSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Renders an exported session as Markdown or QMD text, with a body digest unless verification is left out
func RenderSessionMarkdown(session *pyjson.Object, format string, includeVerification bool) (string, error) {
	if err := checkExportFormat(format); err != nil {
		return "", err
	}
	body, err := exportBodyWithoutHash(session, format, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return "", err
	}
	if !includeVerification {
		head, _, _ := strings.Cut(body, "\n"+exportVerificationHeader+"\n")
		return pystr.Rstrip(head) + "\n", nil
	}
	digestBody := strings.ReplaceAll(body, "`"+exportSHAMarker+"`", "`pending`")
	return strings.ReplaceAll(body, exportSHAMarker, textSHA256(digestBody)), nil
}

// Deterministic path safe file name of a session export, the id followed by a slug of the title
func SafeSessionFilename(session *pyjson.Object, format string) (string, error) {
	if err := checkExportFormat(format); err != nil {
		return "", err
	}
	title := "session"
	if v := exportGet(session, "title"); pyrepr.Truthy(v) {
		title = pyrepr.Str(v)
	}
	slug := strings.ToLower(strings.Trim(exportSlugRE.ReplaceAllString(title, "-"), ".-_"))
	if slug == "" {
		slug = "session"
	}
	return exportedSessionID(session) + "-" + pystr.Slice(slug, 0, min(60, utf8.RuneCountInString(slug))) + "." + format, nil
}

// Hex SHA256 of the bytes of a file
func FileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Whether an export file still matches its digest and names the session and message count, with the reason
func VerifyExportFile(path string, session *pyjson.Object) (bool, string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, "file missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if !utf8.Valid(data) {
		return false, "", &pyint.Error{Kind: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode the export file " + path}
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	match := exportSHALineRE.FindStringSubmatch(text)
	if match == nil {
		return false, "sha256 marker missing", nil
	}
	digestBody := exportSHALineRE.ReplaceAllLiteralString(text, "- SHA256 of exported body: `pending`")
	if textSHA256(digestBody) != match[1] {
		return false, "sha256 mismatch", nil
	}
	if !strings.Contains(text, "- Exported messages: `"+pyrepr.Str(exportedMessageCount(session))+"`") {
		return false, "message count mismatch", nil
	}
	if !strings.Contains(text, "- Session id: `"+exportedSessionID(session)+"`") {
		return false, "session id mismatch", nil
	}
	return true, "ok", nil
}

// Copy of a value with every string run through the forced secret redaction
func redactExportValue(value any) any {
	switch v := state.ExportJSONValue(value).(type) {
	case string:
		return RedactSensitiveText(v, RedactOptions{})
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactExportValue(item)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range v.Keys {
			out.Set(k, redactExportValue(v.Vals[k]))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = redactExportValue(item)
		}
		return out
	default:
		return v
	}
}

// Copy of a session export with secrets redacted from its messages and segments
func RedactSessionData(session *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range session.Keys {
		out.Set(k, session.Vals[k])
	}
	for _, k := range []string{"messages", "segments"} {
		if v := exportGet(session, k); v != nil {
			out.Set(k, redactExportValue(v))
		}
	}
	return out
}

// Export directory with the user home expanded, created when missing
func exportDirectory(outputDir string) (string, error) {
	outDir, err := pypath.PathExpanduser(pypath.PathStr(outputDir))
	if err != nil {
		return "", err
	}
	return outDir, os.MkdirAll(outDir, 0o777)
}

// Writes the Markdown or QMD export of a session into the directory and returns its path
func WriteSessionMarkdown(session *pyjson.Object, outputDir, format string, force bool) (string, error) {
	outDir, err := exportDirectory(outputDir)
	if err != nil {
		return "", err
	}
	name, err := SafeSessionFilename(session, format)
	if err != nil {
		return "", err
	}
	path := filepath.Join(outDir, name)
	if _, statErr := os.Stat(path); statErr == nil && !force {
		return "", &ExportExistsError{Path: path}
	}
	text, err := RenderSessionMarkdown(session, format, true)
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(text), 0o666)
}

// Appends the record of one written export to manifest.jsonl in the directory and returns the manifest path
func AppendManifestEntry(outputDir string, session *pyjson.Object, path, format string) (string, error) {
	sessionID := exportedSessionID(session)
	lineage := exportGet(session, "lineage_session_ids")
	if !pyrepr.Truthy(lineage) {
		lineage = []any{sessionID}
	}
	digest, err := FileSHA256(path)
	if err != nil {
		return "", err
	}
	entry := map[string]any{"session_id": sessionID, "lineage_session_ids": state.ExportJSONValue(lineage), "path": pypath.PathStr(path),
		"format": format, "message_count": exportedMessageCount(session), "sha256": digest, "exported_at": float64(time.Now().UnixNano()) / 1e9}
	line, err := pyjson.Dumps(entry, pyjson.EnsureASCII(false), pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	outDir, err := exportDirectory(outputDir)
	if err != nil {
		return "", err
	}
	manifest := filepath.Join(outDir, "manifest.jsonl")
	fh, err := os.OpenFile(manifest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return "", err
	}
	if _, err := fh.WriteString(line + "\n"); err != nil {
		fh.Close()
		return "", err
	}
	return manifest, fh.Close()
}

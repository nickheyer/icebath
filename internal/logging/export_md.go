package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Exporter name a Markdown export records, the verification heading and the digest marker it closes with
const (
	markdownExporter    = "icebath sessions export (md/qmd) v1"
	verificationHeading = "## Export verification"
	shaPlaceholder      = "__SHA256_PLACEHOLDER__"
)

// Digest line of a Markdown export and the runs of characters a file name slug replaces
var (
	shaLine = regexp.MustCompile("- SHA256 of exported body: `([0-9a-f]{64})`")
	nonSlug = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// Id a Markdown export shows a session under, unknown-session when it has none
func markdownID(s *jv) string {
	return first(s.get("id"), s.get("session_id"), jvString("unknown-session")).display()
}

// Compression segments of a session, the session itself when it has none
func segmentsOf(s *jv) []*jv {
	if seg := s.get("segments"); seg != nil && seg.kind == 'a' && len(seg.vals) > 0 {
		return seg.objects()
	}
	return []*jv{s}
}

// Messages across every segment of a session
func markdownCount(s *jv) int {
	n := 0
	for _, seg := range segmentsOf(s) {
		if m := seg.get("messages"); m != nil && m.kind == 'a' {
			n += len(m.vals)
		}
	}
	return n
}

// Front matter line of one field, null, booleans, numbers and lists as JSON and anything else as a JSON string
func frontMatterLine(key string, v *jv) string {
	switch {
	case v.null():
		return key + ": null"
	case v.kind == 'b':
		return key + ": " + strconv.FormatBool(v.b)
	case v.kind == 'n', v.kind == 'a', v.kind == 's':
		return key + ": " + v.text(jsonLayout{})
	}
	return key + ": " + jvString(v.display()).text(jsonLayout{})
}

// Indented JSON block of a value in a json fence
func jsonBlock(v *jv) string {
	return "```json\n" + v.text(jsonLayout{indent: 2}) + "\n```"
}

// Heading of one message, the tool name for a named tool result, with its UTC time when one is stored
func markdownHeading(m *jv) string {
	role := first(m.get("role"), jvString("message")).display()
	name := first(m.get("name"), m.get("tool_name"))
	label := capitalized(role)
	if role == "tool" && name.truthy() {
		label = "Tool — " + name.display()
	}
	if ts := isoMicro(first(m.get("created_at"), m.get("timestamp"))); ts != "" {
		return "### " + label + " — " + ts
	}
	return "### " + label
}

// Messages section of a session, each compression segment under its own heading when there are several
func markdownMessages(s *jv) string {
	segs := segmentsOf(s)
	if markdownCount(s) == 0 {
		return "## Messages\n\n_No messages in this session._\n"
	}
	parts := []string{"## Messages\n"}
	for _, seg := range segs {
		if len(segs) > 1 {
			parts = append(parts, "## Compression segment: "+markdownID(seg)+"\n")
		}
		for _, m := range messagesOf(seg) {
			parts = append(parts, markdownHeading(m)+"\n")
			rendered := ""
			switch content := m.get("content"); {
			case content.null():
			case content.kind == 's':
				rendered = strings.TrimRightFunc(content.str, unicode.IsSpace)
			default:
				rendered = jsonBlock(content)
			}
			if rendered != "" {
				parts = append(parts, rendered+"\n")
			}
			if calls := m.get("tool_calls"); calls.truthy() {
				parts = append(parts, "\n\n## Tool calls\n\n"+jsonBlock(calls)+"\n")
			}
			parts = append(parts, "")
		}
	}
	return strings.TrimRightFunc(strings.Join(parts, "\n"), unicode.IsSpace) + "\n"
}

// Export body with front matter, messages and the verification block, its digest line still unfilled
func markdownBody(s *jv, format string, exportedAt float64) string {
	sid := markdownID(s)
	exported := isoSeconds(exportedAt)
	count := markdownCount(s)
	toolCalls := first(s.get("tool_call_count"), jvInt(0))
	fields := []struct {
		key string
		val *jv
	}{
		{"session_id", jvString(sid)},
		{"title", s.get("title")},
		{"source", s.get("source")},
		{"created_at", jvString(isoMicro(first(s.get("started_at"), s.get("created_at"))))},
		{"updated_at", jvString(isoMicro(first(s.get("last_active"), s.get("updated_at"))))},
		{"ended_at", jvString(isoMicro(s.get("ended_at")))},
		{"model", s.get("model")},
		{"provider", first(s.get("billing_provider"), s.get("provider"))},
		{"cwd", s.get("cwd")},
		{"archived", &jv{kind: 'b', b: s.get("archived").truthy()}},
		{"message_count", jvInt(count)},
		{"tool_call_count", toolCalls},
	}
	if lineage := s.get("lineage_session_ids"); lineage.truthy() {
		fields = append(fields, struct {
			key string
			val *jv
		}{"lineage_session_ids", lineage})
	}
	fm := []string{"---"}
	for _, f := range fields {
		fm = append(fm, frontMatterLine(f.key, f.val))
	}
	fm = append(fm, frontMatterLine("format", jvString(format)), frontMatterLine("exported_at", jvString(exported)),
		frontMatterLine("exporter", jvString(markdownExporter)), "---", "")
	parts := []string{strings.Join(fm, "\n"), "# " + first(s.get("title"), jvString(sid)).display() + "\n", "Session ID: `" + sid + "`\n"}
	if src := s.get("source"); src.truthy() {
		parts = append(parts, "Source: `"+src.display()+"`\n")
	}
	if cwd := s.get("cwd"); cwd.truthy() {
		parts = append(parts, "Working directory: `"+cwd.display()+"`\n")
	}
	stored := strconv.Itoa(count)
	if mc := s.get("message_count"); mc != nil {
		stored = mc.display()
	}
	parts = append(parts, markdownMessages(s), verificationHeading+"\n",
		"- Session id: `"+sid+"`",
		"- Exported messages: `"+strconv.Itoa(count)+"`",
		"- Source DB message count at export: `"+stored+"`",
		"- Exported at: `"+exported+"`",
		"- SHA256 of exported body: `"+shaPlaceholder+"`")
	return strings.TrimRightFunc(strings.Join(parts, "\n"), unicode.IsSpace) + "\n"
}

// Rejects a Markdown flavour other than md and qmd
func checkMarkdownFormat(format string) error {
	if format != "md" && format != "qmd" {
		return &FormatError{msg: "fmt must be 'md' or 'qmd'"}
	}
	return nil
}

// One exported session as Markdown or Quarto Markdown, closing with a digest of itself unless verification is off
func RenderMarkdown(session []byte, format string, verification bool) (string, error) {
	if err := checkMarkdownFormat(format); err != nil {
		return "", err
	}
	s, err := parseDoc(session)
	if err != nil {
		return "", err
	}
	body := markdownBody(s, format, float64(time.Now().UnixNano())/1e9)
	if !verification {
		head, _, _ := strings.Cut(body, "\n"+verificationHeading+"\n")
		return strings.TrimRightFunc(head, unicode.IsSpace) + "\n", nil
	}
	digest := sha256.Sum256([]byte(strings.ReplaceAll(body, "`"+shaPlaceholder+"`", "`pending`")))
	return strings.ReplaceAll(body, shaPlaceholder, hex.EncodeToString(digest[:])), nil
}

// Path safe file name of a Markdown export, the session id and a slug of its title
func MarkdownFilename(session []byte, format string) (string, error) {
	if err := checkMarkdownFormat(format); err != nil {
		return "", err
	}
	s, err := parseDoc(session)
	if err != nil {
		return "", err
	}
	return markdownFilename(s, format), nil
}

// File name of a Markdown export of a decoded session
func markdownFilename(s *jv, format string) string {
	title := first(s.get("title"), jvString("session")).display()
	slug := strings.ToLower(strings.Trim(nonSlug.ReplaceAllString(title, "-"), ".-_"))
	if slug == "" {
		slug = "session"
	}
	id := strings.Map(func(r rune) rune {
		if strings.ContainsRune(nameSeparators, r) {
			return '-'
		}
		return r
	}, markdownID(s))
	return id + "-" + slug[:min(len(slug), 60)] + "." + format
}

// Writes a Markdown export into dir and returns its path, an existing file kept unless force is set
func WriteMarkdown(ctx context.Context, session []byte, dir, format string, force bool) (string, error) {
	out := home.ExpandUser(dir)
	if err := os.MkdirAll(out, 0o777); err != nil {
		return "", err
	}
	name, err := MarkdownFilename(session, format)
	if err != nil {
		return "", err
	}
	path := filepath.Join(out, name)
	if pathExists(path) && !force {
		return "", &fs.PathError{Op: "export", Path: path, Err: fs.ErrExist}
	}
	text, err := RenderMarkdown(session, format, true)
	if err != nil {
		return "", err
	}
	if err := fileutil.WriteText(ctx, path, text, fileutil.KeepPerm()); err != nil {
		return "", err
	}
	return path, nil
}

// Checks a Markdown export file against its digest, message count and session id, with the reason it fails
func VerifyMarkdown(path string, session []byte) (bool, string, error) {
	s, err := parseDoc(session)
	if err != nil {
		return false, "", err
	}
	if !pathExists(path) {
		return false, "file missing", nil
	}
	text, err := fileutil.ReadText(path)
	if err != nil {
		return false, "", err
	}
	m := shaLine.FindStringSubmatch(text)
	if m == nil {
		return false, "sha256 marker missing", nil
	}
	digest := sha256.Sum256([]byte(shaLine.ReplaceAllString(text, "- SHA256 of exported body: `pending`")))
	switch {
	case hex.EncodeToString(digest[:]) != m[1]:
		return false, "sha256 mismatch", nil
	case !strings.Contains(text, "- Exported messages: `"+strconv.Itoa(markdownCount(s))+"`"):
		return false, "message count mismatch", nil
	case !strings.Contains(text, "- Session id: `"+markdownID(s)+"`"):
		return false, "session id mismatch", nil
	}
	return true, "ok", nil
}

// Appends the manifest entry of a written export to manifest.jsonl in dir and returns the manifest path
func AppendManifest(dir string, session []byte, path, format string) (string, error) {
	s, err := parseDoc(session)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	lineage := s.get("lineage_session_ids")
	if !lineage.truthy() {
		lineage = &jv{kind: 'a', vals: []*jv{jvString(markdownID(s))}}
	}
	entry := &jv{kind: 'o',
		keys: []string{"session_id", "lineage_session_ids", "path", "format", "message_count", "sha256", "exported_at"},
		vals: []*jv{jvString(markdownID(s)), lineage, jvString(path), jvString(format), jvInt(markdownCount(s)),
			jvString(hex.EncodeToString(digest[:])), jvFloat(float64(time.Now().UnixNano()) / 1e9)}}
	out := home.ExpandUser(dir)
	if err := os.MkdirAll(out, 0o777); err != nil {
		return "", err
	}
	manifest := filepath.Join(out, "manifest.jsonl")
	f, err := os.OpenFile(manifest, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(entry.text(jsonLayout{sorted: true}) + lineEnding)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	return manifest, nil
}

// Session document with every string under its messages and segments passed through redact
func RedactSession(session []byte, redact func(string) string) ([]byte, error) {
	s, err := parseDoc(session)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"messages", "segments"} {
		if v := s.get(key); !v.null() {
			s = s.with(key, redacted(v, redact))
		}
	}
	return []byte(s.text(jsonLayout{})), nil
}

// Copy of v with every string value passed through redact, object keys kept
func redacted(v *jv, redact func(string) string) *jv {
	switch v.kind {
	case 's':
		return jvString(redact(v.str))
	case 'o', 'a':
		out := &jv{kind: v.kind, keys: v.keys, vals: make([]*jv, len(v.vals))}
		for i, child := range v.vals {
			out.vals[i] = redacted(child, redact)
		}
		return out
	}
	return v
}

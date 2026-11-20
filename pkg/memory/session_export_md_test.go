package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Exported telegram session with a user turn, a tool calling assistant turn and its tool result
func mdExportSession(overrides ...any) *pyjson.Object {
	session := pyjson.ObjectOf("id", "20260706_123456_abcd1234", "title", "Export Test", "source", "telegram", "model", "gpt-5.5",
		"billing_provider", "openai-codex", "cwd", "/tmp/project", "started_at", 1783331696.0, "last_active", 1783331705.0,
		"ended_at", 1783331710.0, "message_count", 3, "tool_call_count", 1, "archived", 0, "messages", []any{
			pyjson.ObjectOf("role", "user", "content", "Hello", "created_at", 1783331697.0),
			pyjson.ObjectOf("role", "assistant", "content", "", "tool_calls", []any{pyjson.ObjectOf("function",
				pyjson.ObjectOf("name", "terminal", "arguments", "{\"command\": \"pwd\"}"))}, "created_at", 1783331698.0),
			pyjson.ObjectOf("role", "tool", "name", "terminal", "content", "output", "created_at", 1783331699.0),
		})
	for i := 0; i+1 < len(overrides); i += 2 {
		session.Set(overrides[i].(string), overrides[i+1])
	}
	return session
}

func TestSafeSessionFilenameIsDeterministicAndPathSafe(t *testing.T) {
	filename, err := SafeSessionFilename(mdExportSession("id", "20260706_123456_abcd1234", "title", "Bad / title: * ?"), "qmd")
	if err != nil || !strings.HasPrefix(filename, "20260706_123456_abcd1234-") || !strings.HasSuffix(filename, ".qmd") || strings.ContainsAny(filename, "/:*?") {
		t.Fatalf("file name %q %v", filename, err)
	}
	if got, _ := SafeSessionFilename(pyjson.ObjectOf("id", "x", "title", "Bad / title: * ?"), "qmd"); got != "x-bad-title.qmd" {
		t.Fatalf("file name %q", got)
	}
	if got, _ := SafeSessionFilename(pyjson.ObjectOf("title", "...___"), "md"); got != "unknown-session-session.md" {
		t.Fatalf("slugless file name %q", got)
	}
	if _, err := SafeSessionFilename(mdExportSession(), "html"); err == nil || err.Error() != "fmt must be 'md' or 'qmd'" {
		t.Fatalf("format error %v", err)
	}
}

func TestVerifyExportFileChecksCountAndSha(t *testing.T) {
	session := mdExportSession()
	dir := t.TempDir()
	path, err := WriteSessionMarkdown(session, dir, "md", false)
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason, err := VerifyExportFile(path, session); err != nil || !ok || reason != "ok" {
		t.Fatalf("fresh export %v %q %v", ok, reason, err)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(text), "Hello", "Tampered")), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, reason, err := VerifyExportFile(path, session); err != nil || ok || !strings.Contains(reason, "sha256") {
		t.Fatalf("tampered export %v %q %v", ok, reason, err)
	}
}

func TestExportBodyMatchesTheReferenceRendering(t *testing.T) {
	session := mdExportSession()
	messages, _ := session.Get("messages")
	session.Set("messages", append(messages.([]any), pyjson.ObjectOf("role", "assistant", "content",
		[]any{pyjson.ObjectOf("type", "text", "text", "caf\u00e9  ")}, "timestamp", 1783331700.25)))
	if got, err := exportBodyWithoutHash(session, "md", 1783331800.5); err != nil || got != "---\nsession_id: \"20260706_123456_abcd1234\"\ntitle: \"Export Test\"\nsource: \"telegram\"\ncreated_at: \"2026-07-06T09:54:56Z\"\nupdated_at: \"2026-07-06T09:55:05Z\"\nended_at: \"2026-07-06T09:55:10Z\"\nmodel: \"gpt-5.5\"\nprovider: \"openai-codex\"\ncwd: \"/tmp/project\"\narchived: false\nmessage_count: 4\ntool_call_count: 1\nformat: \"md\"\nexported_at: \"2026-07-06T09:56:40.500000Z\"\nexporter: \"icebath sessions export (md/qmd) v1\"\n---\n\n# Export Test\n\nSession ID: `20260706_123456_abcd1234`\n\nSource: `telegram`\n\nWorking directory: `/tmp/project`\n\n## Messages\n\n### User \u2014 2026-07-06T09:54:57Z\n\nHello\n\n\n### Assistant \u2014 2026-07-06T09:54:58Z\n\n\n\n## Tool calls\n\n```json\n[\n  {\n    \"function\": {\n      \"name\": \"terminal\",\n      \"arguments\": \"{\\\"command\\\": \\\"pwd\\\"}\"\n    }\n  }\n]\n```\n\n\n### Tool \u2014 terminal \u2014 2026-07-06T09:54:59Z\n\noutput\n\n\n### Assistant \u2014 2026-07-06T09:55:00.250000Z\n\n```json\n[\n  {\n    \"type\": \"text\",\n    \"text\": \"caf\u00e9  \"\n  }\n]\n```\n\n## Export verification\n\n- Session id: `20260706_123456_abcd1234`\n- Exported messages: `4`\n- Source DB message count at export: `3`\n- Exported at: `2026-07-06T09:56:40.500000Z`\n- SHA256 of exported body: `__SHA256_PLACEHOLDER__`\n" {
		t.Fatalf("body %v\n%q", err, got)
	}
	lineage := pyjson.ObjectOf("session_id", "tip", "lineage_session_ids", []any{"root", "tip"}, "segments", []any{
		pyjson.ObjectOf("id", "root", "messages", []any{pyjson.ObjectOf("role", "user", "content", "one\n\n")}),
		pyjson.ObjectOf("id", "tip", "messages", []any{})})
	if got, err := exportBodyWithoutHash(lineage, "qmd", 1783331800.0); err != nil || got != "---\nsession_id: \"tip\"\ntitle: null\nsource: null\ncreated_at: \"\"\nupdated_at: \"\"\nended_at: \"\"\nmodel: null\nprovider: null\ncwd: null\narchived: false\nmessage_count: 1\ntool_call_count: 0\nlineage_session_ids: [\"root\", \"tip\"]\nformat: \"qmd\"\nexported_at: \"2026-07-06T09:56:40Z\"\nexporter: \"icebath sessions export (md/qmd) v1\"\n---\n\n# tip\n\nSession ID: `tip`\n\n## Messages\n\n## Compression segment: root\n\n### User\n\none\n\n\n## Compression segment: tip\n\n## Export verification\n\n- Session id: `tip`\n- Exported messages: `1`\n- Source DB message count at export: `1`\n- Exported at: `2026-07-06T09:56:40Z`\n- SHA256 of exported body: `__SHA256_PLACEHOLDER__`\n" {
		t.Fatalf("lineage body %v\n%q", err, got)
	}
	if got, err := exportBodyWithoutHash(pyjson.NewObject(), "md", 1783331800.0); err != nil || got != "---\nsession_id: \"unknown-session\"\ntitle: null\nsource: null\ncreated_at: \"\"\nupdated_at: \"\"\nended_at: \"\"\nmodel: null\nprovider: null\ncwd: null\narchived: false\nmessage_count: 0\ntool_call_count: 0\nformat: \"md\"\nexported_at: \"2026-07-06T09:56:40Z\"\nexporter: \"icebath sessions export (md/qmd) v1\"\n---\n\n# unknown-session\n\nSession ID: `unknown-session`\n\n## Messages\n\n_No messages in this session._\n\n## Export verification\n\n- Session id: `unknown-session`\n- Exported messages: `0`\n- Source DB message count at export: `0`\n- Exported at: `2026-07-06T09:56:40Z`\n- SHA256 of exported body: `__SHA256_PLACEHOLDER__`\n" {
		t.Fatalf("empty body %v\n%q", err, got)
	}
}

func TestWriteSessionMarkdownRefusesAnExistingFileAndTheManifestRecordsEachExport(t *testing.T) {
	session := mdExportSession()
	dir := filepath.Join(t.TempDir(), "exports")
	path, err := WriteSessionMarkdown(session, dir, "md", false)
	if err != nil || path != filepath.Join(dir, "20260706_123456_abcd1234-export-test.md") {
		t.Fatalf("path %q %v", path, err)
	}
	_, err = WriteSessionMarkdown(session, dir, "md", false)
	if exists, ok := err.(*ExportExistsError); !ok || exists.Error() != path {
		t.Fatalf("second write %v", err)
	}
	if _, err := WriteSessionMarkdown(session, dir, "md", true); err != nil {
		t.Fatalf("forced write %v", err)
	}
	manifest, err := AppendManifestEntry(dir, session, path, "md")
	if err != nil || manifest != filepath.Join(dir, "manifest.jsonl") {
		t.Fatalf("manifest %q %v", manifest, err)
	}
	digest, _ := FileSHA256(path)
	line, _ := os.ReadFile(manifest)
	want := "{\"exported_at\": "
	if !strings.HasPrefix(string(line), want) || !strings.HasSuffix(string(line), ", \"format\": \"md\", \"lineage_session_ids\": [\"20260706_123456_abcd1234\"], "+
		"\"message_count\": 3, \"path\": \""+path+"\", \"session_id\": \"20260706_123456_abcd1234\", \"sha256\": \""+digest+"\"}\n") {
		t.Fatalf("manifest line %q", line)
	}
	text, err := RenderSessionMarkdown(session, "md", false)
	if err != nil || strings.Contains(text, "Export verification") || !strings.HasSuffix(text, "\n\noutput\n") {
		t.Fatalf("render without verification %v %q", err, text)
	}
}

func TestRedactSessionDataMasksSecretsInMessagesOnly(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	session := pyjson.ObjectOf("id", "s", "title", "key "+secret, "messages", []any{
		pyjson.ObjectOf("role", "user", "content", "my key is "+secret, "n", 3),
		pyjson.ObjectOf("role", "assistant", "content", []any{pyjson.ObjectOf("type", "text", "text", "use "+secret)})})
	redacted := RedactSessionData(session)
	if title, _ := redacted.Get("title"); title != "key "+secret {
		t.Fatalf("title %v", title)
	}
	messages, _ := redacted.Get("messages")
	first := messages.([]any)[0].(*pyjson.Object)
	if content, _ := first.Get("content"); strings.Contains(content.(string), secret) || !strings.HasPrefix(content.(string), "my key is ") {
		t.Fatalf("content %v", content)
	}
	if n, _ := first.Get("n"); n != 3 {
		t.Fatalf("number %v", n)
	}
	second := messages.([]any)[1].(*pyjson.Object)
	parts, _ := second.Get("content")
	if text, _ := parts.([]any)[0].(*pyjson.Object).Get("text"); strings.Contains(text.(string), secret) {
		t.Fatalf("nested text %v", text)
	}
	original, _ := session.Get("messages")
	if content, _ := original.([]any)[0].(*pyjson.Object).Get("content"); content != "my key is "+secret {
		t.Fatalf("the source session was changed: %v", content)
	}
}

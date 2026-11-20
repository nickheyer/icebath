package state

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Exported session with a system turn, two user prompts, an assistant turn and a tool result
func sampleExportSession() *pyjson.Object {
	return pyjson.ObjectOf("id", "sess-123", "source", "cli", "model", "test/model", "title", "Debug auth flow",
		"started_at", 1700000000, "message_count", 5, "messages", []any{
			pyjson.ObjectOf("id", 1, "role", "system", "content", "hidden system context", "timestamp", 1700000000),
			pyjson.ObjectOf("id", 2, "role", "user", "content", "Why is login broken?", "timestamp", 1700000001, "platform_message_id", "evt-2"),
			pyjson.ObjectOf("id", 3, "role", "assistant", "content", "I will inspect the auth middleware.", "timestamp", 1700000002),
			pyjson.ObjectOf("id", 4, "role", "tool", "tool_name", "read_file", "content", "def redirect_after_login(): pass", "timestamp", 1700000003),
			pyjson.ObjectOf("id", 5, "role", "user", "content", []any{pyjson.ObjectOf("type", "text", "text", "Only show me the prompts.")}, "timestamp", 1700000004),
		})
}

func TestExportRecordCountSwitchesUnitForPromptOnlyExports(t *testing.T) {
	sessions := []*pyjson.Object{sampleExportSession()}
	if n, noun, err := ExportRecordCount(sessions, nil); err != nil || n != 1 || noun != "session" {
		t.Fatalf("count %d %q %v", n, noun, err)
	}
	if n, noun, err := ExportRecordCount(sessions, "user-prompts"); err != nil || n != 2 || noun != "prompt" {
		t.Fatalf("prompt count %d %q %v", n, noun, err)
	}
}

func TestRenderSessionsExportMarkdownAndPromptViews(t *testing.T) {
	sessions := []*pyjson.Object{sampleExportSession()}
	metadata := "- Session ID: `sess-123`\n- Source: `cli`\n- Model: `test/model`\n- Title: Debug auth flow\n" +
		"- Started: 2023-11-14T22:13:20Z\n- Messages: 5\n\n"
	wantMarkdown := "# Session: Debug auth flow\n" + metadata +
		"## User - 2023-11-14T22:13:21Z\n\nWhy is login broken?\n\n" +
		"## Assistant - 2023-11-14T22:13:22Z\n\nI will inspect the auth middleware.\n\n" +
		"## Tool: read_file - 2023-11-14T22:13:23Z\n\n<details><summary>read_file</summary>\n\n" +
		"```text\ndef redirect_after_login(): pass\n```\n\n</details>\n\n" +
		"## User - 2023-11-14T22:13:24Z\n\nOnly show me the prompts.\n"
	if got, err := RenderSessionsExport(sessions, "md", nil); err != nil || got != wantMarkdown {
		t.Fatalf("markdown %v\n%q\nwant\n%q", err, got, wantMarkdown)
	}
	wantPrompts := "# User prompts for session sess-123\n" + metadata +
		"## 1. 2023-11-14T22:13:21Z\nMessage ID: `2`\n\nWhy is login broken?\n\n" +
		"## 2. 2023-11-14T22:13:24Z\nMessage ID: `5`\n\nOnly show me the prompts.\n"
	if got, err := RenderSessionsExport(sessions, "markdown", "user-prompts"); err != nil || got != wantPrompts {
		t.Fatalf("prompt markdown %v\n%q\nwant\n%q", err, got, wantPrompts)
	}
	wantJSONL := `{"session_id": "sess-123", "index": 1, "created_at": "2023-11-14T22:13:21Z", "role": "user", "text": "Why is login broken?", "message_id": 2, "event_id": "evt-2"}` + "\n" +
		`{"session_id": "sess-123", "index": 2, "created_at": "2023-11-14T22:13:24Z", "role": "user", "text": "Only show me the prompts.", "message_id": 5}` + "\n"
	if got, err := RenderSessionsExport(sessions, "jsonl", "prompts"); err != nil || got != wantJSONL {
		t.Fatalf("prompt jsonl %v\n%q\nwant\n%q", err, got, wantJSONL)
	}
	two := []*pyjson.Object{sampleExportSession(), pyjson.ObjectOf("session_id", "other", "messages", nil)}
	got, err := RenderSessionsExport(two, "markdown", nil)
	if err != nil || !strings.HasPrefix(got, "# Icebath sessions export\n\n## Session: Debug auth flow\n- Session ID: `sess-123`\n") ||
		!strings.Contains(got, "### User - 2023-11-14T22:13:21Z\n") || !strings.HasSuffix(got, "\n## Session: other\n- Session ID: `other`\n\n_No messages found._\n") {
		t.Fatalf("multi markdown %v\n%q", err, got)
	}
	if got, err := RenderSessionsExport(nil, "markdown", "user"); err != nil || got != "# User prompts export\n\n_No user prompts found._\n" {
		t.Fatalf("empty prompt markdown %v %q", err, got)
	}
	if got, err := RenderSessionsExport(nil, "jsonl", nil); err != nil || got != "" {
		t.Fatalf("empty jsonl %v %q", err, got)
	}
	if _, err := RenderSessionsExport(sessions, "xml", nil); err == nil || err.Error() != "Unsupported session export format: xml" {
		t.Fatalf("format error %v", err)
	}
	if _, err := RenderSessionsExport(sessions, "jsonl", "assistant"); err == nil || err.Error() != "Unsupported session export filter: assistant" {
		t.Fatalf("filter error %v", err)
	}
}

func TestSaveFormatsNormalizeAndRender(t *testing.T) {
	for token, want := range map[string]string{"": "json", " Snapshot ": "json", "MARKDOWN": "md", "html": "html"} {
		if got, err := NormalizeSaveFormat(token); err != nil || got != want {
			t.Fatalf("%q normalized to %q %v", token, got, err)
		}
	}
	if _, err := NormalizeSaveFormat("pdf"); err == nil || err.Error() != "Unknown format 'pdf' — expected one of: json, md, html" {
		t.Fatalf("format error %v", err)
	}
	if got := DefaultSaveFilename("20260308_000001 ab/cd", "md"); got != "icebath_session_20260308_000001abcd.md" {
		t.Fatalf("file name %q", got)
	}
	if got := DefaultSaveFilename("///", "json"); got != "icebath_session_session.json" {
		t.Fatalf("empty id file name %q", got)
	}
	session := pyjson.ObjectOf("id", "s1", "title", "Tést", "messages", []any{})
	if got, err := RenderSessionForSave(session, "json"); err != nil || got != "{\n  \"id\": \"s1\",\n  \"title\": \"Tést\",\n  \"messages\": []\n}" {
		t.Fatalf("json save %v %q", err, got)
	}
	if got, err := RenderSessionForSave(session, "md"); err != nil || got != "# Session: Tést\n- Session ID: `s1`\n- Title: Tést\n\n_No messages found._\n" {
		t.Fatalf("markdown save %v %q", err, got)
	}
	if _, err := RenderSessionForSave(session, "yaml"); err == nil || err.Error() != "Unknown save format: 'yaml'" {
		t.Fatalf("save error %v", err)
	}
	if c, i := ExportProjection(true); !c || i {
		t.Fatalf("transcript projection %v %v", c, i)
	}
	if c, i := ExportProjection(false); c || !i {
		t.Fatalf("snapshot projection %v %v", c, i)
	}
}

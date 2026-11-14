package state

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Messages covering every rendering branch, the same sample the reference page was recorded from
func htmlSampleMessages() []*pyjson.Object {
	user := func(text string) *pyjson.Object { return pyjson.ObjectOf("role", "user", "content", text) }
	return []*pyjson.Object{
		pyjson.ObjectOf("role", "session_meta", "content", "hidden"),
		pyjson.ObjectOf("role", "system", "content", "be <kind> & 'calm'", "timestamp", 0),
		pyjson.ObjectOf("role", "user", "content", []any{pyjson.ObjectOf("type", "text", "text", "look \"here\""),
			pyjson.ObjectOf("type", "image_url", "image_url", pyjson.ObjectOf("url", "x")), 7, pyjson.ObjectOf("type", "other")}, "timestamp", nil),
		pyjson.ObjectOf("role", "assistant", "content", "", "tool_calls", []any{
			pyjson.ObjectOf("id", "c1", "type", "function", "function", pyjson.ObjectOf("name", "read<file>", "arguments", "{\"p\": \"a&b\"}")),
			pyjson.ObjectOf("id", "c2")}, "reasoning", "think > act", "timestamp", "not-a-timestamp"),
		pyjson.ObjectOf("role", "tool", "content", "out\nput", "tool_name", "read_file"),
		pyjson.ObjectOf("role", "We ird/Role", "content", "x", "reasoning_content", "r2"),
		pyjson.ObjectOf("content", "no role"), user("8"), user("9"), user("10"), user("11"),
	}
}

// The two sessions of the reference page, one long titled with a system prompt and one untitled
func htmlSampleSessions() (*pyjson.Object, *pyjson.Object) {
	rows := []any{}
	for _, m := range htmlSampleMessages() {
		rows = append(rows, m)
	}
	s1 := pyjson.ObjectOf("id", "abc/1 2", "title", strings.Repeat("T", 60), "model", nil, "started_at", 0,
		"system_prompt", "You <are> here", "messages", rows)
	s2 := pyjson.ObjectOf("id", "s2", "title", nil, "preview", "pre&view", "messages", []any{})
	return s1, s2
}

func TestHTMLExportMatchesTheReferencePage(t *testing.T) {
	data, err := os.ReadFile("testdata/session_export_html_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref map[string]string
	if err := json.Unmarshal(data, &ref); err != nil {
		t.Fatal(err)
	}
	s1, s2 := htmlSampleSessions()
	check := func(name, got string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != ref[name] {
			i := 0
			for i < len(got) && i < len(ref[name]) && got[i] == ref[name][i] {
				i++
			}
			t.Fatalf("%s differs at byte %d: got %q, want %q", name, i, got[max(0, i-60):min(len(got), i+60)], ref[name][max(0, i-60):min(len(ref[name]), i+60)])
		}
	}
	messages, err := generateMessagesHTML(htmlSampleMessages())
	check("messages_html", messages, err)
	single, err := sessionViewHTML(s1, false)
	check("view_single", single, err)
	multi, err := sessionViewHTML(s2, true)
	check("view_multi", multi, err)
	check("sidebar", sidebarHTML([]*pyjson.Object{s1, s2}), nil)
	empty, err := GenerateMultiSessionHTMLExport(nil)
	check("empty", empty, err)
	fullSingle, err := renderHTMLExport([]*pyjson.Object{s1}, "2026-01-02 03:04:05", "NONCE")
	check("full_single", fullSingle, err)
	fullMulti, err := renderHTMLExport([]*pyjson.Object{s1, s2}, "2026-01-02 03:04:05", "NONCE")
	check("full_multi", fullMulti, err)
}

func TestSingleSessionUntitledCoalescesNoneTitleAndModel(t *testing.T) {
	html, err := GenerateHTMLExport(pyjson.ObjectOf("id", "abc", "title", nil, "model", nil, "messages", []any{}))
	if err != nil || strings.Contains(html, "<title>None</title>") || strings.Contains(html, "<strong>Model:</strong> None") {
		t.Fatalf("export %v", err)
	}
	if !strings.Contains(html, "<title>Icebath Session</title>") || !strings.Contains(html, "<strong>Model:</strong> Unknown") {
		t.Fatal("the untitled defaults are missing")
	}
	nonce := regexp.MustCompile(`nonce="([A-Za-z0-9_-]{22})"`).FindStringSubmatch(html)
	if nonce == nil || !regexp.MustCompile(`Generated on \d{4}-\d\d-\d\d \d\d:\d\d:\d\d`).MatchString(html) {
		t.Fatal("the page carries no script nonce or generation time")
	}
}

func TestRoleIsEscapedInHTMLExport(t *testing.T) {
	html, err := generateMessagesHTML([]*pyjson.Object{pyjson.ObjectOf("role", "<img src=x onerror=alert(document.domain)>",
		"content", "hello", "timestamp", 1700000000)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<img src=x onerror=alert(document.domain)>") || !strings.Contains(html, "&lt;img src=x onerror=alert(document.domain)&gt;") {
		t.Fatalf("role not escaped: %s", html)
	}
	class := regexp.MustCompile(`class="(message message-[^"]*active)"`).FindStringSubmatch(html)
	if class == nil || !strings.Contains(class[1], " message-") || strings.Count(class[1], "message-") != 1 {
		t.Fatalf("class attribute %v", class)
	}
	if want := time.Unix(1700000000, 0).Local().Format("2006-01-02 15:04:05"); !strings.Contains(html, `<div class="timestamp">`+want+`</div>`) {
		t.Fatalf("timestamp %s missing: %s", want, html)
	}
}

func TestHTMLExportEscapesToolCallNames(t *testing.T) {
	payload := `<img src=x onerror="alert(document.domain)">`
	rendered, err := generateMessagesHTML([]*pyjson.Object{pyjson.ObjectOf("role", "assistant", "content", "", "tool_calls", []any{
		pyjson.ObjectOf("id", "call_1", "type", "function", "function", pyjson.ObjectOf("name", payload, "arguments", "<b>x</b>"))})})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, payload) || !strings.Contains(rendered, "&lt;img src=x onerror=&quot;alert(document.domain)&quot;&gt;") ||
		!strings.Contains(rendered, "&lt;b&gt;x&lt;/b&gt;") {
		t.Fatalf("tool call not escaped: %s", rendered)
	}
}

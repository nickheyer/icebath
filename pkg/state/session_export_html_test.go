package state

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

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

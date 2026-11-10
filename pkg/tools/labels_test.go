package tools

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestBuildToolPreview(t *testing.T) {
	SetToolPreviewMaxLen(0)
	defer SetToolPreviewMaxLen(0)
	if BuildToolPreview(nil, "terminal", nil, nil) != "" || BuildToolPreview(nil, "terminal", map[string]any{}, nil) != "" {
		t.Fatal("empty args give no preview")
	}
	args := map[string]any{"action": "patch", "target": "memory", "old_text": "port 8080", "content": "port 9090"}
	if got := BuildToolPreview(nil, "memory", args, nil); got != `~memory: "port 8080"` {
		t.Fatalf("memory patch preview %q", got)
	}
	one := map[string]any{"target": "user", "operations": []any{map[string]any{"action": "add", "content": "Name: Alice"}}}
	if got := BuildToolPreview(nil, "memory", one, nil); got != `+user: "Name: Alice"` {
		t.Fatalf("memory add preview %q", got)
	}
	three := map[string]any{"target": "memory", "operations": []any{
		map[string]any{"action": "remove", "old_text": "port 8080"}, map[string]any{"action": "add", "content": "a"},
		map[string]any{"action": "patch", "old_text": "b", "content": "c"}}}
	if got := BuildToolPreview(nil, "memory", three, nil); got != `-memory: "port 8080" (2 more)` {
		t.Fatalf("memory batch preview %q", got)
	}
	secret := "sk-proj-ABCD1234567890EFGH"
	got := BuildToolPreview(nil, "browser_type", map[string]any{"ref": "@e3", "text": secret}, nil)
	if got == "" || strings.Contains(got, secret) || !strings.Contains(got, "sk-pro") {
		t.Fatalf("browser_type preview %q", got)
	}
	if got := BuildToolPreview(nil, "browser_type", map[string]any{"ref": "@e3", "text": "hello world search query"}, nil); !strings.Contains(got, "hello world search query") {
		t.Fatalf("normal text preview %q", got)
	}
	safe := RedactToolArgsForDisplay(nil, "browser_type", map[string]any{"ref": "@e3", "text": "ghp_ABCDEFGHIJ1234567890"})
	if safe["ref"] != "@e3" || !strings.HasPrefix(safe["text"].(string), "ghp_AB") || safe["text"] == "ghp_ABCDEFGHIJ1234567890" {
		t.Fatalf("display args %v", safe)
	}
	thirty := 30
	if got := BuildToolPreview(nil, "delegate_task", map[string]any{"tasks": []any{map[string]any{"goal": strings.Repeat("A", 80)}, map[string]any{"goal": strings.Repeat("B", 80)}}}, &thirty); got == "" || len([]rune(got)) > 30 {
		t.Fatalf("delegate preview %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz"
	for maxLen := 1; maxLen <= 4; maxLen++ {
		for tool, args := range map[string]map[string]any{"terminal": {"command": long}, "web_search": {"query": long}} {
			if got := BuildToolPreview(nil, tool, args, &maxLen); got == "" || len([]rune(got)) > maxLen {
				t.Fatalf("%s max %d preview %q", tool, maxLen, got)
			}
		}
	}
	if got := BuildToolPreview(nil, "terminal", map[string]any{"command": "cd app && npm test 2>&1 | tail -5; echo done"}, nil); got != "npm test + 1 command" {
		t.Fatalf("shell summary %q", got)
	}
	if got := SummarizeShellCommand("cd app && npm test 2>&1 | tail -5 && echo ---- && git status"); got != "npm test + 1 command" {
		t.Fatalf("shell summary plus %q", got)
	}
	if got := BuildToolPreview(nil, "read_file", map[string]any{"path": "/srv/app/main.go", "offset": 10, "limit": 5}, nil); got != "main.go L10-14" {
		t.Fatalf("read_file preview %q", got)
	}
}

func TestLabelsAndVerbs(t *testing.T) {
	SetFriendlyToolLabels(true)
	defer SetFriendlyToolLabels(true)
	label, ok := LabelForToolName(nil, "connectors__github__GITHUB_CREATE_ISSUE", map[string]any{"connector_alias": "work"})
	if !ok || label.Kind != "connector" || label.App != "GitHub" || label.Action != "create issue (work)" || label.Text() != "GitHub · create issue (work)" {
		t.Fatalf("connector label %+v", label)
	}
	label, _ = LabelForToolName(nil, "mcp_stripe_mcp__list_customers", nil)
	if label.Kind != "mcp" || label.App != "Stripe" || label.Action != "list customers" || label.Emoji != MCPEmoji {
		t.Fatalf("mcp label %+v", label)
	}
	if AppTitle("google-drive_v2") != "Google Drive V2" || AppTitle("") != "" {
		t.Fatal("app title")
	}
	labels := LabelsForCall(nil, "tool_describe", map[string]any{"names": []any{"a", "b"}})
	if len(labels) != 1 || labels[0].Action != "2 tools" || labels[0].App != "Reading tool details" {
		t.Fatalf("describe labels %+v", labels)
	}
	labels = LabelsForCall(nil, "tool_call", map[string]any{"calls": []any{map[string]any{"name": "mcp_x__y"}, map[string]any{}}})
	if len(labels) != 2 || labels[1].App != "Unnamed call" {
		t.Fatalf("bridge labels %+v", labels)
	}
	if LabelsForCall(nil, "terminal", map[string]any{"command": "ls"}) != nil {
		t.Fatal("local tools produce no bridge labels")
	}
	args := map[string]any{"query": "weather in NYC"}
	if got := BuildToolLabel(nil, "web_search", args, nil); got != "Searching the web for weather in NYC" {
		t.Fatalf("friendly label %q", got)
	}
	SetFriendlyToolLabels(false)
	if got := BuildToolLabel(nil, "web_search", args, nil); got != BuildToolPreview(nil, "web_search", args, nil) || strings.Contains(got, "Searching the web") {
		t.Fatalf("disabled label %q", got)
	}
	if BuildStatusPhrase(nil, "terminal", map[string]any{"command": "ls"}, 49) != "" {
		t.Fatal("status phrase respects the toggle")
	}
	SetFriendlyToolLabels(true)
	phrase := BuildStatusPhrase(nil, "terminal", map[string]any{"command": strings.Repeat("x", 300)}, 49)
	if phrase == "" || len([]rune(phrase)) > 49 || !strings.HasSuffix(phrase, "…") || !strings.HasPrefix(phrase, "is running ") {
		t.Fatalf("status phrase %q", phrase)
	}
}

func TestVerboseToolArgsTextPrintsArgsAsPythonJSONDumps(t *testing.T) {
	if got := VerboseToolArgsText(pyjson.ObjectOf("a", "x\ny"), 0, true); got != "```\n{\"a\": \"x\ny\"}\n```" {
		t.Fatalf("code block %q", got)
	}
	args := pyjson.ObjectOf("zeta", int64(1), "alpha", 1.0, "path", "caf\u00e9\ttab", "nested", pyjson.ObjectOf("b", []any{int64(2), 2.5, nil, true}), "odd", struct{ X int }{3})
	want := `{"zeta": 1, "alpha": 1.0, "path": "café\ttab", "nested": {"b": [2, 2.5, null, true]}, "odd": "{3}"}`
	if got := VerboseToolArgsText(args, 0, false); got != want {
		t.Fatalf("plain got %q want %q", got, want)
	}
	if got := VerboseToolArgsText(args, 20, false); got != `{"zeta": 1, "alph...` {
		t.Fatalf("capped %q", got)
	}
	if got := VerboseToolArgsText(args, 0, true); got != "```\n"+strings.ReplaceAll(want, `\t`, "\t")+"\n```" {
		t.Fatalf("code block with a tab %q", got)
	}
	fenced := pyjson.ObjectOf("code", "```go\nx```")
	if got := VerboseToolArgsText(fenced, 0, true); got != `{"code": "`+"```go\\nx```"+`"}` {
		t.Fatalf("args holding a fence %q", got)
	}
}

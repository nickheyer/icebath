package acp

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func contentText(t *testing.T, c pyacp.ToolCallContent) string {
	t.Helper()
	cc, ok := c.(*pyacp.ContentToolCallContent)
	if !ok {
		t.Fatalf("content %T", c)
	}
	return cc.Content.(*pyacp.TextContentBlock).Text
}

func TestToolKindMapUnknownToolReturnsOtherKind(t *testing.T) {
	if GetToolKind("nonexistent_tool_xyz") != "other" {
		t.Fatal("kind")
	}
}

func TestMakeToolCallIdIdsAreUnique(t *testing.T) {
	ids := map[string]bool{}
	for range 100 {
		ids[MakeToolCallID()] = true
	}
	if len(ids) != 100 {
		t.Fatal(len(ids))
	}
}

func TestBuildToolTitleTerminalTitleTruncatesLongCommand(t *testing.T) {
	title := BuildToolTitle("terminal", obj("command", strings.Repeat("x", 200)))
	if runeLen(title) >= 120 || !strings.Contains(title, "...") {
		t.Fatalf("title %q", title)
	}
}

func TestBuildToolTitleUnknownToolUsesName(t *testing.T) {
	if got := BuildToolTitle("some_new_tool", obj("foo", "bar")); got != "some_new_tool" {
		t.Fatalf("title %q", got)
	}
}

func TestBuildToolTitleTitleDerivesFromDisplayPreview(t *testing.T) {
	for _, c := range []struct {
		name string
		args *pyjson.Object
	}{
		{"terminal", obj("command", "git status --short")},
		{"read_file", obj("path", "/etc/hosts", "offset", int64(10))},
		{"search_files", obj("pattern", "TODO", "path", "src")},
		{"web_search", obj("query", "hermes agent acp")},
		{"execute_code", obj("code", "\nfrom hermes_tools import terminal\nprint('done')")},
		{"skill_view", obj("name", "github", "file_path", "references/x.md")},
	} {
		limit := 80
		preview := tools.BuildToolPreview(nil, c.name, pyjson.Plain(c.args).(map[string]any), &limit)
		if !strings.Contains(BuildToolTitle(c.name, c.args), preview) {
			t.Fatalf("%s preview %q", c.name, preview)
		}
	}
}

func TestBuildToolStartForPatch(t *testing.T) {
	res := BuildToolStart("tc-1", "patch", obj("path", "src/main.py", "old_string", "print('hello')", "new_string", "print('world')"), nil)
	if *res.Kind != "edit" || len(res.Content) < 1 || !strings.Contains(contentText(t, res.Content[0]), "src/main.py") {
		t.Fatalf("start %#v", res)
	}
}

func TestBuildToolStartAutoApprovedEditStartShowsDiffContent(t *testing.T) {
	args := obj("path", "/tmp/acp.txt", "old_string", "old", "new_string", "new")
	res := BuildToolStart("tc-auto-edit", "patch", args, &tools.EditProposal{ToolName: "patch", Path: "/tmp/acp.txt", OldText: pyacp.Ptr("old\n"), NewText: "new\n"})
	d, ok := res.Content[0].(*pyacp.FileEditToolCallContent)
	if *res.Kind != "edit" || len(res.Content) != 1 || !ok || d.Path != "/tmp/acp.txt" || *d.OldText != "old\n" || d.NewText != "new\n" {
		t.Fatalf("start %#v", res)
	}
}

func TestBuildToolStartForBrowserNavigate(t *testing.T) {
	res := BuildToolStart("tc-browser-start", "browser_navigate", obj("url", "https://x.com"), nil)
	if !strings.Contains(res.Title, "https://x.com") || *res.Kind != "fetch" || !strings.Contains(contentText(t, res.Content[0]), "https://x.com") || res.RawInput != nil {
		t.Fatalf("start %#v", res)
	}
}

func TestBuildToolCompleteForTerminal(t *testing.T) {
	res := BuildToolComplete("tc-2", "terminal", pyacp.Ptr("total 42\ndrwxr-xr-x 2 root root 4096 ..."), nil, nil, false)
	if *res.Status != "completed" || len(res.Content) < 1 || !strings.Contains(contentText(t, res.Content[0]), "total 42") || res.RawOutput != nil {
		t.Fatalf("complete %#v", res)
	}
}

func TestBuildToolCompleteMarksReturncodeNonzeroAsFailed(t *testing.T) {
	res := BuildToolComplete("tc-fail", "execute_code", pyacp.Ptr(`{"output": "bad", "returncode": 2}`), nil, nil, false)
	if *res.Status != "failed" {
		t.Fatal(*res.Status)
	}
}

func TestBuildToolCompleteForSearchFilesFormatsMatches(t *testing.T) {
	res := BuildToolComplete("tc-search", "search_files", pyacp.Ptr(`{"total_count":2,"matches":[{"path":"README.md","line":3,"content":"TODO: fix this"},{"path":"src/app.py","line":9,"content":"needle"}],"truncated":true}`), nil, nil, false)
	text := contentText(t, res.Content[0])
	if !strings.Contains(text, "README.md:3") || !strings.Contains(text, "TODO: fix this") || !strings.Contains(text, "Results truncated") || res.RawOutput != nil {
		t.Fatalf("text %q", text)
	}
}

func TestBuildToolCompleteGenericallyFormatsUnknownJsonDictWithoutRawOutput(t *testing.T) {
	res := BuildToolComplete("tc-recall-search", "memory_archive_search", pyacp.Ptr(`{"results":[{"id":"obs-1","status":"active","content":"Recall should render as a readable summary."}],"trust":"lower-trust archive evidence"}`), nil, nil, false)
	text := contentText(t, res.Content[0])
	if !strings.Contains(text, "lower-trust archive evidence") || !strings.Contains(text, "Recall should render as a readable summary") || strings.Contains(text, `{"results"`) || res.RawOutput != nil {
		t.Fatalf("text %q", text)
	}
}

func TestExtractLocationsWithPath(t *testing.T) {
	locs, err := ExtractLocations(obj("path", "src/app.py", "offset", int64(42)))
	if err != nil || len(locs) != 1 || locs[0].Path != "src/app.py" || *locs[0].Line != 42 {
		t.Fatalf("locs %#v %v", locs, err)
	}
}

func TestExtractLocationsWithoutPath(t *testing.T) {
	locs, err := ExtractLocations(obj("command", "echo hi"))
	if err != nil || locs == nil || len(locs) != 0 {
		t.Fatalf("locs %#v", locs)
	}
}

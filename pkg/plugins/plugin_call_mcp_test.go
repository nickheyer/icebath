package plugins

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Plugin context of my-plugin in a fresh home whose config carries the given plugins.entries YAML body
func callMCPContext(t *testing.T, entriesYAML string) (context.Context, *Context) {
	t.Helper()
	h := t.TempDir()
	if entriesYAML != "" {
		writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n"+entriesYAML)
	}
	return homeCtx(h), newContext(&Manifest{Name: "my-plugin", Key: "my-plugin"}, NewManager(h))
}

// Transport double recording the server, tool, timeout and arguments of each MCP call it answers
type callMCPTransport struct {
	response string
	captured map[string]any
}

func installCallMCPTransport(t *testing.T, response string) *callMCPTransport {
	t.Helper()
	tr := &callMCPTransport{response: response, captured: map[string]any{}}
	previous := installedServices()
	InstallServices(&Services{CallMCPTool: func(_ context.Context, server, tool string, timeout float64, args map[string]any) (string, error) {
		tr.captured["server"], tr.captured["tool"], tr.captured["timeout"], tr.captured["args"] = server, tool, timeout, args
		return tr.response, nil
	}})
	t.Cleanup(func() { InstallServices(previous) })
	return tr
}

// Fails unless the call was refused with the permission error of the allowlist gate
func callMCPDenied(t *testing.T, err error) string {
	t.Helper()
	var denied *PluginPermissionError
	if !errors.As(err, &denied) {
		t.Fatalf("call_mcp error = %v, want a permission error", err)
	}
	return denied.Error()
}

func TestDefaultDenyWhenKeyAbsent(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin: {}\n")
	_, err := c.CallMCP(ctx, "github", "create_issue", map[string]any{"title": "x"}, nil)
	msg := callMCPDenied(t, err)
	if !strings.Contains(msg, "plugins.entries.my-plugin.mcp_allowlist") || !strings.Contains(msg, "github") {
		t.Fatalf("denial message %q", msg)
	}
}

func TestDefaultDenyWhenPluginHasNoEntry(t *testing.T) {
	ctx, c := callMCPContext(t, "")
	_, err := c.CallMCP(ctx, "github", "create_issue", nil, nil)
	callMCPDenied(t, err)
}

func TestDefaultDenyWhenConfigUnreadable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	writeFile(t, blocker, "x")
	h := filepath.Join(blocker, "home")
	if _, err := loadConfig(h); err == nil {
		t.Fatal("a home under a regular file loaded its config without an error")
	}
	c := newContext(&Manifest{Name: "my-plugin", Key: "my-plugin"}, NewManager(h))
	_, err := c.CallMCP(homeCtx(h), "github", "create_issue", nil, nil)
	callMCPDenied(t, err)
}

func TestUnlistedServerDeniedEvenWithOtherGrants(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: [knowledge_rag]\n")
	_, err := c.CallMCP(ctx, "github", "create_issue", nil, nil)
	if msg := callMCPDenied(t, err); !strings.Contains(msg, "github") {
		t.Fatalf("denial message %q", msg)
	}
}

func TestNonListAllowlistIsDenied(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: '*'\n")
	_, err := c.CallMCP(ctx, "github", "create_issue", nil, nil)
	callMCPDenied(t, err)
}

func TestDeniedCallNeverTouchesTransport(t *testing.T) {
	ctx, c := callMCPContext(t, "")
	tr := installCallMCPTransport(t, `{"result": "hi"}`)
	_, err := c.CallMCP(ctx, "github", "create_issue", nil, nil)
	callMCPDenied(t, err)
	if len(tr.captured) != 0 {
		t.Fatalf("a denied call reached the transport: %v", tr.captured)
	}
}

func TestAllowedCallRoutesThroughExistingHandler(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: [github]\n")
	tr := installCallMCPTransport(t, `{"result": "issue #7 created"}`)
	result, err := c.CallMCP(ctx, "github", "create_issue", map[string]any{"title": "bug"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.captured["server"] != "github" || tr.captured["tool"] != "create_issue" || !reflect.DeepEqual(tr.captured["args"], map[string]any{"title": "bug"}) {
		t.Fatalf("captured %v", tr.captured)
	}
	if !reflect.DeepEqual(result, map[string]any{"ok": true, "result": "issue #7 created"}) {
		t.Fatalf("result %v", result)
	}
}

func TestErrorResultMapsToOkFalse(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: [github]\n")
	installCallMCPTransport(t, `{"error": "MCP server 'github' is not connected"}`)
	result, err := c.CallMCP(ctx, "github", "create_issue", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := result["error"].(string)
	if result["ok"] != false || !strings.Contains(msg, "not connected") {
		t.Fatalf("result %v", result)
	}
}

func TestStructuredContentPassthrough(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: [rag]\n")
	installCallMCPTransport(t, `{"result": "text part", "structuredContent": {"hits": 3}}`)
	result, err := c.CallMCP(ctx, "rag", "query", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true || result["result"] != "text part" || !reflect.DeepEqual(result["structuredContent"], map[string]any{"hits": float64(3)}) {
		t.Fatalf("result %v", result)
	}
}

func TestTimeoutDefaultsAndBounds(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: [s]\n")
	tr := installCallMCPTransport(t, `{"result": ""}`)
	for _, tc := range []struct {
		timeout any
		want    float64
	}{{nil, 30.0}, {0, 1.0}, {99999, 600.0}, {"nonsense", 30.0}} {
		if _, err := c.CallMCP(ctx, "s", "t", nil, tc.timeout); err != nil {
			t.Fatal(err)
		}
		if tr.captured["timeout"] != tc.want {
			t.Fatalf("timeout %v reached the transport as %v, want %v", tc.timeout, tr.captured["timeout"], tc.want)
		}
	}
}

func TestOversizedResultIsTruncated(t *testing.T) {
	ctx, c := callMCPContext(t, "    my-plugin:\n      mcp_allowlist: [big]\n")
	installCallMCPTransport(t, strings.Repeat("x", mcpResultCharCap+5000))
	result, err := c.CallMCP(ctx, "big", "dump", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := result["result"].(string)
	if result["ok"] != true || result["truncated"] != true || utf8.RuneCountInString(text) > mcpResultCharCap+20 || !strings.HasSuffix(text, "… [truncated]") {
		t.Fatalf("ok %v truncated %v length %d", result["ok"], result["truncated"], utf8.RuneCountInString(text))
	}
}

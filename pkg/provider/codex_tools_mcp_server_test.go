package provider

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSignatureFromSchema(t *testing.T) {
	t.Run("test_simple_required_string_param", func(t *testing.T) {
		sig, annots := codexToolsSignatureFromSchema(map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}})
		if len(sig.Params) != 1 || sig.Params[0].Name != "query" || annots["query"] != "str" || !sig.Params[0].Required || sig.ReturnAnnotation != "str" {
			t.Fatalf("simple required %+v %v", sig, annots)
		}
	})
	t.Run("test_skip_private_params", func(t *testing.T) {
		sig, annots := codexToolsSignatureFromSchema(map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "_internal": map[string]any{"type": "string"}}, "required": []any{"query", "_internal"}})
		if _, ok := annots["_internal"]; ok || len(sig.Params) != 1 || sig.Params[0].Name != "query" {
			t.Fatalf("private params %+v %v", sig, annots)
		}
	})
	t.Run("test_all_json_types", func(t *testing.T) {
		_, annots := codexToolsSignatureFromSchema(map[string]any{"type": "object", "properties": map[string]any{"s": map[string]any{"type": "string"}, "i": map[string]any{"type": "integer"},
			"n": map[string]any{"type": "number"}, "b": map[string]any{"type": "boolean"}, "a": map[string]any{"type": "array"}, "o": map[string]any{"type": "object"}}, "required": []any{"s", "i", "n", "b", "a", "o"}})
		if !reflect.DeepEqual(annots, map[string]string{"s": "str", "i": "int", "n": "float", "b": "bool", "a": "list", "o": "dict"}) {
			t.Fatalf("all json types %v", annots)
		}
	})
	_, annots := codexToolsSignatureFromSchema(map[string]any{"properties": map[string]any{"limit": map[string]any{"type": "integer"}, "blob": map[string]any{}}})
	if annots["limit"] != "Optional[int]" || annots["blob"] != "Optional[Any]" {
		t.Fatalf("optional %v", annots)
	}
	if sig, _ := codexToolsSignatureFromSchema(nil); len(sig.Params) != 0 {
		t.Fatalf("nil schema %+v", sig)
	}
}

func TestModuleSurfaceExposedToolsAreSafeSubset(t *testing.T) {
	for _, forbidden := range []string{"terminal", "shell", "read_file", "write_file", "patch", "search_files", "process"} {
		if slices.Contains(codexToolsExposedTools, forbidden) {
			t.Fatalf("%s must not be exposed through the codex callback because codex has a builtin equivalent", forbidden)
		}
	}
}

func TestModuleSurfaceEveryExposedToolIsACoreRegisteredTool(t *testing.T) {
	registered := tools.Default.Names()
	var missing []string
	for _, name := range codexToolsExposedTools {
		if !slices.Contains(registered, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("EXPOSED_TOOLS names no core tool registers: %v", missing)
	}
}

type codexToolsFakeRunner struct{ run func() error }

func (f codexToolsFakeRunner) Run() error { return f.run() }

func withCodexToolsBuild(t *testing.T, build func() (codexToolsRunner, error)) {
	prev := codexToolsBuildServer
	codexToolsBuildServer = build
	prevLog := slog.Default()
	t.Cleanup(func() { codexToolsBuildServer = prev; slog.SetDefault(prevLog) })
	t.Setenv("ICEBATH_QUIET", "")
	t.Setenv("ICEBATH_REDACT_SECRETS", "")
}

func TestMainExitStatuses(t *testing.T) {
	t.Run("test_main_returns_2_when_mcp_unavailable", func(t *testing.T) {
		withCodexToolsBuild(t, func() (codexToolsRunner, error) { return nil, &CodexToolsImportError{Msg: "mcp not installed"} })
		if rc := RunCodexToolsMCPServer([]string{"--verbose"}); rc != 2 {
			t.Fatalf("unavailable rc %d", rc)
		}
	})
	t.Run("test_main_handles_keyboard_interrupt", func(t *testing.T) {
		withCodexToolsBuild(t, func() (codexToolsRunner, error) {
			return codexToolsFakeRunner{func() error { return errCodexToolsInterrupted }}, nil
		})
		if rc := RunCodexToolsMCPServer(nil); rc != 0 {
			t.Fatalf("keyboard interrupt rc %d", rc)
		}
	})
	t.Run("test_main_returns_1_on_runtime_error", func(t *testing.T) {
		withCodexToolsBuild(t, func() (codexToolsRunner, error) {
			return codexToolsFakeRunner{func() error { return errors.New("boom") }}, nil
		})
		if rc := RunCodexToolsMCPServer(nil); rc != 1 {
			t.Fatalf("runtime error rc %d", rc)
		}
	})
	withCodexToolsBuild(t, func() (codexToolsRunner, error) { return nil, &CodexToolsImportError{Msg: "mcp not installed"} })
	codexToolsBuildServer = func() (codexToolsRunner, error) {
		return codexToolsFakeRunner{func() error { panic(errors.New("crash")) }}, nil
	}
	if rc := RunCodexToolsMCPServer(nil); rc != 1 {
		t.Fatalf("panic rc %d", rc)
	}
}

func codexToolsRoundTrip(t *testing.T, s *CodexToolsServer, lines ...string) []any {
	var out strings.Builder
	s.in, s.out = strings.NewReader(strings.Join(lines, "\n")+"\n"), &out
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	var replies []any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		v, err := pyjson.Loads(l)
		if err != nil {
			t.Fatalf("%q %v", l, err)
		}
		replies = append(replies, pyjson.Plain(v))
	}
	return replies
}

func TestCodexToolsServerProtocol(t *testing.T) {
	var calls []map[string]any
	defs := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "web_search", "description": "Search the web", "parameters": map[string]any{"type": "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}, "max_results": map[string]any{"type": "integer"}, "filters": map[string]any{"type": "object"}}, "required": []any{"query"}}}},
		{"type": "function", "function": map[string]any{"name": "skills_list", "parameters": nil}},
		{"type": "function", "function": map[string]any{"name": "terminal", "description": "never exposed"}},
	}
	s := codexToolsAssemble(defs, func(name string, args map[string]any) string {
		calls = append(calls, map[string]any{"name": name, "args": args})
		if name == "skills_list" {
			panic(errors.New("registry exploded"))
		}
		return `{"ok": true}`
	})
	replies := codexToolsRoundTrip(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"go","max_results":"5","filters":"{\"site\": \"go.dev\"}","extra":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"web_search","arguments":{"max_results":2.5}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"skills_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"terminal","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"logging/setLevel","params":{"level":"debug"}}`,
		`{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`not json`,
	)
	if len(replies) != 8 {
		t.Fatalf("replies %v", replies)
	}
	init := replies[0].(map[string]any)["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["instructions"] != codexToolsInstructions || init["serverInfo"].(map[string]any)["name"] != "icebath-tools" ||
		!reflect.DeepEqual(init["capabilities"].(map[string]any)["tools"], map[string]any{"listChanged": false}) {
		t.Fatalf("initialize %v", init)
	}
	list := replies[1].(map[string]any)["result"].(map[string]any)["tools"].([]any)
	if len(list) != 2 {
		t.Fatalf("tools %v", list)
	}
	ws := list[0].(map[string]any)
	wantInput := map[string]any{"properties": map[string]any{
		"filters":     map[string]any{"anyOf": []any{map[string]any{"additionalProperties": true, "type": "object"}, map[string]any{"type": "null"}}, "default": nil, "title": "Filters"},
		"max_results": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}, "default": nil, "title": "Max Results"},
		"query":       map[string]any{"title": "Query", "type": "string"}},
		"required": []any{"query"}, "title": "web_searchArguments", "type": "object"}
	if ws["name"] != "web_search" || ws["description"] != "Search the web" || !reflect.DeepEqual(ws["inputSchema"], wantInput) ||
		ws["outputSchema"].(map[string]any)["title"] != "web_searchOutput" {
		t.Fatalf("web_search listing %v", ws)
	}
	if sl := list[1].(map[string]any); sl["description"] != "Icebath skills_list tool" || !reflect.DeepEqual(sl["inputSchema"], map[string]any{"properties": map[string]any{}, "title": "skills_listArguments", "type": "object"}) {
		t.Fatalf("skills_list listing %v", sl)
	}
	if calls[0]["name"] != "web_search" || !pyrepr.Equal(pyjson.Plain(calls[0]["args"]), map[string]any{"query": "go", "max_results": 5, "filters": map[string]any{"site": "go.dev"}}) {
		t.Fatalf("dispatched %v", calls[0])
	}
	ok := replies[2].(map[string]any)["result"].(map[string]any)
	if !reflect.DeepEqual(ok, map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"ok": true}`}}, "structuredContent": map[string]any{"result": `{"ok": true}`}, "isError": false}) {
		t.Fatalf("call result %v", ok)
	}
	bad := replies[3].(map[string]any)["result"].(map[string]any)
	badText := bad["content"].([]any)[0].(map[string]any)["text"].(string)
	if bad["isError"] != true || badText != "Error executing tool web_search: 2 validation errors for web_searchArguments\n"+
		"max_results\n  Input should be a valid integer, got a number with a fractional part [type=int_from_float, input_value=2.5, input_type=float]\n    For further information visit https://errors.pydantic.dev/2.14/v/int_from_float\n"+
		"query\n  Field required [type=missing, input_value={'max_results': 2.5}, input_type=dict]\n    For further information visit https://errors.pydantic.dev/2.14/v/missing" {
		t.Fatalf("validation %q", badText)
	}
	raised := replies[4].(map[string]any)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if raised != `{"error": "registry exploded", "tool": "skills_list"}` {
		t.Fatalf("raised %v", raised)
	}
	if unknown := replies[5].(map[string]any)["result"].(map[string]any); unknown["isError"] != true || unknown["content"].([]any)[0].(map[string]any)["text"] != "Unknown tool: terminal" {
		t.Fatalf("unknown %v", unknown)
	}
	if e := replies[6].(map[string]any)["error"].(map[string]any); e["code"] != int64(-32601) || e["message"] != "Method not found" {
		t.Fatalf("method not found %v", e)
	}
	if v := replies[7].(map[string]any)["result"].(map[string]any)["protocolVersion"]; v != "2025-11-25" {
		t.Fatalf("fallback version %v", v)
	}
}

func TestCodexToolsServerChildModeOverStdio(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cmd := exec.Command(os.Args[0], CodexToolsMCPServerEntry, "--verbose")
	cmd.Stderr = io.Discard
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	reader := bufio.NewReader(stdout)
	var lines []any
	for range 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("child read %v", err)
		}
		v, _ := pyjson.Loads(line)
		lines = append(lines, pyjson.Plain(v))
	}
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exit %v", err)
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("child did not exit after stdin closed")
	}
	if lines[0].(map[string]any)["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("child initialize %v", lines[0])
	}
	var names []string
	for _, tool := range lines[1].(map[string]any)["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if !slices.Contains(names, "skills_list") || !slices.Contains(names, "skill_view") {
		t.Fatalf("child tools %v", names)
	}
}

func TestCodexToolsPreParseDecodesOnlyContainerJSON(t *testing.T) {
	sig, _ := codexToolsSignatureFromSchema(map[string]any{"properties": map[string]any{"flag": map[string]any{"type": "boolean"}, "items": map[string]any{"type": "array"},
		"n": map[string]any{"type": "integer"}, "q": map[string]any{"type": "string"}, "note": map[string]any{"type": "string"}}, "required": []any{"q"}})
	args := pyjson.NewObject()
	for k, v := range map[string]any{"flag": "true", "items": "[1, 2]", "n": "7", "q": "[3]", "note": "[4]"} {
		args.Set(k, v)
	}
	got := codexToolsPreParse(sig, args)
	if got.Vals["flag"] != "true" || got.Vals["n"] != "7" || got.Vals["q"] != "[3]" || !pyrepr.Equal(got.Vals["items"], []any{1, 2}) || !pyrepr.Equal(got.Vals["note"], []any{4}) {
		t.Fatalf("pre parse %v", got.Vals)
	}
}

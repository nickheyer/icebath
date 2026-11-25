package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

type fixed []Def

func (f fixed) Tools() []Def { return f }

func echo(_ context.Context, call *Call, args struct {
	Text string `json:"text"`
	Fail bool   `json:"fail"`
}) (string, error) {
	if args.Fail {
		return "", errors.New("told to fail")
	}
	return call.Env.Cwd + ":" + args.Text, nil
}

func host(t *testing.T) (*Tools, *Env) {
	t.Helper()
	cfg := config.New(t.TempDir())
	tools := New(cfg)
	nested := new(Registry)
	tools.Register(nested)
	nested.Register(fixed{
		{Name: "zeta", Toolset: "test", Schema: Schema("Echo text", []string{"text"}, "text", Property("string", "What to echo"), "fail", Property("boolean", "Fail instead")), Handler: Bind(echo)},
		{Name: "hidden", Schema: Schema("Never offered", nil), Handler: Bind(echo), Check: func(*Env) bool { return false }},
		{Name: "broken", Handler: Bind(echo)},
	})
	tools.Register(fixed{
		{Name: "alpha", Schema: Schema("Small results", nil), MaxResultChars: 100, Handler: func(context.Context, *Call) string { return strings.Repeat("x", 500) }},
		{Name: "loose", Schema: jsonutil.ObjectOf("description", "Required as a decoded list", "parameters", map[string]any{"type": "object", "required": []any{"need"}}), Handler: func(context.Context, *Call) string { return "" }},
	})
	return tools, &Env{Cwd: "/work", Config: cfg}
}

func TestDefinitionsOfferAvailableToolsByName(t *testing.T) {
	tools, env := host(t)
	definitions := tools.Definitions(env)
	var names []string
	for _, definition := range definitions {
		if definition["type"] != "function" {
			t.Fatalf("definition type = %v, want function", definition["type"])
		}
		names = append(names, definition["function"].(*jsonutil.Object).Value("name").(string))
	}
	if got, want := strings.Join(names, ","), "alpha,loose,zeta"; got != want {
		t.Fatalf("Definitions() names = %s, want %s: sorted, without the tool whose check fails or the one without a schema", got, want)
	}
	encoded, err := json.Marshal(definitions[2])
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"function":{"description":"Echo text","parameters":{"type":"object","properties":{"text":{"type":"string","description":"What to echo"},"fail":{"type":"boolean","description":"Fail instead"}},"required":["text"]},"name":"zeta"},"type":"function"}`
	if string(encoded) != want {
		t.Fatalf("definition = %s\nwant         %s", encoded, want)
	}
	if again := tools.Definitions(env)[2]["function"].(*jsonutil.Object); again.Len() != 3 {
		t.Fatalf("a second Definitions() call saw %d schema keys, want 3: the registered schema must not be changed", again.Len())
	}
}

func TestDispatch(t *testing.T) {
	tools, env := host(t)
	run := func(name, arguments string) string {
		return tools.Dispatch(t.Context(), &Call{Name: name, RawArgs: arguments, Env: env})
	}
	if got := run("zeta", `{"text":"hi"}`); got != "/work:hi" {
		t.Fatalf("Dispatch() = %q, want /work:hi", got)
	}
	cases := []struct{ name, arguments, want string }{
		{"zeta", `{}`, `{"error":"missing argument text"}`},
		{"zeta", `{"text":"hi","fail":true}`, `{"error":"told to fail"}`},
		{"zeta", `nope`, `{"error":"arguments are not a JSON object: invalid character 'o' in literal null (expecting 'u')"}`},
		{"hidden", `{"text":"hi"}`, `{"error":"unknown tool hidden"}`},
		{"missing", `{}`, `{"error":"unknown tool missing"}`},
		{"loose", ``, `{"error":"missing argument need"}`},
		{"loose", `{"need":1}`, "(no output)"},
	}
	for _, c := range cases {
		got := run(c.name, c.arguments)
		if got != c.want {
			t.Errorf("Dispatch(%s, %s) = %s, want %s", c.name, c.arguments, got, c.want)
		}
		if failed := Failed(got); failed != strings.HasPrefix(c.want, `{"error"`) {
			t.Errorf("Failed(%s) = %v", got, failed)
		}
	}
	if got := run("alpha", ""); len(got) > 100 || !strings.Contains(got, "bytes omitted]") {
		t.Fatalf("Dispatch() of a tool with its own limit returned %d bytes: %q", len(got), got)
	}
	if err := env.Config.Set(toolsconfig.OutputMaxBytes, 200); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	tools.Register(fixed{{Name: "big", Schema: Schema("Big results", nil), Handler: func(context.Context, *Call) string { return strings.Repeat("y", 5000) + "\xff" }}})
	if got := run("big", ""); len(got) > 200 || !strings.HasPrefix(got, "yyyy") || !strings.Contains(got, "[4865 bytes omitted]") || !strings.HasSuffix(got, "y�") {
		t.Fatalf("Dispatch() under the global limit returned %d bytes: %q", len(got), got)
	}
}

func TestFailed(t *testing.T) {
	for result, want := range map[string]bool{
		`{"error":"x"}`: true, `{"error":null}`: false, `{"output":"ok"}`: false, `plain text`: false, ``: false, `["error"]`: false, `{"error":""}`: true,
	} {
		if got := Failed(result); got != want {
			t.Errorf("Failed(%q) = %v, want %v", result, got, want)
		}
	}
}

func TestCaptureKeepsHeadAndTail(t *testing.T) {
	capture := NewCapture(164)
	for range 10 {
		capture.Write([]byte("0123456789abcdefghijklmnopqrstuvwxyzABCD"))
	}
	text := capture.String()
	if !strings.HasPrefix(text, "0123456789abcdefghijklmnopqrstuvwxyzABCD0123456789") || !strings.HasSuffix(text, "uvwxyzABCD") || !strings.Contains(text, "\n[300 bytes omitted]\n") {
		t.Fatalf("Capture kept %q", text)
	}
	small := NewCapture(164)
	small.Write([]byte("fits"))
	if small.String() != "fits" || Bound("fits", 164) != "fits" {
		t.Fatalf("a short capture = %q, want it whole", small.String())
	}
}

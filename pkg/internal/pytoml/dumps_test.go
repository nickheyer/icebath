package pytoml

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Ordered object from alternating keys and values
func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func TestDumpsMatchesTomliW(t *testing.T) {
	component := func(sink *pyjson.Object) *pyjson.Object {
		return obj("version", 1, "components", []any{obj("kind", "observability", "enabled", true, "config", obj(
			"version", 4, "atof", obj("enabled", true, "sinks", []any{sink}),
			"opentelemetry", obj("enabled", false, "endpoints", []any{}), "enable_full_payloads", false))})
	}
	head := "version = 1\n\n[[components]]\nkind = \"observability\"\nenabled = true\n\n[components.config]\nversion = 4\nenable_full_payloads = false\n\n[components.config.atof]\nenabled = true\n"
	tail := "\n[components.config.opentelemetry]\nenabled = false\nendpoints = []\n"
	long := "/" + strings.Repeat("d", 60) + "/atof"
	ascii, unicode85, unicode86 := strings.Repeat("e", 85), strings.Repeat("é", 85), strings.Repeat("é", 86)
	cases := []struct {
		doc  *pyjson.Object
		want string
	}{
		{component(obj("type", "file", "filename", "a.jsonl")), head + "sinks = [\n    { type = \"file\", filename = \"a.jsonl\" },\n]\n" + tail},
		{component(obj("type", "file", "output_directory", long, "mode", "append")), head +
			"\n[[components.config.atof.sinks]]\ntype = \"file\"\noutput_directory = \"" + long + "\"\nmode = \"append\"\n" + tail},
		{obj("x", []any{obj("k", ascii)}), "x = [\n    { k = \"" + ascii + "\" },\n]\n"},
		{obj("x", []any{obj("k", unicode85)}), "x = [\n    { k = \"" + unicode85 + "\" },\n]\n"},
		{obj("x", []any{obj("k", unicode86)}), "[[x]]\nk = \"" + unicode86 + "\"\n"},
		{obj("q", "a\"b\\c\td\n", "dotted.key", obj()), "q = \"a\\\"b\\\\c\td\\n\"\n\n[\"dotted.key\"]\n"},
	}
	for i, c := range cases {
		got, err := Dumps(c.doc)
		if err != nil || got != c.want {
			t.Fatalf("case %d err %v\n%s\nwant\n%s", i, err, got, c.want)
		}
	}
	if _, err := Dumps(obj("none", nil)); err == nil || err.Error() != "Object of type NoneType is not TOML serializable" {
		t.Fatalf("none err %v", err)
	}
	parsed, err := Loads(mustDumps(t, component(obj("type", "file"))))
	if err != nil || parsed["components"].([]any)[0].(map[string]any)["config"].(map[string]any)["atof"].(map[string]any)["sinks"].([]any)[0].(map[string]any)["type"] != "file" {
		t.Fatalf("round trip %v %v", parsed, err)
	}
}

func mustDumps(t *testing.T, doc *pyjson.Object) string {
	t.Helper()
	text, err := Dumps(doc)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

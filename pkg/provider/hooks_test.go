package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

type nested struct {
	Name string `json:"name"`
}

type namespace struct {
	ID     string `json:"id"`
	Value  int    `json:"value"`
	Nested nested `json:"nested"`
}

func TestHookJSONableNormalizesStructs(t *testing.T) {
	got := SanitizeHookPayload(&namespace{ID: "call_1", Value: 42, Nested: nested{Name: "x"}})
	raw, _ := json.Marshal(got)
	if string(raw) != `{"id":"call_1","nested":{"name":"x"},"value":42}` {
		t.Fatalf("payload %s", raw)
	}
}

func TestHookJSONableRedactsSensitiveKeys(t *testing.T) {
	got := HookJSONable(map[string]any{"api_key": "sk-secret", "Authorization": "Bearer x", "openai_api_key": "y", "model": "m", "set-cookie": "c"}, HookJSONCaps{}).(map[string]any)
	for _, k := range []string{"api_key", "Authorization", "openai_api_key", "set-cookie"} {
		if got[k] != "<redacted>" {
			t.Fatalf("%s not redacted: %v", k, got[k])
		}
	}
	if got["model"] != "m" {
		t.Fatal("plain key kept")
	}
	if !IsSensitiveHookKey("Proxy-Authorization") || IsSensitiveHookKey("api_key_hint") {
		t.Fatal("sensitive key rules")
	}
}

func TestHookJSONableBoundsStringsSequencesAndDepth(t *testing.T) {
	long := strings.Repeat("a", 9000)
	if got := HookJSONable(long, HookJSONCaps{}).(string); !strings.HasSuffix(got, "...[truncated 1000 chars]") || len(got) != 8000+len("...[truncated 1000 chars]") {
		t.Fatalf("string cap %d", len(got))
	}
	items := make([]any, 250)
	for i := range items {
		items[i] = i
	}
	seq := HookJSONable(items, HookJSONCaps{}).([]any)
	if len(seq) != 201 || seq[200].(map[string]any)["_truncated_items"] != 50 {
		t.Fatalf("sequence cap %d %v", len(seq), seq[200])
	}
	deep := any("leaf")
	for i := 0; i < 12; i++ {
		deep = map[string]any{"k": deep}
	}
	raw, _ := json.Marshal(HookJSONable(deep, HookJSONCaps{}))
	if !strings.Contains(string(raw), "depth limit") {
		t.Fatalf("depth limit missing: %s", raw)
	}
	if got := HookJSONable([]byte{1, 2, 3}, HookJSONCaps{}); got != "<3 bytes>" {
		t.Fatalf("bytes %v", got)
	}
}

func TestSanitizeHookPayloadRespectsCap(t *testing.T) {
	t.Setenv(HookPayloadMaxCharsEnv, "1500")
	if HookPayloadMaxChars() != 1500 {
		t.Fatal("env cap")
	}
	t.Setenv(HookPayloadMaxCharsEnv, "10")
	if HookPayloadMaxChars() != 1000 {
		t.Fatal("floor 1000")
	}
	t.Setenv(HookPayloadMaxCharsEnv, "junk")
	if HookPayloadMaxChars() != 50000 {
		t.Fatal("junk default")
	}
	t.Setenv(HookPayloadMaxCharsEnv, "1200")
	big := map[string]any{"text": strings.Repeat("x", 5000), "list": make([]any, 100)}
	got := SanitizeHookPayload(big)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("payload %T", got)
	}
	if m["_truncated"] != true || m["original_type"] != "object" {
		t.Fatalf("truncated payload %v", m)
	}
	if preview, _ := m["preview"].(string); len([]rune(preview)) != 1200 {
		t.Fatalf("preview length %d", len([]rune(preview)))
	}
	small := SanitizeHookPayload(map[string]any{"a": 1})
	if small.(map[string]any)["a"] != 1 {
		t.Fatal("small payload returned as is")
	}
}

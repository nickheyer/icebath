package agent

import (
	"os"
	"reflect"
	"testing"
)

func TestSalvageTextParts(t *testing.T) {
	content := []any{" a ", map[string]any{"type": "text", "text": " b "}, map[string]any{"type": "other", "text": "c"}, map[string]any{"type": "input_text", "text": nil}, 7}
	if got := salvageTextParts(content, false); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("typed salvage %#v", got)
	}
	if got := salvageTextParts(content, true); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("any dict salvage %#v", got)
	}
}

func TestTryStripImagePartsFromToolMessages(t *testing.T) {
	a := &AIAgent{provider: " MiMo ", model: " m1 "}
	msgs := []any{
		map[string]any{"role": "tool", "content": []any{map[string]any{"type": "image_url"}, map[string]any{"type": "text", "text": "seen"}}},
		map[string]any{"role": "tool", "content": []any{map[string]any{"type": "input_image"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url"}}},
	}
	if !a.tryStripImagePartsFromToolMessages(msgs, true) {
		t.Fatal("nothing stripped")
	}
	if msgs[0].(map[string]any)["content"] != "seen" || msgs[1].(map[string]any)["content"] != "[image content removed — provider does not accept list-type tool message content]" {
		t.Fatalf("tool rows %#v", msgs[:2])
	}
	if _, ok := msgs[2].(map[string]any)["content"].([]any); !ok || !a.noListToolContentModels[[2]string{"mimo", "m1"}] {
		t.Fatal("user row touched or route not remembered")
	}
	b := &AIAgent{provider: "x"}
	b.tryStripImagePartsFromToolMessages([]any{}, true)
	if len(b.noListToolContentModels) != 0 || b.tryStripImagePartsFromToolMessages("nope", false) {
		t.Fatal("blank model recorded or non list accepted")
	}
}

func TestQwenPrepareChatMessages(t *testing.T) {
	a := &AIAgent{}
	src := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": []any{"hi", map[string]any{"type": "text", "text": "x"}, 3}},
		"junk",
	}
	out := a.qwenPrepareChatMessages(src)
	want := []any{
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "sys", "cache_control": map[string]any{"type": "ephemeral"}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}, map[string]any{"type": "text", "text": "x"}}},
		"junk",
	}
	if !reflect.DeepEqual(out, want) || src[0].(map[string]any)["content"] != "sys" {
		t.Fatalf("qwen shaping %#v", out)
	}
}

func TestAnthropicPreserveDots(t *testing.T) {
	cases := []struct {
		provider, base string
		want           bool
	}{
		{"Bedrock", "", true},
		{"anthropic", "https://api.anthropic.com", false},
		{"custom", "https://dashscope-intl.aliyuncs.com/compatible-mode", true},
		{"custom", "https://opencode.ai/zen/v1", true},
		{"custom", "https://opencode.ai/go/v1", false},
		{"", "https://bedrock-runtime.us-east-1.amazonaws.com", true},
		{"", "https://aiplatform.googleapis.com/v1beta1/projects/p", true},
		{"", "https://us-central1-aiplatform.googleapis.com/v1", false},
	}
	for _, c := range cases {
		if got := (&AIAgent{provider: c.provider, baseURL: c.base}).anthropicPreserveDots(); got != c.want {
			t.Fatalf("%s %s: %v", c.provider, c.base, got)
		}
	}
	if !(&AIAgent{baseURLLower: "https://portal.qwen.ai/v1"}).isQwenPortal() {
		t.Fatal("qwen portal")
	}
}

func TestMaterializeDataURLForVision(t *testing.T) {
	path, err := materializeDataURLForVision("data:image/png;base64,aGVsbG8=")
	if err != nil || path == "" {
		t.Fatalf("materialize %q %v", path, err)
	}
	defer os.Remove(path)
	data, _ := os.ReadFile(path)
	if string(data) != "hello" || path[len(path)-4:] != ".png" {
		t.Fatalf("payload %q at %s", data, path)
	}
	if _, err := materializeDataURLForVision("data:image/png;base64,aGVsbG8"); err == nil || err.Error() != "Incorrect padding" {
		t.Fatalf("bad padding %v", err)
	}
	p2, err := materializeDataURLForVision("data:text/plain;base64,")
	if err != nil || p2[len(p2)-4:] != ".jpg" {
		t.Fatalf("non image mime %q %v", p2, err)
	}
	os.Remove(p2)
}

func TestToolResultContentPassThrough(t *testing.T) {
	a := &AIAgent{}
	if got := a.toolResultContentForActiveModel(nil, "x", "plain"); got != "plain" {
		t.Fatalf("plain %#v", got)
	}
	textOnly := map[string]any{"_multimodal": true, "content": []any{map[string]any{"type": "text", "text": "t"}}}
	if got := a.toolResultContentForActiveModel(nil, "x", textOnly); !reflect.DeepEqual(got, textOnly["content"]) {
		t.Fatalf("text only %#v", got)
	}
}

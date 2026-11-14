package provider

import (
	"reflect"
	"testing"
)

func TestQwenPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	p := bundledPluginProfile(t, "qwen-oauth")
	assertProfileFields(t, p, map[string]any{"name": "qwen-oauth", "aliases": []string{"qwen", "qwen-portal", "qwen-cli"},
		"env_vars": []string{"QWEN_API_KEY"}, "base_url": "https://portal.qwen.ai/v1", "auth_type": "oauth_external",
		"default_max_tokens": 65536, "display_name": "", "base_url_env_var": ""})
	if got := p.BuildExtraBody("s", map[string]any{}); !reflect.DeepEqual(got, map[string]any{"vl_high_resolution_images": true}) {
		t.Fatalf("extra body = %v", got)
	}
	md := map[string]any{"sessionId": "abc"}
	if extra, top := p.BuildAPIKwargsExtras(nil, map[string]any{"qwen_session_metadata": md}); len(extra) != 0 || !reflect.DeepEqual(top, map[string]any{"metadata": md}) {
		t.Fatalf("kwargs = %v %v", extra, top)
	}
	if _, top := p.BuildAPIKwargsExtras(nil, map[string]any{"qwen_session_metadata": map[string]any{}}); len(top) != 0 {
		t.Fatalf("empty metadata sent = %v", top)
	}
}

func TestQwenPrepareMessagesProtectsNestedImageURLRetryMutation(t *testing.T) {
	p := bundledPluginProfile(t, "qwen-oauth")
	imageURL := map[string]any{"url": "data:image/png;base64,original"}
	msgs := []map[string]any{
		{"role": "system", "content": "Be helpful"},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "see image"}, map[string]any{"type": "image_url", "image_url": imageURL}}},
	}
	out := p.PrepareMessages(msgs)
	if reflect.ValueOf(out[1]).Pointer() == reflect.ValueOf(msgs[1]).Pointer() {
		t.Fatal("user message not copied")
	}
	out[1]["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"] = "data:image/png;base64,shrunk"
	if imageURL["url"] != "data:image/png;base64,original" || msgs[1]["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,original" {
		t.Fatal("retry mutation reached the caller's image url")
	}
	want := []any{map[string]any{"type": "text", "text": "Be helpful", "cache_control": map[string]any{"type": "ephemeral"}}}
	if !reflect.DeepEqual(out[0]["content"], want) || msgs[0]["content"] != "Be helpful" {
		t.Fatalf("system = %v", out[0])
	}
}

func TestQwenPrepareMessagesCopyOnWrite(t *testing.T) {
	p := bundledPluginProfile(t, "qwen-oauth")
	if got := p.PrepareMessages(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
	plain := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "x"}}}
	mixed := map[string]any{"role": "user", "content": []any{"hi", 7, map[string]any{"type": "text", "text": "y"}}}
	junk := map[string]any{"role": "user", "content": []any{7}}
	out := p.PrepareMessages([]map[string]any{plain, mixed, junk})
	if reflect.ValueOf(out[0]).Pointer() != reflect.ValueOf(plain).Pointer() || reflect.ValueOf(out[2]).Pointer() != reflect.ValueOf(junk).Pointer() {
		t.Fatal("untouched messages copied")
	}
	if !reflect.DeepEqual(out[1]["content"], []any{map[string]any{"type": "text", "text": "hi"}, map[string]any{"type": "text", "text": "y"}}) {
		t.Fatalf("mixed = %v", out[1]["content"])
	}
}

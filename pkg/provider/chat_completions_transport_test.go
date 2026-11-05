package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Chat completions transport kwargs for a user ping, failing the test on a build error
func ccTransportKwargs(t *testing.T, model string, messages []map[string]any, params map[string]any) map[string]any {
	t.Helper()
	if messages == nil {
		messages = []map[string]any{{"role": "user", "content": "ping"}}
	}
	kw, err := GetTransport(string(ModeChatCompletions)).BuildKwargs(model, messages, nil, params)
	if err != nil {
		t.Fatal(err)
	}
	return kw
}

// Extra body of built kwargs, empty when absent
func ccExtraBody(kw map[string]any) map[string]any {
	eb, _ := kw["extra_body"].(map[string]any)
	if eb == nil {
		return map[string]any{}
	}
	return eb
}

// Effort expectation that accepts any wire value
var ccAnyEffort any = struct{ any bool }{true}

func ccMaxCompletionTokens(n int) map[string]any { return map[string]any{"max_completion_tokens": n} }

func TestProfileWiringNvidiaMaxTokensMatch(t *testing.T) {
	p := bundledPluginProfile(t, "nvidia")
	kw := ccTransportKwargs(t, "nvidia/nemotron", nil, map[string]any{"provider_profile": p, "max_tokens_param_fn": ccMaxCompletionTokens})
	if kw["max_completion_tokens"] != p.DefaultMaxTokens || p.DefaultMaxTokens == 0 {
		t.Fatalf("max_completion_tokens %v, default %d", kw["max_completion_tokens"], p.DefaultMaxTokens)
	}
}

func TestProfileWiringKimiTemperatureOmitted(t *testing.T) {
	coding := bundledPluginProfile(t, "kimi-coding")
	kimi, _ := GetProviderProfile(context.Background(), "kimi")
	legacy := ccTransportKwargs(t, "kimi-k2", nil, map[string]any{"provider_profile": coding, "omit_temperature": true})
	profile := ccTransportKwargs(t, "kimi-k2", nil, map[string]any{"provider_profile": kimi})
	if _, ok := legacy["temperature"]; ok {
		t.Fatal(legacy)
	}
	if _, ok := profile["temperature"]; ok {
		t.Fatal(profile)
	}
}

func TestProfileWiringDeveloperRoleSwap(t *testing.T) {
	msgs := func() []map[string]any {
		return []map[string]any{{"role": "system", "content": "Be helpful"}, {"role": "user", "content": "hi"}}
	}
	if kw := ccTransportKwargs(t, "gpt-5.4", msgs(), map[string]any{}); kw["messages"].([]map[string]any)[0]["role"] != "developer" {
		t.Fatal("legacy path kept the system role")
	}
	p := bundledPluginProfile(t, "openrouter")
	if kw := ccTransportKwargs(t, "gpt-5.4", msgs(), map[string]any{"provider_profile": p}); kw["messages"].([]map[string]any)[0]["role"] != "developer" {
		t.Fatal("profile path kept the system role")
	}
}

func TestProfileWiringRequestOverrides(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	kw := ccTransportKwargs(t, "gpt-5.4", nil, map[string]any{"provider_profile": p, "request_overrides": map[string]any{"extra_body": map[string]any{"custom_key": "custom_val"}}})
	if ccExtraBody(kw)["custom_key"] != "custom_val" {
		t.Fatal(kw)
	}
	kw = ccTransportKwargs(t, "gpt-5.4", nil, map[string]any{"provider_profile": p, "request_overrides": map[string]any{"top_p": 0.9}})
	if kw["top_p"] != 0.9 {
		t.Fatal(kw)
	}
}

func TestTransportParityProfiles(t *testing.T) {
	nvidia := bundledPluginProfile(t, "nvidia")
	kw := ccTransportKwargs(t, "nvidia/llama-3.1-nemotron-70b-instruct", nil, map[string]any{"max_tokens": 4096, "max_tokens_param_fn": ccMaxCompletionTokens, "provider_profile": nvidia})
	if kw["max_completion_tokens"] != 4096 {
		t.Fatalf("nvidia user max_tokens %v", kw["max_completion_tokens"])
	}
	kimi, _ := GetProviderProfile(context.Background(), "kimi-coding")
	kw = ccTransportKwargs(t, "kimi-k2", nil, map[string]any{"provider_profile": kimi, "reasoning_config": map[string]any{"enabled": true, "effort": "high"}})
	eb := ccExtraBody(kw)
	if kw["reasoning_effort"] != "high" || eb["thinking"] != nil || eb["reasoning_effort"] != nil {
		t.Fatalf("kimi %v", kw)
	}
	openrouter, _ := GetProviderProfile(context.Background(), "openrouter")
	prefs := map[string]any{"allow": []any{"anthropic"}, "sort": "price"}
	kw = ccTransportKwargs(t, "anthropic/claude-sonnet-4.6", nil, map[string]any{"provider_profile": openrouter, "provider_preferences": prefs})
	if !reflect.DeepEqual(ccExtraBody(kw)["provider"], prefs) {
		t.Fatalf("openrouter prefs %v", kw)
	}
	nous, _ := GetProviderProfile(context.Background(), "nous")
	kw = ccTransportKwargs(t, "hermes-3-llama-3.1-405b", nil, map[string]any{"provider_profile": nous})
	if !reflect.DeepEqual(ccExtraBody(kw)["tags"], ccNousTags()) {
		t.Fatalf("nous tags %v", ccExtraBody(kw)["tags"])
	}
	qwen, _ := GetProviderProfile(context.Background(), "qwen-oauth")
	kw = ccTransportKwargs(t, "qwen3.5-plus", nil, map[string]any{"provider_profile": qwen})
	if ccExtraBody(kw)["vl_high_resolution_images"] != true {
		t.Fatalf("qwen vl %v", kw)
	}
	meta := map[string]any{"sessionId": "s123", "promptId": "p456"}
	kw = ccTransportKwargs(t, "qwen3.5-plus", nil, map[string]any{"provider_profile": qwen, "qwen_session_metadata": meta})
	if !reflect.DeepEqual(kw["metadata"], meta) || ccExtraBody(kw)["metadata"] != nil {
		t.Fatalf("qwen metadata %v", kw)
	}
}

func TestTransportParityCustomOllama(t *testing.T) {
	custom := bundledPluginProfile(t, "custom")
	kw := ccTransportKwargs(t, "llama3.1", nil, map[string]any{"provider_profile": custom, "ollama_num_ctx": 131072})
	if opts, _ := ccExtraBody(kw)["options"].(map[string]any); opts["num_ctx"] != 131072 {
		t.Fatalf("num_ctx %v", kw)
	}
	off := map[string]any{"enabled": false, "effort": "none"}
	kw = ccTransportKwargs(t, "qwen3:72b", nil, map[string]any{"provider_profile": custom, "reasoning_config": off, "base_url": "http://127.0.0.1:11434/v1"})
	if ccExtraBody(kw)["think"] != false {
		t.Fatalf("think %v", kw)
	}
	kw = ccTransportKwargs(t, "mistral-small-latest", nil, map[string]any{"provider_profile": custom, "reasoning_config": off, "base_url": "https://api.mistral.ai/v1"})
	if ccExtraBody(kw)["think"] != nil || kw["reasoning_effort"] != "none" {
		t.Fatalf("mistral custom %v", kw)
	}
}

func TestE2EWiringNvidiaToolMessagesDropNameFields(t *testing.T) {
	p := bundledPluginProfile(t, "nvidia")
	msgs := []map[string]any{
		{"role": "user", "content": "run a command"},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}},
		{"role": "tool", "name": "terminal", "tool_name": "terminal", "tool_call_id": "call_1", "content": "ok"},
	}
	fn := func(x any) map[string]any {
		if pyTruthy(x) {
			return map[string]any{"max_tokens": x}
		}
		return map[string]any{}
	}
	kw := ccTransportKwargs(t, "mistralai/mistral-large-3-675b-instruct-2512", msgs, map[string]any{
		"provider_profile": p, "max_tokens": nil, "max_tokens_param_fn": fn, "timeout": 300, "reasoning_config": nil,
		"request_overrides": nil, "session_id": "test", "ollama_num_ctx": nil,
	})
	wire := kw["messages"].([]map[string]any)
	callID := wire[1]["tool_calls"].([]any)[0].(map[string]any)["id"]
	if want := map[string]any{"role": "tool", "tool_call_id": callID, "content": "ok"}; !reflect.DeepEqual(wire[2], want) {
		t.Fatalf("tool message %v", wire[2])
	}
	if msgs[2]["name"] != "terminal" || msgs[2]["tool_name"] != "terminal" {
		t.Fatal("stored history mutated")
	}
}

func TestE2EWiringDeepSeekNoForcedMaxTokens(t *testing.T) {
	p := bundledPluginProfile(t, "deepseek")
	fn := func(x any) map[string]any {
		if pyTruthy(x) {
			return map[string]any{"max_tokens": x}
		}
		return map[string]any{}
	}
	kw := ccTransportKwargs(t, "deepseek-chat", []map[string]any{{"role": "user", "content": "hi"}}, map[string]any{
		"provider_profile": p, "max_tokens": nil, "max_tokens_param_fn": fn, "timeout": 300, "session_id": "test",
	})
	if kw["model"] != "deepseek-chat" || kw["max_tokens"] != nil {
		t.Fatal(kw)
	}
}

func TestNamedCustomRouteKeepsFinalReasoningEffort(t *testing.T) {
	bundledPluginProfile(t, "custom")
	for _, effort := range []string{"low", "medium", "high"} {
		var outputs []map[string]any
		for _, provider := range []string{"custom", "custom:fixture"} {
			p, ok := GetProviderProfile(context.Background(), provider)
			if !ok {
				t.Fatalf("no profile for %s", provider)
			}
			outputs = append(outputs, ccTransportKwargs(t, "fixture-model", []map[string]any{{"role": "user", "content": "fixture"}}, map[string]any{
				"provider_profile": p, "base_url": "http://127.0.0.1:1/v1", "reasoning_config": map[string]any{"enabled": true, "effort": effort},
			}))
		}
		if outputs[0]["reasoning_effort"] != effort || !reflect.DeepEqual(outputs[0], outputs[1]) {
			t.Fatalf("effort %s: %v vs %v", effort, outputs[0], outputs[1])
		}
	}
}

func TestProfileTransportReasoningWire(t *testing.T) {
	enabled := func(effort string) map[string]any { return map[string]any{"enabled": true, "effort": effort} }
	cases := []struct {
		name, profile, model, baseURL, providerName string
		reasoning                                   any
		supports                                    any
		effort                                      any
		extraBody                                   map[string]any
		absentExtra                                 []string
	}{
		{name: "ollama cloud xhigh", profile: "ollama-cloud", model: "deepseek-v4-pro:cloud", baseURL: "https://ollama.com/v1", providerName: "ollama-cloud", reasoning: enabled("xhigh"), supports: true, effort: "max", absentExtra: []string{"reasoning"}},
		{name: "deepseek live wire", profile: "deepseek", model: "deepseek-v4-pro", baseURL: "https://api.deepseek.com/v1", providerName: "deepseek", reasoning: enabled("high"), effort: "high", extraBody: map[string]any{"thinking": map[string]any{"type": "enabled"}}},
		{name: "deepseek v3 omits thinking", profile: "deepseek", model: "deepseek-v3-0324", baseURL: "https://api.deepseek.com/v1", providerName: "deepseek", reasoning: enabled("high"), absentExtra: []string{"thinking"}},
		{name: "deepseek unset keeps thinking", profile: "deepseek", model: "deepseek-v4.1-flash-expires-on-0910", baseURL: "https://api.deepseek.com/v1", providerName: "deepseek", extraBody: map[string]any{"thinking": map[string]any{"type": "enabled"}}},
		{name: "deepseek effort none", profile: "deepseek", model: "deepseek-v4.1-flash-expires-on-0910", baseURL: "https://api.deepseek.com/v1", providerName: "deepseek", reasoning: map[string]any{"effort": "none"}, extraBody: map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{name: "deepseek cli none", profile: "deepseek", model: "deepseek-v4-pro", baseURL: "https://api.deepseek.com/v1", providerName: "deepseek", reasoning: ParseReasoningEffort("none"), extraBody: map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{name: "opencode zen max", profile: "opencode-zen", model: "x-preview-f-free", baseURL: "https://opencode.ai/zen/v1", reasoning: enabled("max"), effort: "max", extraBody: map[string]any{}},
		{name: "opencode go flash", profile: "opencode-go", model: "deepseek-flash", baseURL: "https://opencode.ai/zen/go/v1", reasoning: enabled("max"), effort: "max", extraBody: map[string]any{}},
		{name: "opencode go kimi", profile: "opencode-go", model: "kimi-k2.6", baseURL: "https://opencode.ai/zen/go/v1", reasoning: enabled("high"), effort: "high", extraBody: map[string]any{}},
		{name: "opencode go deepseek", profile: "opencode-go", model: "deepseek-v4-pro", baseURL: "https://opencode.ai/zen/go/v1", reasoning: enabled("high"), effort: "high", extraBody: map[string]any{}},
		{name: "deepinfra on", profile: "deepinfra", model: "deepseek-ai/DeepSeek-V4.1-Flash", providerName: "deepinfra", reasoning: enabled("high"), supports: false, effort: "high", absentExtra: []string{"reasoning"}},
		{name: "deepinfra off", profile: "deepinfra", model: "zai-org/GLM-4.6", providerName: "deepinfra", reasoning: map[string]any{"enabled": false}, supports: false, effort: "none"},
		{name: "groq high", profile: "custom", model: "qwen/qwen3.6-27b", baseURL: "https://api.groq.com/openai/v1", providerName: "custom", reasoning: enabled("high"), effort: "default", absentExtra: []string{"think", "reasoning"}},
		{name: "groq off", profile: "custom", model: "qwen/qwen3.6-27b", baseURL: "https://api.groq.com/openai/v1", providerName: "custom", reasoning: map[string]any{"enabled": false, "effort": "medium"}, effort: "none", absentExtra: []string{"think", "reasoning"}},
		{name: "minimax base url", profile: "minimax", model: "MiniMax-M3", baseURL: "https://api.minimax.io/v1", reasoning: enabled("medium"), effort: ccAnyEffort, extraBody: map[string]any{"reasoning_split": true, "thinking": map[string]any{"type": "adaptive"}}},
		{name: "zai glm 5.2 max", profile: "zai", model: "glm-5.2", baseURL: "https://api.z.ai/api/paas/v4", providerName: "zai", reasoning: enabled("max"), effort: "max"},
		{name: "nebius low", profile: "nebius-token-factory", model: "deepseek-ai/DeepSeek-V4-Pro", baseURL: "https://api.tokenfactory.nebius.com/v1", providerName: "nebius-token-factory", reasoning: enabled("low"), effort: "low", extraBody: map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := bundledPluginProfile(t, tc.profile)
			params := map[string]any{"provider_profile": p, "reasoning_config": tc.reasoning}
			if tc.baseURL != "" {
				params["base_url"] = tc.baseURL
			}
			if tc.providerName != "" {
				params["provider_name"] = tc.providerName
			}
			if tc.supports != nil {
				params["supports_reasoning"] = tc.supports
			}
			kw := ccTransportKwargs(t, tc.model, nil, params)
			if kw["model"] != tc.model || (tc.effort != ccAnyEffort && kw["reasoning_effort"] != tc.effort) {
				t.Fatalf("reasoning_effort %v want %v in %v", kw["reasoning_effort"], tc.effort, kw)
			}
			if tc.extraBody != nil && !reflect.DeepEqual(ccExtraBody(kw), tc.extraBody) {
				t.Fatalf("extra_body %v want %v", ccExtraBody(kw), tc.extraBody)
			}
			if _, has := kw["extra_body"]; has && tc.extraBody != nil && len(tc.extraBody) == 0 {
				t.Fatalf("extra_body present %v", kw)
			}
			for _, k := range tc.absentExtra {
				if _, has := ccExtraBody(kw)[k]; has {
					t.Fatalf("extra_body carries %s: %v", k, kw)
				}
			}
		})
	}
}

func TestZaiFullKwargsIntegration(t *testing.T) {
	p := bundledPluginProfile(t, "zai")
	kw := ccTransportKwargs(t, "glm-5.2", nil, map[string]any{"provider_profile": p, "reasoning_config": map[string]any{"enabled": true, "effort": "max"}, "base_url": "https://api.z.ai/api/paas/v4", "provider_name": "zai"})
	if kw["reasoning_effort"] != "max" || !reflect.DeepEqual(ccExtraBody(kw)["thinking"], map[string]any{"type": "enabled", "clear_thinking": false}) {
		t.Fatal(kw)
	}
	cases := []struct {
		model     string
		reasoning any
		thinking  string
		effort    any
	}{
		{"glm-5.3-flash", map[string]any{"enabled": false}, "enabled", "low"},
		{"glm-5.2", map[string]any{"enabled": false}, "disabled", nil},
		{"glm-5.3", nil, "", nil},
	}
	for _, tc := range cases {
		kw := ccTransportKwargs(t, tc.model, nil, map[string]any{"provider_profile": p, "reasoning_config": tc.reasoning, "base_url": "https://open.bigmodel.cn/api/paas/v4", "provider_name": "zai"})
		var want any
		switch tc.thinking {
		case "enabled":
			want = map[string]any{"type": "enabled", "clear_thinking": false}
		case "disabled":
			want = map[string]any{"type": "disabled"}
		}
		if kw["reasoning_effort"] != tc.effort || !reflect.DeepEqual(ccExtraBody(kw)["thinking"], want) {
			t.Fatalf("%s: %v", tc.model, kw)
		}
	}
}

// Wire messages of the transport for dict messages and kwargs
func ccConvert(t *testing.T, msgs []map[string]any, kwargs map[string]any) []map[string]any {
	t.Helper()
	out, err := GetTransport(string(ModeChatCompletions)).ConvertMessages(msgs, kwargs)
	if err != nil {
		t.Fatal(err)
	}
	return out.([]map[string]any)
}

func ccMaxTokens(n int) map[string]any { return map[string]any{"max_tokens": n} }

func TestChatCompletionsConvertMessagesStripsSidecars(t *testing.T) {
	r := ccConvert(t, []map[string]any{{"role": "user", "content": "hi", "message_id": "adm-123"}}, nil)
	if _, has := r[0]["message_id"]; has || r[0]["content"] != "hi" {
		t.Fatal(r)
	}
	msgs := []map[string]any{{"role": "user", "content": "hi", "timestamp": 1781976577.0}}
	r = ccConvert(t, msgs, nil)
	if _, has := r[0]["timestamp"]; has || r[0]["content"] != "hi" || r[0]["role"] != "user" || msgs[0]["timestamp"] != 1781976577.0 {
		t.Fatal(r)
	}
	tcs := []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}
	msgs = []map[string]any{
		{"role": "user", "content": "use a tool"},
		{"role": "assistant", "content": nil, "tool_calls": tcs, "anthropic_content_blocks": []any{map[string]any{"thinking": "signed"}},
			"bedrock_content_blocks": []any{map[string]any{"reasoningContent": map[string]any{"redactedContentBase64": "cmVhc29uaW5n"}}}},
	}
	r = ccConvert(t, msgs, map[string]any{"model": "gpt-4o"})
	_, hasA := r[1]["anthropic_content_blocks"]
	_, hasB := r[1]["bedrock_content_blocks"]
	if hasA || hasB || !reflect.DeepEqual(r[1]["tool_calls"], msgs[1]["tool_calls"]) || msgs[1]["anthropic_content_blocks"] == nil || msgs[1]["bedrock_content_blocks"] == nil {
		t.Fatal(r)
	}
}

func TestChatCompletionsConvertMessagesNameAndScaffolding(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "hi", "name": "sylvain"}, {"role": "tool", "tool_call_id": "call_1", "content": "ok", "name": "execute_code"}}
	r := ccConvert(t, msgs, nil)
	if !reflect.DeepEqual(r[1], map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"}) || r[0]["name"] != "sylvain" || msgs[1]["name"] != "execute_code" {
		t.Fatal(r)
	}
	clean := []map[string]any{{"role": "user", "content": "hi"}}
	if r := ccConvert(t, clean, nil); &r[0] != &clean[0] {
		t.Fatal("clean history was copied")
	}
	msgs = []map[string]any{
		{"role": "user", "content": "run the task"},
		{"role": "assistant", "content": "(empty)", "_empty_recovery_synthetic": true},
		{"role": "user", "content": "continue", "_empty_recovery_synthetic": true},
		{"role": "assistant", "content": "done", "_thinking_prefill": true, "_empty_terminal_sentinel": true},
	}
	r = ccConvert(t, msgs, nil)
	for _, m := range r {
		for k := range m {
			if k[0] == '_' {
				t.Fatal(m)
			}
		}
	}
	if r[1]["content"] != "(empty)" || r[2]["content"] != "continue" || msgs[1]["_empty_recovery_synthetic"] != true {
		t.Fatal(r)
	}
}

func TestChatCompletionsConvertMessagesCopyOnWriteForDirtyHistory(t *testing.T) {
	clean := map[string]any{"id": "call_clean", "type": "function", "function": map[string]any{"name": "safe", "arguments": "{}"}}
	dirty := map[string]any{"id": "call_dirty", "call_id": "call_dirty", "response_item_id": "fc_dirty",
		"extra_content": map[string]any{"google": map[string]any{"thought_signature": "SIG"}}, "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}
	tcs := []any{clean, dirty}
	msgs := []map[string]any{{"role": "user", "content": "hi", "metadata": map[string]any{"large": []any{"shared"}}}, {"role": "assistant", "content": "ok", "tool_calls": tcs}}
	r := ccConvert(t, msgs, map[string]any{"model": "gpt-4o"})
	if &r[0] == &msgs[0] || reflect.ValueOf(r[0]).Pointer() != reflect.ValueOf(msgs[0]).Pointer() || reflect.ValueOf(r[1]).Pointer() == reflect.ValueOf(msgs[1]).Pointer() {
		t.Fatal("copy on write identities wrong")
	}
	out := r[1]["tool_calls"].([]any)
	if &out[0] == &tcs[0] || reflect.ValueOf(out[0]).Pointer() != reflect.ValueOf(clean).Pointer() || reflect.ValueOf(out[1]).Pointer() == reflect.ValueOf(dirty).Pointer() {
		t.Fatal("tool call identities wrong")
	}
	for _, k := range []string{"call_id", "response_item_id", "extra_content"} {
		if _, has := out[1].(map[string]any)[k]; has {
			t.Fatalf("%s kept", k)
		}
	}
	if dirty["call_id"] == nil || dirty["extra_content"] == nil {
		t.Fatal("stored tool call mutated")
	}
}

func TestChatCompletionsBuildKwargsProfiles(t *testing.T) {
	hi := []map[string]any{{"role": "user", "content": "Hi"}}
	openrouter := bundledPluginProfile(t, "openrouter")
	kw := ccTransportKwargs(t, "gpt-4o", hi, map[string]any{"provider_profile": openrouter, "provider_preferences": map[string]any{"only": []any{"openai"}}})
	if !reflect.DeepEqual(ccExtraBody(kw)["provider"], map[string]any{"only": []any{"openai"}}) {
		t.Fatal(kw)
	}
	nous, _ := GetProviderProfile(context.Background(), "nous")
	if kw := ccTransportKwargs(t, "gpt-4o", hi, map[string]any{"provider_profile": nous}); !reflect.DeepEqual(ccExtraBody(kw)["tags"], ccNousTags()) {
		t.Fatal(kw)
	}
	if kw := ccTransportKwargs(t, "gpt-4o", hi, map[string]any{"provider_profile": nous, "supports_reasoning": true, "reasoning_config": map[string]any{"enabled": false}}); ccExtraBody(kw)["reasoning"] != nil {
		t.Fatal(kw)
	}
	custom, _ := GetProviderProfile(context.Background(), "custom")
	if kw := ccTransportKwargs(t, "llama3", hi, map[string]any{"provider_profile": custom, "ollama_num_ctx": 32768}); ccExtraBody(kw)["options"].(map[string]any)["num_ctx"] != 32768 {
		t.Fatal(kw)
	}
	if kw := ccTransportKwargs(t, "qwen3", hi, map[string]any{"provider_profile": custom, "reasoning_config": map[string]any{"effort": "none"}, "base_url": "http://127.0.0.1:11434/v1"}); ccExtraBody(kw)["think"] != false {
		t.Fatal(kw)
	}
	kw = ccTransportKwargs(t, "mistral-small-latest", hi, map[string]any{"provider_profile": custom, "reasoning_config": map[string]any{"enabled": false, "effort": "none"}, "base_url": "https://api.mistral.ai/v1"})
	if ccExtraBody(kw)["think"] != nil || kw["reasoning_effort"] != "none" {
		t.Fatal(kw)
	}
	if kw := ccTransportKwargs(t, "gpt-4o", hi, map[string]any{"provider_profile": Profile{Name: "_t", OmitTemperature: true}, "temperature": 0.3}); kw["temperature"] != nil {
		t.Fatal(kw)
	}
	for _, provider := range []string{"nous", "openrouter"} {
		p, _ := GetProviderProfile(context.Background(), provider)
		kw := ccTransportKwargs(t, "openai/gpt-5.6-sol", hi, map[string]any{"reasoning_config": map[string]any{"enabled": true, "effort": "ultra"},
			"supports_reasoning": true, "provider_profile": p, "provider_name": provider, "base_url": p.BaseURL})
		if !reflect.DeepEqual(ccExtraBody(kw)["reasoning"], map[string]any{"enabled": true, "effort": "max"}) {
			t.Fatalf("%s: %v", provider, kw)
		}
	}
	lm, _ := GetProviderProfile(context.Background(), "lmstudio")
	base := map[string]any{"provider_profile": lm, "supports_reasoning": true, "reasoning_config": map[string]any{"effort": "high"}}
	toggle := copyDict(base)
	toggle["lmstudio_reasoning_options"] = []any{"off", "on"}
	if kw := ccTransportKwargs(t, "gpt-oss", hi, toggle); kw["reasoning_effort"] != nil {
		t.Fatal(kw)
	}
	allowed := copyDict(base)
	allowed["lmstudio_reasoning_options"] = []any{"off", "low", "medium", "high"}
	if kw := ccTransportKwargs(t, "gpt-oss", hi, allowed); kw["reasoning_effort"] != "high" {
		t.Fatal(kw)
	}
}

func TestChatCompletionsBuildKwargsGemini(t *testing.T) {
	hi := []map[string]any{{"role": "user", "content": "Hi"}}
	kw := ccTransportKwargs(t, "gemini-3-flash-preview", hi, map[string]any{"provider_name": "gemini",
		"base_url": "https://generativelanguage.googleapis.com/v1beta/openai", "reasoning_config": map[string]any{"enabled": true, "effort": "high"}})
	eb := ccExtraBody(kw)
	nested, _ := eb["extra_body"].(map[string]any)
	google, _ := nested["google"].(map[string]any)
	if eb["thinking_config"] != nil || !reflect.DeepEqual(google["thinking_config"], map[string]any{"include_thoughts": true, "thinking_level": "high"}) {
		t.Fatal(kw)
	}
	gemini := bundledPluginProfile(t, "gemini")
	params := func(effort string) map[string]any {
		p := map[string]any{"provider_profile": gemini, "provider_name": "gemini", "base_url": gemini.BaseURL, "max_tokens": 4096, "max_tokens_param_fn": ccMaxTokens}
		if effort != "" {
			p["reasoning_config"] = map[string]any{"enabled": true, "effort": effort}
		}
		return p
	}
	for model, level := range map[string]string{"gemini-3.7-flash": "high", "gemini-3.8-flash": "medium"} {
		effort := "ultra"
		if level == "medium" {
			effort = "medium"
		}
		kw := ccTransportKwargs(t, model, hi, params(effort))
		tc, _ := ccExtraBody(kw)["thinking_config"].(map[string]any)
		if kw["max_tokens"] != geminiDefaultMaxOutputTokens || tc["thinkingLevel"] != level {
			t.Fatalf("%s: %v", model, kw)
		}
	}
	if kw := ccTransportKwargs(t, "gemini-3.7-flash", hi, params("")); kw["max_tokens"] != 4096 {
		t.Fatal(kw)
	}
	nous, _ := GetProviderProfile(context.Background(), "nous")
	for _, tc := range []struct {
		model, baseURL string
		tags           bool
	}{
		{"anthropic/claude-sonnet-4.6", "https://generativelanguage.googleapis.com/v1beta", false},
		{"hermes-3-405b", "https://inference.nousresearch.com/v1", true},
		{"anthropic/claude-sonnet-4.6", "https://generativelanguage.googleapis.com/v1beta/openai", true},
	} {
		kw := ccTransportKwargs(t, tc.model, []map[string]any{{"role": "user", "content": "hi"}}, map[string]any{"provider_profile": nous, "base_url": tc.baseURL, "session_id": "s1", "max_tokens": nil})
		if _, has := ccExtraBody(kw)["tags"]; has != tc.tags {
			t.Fatalf("%s tags %v: %v", tc.baseURL, has, kw)
		}
	}
}

func TestChatCompletionsMoonshotToolSchemas(t *testing.T) {
	build := func(model string, params map[string]any) map[string]any {
		tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "search", "description": "Search", "parameters": params}}}
		kw, err := GetTransport(string(ModeChatCompletions)).BuildKwargs(model, []map[string]any{{"role": "user", "content": "Hi"}}, tools, map[string]any{"max_tokens_param_fn": ccMaxTokens})
		if err != nil {
			t.Fatal(err)
		}
		return kw["tools"].([]map[string]any)[0]["function"].(map[string]any)["parameters"].(map[string]any)
	}
	missingType := func() map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"description": "query"}}}
	}
	if p := build("moonshotai/kimi-k2.6", missingType()); p["properties"].(map[string]any)["q"].(map[string]any)["type"] != "string" {
		t.Fatal(p)
	}
	if p := build("moonshotai/kimi-k3", map[string]any{"type": "object", "properties": map[string]any{}}); !reflect.DeepEqual(p["required"], []any{}) {
		t.Fatalf("required %#v", p["required"])
	}
	if p := build("anthropic/claude-sonnet-4.6", missingType()); p["properties"].(map[string]any)["q"].(map[string]any)["type"] != nil {
		t.Fatal(p)
	}
}

func ccCacheMessages(instructions string) []map[string]any {
	return []map[string]any{{"role": "system", "content": instructions}, {"role": "user", "content": "hello"}}
}

func ccCacheTools(name string) []map[string]any {
	return []map[string]any{{"type": "function", "function": map[string]any{"name": name, "description": "Look something up.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
}

// Kwargs of the prompt cache key cases with the stable system prompt and lookup tool
func ccCacheKwargs(t *testing.T, instructions, toolName string, params map[string]any) map[string]any {
	t.Helper()
	kw, err := GetTransport(string(ModeChatCompletions)).BuildKwargs("cache-model", ccCacheMessages(instructions), ccCacheTools(toolName), params)
	if err != nil {
		t.Fatal(err)
	}
	return kw
}

// JSON body the OpenAI SDK request builder sends for the kwargs
func ccRequestBody(t *testing.T, kwargs map[string]any, stream bool) map[string]any {
	t.Helper()
	kw := copyDict(kwargs)
	if stream {
		kw["stream"] = true
	}
	req, err := auxSDKBuildRequest(&auxOpenAIClient{apiKey: "test-key", baseURL: "https://cache-capable.test/v1"}, kw)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{}
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPromptCacheKeyCapability(t *testing.T) {
	capable := Profile{Name: "cache-capable", SupportsPromptCacheKey: true}
	kw := ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"session_id": "cron_job_2026-07-15T10:00:00Z", "provider_profile": capable})
	key, _ := kw["prompt_cache_key"].(string)
	if !strings.HasPrefix(key, "pck_") || ccRequestBody(t, kw, false)["prompt_cache_key"] != key {
		t.Fatal(kw)
	}
	kw = ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"session_id": "cron_job_2026-07-15T10:05:00Z", "supports_prompt_cache_key": true})
	if body := ccRequestBody(t, kw, true); body["prompt_cache_key"] != kw["prompt_cache_key"] || kw["prompt_cache_key"] == nil {
		t.Fatal(body)
	}
	kw = ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"session_id": "cron_job_2026-07-15T10:07:00Z", "base_url": "https://api.openai.com/v1"})
	if key, _ := kw["prompt_cache_key"].(string); !strings.HasPrefix(key, "pck_") {
		t.Fatal(kw)
	}
	for _, base := range []string{"https://myproxy.example.com/api.openai.com/v1", "https://api.openai.com.evil.example/v1", "https://eastus.api.cognitive.microsoft.com/openai/v1"} {
		if kw := ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"session_id": "cron_job_2026-07-15T10:08:00Z", "base_url": base}); kw["prompt_cache_key"] != nil {
			t.Fatalf("%s: %v", base, kw)
		}
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, provider := range []string{"", "anthropic", "custom"} {
		params := map[string]any{"session_id": "cron_job_2026-07-15T10:00:00Z"}
		if provider != "" {
			p, ok := GetProviderProfile(context.Background(), provider)
			if !ok {
				t.Fatalf("no %s profile", provider)
			}
			params["provider_profile"] = p
		}
		kw := ccCacheKwargs(t, "You are stable.", "lookup", params)
		if _, has := kw["prompt_cache_key"]; has {
			t.Fatal(kw)
		}
		if _, has := ccRequestBody(t, kw, false)["prompt_cache_key"]; has {
			t.Fatal("body carries prompt_cache_key")
		}
	}
}

func TestPromptCacheKeyCallerOverrides(t *testing.T) {
	capable := Profile{Name: "cache-capable", SupportsPromptCacheKey: true}
	top := ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"provider_profile": capable, "request_overrides": map[string]any{"prompt_cache_key": "caller-top-level"}})
	inBody := ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"provider_profile": capable, "request_overrides": map[string]any{"extra_body": map[string]any{"prompt_cache_key": "caller-extra-body"}}})
	if top["prompt_cache_key"] != "caller-top-level" || ccExtraBody(top)["prompt_cache_key"] != nil || inBody["prompt_cache_key"] != nil || ccExtraBody(inBody)["prompt_cache_key"] != "caller-extra-body" {
		t.Fatal(top, inBody)
	}
	key := func(sessionID, instructions, tool string) any {
		return ccCacheKwargs(t, instructions, tool, map[string]any{"session_id": sessionID, "supports_prompt_cache_key": true})["prompt_cache_key"]
	}
	first, second := key("cron_job_20260715_100000", "You are stable.", "lookup"), key("cron_job_20260715_100500", "You are stable.", "lookup")
	if first != second || first == key("cron_job_20260715_100500", "You are different.", "lookup") || first == key("cron_job_20260715_100500", "You are stable.", "search") {
		t.Fatal("cron cache keys")
	}
	if key("session_alice_1", "You are stable.", "lookup") == key("session_bob_1", "You are stable.", "lookup") {
		t.Fatal("unrelated sessions share a key")
	}
	long := strings.Repeat("k", 200)
	kw := ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"provider_profile": capable, "request_overrides": map[string]any{"prompt_cache_key": long}})
	if k, _ := kw["prompt_cache_key"].(string); !strings.HasPrefix(k, "pck_") || len(k) > 64 || ccRequestBody(t, kw, false)["prompt_cache_key"] != k {
		t.Fatal(kw)
	}
	kw = ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"provider_profile": capable, "request_overrides": map[string]any{"extra_body": map[string]any{"prompt_cache_key": long}}})
	if k, _ := ccExtraBody(kw)["prompt_cache_key"].(string); !strings.HasPrefix(k, "pck_") || len(k) > 64 || kw["prompt_cache_key"] != nil {
		t.Fatal(kw)
	}
	kw = ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"supports_prompt_cache_key": true, "request_overrides": map[string]any{"prompt_cache_key": long}})
	if k, _ := kw["prompt_cache_key"].(string); !strings.HasPrefix(k, "pck_") || len(k) > 64 {
		t.Fatal(kw)
	}
	kw = ccCacheKwargs(t, "You are stable.", "lookup", map[string]any{"provider_profile": capable, "request_overrides": map[string]any{"prompt_cache_key": "   "}})
	if _, has := kw["prompt_cache_key"]; has {
		t.Fatal(kw)
	}
}

// Nous Portal tags without a session in the list shape the profile body carries
func ccNousTags() []any {
	var out []any
	for _, tag := range nousPortalTags(context.Background(), "") {
		out = append(out, tag)
	}
	return out
}

func TestProfileHookWithoutLMStudioKeywordStillCalled(t *testing.T) {
	old := Profile{Name: "old-signature", BuildAPIKwargsExtrasHook: func(_ *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
		if _, has := kw["lmstudio_reasoning_options"]; has {
			t.Fatal("lmstudio_reasoning_options reached a hook whose caller passed none")
		}
		return map[string]any{}, map[string]any{"reasoning_effort": "low"}
	}}
	if kw := ccTransportKwargs(t, "m", []map[string]any{{"role": "user", "content": "Hi"}}, map[string]any{"provider_profile": old}); kw["reasoning_effort"] != "low" {
		t.Fatal(kw)
	}
}

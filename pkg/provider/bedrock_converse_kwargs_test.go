package provider

import (
	"errors"
	"strings"
	"testing"
)

const bcNovaToolsRejection = "An error occurred (ValidationException) when calling the ConverseStream " +
	"operation: The model returned the following errors: Malformed input " +
	"request: #/toolConfig/tools/18: extraneous key [cachePoint] is not " +
	"permitted, please reformat your input and try again."

const bcTestTool = `[{"type":"function","function":{"name":"test","description":"Test","parameters":{}}}]`

func bcFloat(v float64) *float64 { return &v }
func bcInt(v int) *int           { return &v }

func bcHasCachePoint(t testing.TB, blocks any) bool {
	list, _ := asList(blocks)
	for _, b := range list {
		if bcJSON(t, b) == `{"cachePoint":{"type":"default"}}` {
			return true
		}
	}
	return false
}

func bcLastIsCachePoint(t testing.TB, blocks any) bool {
	list, _ := asList(blocks)
	return len(list) > 0 && bcJSON(t, list[len(list)-1]) == `{"cachePoint":{"type":"default"}}`
}

func bcResetCachePoints(t *testing.T) {
	ResetBedrockCachePointRejections()
	t.Cleanup(ResetBedrockCachePointRejections)
}

func TestBuildConverseKwargs(t *testing.T) {
	bcResetCachePoints(t)
	t.Run("basic_kwargs", func(t *testing.T) {
		in := newBedrockConverseInput("anthropic.claude-sonnet-4-6-20250514-v1:0", bcMaps(t, `[{"role":"system","content":"Be helpful."},{"role":"user","content":"Hi"}]`))
		in.MaxTokens = bcInt(1024)
		kw := buildBedrockConverseKwargs(in)
		if kw["modelId"] != in.Model || kw["inferenceConfig"].(map[string]any)["maxTokens"] != 1024 || kw["system"] == nil || len(kw["messages"].([]any)) < 1 {
			t.Fatal(kw)
		}
	})
	t.Run("includes_tools", func(t *testing.T) {
		in := newBedrockConverseInput("test-model", bcMaps(t, `[{"role":"user","content":"Hi"}]`))
		in.Tools = bcMaps(t, bcTestTool)
		kw := buildBedrockConverseKwargs(in)
		if tc, ok := kw["toolConfig"].(map[string]any); !ok || len(tc["tools"].([]any)) != 1 {
			t.Fatal(kw)
		}
	})
	t.Run("max_tokens_none_omits_cap", func(t *testing.T) {
		kw := buildBedrockConverseKwargs(bedrockConverseInput{Model: "test-model", Messages: bcMaps(t, `[{"role":"user","content":"Hi"}]`), Temperature: bcFloat(0.1)})
		cfg := kw["inferenceConfig"].(map[string]any)
		if _, has := cfg["maxTokens"]; has || cfg["temperature"] != 0.1 {
			t.Fatal(cfg)
		}
	})
	t.Run("max_tokens_none_and_no_sampling_drops_empty_inference_config", func(t *testing.T) {
		kw := buildBedrockConverseKwargs(bedrockConverseInput{Model: "test-model", Messages: bcMaps(t, `[{"role":"user","content":"Hi"}]`)})
		if _, has := kw["inferenceConfig"]; has {
			t.Fatal(kw)
		}
	})
	t.Run("bedrock_xai_grok_models_never_receive_sampling_params", func(t *testing.T) {
		for _, model := range []string{"us.xai.grok-4.6", "global.xai.grok-4.6", "test-model", "qwen.qwen3-vl-235b-a22b"} {
			in := newBedrockConverseInput(model, bcMaps(t, `[{"role":"user","content":"Hi"}]`))
			in.Temperature, in.TopP = bcFloat(0.3), bcFloat(0.9)
			cfg := buildBedrockConverseKwargs(in)["inferenceConfig"].(map[string]any)
			_, hasT := cfg["temperature"]
			_, hasP := cfg["topP"]
			grok := strings.Contains(model, "grok")
			if grok && (hasT || hasP) || !grok && (cfg["temperature"] != 0.3 || cfg["topP"] != 0.9) {
				t.Fatal(model, cfg)
			}
		}
	})
	turns := `[{"role":"system","content":"Be helpful."},{"role":"user","content":"First"},{"role":"assistant","content":"Reply"},{"role":"user","content":"Second"}]`
	t.Run("cache_point_added_for_supported_model", func(t *testing.T) {
		in := newBedrockConverseInput("anthropic.claude-sonnet-4-6-20250514-v1:0", bcMaps(t, turns))
		in.Tools = bcMaps(t, bcTestTool)
		kw := buildBedrockConverseKwargs(in)
		msgs := kw["messages"].([]any)
		if !bcLastIsCachePoint(t, kw["system"]) || !bcLastIsCachePoint(t, kw["toolConfig"].(map[string]any)["tools"]) ||
			!bcLastIsCachePoint(t, msgs[len(msgs)-2].(map[string]any)["content"]) || bcLastIsCachePoint(t, msgs[len(msgs)-1].(map[string]any)["content"]) {
			t.Fatal(bcJSON(t, kw))
		}
	})
	t.Run("no_cache_point_for_unsupported_model", func(t *testing.T) {
		kw := buildBedrockConverseKwargs(newBedrockConverseInput("meta.llama3-70b-instruct-v1:0", bcMaps(t, turns)))
		if bcHasCachePoint(t, kw["system"]) {
			t.Fatal(kw)
		}
		for _, m := range kw["messages"].([]any) {
			if bcHasCachePoint(t, m.(map[string]any)["content"]) {
				t.Fatal(m)
			}
		}
	})
}

func bcNovaKwargs(t *testing.T) map[string]any {
	in := newBedrockConverseInput("us.amazon.nova-pro-v1:0", bcMaps(t, `[{"role":"system","content":"Be helpful."},{"role":"user","content":"First"},
		{"role":"assistant","content":"Reply"},{"role":"user","content":"Second"}]`))
	in.Tools = bcMaps(t, bcTestTool)
	return buildBedrockConverseKwargs(in)
}

func bcSecondNewest(kw map[string]any) any {
	msgs := kw["messages"].([]any)
	return msgs[len(msgs)-2].(map[string]any)["content"]
}

func TestCachePointRejectionRecovery(t *testing.T) {
	t.Run("classifies_tools_rejection", func(t *testing.T) {
		bcResetCachePoints(t)
		if got := bedrockCachePointRejectionPlacement(errors.New(bcNovaToolsRejection)); got != "tools" {
			t.Fatal(got)
		}
	})
	t.Run("classifies_system_and_messages_rejections", func(t *testing.T) {
		if got := bedrockCachePointRejectionPlacement(errors.New("Malformed input request: #/system/1: extraneous key [cachePoint] is not permitted")); got != "system" {
			t.Fatal(got)
		}
		if got := bedrockCachePointRejectionPlacement(errors.New("Malformed input request: #/messages/2/content/3: extraneous key [cachePoint] is not permitted")); got != "messages" {
			t.Fatal(got)
		}
	})
	t.Run("ignores_unrelated_errors", func(t *testing.T) {
		for _, msg := range []string{"ThrottlingException: Too many requests", "Malformed input request: #/toolConfig/tools/0: extraneous key [toolChoice] is not permitted"} {
			if got := bedrockCachePointRejectionPlacement(errors.New(msg)); got != "" {
				t.Fatal(msg, got)
			}
		}
	})
	t.Run("strip_removes_only_the_rejected_placement", func(t *testing.T) {
		bcResetCachePoints(t)
		kw := bcNovaKwargs(t)
		tools := func(m map[string]any) any { return m["toolConfig"].(map[string]any)["tools"] }
		if !bcHasCachePoint(t, tools(kw)) {
			t.Fatal(kw)
		}
		stripped, changed := stripBedrockCachePoints(kw, "tools")
		if !changed || bcHasCachePoint(t, tools(stripped)) || !bcLastIsCachePoint(t, stripped["system"]) || !bcLastIsCachePoint(t, bcSecondNewest(stripped)) || !bcHasCachePoint(t, tools(kw)) {
			t.Fatal(bcJSON(t, stripped))
		}
	})
	t.Run("strip_is_identity_when_marker_absent", func(t *testing.T) {
		kw := bcMap(t, `{"modelId":"x","toolConfig":{"tools":[{"toolSpec":{}}]}}`)
		if out, changed := stripBedrockCachePoints(kw, "tools"); changed || bcJSON(t, out) != bcJSON(t, kw) {
			t.Fatal(out)
		}
	})
	t.Run("recovery_records_verdict_so_later_turns_omit_the_marker", func(t *testing.T) {
		bcResetCachePoints(t)
		retry, ok := recoverFromBedrockCachePointRejection(errors.New(bcNovaToolsRejection), bcNovaKwargs(t))
		if !ok || bcHasCachePoint(t, retry["toolConfig"].(map[string]any)["tools"]) {
			t.Fatal(retry)
		}
		rebuilt := bcNovaKwargs(t)
		if bcHasCachePoint(t, rebuilt["toolConfig"].(map[string]any)["tools"]) || !bcLastIsCachePoint(t, rebuilt["system"]) || !bcLastIsCachePoint(t, bcSecondNewest(rebuilt)) {
			t.Fatal(bcJSON(t, rebuilt))
		}
	})
	t.Run("verdict_is_scoped_to_the_rejecting_model", func(t *testing.T) {
		bcResetCachePoints(t)
		recoverFromBedrockCachePointRejection(errors.New(bcNovaToolsRejection), bcNovaKwargs(t))
		in := newBedrockConverseInput("anthropic.claude-sonnet-4-6-20250514-v1:0", bcMaps(t, `[{"role":"user","content":"hi"}]`))
		in.Tools = bcMaps(t, bcTestTool)
		if kw := buildBedrockConverseKwargs(in); !bcLastIsCachePoint(t, kw["toolConfig"].(map[string]any)["tools"]) {
			t.Fatal(kw)
		}
	})
	t.Run("recovery_declines_when_nothing_can_be_stripped", func(t *testing.T) {
		bcResetCachePoints(t)
		if _, ok := recoverFromBedrockCachePointRejection(errors.New(bcNovaToolsRejection), bcMap(t, `{"modelId":"us.amazon.nova-pro-v1:0","toolConfig":{"tools":[{"toolSpec":{}}]}}`)); ok {
			t.Fatal("recovered")
		}
	})
}

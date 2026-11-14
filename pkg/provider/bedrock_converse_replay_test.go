package provider

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestStreamConverseWithCallbacks(t *testing.T) {
	t.Run("cache_tokens_folded_into_prompt_tokens", func(t *testing.T) {
		r, _, err := streamConverseWithCallbacks(bcEvents(t, `[{"messageStart":{"role":"assistant"}},{"contentBlockStart":{"contentBlockIndex":0,"start":{}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"hi"}}},{"contentBlockStop":{"contentBlockIndex":0}},{"messageStop":{"stopReason":"end_turn"}},
			{"metadata":{"usage":{"inputTokens":50,"outputTokens":20,"cacheReadInputTokens":900,"cacheWriteInputTokens":300}}}]`), bedrockStreamCallbacks{})
		if err != nil || r.Usage.PromptTokens != 1250 || r.Usage.TotalTokens != 1270 || r.Usage.CacheReadInputTokens != 900 || r.Usage.CacheCreationInputTokens != 300 {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("text_deltas_suppressed_when_tool_use_present", func(t *testing.T) {
		var deltas []string
		r, _, err := streamConverseWithCallbacks(bcEvents(t, `[{"messageStart":{"role":"assistant"}},{"contentBlockStart":{"contentBlockIndex":0,"start":{}}},
			{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Let me check."}}},{"contentBlockStop":{"contentBlockIndex":0}},
			{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"c1","name":"search"}}}},
			{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"q\":\"test\"}"}}}},{"contentBlockStop":{"contentBlockIndex":1}},
			{"messageStop":{"stopReason":"tool_use"}},{"metadata":{"usage":{"inputTokens":0,"outputTokens":0}}}]`),
			bedrockStreamCallbacks{OnTextDelta: func(s string) error { deltas = append(deltas, s); return nil }})
		if err != nil || !reflect.DeepEqual(deltas, []string{"Let me check."}) || r.Content != "Let me check." || len(r.ToolCalls) != 1 {
			t.Fatalf("%+v %v %v", r, deltas, err)
		}
	})
}

func TestGuardrailConfigInConverseKwargs(t *testing.T) {
	guardrail := map[string]any{"guardrailIdentifier": "gr-abc123", "guardrailVersion": "1", "streamProcessingMode": "async", "trace": "enabled"}
	in := newBedrockConverseInput("test-model", bcMaps(t, `[{"role":"user","content":"Hi"}]`))
	in.GuardrailConfig = guardrail
	if kw := buildBedrockConverseKwargs(in); !reflect.DeepEqual(kw["guardrailConfig"], guardrail) {
		t.Fatal(kw)
	}
	in.GuardrailConfig = map[string]any{}
	if kw := buildBedrockConverseKwargs(in); kw["guardrailConfig"] != nil {
		t.Fatal(kw)
	}
}

func TestBuildConverseKwargsToolStripping(t *testing.T) {
	in := newBedrockConverseInput("us.deepseek.r1-v1:0", bcMaps(t, `[{"role":"user","content":"Hi"}]`))
	in.Tools = bcMaps(t, `[{"type":"function","function":{"name":"test","description":"t","parameters":{}}}]`)
	if kw := buildBedrockConverseKwargs(in); kw["toolConfig"] != nil {
		t.Fatal(kw)
	}
}

func TestEmptyTextBlockFix(t *testing.T) {
	blocks := convertContentToConverse(nil)
	if text := bcBlock(t, blocks[0])["text"]; text != bedrockEmptyTextPlaceholder || strings.TrimSpace(text.(string)) == "" {
		t.Fatal(blocks)
	}
}

func TestImageBase64Decoding(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg=="
	raw, _ := base64.StdEncoding.DecodeString(png)
	blocks := convertContentToConverse([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + png}}})
	img := bcBlock(t, bcBlock(t, blocks[0])["image"])
	if len(blocks) != 1 || img["format"] != "png" || !bytes.Equal(bcBlock(t, img["source"])["bytes"].([]byte), raw) {
		t.Fatal(blocks)
	}
	blocks = convertContentToConverse([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,NOT_VALID_BASE64!!!"}}})
	if _, ok := bcBlock(t, bcBlock(t, bcBlock(t, blocks[0])["image"])["source"])["bytes"].([]byte); len(blocks) != 1 || !ok {
		t.Fatal(blocks)
	}
}

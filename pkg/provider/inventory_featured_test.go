package provider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func invUserRow(slug, apiURL string, models []any) map[string]any {
	_, name, found := strings.Cut(slug, ":")
	if !found {
		name = slug
	}
	row := invRow(slug, name, models, false, true, "user-config")
	row["api_url"] = apiURL
	return row
}

func TestBuiltInRowKeepsModelsWhenUserRowIsSameUpstream(t *testing.T) {
	invTestHome(t)
	for _, twin := range []string{"custom:openrouter", "custom:my-or-relay"} {
		rows := []map[string]any{
			invRow("openrouter", "OpenRouter", []any{"anthropic/claude-fable-5", "openai/gpt-6"}, false, false, "builtin"),
			invUserRow(twin, "https://openrouter.ai/api/v1", []any{"anthropic/claude-fable-5", "openai/gpt-6", "qwen/qwen-4-max"}),
		}
		stripAggregatorOverlaps(context.Background(), rows)
		if !reflect.DeepEqual(rows[0]["models"], []any{"anthropic/claude-fable-5", "openai/gpt-6"}) || rows[0]["total_models"] != 2 {
			t.Fatalf("%s twin stripped %v", twin, rows[0])
		}
	}
}

func TestGenuinelyDifferentUpstreamStillStripsOverlaps(t *testing.T) {
	invTestHome(t)
	rows := []map[string]any{
		invRow("openrouter", "OpenRouter", []any{"openai/gpt-6", "deepseek/deepseek-v4.1-flash"}, false, false, "builtin"),
		invUserRow("custom:proxy", "https://my-proxy.example.com/v1", []any{"openai/gpt-6"}),
	}
	stripAggregatorOverlaps(context.Background(), rows)
	if !reflect.DeepEqual(rows[0]["models"], []any{"deepseek/deepseek-v4.1-flash"}) || rows[0]["total_models"] != 1 {
		t.Fatalf("row %v", rows[0])
	}
}

var invModelScopeModels = []any{"Qwen/Qwen3.8-Flash-Next", "Qwen/Qwen3.8-27B", "Qwen/Qwen3-235B-A22B", "Qwen/Qwen3.5-397B-A17B",
	"Qwen/Qwen3.5-122B-A10B", "Qwen/Qwen3-VL-235B-A22B-Instruct", "Qwen/Qwen3-VL-8B-Instruct", "Qwen/Qwen3-VL-8B-Thinking",
	"deepseek-ai/DeepSeek-V4-Flash-0731", "deepseek-ai/DeepSeek-V4-Pro", "ZhipuAI/GLM-4.7-Flash", "ZhipuAI/GLM-5.2", "stepfun-ai/Step-3.7-Flash"}

func invLabModels() []any {
	out := []any{}
	for i := range 12 {
		out = append(out, "lab"+pyStr(i%3)+"/model-"+pyStr(i))
	}
	return out
}

func TestFeaturedShortlistOnlyForBuiltInRoutingAggregators(t *testing.T) {
	invTestHome(t)
	cases := []map[string]any{
		invRow("modelscope", "modelscope", append([]any(nil), invModelScopeModels...), true, true, ""),
		invRow("custom:lab", "Lab", []any{"openai/model-a", "anthropic/model-b", "openai/model-c"}, false, true, ""),
		invRow("anthropic", "Anthropic", []any{"claude-opus-5", "claude-sonnet-5"}, false, false, ""),
		invRow("opencode-go", "OpenCode Go", invLabModels(), false, false, ""),
	}
	for _, row := range cases {
		applyFeatured(context.Background(), []map[string]any{row}, nil)
		if !reflect.DeepEqual(row["featured_models"], []any{}) {
			t.Fatalf("%v featured %v", row["slug"], row["featured_models"])
		}
	}
	if !reflect.DeepEqual(cases[0]["models"], invModelScopeModels) {
		t.Fatal("user defined models trimmed")
	}
}

func TestBuiltInRoutingAggregatorKeepsFeaturedShortlist(t *testing.T) {
	invTestHome(t)
	models := invLabModels()
	row := invRow("openrouter", "OpenRouter", models, false, false, "")
	applyFeatured(context.Background(), []map[string]any{row}, nil)
	featured, _ := row["featured_models"].([]any)
	if len(featured) == 0 || len(featured) > 15 {
		t.Fatalf("featured %v", featured)
	}
	last := -1
	for _, m := range featured {
		i := slices.Index(models, m)
		if i <= last {
			t.Fatalf("featured %v not a subset in row order", featured)
		}
		last = i
	}
}

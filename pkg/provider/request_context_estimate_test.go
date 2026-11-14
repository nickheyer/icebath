package provider

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var estimatorB64 = "data:image/png;base64," + strings.Repeat("A", 400000)

func estimatorChat(parts ...any) map[string]any {
	return map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": parts}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "t"}}},
	}
}

func TestImagePartsCostTheLearnedPriceOnBothWireShapes(t *testing.T) {
	ctx := context.Background()
	chat := estimatorChat(map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": estimatorB64}})
	responses := map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": estimatorB64}}}}, "instructions": strings.Repeat("i", 400)}
	def := EstimateRequestContextTokens(ctx, chat)
	if def < DefaultImageTokenCost || def >= DefaultImageTokenCost+200 {
		t.Fatalf("chat estimate %d", def)
	}
	if n := EstimateRequestContextTokens(ctx, responses); n < DefaultImageTokenCost || n >= DefaultImageTokenCost+200 {
		t.Fatalf("responses estimate %d", n)
	}
	cost := 4000
	if n := EstimateRequestContextTokens(ImageCostContext(ctx, &cost), chat); n < 4000 || n >= 4200 {
		t.Fatalf("learned cost estimate %d", n)
	}
	if CodexStaleTimeoutFloor(def) != 0 {
		t.Fatal("one screenshot bought a codex stale floor")
	}
}

func TestTextPayloadsAndToolSchemasKeepTheLegacyEstimate(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": estimatorB64}}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "vision", "parameters": map[string]any{"type": "image"}}}}
	legacy := 0
	for _, m := range messages {
		legacy += pystr.Len(pyrepr.Str(m))
	}
	legacy = (legacy + pystr.Len(pyrepr.Str(tools))) / 4
	got := EstimateRequestContextTokens(context.Background(), map[string]any{"messages": messages, "tools": tools})
	if math.Abs(float64(got-legacy)) >= float64(legacy)*0.02 {
		t.Fatalf("estimate %d legacy %d", got, legacy)
	}
}

func TestSchemaNodesWithStructuredTypeValuesKeepTheLegacyEstimate(t *testing.T) {
	ctx := context.Background()
	toolsNamed := func(param string) []any {
		return []any{map[string]any{"type": "function", "function": map[string]any{"name": "memory_search", "parameters": map[string]any{"type": "object", "properties": map[string]any{
			param:   map[string]any{"type": "string", "enum": []any{"episode", "semantic"}},
			"maybe": map[string]any{"type": []any{"string", "null"}},
		}}}}}
	}
	messages := []any{map[string]any{"role": "user", "content": "hi"}}
	if a, b := EstimateRequestContextTokens(ctx, map[string]any{"messages": messages, "tools": toolsNamed("type")}), EstimateRequestContextTokens(ctx, map[string]any{"messages": messages, "tools": toolsNamed("kind")}); a != b {
		t.Fatalf("type key %d kind key %d", a, b)
	}
	chat := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": estimatorB64}},
	}}}, "tools": toolsNamed("type")}
	if n := EstimateRequestContextTokens(ctx, chat); n < DefaultImageTokenCost || n >= DefaultImageTokenCost+400 {
		t.Fatalf("estimate %d", n)
	}
}

func TestEstimateCountsPythonStrOfScalarsTypedValuesAndBareLists(t *testing.T) {
	ctx := context.Background()
	if n := EstimateRequestContextTokens(ctx, map[string]any{"messages": []any{"éééééééé"}}); n != 2 {
		t.Fatalf("code points not counted %d", n)
	}
	if n := EstimateRequestContextTokens(ctx, map[string]any{"a": true, "b": 1.0, "c": nil}); n != (4+3)/4 {
		t.Fatalf("scalar str lengths %d", n)
	}
	typed := map[string]any{"messages": []map[string]any{{"role": "user"}}}
	untyped := map[string]any{"messages": []any{map[string]any{"role": "user"}}}
	if EstimateRequestContextTokens(ctx, typed) != EstimateRequestContextTokens(ctx, untyped) {
		t.Fatal("typed message slice estimated differently")
	}
	if n := EstimateRequestContextTokens(ctx, []any{"abcdefgh", "abcdefgh"}); n != 4 {
		t.Fatalf("bare list %d", n)
	}
}

func TestValidatedOpenRouterProviderSortAcceptsValidValues(t *testing.T) {
	for raw, want := range map[string]string{"price": "price", " latency ": "latency", "THROUGHPUT": "throughput"} {
		if got, ok := validatedOpenRouterProviderSort(raw); !ok || got != want {
			t.Fatalf("%q gave %q %v", raw, got, ok)
		}
	}
}

func TestValidatedOpenRouterProviderSortRejectsInvalidValues(t *testing.T) {
	for _, raw := range []any{"intelligence", "", nil} {
		if got, ok := validatedOpenRouterProviderSort(raw); ok || got != "" {
			t.Fatalf("%#v accepted as %q", raw, got)
		}
	}
}

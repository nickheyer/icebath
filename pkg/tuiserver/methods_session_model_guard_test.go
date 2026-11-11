package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestDesktopSurfacesCarryTheExactTierUltrafastIsNeverPlainFast(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rows := []map[string]any{
		{"slug": "openai-codex", "models": []any{"gpt-6-astra-900k", "gpt-6.1-sol", "gpt-6.1-sol-900k", "gpt-6-sol", "gpt-daybreak-blue-latest-900k"}},
		{"slug": "openrouter", "models": []any{"openai/gpt-6-astra", "openai/gpt-6.1-sol"}},
	}
	if err := provider.ApplyCapabilities(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	caps := map[string]map[string]any{}
	for _, row := range rows {
		for model, c := range row["capabilities"].(map[string]any) {
			caps[model] = c.(map[string]any)
		}
	}
	has := func(model, key string) bool { return pyrepr.Truthy(caps[model][key]) }
	for _, model := range []string{"gpt-6-astra-900k", "gpt-6.1-sol", "gpt-6.1-sol-900k"} {
		if !has(model, "fast") || !has(model, "ultrafast") {
			t.Fatalf("%s caps %v", model, caps[model])
		}
	}
	for _, model := range []string{"gpt-6-sol", "gpt-daybreak-blue-latest-900k"} {
		if !has(model, "fast") || has(model, "ultrafast") {
			t.Fatalf("%s caps %v", model, caps[model])
		}
	}
	for _, model := range []string{"openai/gpt-6-astra", "openai/gpt-6.1-sol"} {
		if has(model, "fast") || has(model, "ultrafast") {
			t.Fatalf("%s caps %v", model, caps[model])
		}
	}
	tierOf := func(params map[string]any) *string {
		_, _, tier, err := createOverrides(params)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	for _, c := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"fast": true, "service_tier": "ultrafast"}, "ultrafast"},
		{map[string]any{"fast": true, "service_tier": "normal"}, ""},
		{map[string]any{"fast": true}, "priority"},
	} {
		if got := tierOf(c.params); got == nil || *got != c.want {
			t.Fatalf("%v: %v", c.params, got)
		}
	}
	if got := tierOf(map[string]any{}); got != nil {
		t.Fatalf("empty params tier %v", *got)
	}
	if _, _, _, err := createOverrides(map[string]any{"service_tier": "turbo"}); err == nil {
		t.Fatal("unknown tier accepted")
	}
}

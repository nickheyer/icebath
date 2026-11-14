package provider

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestExplicitProviderStaleTimeoutWinsOverContextTierAndReasoningFloor(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	unsetEnvForTest(t, StreamStaleTimeoutEnv)
	kwargs := map[string]any{"model": "gpt-5.6-sol", "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("word ", 60000)}}}
	writeConfigYAML(t, h, "providers:\n  custom:\n    stale_timeout_seconds: 60\n")
	if got := cloudStaleTimeoutFor(context.Background(), "custom", "gpt-5.6-sol", kwargs); got != 60 {
		t.Fatalf("explicit = %v, want 60", got)
	}
	writeConfigYAML(t, h, "")
	if got := cloudStaleTimeoutFor(context.Background(), "custom", "gpt-5.6-sol", kwargs); got != 600 {
		t.Fatalf("implicit = %v, want 600", got)
	}
}

func TestGPT56FloorReachesTheCloudStreamResolver(t *testing.T) {
	ctx := context.Background()
	if got := cloudStaleTimeout(ctx, 180, map[string]any{"model": "gpt-5.6-sol", "input": "small"}); got != 600 {
		t.Fatalf("gpt-5.6-sol = %v, want 600", got)
	}
	for _, m := range []string{"gpt-4.1", "gpt-4o", "gpt-5.1-chat-latest"} {
		if got := cloudStaleTimeout(ctx, 180, map[string]any{"model": m, "input": "small"}); got != 180 {
			t.Fatalf("%s = %v, want 180", m, got)
		}
	}
	if got := cloudStaleTimeout(ctx, 900, map[string]any{"model": "gpt-5.6-sol", "input": "small"}); got != 900 {
		t.Fatalf("explicit base = %v, want 900", got)
	}
}

func TestBedrockProfileIDReachesTheReasoningFloor(t *testing.T) {
	want, ok := 0.0, false
	for _, c := range []string{"claude-opus-4-6-v1:0", "claude-opus-4.6-v1:0"} {
		if d, found := ReasoningStaleTimeoutFloor(c); found {
			want, ok = d.Seconds(), true
			break
		}
	}
	if !ok {
		t.Fatal("no floor for the claude opus 4.6 forms")
	}
	if got, found := bedrockReasoningStaleFloor(" US.anthropic.claude-opus-4-6-v1:0 "); !found || got != want {
		t.Fatalf("profile floor = %v %v, want %v", got, found, want)
	}
	if got := cloudStaleTimeout(context.Background(), 180, map[string]any{"modelId": "us.anthropic.claude-opus-4-6-v1:0"}); got != math.Max(180, want) {
		t.Fatalf("modelId timeout = %v", got)
	}
	for _, id := range []any{nil, "", 5, "us.amazon.nova-lite-v1:0"} {
		if _, found := bedrockReasoningStaleFloor(id); found {
			t.Fatalf("%v unexpectedly floored", id)
		}
	}
	if got := swapDigitSeparator("a-4-6-b1-2", '-', '.'); got != "a-4.6-b1.2" {
		t.Fatalf("swap = %q", got)
	}
}

func TestProviderTimeoutsCoerceLikePythonFloat(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfigYAML(t, h, "providers:\n  a:\n    request_timeout_seconds: '45'\n    models:\n      m1:\n        timeout_seconds: true\n      m2:\n        timeout_seconds: -3\n  b:\n    stale_timeout_seconds: .nan\n  c:\n    stale_timeout_seconds: 'inf'\n")
	ctx := context.Background()
	if got, ok := GetProviderRequestTimeout(ctx, "a", "m1"); !ok || got != 1 {
		t.Fatalf("model bool = %v %v", got, ok)
	}
	if got, ok := GetProviderRequestTimeout(ctx, "a", "m2"); !ok || got != 45 {
		t.Fatalf("model fallback = %v %v", got, ok)
	}
	if _, ok := GetProviderStaleTimeout(ctx, "b", ""); ok {
		t.Fatal("nan accepted")
	}
	if got, ok := GetProviderStaleTimeout(ctx, "c", ""); !ok || !math.IsInf(got, 1) {
		t.Fatalf("inf = %v %v", got, ok)
	}
	if _, ok := GetProviderStaleTimeout(ctx, "", "m1"); ok {
		t.Fatal("empty provider answered")
	}
}

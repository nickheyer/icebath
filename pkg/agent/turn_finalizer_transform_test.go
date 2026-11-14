package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

var _ provider.CodexRuntimeTurnHooks = (*AIAgent)(nil)

func TestHeldTransformFiresOncePerTurnAndRecordsTheOriginal(t *testing.T) {
	calls := 0
	a := &AIAgent{sessionID: "s1", model: "m", platform: "telegram", holdStream: true}
	var seen map[string]any
	a.heldTransforms = func(_ context.Context, kw map[string]any) (string, bool) {
		calls++
		seen = kw
		return "REWRITTEN", false
	}
	got, transformed, pre := a.ApplyLLMOutputTransform("original", "turn-1")
	if got != "REWRITTEN" || !transformed || pre != "original" {
		t.Fatalf("first call = %q %v %v", got, transformed, pre)
	}
	if seen["platform"] != "telegram" || seen["turn_id"] != "turn-1" || seen["session_id"] != "s1" || seen["response_text"] != "original" {
		t.Fatalf("payload %v", seen)
	}
	got, transformed, pre = a.ApplyLLMOutputTransform("later text", "turn-1")
	if calls != 1 || got != "later text" || !transformed || pre != "original" {
		t.Fatalf("recorded call = %q %v %v after %d calls", got, transformed, pre, calls)
	}
	if got, transformed, pre := applyLLMOutputTransform(context.Background(), a, "", "turn-2", nil); got != "" || transformed || pre != nil || calls != 1 {
		t.Fatalf("empty response = %q %v %v", got, transformed, pre)
	}
	plat := ""
	a.heldTransforms = func(_ context.Context, kw map[string]any) (string, bool) { seen = kw; return "", true }
	got, transformed, pre = applyLLMOutputTransform(context.Background(), a, "secret", "turn-3", &plat)
	if got != heldTransformFailedReply || !transformed || pre != "secret" || seen["platform"] != "" {
		t.Fatalf("failed transform = %q %v %v %v", got, transformed, pre, seen["platform"])
	}
	a.heldTransforms = func(context.Context, map[string]any) (string, bool) { panic("dispatch broke") }
	if got, _, _ := a.ApplyLLMOutputTransform("kept back", "turn-4"); got != heldTransformFailedReply {
		t.Fatalf("panicking dispatch = %q", got)
	}
}

func TestEffectiveRequestOverridesCopyAndDropSpeedForUnprovisionedModel(t *testing.T) {
	a := &AIAgent{model: "claude-opus-4-6", provider: "custom", requestOverrides: map[string]any{"speed": "fast", "x": 1}}
	got := a.EffectiveRequestOverrides()
	if got["speed"] != "fast" || got["x"] != 1 {
		t.Fatalf("overrides %v", got)
	}
	got["x"] = 2
	if a.requestOverrides["x"] != 1 {
		t.Fatal("effective overrides alias the agent overrides")
	}
	a.markFastModeUnavailable()
	if _, has := a.EffectiveRequestOverrides()["speed"]; has {
		t.Fatal("speed kept for a model without fast capacity")
	}
	a.requestOverrides = map[string]any{"y": true}
	if got := a.EffectiveRequestOverrides(); got["y"] != true || len(got) != 1 {
		t.Fatalf("reassigned overrides not read live: %v", got)
	}
	a.serviceTier, a.fastAutoSeconds = "cold", 60
	beginFastModeTurn(a, []map[string]any{{"role": "user", "content": "hi"}})
	if a.fastMode().Open() {
		t.Fatal("cold window opened on a continuing session")
	}
	beginFastModeTurn(a, []map[string]any{{"role": "system", "content": "s"}})
	if !a.fastMode().Open() {
		t.Fatal("cold window stayed closed on a fresh session")
	}
}

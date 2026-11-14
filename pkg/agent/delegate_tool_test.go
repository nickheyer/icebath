package agent

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func cacheMarkers(messages []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range messages {
		if blocks, ok := m["content"].([]any); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok {
					if cc, ok := bm["cache_control"].(map[string]any); ok {
						out = append(out, cc)
					}
				}
			}
		}
		if cc, ok := m["cache_control"].(map[string]any); ok {
			out = append(out, cc)
		}
	}
	return out
}

func ttlWireMessages() []map[string]any {
	return []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "hi"},
		{"role": "assistant", "content": "ok"}, {"role": "user", "content": "go"}}
}

func TestChild1hBecomes5mAndWireMarkersDropTTL(t *testing.T) {
	child := &AIAgent{cacheTTL: "1h"}
	applyChildCacheTTL(child)
	if child.cacheTTL != "5m" {
		t.Fatalf("child ttl %q", child.cacheTTL)
	}
	marks := cacheMarkers(provider.ApplyAnthropicCacheControl(ttlWireMessages(), child.cacheTTL, false, "", false))
	if len(marks) == 0 {
		t.Fatal("no cache markers on the child wire")
	}
	for _, m := range marks {
		if _, has := m["ttl"]; has {
			t.Fatalf("child marker carries a ttl %v", m)
		}
	}
	parentHas1h := false
	for _, m := range cacheMarkers(provider.ApplyAnthropicCacheControl(ttlWireMessages(), "1h", false, "", false)) {
		parentHas1h = parentHas1h || m["ttl"] == "1h"
	}
	if !parentHas1h {
		t.Fatal("parent 1h layout carries no 1h marker")
	}
}

func TestDisabledAnd5mAreLeftAlone(t *testing.T) {
	for _, ttl := range []string{"", "5m"} {
		child := &AIAgent{cacheTTL: ttl}
		applyChildCacheTTL(child)
		if child.cacheTTL != ttl {
			t.Fatalf("ttl %q became %q", ttl, child.cacheTTL)
		}
	}
}

func capChild(window int, capTokens any) *AIAgent {
	o := DefaultContextCompressorOptions("anthropic/claude-fable-5.1")
	o.ConfigContextLength, o.ThresholdTokensCap = &window, capTokens
	return &AIAgent{contextCompressor: NewContextCompressor(context.Background(), o)}
}

func capThreshold(a *AIAgent) int { return a.contextCompressor.(*ContextCompressor).ThresholdTokens() }

func TestDefaultIsNoCapChildKeepsTheRatioTrigger(t *testing.T) {
	child := capChild(1_000_000, nil)
	for _, cfg := range []map[string]any{{}, {"compression_threshold_tokens": 0}} {
		if err := applyChildCompressionCap(child, cfg); err != nil || capThreshold(child) != 500_000 {
			t.Fatalf("cfg %v threshold %d err %v", cfg, capThreshold(child), err)
		}
	}
}

func TestExplicitCapIsTheLowerOfDelegationAndGlobalAndNeverRaises(t *testing.T) {
	child := capChild(1_000_000, nil)
	if err := applyChildCompressionCap(child, map[string]any{"compression_threshold_tokens": 300_000}); err != nil || capThreshold(child) != 300_000 {
		t.Fatalf("explicit cap threshold %d", capThreshold(child))
	}
	child = capChild(1_000_000, 150_000)
	if err := applyChildCompressionCap(child, map[string]any{"compression_threshold_tokens": 200_000}); err != nil || capThreshold(child) != 150_000 {
		t.Fatalf("global cap threshold %d", capThreshold(child))
	}
	small := capChild(128_000, nil)
	before := capThreshold(small)
	if err := applyChildCompressionCap(small, map[string]any{"compression_threshold_tokens": 200_000}); err != nil || capThreshold(small) != before {
		t.Fatalf("cap above the ratio trigger moved it %d to %d", before, capThreshold(small))
	}
}

func TestConfigValuesAreValidatedNotCoerced(t *testing.T) {
	for _, bad := range []any{true, "200k", 5, 15_999, -1, 1.5} {
		if got, err := childCompressionCapTokens(bad); got != nil || err != nil {
			t.Fatalf("%v accepted as %v %v", bad, got, err)
		}
	}
	for _, off := range []any{nil, 0, false} {
		if got, err := childCompressionCapTokens(off); got != nil || err != nil {
			t.Fatalf("%v read as a cap", off)
		}
	}
	if got, _ := childCompressionCapTokens(16_000); got == nil || *got != 16_000 {
		t.Fatal("16000 refused")
	}
	if got, _ := childCompressionCapTokens(300_000.0); got == nil || *got != 300_000 {
		t.Fatal("300000.0 refused")
	}
	var refusal *delegateValueError
	if _, err := childCompressionCapTokens(math.NaN()); !errors.As(err, &refusal) {
		t.Fatalf("NaN is not the int() ValueError %v", err)
	}
	child := capChild(1_000_000, nil)
	if err := applyChildCompressionCap(child, map[string]any{"compression_threshold_tokens": true}); err != nil || capThreshold(child) != 500_000 {
		t.Fatalf("true moved the trigger to %d", capThreshold(child))
	}
}

func TestBindSubagentParentDoesNotPinAgent(t *testing.T) {
	ctx, collected := func() (context.Context, func() bool) {
		a := &AIAgent{sessionID: "pinned"}
		ctx := bindSubagentParent(context.Background(), a)
		if plugins.GetActiveSubagentParent(ctx) != a {
			t.Fatal("bound parent does not resolve")
		}
		done := make(chan struct{})
		runtime.AddCleanup(a, func(ch chan struct{}) { close(ch) }, done)
		return ctx, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		}
	}()
	if plugins.GetActiveSubagentParent(context.Background()) != nil {
		t.Fatal("an unbound context resolves a parent")
	}
	for i := 0; i < 20 && !collected(); i++ {
		runtime.GC()
		runtime.Gosched()
	}
	if !collected() || plugins.GetActiveSubagentParent(ctx) != nil {
		t.Fatal("context snapshot still pins the agent")
	}
}

func TestSessionJoinsDelegationFollowsTheCarriedSchema(t *testing.T) {
	built := tools.DelegateTaskSchema
	joined := tools.OneshotDelegateFunction(map[string]any{"name": "delegate_task",
		"description": "x " + tools.OneshotDescriptionDelivery, "parameters": built["parameters"]})
	inline := &AIAgent{tools: []any{map[string]any{"type": "function", "function": joined}}}
	if !sessionJoinsDelegation(inline) {
		t.Fatal("joined schema does not join")
	}
	background := &AIAgent{tools: []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "delegate_task", "description": "END YOUR TURN", "parameters": built["parameters"]}}}}
	if sessionJoinsDelegation(background) || sessionJoinsDelegation(&AIAgent{}) {
		t.Fatal("background or absent delegate_task joins")
	}
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	if !tools.AgentIsOneshot(context.Background(), &inline.oneshotSession, nil) {
		t.Fatal("one shot marker not frozen")
	}
	if sessionJoinsDelegation(inline) {
		t.Fatal("a one shot session joins through the gateway path")
	}
}

func TestAgentConnectionSurfaceFollowsTheAgentKind(t *testing.T) {
	if agentConnectionSurface(&AIAgent{connectionCallback: func() {}}) != "card" ||
		agentConnectionSurface(&AIAgent{sideAgent: true}) != "side" || agentConnectionSurface(&AIAgent{}) != "link" {
		t.Fatal("surface")
	}
}

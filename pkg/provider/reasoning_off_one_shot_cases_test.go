package provider

import (
	"reflect"
	"testing"
)

// Minimal agent surface ReasoningConfigForWire needs, the tests/agent _AgentStandIn shape
func reasoningStandIn(cfg map[string]any) *fakeChatAgent {
	return &fakeChatAgent{reasoning: cfg}
}

func TestReasoningOffOneShotOverrideFlagConsumedExactlyOnce(t *testing.T) {
	user := map[string]any{"enabled": true, "effort": "high"}
	a := reasoningStandIn(map[string]any{"enabled": true, "effort": "high"})
	if got := ReasoningConfigForWire(t.Context(), a); !reflect.DeepEqual(got, user) {
		t.Fatalf("untouched config %v", got)
	}
	a.ephemeralOff = true
	cfg := ReasoningConfigForWire(t.Context(), a)
	if cfg["enabled"] != false || cfg["effort"] != "none" {
		t.Fatalf("override %v", cfg)
	}
	if a.ephemeralOff {
		t.Fatal("The one-shot override must be consumed by the first call.")
	}
	if got := ReasoningConfigForWire(t.Context(), a); !reflect.DeepEqual(got, user) {
		t.Fatalf("later calls keep the user config, got %v", got)
	}
}

func TestReasoningOffOneShotOverrideFlagWithNoUserReasoningConfig(t *testing.T) {
	a := reasoningStandIn(nil)
	a.ephemeralOff = true
	if got := ReasoningConfigForWire(t.Context(), a); !reflect.DeepEqual(got, map[string]any{"enabled": false, "effort": "none"}) {
		t.Fatalf("cfg %v", got)
	}
}

func TestReasoningOffOneShotOverrideRejectedDisableResendsUsersConfigVerbatim(t *testing.T) {
	a := reasoningStandIn(map[string]any{"enabled": true, "effort": "high"})
	a.disableRej, a.ephemeralOff = true, true
	if got := ReasoningConfigForWire(t.Context(), a); !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "high"}) {
		t.Fatalf("cfg %v", got)
	}
	if a.ephemeralOff {
		t.Fatal("the ephemeral override must be discarded")
	}
	a.reasoning = map[string]any{"enabled": false}
	if got := ReasoningConfigForWire(t.Context(), a); got != nil {
		t.Fatalf("a config that is itself a disable is omitted, got %v", got)
	}
}

func TestReasoningOffOneShotOverrideMandatoryReasoningRouteStepsADisableUpToTheFloor(t *testing.T) {
	a := reasoningStandIn(map[string]any{"enabled": false})
	a.disableRej, a.flr = true, true
	if got := ReasoningConfigForWire(t.Context(), a); !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": ReasoningFloorEffort}) {
		t.Fatalf("floored %v", got)
	}
	a.reasoning = map[string]any{"enabled": true, "effort": "high"}
	if got := ReasoningConfigForWire(t.Context(), a); !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": "high"}) {
		t.Fatalf("verbatim %v", got)
	}
}

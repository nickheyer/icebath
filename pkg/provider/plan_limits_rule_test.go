package provider

import "testing"

var plPricing = map[string]any{
	"example/model-a":         map[string]any{"prompt": "0.00000001", "completion": "0.0000001", "name": "Model A"},
	"example/model-b":         map[string]any{"prompt": "0.0000004", "completion": "0.000004", "name": "Model B"},
	"example/model-c":         map[string]any{"prompt": "0.000005", "completion": "0.000025", "top_tier": true, "name": "Model C"},
	"example/model-free:free": map[string]any{"prompt": "0", "completion": "0", "name": "Model Free"},
}

const plReset = int64(1_900_000_000_000)

func plGauge(id string, usedBp int, scope string, resets int64) WindowGauge {
	return WindowGauge{ID: id, Scope: scope, Label: id, UsedBp: usedBp, ResetsAtMs: &resets}
}

func plSet(dayBp, weekBp, topBp int) []WindowGauge {
	return []WindowGauge{plGauge("day", dayBp, ScopeAll, plReset), plGauge("week", weekBp, ScopeAll, plReset), plGauge("top_tier", topBp, ScopeTopTier, plReset)}
}

func TestPlanLimitTheRule(t *testing.T) {
	dayFull, topFull, healthy := plSet(10_000, 4_000, 2_000), plSet(3_000, 2_000, 10_000), plSet(3_000, 2_000, 2_000)
	for _, m := range []string{"example/model-free:free", "any/x:free"} {
		if !IsPlanFree(m, plPricing) || IsBlocked(m, dayFull, plPricing) || IsBlocked(m, topFull, plPricing) {
			t.Fatalf("free model %s is never blocked", m)
		}
	}
	for _, m := range []string{"example/model-a", "example/model-b", "example/model-c"} {
		if !IsBlocked(m, dayFull, plPricing) {
			t.Fatalf("full all scope blocks %s", m)
		}
	}
	if !IsBlocked("example/model-c", topFull, plPricing) || IsBlocked("example/model-b", topFull, plPricing) || IsBlocked("example/model-a", topFull, plPricing) {
		t.Fatal("full top tier blocks only marked models")
	}
	if IsTopTier("example/model-b", plPricing) || IsTopTier("unknown/model", plPricing) || IsTopTier("example/model-c", map[string]any{}) {
		t.Fatal("unmarked or unpriced is not top tier")
	}
	if !IsTopTier("example/model-d", map[string]any{"example/model-d": map[string]any{"prompt": "0.000005", "completion": "0.000025", "usage_level": "top_tier"}}) ||
		IsTopTier("example/model-e:free", map[string]any{"example/model-e:free": map[string]any{"prompt": "0", "completion": "0", "usage_level": "top_tier"}}) {
		t.Fatal("usage level word marks top tier and a free row never is")
	}
	for m := range plPricing {
		if IsBlocked(m, healthy, plPricing) {
			t.Fatalf("nothing full blocks %s", m)
		}
	}
	gauges := []WindowGauge{plGauge("day", 10_000, ScopeAll, 1_000), plGauge("week", 10_000, ScopeAll, plReset)}
	if g := BlockingGauge(gauges, false, false); g == nil || g.ID != "week" {
		t.Fatal("the gauge that frees up last binds")
	}
	if PlanModelLabel("example/model-a", plPricing) != "Model A" || PlanModelLabel("no/name", plPricing) != "no/name" {
		t.Fatal("labels come from the catalog")
	}
}

func TestPlanLimitWindowNoticePolicy(t *testing.T) {
	s := NewCreditsState()
	s.DenominatorKind, s.Gauges, s.FromHeader, s.SpendSource = "usage_window", plSet(10_000, 4_000, 2_000), true, ""
	latch := NewCreditsLatch()
	show, _ := EvaluateCreditsNotices(s, latch, false, false)
	if n := cpFind(show, CreditsWindowLimitKey); n == nil || n.Level != "error" {
		t.Fatalf("full day allowance shows the limit line, got %v", cpKeys(show))
	}
	s.Gauges = plSet(3_000, 2_000, 2_000)
	show, clear := EvaluateCreditsNotices(s, latch, false, false)
	if cpFind(show, CreditsRestoredKey) == nil || len(clear) == 0 || clear[0] != CreditsWindowLimitKey {
		t.Fatalf("reset restores access, got %v %v", cpKeys(show), clear)
	}
}

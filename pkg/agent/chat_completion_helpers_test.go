package agent

import "testing"

func TestCapToRunBudgetHalvesTheRemainingBudgetWithASixtySecondFloor(t *testing.T) {
	a := &AIAgent{}
	if capToRunBudget(a, 300) != 300 {
		t.Fatal("no budget must leave the timeout alone")
	}
	a.runBudgetSeconds, a.runBudgetStartedAt = 100, nowSeconds()-5
	if got := capToRunBudget(a, 300); got != 60 {
		t.Fatalf("near exhausted budget %v", got)
	}
	a.runBudgetSeconds, a.runBudgetStartedAt = 1000, nowSeconds()
	if got := capToRunBudget(a, 300); got != 300 {
		t.Fatalf("roomy budget %v", got)
	}
	if got := capToRunBudget(a, 900); got < 499 || got > 500 {
		t.Fatalf("half remaining %v", got)
	}
}

func TestHighEffortSilenceFloorAppliesFromHighEffortUpward(t *testing.T) {
	cases := []struct {
		cfg  map[string]any
		want float64
	}{
		{nil, 0},
		{map[string]any{"enabled": true, "effort": "high"}, 300},
		{map[string]any{"enabled": true, "effort": " XHigh "}, 300},
		{map[string]any{"effort": "ultra"}, 300},
		{map[string]any{"enabled": true, "effort": "medium"}, 0},
		{map[string]any{"enabled": false, "effort": "high"}, 0},
		{map[string]any{"enabled": true, "effort": "turbo"}, 0},
		{map[string]any{"enabled": true}, 0},
	}
	for _, c := range cases {
		if got := highEffortSilenceFloor(&AIAgent{reasoningConfig: c.cfg}); got != c.want {
			t.Fatalf("%v floor %v want %v", c.cfg, got, c.want)
		}
	}
}

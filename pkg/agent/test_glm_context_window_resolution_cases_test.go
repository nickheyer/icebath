package agent

import (
	"context"
	"testing"
)

func TestFeasibilityCheckInheritsMainWindowWhenAuxIsTheMainModel(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	client := &fakeFeasibilityClient{baseURL: "https://relay.example/v1/", apiKey: "k"}
	a, c, probe := newFeasibilityAgent(t, 1_000_000, 0.50, "lean", client, "uncatalogued-relay-model", 128_000)
	a.model, a.baseURL, a.provider, a.auxCompressionContextLengthConfig = "uncatalogued-relay-model", "https://relay.example/v1", "custom", nil
	if c.ThresholdTokens() != 500_000 {
		t.Fatalf("threshold before %d", c.ThresholdTokens())
	}
	if err := checkCompressionModelFeasibility(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if c.ThresholdTokens() != 500_000 || a.lastFeasibilityNotice != nil || len(probe.ctxRequests) != 0 {
		t.Fatalf("lowered: threshold=%d notice=%v lookups=%d", c.ThresholdTokens(), a.lastFeasibilityNotice, len(probe.ctxRequests))
	}
	zero := 0
	a.auxCompressionContextLengthConfig = &zero
	if err := checkCompressionModelFeasibility(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if len(probe.ctxRequests) != 1 || c.ThresholdTokens() > 128_000 {
		t.Fatalf("explicit zero pin must re-resolve: lookups=%d threshold=%d", len(probe.ctxRequests), c.ThresholdTokens())
	}
}

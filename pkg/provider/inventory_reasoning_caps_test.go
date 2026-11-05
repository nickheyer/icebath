package provider

import (
	"context"
	"testing"
	"time"
)

// Fixes the aggregator reasoning catalog of one provider, no warmers and an empty models.dev cache
func patchReasoningCatalog(t *testing.T, provider string, byModel map[string]*ReasoningCapabilities) {
	t.Helper()
	invTestHome(t)
	savedCache, savedTime := modelsDevCache, modelsDevCacheTime
	modelsDevCache, modelsDevCacheTime = map[string]any{}, time.Now()
	t.Cleanup(func() { modelsDevCache, modelsDevCacheTime = savedCache, savedTime })
	read := func(_ context.Context, model string, _ time.Duration, _ bool) (ReasoningCapabilities, bool) {
		if caps := byModel[model]; caps != nil {
			return *caps, true
		}
		return ReasoningCapabilities{}, false
	}
	if provider == "openrouter" {
		inventoryOpenRouterCapsFn = read
	} else {
		inventoryNousCapsFn = read
	}
}

func modelCaps(t *testing.T, slug, model string) map[string]any {
	t.Helper()
	rows := []map[string]any{{"slug": slug, "models": []any{model}}}
	if err := applyCapabilities(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	return rows[0]["capabilities"].(map[string]any)[model].(map[string]any)
}

var optionalReasoning = &ReasoningCapabilities{SupportsReasoning: true, SupportedEfforts: []string{"xhigh", "high"}}

func TestOptionalReasoningRouteCanDisable(t *testing.T) {
	patchReasoningCatalog(t, "nous", map[string]*ReasoningCapabilities{"deepseek/deepseek-v4-pro": optionalReasoning})
	if caps := modelCaps(t, "nous", "deepseek/deepseek-v4-pro"); caps["can_disable_reasoning"] != true {
		t.Fatalf("caps %v", caps)
	}
}

func TestAdvertisedEffortsNeverReachThePicker(t *testing.T) {
	patchReasoningCatalog(t, "nous", map[string]*ReasoningCapabilities{"deepseek/deepseek-v4-pro": optionalReasoning})
	if caps := modelCaps(t, "nous", "deepseek/deepseek-v4-pro"); caps["supported_efforts"] != nil {
		t.Fatalf("caps %v", caps)
	}
}

func TestNonReasoningRouteOffersNoReasoningControls(t *testing.T) {
	patchReasoningCatalog(t, "nous", map[string]*ReasoningCapabilities{"moonshotai/kimi-k3-instruct": {}})
	caps := modelCaps(t, "nous", "moonshotai/kimi-k3-instruct")
	if _, ok := caps["can_disable_reasoning"]; caps["reasoning"] != false || ok {
		t.Fatalf("caps %v", caps)
	}
}

func TestReasoningMandatoryRouteCannotDisable(t *testing.T) {
	patchReasoningCatalog(t, "nous", map[string]*ReasoningCapabilities{"z-ai/glm-5.3": {SupportsReasoning: true,
		SupportedEfforts: []string{"max", "high", "low"}, Mandatory: true}})
	if caps := modelCaps(t, "nous", "z-ai/glm-5.3"); caps["can_disable_reasoning"] != false {
		t.Fatalf("caps %v", caps)
	}
}

func TestUnlistedModelStatesNoRestriction(t *testing.T) {
	patchReasoningCatalog(t, "nous", nil)
	caps := modelCaps(t, "nous", "mystery/model")
	_, efforts := caps["supported_efforts"]
	_, disable := caps["can_disable_reasoning"]
	if efforts || disable || caps["reasoning"] != true {
		t.Fatalf("caps %v", caps)
	}
}

func TestProvidersWithoutAReasoningCatalogAreUntouched(t *testing.T) {
	patchReasoningCatalog(t, "nous", map[string]*ReasoningCapabilities{"gpt-5.6": {SupportsReasoning: true,
		SupportedEfforts: []string{"high"}, Mandatory: true}})
	caps := modelCaps(t, "openai-api", "gpt-5.6")
	_, efforts := caps["supported_efforts"]
	_, disable := caps["can_disable_reasoning"]
	if efforts || disable {
		t.Fatalf("caps %v", caps)
	}
}

func TestOpenrouterUsesItsOwnCatalog(t *testing.T) {
	patchReasoningCatalog(t, "openrouter", map[string]*ReasoningCapabilities{"x-ai/grok-5": {SupportsReasoning: true, Mandatory: true}})
	if caps := modelCaps(t, "openrouter", "x-ai/grok-5"); caps["can_disable_reasoning"] != false {
		t.Fatalf("caps %v", caps)
	}
}

func TestCatalogFailureNeverBreaksThePicker(t *testing.T) {
	patchReasoningCatalog(t, "nous", nil)
	inventoryNousCapsFn = func(context.Context, string, time.Duration, bool) (ReasoningCapabilities, bool) {
		return ReasoningCapabilities{}, false
	}
	caps := modelCaps(t, "nous", "deepseek/deepseek-v4-pro")
	if _, ok := caps["supported_efforts"]; ok || caps["reasoning"] != true {
		t.Fatalf("caps %v", caps)
	}
}

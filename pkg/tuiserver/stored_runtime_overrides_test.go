package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Session row object of a JSON literal
func rowOf(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	row, err := pyjson.LoadsDict(text)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestStoredSessionRuntimeOverridesSkipsBareBillingProvider(t *testing.T) {
	toolsetHome(t, "")
	ctx := context.Background()
	ov := storedSessionRuntimeOverrides(ctx, rowOf(t, `{"model": "my-model", "billing_provider": "custom"}`))
	if _, present := ov["provider_override"]; present {
		t.Fatalf("custom bucket restored: %v", ov)
	}
	if mo := ov["model_override"].(map[string]any); mo["provider"] != nil {
		t.Fatalf("model_override provider = %v", mo["provider"])
	}
	for _, bare := range []string{"auto", "custom"} {
		ov = storedSessionRuntimeOverrides(ctx, rowOf(t, `{"model": "m", "billing_provider": "`+bare+`"}`))
		if _, present := ov["provider_override"]; present {
			t.Fatalf("%s bucket restored: %v", bare, ov)
		}
	}
	ov = storedSessionRuntimeOverrides(ctx, rowOf(t, `{"model": "m", "billing_provider": "anthropic"}`))
	if ov["provider_override"] != "anthropic" || ov["model_override"].(map[string]any)["provider"] != "anthropic" {
		t.Fatalf("anthropic = %v", ov)
	}
	toolsetHome(t, "custom_providers:\n  - name: myendpoint\n    base_url: https://myendpoint.invalid/v1\n    api_key: sk-test\n    model: m\n")
	ov = storedSessionRuntimeOverrides(ctx, rowOf(t, `{"model": "m", "billing_provider": "custom", "model_config": {"provider": "custom:myendpoint"}}`))
	if ov["provider_override"] != "custom:myendpoint" || ov["model_override"].(map[string]any)["provider"] != "custom:myendpoint" {
		t.Fatalf("named custom = %v", ov)
	}
}

func TestStoredSessionRuntimeOverridesRestoresExplicitNormalTier(t *testing.T) {
	toolsetHome(t, "")
	overrides := storedSessionRuntimeOverrides(context.Background(), rowOf(t, `{"model": "gpt-5.4", "model_config": {"service_tier": "normal"}}`))
	tier, present := overrides["service_tier_override"]
	if !present || tier != "" {
		t.Fatalf("overrides = %v", overrides)
	}
}

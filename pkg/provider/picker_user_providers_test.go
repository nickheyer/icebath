package provider

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

func TestSwitchModelResolvesUserProviderCredentials(t *testing.T) {
	ctx, _ := switchAliasHome(t, "providers:\n  local-ollama:\n    api: http://localhost:11434/v1\n    name: Local Ollama\n    default_model: minimax-m2.7:cloud\n")
	fakeSwitchPipeline(t, true)
	userProviders := map[string]any{"local-ollama": map[string]any{
		"api": "http://localhost:11434/v1", "name": "Local Ollama", "default_model": "minimax-m2.7:cloud"}}
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "kimi-k2.5:cloud", CurrentProvider: "local-ollama",
		CurrentBaseURL: "http://localhost:11434/v1", UserProviders: userProviders})
	if !res.Success || res.ErrorMessage != "" {
		t.Fatalf("result %+v", res)
	}
}

func TestSectionThreeProbesKeylessEndpointWithSingularDefaultModel(t *testing.T) {
	ctx, calls := pickerTestHome(t, func(string, string) ([]string, bool) {
		return []string{"live-model-1", "live-model-2", "live-model-3"}, true
	})
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "local-ollama", MaxModels: intPtr(50),
		UserProviders: map[string]any{"local-ollama": map[string]any{
			"name": "Local Ollama", "api": "http://localhost:11434/v1", "default_model": "llama3"}}})
	if len(*calls) == 0 || (*calls)[0].apiKey != "" {
		t.Fatalf("singular default_model must not suppress live discovery, calls %+v", *calls)
	}
	row := pickerRow(rows, "local-ollama")
	if !reflect.DeepEqual(rowModels(row), []string{"live-model-1", "live-model-2", "live-model-3"}) || row["total_models"] != 3 {
		t.Fatalf("row %v", row)
	}
}

// Keeps only the azure-foundry overlay and empties the models.dev provider mapping
func azureFoundryOnlyPicker(t *testing.T, cached []string) context.Context {
	t.Helper()
	overlay := icebathOverlays["azure-foundry"]
	ctx, _ := pickerTestHome(t, nil)
	savedMapping := providerToModelsDev
	providerToModelsDev = map[string]string{}
	t.Cleanup(func() { providerToModelsDev = savedMapping })
	icebathOverlays = map[string]IcebathOverlay{"azure-foundry": overlay}
	cachedProviderModelIDsFn = func(context.Context, string, CachedModelsOptions) []string { return slices.Clone(cached) }
	return ctx
}

func TestOverlayProviderRowMergesConfiguredModels(t *testing.T) {
	ctx := azureFoundryOnlyPicker(t, []string{"gpt-5.6-sol", "shared"})
	t.Setenv("AZURE_FOUNDRY_API_KEY", "test-key")
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "azure-foundry", MaxModels: intPtr(50),
		UserProviders: map[string]any{"azure-foundry": map[string]any{"models": []any{"gpt-5.5", "shared", "gpt-4.1-mini"}}}})
	row := pickerRow(rows, "azure-foundry")
	if row == nil || row["source"] != "hermes" {
		t.Fatalf("row %v", row)
	}
	if !reflect.DeepEqual(rowModels(row), []string{"gpt-5.5", "shared", "gpt-4.1-mini", "gpt-5.6-sol"}) || row["total_models"] != 4 {
		t.Fatalf("row %v", row)
	}
}

func TestEntraOnlyAzureFoundryRowIsListedWithoutAPIKey(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		listed  bool
	}{{"https://r.openai.azure.com/openai/v1", true}, {"", false}} {
		t.Run(tc.baseURL, func(t *testing.T) {
			ctx := azureFoundryOnlyPicker(t, []string{"gpt-5.6-sol"})
			savedCfg := modelConfigDictFn
			modelConfigDictFn = func(context.Context) map[string]any {
				return map[string]any{"provider": "azure-foundry", "auth_mode": "entra_id", "base_url": tc.baseURL}
			}
			t.Cleanup(func() { modelConfigDictFn = savedCfg })
			t.Setenv("AZURE_FOUNDRY_API_KEY", "")
			t.Setenv("AZURE_FOUNDRY_BASE_URL", "")
			rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{MaxModels: intPtr(50)})
			if (pickerRow(rows, "azure-foundry") != nil) != tc.listed {
				t.Fatalf("listed want %v rows %v", tc.listed, rows)
			}
			slugs, err := collectAuthedProviderSlugs(ctx, map[string]any{}, nil)
			if err != nil || slices.Contains(slugs, "azure-foundry") != tc.listed {
				t.Fatalf("collected %v %v want %v", slugs, err, tc.listed)
			}
		})
	}
}

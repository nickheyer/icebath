package provider

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func listPickerRow(slug string, models []string) map[string]any {
	return map[string]any{"slug": slug, "name": slug, "is_current": false, "is_user_defined": false,
		"models": stringsToAny(models), "total_models": len(models), "source": "built-in"}
}

// Replaces the authenticated listing and the OpenRouter catalog the picker filter reads
func fakePickerListing(t *testing.T, list func(AuthenticatedProvidersOptions) []map[string]any, openrouter []CatalogModel) {
	t.Helper()
	pickerTestHome(t, nil)
	savedList, savedOR := pickerListAuthenticatedFn, pickerOpenRouterModelsFn
	t.Cleanup(func() { pickerListAuthenticatedFn, pickerOpenRouterModelsFn = savedList, savedOR })
	if list != nil {
		pickerListAuthenticatedFn = func(_ context.Context, opts AuthenticatedProvidersOptions) ([]map[string]any, error) {
			return list(opts), nil
		}
	}
	pickerOpenRouterModelsFn = func(context.Context, FetchModelsOptions) []CatalogModel { return openrouter }
}

func TestPassthroughKwargsToBase(t *testing.T) {
	var captured AuthenticatedProvidersOptions
	fakePickerListing(t, func(opts AuthenticatedProvidersOptions) []map[string]any {
		captured = opts
		return nil
	}, nil)
	user := map[string]any{"foo": map[string]any{"api": "http://x"}}
	custom := []any{map[string]any{"name": "bar", "base_url": "http://y"}}
	if _, err := ListPickerProviders(context.Background(), PickerProvidersOptions{CurrentProvider: "openrouter",
		CurrentBaseURL: "http://x", CurrentModel: "openai/gpt-5.4", UserProviders: user, CustomProviders: custom,
		MaxModels: intPtr(12)}); err != nil {
		t.Fatal(err)
	}
	if captured.CurrentProvider != "openrouter" || captured.CurrentBaseURL != "http://x" ||
		captured.CurrentModel != "openai/gpt-5.4" || !reflect.DeepEqual(captured.UserProviders, user) ||
		!reflect.DeepEqual(captured.CustomProviders, custom) || captured.MaxModels == nil || *captured.MaxModels != 12 {
		t.Fatalf("captured %+v", captured)
	}
}

func TestCurrentCustomEndpointPassthroughMarksCurrentRow(t *testing.T) {
	fakePickerListing(t, nil, nil)
	savedMap := providerToModelsDev
	providerToModelsDev = map[string]string{}
	t.Cleanup(func() { providerToModelsDev = savedMap })
	rows, err := ListPickerProviders(context.Background(), PickerProvidersOptions{CurrentProvider: "custom:ollama",
		CurrentBaseURL: "http://localhost:11434/v1", CurrentModel: "glm-5.1", UserProviders: map[string]any{},
		CustomProviders: []any{
			map[string]any{"name": "Ollama — GLM 5.1", "base_url": "http://localhost:11434/v1", "api_key": "ollama",
				"model": "glm-5.1", "discover_models": false},
			map[string]any{"name": "Ollama — Qwen3", "base_url": "http://localhost:11434/v1", "api_key": "ollama",
				"model": "qwen3", "discover_models": false}},
		MaxModels: intPtr(50)})
	if err != nil {
		t.Fatal(err)
	}
	var custom []map[string]any
	for _, r := range rows {
		if pyTruthy(r["is_user_defined"]) {
			custom = append(custom, r)
		}
	}
	if len(custom) != 1 || custom[0]["slug"] != "custom:ollama" || custom[0]["is_current"] != true ||
		!reflect.DeepEqual(rowModels(custom[0]), []string{"glm-5.1", "qwen3"}) {
		t.Fatalf("custom rows %v", custom)
	}
}

func TestCuratedOpenrouterRowKeepsFreeTailPastMaxModels(t *testing.T) {
	var curated []CatalogModel
	var curatedIDs, other []string
	for i := 0; i < 55; i++ {
		curated = append(curated, CatalogModel{ID: fmt.Sprintf("vendor/model-%d", i)})
	}
	curated = append(curated, CatalogModel{ID: "stealth/free-model", Description: "free, stealth model"})
	for _, m := range curated {
		curatedIDs = append(curatedIDs, m.ID)
	}
	for i := 0; i < 60; i++ {
		other = append(other, fmt.Sprintf("other-%d", i))
	}
	fakePickerListing(t, func(AuthenticatedProvidersOptions) []map[string]any {
		deepseek := listPickerRow("deepseek", other[:50])
		deepseek["total_models"] = len(other)
		return []map[string]any{listPickerRow("openrouter", curatedIDs[:50]), deepseek}
	}, curated)
	rows, err := ListPickerProviders(context.Background(), PickerProvidersOptions{MaxModels: intPtr(50)})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowModels(pickerRow(rows, "openrouter")); !reflect.DeepEqual(got, curatedIDs) {
		t.Fatalf("openrouter %v", got)
	}
	if got := rowModels(pickerRow(rows, "deepseek")); len(got) != 50 {
		t.Fatalf("deepseek %d", len(got))
	}
}

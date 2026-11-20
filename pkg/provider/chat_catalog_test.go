package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func catalogItems() []any {
	return []any{
		map[string]any{"id": "wan2.7-image-pro"},
		map[string]any{"id": "acme/text-to-image"},
		map[string]any{"id": "gen-1", "capabilities": map[string]any{"type": "image"}},
		map[string]any{"id": "clip-gen", "capabilities": map[string]any{"type": "video-generation"}},
		map[string]any{"id": "router/flux-pro", "architecture": map[string]any{"output_modalities": []any{"image"}}},
		map[string]any{"id": "qwen3.7-plus", "capabilities": map[string]any{"type": "chat"}},
		map[string]any{"id": "qwen-vl-max"},
		map[string]any{"id": "missing-capabilities"},
		map[string]any{"id": "malformed-capabilities", "capabilities": "image"},
		map[string]any{"id": "unknown-kind", "capabilities": map[string]any{"type": "future-kind"}},
	}
}

func TestLiveCatalogDropsCapabilityTypedAndNameShapedGenerationModels(t *testing.T) {
	metaTest(t)
	got := ChatCatalogIDs(catalogItems())
	want := []string{"qwen3.7-plus", "qwen-vl-max", "missing-capabilities", "malformed-capabilities", "unknown-kind"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	srv := modelsResponseServer(t, catalogItems())
	profile := Profile{Name: "alibaba-token-plan", BaseURL: srv.URL, ModelListing: true}
	if live, ok := profile.FetchModels(context.Background(), "test-key", srv.URL, 5*time.Second); !ok || !reflect.DeepEqual(live, want) {
		t.Fatalf("fetch_models got %v %v", live, ok)
	}
}

func TestSharedChatCatalogDropsCachedGenerationIds(t *testing.T) {
	metaTest(t)
	got := WithoutGenerationModels([]string{"qwen3.7-plus", "wan2.7-image-pro", "acme/text-to-video", "qwen-vl-max"})
	if !reflect.DeepEqual(got, []string{"qwen3.7-plus", "qwen-vl-max"}) {
		t.Fatalf("got %v", got)
	}
	kept := []string{"qwen3.7-plus", "qwen-vl-max"}
	if out := WithoutGenerationModels(kept); !reflect.DeepEqual(out, kept) {
		t.Fatalf("unchanged list must be returned as is, got %v", out)
	}
	newTestHome(t)
	ResetProviderModelsCaches()
	savedFP, savedLoad, savedSpawn := credentialFingerprintFn, loadProviderModelsCacheFn, spawnSWRRefreshFn
	t.Cleanup(func() {
		credentialFingerprintFn, loadProviderModelsCacheFn, spawnSWRRefreshFn = savedFP, savedLoad, savedSpawn
		ResetProviderModelsCaches()
	})
	credentialFingerprintFn = func(context.Context, string) string { return "fp" }
	loadProviderModelsCacheFn = func(context.Context) map[string]any {
		return map[string]any{"alibaba-token-plan": map[string]any{"fp": "fp", "at": float64(1e10),
			"models": stringsToAny([]string{"qwen3.7-plus", "wan2.7-image-pro", "acme/text-to-video", "qwen-vl-max"})}}
	}
	spawnSWRRefreshFn = func(context.Context, string, func() (map[string]any, bool)) {}
	if got := CachedProviderModelIDs(context.Background(), "alibaba-token-plan", CachedModelsOptions{}); !reflect.DeepEqual(got, []string{"qwen3.7-plus", "qwen-vl-max"}) {
		t.Fatalf("cached_provider_model_ids %v", got)
	}
}

func TestDirectSwitchRejectsAGenerationModelAndKeepsAVisionChatModel(t *testing.T) {
	metaTest(t)
	if !IsKnownNonChatModel("vendor/image-pro") {
		t.Fatal("vendor/image-pro must be rejected as non chat")
	}
	if IsKnownNonChatModel("qwen-vl-max") {
		t.Fatal("qwen-vl-max is a vision chat model")
	}
	ctx := switchValidateHome(t, "")
	fakeSwitchPipeline(t, true)
	state := func(id string) *modelSwitch {
		return catalogSwitchState(id, "alibaba-token-plan", "https://token-plan.example.test/v1", "Alibaba Cloud (Token Plan)")
	}
	rejected := validateSwitch(ctx, state("vendor/image-pro"))
	kept := validateSwitch(ctx, state("qwen-vl-max"))
	if rejected == nil || rejected.Success || !strings.Contains(strings.ToLower(rejected.ErrorMessage), "chat") {
		t.Fatalf("rejected %+v", rejected)
	}
	if kept != nil {
		t.Fatalf("kept %+v", kept)
	}
}

func TestCapabilityTypedIDIsRejectedOnceTheCatalogHasSeenIt(t *testing.T) {
	metaTest(t)
	if IsKnownNonChatModel("gen-1") {
		t.Fatal("gen-1 has no generation token before the catalog proves it")
	}
	ids := ChatCatalogIDs([]any{map[string]any{"id": "gen-1", "capabilities": map[string]any{"type": "image"}}})
	if len(ids) != 0 {
		t.Fatalf("generation row must leave the catalog, got %v", ids)
	}
	if !IsKnownNonChatModel("GEN-1") {
		t.Fatal("a parsed generation id is non chat in any casing")
	}
	srv := modelsResponseServer(t, []any{map[string]any{"id": "gen-1", "capabilities": map[string]any{"type": "image"}}})
	profile := Profile{Name: "example", BaseURL: srv.URL, ModelListing: true}
	if live, ok := profile.FetchModels(context.Background(), "test-key", srv.URL, 5*time.Second); !ok || len(live) != 0 {
		t.Fatalf("fetch_models got %v %v", live, ok)
	}
	ctx := switchValidateHome(t, "")
	fakeSwitchPipeline(t, true)
	if rejected := validateSwitch(ctx, catalogSwitchState("gen-1", "example", "https://catalog.example.test/v1", "Example")); rejected == nil || rejected.Success {
		t.Fatalf("rejected %+v", rejected)
	}
}

func TestCatalogItemIsGenerationFailsOpen(t *testing.T) {
	cases := map[string]bool{
		"wan2.7-image-pro": true, "acme/text-to-image": true, "stable-diffusion-xl": true, "sdxl-turbo": true,
		"black-forest-labs/flux-1.1-pro": true, "video-gen-2": true, "qwen-vl-max": false, "vision-pro": false,
		"imagen-free": false, "myimage": false,
	}
	for id, want := range cases {
		if got := ModelIDIsGeneration(id); got != want {
			t.Fatalf("%s: got %v want %v", id, got, want)
		}
	}
	if CatalogItemIsGeneration("not-a-dict") {
		t.Fatal("non dict rows fail open")
	}
	if !CatalogItemIsGeneration(map[string]any{"id": "x", "tags": []any{"Image-Gen"}}) {
		t.Fatal("top level tags prove generation")
	}
	if !CatalogItemIsGeneration(map[string]any{"id": "x", "metadata": map[string]any{"tags": []any{"video"}}}) {
		t.Fatal("metadata tags prove generation")
	}
	if CatalogItemIsGeneration(map[string]any{"id": "x", "architecture": map[string]any{"output_modalities": []any{"image", "text"}}}) {
		t.Fatal("an image and text model stays chat")
	}
	if ChatCatalogIDs("nope") == nil || len(ChatCatalogIDs("nope")) != 0 {
		t.Fatal("non list payload yields an empty id list")
	}
}

// Serves a models listing of the given catalog items the way an OpenAI compatible endpoint does
func modelsResponseServer(t *testing.T, items []any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Direct switch state on the given provider for one requested model id
func catalogSwitchState(id, provider, baseURL, label string) *modelSwitch {
	return &modelSwitch{SwitchRoute: SwitchRoute{NewModel: id, TargetProvider: provider, ValidationHeaders: map[string]string{}},
		RawInput: id, CurrentProvider: provider, CurrentBaseURL: baseURL, ProviderLabel: label}
}

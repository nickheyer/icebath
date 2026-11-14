package tools_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The one managed image generation row of the catalog
func managedImageRow(t *testing.T) *pyjson.Object {
	t.Helper()
	for _, p := range tools.CategoryProvidersForTest("image_gen") {
		if v, _ := p.Get("managed_nous_feature"); v == "image_gen" {
			return p
		}
	}
	t.Fatal("no managed image row")
	return nil
}

// Swaps the subscription snapshot hook for one test
func swapNousFeatures(t *testing.T, f tools.NousToolFeatures) {
	t.Helper()
	prev := tools.ToolsConfigHooks.NousFeatures
	tools.ToolsConfigHooks.NousFeatures = func(context.Context, string, map[string]any, bool) tools.NousToolFeatures { return f }
	t.Cleanup(func() { tools.ToolsConfigHooks.NousFeatures = prev })
}

// Registers every bundled image generation provider the way plugin discovery loads them
func registerBundledImageProviders(t *testing.T) {
	t.Helper()
	media.ResetImageGenRegistry()
	t.Cleanup(media.ResetImageGenRegistry)
	for _, key := range plugins.BundledPluginKeys() {
		p, _ := plugins.BundledPlugin(key)
		if prov, ok := media.BundledImageGenProvider(p); ok {
			if err := media.RegisterImageGenProvider(prov, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestExactlyOneImageRowIsActiveForAManagedSelection(t *testing.T) {
	s, _ := casesSetup(t)
	registerBundledImageProviders(t)
	swapNousFeatures(t, tools.NousToolFeatures{Features: map[string]tools.NousToolFeature{"image_gen": {ManagedByNous: true}}})
	plugin := s.PluginImageGenProvidersForTest()
	if len(plugin) == 0 {
		t.Fatal("no bundled image generation rows")
	}
	rows := append(tools.CategoryProvidersForTest("image_gen"), plugin...)
	managed, _ := managedImageRow(t).Get("name")
	for _, model := range []string{"fal-ai/flux-2/klein/9b", "krea-2-medium", "google/gemini-3-pro-image"} {
		cfg := config.Tree{"image_gen": map[string]any{"provider": "nous", "model": model}}
		var active []any
		for _, r := range rows {
			ok, err := s.IsProviderActive(r, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				name, _ := r.Get("name")
				active = append(active, name)
			}
		}
		if len(active) != 1 || active[0] != managed {
			t.Fatalf("%s active %v", model, active)
		}
	}
}

func TestGuiModelCatalogForTheManagedRowSpansEveryManagedGateway(t *testing.T) {
	s, _ := casesSetup(t)
	registerBundledImageProviders(t)
	paid := true
	swapNousFeatures(t, tools.NousToolFeatures{Features: map[string]tools.NousToolFeature{},
		Account: &tools.NousToolAccount{LoggedIn: true, PaidServiceAccess: &paid, ToolGatewayEntitled: true, EntitledFor: func(string) bool { return true }}})
	plugin := tools.ResolveToolsetModelPluginForTest("image_gen", managedImageRow(t))
	catalog, def := s.ToolsetModelCatalogForTest("image_gen", plugin, config.Tree{})
	falIDs, _, _ := tools.ToolsConfigHooks.FALModels()
	if !slices.Contains(catalog, def) || !slices.Contains(falIDs, def) {
		t.Fatalf("default %q catalog %v", def, catalog)
	}
	for id := range media.KreaModelIDs() {
		if !slices.Contains(catalog, id) {
			t.Fatalf("krea model %s missing from %v", id, catalog)
		}
	}
	for _, id := range catalog {
		if strings.HasPrefix(id, "fal-ai/krea/") {
			t.Fatalf("Krea 2 must appear once, natively: %s", id)
		}
	}
}

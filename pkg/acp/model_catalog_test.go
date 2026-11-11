package acp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

func withConfig(t *testing.T, yaml string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withLiveModels(t *testing.T, ids []string, ok bool) {
	t.Helper()
	prev := fetchPickerLiveModels
	fetchPickerLiveModels = func(context.Context, any, string, string, bool, map[string]string, time.Duration, string) (provider.CatalogRows, bool) {
		return provider.CatalogRows{IDs: ids}, ok
	}
	t.Cleanup(func() { fetchPickerLiveModels = prev })
}

func TestNamedCustomProviderCatalogsLiveDiscoveryExtendsDeclaredModels(t *testing.T) {
	t.Setenv("SOME_KEY", "k")
	withConfig(t, "providers:\n  relay:\n    name: Relay\n    base_url: https://relay.example/v1\n    key_env: SOME_KEY\n    default_model: model-a\n")
	withLiveModels(t, []string{"model-a", "model-b"}, true)
	cats := namedCustomProviderCatalogs(context.Background())
	if len(cats) != 1 || cats[0].slug != "custom:relay" || !reflect.DeepEqual(cats[0].models, [][2]string{{"model-a", ""}, {"model-b", ""}}) {
		t.Fatalf("catalogs %#v", cats)
	}
}

func TestNamedCustomProviderCatalogsDisabledProviderSkipped(t *testing.T) {
	t.Setenv("SOME_KEY", "k")
	withConfig(t, "providers:\n  off:\n    name: Disabled Endpoint\n    base_url: https://off.example/v1\n    key_env: SOME_KEY\n    default_model: m\n    enabled: false\n")
	withLiveModels(t, nil, false)
	if cats := namedCustomProviderCatalogs(context.Background()); len(cats) != 0 {
		t.Fatalf("catalogs %#v", cats)
	}
}

func TestNamedCustomProviderCatalogsNoCredentialAndNoDeclaredModelsSkipped(t *testing.T) {
	os.Unsetenv("MISSING_KEY")
	withConfig(t, "providers:\n  bare:\n    name: Bare\n    base_url: https://bare.example/v1\n    key_env: MISSING_KEY\n")
	withLiveModels(t, nil, false)
	if cats := namedCustomProviderCatalogs(context.Background()); len(cats) != 0 {
		t.Fatalf("catalogs %#v", cats)
	}
}

func TestNamedCustomProviderCatalogsLegacyCustomProvidersListIncluded(t *testing.T) {
	t.Setenv("SOME_KEY", "k")
	withConfig(t, "custom_providers:\n- name: Legacy Endpoint\n  base_url: https://legacy.example/v1\n  key_env: SOME_KEY\n  model: legacy-model\n")
	withLiveModels(t, nil, false)
	cats := namedCustomProviderCatalogs(context.Background())
	if len(cats) != 1 || cats[0].slug != "custom:legacy-endpoint" || cats[0].label != "Legacy Endpoint" || !reflect.DeepEqual(cats[0].models, [][2]string{{"legacy-model", ""}}) {
		t.Fatalf("catalogs %#v", cats)
	}
}

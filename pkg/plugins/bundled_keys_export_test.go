package plugins

import (
	"slices"
	"testing"
)

func TestBundledPluginKeysFollowRegistrationOrder(t *testing.T) {
	bundledMu.Lock()
	savedByKey, savedOrder := bundledByKey, bundledOrder
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
	bundledMu.Unlock()
	t.Cleanup(func() {
		bundledMu.Lock()
		bundledByKey, bundledOrder = savedByKey, savedOrder
		bundledMu.Unlock()
	})
	if keys := BundledPluginKeys(); len(keys) != 0 {
		t.Fatalf("empty registry %v", keys)
	}
	RegisterBundled(fakeBundled{Manifest{Name: "zeta"}})
	RegisterBundled(fakeBundled{Manifest{Name: "beta", Key: "web/beta"}})
	RegisterBundled(fakeBundled{Manifest{Name: "alpha"}})
	keys := BundledPluginKeys()
	if !slices.Equal(keys, []string{"zeta", "web/beta", "alpha"}) {
		t.Fatalf("keys %v", keys)
	}
	keys[0] = "mutated"
	if got := BundledPluginKeys(); got[0] != "zeta" {
		t.Fatalf("caller mutation reached the registry %v", got)
	}
}

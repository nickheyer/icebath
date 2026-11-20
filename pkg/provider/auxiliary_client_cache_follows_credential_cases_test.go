package provider

import (
	"fmt"
	"strings"
	"testing"
)

func TestPoolTokenRotationChangesCacheKeyAndHidesTheSecret(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai-codex"} {
		auxIsolateClientCache(t)
		ctx := auxRecoveryHome(t, "")
		keyFor := func() auxClientCacheKey {
			key, _, err := auxClientCacheKeyFor(ctx, provider, false, "", nil, "", nil, false, "", "claude-sonnet-4-5")
			if err != nil {
				t.Fatal(err)
			}
			return key
		}
		auxSeedPool(t, ctx, provider, []string{"tok-old"}, "")
		before := keyFor()
		auxSeedPool(t, ctx, provider, []string{"tok-new"}, "")
		after := keyFor()
		if before == after {
			t.Fatalf("%s: rotated token kept the key %+v", provider, before)
		}
		if flat := fmt.Sprintf("%#v%#v", before, after); strings.Contains(flat, "tok-old") || strings.Contains(flat, "tok-new") {
			t.Fatalf("%s: token entered the key %s", provider, flat)
		}
		if keyFor() != after {
			t.Fatalf("%s: unchanged credentials changed the key", provider)
		}
	}
}

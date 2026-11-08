package provider

import "testing"

func TestMinimaxM3StaleCacheGuardM2CacheNotClobbered(t *testing.T) {
	ctx := probelessResolver(t)
	base := "https://api.minimaxi.com/anthropic"
	for _, slug := range []string{"MiniMax-M2.7", "MiniMax-M2.5", "MiniMax-M2.1"} {
		SaveContextLength(ctx, slug, base, 204_800, "")
		if got := GetModelContextLength(ctx, ContextLengthRequest{Model: slug, BaseURL: base, Provider: "minimax-cn"}); got != 204_800 {
			t.Fatalf("%s should stay 204800, got %d", slug, got)
		}
	}
}

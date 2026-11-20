package provider

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Seeds one cached client under the async or sync key of a provider and returns the key
func auxSeedCachedClientMode(t *testing.T, ctx context.Context, provider string, asyncMode bool, client AuxClient) auxClientCacheKey {
	t.Helper()
	key, _, err := auxClientCacheKeyFor(ctx, provider, asyncMode, "", nil, "", nil, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(key, auxCachedClient{Client: client, DefaultModel: "test-model"})
	auxClientCacheMu.Unlock()
	return key
}

func TestEvictCachedClientsMatchesProviderAfterProfileHomeKey(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	other := &auxCloseCounter{}
	syncKey := auxSeedCachedClientMode(t, ctx, "anthropic", false, &auxCloseCounter{})
	asyncKey := auxSeedCachedClientMode(t, ctx, "anthropic", true, &auxCloseCounter{})
	otherKey := auxSeedCachedClientMode(t, ctx, "openai-codex", false, other)

	auxEvictCachedClients(ctx, "anthropic")

	if auxCacheHas(syncKey) || auxCacheHas(asyncKey) || !auxCacheHas(otherKey) {
		t.Fatalf("sync %v async %v other %v", auxCacheHas(syncKey), auxCacheHas(asyncKey), auxCacheHas(otherKey))
	}
	if other.closes != 0 {
		t.Fatalf("other provider client closed %d times", other.closes)
	}
}

func TestEvictCachedClientsIsScopedToTheCallingProfile(t *testing.T) {
	auxIsolateClientCache(t)
	ctxA := auxMainTestHome(t, "")
	ctxB := home.WithOverride(ctxA, filepath.Join(t.TempDir(), "profiles", "b"))
	keyA := auxSeedCachedClientMode(t, ctxA, "anthropic", false, &auxCloseCounter{})
	keyB := auxSeedCachedClientMode(t, ctxB, "anthropic", false, &auxCloseCounter{})
	if keyA.Home == keyB.Home {
		t.Fatalf("profile homes share the key %q", keyA.Home)
	}

	auxEvictCachedClients(ctxA, "anthropic")

	if auxCacheHas(keyA) || !auxCacheHas(keyB) {
		t.Fatalf("a %v b %v", auxCacheHas(keyA), auxCacheHas(keyB))
	}
}

func TestEvictCachedClientsDoesNotClosePossiblyInFlightClient(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	entry := &auxCloseCounter{}
	key := auxSeedCachedClientMode(t, ctx, "anthropic", false, entry)

	auxEvictCachedClients(ctx, "anthropic")

	if auxCacheHas(key) || entry.closes != 0 {
		t.Fatalf("cached %v closes %d", auxCacheHas(key), entry.closes)
	}
}

func TestPoolRotationEvictsClientBuiltWithRevokedCredential(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	staleKey := auxSeedCachedClientMode(t, ctx, "anthropic", false, &auxCloseCounter{})
	pool := &auxFakeRecoveryPool{}
	prevPool := auxRecoveryPoolFn
	auxRecoveryPoolFn = func(context.Context, string, string) (auxRecoveryPool, error) { return pool, nil }
	t.Cleanup(func() { auxRecoveryPoolFn = prevPool })
	authError := &AuxSDKError{StatusCode: 401, Message: "revoked OAuth token"}

	if !auxRecoverProviderPool(ctx, "anthropic", authError, "revoked-token") {
		t.Fatal("rotation must report a fresh credential")
	}
	if auxCacheHas(staleKey) {
		t.Fatal("stale client survived the rotation")
	}
	if len(pool.rotations) != 1 {
		t.Fatalf("rotations %d", len(pool.rotations))
	}
}

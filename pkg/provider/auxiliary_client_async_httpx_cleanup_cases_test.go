package provider

import (
	"context"
	"testing"
)

// Async client double whose close hands back an awaitable and whose transport can be neutered
type auxAsyncCloseClient struct {
	auxBareClient
	closes   int
	awaited  bool
	neutered int
}

func (c *auxAsyncCloseClient) Close() error { c.closes++; return nil }
func (c *auxAsyncCloseClient) CloseAwaitable() func() error {
	c.closes++
	return func() error { c.awaited = true; return nil }
}
func (c *auxAsyncCloseClient) ForceCloseTransport() { c.neutered++ }

// Client whose close records whether the cache lock was free while it ran
type auxLockObservingClient struct {
	auxBareClient
	observations []bool
}

func (c *auxLockObservingClient) Close() error {
	acquired := auxClientCacheMu.TryLock()
	c.observations = append(c.observations, acquired)
	if acquired {
		auxClientCacheMu.Unlock()
	}
	return nil
}

func auxCacheEntry(t *testing.T, key auxClientCacheKey, client AuxClient, loop any) {
	t.Helper()
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	auxClientCacheSetLocked(key, auxCachedClient{Client: client, DefaultModel: "test-model", BoundLoop: loop})
}

func auxCacheHas(key auxClientCacheKey) bool {
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	_, ok := auxClientCache[key]
	return ok
}

func TestNeuterAsyncHttpxDelDelBecomesNoop(t *testing.T) {
	auxIsolateClientCache(t)
	client := &auxAsyncCloseClient{}
	auxCacheEntry(t, auxClientCacheKey{Provider: "test_del", Async: true}, client, &auxTestLoop{closed: true})
	CleanupStaleAsyncClients()
	if client.neutered != 1 {
		t.Fatalf("the dead loop transport must be neutered once, got %d", client.neutered)
	}
	auxForceCloseAsyncTransport(client)
	if client.neutered != 2 {
		t.Fatal("a neutered transport stays silent on every later finalization")
	}
}

func TestCleanupStaleAsyncClientsRemovesStaleEntries(t *testing.T) {
	auxIsolateClientCache(t)
	client := &auxCloseCounter{}
	key := auxClientCacheKey{Provider: "test_stale", Async: true}
	auxCacheEntry(t, key, client, &auxTestLoop{closed: true})
	CleanupStaleAsyncClients()
	if client.closes != 1 {
		t.Fatalf("close called %d times", client.closes)
	}
	if auxCacheHas(key) {
		t.Fatal("Stale entry should be removed")
	}
}

func TestCleanupStaleAsyncClientsAwaitsAsyncCloseForClosedLoop(t *testing.T) {
	auxIsolateClientCache(t)
	client := &auxAsyncCloseClient{}
	auxCacheEntry(t, auxClientCacheKey{Provider: "test_async_close", Async: true}, client, &auxTestLoop{closed: true})
	CleanupStaleAsyncClients()
	if !client.awaited {
		t.Fatal("the async close of a closed loop client must be awaited")
	}
}

func TestCleanupStaleAsyncClientsShutdownClosesOutsideCacheLock(t *testing.T) {
	auxIsolateClientCache(t)
	client := &auxLockObservingClient{}
	auxCacheEntry(t, auxClientCacheKey{Provider: "test_shutdown_lock"}, client, nil)
	ShutdownCachedClients(context.Background())
	if len(client.observations) != 1 || !client.observations[0] {
		t.Fatalf("lock observations %v", client.observations)
	}
}

func TestCleanupStaleAsyncClientsShutdownDoesNotAwaitLiveForeignLoopClient(t *testing.T) {
	auxIsolateClientCache(t)
	client := &auxAsyncCloseClient{}
	auxCacheEntry(t, auxClientCacheKey{Provider: "test_shutdown_foreign_loop", Async: true}, client, &auxTestLoop{})
	ShutdownCachedClients(context.Background())
	if client.awaited {
		t.Fatal("a live foreign loop client must not be awaited from another loop")
	}
}

func TestCleanupStaleAsyncClientsKeepsLiveEntries(t *testing.T) {
	auxIsolateClientCache(t)
	key := auxClientCacheKey{Provider: "test_live", Async: true}
	auxCacheEntry(t, key, &auxCloseCounter{}, &auxTestLoop{})
	CleanupStaleAsyncClients()
	if !auxCacheHas(key) {
		t.Fatal("Live entry should be preserved")
	}
}

func TestCleanupStaleAsyncClientsKeepsEntriesWithoutLoop(t *testing.T) {
	auxIsolateClientCache(t)
	key := auxClientCacheKey{Provider: "test_sync"}
	auxCacheEntry(t, key, &auxCloseCounter{}, nil)
	CleanupStaleAsyncClients()
	if !auxCacheHas(key) {
		t.Fatal("Sync entry should be preserved")
	}
}

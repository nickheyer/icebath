package mcp

import (
	"testing"
)

func TestLazyFirstUseConnectCheckFnPassesForLazyRegisteredServer(t *testing.T) {
	_, env := managerHome(t)
	key := ServerKey{Name: "playwright"}
	mcpMu.Lock()
	lazyServerConfigs[key] = map[string]any{"lazy": true}
	lazyServerFingerprints[key] = "abc"
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(lazyServerConfigs, key)
		delete(lazyServerFingerprints, key)
		mcpMu.Unlock()
	})
	if !makeCheckFn("playwright")(env) {
		t.Fatal("a lazily registered server must pass its check")
	}
}

func TestLazyFirstUseConnectCheckFnFailsForUnknownServer(t *testing.T) {
	_, env := managerHome(t)
	if makeCheckFn("nope")(env) {
		t.Fatal("an unknown server must fail its check")
	}
}

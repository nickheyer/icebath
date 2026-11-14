package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Clears the multi profile hosting latch, launch env snapshot and served homes for one case like the hermes conftest
func isolateProfileHosting(t *testing.T) {
	t.Helper()
	prevMultiplex := provider.IsMultiplexActive(context.Background())
	provider.SetMultiplexActive(false)
	provider.ResetLaunchEnvSnapshotForTests()
	servedProfileMu.Lock()
	prevServed := servedProfileHomes
	servedProfileHomes = map[string]bool{}
	servedProfileMu.Unlock()
	t.Cleanup(func() {
		provider.SetMultiplexActive(prevMultiplex)
		provider.ResetLaunchEnvSnapshotForTests()
		servedProfileMu.Lock()
		servedProfileHomes = prevServed
		servedProfileMu.Unlock()
	})
}

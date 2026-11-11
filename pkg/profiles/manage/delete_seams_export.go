package manage

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Tree removal a profile delete runs, the _rmtree_with_retry attribute hermes tests replace
var rmtreeForDelete = rmtreeWithRetry

// Replaces the gateway service cleanup a delete runs until the returned restore runs
func SwapCleanupGatewayService(f func(ctx context.Context, name, profileDir string) bool) (restore func()) {
	old := cleanupGatewayService
	cleanupGatewayService = f
	return func() { cleanupGatewayService = old }
}

// Replaces the live default multiplexer probe until the returned restore runs
func SwapLiveDefaultMultiplexer(f func() bool) (restore func()) {
	old := liveDefaultMultiplexer
	liveDefaultMultiplexer = f
	return func() { liveDefaultMultiplexer = old }
}

// Replaces the tree removal a delete runs until the returned restore runs
func SwapRmtreeForDelete(f func(profileDir string, onexc func(pyfs.RmTreeOp, string, error) error) error) (restore func()) {
	old := rmtreeForDelete
	rmtreeForDelete = f
	return func() { rmtreeForDelete = old }
}

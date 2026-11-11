package plugins

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Dispatches the scratch prune entry so a prune child runs the prune instead of this entire suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry(context.Background())
	dropImportedBundledPlugins()
	os.Exit(m.Run())
}

// Empties the bundled table of plugins that packages linked for the external tests registered at init
func dropImportedBundledPlugins() {
	bundledMu.Lock()
	defer bundledMu.Unlock()
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
}

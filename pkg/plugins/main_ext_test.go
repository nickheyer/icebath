package plugins_test

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Dispatches the scratch prune and plugin host entries so a re-executed child runs its mode instead of this suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	home.DispatchScratchPruneEntry(context.Background())
	plugins.DropImportedBundledPlugins()
	os.Exit(m.Run())
}

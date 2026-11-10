package skills

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Dispatches the scratch prune entry so a prune child runs the prune instead of this entire suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry()
	os.Exit(m.Run())
}

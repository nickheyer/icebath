package agent

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Dispatches the scratch prune entry so a prune child runs the prune instead of this entire suite
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	os.Exit(m.Run())
}

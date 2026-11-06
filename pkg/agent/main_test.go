package agent

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Dispatches the scratch prune entry and the trajectory children so a child never runs this entire suite
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	dispatchTrajectoryChild()
	os.Exit(m.Run())
}

package provider

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Dispatches the scratch prune and tools MCP server entries so a child runs its mode instead of this suite
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	DispatchCodexToolsMCPServerEntry()
	os.Exit(m.Run())
}

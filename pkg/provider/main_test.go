package provider

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune and tools MCP server entries so a child runs its mode instead of this suite
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry()
	installLocalStackProbeDoubles()
	DispatchCodexToolsMCPServerEntry()
	os.Exit(m.Run())
}

// Local stack probes report no harness, no Chromium, no faster_whisper and no managed image route
func installLocalStackProbeDoubles() {
	BrowserHarnessCLI = func(*tools.Env) []string { return nil }
	ChromiumExecutable = func(*tools.Env, bool) string { return "" }
	FasterWhisperImportable = func() bool { return false }
	ManagedImageRoute = func(any, any) string { return "" }
}

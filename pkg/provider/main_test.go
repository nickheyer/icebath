package provider

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the fake llama-server, scratch prune and tools MCP server entries so a child runs its mode instead
func TestMain(m *testing.M) {
	dispatchFakeLlamaServerEntry()
	home.DispatchScratchPruneEntry()
	installLocalStackProbeDoubles()
	installSpilloverWiring()
	DispatchCodexToolsMCPServerEntry()
	os.Exit(m.Run())
}

// Session deletion reaches the retirement hooks the process wiring installs, tools real and gateway doubles
func installSpilloverWiring() {
	state.SpilloverSQLFilter = tools.SpilloverSQLFilter
	state.SpilloverNamesInText = tools.SpilloverNamesInText
	state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
	state.AdmissionMediaReferences = func(*pyjson.Object) ([]*pyjson.Object, error) { return nil, nil }
	state.HostedDocumentReferences = func(string, *pyjson.Object) ([]*pyjson.Object, error) { return nil, nil }
	state.ReleaseUnheldMedia = func(*state.SessionDB, []any) error { return nil }
}

// Local stack probes report no harness, no Chromium, no faster_whisper and no managed image route
func installLocalStackProbeDoubles() {
	BrowserHarnessCLI = func(*tools.Env) []string { return nil }
	ChromiumExecutable = func(*tools.Env, bool) string { return "" }
	FasterWhisperImportable = func() bool { return false }
	ManagedImageRoute = func(any, any) string { return "" }
	tools.VisionCapabilities = VisionCapabilities{}
}

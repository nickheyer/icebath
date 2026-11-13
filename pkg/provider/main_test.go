package provider

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/relayhost"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the fake llama-server, scratch prune and tools MCP server entries so a child runs its mode instead
func TestMain(m *testing.M) {
	relayhost.DispatchRelayHostEntry(context.Background())
	relaytest.Provision()
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	dispatchFakeLlamaServerEntry()
	home.DispatchScratchPruneEntry(context.Background())
	installLocalStackProbeDoubles()
	installSpilloverWiring()
	DispatchCodexToolsMCPServerEntry()
	dispatchLeftCoreAgentStartEntry()
	dispatchMoAPresetProbeEntry()
	dispatchRemoteBootChildEntry()
	os.Exit(runUnderPrivateHome(m))
}

// Runs the package cases under a private product home unless the caller named one, removing it afterwards
func runUnderPrivateHome(m *testing.M) int {
	if os.Getenv(home.EnvHome) != "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "icebath-provider-home-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Setenv(home.EnvHome, dir); err != nil {
		panic(err)
	}
	return m.Run()
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
	ChromiumExecutable = func(*tools.Env, bool) string { return "" }
	FasterWhisperImportable = func() bool { return false }
	ManagedImageRoute = func(any, any) string { return "" }
	tools.VisionCapabilities = VisionCapabilities{}
	tools.CredentialPools = XAICredentialPoolSource()
	tools.NousAccount = NousAccountSource()
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
}

package tuiserver

import (
	"context"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestMain(m *testing.M) {
	runEntryTestChild()
	runTestChild()
	DispatchComputeHostEntry(context.Background(), func(code int) { AtExit(); os.Exit(code) })
	home.DispatchScratchPruneEntry(context.Background())
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	tools.AsyncDelegationOwnerProbes.PidExists = kanban.GatewayStatusProbes.PidExists
	tools.AsyncDelegationOwnerProbes.ProcessStartTime = func(pid int) (int64, bool) {
		switch v := kanban.GatewayStatusProbes.ProcessStartTime(pid).(type) {
		case int64:
			return v, true
		case int:
			return int64(v), true
		}
		return 0, false
	}
	state.AdmissionMediaReferences = gateway.AdmissionMediaReferences
	state.HostedDocumentReferences = gateway.HostedDocumentReferences
	state.ReleaseUnheldMedia = gateway.ReleaseUnheldMediaRetainingHistory
	state.SpilloverSQLFilter = tools.SpilloverSQLFilter
	state.SpilloverNamesInText = tools.SpilloverNamesInText
	state.ReferencedSpilloverNames = tools.ReferencedSpilloverNames
	tools.ProfileSecrets = provider.ProfileSecretSource()
	tools.NousAccount = provider.NousAccountSource()
	tools.AsyncDelegationRetirement = provider.BackendRetirement()
	NormalizePersonalityName = commands.NormalizePersonalityName
	RenderPersonalityPrompt = commands.RenderPersonalityPrompt
	PersistPersonality = commands.PersistPersonality
	Personality.EphemeralSystemPrompt = commands.ResolveEphemeralSystemPrompt
	ResolveCommand = func(name string) (ResolvedCommand, bool) {
		d := commands.Resolve(name)
		if d == nil {
			return ResolvedCommand{}, false
		}
		return ResolvedCommand{Name: d.Name, Category: d.Category, Describe: d.Describe,
			DesktopSubcommands: commands.MetaOf(d.Name).DesktopSubcommands}, true
	}
	CommandsByCategory = func() []HelpCategory {
		var out []HelpCategory
		for _, g := range commands.CommandsByCategory(commands.Default) {
			category := HelpCategory{Category: g.Category}
			for _, row := range g.Rows {
				category.Rows = append(category.Rows, [2]string{row.Command, row.Description})
			}
			out = append(out, category)
		}
		return out
	}
	dir, err := os.MkdirTemp("", "tuiserver-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	hermesHome, hermesHomeAtImport = dir, dir
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, mainContextState, serverState, isolatedPlatformDefault)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Puts back the process wide server state a case leaves behind, the per test resets of the hermes conftest
func serverState(t *testing.T) func() {
	methodsMu.RLock()
	savedMethods := maps.Clone(methods)
	methodsMu.RUnlock()
	dbMu.Lock()
	savedDB, savedDBError := sharedDB, dbError
	dbMu.Unlock()
	return func() {
		for _, live := range tuigateway.LiveSessions() {
			closeSessionByID(context.Background(), live.SID, "test_cleanup", nil)
		}
		stopNotificationPollers(t)
		if !gateway.FlushRuntimeStatus(30 * time.Second) {
			t.Error("gateway runtime status was still being written 30s after the test")
		}
		if provider.StopNousAuthKeepalive(30 * time.Second); provider.NousAuthKeepaliveRunning() {
			t.Error("the Nous auth keepalive was still running 30s after the test stopped it")
		}
		tuigateway.ResetForTests()
		tuigateway.ClearParked()
		childMirrorsMu.Lock()
		clear(childMirrors)
		childMirrorsMu.Unlock()
		activeChildRuns.Lock()
		clear(activeChildRuns.at)
		activeChildRuns.Unlock()
		methodsMu.Lock()
		methods = savedMethods
		methodsMu.Unlock()
		dbMu.Lock()
		opened := sharedDB
		sharedDB, dbError = savedDB, savedDBError
		dbMu.Unlock()
		if opened != nil && opened != savedDB {
			_ = opened.Close()
		}
		servedProfileMu.Lock()
		servedProfileHomes = map[string]bool{}
		servedProfileMu.Unlock()
		provider.SetMultiplexActive(false)
		home.PinProcess("", true)
		provider.ResetLaunchEnvSnapshotForTests()
		tools.ResetForegroundExitFenceForTests()
		tools.OnboardingDueResetForTests()
		logging.ResetQueuedHandlers()
	}
}

// Puts the main thread context back to its values before the test, as a fresh test process starts clean
func mainContextState(*testing.T) func() { return pyctx.SnapshotMainForTests() }

// Platform default home of each case inside its own temp dir, never the developer's real one
func isolatedPlatformDefault(t *testing.T) func() { return home.PatchPlatformDefault(t.TempDir()) }

// Stops every notification poller a case left running and waits for each to finish
func stopNotificationPollers(t *testing.T) { joinNotificationPollers(t, nil) }

// Stop events of the notification pollers registered so far
func notificationPollerStops() map[*pythread.Event]bool {
	notificationPollers.mu.Lock()
	defer notificationPollers.mu.Unlock()
	stops := make(map[*pythread.Event]bool, len(notificationPollers.pollers))
	for _, p := range notificationPollers.pollers {
		stops[p.stop] = true
	}
	return stops
}

// Stops every poller whose stop event is not in kept and waits for each to finish
func joinNotificationPollers(t *testing.T, kept map[*pythread.Event]bool) {
	notificationPollers.mu.Lock()
	var pollers, staying []notificationPoller
	for _, p := range notificationPollers.pollers {
		if kept[p.stop] {
			staying = append(staying, p)
		} else {
			pollers = append(pollers, p)
		}
	}
	notificationPollers.pollers = staying
	notificationPollers.mu.Unlock()
	for _, p := range pollers {
		p.stop.Set()
	}
	deadline := time.After(30 * time.Second)
	for _, p := range pollers {
		select {
		case <-p.done:
		case <-deadline:
			t.Error("a notification poller was still running 30s after the test stopped it")
			return
		}
	}
}

// Real agent on a dead loopback endpoint with the given route and reasoning config
func testAgent(t *testing.T, o agent.AIAgentOptions) *agent.AIAgent {
	t.Helper()
	if o.BaseURL == "" {
		o.BaseURL = "http://127.0.0.1:9/v1"
	}
	if o.APIKey == nil || o.APIKey == "" {
		o.APIKey = "test"
	}
	o.MaxIterations, o.QuietMode, o.SkipContextFiles, o.SkipMemory = new(5), true, true, true
	a, err := agent.NewAIAgent(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

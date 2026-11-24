package tuiserver

import (
	"context"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
	ctxutil "github.com/nickheyer/icebath/pkg/utils/ctx"
)

func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry(context.Background())
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	tools.AsyncDelegationOwnerProbes.PidExists = gateway.PidExists
	tools.AsyncDelegationOwnerProbes.ProcessStartTime = func(pid int) (int64, bool) {
		switch v := gateway.GetProcessStartTime(pid).(type) {
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
	tools.PoolKeys = provider.PoolKeySource()
	gateway.ResolveBoundCommand, gateway.RunBoundCommand = commands.BoundGatewayCommand, commands.RunBoundGatewayCommand
	dir, err := os.MkdirTemp("", "tuiserver-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	icebathHome, icebathHomeAtImport = dir, dir
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, mainContextState, serverState, isolatedPlatformDefault)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Puts back the process wide server state a case leaves behind, the per test resets of the test setup
func serverState(t *testing.T) func() {
	methodsMu.RLock()
	savedMethods := maps.Clone(methods)
	methodsMu.RUnlock()
	dbMu.Lock()
	savedDB, savedDBError := sharedDB, dbError
	dbMu.Unlock()
	return func() {
		if !gateway.FlushRuntimeStatus(30 * time.Second) {
			t.Error("gateway runtime status was still being written 30s after the test")
		}
		tuihost.ResetForTests()
		tuihost.ClearParked()
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
		logging.ResetQueuedHandlers()
	}
}

// Puts the main thread context back to its values before the test, as a fresh test process starts clean
func mainContextState(*testing.T) func() { return ctxutil.StoreOf(context.Background()).Checkpoint() }

// Platform default home of each case inside its own temp dir, never the developer's real one
func isolatedPlatformDefault(t *testing.T) func() { return home.PatchPlatformDefault(t.TempDir()) }

// Writes text to a file the test owns
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

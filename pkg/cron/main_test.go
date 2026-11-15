package cron

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune and external worker entries, a worker child running the real job path
func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry(context.Background())
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	tools.ProfileSecrets = provider.ProfileSecretSource()
	dispatchLedgerChild()
	SetupWorkerLogging = func(string, string) error { return nil }
	WorkerProcessBoot = func() {}
	RegisterWorkerConfigHooks = func(context.Context, string, slog.Level) {}
	if len(os.Args) > 1 && os.Args[1] == ExternalWorkerEntry {
		runOneJobSeam = runOneJob
	}
	DispatchExternalWorkerEntry(os.Exit)
	os.Exit(runSandboxed(m))
}

// Runs the cases under a session home, each inside the per test resets of the hermes conftest
func runSandboxed(m *testing.M) int {
	dir, err := os.MkdirTemp("", "cron-home-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv("ICEBATH_HOME", dir)
	os.Setenv("ICEBATH_TEST_ISOLATION", dir)
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, processState)
	return m.Run()
}

// Clears the process wide state a cron test leaves behind, the worker pools a fresh test process never has
func processState(t *testing.T) func() {
	return func() {
		shutdownParallelPools()
		provider.SetMultiplexActive(false)
		home.PinProcess("", true)
		provider.ResetLaunchEnvSnapshotForTests()
		tools.ResetForegroundExitFenceForTests()
		plugins.ResetPluginManagersForTests()
		logging.ResetQueuedHandlers()
		state.CloseLeakedTestInstances()
		state.CloseAll()
	}
}

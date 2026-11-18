package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bundled"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"os"
	"strings"
	"testing"
	"time"
)

// Hidden first argument that routes a re-executed test binary to one of its child stand-ins
const cuChildEntry = "__selfupdate_test_child__"

// Runs a re-executed prune child and arms the prune entry ahead of every init and TestMain child mode
var _ = func() bool {
	home.DispatchScratchPruneEntry(context.Background())
	return true
}()

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if os.Getenv("ICEBATH_CUSTODY_TEST_NO_SUBREAPER") == "1" {
		custodySubreaper = func() bool { return false }
	}
	DispatchCustodyLauncher()
	gateway.DispatchGatewayRestartWatcherEntry(context.Background())
	tools.DispatchCommitGraphRefreshEntry(context.Background())
	if len(os.Args) > 1 && os.Args[1] == servedGatewayStandInEntry {
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	// Race instrumented children exit at once instead of sleeping a second, so their timing matches the build stand-ins
	os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if os.Getenv("ICEBATH_SELFUPDATE_TEST_HOST") == "1" {
		os.Exit(cuChild("claim-host", nil))
	}
	if os.Getenv("VS_BOOT_PROBE") != "" || len(os.Args) > 2 && os.Args[1] == "pm" && os.Args[2] == "worker" ||
		len(os.Args) > 1 && (os.Args[1] == vsChildEntry || os.Args[1] == RelaunchedFlag || os.Args[1] == SourceCompletionEntry) {
		os.Exit(vsChild(os.Args))
	}
	if os.Getenv(rrE2ELedgerEnv) != "" && len(os.Args) > 1 && os.Args[1] == UpdateRestartRecoveryEntry {
		os.Exit(rrE2EChild(os.Args[2:]))
	}
	if len(os.Args) > 2 && os.Args[1] == cuChildEntry {
		os.Exit(cuChild(os.Args[2], os.Args[3:]))
	}
	if len(os.Args) > 2 && os.Args[1] == scChildEntry {
		os.Exit(scChild(os.Args[2], os.Args[3:]))
	}
	LoadCronJobsHook = testCronJobs
	tools.GitlockRunGit = GitlockRunGit
	dir, err := os.MkdirTemp("", "selfupdate-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", dir)
	os.Setenv("ICEBATH_TEST_ISOLATION", dir)
	hermetic.AroundEachTest(m, hermetic.Environ, hermetic.WorkingDir, processState)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Clears the process wide state a test leaves behind, the per test resets of the hermes conftest
func processState(t *testing.T) func() {
	return func() {
		if !gateway.FlushRuntimeStatus(30 * time.Second) {
			t.Error("gateway runtime status was still being written 30s after the test")
		}
		provider.SetMultiplexActive(false)
		home.PinProcess("", true)
		provider.ResetLaunchEnvSnapshotForTests()
		tools.ResetForegroundExitFenceForTests()
		plugins.ResetPluginManagersForTests()
		config.ResetActiveSkinsForTests()
		logging.ResetQueuedHandlers()
		state.CloseLeakedTestInstances()
		state.CloseAll()
	}
}

// Argv that re-executes this test binary as the named child stand-in
func cuChildArgv(mode string, args ...string) []string {
	return append([]string{os.Args[0], cuChildEntry, mode}, args...)
}

package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bundled"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/tools"
	"os"
	"strings"
	"testing"
	"time"
)

// Hidden first argument that routes a re-executed test binary to one of its child stand-ins
const cuChildEntry = "__selfupdate_test_child__"

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if os.Getenv("ICEBATH_CUSTODY_TEST_NO_SUBREAPER") == "1" {
		custodySubreaper = func() bool { return false }
	}
	DispatchCustodyLauncher()
	gateway.DispatchGatewayRestartWatcherEntry(context.Background())
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
	// The doctor's dashboard load check re-executes this binary as the dashboard status report
	if len(os.Args) > 2 && os.Args[1] == "dashboard" && os.Args[2] == "--status" {
		os.Exit(0)
	}
	LoadCronJobsHook = testCronJobs
	tools.GitlockRunGit = GitlockRunGit
	os.Exit(m.Run())
}

// Argv that re-executes this test binary as the named child stand-in
func cuChildArgv(mode string, args ...string) []string {
	return append([]string{os.Args[0], cuChildEntry, mode}, args...)
}

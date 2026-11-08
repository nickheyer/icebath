//go:build !windows

package browser

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/sys/unix"
)

// Kills every process in the CLI's own session group, a vanished or foreign group ignored
func killCLIProcessGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// Owns the published harness daemon when it is this user's session leading daemon spawned by this process
func ownHarnessDaemon(penv map[string]string) daemonOwnership {
	if isWorkerProcess() {
		return ownNever
	}
	daemon := daemonIdentity(penv)
	name := daemon[len(daemon)-1]
	pid, ok := publishedDaemonPid(penv)
	if !ok {
		slog.Default().Debug(fmt.Sprintf("browser_exec: no harness daemon pid published for %s", name))
		return ownNothingYet
	}
	if alreadyOwned(daemon, pid) {
		return ownOwned
	}
	fail := func(err error) daemonOwnership {
		slog.Default().Debug(fmt.Sprintf("browser_exec: no harness daemon to supervise for %s: %s", name, err))
		return ownNothingYet
	}
	argv, err := proc.Cmdline(pid)
	if err != nil {
		return fail(err)
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		return fail(err)
	}
	if sid != pid {
		return ownNothingYet
	}
	uid, err := proc.RealUID(pid)
	if err != nil {
		return fail(err)
	}
	if uid != os.Getuid() {
		return ownNothingYet
	}
	i := slices.Index(argv, "-m")
	if i < 0 {
		return fail(fmt.Errorf("'-m' is not in list"))
	}
	if i+1 >= len(argv) {
		return fail(fmt.Errorf("list index out of range"))
	}
	if argv[i+1] != "browser_harness.daemon" {
		return ownNothingYet
	}
	if !tools.SpawnedByThisProcess(pid) {
		return ownNever
	}
	recordOwnedDaemon(daemon, pid)
	return ownOwned
}

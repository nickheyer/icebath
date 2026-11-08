package gateway

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// CgroupCleanupEntry is the hidden argument the unit ExecStopPost runs to reap orphans left in the service cgroup
const CgroupCleanupEntry = "__icebath_cgroup_cleanup__"

var (
	cgroupReadFile                  = os.ReadFile
	cgroupGetppid                   = os.Getppid
	cgroupKill                      = cgroupSigkill
	cgroupReadPids                  = readCgroupPids
	cgroupOwnPath                   = ownCgroupPath
	cgroupParentIsSystemd           = parentIsSystemd
	cgroupStderr          io.Writer = os.Stderr
	cgroupV2Line                    = regexp.MustCompile(`(?m)^0::(.+)$`)
)

// Text of a file decoded as strict UTF-8, ok false when it cannot be read
func cgroupReadText(path string) (string, bool, error) {
	raw, err := cgroupReadFile(path)
	if err != nil {
		return "", false, nil
	}
	if !utf8.Valid(raw) {
		return "", false, fmt.Errorf("UnicodeDecodeError: 'utf-8' codec can't decode %s", path)
	}
	return string(raw), true, nil
}

// Cgroup v2 path of the calling process, empty when unreadable or absent
func ownCgroupPath() (string, error) {
	text, ok, err := cgroupReadText("/proc/self/cgroup")
	if !ok {
		return "", err
	}
	if m := cgroupV2Line.FindStringSubmatch(text); m != nil {
		return pystr.Strip(m[1]), nil
	}
	return "", nil
}

// Pids listed in the cgroup procs file, unparseable lines skipped and an unreadable file empty
func readCgroupPids(cgroupPath string) ([]int, error) {
	raw, ok, err := cgroupReadText("/sys/fs/cgroup" + cgroupPath + "/cgroup.procs")
	if !ok {
		return nil, err
	}
	var pids []int
	for _, line := range pystr.Splitlines(raw, false) {
		n, ok := pystr.IntText(line)
		if !ok {
			continue
		}
		if !n.IsInt64() || n.Int64() != int64(int(n.Int64())) {
			return nil, fmt.Errorf("OverflowError: Python int too large to convert to C int")
		}
		pids = append(pids, int(n.Int64()))
	}
	return pids, nil
}

// Whether a systemd manager spawned this process, an unreadable parent comm failing closed
func parentIsSystemd() (bool, error) {
	text, ok, err := cgroupReadText(fmt.Sprintf("/proc/%d/comm", cgroupGetppid()))
	if !ok {
		return false, err
	}
	return pystr.Strip(text) == "systemd", nil
}

// Whether one of the pids has a readable gateway runtime command line
func liveGatewayAmong(pids []int) bool {
	for _, pid := range pids {
		if cmdline, ok := statusReadProcessCmdline(pid); ok && cmdline != "" && LooksLikeGatewayRuntimeCommandLine(cmdline) {
			return true
		}
	}
	return false
}

// SIGKILLs every pid of the cgroup but the caller from a fresh read, refused false while a live gateway remains
func ReapCgroup(cgroupPath string) (int, bool, error) {
	if cgroupPath == "" {
		return 0, true, nil
	}
	me := os.Getpid()
	pids, err := cgroupReadPids(cgroupPath)
	if err != nil {
		return 0, false, err
	}
	var others []int
	for _, pid := range pids {
		if pid != me {
			others = append(others, pid)
		}
	}
	if len(others) == 0 {
		return 0, true, nil
	}
	if liveGatewayAmong(others) {
		fmt.Fprintln(cgroupStderr, "cgroup_cleanup: refusing — a live gateway process is still in the "+
			"cgroup; reaping would SIGKILL it. Stop the service first (then "+
			"ExecStopPost reaps its orphans), or call reap_cgroup(path) once "+
			"the gateway process has exited.")
		return 0, false, nil
	}
	if pids, err = cgroupReadPids(cgroupPath); err != nil {
		return 0, false, err
	}
	killed := 0
	for _, pid := range pids {
		if pid == me {
			continue
		}
		if err := cgroupKill(pid); err != nil {
			if cgroupKillSkippable(err) {
				continue
			}
			return killed, false, err
		}
		killed++
	}
	return killed, true, nil
}

// Reaps the own cgroup under a systemd parent, one on refusal or failure
func CgroupCleanupMain() int {
	systemd, err := cgroupParentIsSystemd()
	if err != nil {
		fmt.Fprintln(cgroupStderr, err)
		return 1
	}
	if !systemd {
		fmt.Fprintln(cgroupStderr, "cgroup_cleanup: refusing — not spawned by systemd. Running this "+
			"inside a live process's cgroup would SIGKILL that process. "+
			"Run it via the systemd unit's ExecStopPost, or call "+
			"reap_cgroup(cgroup_path) with an explicit stopped-service path.")
		return 1
	}
	path, err := cgroupOwnPath()
	if err != nil {
		fmt.Fprintln(cgroupStderr, err)
		return 1
	}
	_, reaped, err := ReapCgroup(path)
	if err != nil {
		fmt.Fprintln(cgroupStderr, err)
		return 1
	}
	if !reaped {
		return 1
	}
	return 0
}

// Runs the cgroup reaper and exits when the process was started with the hidden entry argument
func DispatchCgroupCleanupEntry() {
	args := pinnedEntryArgs(os.Args[1:])
	if len(args) > 0 && args[0] == CgroupCleanupEntry {
		os.Exit(CgroupCleanupMain())
	}
}

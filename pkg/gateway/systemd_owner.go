package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

var (
	// Captured run of a short systemctl probe, swapped by tests
	systemdRunProbe = func(ctx context.Context, argv []string, timeout time.Duration) (*pysubprocess.Completed, error) {
		return pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Timeout: timeout})
	}
	systemdProcRoot = "/proc"
)

// Unified cgroup paths of a process from its proc cgroup file, none when unreadable
func pidUnifiedCgroupEntries(pid int) []string {
	path := filepath.Join(systemdProcRoot, strconv.Itoa(pid), "cgroup")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []string
	for _, line := range textutil.Lines(pystr.DecodeUTF8SigReplace(raw)) {
		if _, entry, found := strings.Cut(strings.TrimSpace(line), "::"); found {
			entries = append(entries, entry)
		}
	}
	return entries
}

// First unified cgroup path of a process, not ok when it has none
func getPidCgroupPath(pid int) (string, bool) {
	if entries := pidUnifiedCgroupEntries(pid); len(entries) > 0 {
		return entries[0], true
	}
	return "", false
}

// Scope user or system a cgroup path names, empty when it names neither
func extractScopeFromCgroup(cgroupEntry string) string {
	if strings.Contains(cgroupEntry, "/system.slice/") {
		return "system"
	}
	if strings.Contains(cgroupEntry, "/user.slice/") {
		return "user"
	}
	return ""
}

// True when the live MainPID of the unit is pid, asked of systemctl show in the unit's scope
func unitMainPidIs(ctx context.Context, svcName, cgroupPath string, pid int) bool {
	scopes := [][]string{{}, {"--user"}}
	switch extractScopeFromCgroup(cgroupPath) {
	case "user":
		scopes = [][]string{{"--user"}}
	case "system":
		scopes = [][]string{{}}
	}
	for _, scopeArgs := range scopes {
		argv := append(append([]string{"systemctl"}, scopeArgs...), "show", svcName, "--property=MainPID", "--value")
		result, err := systemdRunProbe(ctx, argv, 10*time.Second)
		if err != nil {
			continue
		}
		if result.Returncode == 0 && strings.TrimSpace(strings.ToValidUTF8(string(result.Stdout), "\uFFFD")) == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}

// Systemd service unit whose live MainPID is pid, not ok when no unit provably owns the process
func getSystemdServiceForPid(ctx context.Context, pid int) (string, bool) {
	for _, cgPath := range pidUnifiedCgroupEntries(pid) {
		if strings.HasSuffix(cgPath, ".service") {
			svcName := cgPath[strings.LastIndex(cgPath, "/")+1:]
			if svcName != "" && unitMainPidIs(ctx, svcName, cgPath, pid) {
				return svcName, true
			}
		}
	}
	return "", false
}

// Unit and scope of the systemd unit whose MainPID is pid, the scope empty when the cgroup omits it
func SystemdOwnerForPid(ctx context.Context, pid int) (unit, scope string, ok bool) {
	unit, ok = getSystemdServiceForPid(ctx, pid)
	if !ok {
		return "", "", false
	}
	cgroup, _ := getPidCgroupPath(pid)
	return unit, extractScopeFromCgroup(cgroup), true
}

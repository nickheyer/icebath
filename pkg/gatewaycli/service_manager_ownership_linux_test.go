package gatewaycli

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// True when the service account id has a mapping in the user namespace of this process
func accountIDMapped(t *testing.T) bool {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		first, _ := strconv.ParseInt(fields[0], 10, 64)
		count, _ := strconv.ParseInt(fields[2], 10, 64)
		if icebathUID >= first && icebathUID < first+count {
			return true
		}
	}
	return false
}

// Reruns the case in a namespace that maps the service account when this one does not, true when the rerun was the case
func ranWhereAccountIsMapped(t *testing.T) bool {
	t.Helper()
	if accountIDMapped(t) {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.count=1", "-test.timeout=60s")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: icebathUID, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: icebathGID, HostID: os.Getgid(), Size: 1}}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("case in the account namespace: %v\n%s", err, out)
	}
	return true
}

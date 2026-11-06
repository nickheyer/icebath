package proc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var tcpStatuses = map[string]string{"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1",
	"05": "FIN_WAIT2", "06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING"}

// Status of every TCP and UDP socket over IPv4 and IPv6 the process holds, as psutil net_connections kind inet lists them
func InetConnectionStatuses(pid int) ([]string, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, err
	}
	inodes := map[string]bool{}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EINVAL) {
				continue
			}
			return nil, err
		}
		if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			inodes[target[len("socket:["):len(target)-1]] = true
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	var out []string
	for _, table := range []struct {
		name string
		tcp  bool
	}{{"tcp", true}, {"tcp6", true}, {"udp", false}, {"udp6", false}} {
		path := "/proc/net/" + table.name
		data, err := os.ReadFile(path)
		if err != nil {
			if strings.HasSuffix(table.name, "6") && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || !inodes[fields[9]] {
				continue
			}
			status := "NONE"
			if table.tcp {
				status = tcpStatuses[fields[3]]
			}
			out = append(out, status)
		}
	}
	return out, nil
}

package proc

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// TCP states of the kernel socket tables keyed by their hex code
var tcpStates = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2", "06": "TIME_WAIT",
	"07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING", "0C": "NEW_SYN_RECV",
}

// State of every IPv4 and IPv6 TCP and UDP socket pid holds, NONE for each UDP socket
func InetStates(pid int) ([]string, error) {
	dir := procPath(pid, "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, procError(pid, err)
	}
	inodes := map[string]bool{}
	for _, e := range entries {
		target, err := os.Readlink(dir + "/" + e.Name())
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EINVAL) {
				continue
			}
			return nil, procError(pid, err)
		}
		if inode, ok := strings.CutPrefix(target, "socket:["); ok {
			inodes[strings.TrimSuffix(inode, "]")] = true
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
		data, err := os.ReadFile(procPath(pid, "net/"+table.name))
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
			state := "NONE"
			if table.tcp {
				state = tcpStates[fields[3]]
			}
			out = append(out, state)
		}
	}
	return out, nil
}

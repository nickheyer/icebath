//go:build linux

package proc

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// Whether pid is a thread group leader rather than a thread id, the pid listing deciding when status is unreadable
func threadGroupLeader(pid int) (bool, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err == nil {
		for _, line := range bytes.Split(data, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("Tgid:")) {
				continue
			}
			if fields := bytes.Fields(line); len(fields) > 1 {
				if tgid, err := strconv.Atoi(string(fields[1])); err == nil {
					return tgid == pid, nil
				}
			}
			break
		}
	}
	return pidListed(pid)
}

package proc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Account owning pid through its real uid in /proc status
func Username(pid int) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoSuchProcess
		}
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				break
			}
			uid, err := strconv.Atoi(fields[0])
			if err != nil {
				return "", err
			}
			return uidName(uid), nil
		}
	}
	return "", fmt.Errorf("no Uid line in /proc/%d/status", pid)
}

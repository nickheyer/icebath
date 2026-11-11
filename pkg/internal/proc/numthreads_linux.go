package proc

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
)

var numThreadsRE = regexp.MustCompile(`Threads:\t(\d+)`)

// Thread count of pid from the Threads line of its status file like psutil num_threads
func NumThreads(pid int) (int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNoSuchProcess
		}
		return 0, err
	}
	m := numThreadsRE.FindSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("IndexError: list index out of range")
	}
	return strconv.Atoi(string(m[1]))
}

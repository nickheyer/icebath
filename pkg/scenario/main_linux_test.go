package scenario

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	modulePath       = "github.com/nickheyer/icebath"
	isolatedEnv      = "ICEBATH_SCENARIO_ISOLATED"
	runTagEnv        = "ICEBATH_SCENARIO_RUN"
	exercisedEnv     = "ICEBATH_SCENARIO_EXERCISED"
	exercisedDefault = "dev/ledger2/exercised.tsv"
)

// Built binary, its coverage directory and the module root shared by every scenario of one run
var suite struct {
	bin      string
	coverDir string
	root     string
	runTag   string
}

// Lists live processes whose environment carries the marker, zombies and this process left out
func tagged(marker string) []int {
	needle := []byte(marker + "\x00")
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !bytes.Contains(append(env, 0), needle) {
			continue
		}
		if stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil {
			if i := bytes.LastIndexByte(stat, ')'); i > 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
				continue
			}
		}
		pids = append(pids, pid)
	}
	return pids
}

// Asks marked processes to end, waits out the grace period, kills the rest and returns how many it killed
func killTagged(marker string, grace time.Duration) int {
	for _, pid := range tagged(marker) {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline) && len(tagged(marker)) > 0; {
		time.Sleep(100 * time.Millisecond)
	}
	killed := 0
	for attempt := 0; attempt < 20; attempt++ {
		pids := tagged(marker)
		if len(pids) == 0 {
			break
		}
		if attempt == 0 {
			killed = len(pids)
		}
		for _, pid := range pids {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return killed
}

package provider

import (
	"fmt"
	"log/slog"
	"runtime"
)

// Kills processes running this binary whose parent is gone, never the live router and never on Windows
func lrReapOrphans(exe string, ownPid int) {
	if runtime.GOOS == "windows" {
		return
	}
	for _, pid := range lrReapPids() {
		procExe, err := lrReapExe(pid)
		if err != nil {
			continue
		}
		ppid, err := lrReapPPID(pid)
		if err != nil {
			ppid = 0
		}
		if procExe != exe || pid == ownPid || (ppid != 0 && lrReapPidExists(ppid)) {
			continue
		}
		slog.Warn(fmt.Sprintf("reaping orphaned llama-server child pid=%d", pid))
		_ = lrReapKill(pid)
	}
}

// Process table view the orphan reaper walks
var (
	lrReapPids      = lrAllPids
	lrReapExe       = psExe
	lrReapPPID      = psPPID
	lrReapPidExists = psPidExists
	lrReapKill      = func(pid int) error {
		p, err := openPSProcess(pid)
		if err != nil {
			return err
		}
		return p.Kill()
	}
)

//go:build !windows

package provider

import (
	"os"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/treekill"
)

// Identities of pid's current descendants, empty when the process table cannot be read
func codexSnapshotDescendants(pid int) []treekill.Proc {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	var out []treekill.Proc
	for _, d := range table.Descendants(pid) {
		if birth, err := proc.Birth(d); err == nil {
			out = append(out, treekill.Proc{PID: d, Birth: birth})
		}
	}
	return out
}

// Terminates the snapshotted descendants deepest first and kills survivors after a bounded wait
func codexReapSnapshotted(descendants []treekill.Proc) {
	if len(descendants) == 0 {
		return
	}
	var live []treekill.Proc
	for i := len(descendants) - 1; i >= 0; i-- {
		child := descendants[i]
		if child.Running() && child.Signal(syscall.SIGTERM) == nil {
			live = append(live, child)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		alive := live[:0:0]
		for _, child := range live {
			if child.Running() {
				alive = append(alive, child)
			}
		}
		if live = alive; len(live) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, child := range live {
		_ = child.Signal(syscall.SIGKILL)
	}
}

func codexTerminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

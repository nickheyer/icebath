//go:build windows

package provider

import (
	"os"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

type codexChildProc struct {
	pid   int
	birth float64
}

func (p codexChildProc) running() bool {
	birth, err := proc.Birth(p.pid)
	return err == nil && birth == p.birth
}

// Identities of pid's current descendants, empty when the process table cannot be read
func codexSnapshotDescendants(pid int) []codexChildProc {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	var out []codexChildProc
	for _, d := range table.Descendants(pid) {
		if birth, err := proc.Birth(d); err == nil {
			out = append(out, codexChildProc{pid: d, birth: birth})
		}
	}
	return out
}

// Terminates the snapshotted descendants deepest first, the Windows terminate and kill being one call
func codexReapSnapshotted(descendants []codexChildProc) {
	for i := len(descendants) - 1; i >= 0; i-- {
		if child := descendants[i]; child.running() {
			_ = proc.Terminate(child.pid)
		}
	}
}

func codexTerminate(p *os.Process) error { return p.Kill() }

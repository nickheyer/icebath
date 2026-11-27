package terminal

import (
	"slices"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/term"
)

// Whether the process runs and is no zombie waiting to be reaped
func Alive(pid int) bool { return alive(pid) }

// Command line of a process of this machine, nil when it can't be read
func Cmdline(pid int) []string { return cmdline(pid) }

// Every live process of this machine, sorted
func Pids() []int {
	table := term.Processes()
	pids := make([]int, 0, len(table))
	for pid := range table {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids
}

// Terminates a process of this machine and its tree after checking it is still the one that started then, a start of zero standing for
// any: TERM to it first with a head start when leaderFirst says so, the profile's grace for all of it, then KILL to what is left
func (t *Terminal) Terminate(cfg *config.Config, pid int, start int64, leaderFirst bool) {
	t.Jobs.terminate(cfg, pid, start, leaderFirst)
}

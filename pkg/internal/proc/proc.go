// Process group control and process table queries shared by the shell and the MCP child processes
package proc

import (
	"os/exec"
)

// Puts the command in its own process group so Kill can reach every descendant
func Group(cmd *exec.Cmd) { group(cmd) }

// Kills the command together with every process in its group
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return kill(cmd)
}

// One row of the process table
type Process struct {
	PID  int
	PPID int
	// Exited but not yet reaped by its parent
	Zombie bool
}

// Processes running at one instant
type Table []Process

// Reads the process table
func Snapshot() (Table, error) { return snapshot() }

// Every descendant of pid, parents before their children
func (t Table) Descendants(pid int) []int {
	kids := map[int][]int{}
	for _, p := range t {
		kids[p.PPID] = append(kids[p.PPID], p.PID)
	}
	seen := map[int]bool{pid: true}
	var out []int
	queue := []int{pid}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, k := range kids[id] {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
			queue = append(queue, k)
		}
	}
	return out
}

// Reports whether pid is in the table and still running
func (t Table) Alive(pid int) bool {
	for _, p := range t {
		if p.PID == pid {
			return !p.Zombie
		}
	}
	return false
}

// Kills one process by id without touching its group
func KillPID(pid int) error { return killPID(pid) }

// Kills pid and every descendant, sweeping twice so children forked during the first pass die too
func KillTree(pid int) {
	for range 2 {
		t, err := snapshot()
		if err != nil {
			killPID(pid)
			return
		}
		pids := append([]int{pid}, t.Descendants(pid)...)
		for _, id := range pids {
			killPID(id)
		}
	}
}

// Kills every descendant of pid that is not in keep, sweeping twice to catch late forks
func KillNewDescendants(pid int, keep map[int]bool) {
	for range 2 {
		t, err := snapshot()
		if err != nil {
			return
		}
		for _, id := range t.Descendants(pid) {
			if !keep[id] {
				killPID(id)
			}
		}
	}
}

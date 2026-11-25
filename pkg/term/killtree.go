package term

// One row of the process table
type procRow struct {
	pid, ppid int
	// Exited but not yet reaped by its parent
	zombie bool
}

// Every live descendant of pid, parents before their children, zombies left for their parent to reap
func descendants(table []procRow, pid int) []int {
	kids := map[int][]int{}
	for _, p := range table {
		if p.zombie {
			continue
		}
		kids[p.ppid] = append(kids[p.ppid], p.pid)
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

// Kills pid and every descendant, sweeping twice so children forked during the first pass die too
func killTree(pid int) {
	for range 2 {
		table, err := processTable()
		if err != nil {
			killPID(pid)
			return
		}
		for _, id := range append([]int{pid}, descendants(table, pid)...) {
			killPID(id)
		}
	}
}

package term

// Every live descendant of pid, parents before their children, zombies left for their parent to reap
func Descendants(pid int) []int {
	children := map[int][]int{}
	for child, parent := range Processes() {
		children[parent] = append(children[parent], child)
	}
	seen := map[int]bool{pid: true}
	var out []int
	for queue := []int{pid}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			if !seen[child] {
				seen[child] = true
				out = append(out, child)
				queue = append(queue, child)
			}
		}
	}
	return out
}

// Kills pid and every descendant, sweeping twice so children forked during the first pass die too
func killTree(pid int) {
	for range 2 {
		for _, id := range append([]int{pid}, Descendants(pid)...) {
			killPID(id)
		}
	}
}

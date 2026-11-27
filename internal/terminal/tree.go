package terminal

// Readings of one process's start time on this host may drift by this many units
const startDrift = 200

// Whether two start times name the same incarnation of a process
func SameStart(recorded, current int64) bool {
	return max(current-recorded, recorded-current) <= startDrift
}

// Whether the process runs and is no zombie
func alive(pid int) bool {
	return pid > 0 && Running(pid) && !zombie(pid)
}

// Whether the process is alive and is still the one that started then, a missing start time standing for any
func ours(pid int, start int64) bool {
	if !alive(pid) {
		return false
	}
	if start == 0 {
		return true
	}
	current, ok := StartTime(pid)
	return ok && current == start
}

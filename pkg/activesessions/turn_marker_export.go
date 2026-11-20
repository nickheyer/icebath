package activesessions

// Creation time of this process, false when the platform cannot read it
func OwnStartTime() (float64, bool) { return ownStartTime() }

// Whether the pid is still the process that started then, known false when liveness cannot be decided
func PidLiveness(pid, processStart any) (alive, known bool) {
	switch pidLiveness(pid, processStart, false) {
	case livenessAlive:
		return true, true
	case livenessDead:
		return false, true
	}
	return false, false
}

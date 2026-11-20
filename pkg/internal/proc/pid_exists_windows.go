package proc

// Whether pid names a process as psutil pid_exists answers, the idle process counting as live
func PidExists(pid int) (bool, error) {
	if pid < 0 {
		return false, nil
	}
	if pid == 0 {
		return true, nil
	}
	return Exists(pid), nil
}

// Always false since a Windows signal zero sends a real control event that can kill the target
func SignalProbeLive(int) bool { return false }

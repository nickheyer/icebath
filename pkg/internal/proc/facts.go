package proc

// Kills pid outright with SIGKILL or TerminateProcess
func KillPID(pid int) error { return killPID(pid) }

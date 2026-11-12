//go:build !windows && !linux

package proc

// Every live pid is a process where the signal probe cannot reach a thread id
func threadGroupLeader(int) (bool, error) { return true, nil }

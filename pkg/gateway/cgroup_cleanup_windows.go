package gateway

import "errors"

// Refuses on Windows, which has no SIGKILL to stop a process group with
func cgroupSigkill(int) error {
	return errors.New("Windows has no SIGKILL to stop a process group with")
}

// Never skips a kill failure on Windows
func cgroupKillSkippable(error) bool { return false }

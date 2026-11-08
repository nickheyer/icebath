package gateway

import "errors"

// Fails as the signal module does on Windows, where SIGKILL is absent
func cgroupSigkill(int) error {
	return errors.New("AttributeError: module 'signal' has no attribute 'SIGKILL'")
}

// Never skips a kill failure on Windows
func cgroupKillSkippable(error) bool { return false }

package pm

import (
	"errors"
	"os/exec"
)

// ResolutionConflict is a uv resolver verdict that the requested set has no solution
type ResolutionConflict struct{ *InstallError }

func (e *ResolutionConflict) Unwrap() error { return e.InstallError }

// Reports whether a string list holds a value
func containsString(list []string, value string) bool {
	for _, s := range list {
		if s == value {
			return true
		}
	}
	return false
}

// Exit status of a waited child as Python reports it
func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return returnCode(exitErr), nil
	}
	return 0, err
}

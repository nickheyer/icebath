//go:build !windows

package gateway

import "errors"

// The OSError the native Windows bootstrap raises on every other host
func probeRefusedOwner(pid int) (reason, remedy string, ok bool, err error) {
	return "", "", false, errors.New("native Windows bootstrap requires Windows")
}

//go:build windows

package gateway

import "errors"

const hasSIGUSR1 = false

var errProcessGone = errors.New("process lookup failed")

// Windows has no SIGUSR1, the graceful restart path never sends it
func sendSIGUSR1(pid int) error { return errors.New("SIGUSR1 is not available on Windows") }

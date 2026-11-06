//go:build windows

package agent

import "errors"

// Windows sockets lack MSG_DONTWAIT so the probe fails like the AttributeError hermes raises there
func peekSocketDead(fd uintptr) (bool, error) {
	return false, errors.New("module 'socket' has no attribute 'MSG_DONTWAIT'")
}

//go:build windows

package gatewaycli

import (
	"strconv"
)

// AttributeError Python raises for an os function Windows does not provide
type osAttributeError struct{ name string }

func (e *osAttributeError) Error() string {
	return "module 'os' has no attribute " + strconv.Quote(e.name)
}

// Fails as os.mkfifo does on Windows where named pipes have no filesystem entry
func mkfifo(path string, mode uint32) error { return &osAttributeError{name: "mkfifo"} }

// Fails as os.chown does on Windows where files have no uid or gid owner
func lchown(path string, uid, gid int) error { return &osAttributeError{name: "chown"} }

//go:build !windows

package tools

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

func executable(path string) bool { return unix.Access(path, unix.X_OK) == nil }

// A kernel of this machine dies with icebath through its owned group, so it inherits no handle of icebath's
func parentHandle(map[string]string, *exec.Cmd) func() { return func() {} }

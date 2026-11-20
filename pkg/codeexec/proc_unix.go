//go:build !windows

package codeexec

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

func hideConsoleWindow(cmd *exec.Cmd) {}

func isExecutable(p string) bool { return unix.Access(p, unix.X_OK) == nil }

// POSIX kernels die with this process through their owned group, so no parent handle is inherited
func parentProcessHandle(map[string]string, *exec.Cmd) func() { return func() {} }

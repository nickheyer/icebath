//go:build windows

package botdesktop

import (
	"errors"
	"syscall"
)

var errUnsupported = errors.New("The agent desktop works only when icebath runs on Linux.")

func pgid(int) (int, error) { return 0, errUnsupported }

func reap(int) {}

func detach() {}

func sessionAttr() *syscall.SysProcAttr { return nil }

const sigterm = syscall.SIGTERM

func root() bool { return false }

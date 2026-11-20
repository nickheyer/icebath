//go:build !windows

package gatewaycli

import (
	"os"
	"os/signal"
)

// Console window state of a non Windows process, always None
func windowsConsoleWindowAttached() *bool { return nil }

// Ignores SIGINT, the only console control a non Windows host has
func absorbWindowsConsoleControls() { signal.Ignore(os.Interrupt) }

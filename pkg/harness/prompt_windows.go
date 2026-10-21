//go:build windows

package harness

import "runtime"

// Platform name, since Windows exposes no uname
func osVersion() string { return runtime.GOOS }

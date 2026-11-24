//go:build !unix && !windows

package hostutil

import "runtime"

func readOSName() string { return goosName() }

func readMachine() string { return runtime.GOARCH }

func readRelease() string { return "" }

func readPlatform() string { return joinLabel(OSName(), Machine()) }

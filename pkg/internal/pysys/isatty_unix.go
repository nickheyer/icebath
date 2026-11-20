//go:build !windows

package pysys

import "github.com/mattn/go-isatty"

// Reports a descriptor the C library isatty answers true for, a terminal device
func fdIsatty(fd uintptr) bool { return isatty.IsTerminal(fd) }

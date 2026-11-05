//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package cliui

// Platforms without termios have no kernel input queue to discard, as tcflush is absent there
func tcflushInput(int) {}

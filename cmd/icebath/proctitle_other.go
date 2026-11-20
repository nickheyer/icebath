//go:build !linux && !windows && !darwin

package main

// Leaves the title alone, the process is named only on Linux and macOS
func setProcessTitleOS(title string) {}

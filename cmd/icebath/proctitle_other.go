//go:build !linux && !windows && !darwin

package main

// Leaves the title alone because hermes names the process only on Linux and macOS
func setProcessTitleOS(title string) {}

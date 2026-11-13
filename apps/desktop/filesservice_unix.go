//go:build !windows

package main

// The user environment registry exists only on Windows
func windowsUserHome() string { return "" }

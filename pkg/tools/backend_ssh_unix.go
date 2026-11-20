//go:build !windows && !icebath_core

package tools

// Only Windows refuses symlinks without Developer Mode, every other error re-raises
func symlinkPrivilegeMissing(err error) bool { return false }

//go:build !windows

package app

import "io"

// No user scope registry PATH exists off Windows, so nothing is removed
func removePathFromWindowsRegistry(io.Writer, string, bool) []string { return []string{} }

// No user scope registry environment exists off Windows, so nothing is removed
func removeIcebathEnvVarsWindows(io.Writer) []string { return []string{} }

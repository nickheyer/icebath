//go:build !windows

package aws

func shellSplit(s string) ([]string, error) { return posixSplit(s) }

func expandPlatformVars(path string) string { return path }

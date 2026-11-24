//go:build unix && !darwin

package hostutil

// No product release on Unix systems other than Apple's
func productRelease() (string, string) { return "", "" }

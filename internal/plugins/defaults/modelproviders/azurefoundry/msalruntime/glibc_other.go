//go:build !(linux && (amd64 || arm64))

package msalruntime

// No GNU C library version is read on this build
func glibcVersion() (int, int, bool) { return 0, 0, false }

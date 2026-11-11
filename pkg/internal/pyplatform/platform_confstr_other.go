//go:build unix && !(linux && !386 && !arm && !faketime)

package pyplatform

// C library of a process that cannot ask a loaded libc, always absent so the executable is scanned
func confstrLibcVer() (string, string, bool) { return "", "", false }

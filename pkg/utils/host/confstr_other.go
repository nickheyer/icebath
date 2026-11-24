//go:build linux && !(!386 && !arm && (cgo || (!android && (amd64 || arm64 || loong64 || ppc64le || riscv64))) && !faketime)

package hostutil

// No confstr call on builds where the foreign function bridge is unavailable, the executable is read instead
func confstrLibc() (string, string, bool) { return "", "", false }

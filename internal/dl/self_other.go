//go:build !windows && !((darwin || freebsd || linux || netbsd) && !386 && !arm)

package dl

// The running process image, whose every symbol lookup reports the missing loader on this build
func Self() *Library { return &Library{path: "process"} }

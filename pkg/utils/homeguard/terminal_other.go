//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package homeguard

// Platforms without a controlling terminal a process can give up keep what they were started with
func leaveTerminal() {}

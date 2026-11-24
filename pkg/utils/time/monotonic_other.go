//go:build !(unix || windows) || aix || hurd

package timeutil

// No boot clock golang.org/x/sys can read here, so Monotonic counts on from the wall clock at process start
func bootSeconds() (float64, bool) { return 0, false }

//go:build !darwin

package app

// Keeps the platform poller, kqueue probing being a macOS concern
func tuiStdinSelectFallback() {}

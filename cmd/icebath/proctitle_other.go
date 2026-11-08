//go:build !linux && !windows

package main

// Names the main thread through pthread_setname_np the way hermes does on macOS
func setProcessTitleOS(title string) {
	setMainThreadNameDarwin(title)
}

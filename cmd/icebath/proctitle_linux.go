package main

import "golang.org/x/sys/unix"

// Names the process through prctl PR_SET_NAME capped at fifteen bytes, a refusal ignored
func setProcessTitleOS(title string) {
	if len(title) > 15 {
		title = title[:15]
	}
	_ = unix.Prctl(unix.PR_SET_NAME, uintptr(unsafePointer(append([]byte(title), 0))), 0, 0, 0)
}

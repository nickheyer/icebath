//go:build windows

package harnesshost

// STATUS_CONTROL_C_EXIT, the status Python returns after an unhandled KeyboardInterrupt on Windows
func exitInterrupted() int { return int(int32(-1073741510)) }

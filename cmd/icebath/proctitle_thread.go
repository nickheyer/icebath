//go:build linux || darwin

package main

import "runtime"

// Keeps the main goroutine on the main thread, the thread ps and top read the name of, until the title is set
func init() { runtime.LockOSThread() }

// Lets the main goroutine move between threads again once the title call is made
func releaseTitleThread() { runtime.UnlockOSThread() }

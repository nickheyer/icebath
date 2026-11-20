//go:build !linux && !darwin

package main

// Nothing to release where no title call names a thread
func releaseTitleThread() {}

//go:build !linux

package gfx

import "errors"

// Shared memory objects are made only on Linux, every other system sending pixels through the terminal
func shmPut(name string, data []byte) error {
	return errors.New("shared memory objects are not made here")
}

func shmWorks() bool { return false }

func shmSweep() {}

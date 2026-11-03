//go:build !linux

package gfx

import "errors"

// Error of every shared memory call on a system without /dev/shm objects
var errNoShm = errors.New("shared memory objects are made only on linux")

func shmPut(name string, data []byte) error { return errNoShm }

func shmLink(from, to string) error { return errNoShm }

func shmTake(name string) ([]byte, error) { return nil, errNoShm }

func shmRemove(name string) {}

func shmWorks() bool { return false }

func shmSweep(prefix string) {}

func shmReap() {}

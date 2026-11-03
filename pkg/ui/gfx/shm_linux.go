package gfx

import (
	"fmt"
	"os"
	"path/filepath"
)

// Directory POSIX shared memory objects live in
const shmDir = "/dev/shm/"

// Writes the bytes into a shared memory object of the name, which the terminal unlinks once it has read them
func shmPut(name string, data []byte) error {
	return os.WriteFile(shmDir+name, data, 0o600)
}

// Reports whether shared memory objects can be made here, trying one
func shmWorks() bool {
	name := "icebath-probe"
	if err := shmPut(name, []byte{0}); err != nil {
		return false
	}
	os.Remove(shmDir + name)
	return true
}

// Removes every shared memory object this process left for a terminal that never read it
func shmSweep() {
	matches, _ := filepath.Glob(shmDir + fmt.Sprintf("icebath-%d-*", os.Getpid()))
	for _, m := range matches {
		os.Remove(m)
	}
}

package gfx

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Directory POSIX shared memory objects live in
const shmDir = "/dev/shm/"

// Writes the bytes into a shared memory object of the name, which the terminal unlinks once it read them
func shmPut(name string, data []byte) error {
	return os.WriteFile(shmDir+name, data, 0o600)
}

// Gives a shared memory object a second name, the terminal unlinking that one while the first stays
func shmLink(from, to string) error {
	return os.Link(shmDir+from, shmDir+to)
}

// Reads a shared memory object back and removes it
func shmTake(name string) ([]byte, error) {
	data, err := os.ReadFile(shmDir + name)
	os.Remove(shmDir + name)
	return data, err
}

// Removes a shared memory object, ignoring one already gone
func shmRemove(name string) {
	if name != "" {
		os.Remove(shmDir + name)
	}
}

// Reports whether shared memory objects can be made here, making and removing one
func shmWorks() bool {
	name := fmt.Sprintf("icebath-%d-probe", os.Getpid())
	if err := shmPut(name, []byte{0}); err != nil {
		return false
	}
	shmRemove(name)
	return true
}

// Removes every shared memory object whose name starts with the prefix
func shmSweep(prefix string) {
	matches, _ := filepath.Glob(shmDir + prefix + "*")
	for _, m := range matches {
		os.Remove(m)
	}
}

// Removes objects over a minute old left by icebath processes that no longer run
func shmReap() {
	entries, _ := os.ReadDir(shmDir)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "icebath-")
		pid, _, found := strings.Cut(rest, "-")
		if !ok || !found {
			continue
		}
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < time.Minute {
			continue
		}
		if _, err := os.Stat("/proc/" + pid); os.IsNotExist(err) {
			os.Remove(shmDir + e.Name())
		}
	}
}

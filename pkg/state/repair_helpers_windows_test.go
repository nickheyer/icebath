//go:build windows

package state

import (
	"fmt"
	"os"
)

// Lock holding helper is a POSIX flock fixture, absent on Windows
func holdFlock(string) {
	fmt.Println("ERROR flock helper unavailable on windows")
	os.Exit(2)
}

//go:build !windows

package state

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Holds an exclusive flock on the lock path, printing locked once owned and sleeping until killed
func holdFlock(lockPath string) {
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX); err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	time.Sleep(30 * time.Second)
}

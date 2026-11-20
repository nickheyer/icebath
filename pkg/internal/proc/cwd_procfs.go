//go:build netbsd || solaris || illumos

package proc

import (
	"fmt"
	"os"
	"runtime"
)

// NetBSD and Solaris expose every process cwd as a procfs link
const CwdAvailable = true

// Working directory of pid read from its procfs cwd link
func Cwd(pid int) (string, error) {
	link := fmt.Sprintf("/proc/%d/cwd", pid)
	if runtime.GOOS != "netbsd" {
		link = fmt.Sprintf("/proc/%d/path/cwd", pid)
	}
	return os.Readlink(link)
}

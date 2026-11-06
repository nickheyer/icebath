package pysubprocess

import "os/exec"

// Popen returncode of an exited child, negative for a signal on POSIX
func Returncode(e *exec.ExitError) int { return returncode(e) }

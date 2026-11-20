//go:build windows

package agent

import "os"

// Terminates a process the way Popen.terminate does on Windows
func terminateProcess(p *os.Process) error { return p.Kill() }

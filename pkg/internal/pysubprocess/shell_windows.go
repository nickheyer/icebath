//go:build windows

package pysubprocess

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Command Popen runs for shell=True, ComSpec /c with the quoted string and a hidden window
func ShellCommand(command string) (*exec.Cmd, error) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		if !filepath.IsAbs(comspec) {
			return nil, errors.New("shell not found: neither %ComSpec% nor %SystemRoot% is set")
		}
	}
	cmd := exec.Command(comspec)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CmdLine: fmt.Sprintf(`%s /c "%s"`, comspec, command)}
	return cmd, nil
}

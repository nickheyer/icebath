package browser

import (
	"errors"
	"os/exec"
)

// The open command of macOS, which hands the URL to the default browser
func platformLaunchers() []Launcher {
	if _, err := exec.LookPath("open"); err != nil {
		return nil
	}
	return []Launcher{{Args: []string{"open", "%s"}}}
}

// No operating system URL handler outside Windows, every opener here is a command
func systemOpen(string) error { return errors.ErrUnsupported }

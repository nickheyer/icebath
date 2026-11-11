//go:build !windows

package dashboard

import "os/exec"

// Command of an argv run as given
func popenCommand(argv []string) *exec.Cmd { return exec.Command(argv[0], argv[1:]...) }

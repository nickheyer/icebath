package app

import (
	"os"
	"os/exec"
)

// Editors tried after VISUAL and EDITOR
var fallbackEditors = []string{"/usr/bin/editor", "/usr/bin/nano", "/usr/bin/pico", "/usr/bin/vi", "/usr/bin/emacs"}

// Runs an editor command on a file with the terminal attached, swapped by tests
var runEditorCommand = func(argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

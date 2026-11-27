//go:build windows

package hooks

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// A hook's command starts without a console window
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// Interpreters of the scripts Windows cannot run by themselves, there being no shebang
var interpreters = map[string]string{".sh": "bash", ".bash": "bash", ".py": "python"}

// Interpreter a script the command names needs before it: bash for a shell script, Python for a Python one; none for anything else
func (r *Registrant) interpreter(_ context.Context, argv []string) ([]string, error) {
	kind, ok := interpreters[strings.ToLower(filepath.Ext(argv[0]))]
	if !ok || !fsx.IsFile(argv[0]) {
		return nil, nil
	}
	if kind == "python" {
		path, err := exec.LookPath("python")
		if err != nil {
			return nil, err
		}
		return []string{path}, nil
	}
	if bash := r.packages.GitBash(); bash != "" {
		return []string{bash}, nil
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		return []string{bash}, nil
	}
	return nil, errors.New("No shell found. icebath needs bash (Git for Windows on Windows). Run `icebath pm install git` or install Git for Windows.")
}

// Program of a bare command, found the way CreateProcess searches PATH
func executable(name string, _ map[string]string) (string, error) {
	return exec.LookPath(name)
}

// Whether a start failed on a file Windows cannot run
func badFormat(err error) bool { return errors.Is(err, windows.ERROR_BAD_EXE_FORMAT) }

// Whether the file is there, all Windows tells of running or reading it
func usable(path string, _ bool) bool { return fsx.IsFile(path) }

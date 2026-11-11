package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Venv prefix holding pyvenv.cfg above or beside the resolved running binary, ok false outside any venv
var owningVenvPrefix = func() (string, bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	resolved, err := pypath.Resolve(exe)
	if err != nil {
		return "", false, err
	}
	exeDir := filepath.Dir(resolved)
	for _, prefix := range []string{filepath.Dir(exeDir), exeDir} {
		if isRegular(filepath.Join(prefix, "pyvenv.cfg")) {
			return prefix, true, nil
		}
	}
	return "", false, nil
}

// File name of the install binary a checkout root holds
func owningInstallBinary() string {
	if runtime.GOOS == "windows" {
		return "icebath.exe"
	}
	return "icebath"
}

// pathlib equality of two paths, the str form on POSIX and its lowercase on Windows
func owningSamePath(a, b string) bool {
	a, b = pypath.PathStr(a), pypath.PathStr(b)
	if runtime.GOOS == "windows" {
		return pystr.Lower(a) == pystr.Lower(b)
	}
	return a == b
}

// Checkout whose in tree venv the running binary sits in when that is not projectRoot, empty otherwise
func owningInstallRoot(projectRoot string) (string, error) {
	prefix, inVenv, err := owningVenvPrefix()
	if err != nil || !inVenv {
		return "", err
	}
	venv, err := pypath.Resolve(prefix)
	if err != nil {
		return "", err
	}
	root, err := pypath.Resolve(projectRoot)
	if err != nil {
		return "", err
	}
	if name := pypath.PathName(venv); name != "venv" && name != ".venv" {
		return "", nil
	}
	owner := pypath.PathParent(venv)
	if !isRegular(pypath.PathJoin(owner, owningInstallBinary())) {
		if owner, err = pmEnvironmentOwner(venv); err != nil {
			return "", err
		}
	}
	if owner == "" || owningSamePath(owner, root) {
		return "", nil
	}
	for _, entry := range strings.Split(os.Getenv("PYTHONPATH"), string(os.PathListSeparator)) {
		if entry == "" {
			continue
		}
		chosen, err := pypath.Resolve(entry)
		if err != nil {
			return "", err
		}
		if owningSamePath(root, chosen) {
			return "", nil
		}
	}
	return owner, nil
}

// Checkout a PM generation venv belongs to, proven by the install key hashing its recorded path, else empty
func pmEnvironmentOwner(venv string) (string, error) {
	generations := pypath.PathParent(pypath.PathParent(venv))
	state := pypath.PathParent(generations)
	if pypath.PathName(generations) != "environments" {
		return "", nil
	}
	data, err := os.ReadFile(pypath.PathJoin(state, "inputs", ".project-root"))
	if err != nil {
		return "", nil
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", errors.New("'utf-8' codec can't decode the recorded project root")
	}
	owner := pypath.PathStr(pystr.Strip(string(data)))
	if pm.InstallKey(owner) != pypath.PathName(state) || !isRegular(pypath.PathJoin(owner, owningInstallBinary())) {
		return "", nil
	}
	return owner, nil
}

// Runs this command with the owning install's binary and exits with its status, returning nil when nothing is redirected
func retargetToOwningInstall(ctx context.Context, projectRoot string) error {
	owner, err := owningInstallRoot(projectRoot)
	if err != nil || owner == "" {
		return err
	}
	prefix, _, err := owningVenvPrefix()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "⚠ %s is running the checkout at %s, not its own code.\n", pypath.PathJoin(owner, pypath.PathName(prefix)), projectRoot)
	fmt.Fprintf(os.Stdout, "→ Updating %s with its own updater; this also points its venv back at it.\n", owner)
	cmd := exec.CommandContext(ctx, pypath.PathJoin(owner, owningInstallBinary()), os.Args[1:]...)
	cmd.Dir = owner
	cmd.Env = append(os.Environ(), "PYTHONPATH="+owner)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var exit *exec.ExitError
	if err := cmd.Run(); errors.As(err, &exit) {
		return updateExit(pysubprocess.Returncode(exit))
	} else if err != nil {
		return err
	}
	return updateExit(0)
}

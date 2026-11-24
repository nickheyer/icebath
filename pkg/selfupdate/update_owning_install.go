//go:build !icebath_core && !icebath_noextras

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

	"github.com/nickheyer/icebath/pkg/pm"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Venv prefix holding pyvenv.cfg above or beside the resolved running binary, ok false outside any venv
var owningVenvPrefix = func() (string, bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	resolved, err := pathutil.Resolve(exe)
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
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.ToLower(a) == strings.ToLower(b)
	}
	return a == b
}

// Checkout whose in tree venv the running binary sits in when that is not projectRoot, empty otherwise
func owningInstallRoot(projectRoot string) (string, error) {
	prefix, inVenv, err := owningVenvPrefix()
	if err != nil || !inVenv {
		return "", err
	}
	venv, err := pathutil.Resolve(prefix)
	if err != nil {
		return "", err
	}
	root, err := pathutil.Resolve(projectRoot)
	if err != nil {
		return "", err
	}
	if name := pathutil.Name(venv); name != "venv" && name != ".venv" {
		return "", nil
	}
	owner := filepath.Dir(filepath.Clean(venv))
	if !isRegular(pathutil.Join(owner, owningInstallBinary())) {
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
		chosen, err := pathutil.Resolve(entry)
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
	generations := filepath.Dir(filepath.Clean(filepath.Dir(filepath.Clean(venv))))
	state := filepath.Dir(filepath.Clean(generations))
	if pathutil.Name(generations) != "environments" {
		return "", nil
	}
	data, err := os.ReadFile(pathutil.Join(state, "inputs", ".project-root"))
	if err != nil {
		return "", nil
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", errors.New("the recorded project root is not valid UTF-8")
	}
	owner := filepath.Clean(strings.TrimSpace(string(data)))
	if pm.InstallKey(owner) != pathutil.Name(state) || !isRegular(pathutil.Join(owner, owningInstallBinary())) {
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
	fmt.Fprintf(os.Stdout, "⚠ %s is running the checkout at %s, not its own code.\n", pathutil.Join(owner, pathutil.Name(prefix)), projectRoot)
	fmt.Fprintf(os.Stdout, "→ Updating %s with its own updater; this also points its venv back at it.\n", owner)
	cmd := exec.CommandContext(ctx, pathutil.Join(owner, owningInstallBinary()), os.Args[1:]...)
	cmd.Dir = owner
	cmd.Env = append(os.Environ(), "PYTHONPATH="+owner)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var exit *exec.ExitError
	if err := cmd.Run(); errors.As(err, &exit) {
		return updateExit(executil.ExitCode(exit.ProcessState))
	} else if err != nil {
		return err
	}
	return updateExit(0)
}

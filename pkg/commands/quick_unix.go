//go:build !windows

package commands

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/tools"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
)

// Shell argv for the gateway command in a new session, start_new_session making the shell its group leader
func gatewayProgram(command string, _ bool) (quickProgram, error) {
	return posixShellProgram(command, &syscall.SysProcAttr{Setsid: true})
}

// Builds sh -c with the command, refusing one holding a NUL byte that no argv entry can carry
func posixShellProgram(command string, attr *syscall.SysProcAttr) (quickProgram, error) {
	sh, err := executil.Shell()
	if err != nil {
		return quickProgram{}, err
	}
	if strings.IndexByte(command, 0) >= 0 {
		return quickProgram{}, errEmbeddedNUL
	}
	return quickProgram{path: sh, argv: []string{sh, "-c", command}, attr: attr}, nil
}

// Refuses a NUL byte in the environment or the cwd, which execve and chdir cannot carry
func finishProgram(prog quickProgram, cwd string, env []string) (quickProgram, error) {
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return prog, errEmbeddedNUL
		}
	}
	if strings.IndexByte(cwd, 0) >= 0 {
		return prog, errEmbeddedNUL
	}
	return prog, nil
}

// Environment entries in the insertion order of the build_subprocess_env dict, as execve receives them
func quickEnvList(env *tools.ChildEnv) []string { return env.Environ() }

// Inherited stdin, none when descriptor zero is closed so the child starts without one
func quickStdin() (*os.File, func()) {
	if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); err != nil {
		return nil, func() {}
	}
	return os.Stdin, func() {}
}

// Spawn failure, the chdir error naming cwd when entering the directory is what failed
func spawnError(err error, cwd string) error {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "fork/exec" && cwd != "" {
		if cerr := chdirError(cwd); cerr != nil {
			return &fs.PathError{Op: "chdir", Path: cwd, Err: cerr}
		}
	}
	return err
}

// Errno chdir would fail with, resolution then directory type then search permission for the effective ids
func chdirError(dir string) error {
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.ENOTDIR
	}
	return unix.Faccessat(unix.AT_FDCWD, dir, unix.X_OK, unix.AT_EACCESS)
}

// SIGKILL to a shell not yet reaped, a shell already gone ignored and a refused signal returned
func killShell(q *quickProc) error {
	if closed(q.exited) {
		return nil
	}
	err := q.proc.Signal(syscall.SIGKILL)
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// SIGKILL to the process group the shell led, a group already gone or not ours ignored
func killQuickGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

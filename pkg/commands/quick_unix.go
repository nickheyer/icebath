//go:build !windows

package commands

import (
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Shell argv for the gateway command in a new session, start_new_session making the shell its group leader
func gatewayProgram(command string, _ bool) (quickProgram, error) {
	return posixShellProgram([]any{command}, &syscall.SysProcAttr{Setsid: true})
}

// Shell argv for the CLI command in the caller's process group, any other value going through list
func cliProgram(command any) (quickProgram, error) {
	items := []any{command}
	switch command.(type) {
	case string, []byte:
	default:
		var err error
		if items, err = pyListItems(command); err != nil {
			return quickProgram{}, err
		}
	}
	return posixShellProgram(items, &syscall.SysProcAttr{})
}

// Builds sh -c with the items, each checked for its type then an embedded NUL as fork_exec converts them
func posixShellProgram(items []any, attr *syscall.SysProcAttr) (quickProgram, error) {
	sh := pysubprocess.UnixShell()
	argv := []string{sh, "-c"}
	for _, item := range items {
		var arg string
		switch v := item.(type) {
		case string:
			arg = v
		case []byte:
			arg = string(v)
		default:
			return quickProgram{}, notPathLike(item)
		}
		if strings.IndexByte(arg, 0) >= 0 {
			return quickProgram{}, pyError("embedded null byte")
		}
		argv = append(argv, arg)
	}
	return quickProgram{path: sh, argv: argv, attr: attr}, nil
}

// ValueError fork_exec raises for a NUL in the environment or the cwd
func finishProgram(prog quickProgram, cwd string, env []string) (quickProgram, error) {
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return prog, pyError("embedded null byte")
		}
	}
	if strings.IndexByte(cwd, 0) >= 0 {
		return prog, pyError("embedded null byte")
	}
	return prog, nil
}

// Environment entries in the order the process environment lists them, keys it lacks appended sorted
func quickEnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	seen := map[string]bool{}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || seen[kv[:i]] {
			continue
		}
		if v, ok := env[kv[:i]]; ok {
			seen[kv[:i]] = true
			out = append(out, kv[:i]+"="+v)
		}
	}
	rest := make([]string, 0, len(env)-len(seen))
	for k := range env {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Inherited stdin, none when descriptor zero is closed so the child starts without one as Popen leaves it
func quickStdin() (*os.File, func()) {
	if _, err := unix.FcntlInt(0, unix.F_GETFD, 0); err != nil {
		return nil, func() {}
	}
	return os.Stdin, func() {}
}

// Python str of a spawn failure, the cwd named when the child chdir is what failed
func spawnErrorText(err error, cwd string) string {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "fork/exec" && cwd != "" {
		if cerr := chdirError(cwd); cerr != nil {
			if text, ok := pypath.ErrnoText(cerr); ok {
				return text + ": " + pyrepr.StrRepr(cwd)
			}
		}
	}
	return pypath.OSErrorText(err)
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

// Popen.kill, SIGKILL to a shell not yet reaped with ProcessLookupError suppressed and PermissionError returned
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

// killpg of the group the shell led, ProcessLookupError and PermissionError suppressed
func killQuickGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// After a timeout kill run waits for the shell and closes the pipes, leaving any grandchild running
func reapAfterTimeoutKill(q *quickProc) {
	<-q.exited
	q.stdout.close()
	q.stderr.close()
}

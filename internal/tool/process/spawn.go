package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
)

const envLaunchTimeout = 10 * time.Second

type LaunchError struct {
	ExitCode int
	Output   string
}

func (e *LaunchError) Error() string {
	out := e.Output
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf("launch wrapper exited %d without a PID: %s", e.ExitCode, out)
}

type scopeLeftError struct{ err error }

func (e *scopeLeftError) Error() string {
	return "PTY scope could not be reaped; refusing pipe fallback to avoid duplicate command execution"
}

func (e *scopeLeftError) Unwrap() error { return e.err }

type pipeJob struct {
	cmd    *exec.Cmd
	out    *os.File
	done   chan struct{}
	status int
}

func newPipeJob(cmd *exec.Cmd, out *os.File) *pipeJob {
	j := &pipeJob{cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		j.status = proc.ExitOf(cmd.ProcessState).Status()
		close(j.done)
	}()
	return j
}

func (j *pipeJob) PID() int { return j.cmd.Process.Pid }

func (j *pipeJob) Stdin() io.WriteCloser { return nil }

func (j *pipeJob) Output() io.Reader {
	if j.out == nil {
		return nil
	}
	return j.out
}

func (j *pipeJob) Wait(ctx context.Context) (int, error) {
	select {
	case <-j.done:
		return j.status, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (j *pipeJob) Exited() (int, bool) {
	select {
	case <-j.done:
		return j.status, true
	default:
		return 0, false
	}
}

func (j *pipeJob) Kill() error {
	if err := j.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (j *pipeJob) Close() error {
	if j.out == nil {
		return nil
	}
	return j.out.Close()
}

func (r *Registry) spawnEnv(ctx context.Context, extra map[string]string) (map[string]string, error) {
	env, err := r.host.ChildEnv(ctx, extra)
	if err != nil {
		return nil, err
	}
	env["PYTHONUNBUFFERED"] = "1"
	return env, nil
}

func (r *Registry) scopeArgv(ctx context.Context, s *Session, command, unitSuffix, label string) ([]string, error) {
	shellPath, err := r.host.Shell(ctx)
	if err != nil {
		return nil, err
	}
	argv := []string{shellPath, "-lic", "set +m; " + command}
	supervised := isLinux && r.role.SupervisedGateway()
	if supervised && r.scopes.available(ctx) {
		s.systemdUnit = workerUnitTag + unitSuffix + ".scope"
		return r.scopes.buildArgv(argv, unitSuffix), nil
	}
	if supervised {
		slog.Debug(label + " background executor not isolated in a systemd scope " +
			"(systemd-run --user unavailable); worker shares the gateway cgroup.")
	}
	return argv, nil
}

func (r *Registry) SpawnLocal(ctx context.Context, o SpawnOptions) (*Session, error) {
	base, err := r.spawnEnv(ctx, o.Env)
	if err != nil {
		return nil, err
	}
	parentPath := ""
	if o.RestoreLoginPath {
		parentPath = base["PATH"]
	}
	safe := r.host.RestoreLoginPath(r.host.RewriteBackground(o.Command), parentPath)
	cwd := o.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	s := newSession(ctx, newProcessID(), o, r.host.SafeCwd(cwd))
	log, err := r.workerOutputLog(ctx, s.id)
	if err != nil {
		return nil, err
	}
	scopeAttempted := false
	if o.PTY && log == "" {
		err := r.spawnPTY(ctx, s, safe, base)
		if err == nil {
			return s, nil
		}
		if errors.Is(err, ErrClosed) {
			return nil, err
		}
		if ptyMissing(err) {
			s.ptyFallback = ptyUnavailableReason
			slog.Warn("pseudo-terminals unavailable, falling back to pipe mode")
		} else {
			s.ptyFallback = "the PTY spawn failed (" + err.Error() + ")"
			slog.Warn("PTY spawn failed, falling back to pipe mode", "err", err)
			if s.systemdUnit != "" {
				scopeAttempted = true
				if !stopUnit(ctx, s.systemdUnit) {
					return nil, &scopeLeftError{err: err}
				}
				s.systemdUnit = ""
			}
		}
	}
	unitSuffix := s.id
	if scopeAttempted {
		unitSuffix = s.id + "-pipe-fallback"
	}
	command := safe
	if log != "" {
		command = recordExitCommand(log, safe)
	}
	argv, err := r.scopeArgv(ctx, s, command, unitSuffix, "Local")
	if err != nil {
		return nil, err
	}
	env := maps.Clone(base)
	if s.systemdUnit != "" {
		env = userBusEnv(env)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.cwd
	cmd.Env = envList(env)
	var readEnd, writeEnd *os.File
	if log != "" {
		if writeEnd, err = openOutputLog(log); err != nil {
			return nil, err
		}
	} else if readEnd, writeEnd, err = os.Pipe(); err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = writeEnd, writeEnd
	err = r.startJob(ctx, s, cmd, o.PersistOnRelease)
	_ = writeEnd.Close()
	if err != nil {
		if readEnd != nil {
			_ = readEnd.Close()
		}
		return nil, err
	}
	s.process = newPipeJob(cmd, readEnd)
	s.outputLog = log
	s.pid = cmd.Process.Pid
	s.hostStart = startFingerprint(s.pid)
	reader := func(c context.Context) { r.readPipe(c, s) }
	if log != "" {
		reader = func(c context.Context) { r.readLog(c, s) }
	}
	if err := r.trackStarted(ctx, s, reader); err != nil {
		r.reapUntracked(ctx, s)
		return nil, err
	}
	return s, nil
}

func (r *Registry) spawnPTY(ctx context.Context, s *Session, safe string, base map[string]string) error {
	argv, err := r.scopeArgv(ctx, s, safe, s.id, "PTY")
	if err != nil {
		return err
	}
	env := maps.Clone(base)
	if s.systemdUnit != "" {
		env = userBusEnv(env)
	}
	for _, k := range []string{"GIT_PAGER", "PAGER"} {
		if _, ok := env[k]; !ok {
			env[k] = "cat"
		}
	}
	j, err := startPTY(argv, s.cwd, envList(env))
	if err != nil {
		return err
	}
	r.ownPTYJob(s, j)
	s.pid = j.pid()
	s.hostStart = startFingerprint(s.pid)
	s.pty = j
	if err := r.trackStarted(ctx, s, func(c context.Context) { r.readPTY(c, s) }); err != nil {
		r.reapUntracked(ctx, s)
		return err
	}
	return nil
}

func (r *Registry) Adopt(ctx context.Context, p tool.Process, o SpawnOptions, outputSoFar string) (*Session, error) {
	if p == nil {
		return nil, errors.New("no foreground process to adopt")
	}
	s := newSession(ctx, newProcessID(), o, o.Cwd)
	s.process = p
	s.pid = p.PID()
	s.hostStart = startFingerprint(s.pid)
	handed := r.takeHandoffGroup(s, p)
	if outputSoFar != "" {
		s.appendOutput(outputSoFar)
	}
	if err := r.trackStarted(ctx, s, func(c context.Context) { r.readPipe(c, s) }); err != nil {
		r.returnHandoffGroup(s, p, handed)
		return nil, err
	}
	return s, nil
}

func envTempDir(env tool.Environment) string {
	if t := env.TempDir(); strings.HasPrefix(t, "/") {
		if t = strings.TrimRight(t, "/"); t == "" {
			return "/"
		}
		return t
	}
	return os.TempDir()
}

func (r *Registry) SpawnEnv(ctx context.Context, env tool.Environment, o SpawnOptions) (*Session, error) {
	s := newSession(ctx, newProcessID(), o, o.Cwd)
	s.env = env
	s.pidScope = scopeSandbox
	temp := envTempDir(env)
	prefix := temp + "/icebath_bg_" + s.id
	logPath, pidPath, exitPath := prefix+".log", prefix+".pid", prefix+".exit"
	q := shell.Quote
	launch := "mkdir -p " + q(temp) + " && " +
		"( nohup bash -lc " + q(o.Command) + " > " + q(logPath) + " 2>&1; " +
		"rc=$?; printf '%s\\n' \"$rc\" > " + q(exitPath) + " ) & " +
		"echo $! > " + q(pidPath) + " && cat " + q(pidPath)
	res, err := env.Execute(ctx, launch, tool.ExecOptions{Cwd: o.Cwd, Timeout: envLaunchTimeout})
	if err != nil {
		return nil, err
	}
	output := strings.TrimSpace(res.Output)
	found := false
	for _, line := range outputLines(output) {
		line = strings.TrimSpace(line)
		if line == "" || !asciiDigits([]byte(line)) {
			continue
		}
		if pid, err := strconv.Atoi(line); err == nil {
			s.pid, found = pid, true
			break
		}
	}
	if !found {
		return nil, &LaunchError{ExitCode: res.ExitCode, Output: output}
	}
	if err := r.trackStarted(ctx, s, func(c context.Context) { r.pollEnv(c, s, env, logPath, pidPath, exitPath) }); err != nil {
		return nil, err
	}
	return s, nil
}

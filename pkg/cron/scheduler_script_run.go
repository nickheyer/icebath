package cron

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds between run claim heartbeats of a long one shot script
var runClaimHeartbeatSeconds = 60.0

// Kill paths, spawn, interpreter and bash lookups, script run and claim refresh a test swaps
var (
	cronStartScript            = startCronScript
	cronSysExecutable          = cronHostPython
	cronKillProcessTree        = provider.KillProcessTree
	cronTerminateScriptProcess = func(s *Store, p *cronScriptProcess) error { return s.terminateCronScriptProcess(p) }
	cronTerminateScriptTree    = func(s *Store, p *cronScriptProcess) error { return s.terminateCronScriptTree(p) }
	cronWhich                  = pyshutil.Which
	cronBashFallback           = "/bin/bash"
	cronRunJobScript           = (*Store).runJobScript
	cronHeartbeatRunClaim      = (*Store).heartbeatRunClaim
)

// Any float the value converts to, the parse a zero meaning unlimited timeout uses
func anyFloat(raw any) (float64, bool, error) {
	v, err := pyint.Float(raw)
	return v, err == nil, err
}

// Session DB init bound from env, config, then ten seconds, zero passing through as unlimited
func (s *Store) getSessionDBTimeout(ctx context.Context) float64 {
	if v, ok := timeoutSetting(s, ctx, "ICEBATH_CRON_SESSION_DB_TIMEOUT", "session_db_timeout_seconds", "cron.session_db_timeout_seconds", anyFloat); ok {
		return v
	}
	return 10.0
}

// Script path confined to the profile scripts folder, or the refusal the run reports
func (s *Store) resolveJobScriptPath(scriptPath string) (string, string, error) {
	scriptsDir := filepath.Join(s.Home(), "scripts")
	if err := ensureCronDir(scriptsDir); err != nil {
		return "", "", err
	}
	scriptsDirResolved, err := pypath.Resolve(scriptsDir)
	if err != nil {
		return "", "", err
	}
	if strings.IndexByte(scriptPath, 0) >= 0 {
		return "", "Blocked: script path contains a NUL byte: " + pyrepr.StrRepr(scriptPath), nil
	}
	raw, err := pypath.PathExpanduser(scriptPath)
	if err != nil {
		return "", "Blocked: script path is not a valid filesystem path: " + pyrepr.StrRepr(scriptPath), nil
	}
	target := raw
	if !pypath.IsAbs(raw) {
		target = pypath.PathJoin(scriptsDir, raw)
	}
	path, err := pypath.Resolve(target)
	if err != nil {
		return "", "", err
	}
	if _, inside := pypath.PathRelativeTo(path, scriptsDirResolved); !inside {
		return "", fmt.Sprintf("Blocked: script path resolves outside the scripts directory (%s): %s", scriptsDirResolved, pyrepr.StrRepr(scriptPath)), nil
	}
	info, err := pypath.PathlibStat(path)
	if err != nil {
		return "", "", err
	}
	if info == nil {
		return "", fmt.Sprintf("Script not found: %s. Cron scripts are looked up only in this profile's folder "+
			"(%s); if the job was copied from another profile, copy the script "+
			"there too, or edit the job with `icebath cron edit`.", path, scriptsDirResolved), nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Sprintf("Script path is not a file: %s", path), nil
	}
	return path, "", nil
}

// Absolute Python interpreter a job names, or the refusal for a bare, missing, non executable or non Python path
func resolveCronInterpreter(interpreter string) (string, string, error) {
	raw := pystr.Strip(interpreter)
	unresolvable := func(text string) (string, string, error) {
		return "", fmt.Sprintf("Unable to resolve interpreter path %s: %s", pyrepr.StrRepr(raw), text), nil
	}
	resolved, err := pypath.PathExpanduser(raw)
	if err != nil {
		return unresolvable(err.Error())
	}
	if !pypath.IsAbs(resolved) {
		return "", fmt.Sprintf("Interpreter must be an absolute or ~-prefixed path (got %s). "+
			"Bare names like 'python3' are not stable across PATH changes.", pyrepr.StrRepr(raw)), nil
	}
	if strings.IndexByte(resolved, 0) >= 0 {
		return "", "", &pypath.ValueError{Msg: "stat: embedded null character in path"}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "Interpreter not found: " + raw, nil
		}
		return unresolvable(pypath.OSErrorText(err))
	}
	target, err := pypath.Resolve(resolved)
	if err != nil {
		return unresolvable(pypath.OSErrorText(err))
	}
	names := []string{pystr.Lower(pypath.PathName(resolved)), pystr.Lower(pypath.PathName(target))}
	if !info.Mode().IsRegular() {
		return "", "Interpreter path is not a file: " + resolved, nil
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", "Interpreter is not executable: " + resolved, nil
	}
	for _, name := range names {
		if !cronPythonImageRe.MatchString(name) || strings.HasPrefix(name, "pythonw") {
			return "", "Interpreter must be a Python executable (python, python3, python3.12, ...): " + resolved, nil
		}
	}
	return resolved, "", nil
}

// Lower cased file name of a Python interpreter executable
var cronPythonImageRe = regexp.MustCompile(`^pythonw?(?:\d+(?:\.\d+)*)?(?:\.exe)?\n?$`)

// First python3 or python on PATH, the interpreter Python scripts run on
func cronHostPython() (string, error) {
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(p), `\microsoft\windowsapps\`) {
			continue
		}
		return p, nil
	}
	return "", errors.New("no Python interpreter found on PATH")
}

// Argv by extension, bash for sh scripts, else the job interpreter or the Python on PATH
func (s *Store) scriptArgv(path string, interpreter *string) ([]string, string, error) {
	suffix := pystr.Lower(pypath.PathSuffix(path))
	if suffix == ".sh" || suffix == ".bash" {
		bash, found := cronWhich("bash", nil)
		if !found {
			if info, statErr := os.Stat(cronBashFallback); statErr == nil && info.Mode().IsRegular() {
				bash, found = cronBashFallback, true
			}
		}
		if !found {
			return nil, fmt.Sprintf("Cannot run .sh/.bash script %s: bash not found on PATH. "+
				"On Windows, install Git for Windows (which ships Git Bash) "+
				"or rewrite the script as Python (.py).", pyrepr.StrRepr(pypath.PathName(path))), nil
		}
		return []string{bash, path}, "", nil
	}
	if interpreter != nil && pystr.Strip(*interpreter) != "" {
		pythonExe, refusal, err := resolveCronInterpreter(*interpreter)
		if err != nil {
			return nil, "", err
		}
		if pythonExe == "" {
			return nil, refusal, nil
		}
		return []string{pythonExe, path}, "", nil
	}
	host, err := cronSysExecutable()
	if err != nil {
		return nil, "", err
	}
	return []string{host, path}, "", nil
}

// Running script with its own pipe ends, closed once the process exits and both pipes reach EOF
type cronScriptProcess struct {
	cmd            *exec.Cmd
	stdout, stderr *os.File
	outBuf, errBuf bytes.Buffer
	exited         chan struct{}
	drained        chan struct{}
	state          *os.ProcessState
}

// Whether the process has exited, the poll check
func (p *cronScriptProcess) poll() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// Waits for exit up to the bound, true once exited
func (p *cronScriptProcess) wait(d time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// Starts the script in its own session with stdout and stderr captured
func startCronScript(argv []string, dir string, env map[string]string) (*cronScriptProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = cronScriptSysProcAttr()
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{outR, outW, errR, errW} {
			f.Close()
		}
		return nil, err
	}
	outW.Close()
	errW.Close()
	p := &cronScriptProcess{cmd: cmd, stdout: outR, stderr: errR, exited: make(chan struct{}), drained: make(chan struct{})}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); io.Copy(&p.outBuf, outR) }()
	go func() { defer readers.Done(); io.Copy(&p.errBuf, errR) }()
	go func() {
		p.state, _ = cmd.Process.Wait()
		close(p.exited)
	}()
	go func() {
		readers.Wait()
		<-p.exited
		close(p.drained)
	}()
	return p, nil
}

// Hard stop of the script and every child it spawned, a script that outlives the kill reported as TimeoutExpired
func (s *Store) terminateCronScriptProcess(p *cronScriptProcess) error {
	if p.poll() {
		return nil
	}
	terminateCronScriptGroup(p)
	if p.wait(time.Second) {
		return nil
	}
	p.cmd.Process.Kill()
	if p.wait(time.Second) {
		return nil
	}
	seconds := 1.0
	return &pysubprocess.TimeoutExpired{Cmd: p.cmd.Args, TimeoutFloat: &seconds}
}

// Kills the whole tree through the provider walk, falling back to the process group path
func (s *Store) terminateCronScriptTree(p *cronScriptProcess) error {
	if p.poll() {
		return nil
	}
	fallback := func(reason string) error {
		s.log.Warn(reason + "; falling back to process-group termination")
		return cronTerminateScriptProcess(s, p)
	}
	pid := p.cmd.Process.Pid
	if pid <= 0 {
		return fallback(fmt.Sprintf("Cron script tree-kill received invalid pid %d", pid))
	}
	signalled, panicked := func() (ok bool, failed bool) {
		defer func() {
			if recover() != nil {
				failed = true
			}
		}()
		return cronKillProcessTree(pid, 0), false
	}()
	if panicked {
		return fallback(fmt.Sprintf("Cron script tree-kill failed for pid %d", pid))
	}
	if signalled {
		return nil
	}
	return fallback(fmt.Sprintf("Cron script tree-kill reported no signal for pid %d", pid))
}

// Reaps a terminated script within bounds, abandoning pipes a surviving descendant keeps open
func drainScriptPipes(p *cronScriptProcess) {
	select {
	case <-p.drained:
		return
	case <-time.After(5 * time.Second):
	}
	p.cmd.Process.Kill()
	p.stdout.Close()
	p.stderr.Close()
	p.wait(5 * time.Second)
}

// Text mode decode of captured output in the locale encoding, UTF-8 on Windows, bad bytes replaced and newlines universal
func scriptText(b []byte) (string, error) {
	encoding := "utf-8"
	if runtime.GOOS != "windows" {
		encoding = pycodecs.LocaleEncoding()
	}
	text, err := pycodecs.DecodeReplace(b, encoding)
	if err != nil {
		return "", err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// Python str of an error the script run reports, OS failures in their errno form
func scriptErrorText(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Redacts both streams, a failing redactor replacing both with the failure marker
func (s *Store) redactScriptOutput(stdout, stderr string) (outText, errText string) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn(fmt.Sprintf("Failed to redact sensitive text from output: %v", r))
			outText, errText = "[REDACTED - redaction failed]", "[REDACTED - redaction failed]"
		}
	}()
	env := &tools.Env{Home: s.Home()}
	return tools.RedactSensitiveTextWith(env, stdout, tools.RedactOptions{}), tools.RedactSensitiveTextWith(env, stderr, tools.RedactOptions{})
}

// Runs a job script under the timeout and cancel event, success and stdout or the error text for the agent
func (s *Store) runJobScript(ctx context.Context, scriptPath, workdir string, cancel CancelSource, interpreter *string) (bool, string, error) {
	path, refusal, err := s.resolveJobScriptPath(scriptPath)
	if err != nil {
		return false, "", err
	}
	if path == "" {
		return false, refusal, nil
	}
	scriptTimeout := s.getScriptTimeout(ctx)
	ok, output, err := s.runResolvedScript(ctx, path, workdir, cancel, interpreter, scriptTimeout)
	if err != nil {
		return false, "Script execution failed: " + scriptErrorText(err), nil
	}
	return ok, output, nil
}

// Tool environment of the store home, flagged multiplex when the fire runs under multiplex semantics
func (s *Store) scriptToolEnv(ctx context.Context) *tools.Env {
	env := &tools.Env{Home: s.Home(), Values: &sync.Map{}}
	if provider.IsMultiplexActive(ctx) {
		env.Values.Store("session.multiplex", true)
	}
	return env
}

// Body of the script run whose errors become the execution failure text
func (s *Store) runResolvedScript(ctx context.Context, path, workdir string, cancel CancelSource, interpreter *string, scriptTimeout int) (bool, string, error) {
	argv, refusal, err := s.scriptArgv(path, interpreter)
	if err != nil {
		return false, "", err
	}
	if argv == nil {
		return false, refusal, nil
	}
	env, err := tools.BuildSubprocessEnv(ctx, s.scriptToolEnv(ctx), tools.SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true, StripLaunchProfile: true})
	if err != nil {
		return false, "", err
	}
	dir := workdir
	if dir == "" {
		dir = filepath.Dir(path)
	}
	proc, err := cronStartScript(argv, dir, env)
	if err != nil {
		return false, "", err
	}
	deadline := time.Now().Add(time.Duration(scriptTimeout) * time.Second)
	for {
		if cancel != nil && cancel.IsSet() {
			if err := cronTerminateScriptTree(s, proc); err != nil {
				return false, "", err
			}
			drainScriptPipes(proc)
			return false, "Script cancelled: " + cancelReason(cancel), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if err := cronTerminateScriptTree(s, proc); err != nil {
				return false, "", err
			}
			drainScriptPipes(proc)
			return false, fmt.Sprintf("Script timed out after %ds: %s", scriptTimeout, path), nil
		}
		finished := false
		select {
		case <-proc.drained:
			finished = true
		case <-time.After(min(100*time.Millisecond, remaining)):
		}
		if finished {
			break
		}
	}
	stdout, err := scriptText(proc.outBuf.Bytes())
	if err != nil {
		return false, "", err
	}
	stderr, err := scriptText(proc.errBuf.Bytes())
	if err != nil {
		return false, "", err
	}
	stdout, stderr = s.redactScriptOutput(pystr.Strip(stdout), pystr.Strip(stderr))
	if code := cronScriptReturnCode(proc.state); code != 0 {
		parts := []string{fmt.Sprintf("Script exited with code %d", code)}
		if stderr != "" {
			parts = append(parts, "stderr:\n"+stderr)
		}
		if stdout != "" {
			parts = append(parts, "stdout:\n"+stdout)
		}
		return false, strings.Join(parts, "\n"), nil
	}
	return true, stdout, nil
}

// Runs a heartbeat loop on its own goroutine, the returned channel closing once the loop returns
func startHeartbeatThread(loop func()) <-chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		loop()
	}()
	return finished
}

// Runs the script while heartbeating an owned one shot claim so a long script keeps its claim
func (s *Store) runJobScriptWithClaimHeartbeat(ctx context.Context, job *Job, scriptPath, workdir string, cancel CancelSource) (bool, string, error) {
	run := func() (bool, string, error) {
		return cronRunJobScript(s, ctx, scriptPath, workdir, cancel, job.Interpreter)
	}
	owner := ""
	if job.RunClaim != nil {
		owner = job.RunClaim.By
	}
	if job.Schedule == nil || job.Schedule.Kind != "once" || owner == "" {
		return run()
	}
	jobID := job.ID
	stop := make(chan struct{})
	finished := startHeartbeatThread(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(runClaimHeartbeatSeconds * float64(time.Second))):
				if _, err := cronHeartbeatRunClaim(s, home.WithOverride(ctx, s.home), jobID, owner); err != nil {
					s.log.Debug(fmt.Sprintf("Job '%s': script run_claim heartbeat failed", jobID), "error", err)
				}
			}
		}
	})
	defer func() {
		close(stop)
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	}()
	return run()
}

// Interpreter guard path resolution for a job script under the store home, false when it has none
func resolveScriptPath(s *Store, script string) (string, bool) {
	return tools.ResolveLifecycleScriptPath(s.Home(), script)
}

// Refuses a prompt or script carrying a gateway lifecycle command, scripts read from the store home
func checkGatewayLifecycle(s *Store, prompt, script string) error {
	return tools.CheckGatewayLifecycle(&tools.Env{Home: s.Home()}, prompt, script)
}

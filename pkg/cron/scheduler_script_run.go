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
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds between run claim heartbeats of a long one shot script
var runClaimHeartbeatSeconds = 60.0

const posixScriptBootstrap = `import importlib.machinery, os, sys, types
repo, script = sys.argv[1], sys.argv[2]
sys.argv = [script] + sys.argv[3:]
if sys.flags.safe_path:
    sys.path.insert(0, repo)
else:
    sys.path[0:1] = [os.path.dirname(script), repo]
main = types.ModuleType("__main__")
main.__file__ = script
main.__loader__ = importlib.machinery.SourceFileLoader("__main__", script)
main.__cached__ = None
main.__builtins__ = __builtins__
sys.modules["__main__"] = main
with open(script, "rb") as f:
    code = compile(f.read(), script, "exec")
exec(code, main.__dict__)
`

var cronInterpreterImageRe = regexp.MustCompile(`^pythonw?(?:\d+(?:\.\d+)*)?(?:\.exe)?$`)

// Kill paths a test swaps to observe which one a timeout or cancel reaches
var (
	cronKillProcessTree        = provider.KillProcessTree
	cronTerminateScriptProcess = func(s *Store, p *cronScriptProcess) { s.terminateCronScriptProcess(p) }
	cronTerminateScriptTree    = func(s *Store, p *cronScriptProcess) { s.terminateCronScriptTree(p) }
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

// Lowercased keys and stripped values of a venv pyvenv.cfg, empty when unreadable
func readWindowsPyvenvCfg(venvDir string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(filepath.Join(venvDir, "pyvenv.cfg"))
	if err != nil {
		return out
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	for _, raw := range pystr.Splitlines(pystr.DecodeUTF8Replace(data), false) {
		key, value, found := strings.Cut(raw, "=")
		if found {
			out[pystr.Lower(pystr.Strip(key))] = pystr.Strip(value)
		}
	}
	return out
}

// Path.exists semantics, missing for ENOENT ENOTDIR EBADF ELOOP and an error for any other stat failure
func pathlibStat(p string) (fs.FileInfo, bool, error) {
	info, err := os.Stat(p)
	if err == nil {
		return info, true, nil
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP) || errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

// Path.relative_to over resolved paths
func pathIsRelativeTo(p, base string) bool {
	if p == base {
		return true
	}
	if !strings.HasSuffix(base, string(filepath.Separator)) {
		base += string(filepath.Separator)
	}
	return strings.HasPrefix(p, base)
}

// Path.expanduser, false where pathlib raises for an undeterminable home
func pathlibExpanduser(p string) (string, bool) {
	expanded := pypath.Expanduser(p)
	if strings.HasPrefix(p, "~") && strings.HasPrefix(expanded, "~") {
		return "", false
	}
	return expanded, true
}

// Path.resolve, absolute and with every existing link followed
func pathlibResolve(p string) (string, error) {
	return pypath.Realpath(p, pypath.Lax)
}

// Script path confined to the profile scripts folder, or the refusal the run reports
func (s *Store) resolveJobScriptPath(scriptPath string) (string, string, error) {
	scriptsDir := filepath.Join(s.Home(), "scripts")
	if err := ensureCronDir(scriptsDir); err != nil {
		return "", "", err
	}
	scriptsDirResolved, err := pathlibResolve(scriptsDir)
	if err != nil {
		return "", "", err
	}
	if strings.IndexByte(scriptPath, 0) >= 0 {
		return "", "Blocked: script path contains a NUL byte: " + pyrepr.StrRepr(scriptPath), nil
	}
	raw, ok := pathlibExpanduser(scriptPath)
	if !ok {
		return "", "Blocked: script path is not a valid filesystem path: " + pyrepr.StrRepr(scriptPath), nil
	}
	target := raw
	if !filepath.IsAbs(raw) {
		target = filepath.Join(scriptsDir, raw)
	}
	path, err := pathlibResolve(target)
	if err != nil {
		return "", "", err
	}
	if !pathIsRelativeTo(path, scriptsDirResolved) {
		return "", fmt.Sprintf("Blocked: script path resolves outside the scripts directory (%s): %s", scriptsDirResolved, pyrepr.StrRepr(scriptPath)), nil
	}
	info, exists, err := pathlibStat(path)
	if err != nil {
		return "", "", err
	}
	if !exists {
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
func resolveCronInterpreter(interpreter string) (string, string) {
	raw := pystr.Strip(interpreter)
	resolved, ok := pathlibExpanduser(raw)
	if !ok {
		return "", fmt.Sprintf("Unable to resolve interpreter path %s: Could not determine home directory.", pyrepr.StrRepr(raw))
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Sprintf("Interpreter must be an absolute or ~-prefixed path (got %s). "+
			"Bare names like 'python3' are not stable across PATH changes.", pyrepr.StrRepr(raw))
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "Interpreter not found: " + raw
		}
		return "", fmt.Sprintf("Unable to resolve interpreter path %s: %s", pyrepr.StrRepr(raw), pypath.OSErrorText(err))
	}
	target, err := pathlibResolve(resolved)
	if err != nil {
		return "", fmt.Sprintf("Unable to resolve interpreter path %s: %s", pyrepr.StrRepr(raw), pypath.OSErrorText(err))
	}
	names := []string{pystr.Lower(filepath.Base(resolved)), pystr.Lower(filepath.Base(target))}
	if !info.Mode().IsRegular() {
		return "", "Interpreter path is not a file: " + resolved
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", "Interpreter is not executable: " + resolved
	}
	for _, name := range names {
		if !cronInterpreterImageRe.MatchString(name) || strings.HasPrefix(name, "pythonw") {
			return "", "Interpreter must be a Python executable (python, python3, python3.12, ...): " + resolved
		}
	}
	return resolved, ""
}

// First python3 or python on PATH, the interpreter a non managed install runs scripts on
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

// Managed installs run a py script on the dependency venv with the checkout first on sys.path and lazy installs off
func posixCronScriptArgv(ctx context.Context, script string) ([]string, map[string]string, error) {
	repo := pm.RepoRoot()
	resident, err := pm.FindResidentRuntime()
	if err != nil {
		return nil, nil, err
	}
	if resident == nil {
		python, err := cronHostPython()
		if err != nil {
			return nil, nil, err
		}
		return []string{python, script}, map[string]string{}, nil
	}
	python, err := pm.ProjectPython(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	if info, err := os.Stat(python); err != nil || !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("dependency environment interpreter is missing: %s", python)
	}
	return []string{python, "-c", posixScriptBootstrap, repo, script}, map[string]string{"ICEBATH_DISABLE_LAZY_INSTALLS": "1"}, nil
}

// Hidden output capturing Windows Python with venv paths overlaid instead of the uv launcher
func windowsCronPythonInvocation(ctx context.Context, pythonExe string) (string, map[string]string, error) {
	if runtime.GOOS != "windows" {
		return pythonExe, map[string]string{}, nil
	}
	interpreter := pythonExe
	venvDir := filepath.Dir(filepath.Dir(interpreter))
	overlay := map[string]string{}
	if strings.ToLower(filepath.Base(interpreter)) == "pythonw.exe" {
		sibling := filepath.Join(filepath.Dir(interpreter), "python.exe")
		if _, err := os.Stat(sibling); err == nil {
			interpreter = sibling
		}
	}
	repo := pm.RepoRoot()
	resident, err := pm.FindResidentRuntime()
	if err != nil {
		return "", nil, err
	}
	if resident != nil {
		environment, err := pm.CommittedVenv(ctx, repo)
		if err != nil {
			return "", nil, err
		}
		if environment != "" {
			site, err := pm.SitePackages(environment)
			if err != nil {
				return "", nil, err
			}
			return resident.Python, map[string]string{"PYTHONPATH": strings.Join([]string{repo, site}, string(os.PathListSeparator))}, nil
		}
	}
	cfg := readWindowsPyvenvCfg(venvDir)
	homeDir := cfg["home"]
	sitePackages := filepath.Join(venvDir, "Lib", "site-packages")
	if _, isUV := cfg["uv"]; isUV && homeDir != "" {
		basePython := filepath.Join(homeDir, "python.exe")
		if _, err := os.Stat(basePython); err == nil {
			if _, err := os.Stat(sitePackages); err == nil {
				interpreter = basePython
				overlay["VIRTUAL_ENV"] = venvDir
				entries := []string{repo, sitePackages}
				if existing := os.Getenv("PYTHONPATH"); existing != "" {
					entries = append(entries, existing)
				}
				overlay["PYTHONPATH"] = strings.Join(entries, string(os.PathListSeparator))
			}
		}
	}
	return interpreter, overlay, nil
}

// Base interpreter argv that runs a script after site.addsitedir so editable installs import
func (s *Store) windowsCronBootstrapArgv(pythonExe string, overlay map[string]string, scriptPath string) []string {
	sitePackages := filepath.Join(overlay["VIRTUAL_ENV"], "Lib", "site-packages")
	for _, item := range strings.Split(overlay["PYTHONPATH"], string(os.PathListSeparator)) {
		if filepath.Base(item) == "site-packages" {
			sitePackages = item
			break
		}
	}
	if info, err := os.Stat(sitePackages); err != nil || !info.IsDir() {
		s.log.Warn(fmt.Sprintf("Windows cron script: venv site-packages %s not found; running "+
			"without .pth processing (editable installs may be unimportable)", sitePackages))
		return []string{pythonExe, scriptPath}
	}
	bootstrap := "import os, runpy, site, sys;" +
		"site.addsitedir(" + pyrepr.StrRepr(sitePackages) + ");" +
		"script = sys.argv[1];" +
		"sys.argv = [script] + sys.argv[2:];" +
		"sys.path.insert(0, os.path.dirname(os.path.abspath(script)));" +
		"runpy.run_path(script, run_name='__main__')"
	return []string{pythonExe, "-c", bootstrap, scriptPath}
}

// Argv and env overlay by extension, bash for sh scripts, else the job interpreter or the install Python
func (s *Store) scriptArgv(ctx context.Context, path string, interpreter *string) ([]string, map[string]string, string, error) {
	suffix := strings.ToLower(filepath.Ext(path))
	if suffix == ".sh" || suffix == ".bash" {
		bash, err := exec.LookPath("bash")
		if err != nil {
			bash = ""
			if info, statErr := os.Stat("/bin/bash"); statErr == nil && info.Mode().IsRegular() {
				bash = "/bin/bash"
			}
		}
		if bash == "" {
			return nil, map[string]string{}, fmt.Sprintf("Cannot run .sh/.bash script %s: bash not found on PATH. "+
				"On Windows, install Git for Windows (which ships Git Bash) "+
				"or rewrite the script as Python (.py).", pyrepr.StrRepr(filepath.Base(path))), nil
		}
		return []string{bash, path}, map[string]string{}, "", nil
	}
	if interpreter != nil && pystr.Strip(*interpreter) != "" {
		pythonExe, refusal := resolveCronInterpreter(*interpreter)
		if pythonExe == "" {
			return nil, map[string]string{}, refusal, nil
		}
		return []string{pythonExe, path}, map[string]string{}, "", nil
	}
	if runtime.GOOS != "windows" {
		argv, overlay, err := posixCronScriptArgv(ctx, path)
		return argv, overlay, "", err
	}
	host, err := cronHostPython()
	if err != nil {
		return nil, nil, "", err
	}
	pythonExe, overlay, err := windowsCronPythonInvocation(ctx, host)
	if err != nil {
		return nil, nil, "", err
	}
	if len(overlay) > 0 {
		return s.windowsCronBootstrapArgv(pythonExe, overlay, path), overlay, "", nil
	}
	return []string{pythonExe, path}, overlay, "", nil
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

// Hard stop of the script and every child it spawned, then reaped within a bounded wait
func (s *Store) terminateCronScriptProcess(p *cronScriptProcess) {
	if p.poll() {
		return
	}
	terminateCronScriptGroup(p)
	if !p.wait(time.Second) {
		p.cmd.Process.Kill()
		p.wait(time.Second)
	}
}

// Kills the whole tree through the provider walk, falling back to the process group path
func (s *Store) terminateCronScriptTree(p *cronScriptProcess) {
	if p.poll() {
		return
	}
	fallback := func(reason string) {
		s.log.Warn(reason + "; falling back to process-group termination")
		cronTerminateScriptProcess(s, p)
	}
	pid := p.cmd.Process.Pid
	if pid <= 0 {
		fallback(fmt.Sprintf("Cron script tree-kill received invalid pid %d", pid))
		return
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
		fallback(fmt.Sprintf("Cron script tree-kill failed for pid %d", pid))
		return
	}
	if signalled {
		return
	}
	fallback(fmt.Sprintf("Cron script tree-kill reported no signal for pid %d", pid))
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

// Text mode decode of captured output, invalid bytes replaced and newlines universal
func scriptText(b []byte) string {
	text := pystr.DecodeUTF8Replace(b)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
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
		return false, "Script execution failed: " + err.Error(), nil
	}
	return ok, output, nil
}

// Body of the script run whose errors become the execution failure text
func (s *Store) runResolvedScript(ctx context.Context, path, workdir string, cancel CancelSource, interpreter *string, scriptTimeout int) (bool, string, error) {
	argv, overlay, refusal, err := s.scriptArgv(ctx, path, interpreter)
	if err != nil {
		return false, "", err
	}
	if argv == nil {
		return false, refusal, nil
	}
	env, err := tools.BuildSubprocessEnv(ctx, &tools.Env{Home: s.Home()}, tools.SubprocessEnvOptions{StripLaunchProfile: true})
	if err != nil {
		return false, "", err
	}
	for k, v := range overlay {
		env[k] = v
	}
	dir := workdir
	if dir == "" {
		dir = filepath.Dir(path)
	}
	proc, err := startCronScript(argv, dir, env)
	if err != nil {
		return false, "", err
	}
	deadline := time.Now().Add(time.Duration(scriptTimeout) * time.Second)
	for {
		if cancel != nil && cancel.IsSet() {
			cronTerminateScriptTree(s, proc)
			drainScriptPipes(proc)
			return false, "Script cancelled: " + cancelReason(cancel), nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			cronTerminateScriptTree(s, proc)
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
	stdout := pystr.Strip(scriptText(proc.outBuf.Bytes()))
	stderr := pystr.Strip(scriptText(proc.errBuf.Bytes()))
	stdout, stderr = s.redactScriptOutput(stdout, stderr)
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

// Runs the script while heartbeating an owned one shot claim so a long script keeps its claim
func (s *Store) runJobScriptWithClaimHeartbeat(ctx context.Context, job *Job, scriptPath, workdir string, cancel CancelSource) (bool, string, error) {
	run := func() (bool, string, error) {
		return s.runJobScript(ctx, scriptPath, workdir, cancel, job.Interpreter)
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
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(time.Duration(runClaimHeartbeatSeconds * float64(time.Second)))
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := s.heartbeatRunClaim(jobID, owner); err != nil {
					s.log.Debug(fmt.Sprintf("Job '%s': script run_claim heartbeat failed", jobID), "error", err)
				}
			}
		}
	}()
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

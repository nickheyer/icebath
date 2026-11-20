package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const (
	DefaultPhaseTimeout = 600.0
	DefaultReadyTimeout = 60.0
	tailChars           = 2000
)

var PhaseOrder = []string{"bootstrap", "build", "test"}

// Process ownership of pkg/tools process_identity, installed by the pkg/plugins init
var (
	SpawnOwned                   func(ctx context.Context, cmd *exec.Cmd, purpose string) error
	ReleaseProcessGroupOnceEmpty func(pgid int, poll func(), timeout time.Duration) bool
)

// PhaseResult is one recipe command run, a nil exit code meaning it timed out
type PhaseResult struct {
	Phase      string
	Command    string
	ExitCode   *int
	Duration   float64
	OutputTail string
	TimedOut   bool
}

// Reports a zero exit without a timeout
func (p *PhaseResult) OK() bool { return p.ExitCode != nil && *p.ExitCode == 0 && !p.TimedOut }

// JSON mapping of the phase with the duration rounded to milliseconds
func (p *PhaseResult) ToDict() *pyjson.Object {
	var code any
	if p.ExitCode != nil {
		code = int64(*p.ExitCode)
	}
	return pyjson.ObjectOf("phase", p.Phase, "command", p.Command, "exitCode", code,
		"duration", pyint.Round(p.Duration, 3), "ok", p.OK(), "timedOut", p.TimedOut, "outputTail", p.OutputTail)
}

// ReadinessResult is the start phase poll of the app URL
type ReadinessResult struct {
	URL        string
	Ready      bool
	StatusCode *int
	Duration   float64
	Error      *string
	OutputTail string
}

// JSON mapping of the readiness poll with the duration rounded to milliseconds
func (r *ReadinessResult) ToDict() *pyjson.Object {
	var code, errText any
	if r.StatusCode != nil {
		code = int64(*r.StatusCode)
	}
	if r.Error != nil {
		errText = *r.Error
	}
	return pyjson.ObjectOf("url", r.URL, "ready", r.Ready, "statusCode", code,
		"duration", pyint.Round(r.Duration, 3), "error", errText, "outputTail", r.OutputTail)
}

// VerifyResult is a whole verification pass
type VerifyResult struct {
	RecipeName string
	Phases     []*PhaseResult
	Readiness  *ReadinessResult
}

// Reports every phase passing and the app answering when it was started
func (v *VerifyResult) OK() bool {
	return allOK(v.Phases) && (v.Readiness == nil || v.Readiness.Ready)
}

func allOK(phases []*PhaseResult) bool {
	for _, p := range phases {
		if !p.OK() {
			return false
		}
	}
	return true
}

// JSON mapping of the pass, readiness null when the app never started
func (v *VerifyResult) ToDict() *pyjson.Object {
	phases := make([]any, len(v.Phases))
	for i, p := range v.Phases {
		phases[i] = p.ToDict()
	}
	var readiness any
	if v.Readiness != nil {
		readiness = v.Readiness.ToDict()
	}
	return pyjson.ObjectOf("recipe", v.RecipeName, "ok", v.OK(), "phases", phases, "readiness", readiness)
}

// Last tail characters of text counted in code points
func tail(text string) string {
	if n := pystr.Len(text); n > tailChars {
		return pystr.Slice(text, n-tailChars, n)
	}
	return text
}

// Text mode output, the locale codec with errors replace and universal newlines
func decodeOutput(raw []byte) (string, error) {
	text, err := pycodecs.DecodeReplace(raw, pycodecs.LocaleEncoding())
	return pystr.TranslateNewlines(text), err
}

// Byte sink shared by a reader goroutine and the timeout path
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) snapshot() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.buf.Bytes())
}

// Shell command with stdin on the null device and stdout plus stderr on one pipe the caller reads
func shellWithPipe(command, root string) (*exec.Cmd, *os.File, *os.File, error) {
	cmd, err := pysubprocess.ShellCommand(command)
	if err != nil {
		return nil, nil, nil, &OSError{Class: "FileNotFoundError", Err: err}
	}
	cmd.Dir = root
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, &OSError{Err: err}
	}
	cmd.Stdout, cmd.Stderr = w, w
	return cmd, r, w, nil
}

// Runs one phase command in root, killing the shell and keeping the output so far once it outlives timeout
func runPhaseCommandReal(phase, command, root string, timeout float64, onOutput func(string)) (*PhaseResult, error) {
	started := time.Now()
	cmd, r, w, err := shellWithPipe(command, root)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, &OSError{Err: err}
	}
	w.Close()
	var out lockedBuffer
	eof := make(chan struct{})
	go func() {
		_, _ = io.Copy(&out, r)
		close(eof)
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	var waitErr error
	procDone, pipeDone, timedOut := false, false, false
	for !(procDone && pipeDone) && !timedOut {
		select {
		case waitErr = <-exited:
			procDone = true
		case <-eof:
			pipeDone = true
			eof = nil
		case <-timer.C:
			timedOut = true
		}
	}
	result := &PhaseResult{Phase: phase, Command: command}
	var output string
	if timedOut {
		if !procDone {
			_ = cmd.Process.Kill()
			<-exited
		}
		if runtime.GOOS == "windows" {
			// Windows collects the whole remaining output as text after the kill
			if !pipeDone {
				<-eof
			}
			r.Close()
			if output, err = decodeOutput(out.snapshot()); err != nil {
				return nil, err
			}
		} else {
			output = pystr.DecodeUTF8Replace(out.snapshot())
			r.Close()
		}
		result.TimedOut = true
	} else {
		r.Close()
		if output, err = decodeOutput(out.snapshot()); err != nil {
			return nil, err
		}
		code := 0
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = pysubprocess.Returncode(ee)
		} else if waitErr != nil {
			return nil, &OSError{Err: waitErr}
		}
		result.ExitCode = &code
	}
	result.Duration = time.Since(started).Seconds()
	if onOutput != nil && output != "" {
		onOutput(output)
	}
	result.OutputTail = tail(output)
	return result, nil
}

// Phase runner, swapped by tests
var runPhaseCommand = runPhaseCommandReal

// Child process view the teardown drives, a real child or a test double
type process interface {
	pid() int
	poll() bool
	terminate() error
	kill() error
	wait(timeout time.Duration) bool
}

// Started child reaped by its own goroutine so poll and wait never block past their timeout
type childProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func newChildProcess(cmd *exec.Cmd) *childProcess {
	c := &childProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	return c
}

func (c *childProcess) pid() int { return c.cmd.Process.Pid }

func (c *childProcess) poll() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *childProcess) wait(timeout time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stops the app group with SIGTERM then SIGKILL after ten seconds, only the direct child when it shares our group
func terminateProcessGroup(p process) error {
	if p.poll() {
		return nil
	}
	pgid, hasGroup := 0, false
	if killpg != nil && getpgid != nil {
		g, err := getpgid(p.pid())
		switch {
		case err == nil:
			pgid, hasGroup = g, true
		case !isLookupOrPermission(err):
			return &OSError{Err: err}
		}
	}
	if hasGroup && pgid != p.pid() {
		hasGroup = false
	}
	stop := func(force bool, fallback func() error) error {
		if hasGroup {
			return killpg(pgid, force)
		}
		return fallback()
	}
	if err := stop(false, p.terminate); err != nil {
		if isLookupOrPermission(err) {
			return nil
		}
		return &OSError{Err: err}
	}
	if !p.wait(10 * time.Second) {
		if err := stop(true, p.kill); err != nil && !isLookupOrPermission(err) {
			return &OSError{Err: err}
		}
		p.wait(5 * time.Second)
	}
	return nil
}

// Boots the start command as an owned group, polls readiness, then tears the group down and collects its output
func runStartPhase(ctx context.Context, recipe *Recipe, root string, readyTimeout float64, portOverride *int) (*ReadinessResult, error) {
	if recipe.Start == nil {
		return nil, errors.New("AssertionError")
	}
	port := "8000"
	switch {
	case portOverride != nil && *portOverride != 0:
		port = fmt.Sprint(*portOverride)
	case recipe.PortTrue || recipe.Port != nil && *recipe.Port != 0:
		port = recipe.portText()
	}
	url := "http://127.0.0.1:" + port + recipe.ReadinessPath
	started := time.Now()
	cmd, r, w, err := shellWithPipe(*recipe.Start, root)
	if err != nil {
		return nil, err
	}
	if err := SpawnOwned(ctx, cmd, "verify-app"); err != nil {
		r.Close()
		w.Close()
		return nil, &OSError{Err: err}
	}
	w.Close()
	proc := newChildProcess(cmd)
	ready, status, pollText, pollErr := pollReadiness(url, readyTimeout, time.Second)
	if err := terminateProcessGroup(proc); err != nil {
		r.Close()
		return nil, err
	}
	if proc.poll() {
		ReleaseProcessGroupOnceEmpty(proc.pid(), func() { proc.poll() }, time.Second)
	}
	raw, readErr := io.ReadAll(r)
	r.Close()
	output := ""
	if readErr == nil {
		if output, err = decodeOutput(raw); err != nil {
			return nil, err
		}
	}
	if pollErr != nil {
		return nil, pollErr
	}
	return &ReadinessResult{URL: url, Ready: ready, StatusCode: status, Duration: time.Since(started).Seconds(),
		Error: pollText, OutputTail: tail(output)}, nil
}

// docker compose ps probe result, its exit code and decoded streams
type composeProbeResult struct {
	Returncode     int
	Stdout, Stderr string
}

// Read only docker compose ps in root, 15 s bounding exit and both pipes, FileNotFoundError and TimeoutExpired surfacing as their own errors
var composeProbe = func(root string) (*composeProbeResult, error) {
	cmd := exec.Command("docker", "compose", "ps", "--status", "running", "--format", "{{.Name}}")
	cmd.Dir = root
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, &OSError{Err: err}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, &OSError{Err: err}
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	startErr := cmd.Start()
	outW.Close()
	errW.Close()
	if startErr != nil {
		outR.Close()
		errR.Close()
		return nil, startError(startErr)
	}
	var stdout, stderr lockedBuffer
	outEOF, errEOF := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = io.Copy(&stdout, outR)
		close(outEOF)
	}()
	go func() {
		_, _ = io.Copy(&stderr, errR)
		close(errEOF)
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	var waitErr error
	procDone, timedOut := false, false
	for !(procDone && outEOF == nil && errEOF == nil) && !timedOut {
		select {
		case waitErr = <-exited:
			procDone = true
		case <-outEOF:
			outEOF = nil
		case <-errEOF:
			errEOF = nil
		case <-timer.C:
			timedOut = true
		}
	}
	if timedOut {
		if !procDone {
			_ = cmd.Process.Kill()
			<-exited
		}
		if runtime.GOOS == "windows" {
			// Windows drains both pipes to their end after the kill before raising
			if outEOF != nil {
				<-outEOF
			}
			if errEOF != nil {
				<-errEOF
			}
		}
		outR.Close()
		errR.Close()
		return nil, errComposeProbeTimeout
	}
	outR.Close()
	errR.Close()
	res := &composeProbeResult{}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		res.Returncode = pysubprocess.Returncode(ee)
	} else if waitErr != nil {
		return nil, &OSError{Err: waitErr}
	}
	enc := pycodecs.LocaleEncoding()
	out, err := pycodecs.Decode(stdout.snapshot(), enc)
	if err != nil {
		return nil, err
	}
	errText, err := pycodecs.Decode(stderr.snapshot(), enc)
	if err != nil {
		return nil, err
	}
	res.Stdout, res.Stderr = pystr.TranslateNewlines(out), pystr.TranslateNewlines(errText)
	return res, nil
}

var errComposeProbeTimeout = errors.New("TimeoutExpired")

// FileNotFoundError for a missing executable or working directory, the OSError otherwise
func startError(err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &OSError{Class: "FileNotFoundError", Err: err}
	}
	return &OSError{Err: err}
}

// Reason docker compose build and up must not run at root, empty to proceed
func composeLiveStateReasonReal(root string) (string, error) {
	res, err := composeProbe(root)
	var oe *OSError
	switch {
	case errors.As(err, &oe) && oe.Class == "FileNotFoundError":
		return "", nil
	case errors.Is(err, errComposeProbeTimeout):
		return "docker compose ps timed out after 15s; live containers cannot be ruled out", nil
	case err != nil:
		return "", err
	}
	if res.Returncode != 0 {
		text := res.Stderr
		if text == "" {
			text = res.Stdout
		}
		detail := pystr.Splitlines(pystr.Strip(text), false)
		last := "no output"
		if len(detail) > 0 {
			last = detail[len(detail)-1]
		}
		return fmt.Sprintf("docker compose ps failed (exit %d): %s", res.Returncode, last), nil
	}
	var names []string
	for _, line := range pystr.Splitlines(res.Stdout, false) {
		if pystr.Strip(line) != "" {
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	return "this compose project already has running container(s): " + strings.Join(names, ", "), nil
}

// Compose live state check, swapped by tests
var composeLiveStateReason = composeLiveStateReasonReal

// VerifyOptions are the run_verify keyword arguments, start from NewVerifyOptions for the defaults
type VerifyOptions struct {
	Phases        []string
	PhaseTimeout  float64
	ReadyTimeout  float64
	SkipStart     bool
	PortOverride  *int
	StopOnFailure bool
	OnOutput      func(string)
}

// Options with the run_verify defaults, every phase, 600 s phases, 60 s readiness and stop on failure
func NewVerifyOptions() VerifyOptions {
	return VerifyOptions{PhaseTimeout: DefaultPhaseTimeout, ReadyTimeout: DefaultReadyTimeout, StopOnFailure: true}
}

func (r *Recipe) phaseCommands(phase string) []string {
	switch phase {
	case "bootstrap":
		return r.Bootstrap
	case "build":
		return r.Build
	}
	return r.Test
}

// Runs the selected phases in order then boots, polls and tears down the app, refusing a compose project with live containers
func RunVerify(ctx context.Context, root string, recipe *Recipe, opts VerifyOptions) (*VerifyResult, error) {
	selected := opts.Phases
	if len(selected) == 0 {
		selected = append(slices.Clone(PhaseOrder), "start")
	}
	result := &VerifyResult{RecipeName: recipe.Name}
	mutating := slices.Contains(selected, "build") || slices.Contains(selected, "start") && !opts.SkipStart
	if recipe.Kind == "compose" && mutating {
		reason, err := composeLiveStateReason(root)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			command := "docker compose build"
			if len(recipe.Build) > 0 {
				command = recipe.Build[0]
			}
			one := 1
			result.Phases = append(result.Phases, &PhaseResult{Phase: "build", Command: command, ExitCode: &one,
				OutputTail: "Refusing to run: " + reason + ". `docker compose build` + `up` would replace " +
					"live containers on an image-hash change, destroying any container-local " +
					"state they carry. If you intend to rebuild this live deployment, run " +
					"`docker compose build`/`up` yourself."})
			return result, nil
		}
	}
	for _, phase := range PhaseOrder {
		if !slices.Contains(selected, phase) {
			continue
		}
		for _, command := range recipe.phaseCommands(phase) {
			pr, err := runPhaseCommand(phase, command, root, opts.PhaseTimeout, opts.OnOutput)
			if err != nil {
				return nil, err
			}
			result.Phases = append(result.Phases, pr)
			if !pr.OK() && opts.StopOnFailure {
				return result, nil
			}
		}
	}
	if !opts.SkipStart && slices.Contains(selected, "start") && recipe.Start != nil && *recipe.Start != "" && allOK(result.Phases) {
		readiness, err := runStartPhase(ctx, recipe, root, opts.ReadyTimeout, opts.PortOverride)
		if err != nil {
			return nil, err
		}
		result.Readiness = readiness
	}
	return result, nil
}

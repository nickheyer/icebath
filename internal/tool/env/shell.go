package env

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	waitBoundGrace         = 2 * time.Second
	fetchTimeout           = 300 * time.Second
	sudoProbeTimeout       = 3 * time.Second
	defaultSnapshotTimeout = 30 * time.Second
	nonloginProbeCap       = 15 * time.Second
	activityInterval       = 10 * time.Second
	traceHeartbeat         = 30 * time.Second
	drainIdleTick          = 100 * time.Millisecond
	activityLabel          = "terminal command running"
)

var (
	_ tool.Environment = (*Shell)(nil)
	_ Backend          = (*Local)(nil)
	_ CwdKeeper        = (*Local)(nil)
	_ PathQuoter       = (*Local)(nil)
	_ ScopedNames      = (*Local)(nil)
)

type ShellOptions struct {
	Cwd         string
	Timeout     time.Duration
	Prepare     CommandPreparer
	Foreground  *Foreground
	Passthrough *Passthrough
	Secrets     *secret.Scope
	Homes       *home.Resolver
	Home        string
	Limits      tool.OutputLimits
}

type Shell struct {
	b         Backend
	traits    Traits
	o         ShellOptions
	id        string
	tempDir   string
	snapPath  string
	cwdFile   string
	cwdMark   string
	pipeMark  string
	debug     bool
	maxOutput int

	mu             sync.Mutex
	cwd            string
	snapshotReady  bool
	preferNonlogin bool
	excluded       map[string]bool
	forwards       map[forwardKey]*portForward
	envType        string
	sandboxHome    string
}

func (s *Shell) setEnvType(kind string) {
	s.mu.Lock()
	s.envType = kind
	s.mu.Unlock()
}

func (s *Shell) EnvType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.envType != "" {
		return s.envType
	}
	return s.b.Kind()
}

func (s *Shell) setSandboxHome(h string) {
	s.mu.Lock()
	s.sandboxHome = h
	s.mu.Unlock()
}

func (s *Shell) SandboxHome() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PluginSandboxHome(s.sandboxHome)
}

func NewShell(b Backend, o ShellOptions) (*Shell, error) {
	switch {
	case b == nil:
		return nil, &tool.UnfilledError{Name: "env.Backend"}
	case o.Prepare == nil:
		return nil, &tool.UnfilledError{Name: "env.CommandPreparer"}
	case o.Foreground == nil:
		return nil, &tool.UnfilledError{Name: "env.ShellOptions.Foreground"}
	case o.Passthrough == nil:
		return nil, &tool.UnfilledError{Name: "env.ShellOptions.Passthrough"}
	case o.Secrets == nil:
		return nil, &tool.UnfilledError{Name: "env.ShellOptions.Secrets"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "env.ShellOptions.Homes"}
	case o.Home == "":
		return nil, &tool.UnfilledError{Name: "env.ShellOptions.Home"}
	case o.Timeout <= 0:
		return nil, &tool.UnfilledError{Name: "env.ShellOptions.Timeout"}
	}
	traits := b.Traits()
	tempDir := traits.TempDir
	if tempDir == "" {
		tempDir = "/tmp"
	}
	dir := strings.TrimRight(tempDir, "/")
	if dir == "" {
		dir = "/"
	}
	o.Limits = o.Limits.Resolved()
	maxOutput := o.Limits.MaxBytes
	id := randomHex(12)
	return &Shell{
		b: b, traits: traits, o: o, id: id, tempDir: tempDir, maxOutput: maxOutput,
		snapPath: dir + "/icebath-snap-" + id + ".sh",
		cwdFile:  dir + "/icebath-cwd-" + id + ".txt",
		cwdMark:  cwdMarker(id),
		pipeMark: pipeMarker(id),
		debug:    config.EnvEnabled("ICEBATH_DEBUG_INTERRUPT", ""),
		cwd:      o.Cwd,
		excluded: map[string]bool{},
	}, nil
}

func (s *Shell) Backend() Backend { return s.b }

func (s *Shell) Kind() string { return s.b.Kind() }

func (s *Shell) Local() bool { return s.traits.Local }

func (s *Shell) HostFilesystem() bool { return s.traits.HostFilesystem }

func (s *Shell) Persistent() bool { return s.traits.Persistent }

func (s *Shell) TempDir() string { return s.tempDir }

func (s *Shell) Cwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

func (s *Shell) SetCwd(cwd string) {
	s.mu.Lock()
	s.cwd = cwd
	s.mu.Unlock()
}

func (s *Shell) excludedNames(ctx context.Context) []string {
	if !s.traits.ProfileScopedPassthrough {
		return nil
	}
	if s.o.Secrets.Multiplexed(ctx) {
		names, err := s.o.Passthrough.All(ctx)
		if err != nil {
			slog.Debug("Could not refresh profile-scoped snapshot exclusions", "err", err)
		} else {
			if sn, ok := s.b.(ScopedNames); ok {
				names = append(names, sn.ScopedPassthroughNames(ctx)...)
			}
			s.mu.Lock()
			for _, n := range names {
				if ValidShellEnvName(n) {
					s.excluded[n] = true
				}
			}
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.excluded))
	for n := range s.excluded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Shell) quoteShellPath(p string) string {
	if q, ok := s.b.(PathQuoter); ok {
		return q.QuoteShellPath(p)
	}
	return shell.Quote(p)
}

func (s *Shell) scriptPaths(cwd string) scriptPaths {
	quotedCwd := quoteCwdForCd(cwd)
	if q, ok := s.b.(PathQuoter); ok {
		quotedCwd = q.QuoteCwd(cwd)
	}
	return scriptPaths{
		quotedCwd:       quotedCwd,
		quotedSnap:      s.quoteShellPath(s.snapPath),
		snapTmpTemplate: s.quoteShellPath(s.snapPath + snapTmpSuffix),
		cwdMarker:       s.cwdMark,
	}
}

func (s *Shell) snapshotTimeout() time.Duration {
	if s.traits.SnapshotTimeout > 0 {
		return s.traits.SnapshotTimeout
	}
	return defaultSnapshotTimeout
}

func (s *Shell) InitSession(ctx context.Context) {
	bootstrap := snapshotBootstrapScript(s.scriptPaths(s.Cwd()), s.excludedNames(ctx))
	timeout := s.snapshotTimeout()
	res, err := s.runDirect(ctx, bootstrap, RunOptions{Login: true, Timeout: timeout})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("snapshot bootstrap failed with exit code %d", res.ExitCode)
	}
	if err == nil {
		s.mu.Lock()
		s.snapshotReady = true
		s.mu.Unlock()
		s.updateCwd(&res)
		slog.Info(fmt.Sprintf("Session snapshot created (session=%s, cwd=%s)", s.id, s.Cwd()))
		return
	}
	prefer, detail := s.probeNonlogin(ctx, err.Error())
	s.mu.Lock()
	s.snapshotReady = false
	s.preferNonlogin = prefer
	s.mu.Unlock()
	if prefer {
		slog.Warn(fmt.Sprintf("init_session failed (session=%s): %s — login bash unusable; falling back to non-login bash -c", s.id, err))
		return
	}
	slog.Warn(fmt.Sprintf("init_session failed (session=%s): %s — falling back to bash -l per command", s.id, detail))
}

func (s *Shell) probeNonlogin(ctx context.Context, detail string) (bool, string) {
	timeout := min(nonloginProbeCap, s.snapshotTimeout())
	res, err := s.runDirect(ctx, "true", RunOptions{Timeout: timeout})
	if err != nil {
		return false, detail + "; non-login probe: " + err.Error()
	}
	return res.ExitCode == 0, detail
}

func (s *Shell) spawn(ctx context.Context, script string, o RunOptions) (tool.Process, error) {
	if keeper, ok := s.b.(CwdKeeper); ok {
		s.mu.Lock()
		s.cwd = keeper.RecoverCwd(s.cwd)
		s.mu.Unlock()
	}
	o.Cwd = s.Cwd()
	return s.b.RunBash(ctx, script, o)
}

func (s *Shell) runDirect(ctx context.Context, script string, o RunOptions) (tool.ExecResult, error) {
	p, err := s.spawn(ctx, script, o)
	if err != nil {
		return tool.ExecResult{}, err
	}
	res, handed := s.wait(ctx, p, waitOptions{timeout: o.Timeout, out: newCollector(unboundedCaptureChars, "", nil)})
	if !handed {
		release(p)
	}
	return res, nil
}

func release(p tool.Process) {
	if r, ok := p.(Releaser); ok {
		r.Release()
	}
}

func (s *Shell) sudoNopasswd(ctx context.Context) bool {
	if !s.traits.PasswordlessSudoProbe {
		return false
	}
	res, err := s.runDirect(ctx, "sudo -n true", RunOptions{Timeout: sudoProbeTimeout})
	return err == nil && res.ExitCode == 0
}

func (s *Shell) killProcess(p tool.Process) {
	_ = p.Kill()
}

func (s *Shell) forceKillProcess(p tool.Process) {
	if f, ok := p.(ForceKiller); ok {
		_ = f.ForceKill()
		return
	}
	s.killProcess(p)
}

func (s *Shell) killSpawnedTree(p tool.Process) {
	s.killProcess(p)
	pid := p.PID()
	if pid <= 0 {
		return
	}
	id, err := proc.Identify(pid)
	if err != nil {
		slog.Debug("terminal wait-bound kill_process_tree failed", "err", err)
		return
	}
	if err := proc.SignalTree(id, syscall.SIGKILL); err != nil {
		slog.Debug("terminal wait-bound kill_process_tree failed", "err", err)
	}
}

func (s *Shell) newCollector(ctx context.Context, bounded bool) *collector {
	if !bounded {
		return newCollector(unboundedCaptureChars, "", nil)
	}
	dir := home.TerminalOutput.In(s.o.Home)
	path := filepath.Join(dir, fmt.Sprintf("out-%d-%d-%x.log", time.Now().Unix(), os.Getpid(), randomUint16()))
	pruneSpills(dir, time.Now())
	return newCollector(s.maxOutput, path, func(d string) error {
		return s.o.Homes.MakeUserDir(ctx, d, 0o700, true)
	})
}

func randomUint16() uint16 {
	v, _ := strconv.ParseUint(randomHex(4), 16, 16)
	return uint16(v)
}

func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

func finalize(out *collector, rendered string, code int) tool.ExecResult {
	res := tool.ExecResult{Output: rendered, ExitCode: code}
	if spill := out.closeSpill(); spill != "" {
		res.OutputTotalChars = out.totalChars()
		res.SpillPath = spill
	}
	return res
}

type drainer struct {
	stop chan struct{}
	done chan struct{}
}

func startDrain(src <-chan []byte, out *collector, exited <-chan struct{}) *drainer {
	d := &drainer{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(d.done)
		dec := &utf8Decoder{}
		defer func() { out.add(dec.decode(nil, true)) }()
		idle := 0
		tick := time.NewTimer(drainIdleTick)
		defer tick.Stop()
		for {
			select {
			case <-d.stop:
				return
			default:
			}
			tick.Reset(drainIdleTick)
			select {
			case <-d.stop:
				return
			case chunk, ok := <-src:
				if !ok {
					return
				}
				out.add(dec.decode(chunk, false))
				idle = 0
			case <-tick.C:
				select {
				case <-exited:
					idle++
					if idle >= 3 {
						return
					}
				default:
				}
			}
		}
	}()
	return d
}

func (d *drainer) join(limit time.Duration) {
	select {
	case <-d.done:
	case <-time.After(limit):
	}
}

func (d *drainer) halt(limit time.Duration) {
	close(d.stop)
	d.join(limit)
}

type waitOptions struct {
	timeout  time.Duration
	out      *collector
	yield    *tool.Yield
	onYield  func(p tool.Process, partial string) (tool.ExecResult, bool)
	activity func(label string)
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (s *Shell) trace(event, format string, args ...any) {
	if s.debug {
		slog.Info(fmt.Sprintf("[interrupt-debug] wait %s "+format, append([]any{event}, args...)...))
	}
}

func (s *Shell) wait(ctx context.Context, p tool.Process, w waitOptions) (tool.ExecResult, bool) {
	out := w.out
	pp := newPumped(p)
	exited := make(chan struct{})
	waitCtx, stopWait := context.WithCancel(context.Background())
	defer stopWait()
	go func() {
		_, _ = p.Wait(waitCtx)
		close(exited)
	}()
	drain := startDrain(pp.ch, out, exited)
	start := time.Now()
	deadline := time.NewTimer(w.timeout)
	defer deadline.Stop()
	activity := time.NewTicker(activityInterval)
	defer activity.Stop()
	heartbeat := time.NewTicker(traceHeartbeat)
	defer heartbeat.Stop()
	_, interrupted := tool.InterruptReason(ctx)
	s.trace("ENTER", "pid=%d timeout=%ss activity_cb=%s initial_interrupt=%t", p.PID(), formatSeconds(w.timeout), callbackState(w.activity), interrupted)
	killAndJoin := func() {
		s.killProcess(p)
		drain.join(2 * time.Second)
		pp.abandon()
	}
	for {
		var yieldCh <-chan struct{}
		if w.onYield != nil {
			yieldCh = w.yield.Requested()
		}
		select {
		case <-exited:
			drain.join(2 * time.Second)
			pp.abandon()
			code, _ := p.Exited()
			s.trace("EXIT (natural)", "pid=%d elapsed=%.1fs returncode=%d", p.PID(), time.Since(start).Seconds(), code)
			return finalize(out, out.render(""), code), false
		case <-ctx.Done():
			if closed(exited) {
				continue
			}
			s.trace("INTERRUPT DETECTED", "pid=%d elapsed=%.1fs — killing process group", p.PID(), time.Since(start).Seconds())
			killAndJoin()
			res := finalize(out, out.render("\n[Command interrupted]"), 130)
			res.Interrupted = true
			return res, false
		case <-yieldCh:
			if closed(exited) || !w.yield.Consume() {
				continue
			}
			drain.halt(time.Second)
			if handed, ok := w.onYield(pp, out.render("")); ok {
				out.closeSpill()
				return handed, true
			}
			drain = startDrain(pp.ch, out, exited)
		case <-deadline.C:
			if closed(exited) {
				continue
			}
			s.trace("TIMEOUT", "pid=%d timeout=%ss", p.PID(), formatSeconds(w.timeout))
			killAndJoin()
			rendered := out.render("\n[Command timed out after " + formatSeconds(w.timeout) + "s]")
			if out.totalChars() == 0 {
				rendered = strings.TrimLeft(rendered, " \t\n\r\v\f")
			}
			res := finalize(out, rendered, 124)
			res.TimedOut = true
			return res, false
		case <-activity.C:
			if w.activity != nil {
				w.activity(fmt.Sprintf("%s (%ds elapsed)", activityLabel, int(time.Since(start).Seconds())))
			}
		case <-heartbeat.C:
			_, interrupted := tool.InterruptReason(ctx)
			s.trace("HEARTBEAT", "pid=%d elapsed=%.0fs interrupt=%t activity_cb=%s", p.PID(), time.Since(start).Seconds(), interrupted, callbackState(w.activity))
		}
	}
}

func callbackState(fn func(string)) string {
	if fn == nil {
		return "MISSING"
	}
	return "set"
}

func (s *Shell) wrap(ctx context.Context, command, cwd string) string {
	s.mu.Lock()
	ready := s.snapshotReady
	s.mu.Unlock()
	return wrapCommandScript(command, wrapOptions{
		paths:         s.scriptPaths(cwd),
		passthrough:   s.excludedNames(ctx),
		snapshotReady: ready,
		pipeMarker:    s.pipeMark,
		persistEnv:    s.traits.PersistEnv,
	})
}

func (s *Shell) updateCwd(res *tool.ExecResult) {
	if s.pipeMark != "" {
		if statuses, cleaned, ok := splitMarker(res.Output, s.pipeMark); ok {
			res.Output = cleaned
			var parsed []int
			valid := true
			for _, f := range strings.Fields(statuses) {
				n, err := strconv.Atoi(f)
				if err != nil {
					valid = false
					break
				}
				parsed = append(parsed, n)
			}
			if valid {
				res.PipelineStatus = parsed
			}
		}
	}
	cwd, cleaned, ok := splitMarker(res.Output, s.cwdMark)
	if !ok {
		return
	}
	res.Output = cleaned
	if cwd == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observed, accepted := cwd, true
	if keeper, ok := s.b.(CwdKeeper); ok {
		observed, accepted = keeper.ObservedCwd(s.cwd, cwd)
	}
	if !accepted {
		return
	}
	s.cwd = observed
	res.Cwd = observed
	res.CwdObserved = true
}

type holder struct {
	mu  sync.Mutex
	p   tool.Process
	out *collector
}

func (h *holder) set(p tool.Process, out *collector) {
	h.mu.Lock()
	h.p, h.out = p, out
	h.mu.Unlock()
}

func (h *holder) get() (tool.Process, *collector) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.p, h.out
}

type outcome struct {
	res tool.ExecResult
	err error
}

func (s *Shell) Execute(ctx context.Context, command string, r tool.ExecOptions) (tool.ExecResult, error) {
	if sy, ok := s.b.(Syncer); ok {
		if err := sy.Sync(ctx, false); err != nil {
			return tool.ExecResult{}, err
		}
	}
	execCommand, sudoStdin, err := s.o.Prepare.Prepare(ctx, command, s.sudoNopasswd)
	if err != nil {
		return tool.ExecResult{}, err
	}
	if r.RewriteCompoundBackground {
		execCommand = s.o.Prepare.RewriteCompoundBackground(execCommand)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = s.o.Timeout
	}
	cwd := r.Cwd
	if cwd == "" {
		cwd = s.Cwd()
	}
	stdin := r.Stdin
	if sudoStdin != nil {
		merged := *sudoStdin
		if r.Stdin != nil {
			merged += *r.Stdin
		}
		stdin = &merged
	}
	if stdin != nil && *stdin != "" && s.traits.StdinMode == StdinHeredoc {
		execCommand = embedStdinHeredoc(execCommand, *stdin)
		stdin = nil
	}
	wrapped := s.wrap(ctx, execCommand, cwd)
	s.mu.Lock()
	login := !s.snapshotReady && !s.preferNonlogin
	s.mu.Unlock()

	h := &holder{}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.spawnAndWait(ctx, wrapped, RunOptions{Login: login, Timeout: timeout, Stdin: stdin}, r, h)
		done <- outcome{res, err}
	}()
	bound := time.NewTimer(timeout + waitBoundGrace)
	defer bound.Stop()
	var res tool.ExecResult
	select {
	case o := <-done:
		if o.err != nil {
			return tool.ExecResult{}, o.err
		}
		res = o.res
	case <-bound.C:
		p, out := h.get()
		if p != nil {
			s.killSpawnedTree(p)
		}
		suffix := "\n[Command timed out after " + formatSeconds(timeout) + "s]"
		if out != nil {
			res = finalize(out, strings.TrimLeft(out.render(suffix), "\n"), 124)
		} else {
			res = tool.ExecResult{Output: strings.TrimLeft(suffix, " \t\n\r\v\f"), ExitCode: 124}
		}
		res.TimedOut = true
	}
	s.updateCwd(&res)
	if rc, ok := s.b.(Recreator); ok && rc.TakeRecreated() {
		res.Recreated = true
	}
	return res, nil
}

func (s *Shell) spawnAndWait(ctx context.Context, wrapped string, o RunOptions, r tool.ExecOptions, h *holder) (tool.ExecResult, error) {
	if !s.o.Foreground.enter() {
		return tool.ExecResult{Output: "[host is exiting: command not started]", ExitCode: 130}, nil
	}
	p, err := s.spawn(ctx, wrapped, o)
	if err != nil {
		s.o.Foreground.leave(s, nil)
		return tool.ExecResult{}, err
	}
	entry, fenced := s.o.Foreground.leave(s, p)
	defer s.o.Foreground.remove(entry)
	if fenced {
		s.forceKillProcess(p)
	}
	out := s.newCollector(ctx, r.BoundedCapture)
	h.set(p, out)
	res, handed := s.wait(ctx, p, waitOptions{timeout: o.Timeout, out: out, yield: r.Yield, onYield: r.OnYield, activity: r.Activity})
	if !handed {
		release(p)
	}
	return res, nil
}

func (s *Shell) FetchFile(ctx context.Context, remote, local string, maxBytes int64) error {
	marker := "__ICEBATH_FETCH_" + randomHex(12) + "__"
	quoted := shell.Quote(remote)
	command := fmt.Sprintf("[ -f %s ] && echo %s && head -c %d < %s | base64 && echo %s", quoted, marker, maxBytes+1, quoted, marker)
	res, err := s.Execute(ctx, command, tool.ExecOptions{Timeout: fetchTimeout})
	if err != nil {
		return err
	}
	first, last := strings.Index(res.Output, marker), strings.LastIndex(res.Output, marker)
	if res.ExitCode != 0 || first == -1 || last <= first {
		return &FetchError{Msg: fmt.Sprintf("could not read %s in the sandbox (missing, not a regular file, or unreadable)", quotedPath(remote))}
	}
	payload := strings.Join(strings.Fields(res.Output[first+len(marker):last]), "")
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return &FetchError{Msg: fmt.Sprintf("transfer of %s was corrupted in transit: %v", quotedPath(remote), err), Err: err}
	}
	if int64(len(data)) > maxBytes {
		return &FetchError{Msg: fmt.Sprintf("%s exceeds the %d MB delivery limit", quotedPath(remote), maxBytes/(1024*1024))}
	}
	return os.WriteFile(local, data, 0o666)
}

func (s *Shell) Realpath(ctx context.Context, path string) (string, bool, error) {
	res, err := s.Execute(ctx, "readlink -f "+shell.Quote(path)+" 2>/dev/null", tool.ExecOptions{})
	if err != nil {
		return "", false, err
	}
	if res.ExitCode != 0 {
		return "", false, nil
	}
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(res.Output, "\r\n", "\n"), "\r", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); strings.HasPrefix(ln, "/") {
			return ln, true, nil
		}
	}
	return "", false, nil
}

type UnsupportedError struct {
	Kind string
	Op   string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("the %s backend does not support %s", e.Kind, e.Op)
}

func (s *Shell) SyncBack(ctx context.Context) error {
	if sy, ok := s.b.(Syncer); ok {
		return sy.SyncBack(ctx)
	}
	return nil
}

func (s *Shell) Sync(ctx context.Context, force bool) error {
	if sy, ok := s.b.(Syncer); ok {
		return sy.Sync(ctx, force)
	}
	return nil
}

func (s *Shell) Close(ctx context.Context, remove bool) error {
	s.closeForwards()
	err := s.b.Close(ctx, remove)
	if !s.traits.Local {
		return err
	}
	stale, _ := filepath.Glob(s.snapPath + ".tmp.*")
	for _, f := range append([]string{s.snapPath, s.cwdFile}, stale...) {
		if rmErr := os.Remove(f); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			slog.Debug("terminal snapshot cleanup failed", "path", f, "err", rmErr)
		}
	}
	return err
}

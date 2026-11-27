package terminal

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Wait past a command's own deadline before the backstop kills it
const backstopGrace = 2 * time.Second

// Longest a sudo -n probe may take inside a backend
const sudoProbeTimeout = 3 * time.Second

// Longest a file copy out of an environment may take
const fetchTimeout = 5 * time.Minute

// What a backend does itself under a Shell: start bash and stop what it started
type Runner interface {
	// Starts bash running the script, as a login shell when login is set, reading stdin when it is not nil
	Run(ctx context.Context, script string, login bool, timeout time.Duration, stdin *string) (Proc, error)
	// Stops the process and everything it started
	Kill(p Proc)
	// Stops it at once with no grace, icebath exiting next
	ForceKill(p Proc)
	// Removes what the environment made, force reaching sandboxes kept otherwise
	Cleanup(force bool) error
}

// Runner with a temp folder of its own on the sandbox side, /tmp otherwise
type TempDirer interface {
	TempDir() string
}

// Runner whose paths need quoting of their own, such as Windows paths in Git Bash
type Quoter interface {
	QuoteCwd(cwd string) string
	QuotePath(path string) string
}

// Runner that has work to do before each command, such as syncing files into its sandbox
type Preparer interface {
	Prepare(ctx context.Context) error
}

// Runner that checks a folder a command reported ending in, answering the folder to keep or false to keep the previous one
type Observer interface {
	Observe(previous, cwd string) (string, bool)
}

// Runner that has work to do once a command's process ended
type Finisher interface {
	Finish(p Proc)
}

// How a Shell is made
type ShellOptions struct {
	Backend string
	Cwd     string
	Timeout time.Duration
	// Exported variables carry from one command to the next
	Persist bool
	// Stdin text is put into the script as a heredoc, for backends that can't hand a process stdin
	Heredoc bool
	// sudo -n true is tried in the environment before a password is asked for
	SudoProbe bool
	// Facts the Environment answers
	Persistent    bool
	SessionScoped bool
	HostFiles     bool
	HostCwd       string
	Mount         string
	// Longest the login shell snapshot may take, 30 seconds when zero
	SnapshotTimeout time.Duration
	// Names of the variables the backend hands each command itself, kept out of the snapshot and over its sourcing so the value a command
	// gets is always the one handed to it; nil for none
	Forwarded func() []string
}

// The execution every bash based backend shares: a snapshot of the login shell's exports, functions and aliases sourced by each command,
// the folder and exports carried from one command to the next, the cwd and pipeline markers, interrupts, timeouts and output capture
type Shell struct {
	runner   Runner
	terminal *Terminal
	opts     ShellOptions

	mu       sync.Mutex
	cwd      string
	ready    bool
	nonlogin bool
	// The sandbox was made again while serving the command in flight
	recreated bool
	home      string
	// Every name the backend ever handed a command itself, kept out of the snapshot for the life of the environment
	forwarded map[string]bool

	id         string
	snapshot   string
	cwdFile    string
	cwdMarker  string
	pipeMarker string
}

// Shell over a backend's runner, its snapshot taken by Init
func NewShell(spec *Spec, runner Runner, opts ShellOptions) *Shell {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.SnapshotTimeout <= 0 {
		opts.SnapshotTimeout = 30 * time.Second
	}
	s := &Shell{runner: runner, terminal: spec.Terminal, opts: opts, cwd: opts.Cwd, id: strs.Random(6)}
	temp := strings.TrimRight(s.TempDir(), "/")
	s.snapshot = temp + "/icebath-snap-" + s.id + ".sh"
	s.cwdFile = temp + "/icebath-cwd-" + s.id + ".txt"
	s.cwdMarker = "__ICEBATH_CWD_" + s.id + "__"
	s.pipeMarker = "__ICEBATH_PIPE_" + s.id + "__"
	return s
}

func (s *Shell) Backend() string { return s.opts.Backend }

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

func (s *Shell) TempDir() string {
	if t, ok := s.runner.(TempDirer); ok {
		return t.TempDir()
	}
	return "/tmp"
}

func (s *Shell) Mount() (string, string) { return s.opts.HostCwd, s.opts.Mount }
func (s *Shell) Persistent() bool        { return s.opts.Persistent }
func (s *Shell) SessionScoped() bool     { return s.opts.SessionScoped }
func (s *Shell) HostFiles() bool         { return s.opts.HostFiles }

// Paths of the session snapshot and the cwd file in the sandbox
func (s *Shell) Snapshot() (snapshot, cwdFile string) { return s.snapshot, s.cwdFile }

func (s *Shell) SandboxHome() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.home
}

// Records the home the backend declared for the sandbox
func (s *Shell) StampHome(home string) {
	s.mu.Lock()
	s.home = home
	s.mu.Unlock()
}

// Notes that the sandbox was made again while serving the command in flight, which its result then says
func (s *Shell) MarkRecreated() {
	s.mu.Lock()
	s.recreated = true
	s.mu.Unlock()
}

func (s *Shell) Cleanup(force bool) error { return s.runner.Cleanup(force) }

// Kills a running command and what it started
func (s *Shell) Kill(p Proc) { s.runner.Kill(p) }

func (s *Shell) quoteCwd(cwd string) string {
	if q, ok := s.runner.(Quoter); ok {
		return q.QuoteCwd(cwd)
	}
	return QuoteCwd(cwd)
}

func (s *Shell) quotePath(path string) string {
	if q, ok := s.runner.(Quoter); ok {
		return q.QuotePath(path)
	}
	return Quote(path)
}

// A cd target quoted, a leading tilde left for the shell to expand
func QuoteCwd(cwd string) string {
	switch {
	case cwd == "~":
		return cwd
	case cwd == "~/":
		return "$HOME"
	case strings.HasPrefix(cwd, "~/"):
		return "$HOME/" + Quote(cwd[2:])
	}
	return Quote(cwd)
}

type scriptPaths struct {
	cwd, snapshot, template, marker string
}

func (s *Shell) paths(cwd string) scriptPaths {
	return scriptPaths{cwd: s.quoteCwd(cwd), snapshot: s.quotePath(s.snapshot), template: s.quotePath(s.snapshot + ".tmp.XXXXXXXXXX"), marker: s.cwdMarker}
}

// Shell expansion of the snapshot temp file made by mktemp
const snapTmp = `"$__icebath_snap_tmp"`

// export -p written to a path without icebath's per session variables and the variables the backend hands each command itself
func exportDump(path string, forwarded []string) string {
	extra := ""
	if len(forwarded) > 0 {
		extra = " " + Join(forwarded)
	}
	return "{ ( unset ${!ICEBATH_SESSION_*} ${!ICEBATH_CRON_AUTO_DELIVER_*} AI_AGENT ICEBATH_AGENT ICEBATH_DELEGATED_CHILD_CONTEXT " +
		"ICEBATH_CRON_SESSION ${!ICEBATH_RPC_*} ICEBATH_KERNEL_DIR ICEBATH_UI_SESSION_ID" + extra + " 2>/dev/null; export -p; ) || true; } > " + path
}

// Names the backend handed commands itself so far, those it hands now added, sorted
func (s *Shell) scoped() []string {
	var names []string
	if s.opts.Forwarded != nil {
		names = s.opts.Forwarded()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		if envName.MatchString(name) {
			if s.forwarded == nil {
				s.forwarded = map[string]bool{}
			}
			s.forwarded[name] = true
		}
	}
	out := make([]string, 0, len(s.forwarded))
	for name := range s.forwarded {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Lines keeping the values handed to a command over the sourcing of the snapshot: saved before it, put back or unset after
func keepForwarded(names []string) (save, restore []string) {
	for _, name := range names {
		present, value := "_ICEBATH_RUNTIME_PASSTHROUGH_"+name+"_PRESENT", "_ICEBATH_RUNTIME_PASSTHROUGH_"+name+"_VALUE"
		save = append(save, present+"=${"+name+"+x}", value+"=${"+name+"-}")
		restore = append(restore, `if [ "$`+present+`" = x ]; then export `+name+`="$`+value+`"; else unset `+name+`; fi`, "unset "+present+" "+value)
	}
	return save, restore
}

func cwdPrintf(marker string) string {
	return fmt.Sprintf(`printf '\n%s%%s%s\n' "$(pwd -P)"`, marker, marker)
}

// Login shell script capturing exports, functions and aliases into the snapshot through mktemp and mv
func bootstrapScript(p scriptPaths, forwarded []string) string {
	return "umask 077\n" +
		"__icebath_snap_tmp=$(mktemp " + p.template + ") || exit 1\n" +
		exportDump(snapTmp, forwarded) + "\n" +
		"__icebath_fns=$(declare -F | awk '{print $3}' | grep -vE '^_[^_]') || true\n" +
		"[ -n \"$__icebath_fns\" ] && declare -f $__icebath_fns >> " + snapTmp + " 2>/dev/null || true\n" +
		"alias -p >> " + snapTmp + "\n" +
		"echo 'shopt -s expand_aliases' >> " + snapTmp + "\n" +
		"echo 'set +e' >> " + snapTmp + "\n" +
		"echo 'set +u' >> " + snapTmp + "\n" +
		"mv -f " + snapTmp + " " + p.snapshot + " || rm -f " + snapTmp + "\n" +
		"builtin cd -- " + p.cwd + " 2>/dev/null || true\n" +
		cwdPrintf(p.marker) + "\n"
}

// Takes the snapshot of the login shell, falling back to a login shell per command, or to plain bash when a login shell is unusable
func (s *Shell) Init(ctx context.Context) {
	err := func() error {
		p, err := s.runner.Run(ctx, bootstrapScript(s.paths(s.Cwd()), s.scoped()), true, s.opts.SnapshotTimeout, nil)
		if err != nil {
			return err
		}
		result := s.wait(ctx, p, wait{timeout: s.opts.SnapshotTimeout})
		if result.Code != 0 {
			return fmt.Errorf("snapshot bootstrap failed with exit code %d", result.Code)
		}
		s.mu.Lock()
		s.ready = true
		s.mu.Unlock()
		s.observe(result)
		return nil
	}()
	if err == nil {
		slog.Info(fmt.Sprintf("Session snapshot created (session=%s, cwd=%s)", s.id, s.Cwd()))
		return
	}
	nonlogin, detail := s.probeNonlogin(ctx, err.Error())
	s.mu.Lock()
	s.ready, s.nonlogin = false, nonlogin
	s.mu.Unlock()
	if nonlogin {
		slog.Warn(fmt.Sprintf("init_session failed (session=%s): %s — login bash unusable; falling back to non-login bash -c", s.id, err))
	} else {
		slog.Warn(fmt.Sprintf("init_session failed (session=%s): %s — falling back to bash -l per command", s.id, detail))
	}
}

// Runs true in plain bash and reports whether that is the only usable way
func (s *Shell) probeNonlogin(ctx context.Context, detail string) (bool, string) {
	timeout := min(s.opts.SnapshotTimeout, 15*time.Second)
	p, err := s.runner.Run(ctx, "true", false, timeout, nil)
	if err != nil {
		return false, detail + "; non-login probe: " + err.Error()
	}
	result := s.wait(ctx, p, wait{timeout: timeout})
	if result.Code != 0 {
		if text := strings.TrimSpace(result.Output); text != "" {
			detail = text
		}
	}
	return result.Code == 0, detail
}

// Lines setting the variables of one command, those given no value unset, in the order of their names
func commandVars(vars map[string]string) []string {
	var set, unset []string
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		switch {
		case !envName.MatchString(name):
		case vars[name] == "":
			unset = append(unset, name)
		default:
			set = append(set, name+"="+Quote(vars[name]))
		}
	}
	var lines []string
	if len(unset) > 0 {
		lines = append(lines, "unset "+strings.Join(unset, " ")+" 2>/dev/null || true")
	}
	if len(set) > 0 {
		lines = append(lines, "export "+strings.Join(set, " "))
	}
	return lines
}

// Script that sources the snapshot, sets the command's own variables, enters the folder, runs the command, dumps the exports again and
// prints the markers
func (s *Shell) wrap(command, cwd string, vars map[string]string) string {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	p := s.paths(cwd)
	escaped := strings.ReplaceAll(command, "'", `'\''`)
	capture := !strings.Contains(command, "<<") && !strings.HasSuffix(command, "\\")
	forwarded := s.scoped()
	save, restore := keepForwarded(forwarded)
	parts := save
	if ready {
		parts = append(parts, "source "+p.snapshot+" >/dev/null 2>&1 || true")
	}
	parts = append(parts, restore...)
	parts = append(parts, commandVars(vars)...)
	parts = append(parts,
		`export AI_AGENT="${AI_AGENT:-icebath}" ICEBATH_AGENT="${ICEBATH_AGENT:-true}"`,
		`export GIT_PAGER="${GIT_PAGER:-cat}" PAGER="${PAGER:-cat}"`,
		"builtin cd -- "+p.cwd+" || exit 126")
	if capture {
		parts = append(parts, "eval '"+escaped+"\n\n__icebath_rc=$? __icebath_ps=(\"${PIPESTATUS[@]}\"); (exit $__icebath_rc) && :'")
	} else {
		parts = append(parts, "eval '"+escaped+"'")
	}
	parts = append(parts, "__icebath_ec=$?", "umask 077")
	if ready && s.opts.Persist {
		parts = append(parts, "__icebath_snap_tmp=$(mktemp "+p.template+") && { "+exportDump(snapTmp, forwarded)+" && mv -f "+snapTmp+" "+p.snapshot+"; } "+
			"2>/dev/null || rm -f "+snapTmp+" 2>/dev/null || true")
	}
	if capture {
		parts = append(parts, fmt.Sprintf(`printf '\n%s%%s%s\n' "${__icebath_ps[*]-}"`, s.pipeMarker, s.pipeMarker))
	}
	return strings.Join(append(parts, cwdPrintf(p.marker), "exit $__icebath_ec"), "\n")
}

// Payload of the last complete marker pair and the output without its line
func cutMarker(output, marker string) (string, string, bool) {
	last := strings.LastIndex(output, marker)
	if last == -1 {
		return "", "", false
	}
	start := max(last-4096, 0)
	first := strings.LastIndex(output[start:last], marker)
	if first == -1 {
		return "", "", false
	}
	first += start
	payload := strings.TrimSpace(output[first+len(marker) : last])
	lineStart := strings.LastIndex(output[:first], "\n")
	if lineStart == -1 {
		lineStart = first
	}
	lineEnd := len(output)
	if i := strings.Index(output[last+len(marker):], "\n"); i >= 0 {
		lineEnd = last + len(marker) + i + 1
	}
	return payload, output[:lineStart] + output[lineEnd:], true
}

// Reads the pipeline and cwd markers out of the output, moving to the folder the command ended in
func (s *Shell) observe(result *Result) {
	if statuses, cleaned, ok := cutMarker(result.Output, s.pipeMarker); ok {
		result.Output = cleaned
		var codes []int
		valid := true
		for _, field := range strings.Fields(statuses) {
			code, err := strconv.Atoi(field)
			if err != nil {
				valid = false
				break
			}
			codes = append(codes, code)
		}
		if valid {
			result.Pipeline = codes
		}
	}
	cwd, cleaned, ok := cutMarker(result.Output, s.cwdMarker)
	if !ok {
		return
	}
	result.Output = cleaned
	if cwd == "" {
		return
	}
	previous := s.Cwd()
	if o, ok := s.runner.(Observer); ok {
		kept, ok := o.Observe(previous, cwd)
		if !ok {
			return
		}
		cwd = kept
	}
	s.SetCwd(cwd)
	result.Cwd = cwd
}

// Command wrapped so stdin is the text, the heredoc's own newline dropped
func embedStdin(command, stdin string) string {
	delimiter := "ICEBATH_STDIN_" + strs.Random(6)
	return "{\n" + command + "\n} << '" + delimiter + "' < <(IFS= read -r -d '' s || :; printf '%s' \"${s%?}\")\n" + stdin + "\n" + delimiter
}

// Inputs of one wait on a started process
type wait struct {
	timeout time.Duration
	output  *collector
	yield   func() bool
	handoff func(Proc, string) *Result
}

// Waits for the process while reading its output, stopping it on an interrupt or a timeout, handing it off when the user asks; the
// runner finishes with a process that ended here, not with one handed off
func (s *Shell) wait(ctx context.Context, p Proc, w wait) *Result {
	result := s.await(ctx, p, w)
	if f, ok := s.runner.(Finisher); ok && result.Yielded == "" {
		f.Finish(p)
	}
	return result
}

func (s *Shell) await(ctx context.Context, p Proc, w wait) *Result {
	if w.timeout <= 0 {
		w.timeout = 120 * time.Second
	}
	output := w.output
	if output == nil {
		output = newCollector(0, "")
	}
	var stop *atomic.Bool
	if w.handoff != nil {
		stop = new(atomic.Bool)
	}
	d := startDrain(p, output, stop)
	deadline := time.Now().Add(w.timeout)
	pause := 5 * time.Millisecond
	for {
		if code, exited := p.Poll(); exited {
			d.join(2 * time.Second)
			if out := p.Output(); out != nil {
				out.Close()
			}
			stdinErr := ""
			if h, ok := p.(*HostProc); ok {
				stdinErr = h.joinStdin(5 * time.Second)
			}
			rendered := output.render("")
			result := output.result(rendered, code)
			if stdinErr != "" {
				result.StdinError = stdinErr
				result.Output = rendered + "\n[stdin write failed: " + stdinErr + "]"
			}
			return result
		}
		if ctx.Err() != nil {
			s.runner.Kill(p)
			d.join(2 * time.Second)
			result := output.result(output.render("\n[Command interrupted]"), 130)
			result.Interrupted = true
			return result
		}
		if w.handoff != nil && w.yield != nil && w.yield() {
			stop.Store(true)
			d.join(time.Second)
			if handed := handoff(w.handoff, p, output.render("")); handed != nil {
				output.closeSpill()
				return handed
			}
			stop.Store(false)
			d = startDrain(p, output, stop)
		}
		if time.Now().After(deadline) {
			s.runner.Kill(p)
			d.join(2 * time.Second)
			rendered := output.render(fmt.Sprintf("\n[Command timed out after %ds]", int(w.timeout.Seconds())))
			if output.count() == 0 {
				rendered = strings.TrimLeft(rendered, " \t\n\r")
			}
			result := output.result(rendered, 124)
			result.TimedOut = true
			return result
		}
		time.Sleep(pause)
		pause = min(time.Duration(float64(pause)*1.5), 200*time.Millisecond)
	}
}

// Runs a handoff, a panic logged and the wait kept up
func handoff(h func(Proc, string) *Result, p Proc, soFar string) (handed *Result) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("yield-to-background handoff failed; continuing to wait", "panic", r)
			handed = nil
		}
	}()
	return h(p, soFar)
}

// Runs a command through the snapshot wrapper with the backstop deadline, following the folder it ends in
func (s *Shell) Execute(ctx context.Context, command string, opts Exec) (*Result, error) {
	if p, ok := s.runner.(Preparer); ok {
		if err := p.Prepare(ctx); err != nil {
			return nil, err
		}
	}
	if !opts.Raw {
		command = RewriteCompoundBackground(command)
	}
	timeout := cmpDuration(opts.Timeout, s.opts.Timeout)
	cwd := opts.Cwd
	if cwd == "" {
		cwd = s.Cwd()
	}
	stdin := opts.Stdin
	if stdin != nil && *stdin != "" && s.opts.Heredoc {
		command, stdin = embedStdin(command, *stdin), nil
	}
	script := s.wrap(command, cwd, opts.Env)
	s.mu.Lock()
	login := !s.ready && !s.nonlogin
	s.mu.Unlock()

	var mu sync.Mutex
	var spawned Proc
	var output *collector
	run := func() (*Result, error) {
		if !s.terminal.enter() {
			return &Result{Output: "[host is exiting: command not started]", Code: 130}, nil
		}
		p, err := s.runner.Run(ctx, script, login, timeout, stdin)
		fenced := s.terminal.leave(s.runner, p)
		if err != nil {
			return nil, err
		}
		defer s.terminal.forget(p)
		collected := newCollector(opts.Limit, opts.Spill)
		mu.Lock()
		spawned, output = p, collected
		mu.Unlock()
		if fenced {
			s.runner.ForceKill(p)
		}
		return s.wait(ctx, p, wait{timeout: timeout, output: collected, yield: opts.Yield, handoff: opts.Handoff}), nil
	}
	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := run()
		done <- outcome{result, err}
	}()
	var result *Result
	select {
	case out := <-done:
		if out.err != nil {
			return nil, out.err
		}
		result = out.result
	case <-time.After(timeout + backstopGrace):
		mu.Lock()
		p, collected := spawned, output
		mu.Unlock()
		if p != nil {
			s.killWedged(p)
		}
		suffix := fmt.Sprintf("\n[Command timed out after %ds]", int(timeout.Seconds()))
		if collected != nil {
			result = collected.result(strings.TrimLeft(collected.render(suffix), "\n"), 124)
		} else {
			result = &Result{Output: strings.TrimLeft(suffix, " \t\n\r"), Code: 124}
		}
		result.TimedOut = true
	}
	s.observe(result)
	s.mu.Lock()
	if s.recreated {
		s.recreated, result.Recreated = false, true
	}
	s.mu.Unlock()
	if result.Pid == 0 {
		mu.Lock()
		if spawned != nil {
			result.Pid = spawned.Pid()
		}
		mu.Unlock()
	}
	return result, nil
}

func cmpDuration(a, b time.Duration) time.Duration {
	if a > 0 {
		return a
	}
	return b
}

// Kills a process whose wait wedged, and its tree when it is a process of this machine
func (s *Shell) killWedged(p Proc) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug("terminal wait-bound kill_process failed", "panic", r)
			}
		}()
		s.runner.Kill(p)
	}()
	if h, ok := p.(*HostProc); ok && h.Pid() > 0 {
		h.KillTree()
	}
}

// Whether sudo runs without a password in the environment, probed with sudo -n and failing closed
func (s *Shell) SudoWithoutPassword(ctx context.Context) bool {
	if !s.opts.SudoProbe {
		return false
	}
	p, err := s.runner.Run(ctx, "sudo -n true", false, sudoProbeTimeout, nil)
	if err != nil {
		return false
	}
	return s.wait(ctx, p, wait{timeout: sudoProbeTimeout}).Code == 0
}

// Copies a regular file out of the environment over base64, read at most max bytes inside the sandbox
func (s *Shell) FetchFile(ctx context.Context, remote, local string, max int) error {
	marker := "__ICEBATH_FETCH_" + strs.Random(6) + "__"
	quoted := Quote(remote)
	command := fmt.Sprintf("[ -f %s ] && echo %s && head -c %d < %s | base64 && echo %s", quoted, marker, max+1, quoted, marker)
	result, err := s.Execute(ctx, command, Exec{Timeout: fetchTimeout, Raw: true})
	if err != nil {
		return err
	}
	first, last := strings.Index(result.Output, marker), strings.LastIndex(result.Output, marker)
	if result.Code != 0 || first == -1 || last <= first {
		return &FetchError{Reason: fmt.Sprintf("could not read %q in the sandbox (missing, not a regular file, or unreadable)", remote)}
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(result.Output[first+len(marker):last]), ""))
	if err != nil {
		return &FetchError{Reason: fmt.Sprintf("transfer of %q was corrupted in transit: %v", remote, err)}
	}
	if len(data) > max {
		return &FetchError{Reason: fmt.Sprintf("%q exceeds the %d MB delivery limit", remote, max/(1024*1024))}
	}
	return os.WriteFile(local, data, 0o600)
}

// Path readlink -f resolves remote to inside the environment, empty when it can't
func (s *Shell) FetchRealpath(ctx context.Context, remote string) string {
	result, err := s.Execute(ctx, "readlink -f "+Quote(remote)+" 2>/dev/null", Exec{Raw: true})
	if err != nil || result.Code != 0 {
		return ""
	}
	lines := strings.Split(result.Output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "/") {
			return line
		}
	}
	return ""
}

// Variables as sorted NAME=value entries
func Environ(vars map[string]string) []string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name + "=" + vars[name]
	}
	return out
}

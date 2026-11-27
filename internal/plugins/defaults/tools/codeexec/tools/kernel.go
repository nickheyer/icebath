package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	codeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Staging folders of kernels, and the age past which the reaper removes one a crash left
	stagingPrefix = "icebath_kernel_"
	stagingAge    = 7 * 24 * time.Hour
	// Longest temp folder a socket is made in, past which /tmp is used, so the socket's path stays within what the system allows
	socketDirMax = 50
)

// What a kernel serves: the session that owns it, the mode, interpreter and folder it runs in, and the tools its module calls
type key struct {
	owner, mode, python, cwd, tools string
}

// One cell's tool authority: the call that runs the cell, which every tool call of the cell goes out under, refused once the cell settled
type authority struct {
	ctx    context.Context
	call   *tools.Call
	active atomic.Bool
	calls  atomic.Int64
}

func (a *authority) dispatch(r *registrant, name string, args json.RawMessage) string {
	if !a.active.Load() {
		return tools.ErrorResult(errors.New("No active execute_code cell: the cell this kernel call belonged to has settled, so its tool authority is " +
			"retired."))
	}
	return r.invoke(a.ctx, a.call, name, args, a.calls.Add(1))
}

// Runs a tool call a script made the way the agent runs one, its whole result handed back
func (r *registrant) invoke(ctx context.Context, parent *tools.Call, name string, args json.RawMessage, n int64) string {
	call := &tools.Call{Name: name, RawArgs: string(args), ToolCallID: fmt.Sprintf("%s_exec_%d", cmp.Or(parent.ToolCallID, "execute_code"), n),
		SessionID: parent.SessionID, Env: parent.Env}
	return r.registry.Invoke(ctx, call)
}

// Reply of a kernel to one cell, or what the host made of its end
type reply struct {
	Status         string `json:"status"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	StdoutClipped  bool   `json:"stdout_clipped"`
	Traceback      string `json:"traceback"`
	ExecutionCount int    `json:"execution_count"`
	Spill          string `json:"stdout_spill_path"`
}

// Bytes kept up to a cap, drained as text
type bounded struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *bounded) write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p[:min(max(0, b.limit-len(b.data)), len(p))]...)
}

func (b *bounded) drain() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.ToValidUTF8(string(b.data), "�")
	b.data = nil
	return text
}

// A Python process of this machine keeping one session's state across calls, served tool calls over its socket
type localKernel struct {
	key key
	// One cell at a time
	run sync.Mutex

	mu       sync.Mutex
	dir      string
	socket   string
	listener net.Listener
	stdin    *os.File

	proc     atomic.Pointer[terminal.HostProc]
	token    string
	sentinel string
	stop     chan struct{}
	stopped  sync.Once
	budget   *budget
	// Index of the budget's log the running cell's calls start at
	cellStart int
	count     int

	// Under the registrant's lock
	attached int
	pinned   bool
	used     time.Time
	idle     time.Duration

	replies   chan reply
	raw       bounded
	stderr    bounded
	authority atomic.Pointer[authority]
}

func newKernel(k key) *localKernel {
	return &localKernel{key: k, stop: make(chan struct{}), budget: newBudget(nil, 0), replies: make(chan reply, 16), used: time.Now(),
		raw: bounded{limit: maxStdout}, stderr: bounded{limit: maxStderr}}
}

// Whether a kernel that was started has ended; one still starting has not
func (k *localKernel) dead() bool {
	p := k.proc.Load()
	if p == nil {
		return false
	}
	_, exited := p.Poll()
	return exited
}

// Kills the kernel's process group, closes its socket and removes its staging folder
func (k *localKernel) teardown() {
	k.stopped.Do(func() { close(k.stop) })
	if p := k.proc.Load(); p != nil {
		if _, exited := p.Poll(); !exited {
			p.KillTree()
		}
		p.Release()
	}
	k.mu.Lock()
	ln, socket, dir, stdin := k.listener, k.socket, k.dir, k.stdin
	k.listener, k.stdin = nil, nil
	k.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	if socket != "" {
		os.Remove(socket)
	}
	if stdin != nil {
		stdin.Close()
	}
	if dir != "" {
		os.RemoveAll(dir)
	}
}

// Random id of 32 hex digits
func hexID() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// Folder a socket is made in, short enough for the length a socket path may have
func socketDir() string {
	if runtime.GOOS == "darwin" {
		return "/tmp"
	}
	if dir := os.TempDir(); len([]rune(dir)) <= socketDirMax || !fsx.IsDir("/tmp") {
		return dir
	}
	return "/tmp"
}

// Listens for the module's calls: on loopback TCP on Windows, else on a socket only its owner may open
func (k *localKernel) bind() (string, error) {
	if windows {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		k.mu.Lock()
		k.listener = ln
		k.mu.Unlock()
		addr := ln.Addr().(*net.TCPAddr)
		return fmt.Sprintf("tcp://%s:%d", addr.IP, addr.Port), nil
	}
	path := filepath.Join(socketDir(), "icebath_rpc_"+hexID()+".sock")
	k.mu.Lock()
	k.socket = path
	k.mu.Unlock()
	ln, err := net.Listen("unix", path)
	if err != nil {
		return "", err
	}
	k.mu.Lock()
	k.listener = ln
	k.mu.Unlock()
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Stages the runner and the tools module, opens the socket and starts the kernel as the leader of an owned group
func (r *registrant) spawn(call *tools.Call, k *localKernel, python, cwd string, set []string, maxCalls int) error {
	dir, err := os.MkdirTemp(os.TempDir(), stagingPrefix)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.dir = dir
	k.mu.Unlock()
	k.token, k.sentinel = strs.Token(32), "@@ICEBATH-KERNEL-"+strs.Token(16)+"@@"
	endpoint, err := k.bind()
	if err != nil {
		return err
	}
	for name, source := range map[string]string{"icebath_tools.py": module(set, false), "icebath_kernel_runner.py": kernelSource} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
			return err
		}
	}
	env := r.childEnv(call, endpoint, k.token, dir)
	env["ICEBATH_KERNEL_SENTINEL"], env["ICEBATH_KERNEL_SPILL_DIR"], env["ICEBATH_RPC_PERSISTENT"] = k.sentinel, dir, "1"
	cmd := exec.Command(python, filepath.Join(dir, "icebath_kernel_runner.py"))
	cmd.Dir = cmp.Or(cwd, dir)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		fsx.CloseAll(stdinR, stdinW)
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		fsx.CloseAll(stdinR, stdinW, stdoutR, stdoutW)
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	terminal.NewSession(cmd)
	proc.HideWindow(cmd)
	release := parentHandle(env, cmd)
	cmd.Env = terminal.Environ(env)
	err = cmd.Start()
	release()
	fsx.CloseAll(stdinR, stdoutW, stderrW)
	if err != nil {
		fsx.CloseAll(stdinW, stdoutR, stderrR)
		return err
	}
	p := terminal.Watch(cmd)
	p.Own(r.host.Supervisor)
	k.budget = newBudget(set, maxCalls)
	k.mu.Lock()
	k.stdin = stdinW
	ln := k.listener
	k.mu.Unlock()
	k.proc.Store(p)
	go k.serve(r, ln)
	go k.readStdout(stdoutR)
	go k.readStderr(stderrR)
	r.startReaper()
	return nil
}

// Serves the module's calls for the kernel's life, each going out under the cell that runs
func (k *localKernel) serve(r *registrant, ln net.Listener) {
	run := func(_ context.Context, name string, args json.RawMessage) string {
		a := k.authority.Load()
		if a == nil {
			return tools.ErrorResult(errors.New("No active execute_code cell: this kernel has no cell authority installed."))
		}
		return a.dispatch(r, name, args)
	}
	for !stopped(k.stop) {
		if !serveOne(context.Background(), ln, k.token, k.budget, k.stop, run) {
			return
		}
	}
}

// Whatever bytes arrive, up to n, nil at the end of the stream
func readSome(f *os.File, n int) []byte {
	p := make([]byte, n)
	for {
		m, err := f.Read(p)
		if m > 0 {
			return p[:m]
		}
		if err != nil {
			return nil
		}
	}
}

// Splits the kernel's stdout into the replies the sentinel frames and the bytes written past them, which count as the running cell's
func (k *localKernel) readStdout(f *os.File) {
	defer f.Close()
	marker := []byte("\n" + k.sentinel + " ")
	var buf []byte
	for {
		chunk := readSome(f, 4096)
		if chunk == nil {
			k.raw.write(buf)
			k.replies <- reply{Status: "kernel-eof"}
			return
		}
		buf = append(buf, chunk...)
		for {
			index := bytes.Index(buf, marker)
			if index < 0 {
				if len(buf) > len(marker) {
					k.raw.write(buf[:len(buf)-len(marker)])
					buf = slices.Clone(buf[len(buf)-len(marker):])
				}
				break
			}
			k.raw.write(buf[:index])
			rest := buf[index+len(marker):]
			newline := bytes.IndexByte(rest, '\n')
			if newline < 0 {
				buf = buf[index:]
				break
			}
			length, err := strconv.Atoi(strings.TrimSpace(string(rest[:newline])))
			if err != nil || length < 0 {
				k.raw.write(marker)
				buf = slices.Clone(rest)
				continue
			}
			body := slices.Clone(rest[newline+1:])
			for len(body) < length {
				more := readSome(f, min(length-len(body), 1<<20))
				if more == nil {
					k.replies <- reply{Status: "kernel-eof"}
					return
				}
				body = append(body, more...)
			}
			var r reply
			if json.Unmarshal([]byte(strings.ToValidUTF8(string(body[:length]), "�")), &r) == nil && r.Status != "" {
				k.replies <- r
			} else {
				k.replies <- reply{Status: "protocol-error"}
			}
			buf = body[length:]
		}
	}
}

func (k *localKernel) readStderr(f *os.File) {
	defer f.Close()
	for chunk := readSome(f, 4096); chunk != nil; chunk = readSome(f, 4096) {
		k.stderr.write(chunk)
	}
}

// Kernels of the process, idle ones reaped
type kernels struct {
	mu      sync.Mutex
	held    map[key]*localKernel
	reaping bool
}

// Kernels nobody uses idle past their limit, taken out of the table; mu held
func (ks *kernels) idle(now time.Time) []*localKernel {
	var out []*localKernel
	for k, kernel := range ks.held {
		if kernel.attached == 0 && now.Sub(kernel.used) > kernel.idle {
			out = append(out, kernel)
			delete(ks.held, k)
		}
	}
	return out
}

// Tears down every kernel whose owner pick accepts, every kernel when pick is nil
func (ks *kernels) shutdown(pick func(owner string) bool) {
	ks.mu.Lock()
	var doomed []*localKernel
	for k, kernel := range ks.held {
		if pick == nil || pick(k.owner) {
			doomed = append(doomed, kernel)
			delete(ks.held, k)
		}
	}
	ks.mu.Unlock()
	for _, kernel := range doomed {
		kernel.teardown()
	}
}

// Takes the kernel out of the table when it still holds it, then tears it down
func (ks *kernels) discard(kernel *localKernel) {
	ks.mu.Lock()
	if ks.held[kernel.key] == kernel {
		delete(ks.held, kernel.key)
	}
	ks.mu.Unlock()
	kernel.teardown()
}

// Kernel for the key, a new one when there is none or reset asks for it or the old one died, the idle reaped and the least recently used
// past the cap of kernels torn down; whether the state of an old one was lost
func (r *registrant) acquire(call *tools.Call, k key, reset, pinned bool) (*localKernel, bool) {
	cfg := call.Env.Config
	limit, idle := max(1, cfg.GetInt(codeconfig.MaxSessionKernels)), time.Duration(max(1, cfg.GetInt(codeconfig.KernelIdleTimeout)))*time.Second
	ks := &r.kernels
	ks.mu.Lock()
	now := time.Now()
	expired := ks.idle(now)
	kernel := ks.held[k]
	reseted := kernel != nil && (reset || kernel.dead())
	if reseted {
		delete(ks.held, k)
		if kernel.attached == 0 {
			expired = append(expired, kernel)
		}
		kernel = nil
	}
	if kernel == nil {
		kernel = newKernel(k)
		kernel.pinned = pinned
		ks.held[k] = kernel
	}
	kernel.used, kernel.idle = now, idle
	kernel.attached++
	unpinned := 0
	var byAge []*localKernel
	for other, held := range ks.held {
		if held.pinned {
			continue
		}
		unpinned++
		if other != k && held.attached == 0 {
			byAge = append(byAge, held)
		}
	}
	slices.SortStableFunc(byAge, func(a, b *localKernel) int { return a.used.Compare(b.used) })
	for _, old := range byAge[:min(len(byAge), max(0, unpinned-limit))] {
		expired = append(expired, old)
		delete(ks.held, old.key)
	}
	ks.mu.Unlock()
	for _, doomed := range expired {
		doomed.teardown()
	}
	return kernel, reseted
}

// Lets the call's hold on the kernel go, tearing it down when it was replaced meanwhile and nobody else holds it
func (r *registrant) detach(kernel *localKernel) {
	ks := &r.kernels
	ks.mu.Lock()
	kernel.attached--
	kernel.used = time.Now()
	orphaned := kernel.attached == 0 && ks.held[kernel.key] != kernel
	ks.mu.Unlock()
	if orphaned {
		kernel.teardown()
	}
}

// Starts the reaper once a kernel exists, until none is left
func (r *registrant) startReaper() {
	ks := &r.kernels
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.reaping {
		return
	}
	ks.reaping = true
	go r.reap()
}

// Reaps idle kernels and staging folders a crash left, as often as the shortest idle limit allows, between 30 seconds and 5 minutes
func (r *registrant) reap() {
	ks := &r.kernels
	for {
		ks.mu.Lock()
		shortest := 30 * time.Minute
		for _, kernel := range ks.held {
			shortest = min(shortest, kernel.idle)
		}
		if len(ks.held) == 0 {
			ks.reaping = false
			ks.mu.Unlock()
			return
		}
		ks.mu.Unlock()
		select {
		case <-r.closed:
			return
		case <-time.After(min(5*time.Minute, max(30*time.Second, shortest/6))):
		}
		ks.mu.Lock()
		expired := ks.idle(time.Now())
		ks.mu.Unlock()
		for _, doomed := range expired {
			doomed.teardown()
		}
		sweepStaging(time.Now())
	}
}

// Removes staging folders untouched for a week, never following a link
func sweepStaging(now time.Time) {
	matches, _ := filepath.Glob(filepath.Join(globEscape(os.TempDir()), stagingPrefix+"*"))
	for _, path := range matches {
		if info, err := os.Lstat(path); err == nil && info.IsDir() && now.Sub(info.ModTime()) > stagingAge {
			os.RemoveAll(path)
		}
	}
}

func globEscape(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`*?[`, r) || r == '\\' && !windows {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Options of one cell
type cellRun struct {
	python, cwd string
	set         []string
	timeout     time.Duration
	maxCalls    int
	reset       bool
}

// Runs the code as one cell of the session's kernel of the mode, interpreter, folder and tools, so its state carries over to the next call
func (r *registrant) local(ctx context.Context, call *tools.Call, code string, c cellRun) string {
	set := slices.Sorted(slices.Values(c.set))
	k := key{owner: owner(call), mode: mode(call.Env.Config), python: c.python, cwd: c.cwd, tools: strings.Join(set, "\x00")}
	start := time.Now()
	kernel, reseted := r.acquire(call, k, c.reset, call.Env.Delegated != nil)
	defer r.detach(kernel)
	reused := kernel.proc.Load() != nil
	a := &authority{ctx: ctx, call: call}
	a.active.Store(true)
	kernel.run.Lock()
	defer kernel.run.Unlock()
	defer a.active.Store(false)
	out, err := r.cell(ctx, call, kernel, a, code, c, set, start, reused, reseted)
	if err == nil {
		return out
	}
	slog.Error("session kernel failed: " + err.Error())
	r.kernels.discard(kernel)
	return failed(err.Error(), kernel.budget.count(), rounded(time.Since(start).Seconds()), "")
}

func (r *registrant) cell(ctx context.Context, call *tools.Call, kernel *localKernel, a *authority, code string, c cellRun, set []string,
	start time.Time, reused, reseted bool) (string, error) {
	if kernel.proc.Load() == nil {
		if err := r.spawn(call, kernel, c.python, c.cwd, set, c.maxCalls); err != nil {
			return "", err
		}
	}
	kernel.cellStart = kernel.budget.restart()
	kernel.raw.drain()
	kernel.stderr.drain()
	kernel.authority.Store(a)
	line, err := json.Marshal(map[string]string{"id": hexID(), "code": code})
	if err != nil {
		return "", err
	}
	kernel.mu.Lock()
	stdin := kernel.stdin
	kernel.mu.Unlock()
	if stdin == nil {
		return "", errors.New("the kernel input pipe is closed")
	}
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		return "", err
	}
	status, got := "success", reply{}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		status = "interrupted"
	case <-timer.C:
		status = "timeout"
	case got = <-kernel.replies:
		if got.Status == "kernel-eof" || got.Status == "protocol-error" {
			status = "error"
		}
	}
	return tools.JSON(r.settle(ctx, call, kernel, status, got, c, set, start, reused, reseted)), nil
}

// Result of a settled cell, the kernel torn down where its state can't be trusted any more
func (r *registrant) settle(ctx context.Context, call *tools.Call, kernel *localKernel, status string, got reply, c cellRun, set []string,
	start time.Time, reused, reseted bool) result {
	cfg := call.Env.Config
	if status == "timeout" || status == "interrupted" {
		r.kernels.discard(kernel)
	}
	kernel.count = cmp.Or(got.ExecutionCount, kernel.count+1)
	stderrRaw := kernel.stderr.drain()
	text, meta := r.truncate(cfg, call.Env, clean(cfg, got.Stdout+kernel.raw.drain()))
	cellStderr := clean(cfg, got.Stderr+stderrRaw)
	info := &kernelInfo{Mode: "session", Reused: reused, ExecutionCount: kernel.count, StateReset: reseted}
	res := result{Status: status, Output: text, ToolCallsMade: kernel.budget.count(), Duration: rounded(time.Since(start).Seconds()), Kernel: info,
		stdout: meta}
	if failures := kernel.budget.failures(kernel.cellStart); len(failures) > 0 {
		res.ToolErrors = failures
	}
	if got.Spill != "" && got.StdoutClipped {
		spilled := got.Spill
		if call.Env.Visible != nil {
			spilled = call.Env.Visible(spilled)
		}
		res.Spill = spilled
		res.Warning = fmt.Sprintf("Cell stdout exceeded the inline cap; head shown. FULL output saved to %s — page it with read_file(path=\"%s\", "+
			"offset=...) instead of re-running. (Kernel state persists: printing a narrower slice next call is often cheaper.)", spilled, spilled)
	}
	switch {
	case status == "timeout":
		message := fmt.Sprintf("Cell timed out after %ss; the session kernel was killed and its state was lost. The next execute_code call starts "+
			"a fresh kernel.", seconds(c.timeout))
		res.ExitCode, res.Error, res.Output = -1, message, timedOut(text, message)
	case status == "interrupted":
		res.ExitCode, res.Output, res.Error = -1, interrupted(ctx, text), "Interrupted; the session kernel was killed and its state was lost."
	case got.Status == "error":
		trace := clean(cfg, got.Traceback)
		res.Status, res.ExitCode, res.Error = "error", 1, cmp.Or(trace, "Cell raised an exception.")
		if cellStderr != "" || trace != "" {
			res.Output = withStderr(text, cellStderr+trace)
		}
		res.Hint = failureHint(trace, set)
	case got.Status == "exit":
		r.kernels.discard(kernel)
		info.Ended = true
		if cellStderr != "" {
			res.Output = withStderr(text, cellStderr)
		}
	case status == "error":
		r.kernels.discard(kernel)
		res.ExitCode = -1
		res.Error = "The session kernel died while running the cell."
		if s := strings.TrimSpace(stderrRaw); s != "" {
			res.Error = "The session kernel died while running the cell: " + s
		}
	case cellStderr != "":
		res.Output = withStderr(text, cellStderr)
	}
	return res
}

// Seconds of a limit as messages quote it, without a fraction for a whole number
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

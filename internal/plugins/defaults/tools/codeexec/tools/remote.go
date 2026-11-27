package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	codeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How often the host looks for a cell's result in a sandbox, each look one command through the backend
const cellPoll = 500 * time.Millisecond

// What a sandbox kernel serves: the session that owns it, its environment and backend, and the tools its module calls
type remoteKey struct {
	owner, backend, env, tools string
}

// A kernel running detached in a sandbox, reached through request and result files
type remoteKernel struct {
	key   remoteKey
	env   terminal.Environment
	dir   string
	pid   string
	token string
	// Under the registry's lock
	used     time.Time
	idle     time.Duration
	count    int
	seq      int
	attached int
	pinned   bool
	order    uint64
}

func (k *remoteKernel) sh(command string, timeout time.Duration) (string, error) {
	out, err := inSandbox(context.Background(), k.env, command, timeout, nil)
	if err != nil {
		return "", err
	}
	return out.Output, nil
}

// Whether the runner still runs, a failure of the transport counting as no
func (k *remoteKernel) alive() bool {
	out, err := k.sh(fmt.Sprintf("kill -0 %s 2>/dev/null && echo ALIVE", quote(k.pid)), 15*time.Second)
	return err == nil && strings.Contains(out, "ALIVE")
}

// Kills the runner and what it started, then removes its folder
func (k *remoteKernel) kill() {
	pid := quote(k.pid)
	for _, step := range [][2]string{
		{fmt.Sprintf("pkill -TERM -P %s 2>/dev/null; kill %s 2>/dev/null; true", pid, pid), "remote kernel kill failed (transport?)"},
		{"rm -rf " + quote(k.dir), "remote kernel dir cleanup failed"},
	} {
		if _, err := k.sh(step[0], 15*time.Second); err != nil {
			slog.Debug(step[1], "error", err)
		}
	}
}

// Sandbox kernels of the process
type remotes struct {
	mu    sync.Mutex
	held  map[remoteKey]*remoteKernel
	order uint64
}

func (rs *remotes) shutdown(pick func(owner string) bool) {
	rs.mu.Lock()
	var doomed []*remoteKernel
	for k, kernel := range rs.held {
		if pick == nil || pick(k.owner) {
			doomed = append(doomed, kernel)
			delete(rs.held, k)
		}
	}
	rs.mu.Unlock()
	for _, kernel := range doomed {
		kernel.kill()
	}
}

func (rs *remotes) discard(kernel *remoteKernel) {
	rs.mu.Lock()
	if rs.held[kernel.key] == kernel {
		delete(rs.held, kernel.key)
	}
	rs.mu.Unlock()
	kernel.kill()
}

// Folder for the run's files in the sandbox: the environment's temp folder when it is absolute, else this machine's
func sandboxTemp(env terminal.Environment) string {
	for _, dir := range []string{env.TempDir(), os.TempDir()} {
		if strings.HasPrefix(dir, "/") {
			return cmp.Or(strings.TrimRight(dir, "/"), "/")
		}
	}
	return os.TempDir()
}

// Ships the variables a sandboxed script runs with as a file and gives the command that sources it and launches
func shipLaunch(ctx context.Context, call *tools.Call, env terminal.Environment, dir, name, launch, rpc, token string, extra ...[2]string) (string,
	error) {
	pairs := append([][2]string{{"ICEBATH_RPC_DIR", rpc}, {"ICEBATH_RPC_TOKEN", token}, {"PYTHONDONTWRITEBYTECODE", "1"}}, extra...)
	if zone := timezone(call.Env.Config); zone != "" {
		pairs = append(pairs, [2]string{"TZ", zone})
	}
	var lines strings.Builder
	for _, kv := range pairs {
		lines.WriteString(kv[0] + "=" + quote(kv[1]) + "\n")
	}
	if err := ship(ctx, env, dir+"/"+name, lines.String(), false); err != nil {
		return "", err
	}
	return fmt.Sprintf("cd %s && ( set -a && . ./%s && set +a && %s )", quote(dir), name, launch), nil
}

// Starts a runner detached in the sandbox, nil with its folder removed when it could not be started
func (r *registrant) spawnRemote(ctx context.Context, call *tools.Call, env terminal.Environment, k remoteKey, set []string, idle time.Duration) *remoteKernel {
	dir := sandboxTemp(env) + "/icebath_rkernel_" + hexID()[:12]
	var kernel *remoteKernel
	err := func() error {
		if _, err := checked(ctx, env, private(dir, dir+"/cells", dir+"/rpc"), "remote kernel dir setup", 15*time.Second, nil); err != nil {
			return err
		}
		secret := strs.Token(32)
		if err := ship(ctx, env, dir+"/kernel_runner.py", remoteRunner(int(idle.Seconds())), false); err != nil {
			return err
		}
		if err := ship(ctx, env, dir+"/icebath_tools.py", module(set, true), false); err != nil {
			return err
		}
		launch, err := shipLaunch(ctx, call, env, dir, "kernel.env", "rm -f ./kernel.env && touch runner.log && chmod 600 runner.log && "+
			`{ nohup python3 kernel_runner.py > runner.log 2>&1 & echo "PID:$!"; }`, dir+"/rpc", secret,
			[2]string{"ICEBATH_KERNEL_DIR", dir}, [2]string{"PYTHONPATH", dir})
		if err != nil {
			return err
		}
		started, err := inSandbox(ctx, env, launch, 20*time.Second, nil)
		if err != nil {
			return err
		}
		pid := ""
		for _, line := range strings.Split(started.Output, "\n") {
			if found, ok := strings.CutPrefix(strings.TrimSpace(line), "PID:"); ok {
				pid = strings.TrimSpace(found)
				break
			}
		}
		if pid == "" || strings.IndexFunc(pid, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
			slog.Warn("remote kernel spawn returned no PID: " + strings.TrimSpace(started.Output))
			return nil
		}
		candidate := &remoteKernel{key: k, env: env, dir: dir, pid: pid, token: secret, used: time.Now(), idle: idle, pinned: call.Env.Delegated != nil}
		if candidate.alive() {
			kernel = candidate
		} else if log, err := candidate.sh("cat "+quote(dir)+"/runner.log", 10*time.Second); err == nil {
			slog.Warn("remote kernel died at spawn: " + strs.Head(log, 500))
		}
		return nil
	}()
	if err != nil {
		slog.Warn("remote kernel spawn failed", "error", err)
	}
	if kernel == nil {
		inSandbox(ctx, env, "rm -rf "+quote(dir), 15*time.Second, nil)
	}
	return kernel
}

// Kernel of the key, made again when reset asks for it or the old one is gone; whether one was reused, reset or lost, nil when none could
// be started
func (r *registrant) acquireRemote(ctx context.Context, call *tools.Call, env terminal.Environment, k remoteKey, set []string, reset bool) (
	kernel *remoteKernel, reused, reseted, lost bool) {
	cfg := call.Env.Config
	idle := time.Duration(max(1, cfg.GetInt(codeconfig.KernelIdleTimeout))) * time.Second
	rs := &r.remotes
	rs.mu.Lock()
	var expired []*remoteKernel
	now := time.Now()
	for other, held := range rs.held {
		if held.attached == 0 && now.Sub(held.used) > held.idle {
			expired = append(expired, held)
			delete(rs.held, other)
		}
	}
	kernel = rs.held[k]
	rs.mu.Unlock()
	for _, doomed := range expired {
		doomed.kill()
	}
	if kernel != nil && reset {
		rs.discard(kernel)
		kernel, reseted = nil, true
	}
	if kernel != nil && (kernel.env != env || !kernel.alive()) {
		rs.discard(kernel)
		kernel, lost = nil, true
	}
	reused = kernel != nil
	if kernel == nil {
		if kernel = r.spawnRemote(ctx, call, env, k, set, idle); kernel != nil {
			rs.mu.Lock()
			rs.order++
			kernel.order = rs.order
			rs.held[k] = kernel
			rs.mu.Unlock()
		}
	}
	return kernel, reused, reseted, lost
}

// Attaches the call to the kernel, the least recently used of the others past the cap of kernels killed
func (r *registrant) attachRemote(call *tools.Call, kernel *remoteKernel) {
	limit := max(1, call.Env.Config.GetInt(codeconfig.MaxSessionKernels))
	rs := &r.remotes
	rs.mu.Lock()
	kernel.used = time.Now()
	kernel.attached++
	var unpinned, byAge []*remoteKernel
	for _, held := range rs.held {
		if !held.pinned {
			unpinned = append(unpinned, held)
			if held != kernel && held.attached == 0 {
				byAge = append(byAge, held)
			}
		}
	}
	var evicted []*remoteKernel
	if len(unpinned) > limit {
		slices.SortFunc(byAge, func(a, b *remoteKernel) int { return cmp.Or(a.used.Compare(b.used), cmp.Compare(a.order, b.order)) })
		for _, old := range byAge[:min(len(byAge), len(unpinned)-limit)] {
			evicted = append(evicted, old)
			delete(rs.held, old.key)
		}
	}
	rs.mu.Unlock()
	for _, doomed := range evicted {
		doomed.kill()
	}
}

func (r *registrant) detachRemote(kernel *remoteKernel) {
	r.remotes.mu.Lock()
	kernel.attached--
	kernel.used = time.Now()
	r.remotes.mu.Unlock()
}

// Result file of one cell as the runner wrote it
type remoteReply struct {
	reply
	StderrClipped bool `json:"stderr_clipped"`
}

// Ships the cell and waits for its result file: the status the runner gave, or timeout, interrupted or protocol-error
func (k *remoteKernel) cell(ctx context.Context, rs *remotes, code string, timeout time.Duration) (string, remoteReply, error) {
	rs.mu.Lock()
	k.seq++
	seq := fmt.Sprintf("%06d", k.seq)
	rs.mu.Unlock()
	body, err := json.Marshal(map[string]string{"id": seq, "code": code})
	if err != nil {
		return "", remoteReply{}, err
	}
	if err := ship(ctx, k.env, k.dir+"/cells/cell_req_"+seq+".json", string(body), true); err != nil {
		return "", remoteReply{}, err
	}
	cells, name := quote(k.dir+"/cells"), quote("cell_res_"+seq+".json")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if out, err := k.sh(fmt.Sprintf("cat %s/%s 2>/dev/null", cells, name), 20*time.Second); err == nil && strings.TrimSpace(out) != "" {
			var got remoteReply
			status := "protocol-error"
			var raw any
			if json.Unmarshal([]byte(strings.TrimSpace(out)), &raw) == nil {
				if _, isObject := raw.(map[string]any); !isObject {
					return "", remoteReply{}, fmt.Errorf("the kernel replied with %T where a mapping was expected", raw)
				}
				json.Unmarshal([]byte(strings.TrimSpace(out)), &got)
				status = cmp.Or(got.Status, "error")
			}
			if _, err := k.sh(fmt.Sprintf("rm -f %s/%s", cells, name), 10*time.Second); err != nil {
				slog.Debug("remote kernel: cell result cleanup failed", "error", err)
			}
			return status, got, nil
		}
		select {
		case <-ctx.Done():
			return "interrupted", remoteReply{}, nil
		case <-time.After(cellPoll):
		}
	}
	return "timeout", remoteReply{}, nil
}

// Runs the code in the session's sandbox kernel, nil when no kernel could be started there so the caller runs it as a script of its own
func (r *registrant) remoteCell(ctx context.Context, call *tools.Call, lease *terminal.Lease, code string, c cellRun, start time.Time) (*result,
	error) {
	set := slices.Sorted(slices.Values(c.set))
	k := remoteKey{owner: owner(call), backend: lease.Backend.Name, env: lease.Key, tools: strings.Join(set, "\x00")}
	kernel, reused, reseted, lost := r.acquireRemote(ctx, call, lease.Env, k, set, c.reset)
	if kernel == nil {
		return nil, nil
	}
	r.attachRemote(call, kernel)
	defer r.detachRemote(kernel)
	rpc := quote(kernel.dir + "/rpc")
	kernel.sh(fmt.Sprintf("rm -f %s/req_* %s/res_*", rpc, rpc), 10*time.Second)
	b := newBudget(c.set, c.maxCalls)
	a := &authority{ctx: ctx, call: call}
	a.active.Store(true)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		poll(ctx, kernel.env, kernel.dir+"/rpc", kernel.token, b, stop, func(_ context.Context, name string, args json.RawMessage) string {
			return a.dispatch(r, name, args)
		})
	}()
	status, got, err := kernel.cell(ctx, &r.remotes, code, c.timeout)
	close(stop)
	a.active.Store(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if err != nil {
		r.remotes.discard(kernel)
		return nil, err
	}
	cfg := call.Env.Config
	info := &kernelInfo{Remote: true, Reused: reused, StateReset: reseted}
	res := &result{Status: "error", ToolCallsMade: b.count(), Kernel: info, ToolErrors: b.failures(0)}
	output := got.Stdout
	if got.Stderr != "" || got.Traceback != "" {
		output = withStderr(output, got.Stderr+got.Traceback)
	}
	res.Output, res.stdout = r.truncate(cfg, call.Env, clean(cfg, output))
	res.Duration = rounded(time.Since(start).Seconds())
	switch status {
	case "timeout", "interrupted", "protocol-error", "no-result":
		r.remotes.discard(kernel)
		info.Ended, info.StateLost, res.ExitCode = true, true, -1
		info.Note = "Remote kernel protocol failure; kernel killed, state lost."
		switch status {
		case "timeout":
			res.Status = "timeout"
			info.Note = "Cell timed out; the remote session kernel was killed and its state was lost. The next call starts a fresh kernel."
			message := fmt.Sprintf("Cell timed out after %ss; the remote session kernel was killed and its state was lost. The next call starts "+
				"fresh.", seconds(c.timeout))
			res.Error, res.Output = message, timedOut(res.Output, message)
		case "interrupted":
			info.Note = "Interrupted; the remote session kernel was killed and its state was lost."
			res.Error, res.Output = info.Note, interrupted(ctx, res.Output)
		}
		return res, nil
	case "exit":
		r.remotes.discard(kernel)
		info.Ended = true
	}
	r.remotes.mu.Lock()
	kernel.count = got.ExecutionCount
	r.remotes.mu.Unlock()
	info.ExecutionCount = got.ExecutionCount
	if status == "ok" || status == "exit" {
		res.Status = "success"
	} else {
		res.ExitCode = 1
	}
	if lost {
		info.StateLost = true
		info.Note = "The previous remote kernel was gone (transport drop, container restart, or idle self-exit); state from earlier calls was lost " +
			"and a fresh kernel was started."
	}
	if trace := strings.TrimSpace(got.Traceback); status == "error" && trace != "" {
		lines := strings.Split(trace, "\n")
		res.Error = clean(cfg, lines[len(lines)-1])
		res.Hint = failureHint(clean(cfg, got.Traceback), set)
	}
	return res, nil
}

// Runs the code in the session's sandbox: in its kernel there, else as a script shipped with the tools module into a private folder of
// its own, serving the script's calls through files while it runs
func (r *registrant) remote(ctx context.Context, call *tools.Call, lease *terminal.Lease, code string, c cellRun) string {
	start := time.Now()
	env, backend := lease.Env, lease.Backend.Name
	check, err := inSandbox(ctx, env, "command -v python3 >/dev/null 2>&1 && echo OK", 15*time.Second, nil)
	if err != nil {
		return r.remoteFailure(err, start, 0)
	}
	if !strings.Contains(check.Output, "OK") {
		return failed(fmt.Sprintf("Python 3 is not available in the %s terminal environment. Install Python to use execute_code with remote backends.",
			backend), 0, 0, "")
	}
	res, err := r.remoteCell(ctx, call, lease, code, c, start)
	if err != nil {
		slog.Warn("remote session-kernel path failed; falling back to per-call", "error", err)
	}
	if res != nil {
		return tools.JSON(res)
	}
	slog.Info(fmt.Sprintf("remote session kernel unavailable on %s; using per-call path", backend))
	return r.script(ctx, call, lease, code, c, start)
}

func (r *registrant) remoteFailure(err error, start time.Time, calls int) string {
	seconds := rounded(time.Since(start).Seconds())
	slog.Error(fmt.Sprintf("execute_code remote failed after %gs with %d tool calls: %v", seconds, calls, err))
	return failed(err.Error(), calls, seconds, "")
}

// Runs the code once as its own script in the sandbox, removing what it shipped afterwards
func (r *registrant) script(ctx context.Context, call *tools.Call, lease *terminal.Lease, code string, c cellRun, start time.Time) string {
	env, backend := lease.Env, lease.Backend.Name
	dir := sandboxTemp(env) + "/icebath_exec_" + hexID()[:12]
	b := newBudget(c.set, c.maxCalls)
	a := &authority{ctx: ctx, call: call}
	a.active.Store(true)
	var res *terminal.Result
	err := func() error {
		stop := make(chan struct{})
		var done chan struct{}
		defer func() {
			close(stop)
			a.active.Store(false)
			if done != nil {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			}
			if _, err := inSandbox(context.Background(), env, "rm -rf "+quote(dir), 15*time.Second, nil); err != nil {
				slog.Debug("Failed to clean up remote sandbox " + dir)
			}
		}()
		if _, err := checked(ctx, env, private(dir, dir+"/rpc"), "remote sandbox setup", 10*time.Second, nil); err != nil {
			return err
		}
		secret := strs.Token(32)
		if err := ship(ctx, env, dir+"/icebath_tools.py", module(c.set, true), false); err != nil {
			return err
		}
		if err := ship(ctx, env, dir+"/script.py", code, false); err != nil {
			return err
		}
		done = make(chan struct{})
		go func() {
			defer close(done)
			poll(ctx, env, dir+"/rpc", secret, b, stop, func(_ context.Context, name string, args json.RawMessage) string {
				return a.dispatch(r, name, args)
			})
		}()
		launch, err := shipLaunch(ctx, call, env, dir, "sandbox.env", "exec python3 script.py", dir+"/rpc", secret)
		if err != nil {
			return err
		}
		slog.Info(fmt.Sprintf("Executing code on %s backend (task %s)...", backend, strs.Head(call.SessionID, 8)))
		res, err = inSandbox(ctx, env, launch, time.Duration(math.Ceil(c.timeout.Seconds()))*time.Second, nil)
		return err
	}()
	if err != nil {
		return r.remoteFailure(err, start, b.count())
	}
	cfg := call.Env.Config
	status := "success"
	switch {
	case res.TimedOut || res.Code == 124:
		status = "timeout"
	case res.Interrupted || res.Code == 130 || ctx.Err() != nil:
		status = "interrupted"
	}
	out := &result{Status: status, ExitCode: res.Code, ToolCallsMade: b.count(), ToolErrors: b.failures(0)}
	out.Output, out.stdout = r.truncate(cfg, call.Env, clean(cfg, res.Output))
	out.Duration = rounded(time.Since(start).Seconds())
	switch {
	case status == "timeout":
		message := fmt.Sprintf("Script timed out after %ss and was killed.", seconds(c.timeout))
		out.Error, out.Output = message, timedOut(out.Output, message)
		slog.Warn(fmt.Sprintf("execute_code (remote) timed out after %gs (limit %ss) with %d tool calls", out.Duration, seconds(c.timeout), b.count()))
	case status == "interrupted":
		out.Output = interrupted(ctx, out.Output)
	case res.Code != 0:
		out.Status, out.Error = "error", fmt.Sprintf("Script exited with code %d", res.Code)
	}
	return tools.JSON(out)
}

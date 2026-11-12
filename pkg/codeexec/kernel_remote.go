package codeexec

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Remote session kernel runner template filled with cell source, capture limit and idle exit
const remoteKernelRunnerTemplate = `"""Auto-generated Icebath REMOTE session-kernel runner (file cell protocol)."""
import contextlib
import io
import json
import os
import sys
import time
import traceback

KDIR = os.environ["ICEBATH_KERNEL_DIR"]
CELLS = os.path.join(KDIR, "cells")
_CAPTURE_LIMIT = {capture_limit}
IDLE_EXIT_SECONDS = {idle_exit}

{cell_source}

def main():
    execution_count = 0
    last_activity = time.time()
    while True:
        pending = sorted(
            f for f in os.listdir(CELLS)
            if f.startswith("cell_req_") and f.endswith(".json")
        )
        if not pending:
            if time.time() - last_activity > IDLE_EXIT_SECONDS:
                return  # self-reap: nobody is talking to us anymore
            time.sleep(0.2)
            continue
        for name in pending:
            req_path = os.path.join(CELLS, name)
            try:
                with open(req_path, "r", encoding="utf-8") as f:
                    request = json.load(f)
            except Exception:
                # Partially-written request (ship in progress): retry next tick.
                continue
            os.remove(req_path)
            last_activity = time.time()
            execution_count += 1
            payload, _ = run_cell(request, execution_count)
            res_name = name.replace("cell_req_", "cell_res_")
            tmp = os.path.join(CELLS, res_name + ".tmp")
            # Cell results carry the executed code's output: owner-only, even if
            # the process umask is permissive.
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(payload, f, ensure_ascii=False)
            os.replace(tmp, os.path.join(CELLS, res_name))
            if payload["status"] == "exit":
                return


if __name__ == "__main__":
    main()
`

// Host poll interval for a cell result file, each poll one transport round trip
var cellPollInterval = 500 * time.Millisecond

// Remote file ship and per cell RPC poll loop, swapped by tests
var (
	shipFileToRemote = shipFile
	remotePollLoop   = rpcPollLoop
)

// Runner source with the cell protocol, capture limit and idle window filled in one pass
func remoteKernelRunnerSource(idleExit *big.Int) string {
	return strings.NewReplacer("{cell_source}", runnerCellSource, "{capture_limit}", fmt.Sprint(maxStdoutBytes),
		"{idle_exit}", idleExit.String()).Replace(remoteKernelRunnerTemplate)
}

// Output text of cmd run on the remote from the filesystem root
func remoteSh(ctx context.Context, envr tools.Environment, cmd string, timeout int) (string, error) {
	result, err := envr.Execute(ctx, cmd, tools.ExecOptions{Cwd: "/", Timeout: timeout})
	if err != nil || result == nil {
		return "", err
	}
	return result.Output, nil
}

// Host record of one detached remote kernel runner
type remoteKernel struct {
	envr           tools.Environment
	envType        string
	kernelDir      string
	pid            string
	rpcToken       string
	owner          string
	lastUsed       time.Time
	executionCount any
	cellSeq        int
	attached       int
	pinned         bool
	order          uint64
}

func (k *remoteKernel) sh(cmd string, timeout int) (string, error) {
	return remoteSh(context.Background(), k.envr, cmd, timeout)
}

// Bounded kill -0 probe through the transport, any transport failure reading as dead
func (k *remoteKernel) isAlive() bool {
	out, err := k.sh(fmt.Sprintf("kill -0 %s 2>/dev/null && echo ALIVE", pyshlex.Quote(k.pid)), 15)
	return err == nil && strings.Contains(out, "ALIVE")
}

// Kills the runner and its children, then removes the kernel dir, logging each transport failure
func (k *remoteKernel) kill() {
	qPid := pyshlex.Quote(k.pid)
	steps := [][2]string{
		{fmt.Sprintf("pkill -TERM -P %s 2>/dev/null; kill %s 2>/dev/null; true", qPid, qPid), "remote kernel kill failed (transport?)"},
		{"rm -rf " + pyshlex.Quote(k.kernelDir), "remote kernel dir cleanup failed"},
	}
	for _, step := range steps {
		if _, err := k.sh(step[0], 15); err != nil {
			slogDebug(nil, step[1], err)
		}
	}
}

// Registry key, a kernel serving only calls with the same owner, backend, task env and sandbox tool set
type remoteKernelKey struct {
	owner     string
	envType   string
	taskEnvID string
	tools     string
}

func remoteKernelKeyFor(owner, envType, taskEnvID string, sandboxTools []string) remoteKernelKey {
	sorted := slices.Clone(sandboxTools)
	sort.Strings(sorted)
	return remoteKernelKey{owner: owner, envType: envType, taskEnvID: taskEnvID, tools: strings.Join(sorted, "\x00")}
}

var remoteRegistry = &kernelRegistry[remoteKernelKey, *remoteKernel]{
	kernels:  map[remoteKernelKey]*remoteKernel{},
	ownerOf:  func(k remoteKernelKey) string { return k.owner },
	teardown: func(k *remoteKernel) { k.kill() },
}

// Registration counter standing in for dict insertion order on last used ties
var remoteKernelOrder uint64

// ShutdownAllRemoteKernels kills every remote session kernel, the process wiring calls it at exit
func ShutdownAllRemoteKernels() { remoteRegistry.shutdown(nil) }

// Disposes one session's remote kernels at the same clear_session boundary as local kernels
func shutdownRemoteKernelsForOwner(owner string) {
	if owner != "" {
		remoteRegistry.shutdown(func(o string) bool { return o == owner })
	}
}

// Disposes every remote kernel whose owner the predicate accepts
func shutdownRemoteKernelsWhere(matches func(owner string) bool) { remoteRegistry.shutdown(matches) }

// Pops idle expired unattached kernels, the caller holds the lock and tears them down after release
func reapRemoteUnlocked(now time.Time, idleTimeout *big.Int) []*remoteKernel {
	limit := new(big.Float).SetInt(idleTimeout)
	var doomed []*remoteKernel
	for key, k := range remoteRegistry.kernels {
		if k.attached == 0 && big.NewFloat(now.Sub(k.lastUsed).Seconds()).Cmp(limit) > 0 {
			doomed = append(doomed, k)
			delete(remoteRegistry.kernels, key)
		}
	}
	return doomed
}

// Pops least recently used unattached kernels beyond the live kernel cap, pinned kernels never counted
func evictRemoteOverCapUnlocked(env *tools.Env, keep remoteKernelKey) []*remoteKernel {
	limit, _ := lifecycleLimits(env)
	var unpinned []remoteKernelKey
	for key, k := range remoteRegistry.kernels {
		if !k.pinned {
			unpinned = append(unpinned, key)
		}
	}
	if len(unpinned) <= limit {
		return nil
	}
	var byAge []remoteKernelKey
	for _, key := range unpinned {
		if key != keep && remoteRegistry.kernels[key].attached == 0 {
			byAge = append(byAge, key)
		}
	}
	sort.Slice(byAge, func(i, j int) bool {
		a, b := remoteRegistry.kernels[byAge[i]], remoteRegistry.kernels[byAge[j]]
		if !a.lastUsed.Equal(b.lastUsed) {
			return a.lastUsed.Before(b.lastUsed)
		}
		return a.order < b.order
	})
	byAge = byAge[:min(len(byAge), len(unpinned)-limit)]
	out := make([]*remoteKernel, 0, len(byAge))
	for _, key := range byAge {
		out = append(out, remoteRegistry.kernels[key])
		delete(remoteRegistry.kernels, key)
	}
	return out
}

// Starts a detached runner on the remote, nil with the kernel dir removed on any failure
func spawnRemoteKernel(ctx context.Context, env *tools.Env, envr tools.Environment, envType, owner string, sandboxTools []string, idleExit *big.Int) (*remoteKernel, error) {
	tmp, err := envTempDir(envr)
	if err != nil {
		return nil, err
	}
	kernelDir := tmp + "/icebath_rkernel_" + uuid4Hex()[:12]
	qDir := pyshlex.Quote(kernelDir)
	var kernel *remoteKernel
	err = func() error {
		if _, err := executeChecked(ctx, envr, privateDirsCmd(kernelDir, kernelDir+"/cells", kernelDir+"/rpc"), "remote kernel dir setup", 15, nil); err != nil {
			return err
		}
		rpcToken := tokenURLSafe(32)
		if err := shipFileToRemote(ctx, envr, kernelDir+"/kernel_runner.py", remoteKernelRunnerSource(idleExit), false); err != nil {
			return err
		}
		if err := shipFileToRemote(ctx, envr, kernelDir+"/icebath_tools.py", generateToolsModule(sandboxTools, "file"), false); err != nil {
			return err
		}
		launch, err := shipEnvFileAndLaunch(ctx, envr, kernelDir, "kernel.env",
			"rm -f ./kernel.env && touch runner.log && chmod 600 runner.log && "+
				`{ nohup python3 kernel_runner.py > runner.log 2>&1 & echo "PID:$!"; }`,
			kernelDir+"/rpc", rpcToken, [2]string{"ICEBATH_KERNEL_DIR", kernelDir}, [2]string{"PYTHONPATH", kernelDir})
		if err != nil {
			return err
		}
		started, err := remoteSh(ctx, envr, launch, 20)
		if err != nil {
			return err
		}
		pid := ""
		for _, line := range pystr.Splitlines(started, false) {
			if stripped := pystr.Strip(line); strings.HasPrefix(stripped, "PID:") {
				pid = pystr.Strip(stripped[4:])
				break
			}
		}
		if !isPyDigit(pid) {
			logger(env).Warn("remote kernel spawn returned no PID: " + pyrepr.Repr(started))
			return nil
		}
		candidate := &remoteKernel{envr: envr, envType: envType, kernelDir: kernelDir, pid: pid, rpcToken: rpcToken,
			owner: owner, lastUsed: time.Now()}
		if candidate.isAlive() {
			kernel = candidate
		} else if log, err := remoteSh(ctx, envr, "cat "+qDir+"/runner.log", 10); err == nil {
			logger(env).Warn("remote kernel died at spawn: " + pystr.Slice(log, 0, 500))
		}
		return nil
	}()
	if err != nil {
		logger(env).Warn("remote kernel spawn failed", "error", err)
	}
	if kernel == nil {
		_, _ = remoteSh(ctx, envr, "rm -rf "+qDir, 15)
	}
	return kernel, nil
}

// Python str.isdigit, non empty and every character a Unicode digit
func isPyDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Remote cell parameters of one execute_code call
type remoteCell struct {
	envType      string
	taskEnvID    string
	sandboxTools []string
	timeout      float64
	maxToolCalls int
	reset        bool
	idleExit     *big.Int
}

// Finds or respawns the owner's kernel after reaping idle entries, reporting reuse, reset and loss
func acquireRemoteKernel(ctx context.Context, env *tools.Env, envr tools.Environment, owner string, c remoteCell) (*remoteKernel, bool, bool, bool, error) {
	key := remoteKernelKeyFor(owner, c.envType, c.taskEnvID, c.sandboxTools)
	stateLost, stateReset := false, false
	remoteRegistry.mu.Lock()
	expired := reapRemoteUnlocked(time.Now(), c.idleExit)
	kernel := remoteRegistry.kernels[key]
	remoteRegistry.mu.Unlock()
	for _, doomed := range expired {
		doomed.kill()
	}
	if kernel != nil && c.reset {
		remoteRegistry.discard(key, kernel)
		kernel, stateReset = nil, true
	}
	if kernel != nil && !kernel.isAlive() {
		remoteRegistry.discard(key, kernel)
		kernel, stateLost = nil, true
	}
	reused := kernel != nil
	if kernel == nil {
		spawned, err := spawnRemoteKernel(ctx, env, envr, c.envType, owner, c.sandboxTools, c.idleExit)
		if err != nil {
			return nil, false, false, false, err
		}
		kernel = spawned
		if kernel != nil {
			kernel.pinned = tools.IsDelegatedChildContext(ctx)
			remoteRegistry.mu.Lock()
			remoteKernelOrder++
			kernel.order = remoteKernelOrder
			remoteRegistry.kernels[key] = kernel
			remoteRegistry.mu.Unlock()
		}
	}
	return kernel, reused, stateReset, stateLost, nil
}

// Ships one cell request and polls for its result file, returning the cell status and payload
func runRemoteCell(ctx context.Context, kernel *remoteKernel, code string, timeout float64) (any, *pyjson.Object, error) {
	remoteRegistry.mu.Lock()
	kernel.cellSeq++
	seq := fmt.Sprintf("%06d", kernel.cellSeq)
	remoteRegistry.mu.Unlock()
	qCells, qRes := pyshlex.Quote(kernel.kernelDir+"/cells"), pyshlex.Quote("cell_res_"+seq+".json")
	request := pyjson.NewObject()
	request.Set("id", seq)
	request.Set("code", code)
	body, err := pyjson.Dumps(request, pyjson.EnsureASCII(false))
	if err != nil {
		return nil, nil, err
	}
	if err := shipFileToRemote(ctx, kernel.envr, kernel.kernelDir+"/cells/cell_req_"+seq+".json", body, true); err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for time.Now().Before(deadline) {
		out, err := kernel.sh(fmt.Sprintf("cat %s/%s 2>/dev/null", qCells, qRes), 20)
		if err != nil {
			out = ""
		}
		if text := pystr.Strip(out); text != "" {
			var status any
			payload := pyjson.NewObject()
			decoded, err := pyjson.Loads(text)
			if err != nil {
				status = "protocol-error"
			} else if obj, ok := decoded.(*pyjson.Object); ok {
				payload = obj
				status = objGet(obj, "status", "error")
			} else {
				return nil, nil, replyShapeError{decoded}
			}
			if _, err := kernel.sh(fmt.Sprintf("rm -f %s/%s", qCells, qRes), 10); err != nil {
				slogDebug(nil, "remote kernel: cell result cleanup failed", err)
			}
			return status, payload, nil
		}
		time.Sleep(cellPollInterval)
	}
	return "timeout", pyjson.NewObject(), nil
}

// Value under key, the default when the key is absent
func objGet(o *pyjson.Object, key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}

// Debug log of a swallowed failure with its error
func slogDebug(env *tools.Env, msg string, err error) { logger(env).Debug(msg, "error", err) }

// Runs one cell in the owner's remote kernel, nil when no kernel could be spawned so the caller runs per call
func executeInRemoteKernel(ctx context.Context, env *tools.Env, envr tools.Environment, code string, c remoteCell) (*pyjson.Object, error) {
	owner := resolveOwner(ctx, env, c.taskEnvID)
	kernel, reused, stateReset, stateLost, err := acquireRemoteKernel(ctx, env, envr, owner, c)
	if err != nil || kernel == nil {
		return nil, err
	}
	key := remoteKernelKeyFor(owner, c.envType, c.taskEnvID, c.sandboxTools)
	remoteRegistry.mu.Lock()
	kernel.lastUsed = time.Now()
	kernel.attached++
	evicted := evictRemoteOverCapUnlocked(env, key)
	remoteRegistry.mu.Unlock()
	for _, doomed := range evicted {
		doomed.kill()
	}
	defer func() {
		remoteRegistry.mu.Lock()
		kernel.attached--
		kernel.lastUsed = time.Now()
		remoteRegistry.mu.Unlock()
	}()
	return runAttachedCell(ctx, env, kernel, key, code, c, reused, stateReset, stateLost)
}

// Runs the cell under a per cell RPC poll loop carrying this call's authority and builds the raw cell result
func runAttachedCell(ctx context.Context, env *tools.Env, kernel *remoteKernel, key remoteKernelKey, code string, c remoteCell, reused, stateReset, stateLost bool) (*pyjson.Object, error) {
	qRPC := pyshlex.Quote(kernel.kernelDir + "/rpc")
	_, _ = kernel.sh(fmt.Sprintf("rm -f %s/req_* %s/res_*", qRPC, qRPC), 10)
	budget := newRPCBudget(c.sandboxTools, c.maxToolCalls)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		remotePollLoop(ctx, env, kernel.envr, kernel.kernelDir+"/rpc", c.taskEnvID, budget, stop, kernel.rpcToken, defaultDispatch(env, c.taskEnvID))
	}()
	status, payload, err := runRemoteCell(ctx, kernel, code, c.timeout)
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	if err != nil {
		remoteRegistry.discard(key, kernel)
		return nil, err
	}
	info := pyjson.NewObject()
	info.Set("reused", reused)
	info.Set("remote", true)
	result := pyjson.NewObject()
	result.Set("status", "error")
	result.Set("stdout", objGet(payload, "stdout", ""))
	result.Set("stderr", objGet(payload, "stderr", ""))
	result.Set("traceback", objGet(payload, "traceback", ""))
	result.Set("tool_calls_made", budget.callCount())
	result.Set("kernel", info)
	result.Set("tool_errors", toolErrorsSince(budget.entries(), 0))
	if isStatus(status, "timeout", "protocol-error", "no-result") {
		remoteRegistry.discard(key, kernel)
		note := "Remote kernel protocol failure; kernel killed, state lost."
		if isStatus(status, "timeout") {
			result.Set("status", "timeout")
			note = "Cell timed out; the remote session kernel was killed and its state was lost. The next call " +
				"starts a fresh kernel."
		}
		info.Set("ended", true)
		info.Set("state_lost", true)
		info.Set("note", note)
		return result, nil
	}
	if isStatus(status, "exit") {
		remoteRegistry.discard(key, kernel)
		info.Set("ended", true)
	}
	count := objGet(payload, "execution_count", 0)
	if !pyrepr.Truthy(count) {
		count = 0
	}
	n, err := pyint.Of(count)
	if err != nil {
		return nil, err
	}
	remoteRegistry.mu.Lock()
	kernel.executionCount = pyint.Value(n)
	remoteRegistry.mu.Unlock()
	info.Set("execution_count", pyint.Value(n))
	if isStatus(status, "ok", "exit") {
		result.Set("status", "success")
	}
	result.Set("stdout_clipped", pyrepr.Truthy(objGet(payload, "stdout_clipped", nil)))
	result.Set("stderr_clipped", pyrepr.Truthy(objGet(payload, "stderr_clipped", nil)))
	if stateReset {
		info.Set("state_reset", true)
	}
	if stateLost {
		info.Set("state_lost", true)
		info.Set("note", "The previous remote kernel was gone (transport drop, container "+
			"restart, or idle self-exit); state from earlier calls was lost and a fresh kernel was started.")
	}
	if tb, _ := result.Get("traceback"); isStatus(status, "error") && pyrepr.Truthy(tb) {
		text, ok := tb.(string)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(tb))
		}
		lines := pystr.Splitlines(pystr.Strip(text), false)
		if len(lines) == 0 {
			return nil, fmt.Errorf("list index out of range")
		}
		result.Set("error", lines[len(lines)-1])
	}
	return result, nil
}

// Reports whether a decoded status is one of the names, a non string status matching none
func isStatus(status any, names ...string) bool {
	s, ok := status.(string)
	return ok && slices.Contains(names, s)
}

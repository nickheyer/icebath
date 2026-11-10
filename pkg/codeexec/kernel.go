package codeexec

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

//go:embed payload/kernel_runner.py
var kernelRunnerSource string

// Owned spawn and temp root lookups, swapped by tests
var (
	spawnOwned = tools.SpawnOwned
	gettempdir = pypath.Gettempdir
)

const (
	defaultMaxSessionKernels = 4
	defaultKernelIdleTimeout = 1800
	childOwnerQualifier      = "::child::"
	staleStagingDirAge       = 7 * 86400 * time.Second
	reaperIntervalFloor      = 30.0
	reaperIntervalCeil       = 300.0
	kernelStagingPrefix      = "icebath_kernel_"
)

// Approval and session identity of exactly one cell, refusing dispatch once the cell settles
type cellAuthority struct {
	taskID string
	ctx    context.Context
	env    *tools.Env
	active atomic.Bool
}

// Snapshot of the calling cell context and env fields so later cells never borrow this identity
func newCellAuthority(ctx context.Context, env *tools.Env, taskID string) *cellAuthority {
	a := &cellAuthority{taskID: taskID, ctx: ctx}
	if env != nil {
		snapshot := *env
		a.env = &snapshot
	}
	a.active.Store(true)
	return a
}

func (a *cellAuthority) retire() { a.active.Store(false) }

// Runs one tool call under this cell context, refused once the cell retired
func (a *cellAuthority) dispatch(toolName string, toolArgs any) string {
	if !a.active.Load() {
		return toolError("No active execute_code cell: the cell this kernel call " +
			"belonged to has settled, so its tool authority is retired.")
	}
	return a.invoke(toolName, toolArgs)
}

func (a *cellAuthority) invoke(toolName string, toolArgs any) string {
	return defaultDispatch(a.env, a.taskID)(a.ctx, toolName, toolArgs)
}

// Byte chunks capped at a total size, drained as replacement decoded text
type boundedBuffer struct {
	mu     sync.Mutex
	chunks [][]byte
	total  int
}

func (b *boundedBuffer) append(data []byte, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keep := min(max(0, limit-b.total), len(data))
	if keep > 0 {
		b.chunks = append(b.chunks, append([]byte(nil), data[:keep]...))
		b.total += keep
	}
}

func (b *boundedBuffer) drain() string {
	b.mu.Lock()
	chunks := b.chunks
	b.chunks, b.total = nil, 0
	b.mu.Unlock()
	return pystr.DecodeUTF8Replace(bytes.Join(chunks, nil))
}

// Unbounded FIFO of decoded reply frames with a timed get
type responseQueue struct {
	mu    sync.Mutex
	items []any
	ready chan struct{}
}

func newResponseQueue() *responseQueue { return &responseQueue{ready: make(chan struct{}, 1)} }

func (q *responseQueue) put(v any) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *responseQueue) get(timeout time.Duration) (any, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			v := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return v, true
		}
		q.mu.Unlock()
		select {
		case <-q.ready:
		case <-timer.C:
			return nil, false
		}
	}
}

func statusObject(status string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("status", status)
	return o
}

// Owner, mode, interpreter, cwd and sorted tool set identifying one session kernel
type kernelKey struct {
	owner, mode, python, cwd, tools string
}

type kernelProc struct {
	p      *os.Process
	exited chan struct{}
}

// One live kernel process plus its RPC server and reader goroutines
type sessionKernel struct {
	key   kernelKey
	owner string
	lock  sync.Mutex

	mu       sync.Mutex
	tmpdir   string
	sockPath string
	listener net.Listener
	stdin    *os.File

	proc     atomic.Pointer[kernelProc]
	rpcToken string
	sentinel string
	stop     chan struct{}
	stopOnce sync.Once

	budget       *rpcBudget
	cellLogStart int
	attached     int
	pinned       bool
	lastUsed     time.Time

	responses      *responseQueue
	raw, stderr    boundedBuffer
	executionCount any
	authority      atomic.Pointer[cellAuthority]
}

func newSessionKernel(key kernelKey) *sessionKernel {
	return &sessionKernel{key: key, owner: key.owner, stop: make(chan struct{}), executionCount: 0,
		budget: newRPCBudget(nil, 0), responses: newResponseQueue(), lastUsed: time.Now()}
}

func (p *kernelProc) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (k *sessionKernel) alive() bool {
	p := k.proc.Load()
	return p != nil && p.running()
}

// True only once a spawned process exited, a kernel still spawning is pending and not dead
func (k *sessionKernel) dead() bool {
	p := k.proc.Load()
	return p != nil && !p.running()
}

// Kills the process group, releases it, closes the RPC socket and removes the staging dir
func (k *sessionKernel) teardown() {
	k.stopOnce.Do(func() { close(k.stop) })
	p := k.proc.Load()
	if p != nil && p.running() {
		killProcessGroup(p.p.Pid, p.p.Kill, p.exited, true)
	}
	if p != nil && runtime.GOOS != "windows" {
		tools.ReleaseProcessGroupOnceEmpty(p.p.Pid, nil, time.Second)
	}
	k.mu.Lock()
	ln, sockPath, tmpdir, stdin := k.listener, k.sockPath, k.tmpdir, k.stdin
	k.listener, k.stdin = nil, nil
	k.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if sockPath != "" {
		_ = os.Remove(sockPath)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if tmpdir != "" {
		_ = os.RemoveAll(tmpdir)
	}
}

// Key to kernel map plus its lock, kernels popped under the lock and torn down outside it
type kernelRegistry[K comparable, V comparable] struct {
	mu       sync.Mutex
	kernels  map[K]V
	ownerOf  func(K) string
	teardown func(V)
}

// Tears down every kernel whose owner pick accepts, every kernel when pick is nil
func (r *kernelRegistry[K, V]) shutdown(pick func(owner string) bool) {
	r.mu.Lock()
	var doomed []V
	for key, kernel := range r.kernels {
		if pick == nil || pick(r.ownerOf(key)) {
			doomed = append(doomed, kernel)
			delete(r.kernels, key)
		}
	}
	r.mu.Unlock()
	for _, kernel := range doomed {
		r.teardown(kernel)
	}
}

// Drops the entry only when it still holds this kernel, then tears the kernel down
func (r *kernelRegistry[K, V]) discard(key K, kernel V) {
	r.mu.Lock()
	if current, ok := r.kernels[key]; ok && current == kernel {
		delete(r.kernels, key)
	}
	r.mu.Unlock()
	r.teardown(kernel)
}

var registry = &kernelRegistry[kernelKey, *sessionKernel]{
	kernels:  map[kernelKey]*sessionKernel{},
	ownerOf:  func(k kernelKey) string { return k.owner },
	teardown: func(k *sessionKernel) { k.teardown() },
}

// Configured max live kernels and idle timeout, each at least one, defaults when not an integer
var lifecycleLimits = func(env *tools.Env) (int, int) {
	cfg := loadConfig(env)
	limit := func(key string, def int) int {
		v, ok := cfg[key]
		if !ok {
			v = def
		}
		n, err := pyint.Of(v)
		if err != nil {
			return def
		}
		if !n.IsInt64() || n.Int64() > math.MaxInt {
			if n.Sign() > 0 {
				return math.MaxInt
			}
			return 1
		}
		return max(1, int(n.Int64()))
	}
	return limit("max_session_kernels", defaultMaxSessionKernels), limit("kernel_idle_timeout", defaultKernelIdleTimeout)
}

// Session key owning the kernel, task id as last resort, qualified by the child id inside a delegated child
func resolveOwner(ctx context.Context, env *tools.Env, taskID string) string {
	owner := tools.CurrentSessionKeyOr(env, "")
	if owner == "" {
		owner = taskID
	}
	if tools.IsDelegatedChildContext(ctx) {
		childID := tools.SessionEnvValue(env, "ICEBATH_SESSION_ID")
		if childID == "" {
			childID = taskID
		}
		owner = owner + childOwnerQualifier + childID
	}
	return owner
}

// ShutdownAllKernels kills every session kernel, the process wiring calls it at exit
func ShutdownAllKernels() { registry.shutdown(nil) }

// ShutdownKernelsForOwner disposes every kernel a session owns when its approval state clears
func ShutdownKernelsForOwner(owner string) {
	if owner != "" {
		registry.shutdown(func(o string) bool { return o == owner })
	}
}

// DelegatedChildOwnerMatcher matches the kernel owners of one delegate_task child
func DelegatedChildOwnerMatcher(childSessionID string) func(owner string) bool {
	suffix := childOwnerQualifier + childSessionID
	return func(owner string) bool { return strings.HasSuffix(owner, suffix) }
}

// ShutdownKernelsForDelegatedChild disposes a finished child's local and remote kernels
func ShutdownKernelsForDelegatedChild(childSessionID string) {
	if childSessionID == "" {
		return
	}
	matcher := DelegatedChildOwnerMatcher(childSessionID)
	registry.shutdown(matcher)
	shutdownRemoteKernelsWhere(matcher)
}

// Serves tool RPC for the kernel life, every dispatch routed through the current cell authority
func (k *sessionKernel) rpcForever(env *tools.Env, ln net.Listener) {
	dispatch := func(_ context.Context, toolName string, args any) string {
		authority := k.authority.Load()
		if authority == nil {
			return toolError("No active execute_code cell: this kernel has no cell authority installed.")
		}
		return authority.dispatch(toolName, args)
	}
	for !stopped(k.stop) {
		rpcServerLoop(context.Background(), env, ln, "", k.budget, k.stop, k.rpcToken, dispatch)
	}
}

// Python slice bound for a possibly negative index against length n
func pyBound(i, n int) int {
	if i < 0 {
		return max(0, n+i)
	}
	return min(i, n)
}

// Reads whatever bytes arrive up to n, nil at end of stream
func readSome(stream *os.File, n int) []byte {
	p := make([]byte, n)
	for {
		m, err := stream.Read(p)
		if m > 0 {
			return p[:m]
		}
		if err != nil {
			return nil
		}
	}
}

// Splits child stdout into sentinel framed replies and raw passthrough attributed to the running cell
func (k *sessionKernel) stdoutReader(stream *os.File) {
	defer stream.Close()
	marker := []byte("\n" + k.sentinel + " ")
	raw := func(data []byte) { k.raw.append(data, maxStdoutBytes) }
	var buf []byte
	for {
		chunk := readSome(stream, 4096)
		if chunk == nil {
			if len(buf) > 0 {
				raw(buf)
			}
			k.responses.put(statusObject("kernel-eof"))
			return
		}
		buf = append(buf, chunk...)
		for {
			index := bytes.Index(buf, marker)
			if index < 0 {
				if len(buf) > len(marker) {
					spill := buf[:len(buf)-len(marker)]
					raw(spill)
					buf = append([]byte(nil), buf[len(spill):]...)
				}
				break
			}
			if index > 0 {
				raw(buf[:index])
			}
			rest := buf[index+len(marker):]
			newline := bytes.IndexByte(rest, '\n')
			if newline < 0 {
				buf = buf[index:]
				break
			}
			header := rest[:newline]
			parsed, ok := pyint.Parse(string(header))
			if !ok || !utf8Header(header) {
				raw(marker)
				buf = rest
				continue
			}
			length := math.MaxInt
			if parsed.IsInt64() && parsed.Int64() <= math.MaxInt {
				length = int(parsed.Int64())
			} else if parsed.Sign() < 0 {
				length = math.MinInt / 2
			}
			body := append([]byte(nil), rest[newline+1:]...)
			for len(body) < length {
				more := readSome(stream, min(length-len(body), 1<<20))
				if more == nil {
					k.responses.put(statusObject("kernel-eof"))
					return
				}
				body = append(body, more...)
			}
			end := pyBound(length, len(body))
			if decoded, err := pyjson.Loads(pystr.DecodeUTF8Replace(body[:end])); err == nil {
				k.responses.put(decoded)
			} else {
				k.responses.put(statusObject("protocol-error"))
			}
			buf = body[end:]
		}
	}
}

// int() of bytes decodes them as UTF-8 first, so undecodable header bytes are no frame
func utf8Header(b []byte) bool { return pystr.DecodeUTF8Replace(b) == string(b) }

func (k *sessionKernel) stderrReader(stream *os.File) {
	defer stream.Close()
	for {
		chunk := readSome(stream, 4096)
		if chunk == nil {
			return
		}
		k.stderr.append(chunk, maxStderrBytes)
	}
}

func tokenURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func uuid4Hex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b)
}

// Binds the tool RPC listener, loopback TCP on Windows and an owner only socket elsewhere
func (k *sessionKernel) bindRPCSocket() (string, error) {
	if runtime.GOOS == "windows" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		addr := ln.Addr().(*net.TCPAddr)
		k.mu.Lock()
		k.sockPath, k.listener = "", ln
		k.mu.Unlock()
		return fmt.Sprintf("tcp://%s:%d", addr.IP.String(), addr.Port), nil
	}
	dir, err := home.SocketSafeTmpdir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "icebath_rpc_"+uuid4Hex()+".sock")
	k.mu.Lock()
	k.sockPath = path
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

func closeFiles(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// Stages the runner and tools module, binds RPC and starts the kernel as an owned group leader
func (k *sessionKernel) spawn(ctx context.Context, env *tools.Env, childPython, childCwd string, sandboxTools []string, maxToolCalls int) error {
	tempRoot, err := gettempdir()
	if err != nil {
		return err
	}
	tmpdir, err := os.MkdirTemp(tempRoot, kernelStagingPrefix)
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.tmpdir = tmpdir
	k.mu.Unlock()
	k.rpcToken = tokenURLSafe(32)
	k.sentinel = "@@ICEBATH-KERNEL-" + tokenURLSafe(16) + "@@"
	endpoint, err := k.bindRPCSocket()
	if err != nil {
		return err
	}
	for _, f := range []struct{ name, src string }{
		{"icebath_tools.py", generateToolsModule(sandboxTools, "uds")},
		{"icebath_kernel_runner.py", kernelRunnerSource},
	} {
		if err := os.WriteFile(filepath.Join(tmpdir, f.name), []byte(f.src), 0o666); err != nil {
			return err
		}
	}
	childEnv, err := buildChildEnv(ctx, env, childEnvSpec{RPCEndpoint: endpoint, RPCToken: k.rpcToken, TmpDir: tmpdir, ChildPython: childPython})
	if err != nil {
		return err
	}
	childEnv["ICEBATH_KERNEL_SENTINEL"] = k.sentinel
	childEnv["ICEBATH_KERNEL_SPILL_DIR"] = tmpdir
	childEnv["ICEBATH_RPC_PERSISTENT"] = "1"
	cmd := exec.Command(childPython, filepath.Join(tmpdir, "icebath_kernel_runner.py"))
	cmd.Dir = childCwd
	if cmd.Dir == "" {
		cmd.Dir = tmpdir
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeFiles(stdinR, stdinW)
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeFiles(stdinR, stdinW, stdoutR, stdoutW)
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	hideConsoleWindow(cmd)
	closeParentHandle := parentProcessHandle(childEnv, cmd)
	cmd.Env = envList(childEnv)
	err = spawnOwned(tools.WithEnvHome(ctx, env), cmd, "code-kernel")
	closeParentHandle()
	closeFiles(stdinR, stdoutW, stderrW)
	if err != nil {
		closeFiles(stdinW, stdoutR, stderrR)
		return err
	}
	k.budget.mu.Lock()
	k.budget.allowed = newRPCBudget(sandboxTools, maxToolCalls).allowed
	k.budget.maxCalls = maxToolCalls
	k.budget.mu.Unlock()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	k.mu.Lock()
	k.stdin = stdinW
	ln := k.listener
	k.mu.Unlock()
	k.proc.Store(&kernelProc{p: cmd.Process, exited: exited})
	go k.rpcForever(env, ln)
	go k.stdoutReader(stdoutR)
	go k.stderrReader(stderrR)
	ensureBackgroundReaper(env)
	return nil
}

// Pops every unattached kernel idle past the timeout, caller holds the registry lock
func popIdleExpired(now time.Time, idleTimeout int) []*sessionKernel {
	var out []*sessionKernel
	for key, k := range registry.kernels {
		if k.attached == 0 && now.Sub(k.lastUsed) > time.Duration(idleTimeout)*time.Second {
			out = append(out, k)
			delete(registry.kernels, key)
		}
	}
	return out
}

// Looks up or registers the kernel for key, sweeping idle kernels and enforcing the unpinned LRU cap
func acquireKernel(env *tools.Env, key kernelKey, reset, pinned bool) (*sessionKernel, bool) {
	limit, idleTimeout := lifecycleLimits(env)
	registry.mu.Lock()
	expired := popIdleExpired(time.Now(), idleTimeout)
	kernel := registry.kernels[key]
	stateReset := kernel != nil && (reset || kernel.dead())
	if stateReset {
		delete(registry.kernels, key)
		if kernel.attached == 0 {
			expired = append(expired, kernel)
		}
		kernel = nil
	}
	if kernel == nil {
		kernel = newSessionKernel(key)
		kernel.pinned = pinned
		registry.kernels[key] = kernel
	}
	kernel.lastUsed = time.Now()
	kernel.attached++
	unpinned := 0
	var byAge []kernelKey
	for k, v := range registry.kernels {
		if v.pinned {
			continue
		}
		unpinned++
		if k != key && v.attached == 0 {
			byAge = append(byAge, k)
		}
	}
	sort.SliceStable(byAge, func(i, j int) bool {
		return registry.kernels[byAge[i]].lastUsed.Before(registry.kernels[byAge[j]].lastUsed)
	})
	for _, k := range byAge[:min(len(byAge), max(0, unpinned-limit))] {
		expired = append(expired, registry.kernels[k])
		delete(registry.kernels, k)
	}
	registry.mu.Unlock()
	for _, doomed := range expired {
		doomed.teardown()
	}
	return kernel, stateReset
}

var reaperStarted bool

// Removes staging dirs untouched for over a week, never chasing a symlink, returning the count
func sweepStaleStagingDirs(now time.Time) (int, error) {
	tempRoot, err := gettempdir()
	if err != nil {
		return 0, err
	}
	matches, _ := filepath.Glob(filepath.Join(globEscape(tempRoot), kernelStagingPrefix+"*"))
	removed := 0
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || now.Sub(info.ModTime()) <= staleStagingDirAge {
			continue
		}
		if linfo, err := os.Lstat(path); err != nil || !linfo.IsDir() {
			continue
		}
		if os.RemoveAll(path) != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

func globEscape(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`*?[\`, r) && !(r == '\\' && runtime.GOOS == "windows") {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// One background pass with the acquire path idle criteria, then the stale staging dir sweep
func reapOnce(env *tools.Env) error {
	_, idleTimeout := lifecycleLimits(env)
	registry.mu.Lock()
	expired := popIdleExpired(time.Now(), idleTimeout)
	registry.mu.Unlock()
	for _, doomed := range expired {
		doomed.teardown()
	}
	_, err := sweepStaleStagingDirs(time.Now())
	return err
}

// Starts the idle reaper goroutine once per process on the first kernel spawn
func ensureBackgroundReaper(env *tools.Env) {
	registry.mu.Lock()
	if reaperStarted {
		registry.mu.Unlock()
		return
	}
	reaperStarted = true
	registry.mu.Unlock()
	go backgroundReaper(env)
}

func backgroundReaper(env *tools.Env) {
	for {
		_, idleTimeout := lifecycleLimits(env)
		time.Sleep(time.Duration(min(reaperIntervalCeil, max(reaperIntervalFloor, float64(idleTimeout)/6.0)) * float64(time.Second)))
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger(env).Error("kernel idle reaper pass failed; retrying next interval", "error", r)
				}
			}()
			if err := reapOnce(env); err != nil {
				logger(env).Error("kernel idle reaper pass failed; retrying next interval", "error", err)
			}
		}()
	}
}

// Python AttributeError text a non dict reply raises when the cell result reads it
type replyShapeError struct{ value any }

func (e replyShapeError) Error() string {
	return fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(e.value))
}

// Waits for the cell reply polling interrupt and deadline, returning the host status and payload
func awaitCell(kernel *sessionKernel, timeout float64, isInterrupted func() bool) (string, *pyjson.Object, error) {
	var deadline time.Time
	if timeout != 0 {
		deadline = time.Now().Add(time.Duration(timeout * float64(time.Second)))
	}
	for {
		if isInterrupted() {
			return "interrupted", pyjson.NewObject(), nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return "timeout", pyjson.NewObject(), nil
		}
		v, ok := kernel.responses.get(50 * time.Millisecond)
		if !ok {
			continue
		}
		payload, isObj := v.(*pyjson.Object)
		if !isObj {
			return "", nil, replyShapeError{v}
		}
		if s, _ := payload.Get("status"); s == "kernel-eof" || s == "protocol-error" {
			return "error", payload, nil
		}
		return "success", payload, nil
	}
}

func withStderr(stdoutText, stderrText string) string {
	return stdoutText + "\n--- stderr ---\n" + stderrText
}

func payloadStr(payload *pyjson.Object, key string) string {
	if v, ok := payload.Get(key); ok {
		return pyrepr.Str(v)
	}
	return ""
}

// Python round(x, 2) of the seconds since start
func elapsedSeconds(start time.Time) float64 { return roundTo(time.Since(start).Seconds(), 2) }

type cellRun struct {
	ctx          context.Context
	env          *tools.Env
	timeoutText  string
	sandboxTools []string
	reused       bool
	stateReset   bool
	execStart    time.Time
}

// Assembles the tool result of one settled cell, disposing the kernel where the contract says so
func cellResult(kernel *sessionKernel, key kernelKey, status string, payload *pyjson.Object, run cellRun) (*pyjson.Object, error) {
	clean := func(text string) string {
		return tools.RedactSensitiveTextWith(run.env, tools.StripANSI(text), tools.RedactOptions{CodeFile: true})
	}
	if status == "timeout" || status == "interrupted" {
		registry.discard(key, kernel)
	}
	duration := elapsedSeconds(run.execStart)
	count, ok := payload.Get("execution_count")
	if !ok {
		prev, err := pyint.Of(kernel.executionCount)
		if err != nil {
			return nil, err
		}
		count = pyint.Value(new(big.Int).Add(prev, big.NewInt(1)))
	}
	n, err := pyint.Of(count)
	if err != nil {
		return nil, err
	}
	kernel.executionCount = pyint.Value(n)
	stderrRaw := kernel.stderr.drain()
	stdoutText, stdoutMeta := truncateStdoutText(run.env, clean(payloadStr(payload, "stdout")+kernel.raw.drain()))
	cellStderr := clean(payloadStr(payload, "stderr") + stderrRaw)
	cellStatus, _ := payload.Get("status")
	kernelInfo := pyjson.NewObject()
	kernelInfo.Set("mode", "session")
	kernelInfo.Set("reused", run.reused)
	kernelInfo.Set("execution_count", kernel.executionCount)
	kernelInfo.Set("state_reset", run.stateReset)
	result := pyjson.NewObject()
	result.Set("status", status)
	result.Set("output", stdoutText)
	result.Set("exit_code", 0)
	result.Set("tool_calls_made", kernel.budget.callCount())
	result.Set("duration_seconds", duration)
	result.Set("kernel", kernelInfo)
	if stdoutMeta != nil {
		result.Update(stdoutMeta)
	}
	if toolErrors := toolErrorsSince(kernel.budget.entries(), kernel.cellLogStart); len(toolErrors) > 0 {
		result.Set("tool_errors", toolErrors)
	}
	cellSpill := ""
	if v, ok := payload.Get("stdout_spill_path"); ok && pyrepr.Truthy(v) {
		cellSpill = pyrepr.Str(v)
	}
	if clipped, _ := payload.Get("stdout_clipped"); cellSpill != "" && pyrepr.Truthy(clipped) {
		result.Set("stdout_spill_path", cellSpill)
		result.Set("warning", fmt.Sprintf("Cell stdout exceeded the inline cap; head shown. FULL output saved to %s "+
			"— page it with read_file(path=\"%s\", offset=...) instead of re-running. "+
			"(Kernel state persists: printing a narrower slice next call is often cheaper.)", cellSpill, cellSpill))
	}
	switch {
	case status == "timeout":
		message := fmt.Sprintf("Cell timed out after %ss; the session kernel was killed and its "+
			"state was lost. The next execute_code call starts a fresh kernel.", run.timeoutText)
		result.Set("exit_code", -1)
		result.Set("error", message)
		if stdoutText != "" {
			result.Set("output", stdoutText+"\n\n⏰ "+message)
		} else {
			result.Set("output", "⏰ "+message)
		}
	case status == "interrupted":
		result.Set("exit_code", -1)
		result.Set("output", formatInterruptedOutput(run.ctx, stdoutText))
		result.Set("error", "Interrupted; the session kernel was killed and its state was lost.")
	case cellStatus == "error":
		trace := clean(payloadStr(payload, "traceback"))
		result.Set("status", "error")
		result.Set("exit_code", 1)
		if trace != "" {
			result.Set("error", trace)
		} else {
			result.Set("error", "Cell raised an exception.")
		}
		if cellStderr != "" || trace != "" {
			result.Set("output", withStderr(stdoutText, cellStderr+trace))
		} else {
			result.Set("output", stdoutText)
		}
		if hint := sandboxFailureHint(trace, run.sandboxTools); hint != "" {
			result.Set("hint", hint)
		}
	case cellStatus == "exit":
		registry.discard(key, kernel)
		kernelInfo.Set("ended", true)
		if cellStderr != "" {
			result.Set("output", withStderr(stdoutText, cellStderr))
		}
	case status == "error":
		registry.discard(key, kernel)
		result.Set("exit_code", -1)
		message := "The session kernel died while running the cell"
		if s := pystr.Strip(stderrRaw); s != "" {
			message += ": " + s
		} else {
			message += "."
		}
		result.Set("error", message)
	case cellStderr != "":
		result.Set("output", withStderr(stdoutText, cellStderr))
	}
	return result, nil
}

// Options of one session kernel cell
type kernelCell struct {
	taskID, mode, childPython, childCwd string
	sandboxTools                        []string
	timeout                             float64
	timeoutText                         string
	maxToolCalls                        int
	reset                               bool
	isInterrupted                       func() bool
}

// Runs one cell in the owner, mode, python, cwd and tool set kernel so state survives across turns
func executeInSessionKernel(ctx context.Context, env *tools.Env, code string, c kernelCell) string {
	sorted := slices.Sorted(slices.Values(c.sandboxTools))
	key := kernelKey{owner: resolveOwner(ctx, env, c.taskID), mode: c.mode, python: c.childPython, cwd: c.childCwd, tools: strings.Join(sorted, "\x00")}
	execStart := time.Now()
	kernel, stateReset := acquireKernel(env, key, c.reset, tools.IsDelegatedChildContext(ctx))
	defer func() {
		registry.mu.Lock()
		kernel.attached--
		kernel.lastUsed = time.Now()
		orphaned := kernel.attached == 0 && registry.kernels[key] != kernel
		registry.mu.Unlock()
		if orphaned {
			kernel.teardown()
		}
	}()
	return runCell(ctx, env, kernel, key, code, c, sorted, execStart, stateReset)
}

func runCell(ctx context.Context, env *tools.Env, kernel *sessionKernel, key kernelKey, code string, c kernelCell, sandboxTools []string, execStart time.Time, stateReset bool) string {
	reused := kernel.proc.Load() != nil
	authority := newCellAuthority(ctx, env, c.taskID)
	kernel.lock.Lock()
	defer kernel.lock.Unlock()
	defer authority.retire()
	result, err := func() (string, error) {
		if kernel.proc.Load() == nil {
			if err := kernel.spawn(ctx, env, c.childPython, c.childCwd, sandboxTools, c.maxToolCalls); err != nil {
				return "", err
			}
		}
		kernel.budget.mu.Lock()
		kernel.budget.calls = 0
		kernel.cellLogStart = len(kernel.budget.log)
		kernel.budget.mu.Unlock()
		kernel.raw.drain()
		kernel.stderr.drain()
		kernel.authority.Store(authority)
		request := pyjson.NewObject()
		request.Set("id", uuid4Hex())
		request.Set("code", code)
		line, err := pyjson.Dumps(request)
		if err != nil {
			return "", err
		}
		kernel.mu.Lock()
		stdin := kernel.stdin
		kernel.mu.Unlock()
		if stdin == nil {
			return "", errors.New("[Errno 32] Broken pipe")
		}
		if _, err := stdin.Write([]byte(line + "\n")); err != nil {
			return "", err
		}
		status, payload, err := awaitCell(kernel, c.timeout, c.isInterrupted)
		if err != nil {
			return "", err
		}
		res, err := cellResult(kernel, key, status, payload, cellRun{ctx: ctx, env: env, timeoutText: c.timeoutText,
			sandboxTools: sandboxTools, reused: reused, stateReset: stateReset, execStart: execStart})
		if err != nil {
			return "", err
		}
		return pyjson.Dumps(res, pyjson.EnsureASCII(false))
	}()
	if err == nil {
		return result
	}
	logger(env).Error(fmt.Sprintf("session kernel failed: %s: %s", errorTypeName(err), err.Error()))
	registry.discard(key, kernel)
	return errorResult(err.Error(), kernel.budget.callCount(), elapsedSeconds(execStart), "")
}

func errorTypeName(err error) string {
	var shape replyShapeError
	if errors.As(err, &shape) {
		return "AttributeError"
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "OSError"
	}
	return fmt.Sprintf("%T", err)
}

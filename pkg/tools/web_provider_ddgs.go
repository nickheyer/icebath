package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Hard wall clock cap per search in seconds, kept as the Python value so messages print it the same way
var ddgsSearchTimeoutSecs any = 30

// Parent poll cadence for worker output and the interrupt flag
var ddgsPollInterval = 100 * time.Millisecond

// Wait after terminate before escalating to kill, in seconds
var ddgsTerminateGraceSecs = 1.0

// Test only hook forwarded to the worker
var ddgsTestHook string

// Last worker started, read by the reap checks of the tests
var ddgsLastWorker atomic.Pointer[ddgsWorkerProc]

// Interrupt probe of the wait loop, the worker interrupt bit or a cancelled context
var ddgsIsInterrupted = func(ctx context.Context) bool { return IsInterrupted(ctx) || ctx.Err() != nil }

// Hidden first argument a main or TestMain routes to the DDGS worker through DispatchDDGSWorkerEntry
const DDGSWorkerEntry = "__icebath_ddgs_worker__"

// Latch proving this binary dispatches the worker entry, the only state in which a search may re-execute it
var ddgsWorkerDispatched atomic.Bool

// DispatchDDGSWorkerEntry runs the worker and exits when argv carries the entry, otherwise records that this binary handles it
func DispatchDDGSWorkerEntry() {
	ddgsWorkerDispatched.Store(true)
	if len(os.Args) > 1 && os.Args[1] == DDGSWorkerEntry {
		os.Exit(ddgsWorkerMain(context.Background(), os.Stdin, os.Stdout, os.Getenv))
	}
}

// ddgsTimeoutError is the TimeoutError of a search past the wall clock cap
type ddgsTimeoutError struct{ msg string }

func (e *ddgsTimeoutError) Error() string { return e.msg }

// ddgsInterruptedError is raised when the interrupt flag trips during a search wait
type ddgsInterruptedError struct{ msg string }

func (e *ddgsInterruptedError) Error() string { return e.msg }

// Seconds of a Python number setting
func pySeconds(v any) time.Duration {
	f, err := pyint.Float(v)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// ddgsWorkerProc is a started worker whose exit is observed without blocking
type ddgsWorkerProc struct {
	proc   *os.Process
	exited chan struct{}
	state  *os.ProcessState
}

// Reports whether the worker has exited, Popen.poll() is not None
func (p *ddgsWorkerProc) poll() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// Popen.returncode text, None while running and the negated signal for a signalled exit
func (p *ddgsWorkerProc) returncode() string {
	if !p.poll() || p.state == nil {
		return "None"
	}
	if ws, ok := p.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return fmt.Sprint(-int(ws.Signal()))
	}
	return fmt.Sprint(p.state.ExitCode())
}

// Terminates a worker, escalates to kill and waits so no orphan remains
func ddgsTerminateAndReap(p *ddgsWorkerProc, grace time.Duration) {
	if p == nil {
		return
	}
	alive := false
	waitUntilDead := func() bool {
		deadline := time.Now().Add(grace)
		for !p.poll() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		return p.poll()
	}
	for _, escalate := range []func(*os.Process) error{ddgsTerminate, (*os.Process).Kill} {
		if p.poll() {
			continue
		}
		if err := escalate(p.proc); err != nil && !errors.Is(err, os.ErrProcessDone) {
			envLog(nil).Debug(fmt.Sprintf("DDGS worker reap error: %s", err))
			return
		}
		alive = !waitUntilDead()
	}
	if alive {
		envLog(nil).Warn(fmt.Sprintf("DDGS worker pid=%d did not exit after kill", p.proc.Pid))
	}
}

// Starts this binary as the worker in its own session with piped stdin and stdout and a silent stderr
func ddgsSpawnWorker(childEnv map[string]string) (*ddgsWorkerProc, *os.File, *os.File, error) {
	if !ddgsWorkerDispatched.Load() {
		panic("ddgs search would re-execute a binary that never called tools.DispatchDDGSWorkerEntry, call it first in main or TestMain")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, nil, err
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, nil, nil, err
	}
	cmd := exec.Command(exe, DDGSWorkerEntry)
	cmd.Stdin, cmd.Stdout = stdinR, stdoutW
	cmd.SysProcAttr = ddgsWorkerAttr()
	for k, v := range childEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	err = cmd.Start()
	stdinR.Close()
	stdoutW.Close()
	if err != nil {
		stdinW.Close()
		stdoutR.Close()
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, nil, nil, raise("OSError", "%s: %s", pyhttpx.OSErrorText(pe.Err), pyrepr.StrRepr(exe))
		}
		return nil, nil, nil, err
	}
	p := &ddgsWorkerProc{proc: cmd.Process, exited: make(chan struct{})}
	go func() {
		st, _ := cmd.Process.Wait()
		p.state = st
		close(p.exited)
	}()
	return p, stdinW, stdoutR, nil
}

// Decodes the worker's stdout envelope, a RuntimeError for any malformed shape
func ddgsParseEnvelope(raw string, p *ddgsWorkerProc) ([]WebSearchHit, error) {
	raw = pystr.Strip(raw)
	if raw == "" {
		return nil, raise("RuntimeError", "DDGS worker exited without a result (code=%s)", p.returncode())
	}
	envelope, err := pyjson.Loads(raw)
	if err != nil {
		return nil, raise("RuntimeError", "DDGS worker returned invalid JSON: %s", pyrepr.StrRepr(pystr.Slice(raw, 0, 200)))
	}
	d, ok := envelope.(*pyjson.Object)
	if !ok {
		return nil, raise("RuntimeError", "DDGS worker returned an invalid envelope: %s", pyrepr.Repr(envelope))
	}
	okv, _ := d.Get("ok")
	if !pyTruthy(okv) {
		errv, _ := d.Get("error")
		return nil, raise("RuntimeError", "%s", pyrepr.Str(pyOr(errv, "DDGS worker failed")))
	}
	rawResults, _ := d.Get("results")
	items, ok := pyOr(rawResults, []any{}).([]any)
	if !ok {
		return nil, raise("RuntimeError", "DDGS worker returned non-list results")
	}
	hits := make([]WebSearchHit, 0, len(items))
	for _, item := range items {
		row, _ := pyDict(item)
		if row == nil {
			row = pyjson.NewObject()
		}
		title, _ := row.Get("title")
		u, _ := row.Get("url")
		description, _ := row.Get("description")
		position, _ := row.Get("position")
		pos, _ := pyint.Of(position)
		n := 0
		if pos != nil && pos.IsInt64() {
			n = int(pos.Int64())
		}
		hits = append(hits, titleHit(pyStrField(title), pyStrField(u), pyStrField(description), n))
	}
	return hits, nil
}

// Runs the search in a disposable worker with a hard deadline, killing it on timeout or interrupt
func ddgsSearchBounded(ctx context.Context, env *Env, query string, safeLimit int) ([]WebSearchHit, error) {
	request := pyObject("query", query, "safe_limit", safeLimit)
	childEnv, err := SanitizeSubprocessEnv(env, environMap(), nil)
	if err != nil {
		return nil, err
	}
	if ddgsTestHook != "" {
		request.Set("test_hook", ddgsTestHook)
		childEnv["ICEBATH_DDGS_ALLOW_TEST_HOOKS"] = "1"
	}
	payload, err := pyjson.Dumps(request)
	if err != nil {
		return nil, err
	}
	p, stdin, stdout, err := ddgsSpawnWorker(childEnv)
	if err != nil {
		return nil, err
	}
	ddgsLastWorker.Store(p)
	result := make(chan string, 1)
	go func() {
		_, _ = io.WriteString(stdin, payload)
		stdin.Close()
		out, _ := io.ReadAll(stdout)
		stdout.Close()
		<-p.exited
		result <- pystr.TranslateNewlines(pystr.DecodeUTF8Replace(out))
	}()
	grace := time.Duration(ddgsTerminateGraceSecs * float64(time.Second))
	interrupted, done, raw := false, false, ""
	deadline := time.Now().Add(pySeconds(ddgsSearchTimeoutSecs))
	for !done {
		if interrupted = ddgsIsInterrupted(ctx); interrupted {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case raw = <-result:
			done = true
		case <-time.After(min(ddgsPollInterval, remaining)):
		}
	}
	ddgsTerminateAndReap(p, grace)
	if !done {
		select {
		case r := <-result:
			if raw == "" {
				raw = r
			}
		case <-time.After(grace):
		}
	}
	if interrupted {
		return nil, &ddgsInterruptedError{"DuckDuckGo search interrupted"}
	}
	if !done {
		return nil, &ddgsTimeoutError{fmt.Sprintf("DuckDuckGo search timed out after %ss", pyrepr.Str(ddgsSearchTimeoutSecs))}
	}
	return ddgsParseEnvelope(raw, p)
}

// Bounded search seam the tests route in process
var ddgsSearchBoundedFn = ddgsSearchBounded

// Text results of one ddgs query, the seam the in process tests replace with a fake package
var ddgsText = func(ctx context.Context, query string, maxResults int) ([]*ddgsTextResult, error) {
	return newDDGSClient(10).text(ctx, ddgsQuery{query: query, region: "us-en", safesearch: "moderate", page: 1}, maxResults)
}

var ddgsImported atomic.Bool

// True once the ddgs extra is importable in the managed environment, a success remembered like sys.modules
var ddgsPackageImportable = func(ctx context.Context) bool {
	if ddgsImported.Load() {
		return true
	}
	ok, err := pm.Available(ctx, "ddgs")
	if err != nil || !ok {
		return false
	}
	ddgsImported.Store(true)
	return true
}

type ddgsProvider struct{ baseWebProvider }

// Available reflects whether the ddgs extra is installed, never touching the network
func (p *ddgsProvider) Available(env *Env) bool {
	return ddgsPackageImportable(context.Background())
}

// Search in a disposable worker with a hard wall clock timeout, every failure a search error result
func (p *ddgsProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	if !p.Available(env) {
		return searchFail("ddgs package is not installed — run `pip install ddgs`"), nil
	}
	hits, err := ddgsSearchBoundedFn(ctx, env, query, max(1, limit))
	if err != nil {
		var te *ddgsTimeoutError
		var ie *ddgsInterruptedError
		switch {
		case errors.As(err, &te):
			envLog(env).Warn(fmt.Sprintf("DDGS search timed out after %ds for query: %s", int(pySeconds(ddgsSearchTimeoutSecs)/time.Second), pyrepr.StrRepr(query)))
			return searchFail(fmt.Sprintf("DuckDuckGo search timed out after %ss — DuckDuckGo may be rate-limiting or slow. Try again later or switch to a different search provider.", pyrepr.Str(ddgsSearchTimeoutSecs))), nil
		case errors.As(err, &ie):
			envLog(env).Info(fmt.Sprintf("DDGS search interrupted for query: %s", pyrepr.StrRepr(query)))
			return searchFail("DuckDuckGo search interrupted"), nil
		}
		envLog(env).Warn(fmt.Sprintf("DDGS search error: %s", err))
		return searchFail(fmt.Sprintf("DuckDuckGo search failed: %s", err)), nil
	}
	envLog(env).Info(fmt.Sprintf("DDGS search '%s': %d results (limit %d)", query, len(hits), limit))
	return searchOK(hits), nil
}

// SetupSchema is the icebath tools picker entry, post setup installing the ddgs extra
func (p *ddgsProvider) SetupSchema() map[string]any {
	return setupSchema("DuckDuckGo (ddgs)", "free · no key · search only", "Search via the ddgs Python package — no API key (pair with any extract provider)", "", "", "", map[string]any{"post_setup": "ddgs"})
}

// Worker entrypoint reading one request from stdin and writing one envelope to stdout
func ddgsWorkerMain(ctx context.Context, in io.Reader, out io.Writer, getenv func(string) string) int {
	write := func(envelope *pyjson.Object) {
		text, _ := pyjson.Dumps(envelope)
		_, _ = io.WriteString(out, text)
	}
	fail := func(msg string, code int) int {
		write(pyObject("ok", false, "error", msg))
		return code
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return fail("invalid request: "+err.Error(), 2)
	}
	if pos, ok := pystr.ValidIndex(data); !ok {
		return fail("invalid request: "+pystr.UTF8DecodeError(data, pos).Error(), 2)
	}
	request, err := pyjson.Loads(string(data))
	if err != nil {
		return fail("invalid request: "+err.Error(), 2)
	}
	d, ok := request.(*pyjson.Object)
	if !ok {
		return 1
	}
	if hook, _ := d.Get("test_hook"); pyTruthy(hook) {
		if getenv("ICEBATH_DDGS_ALLOW_TEST_HOOKS") != "1" {
			return fail("test_hook refused (hooks not enabled)", 3)
		}
		var envelope *pyjson.Object
		switch name := pyrepr.Str(hook); name {
		case "sleep", "gil":
			time.Sleep(30 * time.Second)
			envelope = pyObject("ok", false, "error", name+" hook returned unexpectedly")
		case "empty":
			envelope = pyObject("ok", true, "results", []any{})
		default:
			envelope = pyObject("ok", false, "error", "unknown test_hook: "+pyrepr.Repr(hook))
		}
		write(envelope)
		if okv, _ := envelope.Get("ok"); pyTruthy(okv) {
			return 0
		}
		return 1
	}
	rawQuery, _ := d.Get("query")
	query := pyrepr.Str(pyOr(rawQuery, ""))
	rawLimit, _ := d.Get("safe_limit")
	n, err := pyint.Of(pyOr(rawLimit, 1))
	if err != nil {
		return 1
	}
	safeLimit := math.MaxInt
	if n.IsInt64() && n.Int64() < math.MaxInt {
		safeLimit = int(n.Int64())
	}
	safeLimit = max(1, safeLimit)
	hits, err := runDDGSSearch(ctx, query, safeLimit)
	if err != nil {
		return fail(pyExcClass(err)+": "+err.Error(), 1)
	}
	rows := make([]any, 0, len(hits))
	for _, h := range hits {
		rows = append(rows, pyObject("title", h.Title, "url", h.URL, "description", h.Description, "position", h.Position))
	}
	write(pyObject("ok", true, "results", rows))
	return 0
}

func init() {
	RegisterWebProvider(&ddgsProvider{baseWebProvider{name: "ddgs", display: "DuckDuckGo (ddgs)"}})
}

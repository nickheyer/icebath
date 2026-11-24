package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Hard wall clock cap per search in seconds, kept as the Python value so messages print it the same way
var ddgsSearchTimeoutSecs any = 30

// Parent poll cadence for worker output and the interrupt flag
var ddgsPollInterval = 100 * time.Millisecond

// Wait after terminate before escalating to kill, in seconds
var ddgsTerminateGraceSecs = 1.0

// Reap grace bound when _terminate_and_reap was defined, the default a later patch of the grace never reaches
const ddgsReapGrace = time.Second

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
	f, err := anyutil.ToFloat(v)
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
			return nil, nil, nil, raise("OSError", "%s: %s", errchain.OSText(pe.Err), strconv.Quote(exe))
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
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, raise("RuntimeError", "DDGS worker exited without a result (code=%s)", p.returncode())
	}
	envelope, err := jsonutil.Decode(raw)
	if err != nil {
		return nil, raise("RuntimeError", "DDGS worker returned invalid JSON: %s", strconv.Quote(textutil.First(raw, 200)))
	}
	d, ok := envelope.(*jsonutil.Object)
	if !ok {
		return nil, raise("RuntimeError", "DDGS worker returned an invalid envelope: %s", anyutil.Quote(envelope))
	}
	okv, _ := d.Get("ok")
	if !anyutil.IsTrue(okv) {
		errv, _ := d.Get("error")
		return nil, raise("RuntimeError", "%s", anyutil.String(anyutil.Or(errv, "DDGS worker failed")))
	}
	rawResults, _ := d.Get("results")
	items, ok := anyutil.Or(rawResults, []any{}).([]any)
	if !ok {
		return nil, raise("RuntimeError", "DDGS worker returned non-list results")
	}
	hits := make([]WebSearchHit, 0, len(items))
	for _, item := range items {
		row, _ := pyDict(item)
		if row == nil {
			row = jsonutil.NewObject()
		}
		title, _ := row.Get("title")
		u, _ := row.Get("url")
		description, _ := row.Get("description")
		position, _ := row.Get("position")
		n := 0
		if pos, err := anyutil.ToInt(position); err == nil {
			n = int(pos)
		}
		hits = append(hits, titleHit(anyutil.String(title), anyutil.String(u), anyutil.String(description), n))
	}
	return hits, nil
}

// Runs the search in a disposable worker with a hard deadline, killing it on timeout or interrupt
func ddgsSearchBounded(ctx context.Context, env *Env, query string, safeLimit int) ([]WebSearchHit, error) {
	request := pyObject("query", query, "safe_limit", safeLimit)
	childEnv, err := SanitizeSubprocessEnv(ctx, env, environMap(), nil)
	if err != nil {
		return nil, err
	}
	if ddgsTestHook != "" {
		request.Set("test_hook", ddgsTestHook)
		childEnv["ICEBATH_DDGS_ALLOW_TEST_HOOKS"] = "1"
	}
	payload, err := jsonutil.Encode(request, jsonutil.Spaced(), jsonutil.ASCII())
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
		result <- textutil.NormalizeNewlines(strings.ToValidUTF8(string(out), "\uFFFD"))
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
	ddgsTerminateAndReap(p, ddgsReapGrace)
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
		return nil, &ddgsTimeoutError{fmt.Sprintf("DuckDuckGo search timed out after %ss", anyutil.String(ddgsSearchTimeoutSecs))}
	}
	return ddgsParseEnvelope(raw, p)
}

// Bounded search seam the tests route in process
var ddgsSearchBoundedFn = ddgsSearchBounded

// Text results of one ddgs query, the seam the in process tests replace with a fake package
var ddgsText = func(ctx context.Context, query string, maxResults int) ([]*ddgsTextResult, error) {
	return newDDGSClient(10).text(ctx, ddgsQuery{query: query, region: "us-en", safesearch: "moderate", page: 1}, maxResults)
}

// Whether ddgs is installed in the reference sense, once config selects it as the reference tools setup installs its extra
var ddgsClientAvailable = func(env *Env) bool { return webConfigSelects(env, "ddgs") }

type ddgsProvider struct{ baseWebProvider }

// Available reports the ddgs backend selected in config, never touching the network
func (p *ddgsProvider) Available(env *Env) bool {
	return ddgsClientAvailable(env)
}

// Search in a disposable worker with a hard wall clock timeout, every failure a search error result
func (p *ddgsProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	hits, err := ddgsSearchBoundedFn(ctx, env, query, max(1, limit))
	if err != nil {
		var te *ddgsTimeoutError
		var ie *ddgsInterruptedError
		switch {
		case errors.As(err, &te):
			envLog(env).Warn(fmt.Sprintf("DDGS search timed out after %ds for query: %s", int(pySeconds(ddgsSearchTimeoutSecs)/time.Second), strconv.Quote(query)))
			return searchFail(fmt.Sprintf("DuckDuckGo search timed out after %ss — DuckDuckGo may be rate-limiting or slow. Try again later or switch to a different search provider.", anyutil.String(ddgsSearchTimeoutSecs))), nil
		case errors.As(err, &ie):
			envLog(env).Info(fmt.Sprintf("DDGS search interrupted for query: %s", strconv.Quote(query)))
			return searchFail("DuckDuckGo search interrupted"), nil
		}
		envLog(env).Warn(fmt.Sprintf("DDGS search error: %s", err))
		return searchFail(fmt.Sprintf("DuckDuckGo search failed: %s", err)), nil
	}
	envLog(env).Info(fmt.Sprintf("DDGS search '%s': %d results (limit %d)", query, len(hits), limit))
	return searchOK(hits), nil
}

// SetupSchema is the icebath tools picker entry, nothing to install for the built in client
func (p *ddgsProvider) SetupSchema() map[string]any {
	return setupSchema("DuckDuckGo (ddgs)", "free · no key · search only", "Search via the built-in DuckDuckGo client — no API key (pair with any extract provider)", "", "", "", nil)
}

// Worker entrypoint reading one request from stdin and writing one envelope to stdout
func ddgsWorkerMain(ctx context.Context, in io.Reader, out io.Writer, getenv func(string) string) int {
	write := func(envelope *jsonutil.Object) {
		text, _ := jsonutil.Encode(envelope, jsonutil.Spaced(), jsonutil.ASCII())
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
	if err := textutil.ValidateUTF8(data); err != nil {
		return fail("invalid request: "+err.Error(), 2)
	}
	request, err := jsonutil.Decode(data)
	if err != nil {
		return fail("invalid request: "+err.Error(), 2)
	}
	d, ok := request.(*jsonutil.Object)
	if !ok {
		return 1
	}
	if hook, _ := d.Get("test_hook"); anyutil.IsTrue(hook) {
		if getenv("ICEBATH_DDGS_ALLOW_TEST_HOOKS") != "1" {
			return fail("test_hook refused (hooks not enabled)", 3)
		}
		var envelope *jsonutil.Object
		switch name := anyutil.String(hook); name {
		case "sleep", "gil":
			time.Sleep(30 * time.Second)
			envelope = pyObject("ok", false, "error", name+" hook returned unexpectedly")
		case "empty":
			envelope = pyObject("ok", true, "results", []any{})
		default:
			envelope = pyObject("ok", false, "error", "unknown test_hook: "+anyutil.Quote(hook))
		}
		write(envelope)
		if okv, _ := envelope.Get("ok"); anyutil.IsTrue(okv) {
			return 0
		}
		return 1
	}
	rawQuery, _ := d.Get("query")
	query := anyutil.String(anyutil.Or(rawQuery, ""))
	rawLimit, _ := d.Get("safe_limit")
	n, err := anyutil.ToInt(anyutil.Or(rawLimit, 1))
	if err != nil && !errors.Is(err, anyutil.ErrRange) {
		return 1
	}
	safeLimit := math.MaxInt
	if n < math.MaxInt {
		safeLimit = int(n)
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

// DuckDuckGo search provider the web/ddgs plugin registers
func NewDDGSWebProvider() WebProvider {
	return &ddgsProvider{baseWebProvider{name: "ddgs", display: "DuckDuckGo (ddgs)"}}
}

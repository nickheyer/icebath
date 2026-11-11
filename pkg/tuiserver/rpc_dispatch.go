package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Set of the given names
func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// Whether a method runs on the worker pool instead of the reader
func isLongHandler(method string) bool {
	longHandlersMu.RLock()
	defer longHandlersMu.RUnlock()
	return longHandlers[method]
}

// Queued pool job, its done callback run once whether it ran or was cancelled
type rpcFuture struct {
	fn     func()
	onDone func()
	done   chan struct{}
}

// Marks the job done and runs its callback
func (f *rpcFuture) finish() {
	close(f.done)
	if f.onDone != nil {
		f.onDone()
	}
}

// Pool that runs submitted jobs in arrival order on at most max workers, started on demand
type rpcWorkerPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	max      int
	workers  int
	idle     int
	queue    []*rpcFuture
	shutdown bool
}

// Pool capped at max workers
func newRPCWorkerPool(max int) *rpcWorkerPool {
	p := &rpcWorkerPool{max: max}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Error of a submit after shutdown, the RuntimeError of the executor
var errPoolShutdown = errors.New("cannot schedule new futures after shutdown")

// Queues fn, starting a worker when none idles and the cap allows, onDone running when it ends or is cancelled
func (p *rpcWorkerPool) Submit(fn func(), onDone func()) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdown {
		return errPoolShutdown
	}
	p.queue = append(p.queue, &rpcFuture{fn: fn, onDone: onDone, done: make(chan struct{})})
	if p.idle > 0 {
		p.cond.Signal()
		return nil
	}
	if p.workers < p.max {
		p.workers++
		go p.work()
	}
	return nil
}

// Worker loop taking the oldest job until the pool shuts down
func (p *rpcWorkerPool) work() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.shutdown {
			p.idle++
			p.cond.Wait()
			p.idle--
		}
		if len(p.queue) == 0 {
			p.workers--
			p.mu.Unlock()
			return
		}
		job := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		func() {
			defer job.finish()
			job.fn()
		}()
	}
}

// Refuses new jobs, cancels the queued ones when asked and lets running jobs finish
func (p *rpcWorkerPool) Shutdown(cancelFutures bool) {
	p.mu.Lock()
	p.shutdown = true
	var cancelled []*rpcFuture
	if cancelFutures {
		cancelled, p.queue = p.queue, nil
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	for _, job := range cancelled {
		job.finish()
	}
}

// Worker pool of the long handlers, swapped only where a hermes case installs its own executor
var rpcPool = newRPCWorkerPool(max(2, envInt("ICEBATH_TUI_RPC_POOL_WORKERS", 8)))

func init() {
	registerAtExit(func() { rpcPool.Shutdown(true) })
}

// Request as a plain map tree, a decoded JSON object converted and anything else unchanged
func plainRequest(req any) any {
	if obj, isObject := req.(*pyjson.Object); isObject && obj != nil {
		return pyjson.Plain(obj)
	}
	return req
}

// Ordered JSON object of a request or response, a plain map ordered like the Python dict literal
func frameObject(frame any) *pyjson.Object {
	switch t := frame.(type) {
	case *pyjson.Object:
		return t
	case map[string]any:
		out := pyjson.NewObject()
		for _, key := range []string{"jsonrpc", "id", "method", "params", "result", "error"} {
			if value, present := t[key]; present {
				out.Set(key, pyjsonValue(value))
			}
		}
		for _, key := range sortedKeys(t) {
			if _, present := out.Get(key); !present {
				out.Set(key, pyjsonValue(t[key]))
			}
		}
		return out
	}
	return nil
}

// JSON value with every plain map turned into an ordered object
func pyjsonValue(v any) any {
	if m, isMap := v.(map[string]any); isMap {
		return objectOf(m)
	}
	return v
}

// Answers one request inside the retirement fence, 5035 once the backend is retiring
func handleRequest(ctx context.Context, req any) map[string]any {
	admitted, done := provider.Retirement.Work()
	defer done()
	if !admitted {
		return rpcErr(requestID(req), 5035, "backend is retiring; reconnect to continue", nil)
	}
	return handleAdmittedRequest(ctx, req)
}

// Id member of a request object, nil for anything else
func requestID(req any) any {
	switch t := plainRequest(req).(type) {
	case map[string]any:
		return t["id"]
	}
	return nil
}

// Runs the handler of a normalized request under its contract, mapping profile and config outages to 4064
func handleAdmittedRequest(ctx context.Context, req any) map[string]any {
	rid, method, params, errResp := normalizeRequest(plainRequest(req))
	if errResp != nil {
		return errResp
	}
	fn := lookupMethod(method)
	if fn == nil {
		return rpcErr(rid, -32601, tuigateway.UnknownMethodMessage(method), nil)
	}
	contract := tuigateway.Methods[method]
	if contract != nil {
		validated, problem := tuigateway.ValidateParams(contract, params)
		if problem != "" {
			return rpcErr(rid, 4000, problem, nil)
		}
		params, _ = validated.(map[string]any)
	}
	response, outage := callHandler(context.WithValue(ctx, currentRPCMethodKey{}, method), fn, rid, params)
	if outage != nil {
		return outage
	}
	if contract != nil && response != nil {
		if result, isDict := response["result"].(map[string]any); isDict {
			tuigateway.CheckParamsAccepted(contract, params)
			tuigateway.CheckResult(contract, result)
		}
	}
	return response
}

// Calls a handler, turning a profile or config backend outage it raised into the 4064 answer
func callHandler(ctx context.Context, fn Handler, rid any, params map[string]any) (response map[string]any, outage map[string]any) {
	defer func() {
		failure := recover()
		if failure == nil {
			return
		}
		err, isError := failure.(error)
		if !isError {
			panic(failure)
		}
		var profileErr *ProfileUnavailableError
		var backendErr *config.ConfigBackendUnavailable
		var sourceErr *provider.ConfigBackendUnavailableError
		switch {
		case errors.As(err, &profileErr):
			outage = rpcErr(rid, 4064, profileErr.Error(), nil)
		case errors.As(err, &backendErr):
			outage = rpcErr(rid, 4064, backendErr.SystemExitCode(), nil)
		case errors.As(err, &sourceErr):
			outage = rpcErr(rid, 4064, sourceErr.Message, nil)
		default:
			panic(failure)
		}
	}()
	return fn(ctx, rid, params), nil
}

// Routes one inbound RPC, a long handler to the pool returning nil and every other inline with its response
func Dispatch(ctx context.Context, req any, transport tuigateway.Transport) map[string]any {
	t := transport
	if t == nil {
		t = tuigateway.ModuleStdioTransport()
	}
	ctx = tuigateway.BindTransport(ctx, t)
	if frame := frameObject(req); frame != nil && tuigateway.IsResponseFrame(frame) {
		if !tuigateway.ResolveResponse(frame, t) && !relayComputeHostResponse(ctx, frame) {
			logger().Debug(fmt.Sprintf("dropping response for unknown server request id=%s", pyrepr.Repr(frame.GetOr("id", nil))))
		}
		return nil
	}
	_, method, params, errResp := normalizeRequest(plainRequest(req))
	if errResp != nil {
		return errResp
	}
	if !isLongHandler(method) {
		return handleRequest(ctx, req)
	}
	if !provider.Retirement.Acquire() {
		return rpcErr(requestID(req), 5035, "backend is retiring; reconnect to continue", nil)
	}
	submitted := false
	defer func() {
		if !submitted {
			provider.Retirement.Release()
		}
	}()
	workCtx := ctx
	if owner, isDict := params["owner"].(map[string]any); connectorRPCMethods[method] && isDict && owner["type"] == "session" {
		workCtx = captureConnectorRPCOwner(workCtx, params)
	}
	run := func() {
		var resp map[string]any
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					resp = rpcErr(requestID(req), -32000, "handler error: "+failureText(failure), nil)
				}
			}()
			resp = handleAdmittedRequest(workCtx, req)
		}()
		if resp != nil {
			t.Write(frameObject(resp))
		}
	}
	if err := rpcPool.Submit(run, provider.Retirement.Release); err != nil {
		panic(err)
	}
	submitted = true
	return nil
}

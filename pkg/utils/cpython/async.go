package cpythonutil

import (
	"context"
	"fmt"
)

// Outcome of one awaited task, handed from the loop thread to the waiting goroutine
type asyncResult struct {
	value *Ref
	err   error
}

// Starts an asyncio event loop on a daemon Python thread that runs plugin coroutines, everything that can fail looked up before the thread starts
func (g *GIL) startLoop() (err error) {
	var held []uintptr
	defer func() {
		if err != nil {
			for _, o := range held {
				pyDecRef(o)
			}
		}
	}()
	asyncio, err := g.Import("asyncio")
	if err != nil {
		return err
	}
	held = append(held, asyncio)
	ensureFuture, err := g.Attr(asyncio, "ensure_future")
	if err != nil {
		return err
	}
	held = append(held, ensureFuture)
	inspect, err := g.Import("inspect")
	if err != nil {
		return err
	}
	isAwaitable, err := g.Attr(inspect, "isawaitable")
	pyDecRef(inspect)
	if err != nil {
		return err
	}
	held = append(held, isAwaitable)
	threading, err := g.Import("threading")
	if err != nil {
		return err
	}
	defer pyDecRef(threading)
	loop, err := g.CallMethod(asyncio, "new_event_loop", nil, nil)
	if err != nil {
		return err
	}
	held = append(held, loop)
	runForever, err := g.Attr(loop, "run_forever")
	if err != nil {
		return err
	}
	defer pyDecRef(runForever)
	thread, err := g.CallMethod(threading, "Thread", nil, map[string]any{
		"target": Borrowed(runForever), "name": "plugin-host-loop", "daemon": true,
	})
	if err != nil {
		return err
	}
	held = append(held, thread)
	r, err := g.CallMethod(thread, "start", nil, nil)
	if err != nil {
		return err
	}
	pyDecRef(r)
	h := g.h
	h.asyncio, h.ensureFuture, h.isAwaitable, h.loop, h.loopThread = asyncio, ensureFuture, isAwaitable, loop, thread
	return nil
}

// Cancels every task, lets cancellations settle, stops and joins the loop thread and closes the loop, ctx bounding the waits
func (g *GIL) stopLoop(ctx context.Context) error {
	h := g.h
	if h.loop == 0 {
		return nil
	}
	cancelAll, err := g.NewFunc("cancel_all_tasks", func(g *GIL, _ []any, _ map[string]any) (any, error) {
		tasks, err := g.CallMethod(h.asyncio, "all_tasks", []any{Borrowed(h.loop)}, nil)
		if err != nil {
			return nil, err
		}
		defer pyDecRef(tasks)
		return nil, g.Iterate(tasks, func(task uintptr) error {
			r, err := g.CallMethod(task, "cancel", nil, nil)
			if err == nil {
				pyDecRef(r)
			}
			return err
		})
	})
	if err != nil {
		return err
	}
	err = g.callSoonThreadsafe(cancelAll)
	pyDecRef(cancelAll)
	if err != nil {
		return err
	}
	var waitErr error
	g.Unblocked(func() { waitErr = h.waitSettled(ctx) })
	if waitErr != nil {
		return waitErr
	}
	stop, err := g.Attr(h.loop, "stop")
	if err != nil {
		return err
	}
	err = g.callSoonThreadsafe(stop)
	pyDecRef(stop)
	if err != nil {
		return err
	}
	if err := g.joinLoopThread(ctx); err != nil {
		return err
	}
	r, err := g.CallMethod(h.loop, "close", nil, nil)
	if err != nil {
		return err
	}
	pyDecRef(r)
	for _, o := range []uintptr{h.loopThread, h.loop, h.asyncio, h.isAwaitable, h.ensureFuture} {
		pyDecRef(o)
	}
	h.loopThread, h.loop, h.asyncio, h.isAwaitable, h.ensureFuture = 0, 0, 0, 0, 0
	return nil
}

// Schedules the callable fn on the loop thread
func (g *GIL) callSoonThreadsafe(fn uintptr) error {
	r, err := g.CallMethod(g.h.loop, "call_soon_threadsafe", []any{Borrowed(fn)}, nil)
	if err == nil {
		pyDecRef(r)
	}
	return err
}

// Waits until no async call is pending or ctx ends
func (h *Host) waitSettled(ctx context.Context) error {
	for {
		h.pendingMu.Lock()
		n := len(h.pending)
		h.pendingMu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-h.settled:
		case <-ctx.Done():
			return fmt.Errorf("%d async calls did not settle after cancellation: %w", n, ctx.Err())
		}
	}
}

// Joins the loop thread in short waits, each releasing the GIL inside join, so ctx can end the wait
func (g *GIL) joinLoopThread(ctx context.Context) error {
	for {
		r, err := g.CallMethod(g.h.loopThread, "join", nil, map[string]any{"timeout": 0.05})
		if err != nil {
			return err
		}
		pyDecRef(r)
		alive, err := g.CallMethod(g.h.loopThread, "is_alive", nil, nil)
		if err != nil {
			return err
		}
		running := alive == pyTrue
		pyDecRef(alive)
		if !running {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the host loop thread did not stop: %w", err)
		}
	}
}

// Calls an async callable, runs the awaitable on the loop thread and waits without holding the GIL until it finishes or ctx ends
func (r *Ref) CallAsync(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
	return r.h.await(ctx, func(g *GIL) (uintptr, error) { return g.Call(r.obj, args, kwargs) })
}

// Runs the referenced awaitable on the loop thread and waits without holding the GIL, a plain value converting as it is
func (r *Ref) Await(ctx context.Context) (any, error) {
	return r.h.await(ctx, func(g *GIL) (uintptr, error) {
		pyIncRef(r.obj)
		return r.obj, nil
	})
}

// Runs the referenced awaitable as a task created inside the contextvars context pyctx and returns its raw result
func (r *Ref) AwaitIn(ctx context.Context, pyctx *Ref) (*Ref, error) {
	return r.h.awaitRaw(ctx, pyctx, func(g *GIL) (uintptr, error) {
		pyIncRef(r.obj)
		return r.obj, nil
	})
}

func (h *Host) await(ctx context.Context, produce func(g *GIL) (uintptr, error)) (any, error) {
	r, err := h.awaitRaw(ctx, nil, produce)
	if err != nil {
		return nil, err
	}
	defer r.Release()
	var out any
	err = h.Do(func(g *GIL) error {
		var err error
		out, err = g.FromPython(r.obj)
		return err
	})
	return out, err
}

// Awaits what produce returns as a task, inside pyctx when given, and hands back the raw result; a ctx that ends first cancels the task
func (h *Host) awaitRaw(ctx context.Context, pyctx *Ref, produce func(g *GIL) (uintptr, error)) (*Ref, error) {
	if Forked() {
		return nil, ErrForkedChild
	}
	id := h.pendingSeq.Add(1)
	ch := make(chan asyncResult, 1)
	taskBox := make(chan *Ref, 1)
	err := h.Do(func(g *GIL) error {
		if h.loop == 0 {
			return ErrNoLoop
		}
		aw, err := produce(g)
		if err != nil {
			return err
		}
		awaitable, err := g.IsAwaitable(aw)
		if err != nil {
			pyDecRef(aw)
			return err
		}
		if !awaitable {
			ch <- asyncResult{value: g.adopt(aw)}
			return nil
		}
		h.pendingMu.Lock()
		h.pending[id] = ch
		h.pendingMu.Unlock()
		done, err := g.NewRawFunc("async_call_done", func(g *GIL, args, _ uintptr) (uintptr, error) {
			h.complete(g, id, pyTupleGetItem(args, 0))
			return g.None(), nil
		})
		if err != nil {
			pyDecRef(aw)
			return err
		}
		start, err := g.NewRawFunc("async_call_start", func(g *GIL, _, _ uintptr) (uintptr, error) {
			defer pyDecRef(aw)
			defer pyDecRef(done)
			kwargs := map[string]any{"loop": Borrowed(h.loop)}
			var t uintptr
			var err error
			if pyctx != nil {
				t, err = g.CallMethod(pyctx.obj, "run", []any{Borrowed(h.ensureFuture), Borrowed(aw)}, kwargs)
			} else {
				t, err = g.Call(h.ensureFuture, []any{Borrowed(aw)}, kwargs)
			}
			if err != nil {
				h.deliver(id, asyncResult{err: err})
				return g.None(), nil
			}
			taskBox <- g.adopt(t)
			if rr, err := g.CallMethod(t, "add_done_callback", []any{Borrowed(done)}, nil); err != nil {
				h.deliver(id, asyncResult{err: err})
			} else {
				pyDecRef(rr)
			}
			return g.None(), nil
		})
		if err != nil {
			pyDecRef(aw)
			pyDecRef(done)
			return err
		}
		err = g.callSoonThreadsafe(start)
		if err != nil {
			pyDecRef(aw)
			pyDecRef(done)
		}
		pyDecRef(start)
		return err
	})
	if err != nil {
		h.forget(id)
		return nil, err
	}
	finish := func(res asyncResult) (*Ref, error) {
		select {
		case t := <-taskBox:
			t.Release()
		default:
		}
		return res.value, res.err
	}
	select {
	case res := <-ch:
		return finish(res)
	case <-ctx.Done():
	}
	select {
	case res := <-ch:
		return finish(res)
	case t := <-taskBox:
		cerr := h.Do(func(g *GIL) error {
			cancel, err := g.Attr(t.obj, "cancel")
			if err != nil {
				return err
			}
			defer pyDecRef(cancel)
			return g.callSoonThreadsafe(cancel)
		})
		t.Release()
		if cerr != nil {
			return nil, cerr
		}
		res := <-ch
		if res.err == nil {
			return res.value, nil
		}
		return nil, ctx.Err()
	}
}

// Reads a finished task on the loop thread and hands its outcome to the waiting goroutine
func (h *Host) complete(g *GIL, id uint64, task uintptr) {
	cancelled, err := g.CallMethod(task, "cancelled", nil, nil)
	if err != nil {
		h.deliver(id, asyncResult{err: err})
		return
	}
	wasCancelled := cancelled == pyTrue
	pyDecRef(cancelled)
	if wasCancelled {
		// result() raises the task's own CancelledError, so the error keeps the instance for matching and re-raising
		r, err := g.CallMethod(task, "result", nil, nil)
		if err == nil {
			pyDecRef(r)
			err = &Error{Type: "asyncio.CancelledError", Message: "task was cancelled"}
		}
		h.deliver(id, asyncResult{err: err})
		return
	}
	exc, err := g.CallMethod(task, "exception", nil, nil)
	if err != nil {
		h.deliver(id, asyncResult{err: err})
		return
	}
	if exc != pyNone {
		h.deliver(id, asyncResult{err: g.describe(g.adopt(exc))})
		return
	}
	pyDecRef(exc)
	res, err := g.CallMethod(task, "result", nil, nil)
	if err != nil {
		h.deliver(id, asyncResult{err: err})
		return
	}
	h.deliver(id, asyncResult{value: g.adopt(res)})
}

// Hands an outcome to the goroutine waiting on id, releasing a result nobody waits for any more
func (h *Host) deliver(id uint64, res asyncResult) {
	h.pendingMu.Lock()
	ch := h.pending[id]
	delete(h.pending, id)
	empty := len(h.pending) == 0
	h.pendingMu.Unlock()
	if ch != nil {
		ch <- res
	} else if res.value != nil {
		res.value.Release()
	}
	if empty {
		h.signalSettled()
	}
}

func (h *Host) forget(id uint64) {
	h.pendingMu.Lock()
	delete(h.pending, id)
	empty := len(h.pending) == 0
	h.pendingMu.Unlock()
	if empty {
		h.signalSettled()
	}
}

// Wakes a stopLoop waiting for pending calls to settle
func (h *Host) signalSettled() {
	select {
	case h.settled <- struct{}{}:
	default:
	}
}

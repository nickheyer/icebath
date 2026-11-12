package pyhost

import (
	"context"
	"fmt"
	"time"
)

type asyncResult struct {
	value any
	err   error
}

// Starts the event loop on a Python thread, the way hermes runs plugin coroutines
func (g *GIL) startLoop() error {
	asyncio, err := g.Import("asyncio")
	if err != nil {
		return err
	}
	threading, err := g.Import("threading")
	if err != nil {
		return err
	}
	defer pyDecRef(threading)
	loop, err := g.CallMethod(asyncio, "new_event_loop", nil, nil)
	if err != nil {
		return err
	}
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
	r, err := g.CallMethod(thread, "start", nil, nil)
	if err != nil {
		return err
	}
	pyDecRef(r)
	inspect, err := g.Import("inspect")
	if err != nil {
		return err
	}
	defer pyDecRef(inspect)
	isAwaitable, err := g.Attr(inspect, "isawaitable")
	if err != nil {
		return err
	}
	g.h.asyncio, g.h.loop, g.h.loopThread, g.h.isAwaitable = asyncio, loop, thread, isAwaitable
	return nil
}

// Cancels every task, lets cancellations settle, stops and joins the loop thread and closes the loop
func (g *GIL) stopLoop(deadline time.Time) error {
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
		list, err := g.Call(pyListTypeOf(g), []any{Borrowed(tasks)}, nil)
		if err != nil {
			return nil, err
		}
		defer pyDecRef(list)
		for i, n := 0, pyListSize(list); i < n; i++ {
			r, err := g.CallMethod(pyListGetItem(list, i), "cancel", nil, nil)
			if err != nil {
				return nil, err
			}
			pyDecRef(r)
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	r, err := g.CallMethod(h.loop, "call_soon_threadsafe", []any{Borrowed(cancelAll)}, nil)
	pyDecRef(cancelAll)
	if err != nil {
		return err
	}
	pyDecRef(r)
	settled := false
	g.Unblocked(func() {
		for time.Now().Before(deadline) {
			h.pendingMu.Lock()
			n := len(h.pending)
			h.pendingMu.Unlock()
			if n == 0 {
				settled = true
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	if !settled {
		return fmt.Errorf("async calls did not settle after cancellation")
	}
	stop, err := g.Attr(h.loop, "stop")
	if err != nil {
		return err
	}
	r, err = g.CallMethod(h.loop, "call_soon_threadsafe", []any{Borrowed(stop)}, nil)
	pyDecRef(stop)
	if err != nil {
		return err
	}
	pyDecRef(r)
	if r, err = g.CallMethod(h.loopThread, "join", nil, nil); err != nil {
		return err
	}
	pyDecRef(r)
	if r, err = g.CallMethod(h.loop, "close", nil, nil); err != nil {
		return err
	}
	pyDecRef(r)
	pyDecRef(h.loopThread)
	pyDecRef(h.loop)
	pyDecRef(h.asyncio)
	h.loop, h.loopThread, h.asyncio = 0, 0, 0
	if h.isAwaitable != 0 {
		pyDecRef(h.isAwaitable)
		h.isAwaitable = 0
	}
	return nil
}

func pyListTypeOf(g *GIL) uintptr {
	l := pyListNew(0)
	t := pyType(l)
	pyDecRef(l)
	return t
}

// Calls an async callable, runs the awaitable on the loop thread and waits without holding the GIL
func (r *Ref) CallAsync(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
	return r.h.await(ctx, func(g *GIL) (uintptr, error) { return g.Call(r.obj, args, kwargs) })
}

// Runs the referenced awaitable on the loop thread and waits without holding the GIL, a plain value converts
func (r *Ref) Await(ctx context.Context) (any, error) {
	return r.h.await(ctx, func(g *GIL) (uintptr, error) {
		pyIncRef(r.obj)
		return r.obj, nil
	})
}

func (h *Host) await(ctx context.Context, produce func(g *GIL) (uintptr, error)) (any, error) {
	if Forked() {
		return nil, ErrForkedChild
	}
	id := h.pendingSeq.Add(1)
	ch := make(chan asyncResult, 1)
	taskBox := make(chan *Ref, 1)
	err := h.Do(func(g *GIL) error {
		if h.loop == 0 {
			return errNoLoop
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
			v, err := g.FromPython(aw)
			pyDecRef(aw)
			ch <- asyncResult{v, err}
			return nil
		}
		h.pendingMu.Lock()
		h.pending[id] = ch
		h.pendingMu.Unlock()
		done, err := g.NewRawFunc("async_call_done", func(g *GIL, args, _ uintptr) (uintptr, error) {
			h.complete(g, id, pyTupleGetItem(args, 0))
			pyIncRef(pyNone)
			return pyNone, nil
		})
		if err != nil {
			pyDecRef(aw)
			return err
		}
		start, err := g.NewRawFunc("async_call_start", func(g *GIL, _, _ uintptr) (uintptr, error) {
			defer pyDecRef(aw)
			defer pyDecRef(done)
			t, err := g.CallMethod(h.asyncio, "ensure_future", []any{Borrowed(aw)}, map[string]any{"loop": Borrowed(h.loop)})
			if err != nil {
				h.deliver(id, asyncResult{err: err})
				pyIncRef(pyNone)
				return pyNone, nil
			}
			taskBox <- g.adopt(t)
			if rr, err := g.CallMethod(t, "add_done_callback", []any{Borrowed(done)}, nil); err != nil {
				h.deliver(id, asyncResult{err: err})
			} else {
				pyDecRef(rr)
			}
			pyIncRef(pyNone)
			return pyNone, nil
		})
		if err != nil {
			pyDecRef(aw)
			pyDecRef(done)
			return err
		}
		res, err := g.CallMethod(h.loop, "call_soon_threadsafe", []any{Borrowed(start)}, nil)
		if err != nil {
			pyDecRef(aw)
			pyDecRef(done)
		} else {
			pyDecRef(res)
		}
		pyDecRef(start)
		return err
	})
	if err != nil {
		h.forget(id)
		return nil, err
	}
	finish := func(res asyncResult) (any, error) {
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
			r, err := g.CallMethod(h.loop, "call_soon_threadsafe", []any{Borrowed(cancel)}, nil)
			if err == nil {
				pyDecRef(r)
			}
			return err
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
		h.deliver(id, asyncResult{err: &PyError{Type: "asyncio.CancelledError", Message: "task was cancelled"}})
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
	defer pyDecRef(res)
	v, err := g.FromPython(res)
	h.deliver(id, asyncResult{v, err})
}

func (h *Host) deliver(id uint64, res asyncResult) {
	h.pendingMu.Lock()
	ch := h.pending[id]
	delete(h.pending, id)
	h.pendingMu.Unlock()
	if ch != nil {
		ch <- res
	}
}

func (h *Host) forget(id uint64) {
	h.pendingMu.Lock()
	delete(h.pending, id)
	h.pendingMu.Unlock()
}

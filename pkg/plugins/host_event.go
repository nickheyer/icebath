package plugins

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
)

// Flag a hosted plugin sees as a threading.Event whose state stays on the side that created it
type HostEvent struct {
	mu         sync.Mutex
	flag       bool
	woken      chan struct{}
	child      *childEventRef
	registered map[*PluginHost]hostEventSlot
}

// Parent id an event holds in one plugin host child generation
type hostEventSlot struct {
	id         int64
	generation int
}

// Plugin host child event a HostEvent proxy reads and changes over the wire
type childEventRef struct {
	host       *PluginHost
	ref        int64
	generation int
}

// Unset event living in this process
func NewHostEvent() *HostEvent { return &HostEvent{woken: make(chan struct{})} }

// Sets the flag and wakes every waiter in this process and in the plugin host child
func (e *HostEvent) Set() {
	if e.child != nil {
		_, _ = e.child.call(context.Background(), "set", nil)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.flag {
		e.flag = true
		close(e.woken)
	}
}

// Resets the flag so later waits block again
func (e *HostEvent) clear() {
	if e.child != nil {
		_, _ = e.child.call(context.Background(), "clear", nil)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flag {
		e.flag = false
		e.woken = make(chan struct{})
	}
}

// Whether the flag is set, a child event whose plugin host is gone reading as set
func (e *HostEvent) IsSet() bool {
	if e.child != nil {
		v, err := e.child.call(context.Background(), "is_set", nil)
		return err != nil || v == true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.flag
}

// Blocks until the flag is set, ctx ends or timeout passes (negative waits without limit) and answers the flag
func (e *HostEvent) Wait(ctx context.Context, timeout time.Duration) bool {
	if e.child != nil {
		var seconds any
		if timeout >= 0 {
			seconds = timeout.Seconds()
		}
		v, err := e.child.call(ctx, "wait", seconds)
		if err != nil {
			return ctx.Err() == nil
		}
		return v == true
	}
	e.mu.Lock()
	if e.flag {
		e.mu.Unlock()
		return true
	}
	woken := e.woken
	e.mu.Unlock()
	if timeout == 0 {
		return false
	}
	var expire <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expire = timer.C
	}
	select {
	case <-woken:
		return true
	case <-expire:
	case <-ctx.Done():
	}
	return e.IsSet()
}

// Reason attribute of a child event, empty when it has none and for an event made in this process
func (e *HostEvent) Reason() string {
	if e.child == nil {
		return ""
	}
	v, _ := e.child.call(context.Background(), "reason", nil)
	s, _ := v.(string)
	return s
}

// Runs one event operation on the child event the ref names
func (c *childEventRef) call(ctx context.Context, op string, timeout any) (any, error) {
	return c.host.call(ctx, "event", map[string]any{"ref": c.ref, "op": op, "timeout": timeout}, &c.generation, 0, nil)
}

// Wire reference of an event for the running child, one id per event and child for as long as the event lives
func (h *PluginHost) eventRef(e *HostEvent) map[string]any {
	if e.child != nil && e.child.host == h {
		return map[string]any{"__child_event__": e.child.ref}
	}
	h.mu.Lock()
	generation := h.generation
	h.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if slot, ok := e.registered[h]; ok && slot.generation == generation {
		return map[string]any{"__parent_event__": slot.id}
	}
	h.stateMu.Lock()
	h.parentIDs++
	id := h.parentIDs
	h.parents[id] = parentCallableSlot{event: weak.Make(e), generation: generation}
	h.stateMu.Unlock()
	if e.registered == nil {
		e.registered = map[*PluginHost]hostEventSlot{}
	}
	e.registered[h] = hostEventSlot{id: id, generation: generation}
	runtime.AddCleanup(e, func(r releaseRef) { r.host.forgetParentEvent(r.ref) }, releaseRef{host: h, ref: id})
	return map[string]any{"__parent_event__": id}
}

// Drops the parent slot of an event this process no longer holds
func (h *PluginHost) forgetParentEvent(id int64) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if slot, ok := h.parents[id]; ok && slot.fn == nil {
		delete(h.parents, id)
	}
}

// Event a child ref names, the parent event handed to the child or a proxy for an event made in the child
func (h *PluginHost) resolveEventRef(ref map[string]any) (*HostEvent, bool, error) {
	if raw, ok := ref["__parent_event__"]; ok {
		id, _ := wireInt(raw)
		h.stateMu.Lock()
		slot, held := h.parents[id]
		h.stateMu.Unlock()
		e := slot.event.Value()
		if !held || e == nil {
			return nil, true, &HostBuiltinError{Type: "LookupError", Message: fmt.Sprintf("icebath event %d was released", id)}
		}
		return e, true, nil
	}
	raw, ok := ref["__child_event__"]
	if !ok {
		return nil, false, nil
	}
	id, _ := wireInt(raw)
	h.mu.Lock()
	generation := h.generation
	h.mu.Unlock()
	e := &HostEvent{child: &childEventRef{host: h, ref: id, generation: generation}}
	runtime.AddCleanup(e, func(r releaseRef) { r.host.release(r.generation, r.ref) }, releaseRef{host: h, generation: generation, ref: id})
	return e, true, nil
}

// Serves is_set, set, clear and wait of a parent event the child holds, a wait ending when the child goes
func (h *PluginHost) serveParentEvent(ctx context.Context, params map[string]any) (any, error) {
	id, _ := wireInt(params["id"])
	e, _, err := h.resolveEventRef(map[string]any{"__parent_event__": id})
	if err != nil {
		return nil, err
	}
	switch op := wireStr(params["op"]); op {
	case "is_set":
		return e.IsSet(), nil
	case "set":
		e.Set()
		return nil, nil
	case "clear":
		e.clear()
		return nil, nil
	case "wait":
		timeout, err := pyWaitTimeout(params["timeout"])
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		ch := h.channel
		h.mu.Unlock()
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if ch != nil {
			go func() {
				select {
				case <-ch.closed:
					cancel()
				case <-wctx.Done():
				}
			}()
		}
		return e.Wait(wctx, timeout), nil
	default:
		return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("unknown icebath event operation %s", config.PyRepr(op))}
	}
}

// Wait duration of a Python Event.wait timeout, None without limit and a non positive number not blocking
func pyWaitTimeout(raw any) (time.Duration, error) {
	var seconds float64
	switch v := raw.(type) {
	case nil:
		return -1, nil
	case bool:
		seconds = map[bool]float64{true: 1}[v]
	case int64:
		seconds = float64(v)
	case float64:
		seconds = v
	case *big.Int:
		seconds, _ = new(big.Float).SetInt(v).Float64()
	default:
		return 0, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("'>' not supported between instances of '%s' and 'int'", pyTypeName(raw))}
	}
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0, nil
	}
	if seconds*float64(time.Second) >= math.MaxInt64 {
		return 0, &HostBuiltinError{Type: "OverflowError", Message: "timestamp out of range for C PyTime_t"}
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

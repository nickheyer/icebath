package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// WatchIntervalSeconds is the observe tick of a driven operation, swapped by tests
var WatchIntervalSeconds = 5.0

// The interrupt flag has no wake hook so the tick sleep is sliced and the flag read each slice
const connWakeSlice = 250 * time.Millisecond

// ConnCallback hands the connection.request payload to the surface that draws the card
type ConnCallback func(payload *pyjson.Object) string

// ConnKind holds the per kind hooks of a driven operation and the note appended to its result
type ConnKind struct {
	Prepare func(op *ConnectionOperation)
	Observe func(op *ConnectionOperation)
	Note    string
	Result  func(op *ConnectionOperation) *pyjson.Object
}

// RunOptions carries the session, card callback and tick of one driven operation
type RunOptions struct {
	SessionKey       string
	ToolCallID       string
	Callback         ConnCallback
	TickSeconds      *float64
	WithURLsInResult bool
	Call             any
}

// Blocks the tool goroutine until a fresh operation over targets settles and returns the tool JSON
func RunOperation(ctx context.Context, env *Env, targets []*ConnTarget, kind ConnKind, opts RunOptions) string {
	op := NewConnectionOperation(targets, opts.SessionKey, opts.ToolCallID, opts.Call)
	return OpenAndDrive(ctx, env, op, kind, opts.Callback, opts.TickSeconds, opts.WithURLsInResult)
}

// Registers the operation as the session's open one and drives it, one open per session
func OpenAndDrive(ctx context.Context, env *Env, op *ConnectionOperation, kind ConnKind, cb ConnCallback, tick *float64, withURLs bool) string {
	if err := LiveOpen(ctx, op); err != nil {
		existing := err.(*OperationAlreadyOpen).Existing
		return toolErrorJSON(fmt.Sprintf("a connection operation is already open in this session (%s); it settles "+
			"when the user finishes with the card, on Continue, or at its deadline. Do not start another.", existing.OpID), nil)
	}
	return DriveOperation(ctx, env, op, kind, cb, tick, withURLs)
}

// Prepares, emits the card, watches until settled and renders the settled result
func DriveOperation(ctx context.Context, env *Env, op *ConnectionOperation, kind ConnKind, cb ConnCallback, tick *float64, withURLs bool) string {
	func() {
		defer LiveClose(op)
		kind.Prepare(op)
		op.SettleIfAllResolved()
		if cb != nil && !op.Settled() {
			cb(op.RequestPayload())
		}
		watchOperation(ctx, op, kind, tick)
	}()
	if kind.Result != nil {
		return connDumps(kind.Result(op))
	}
	payload := op.Result(ctx, env, withURLs)
	payload.Set("status", "settled")
	payload.Set("note", kind.Note)
	return connDumps(payload)
}

// Python json.dumps with ensure_ascii off of an operation payload
func connDumps(v any) string {
	out, err := pyjson.Dumps(v, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return out
}

// Observes every tick until the operation settles by resolution, deadline or interrupt
func watchOperation(ctx context.Context, op *ConnectionOperation, kind ConnKind, tick *float64) {
	interval := WatchIntervalSeconds
	if tick != nil {
		interval = *tick
	}
	for !op.Settled() {
		if IsInterrupted(ctx) {
			op.Settle(SettleInterrupt)
			return
		}
		if connNow() >= op.DeadlineAt {
			op.Settle(SettleDeadline)
			return
		}
		kind.Observe(op)
		if op.Settled() || op.SettleIfAllResolved() {
			return
		}
		sleepUntilWake(ctx, op, interval)
	}
}

// Sleeps up to one tick, leaving early on wake, the deadline or the interrupt flag
func sleepUntilWake(ctx context.Context, op *ConnectionOperation, tick float64) {
	until := min(connNow()+tick, op.DeadlineAt)
	for !op.Settled() && !IsInterrupted(ctx) {
		remaining := until - connNow()
		if remaining <= 0 {
			break
		}
		if op.Wake.Wait(min(connWakeSlice, time.Duration(remaining*float64(time.Second)))) {
			break
		}
	}
	op.Wake.Clear()
}

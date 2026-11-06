package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// OperationDeadlineSeconds bounds every connection operation, swapped by tests
var OperationDeadlineSeconds = 600.0

// Wall clock in float seconds, the time.time of the operation records
var connNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// IllegalTransition is a target move the contract refuses
type IllegalTransition struct{ Msg string }

func (e *IllegalTransition) Error() string { return e.Msg }

// ConnEvent is a settable flag a waiter can block on with a timeout
type ConnEvent struct {
	mu  sync.Mutex
	ch  chan struct{}
	set bool
}

func NewConnEvent() *ConnEvent { return &ConnEvent{ch: make(chan struct{})} }

func (e *ConnEvent) Set() {
	e.mu.Lock()
	if !e.set {
		e.set = true
		close(e.ch)
	}
	e.mu.Unlock()
}

func (e *ConnEvent) Clear() {
	e.mu.Lock()
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
	e.mu.Unlock()
}

func (e *ConnEvent) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Blocks until the flag is set or the timeout passes and reports whether it was set
func (e *ConnEvent) Wait(timeout time.Duration) bool {
	e.mu.Lock()
	if e.set {
		e.mu.Unlock()
		return true
	}
	ch := e.ch
	e.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return e.IsSet()
	}
}

// ConnTarget is one row of a connection operation
type ConnTarget struct {
	Name         string
	Kind         string
	Action       string
	State        TargetState
	Detail       string
	Instructions string
	ConnectURL   string
	ConnectionID string
	Attempt      string
	NextReadAt   float64
	RequiredEnv  []*pyjson.Object
	Extra        *pyjson.Object
	Phase        string
	Alias        *string
	RepairID     *string
}

// Pending target of kind running action
func NewConnTarget(name, kind, action string) *ConnTarget {
	return &ConnTarget{Name: name, Kind: kind, Action: action, State: TargetPending}
}

func (t *ConnTarget) resolved() bool { return TargetResolves(t.Kind, t.State) }

// Row snapshot in the card order with the extra fields laid over it
func (t *ConnTarget) snapshot(withURL bool) *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("name", t.Name)
	out.Set("kind", t.Kind)
	out.Set("action", t.Action)
	out.Set("state", string(t.State))
	out.Set("resolved", t.resolved())
	if t.Alias != nil && *t.Alias != "" {
		out.Set("alias", *t.Alias)
	}
	if t.Phase != "" && t.State == TargetInitiated {
		out.Set("phase", t.Phase)
	}
	if t.Detail != "" {
		out.Set("detail", t.Detail)
	}
	if t.Instructions != "" {
		out.Set("instructions", t.Instructions)
	}
	if withURL && t.ConnectURL != "" {
		out.Set("connect_url", t.ConnectURL)
	}
	if t.ConnectionID != "" {
		out.Set("connection_id", t.ConnectionID)
	}
	if t.Attempt != "" {
		out.Set("attempt", t.Attempt)
	}
	if len(t.RequiredEnv) > 0 {
		env := make([]any, len(t.RequiredEnv))
		for i, spec := range t.RequiredEnv {
			env[i] = spec
		}
		out.Set("required_env", env)
	}
	if t.Extra != nil {
		for _, k := range t.Extra.Keys {
			out.Set(k, t.Extra.Vals[k])
		}
	}
	return out
}

// TransitionFields carries the optional row fields a transition writes, nil leaves a field alone
type TransitionFields struct {
	Detail       *string
	ConnectURL   *string
	ConnectionID *string
	Attempt      *string
	Extra        *pyjson.Object
}

// ConnectionOperation is one card's set of targets with a server owned deadline and a frozen result
type ConnectionOperation struct {
	Targets    []*ConnTarget
	SessionKey string
	ProfileKey string
	ToolCallID string
	Call       any
	OpID       string
	CreatedAt  float64
	DeadlineAt float64
	Wake       *ConnEvent

	mu              sync.Mutex
	settledAt       *float64
	settledBy       SettleReason
	seq             int
	settledSnapshot *pyjson.Object
	detached        bool
}

// OperationOnChange receives every frame of every attached operation, the class level renderer hook
var OperationOnChange func(op *ConnectionOperation, change, snapshot *pyjson.Object)

// Operation over targets with a fresh id and the deadline counted from now
func NewConnectionOperation(targets []*ConnTarget, sessionKey, toolCallID string, call any) *ConnectionOperation {
	var b [6]byte
	_, _ = rand.Read(b[:])
	now := connNow()
	return &ConnectionOperation{Targets: targets, SessionKey: sessionKey, ToolCallID: toolCallID, Call: call,
		OpID: hex.EncodeToString(b[:]), CreatedAt: now, DeadlineAt: now + OperationDeadlineSeconds, Wake: NewConnEvent()}
}

// Operation that never reaches the renderer hook, the no card path
func NewDetachedOperation(targets []*ConnTarget, sessionKey string) *ConnectionOperation {
	op := NewConnectionOperation(targets, sessionKey, "", nil)
	op.detached = true
	return op
}

// The target named name or nil
func (o *ConnectionOperation) Target(name string) *ConnTarget {
	for _, t := range o.Targets {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// Moves one target when the contract lets actor, nil change when it already sits in to
func (o *ConnectionOperation) Transition(name string, to TargetState, actor ConnActor, f TransitionFields) (*pyjson.Object, error) {
	t := o.Target(name)
	if t == nil {
		return nil, &IllegalTransition{Msg: fmt.Sprintf("unknown target %s", pyrepr.StrRepr(name))}
	}
	o.mu.Lock()
	if t.State == to {
		o.mu.Unlock()
		return nil, nil
	}
	if AllowedTransition(t.Kind, t.State, to) != actor {
		o.mu.Unlock()
		return nil, &IllegalTransition{Msg: fmt.Sprintf("%s %s: %s -> %s by %s", t.Kind, name, t.State, to, actor)}
	}
	from := t.State
	change := pyjson.NewObject()
	change.Set("target", name)
	change.Set("from", string(from))
	change.Set("to", string(to))
	change.Set("actor", string(actor))
	t.State = to
	if f.Detail != nil {
		t.Detail = *f.Detail
	}
	change.Set("detail", t.Detail)
	if f.ConnectURL != nil {
		t.ConnectURL = *f.ConnectURL
	}
	if f.ConnectionID != nil {
		t.ConnectionID = *f.ConnectionID
	}
	if f.Attempt != nil {
		t.Attempt = *f.Attempt
	}
	if f.Extra != nil && f.Extra.Len() > 0 {
		t.Extra = copyObject(f.Extra)
	}
	snapshot := o.bumpLocked()
	o.mu.Unlock()
	slog.Default().Info(fmt.Sprintf("connection op %s: %s %s -> %s by %s", o.OpID, name, from, to, actor))
	o.Wake.Set()
	o.changed(change, snapshot)
	return change, nil
}

// Replaces a target's link, detail and install phase with no state change
func (o *ConnectionOperation) Refresh(name, connectURL, detail string, actor ConnActor, phase string) error {
	t := o.Target(name)
	if t == nil {
		return &IllegalTransition{Msg: fmt.Sprintf("unknown target %s", pyrepr.StrRepr(name))}
	}
	o.mu.Lock()
	t.ConnectURL = connectURL
	t.Detail = detail
	t.Phase = phase
	change := pyjson.NewObject()
	change.Set("target", name)
	change.Set("from", string(t.State))
	change.Set("to", string(t.State))
	change.Set("actor", string(actor))
	change.Set("detail", detail)
	snapshot := o.bumpLocked()
	o.mu.Unlock()
	o.Wake.Set()
	o.changed(change, snapshot)
	return nil
}

// Runs fn on the target under the operation lock so readers never see a half written row
func (o *ConnectionOperation) UpdateTarget(t *ConnTarget, fn func(t *ConnTarget)) {
	o.mu.Lock()
	fn(t)
	o.mu.Unlock()
}

// The target's state read under the operation lock
func (o *ConnectionOperation) StateOf(t *ConnTarget) TargetState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return t.State
}

// Whether the target is resolved, read under the operation lock
func (o *ConnectionOperation) ResolvedTarget(t *ConnTarget) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return t.resolved()
}

// Advances the write counter until settlement and returns the snapshot the frame carries
func (o *ConnectionOperation) bumpLocked() *pyjson.Object {
	if o.settledSnapshot == nil {
		o.seq++
	}
	return o.resultLocked(true)
}

func (o *ConnectionOperation) changed(change, snapshot *pyjson.Object) {
	if hook := OperationOnChange; hook != nil && !o.detached {
		hook(o, change, snapshot)
	}
}

// Whether there is at least one target and every target is resolved
func (o *ConnectionOperation) AllResolved() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.allResolvedLocked()
}

func (o *ConnectionOperation) allResolvedLocked() bool {
	if len(o.Targets) == 0 {
		return false
	}
	for _, t := range o.Targets {
		if !t.resolved() {
			return false
		}
	}
	return true
}

func (o *ConnectionOperation) Settled() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settledAt != nil
}

// The reason the operation settled, empty while it is open
func (o *ConnectionOperation) SettledBy() SettleReason {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.settledBy
}

// Seconds left until the deadline, at now when given else the wall clock, never negative
func (o *ConnectionOperation) RemainingSeconds(now ...float64) float64 {
	at := connNow()
	if len(now) > 0 {
		at = now[0]
	}
	return max(0, o.DeadlineAt-at)
}

// Freezes the result once, stamping every unresolved target not connected, false for a later caller
func (o *ConnectionOperation) Settle(by SettleReason, now ...float64) bool {
	o.mu.Lock()
	if o.settledAt != nil {
		o.mu.Unlock()
		return false
	}
	at := connNow()
	if len(now) > 0 {
		at = now[0]
	}
	o.settledAt = &at
	o.settledBy = by
	outcomes := make([]string, len(o.Targets))
	for i, t := range o.Targets {
		if !t.resolved() {
			t.State = TargetNotConnected
		}
		outcomes[i] = t.Name + "=" + string(t.State)
	}
	o.settledSnapshot = o.bumpLocked()
	snapshot := o.settledSnapshot
	o.mu.Unlock()
	slog.Default().Info(fmt.Sprintf("connection op %s settled by %s: %s", o.OpID, by, strings.Join(outcomes, ", ")))
	o.Wake.Set()
	o.changed(nil, snapshot)
	return true
}

// Settles with all_resolved when every target is resolved
func (o *ConnectionOperation) SettleIfAllResolved() bool {
	return o.AllResolved() && o.Settle(SettleAllResolved)
}

func (o *ConnectionOperation) snapshotLocked(withURLs bool) *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("op_id", o.OpID)
	out.Set("seq", o.seq)
	out.Set("deadline_at", o.DeadlineAt)
	if o.settledAt != nil {
		out.Set("settled_at", *o.settledAt)
		out.Set("settled_by", string(o.settledBy))
	} else {
		out.Set("settled_at", nil)
		out.Set("settled_by", nil)
	}
	targets := make([]any, len(o.Targets))
	for i, t := range o.Targets {
		targets[i] = t.snapshot(withURLs)
	}
	out.Set("targets", targets)
	return out
}

func (o *ConnectionOperation) resultLocked(withURLs bool) *pyjson.Object {
	if o.settledSnapshot == nil {
		return o.snapshotLocked(withURLs)
	}
	src, _ := o.settledSnapshot.Get("targets")
	frozen, _ := src.([]any)
	targets := make([]any, len(frozen))
	for i, t := range frozen {
		cp := copyObject(t.(*pyjson.Object))
		if !withURLs {
			cp.Delete("connect_url")
		}
		targets[i] = cp
	}
	out := copyObject(o.settledSnapshot)
	out.Set("targets", targets)
	return out
}

// Live snapshot, the frozen one once settled
func (o *ConnectionOperation) Snapshot(withURLs bool) *pyjson.Object {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resultLocked(withURLs)
}

// Model facing result, settled MCP rows carrying the catalog listing of their connected tools
func (o *ConnectionOperation) Result(ctx context.Context, env *Env, withURLs bool) *pyjson.Object {
	o.mu.Lock()
	result := o.resultLocked(withURLs)
	o.mu.Unlock()
	raw, _ := result.Get("targets")
	targets, _ := raw.([]any)
	for _, t := range targets {
		obj := t.(*pyjson.Object)
		obj.Delete("connection_id")
		obj.Delete("resolved")
	}
	if v, _ := result.Get("settled_at"); v == nil {
		return result
	}
	for _, t := range targets {
		obj := t.(*pyjson.Object)
		obj.Delete("instructions")
		var names []string
		if kind, _ := obj.Get("kind"); kind == "mcp" {
			v, _ := obj.Get("tools")
			names = connStringList(v)
		}
		if len(names) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, n := range names {
			want[n] = true
		}
		defs := Default.Definitions(ctx, env, want, true)
		if listing, _ := BuildCatalogListingWithForm(ctx, env, defs, 1500); listing != "" {
			obj.Set("tools_listing", listing)
		}
	}
	return result
}

// The connection.request payload and resume snapshot with live rows and the server deadline
func (o *ConnectionOperation) RequestPayload() *pyjson.Object {
	o.mu.Lock()
	targets := make([]any, len(o.Targets))
	for i, t := range o.Targets {
		targets[i] = t.snapshot(true)
	}
	seq := o.seq
	o.mu.Unlock()
	payload := pyjson.NewObject()
	payload.Set("op_id", o.OpID)
	payload.Set("seq", seq)
	payload.Set("deadline_at", o.DeadlineAt)
	payload.Set("timeout_seconds", OperationDeadlineSeconds)
	payload.Set("targets", targets)
	if o.ToolCallID != "" {
		payload.Set("tool_call_id", o.ToolCallID)
	}
	return payload
}

func copyObject(src *pyjson.Object) *pyjson.Object {
	out := &pyjson.Object{Keys: append([]string(nil), src.Keys...), Vals: make(map[string]any, len(src.Vals))}
	for k, v := range src.Vals {
		out.Vals[k] = v
	}
	return out
}

func connStringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

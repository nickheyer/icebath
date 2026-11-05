package provider

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// AuxExplicitCancelCause is the frozen cause every explicit auxiliary cancellation carries
const AuxExplicitCancelCause = "explicit_host_cancel"

// AuxiliaryExplicitCancellation is the host hard cancel of one auxiliary attempt, never a transport failure
type AuxiliaryExplicitCancellation struct{}

func (*AuxiliaryExplicitCancellation) Error() string {
	return "auxiliary request explicitly cancelled by host"
}

// Cause names the explicit host cancel so nothing re-queries a mutable host event after unwinding
func (*AuxiliaryExplicitCancellation) Cause() string { return AuxExplicitCancelCause }

func (*AuxiliaryExplicitCancellation) auxBaseException() {}

// AuxCancelCheck is an explicit host cancel source polled by protected auxiliary calls
type AuxCancelCheck interface {
	CancelRequested() (bool, error)
}

// AuxCancelFunc adapts a plain function into a cancel source
type AuxCancelFunc func() (bool, error)

// CancelRequested runs the function
func (f AuxCancelFunc) CancelRequested() (bool, error) { return f() }

// AuxCancelEvent is a host event whose set state means an explicit cancel
type AuxCancelEvent interface {
	IsSet() bool
}

type auxInterruptState struct {
	active      bool
	cancelCheck AuxCancelCheck
	cancelEvent AuxCancelEvent
}

type auxInterruptKey struct{}

func auxInterruptStateOf(ctx context.Context) auxInterruptState {
	if ctx == nil {
		return auxInterruptState{}
	}
	s, _ := ctx.Value(auxInterruptKey{}).(auxInterruptState)
	return s
}

// AuxInterruptProtection marks calls under the context interrupt protected, nested scopes inheriting the cancel sources
func AuxInterruptProtection(ctx context.Context, active bool, cancelCheck AuxCancelCheck, cancelEvent AuxCancelEvent) context.Context {
	s := auxInterruptStateOf(ctx)
	s.active = active
	if cancelCheck != nil {
		s.cancelCheck = cancelCheck
	}
	if cancelEvent != nil {
		s.cancelEvent = cancelEvent
	}
	return context.WithValue(ctx, auxInterruptKey{}, s)
}

// Whether the current auxiliary call is interrupt protected
func auxInterruptProtected(ctx context.Context) bool { return auxInterruptStateOf(ctx).active }

// Explicit cancel source of the context, the event preferred over the check, nil when none is installed
func auxCaptureAuxCancelCheck(ctx context.Context) AuxCancelCheck {
	s := auxInterruptStateOf(ctx)
	if s.cancelEvent != nil {
		ev := s.cancelEvent
		return AuxCancelFunc(func() (bool, error) { return ev.IsSet(), nil })
	}
	return s.cancelCheck
}

// Reads a cancel source, a failing source logging at debug and reading as not cancelled
func auxCapturedAuxCancelRequested(check AuxCancelCheck) bool {
	requested, err := check.CancelRequested()
	if err != nil {
		slog.Debug("captured aux cancel check failed", "error", err)
		return false
	}
	return requested
}

// Whether an explicit host cancel overrides the protection of the current call
func auxInterruptCancelRequested(ctx context.Context) bool {
	check := auxCaptureAuxCancelCheck(ctx)
	if check == nil {
		return false
	}
	return auxCapturedAuxCancelRequested(check)
}

// Linearizes explicit cancellation against provider timeout so each attempt has exactly one outcome
type auxCancellationDecision struct {
	source  AuxCancelCheck
	mu      sync.Mutex
	outcome string
}

func newAuxCancellationDecision(source AuxCancelCheck) *auxCancellationDecision {
	return &auxCancellationDecision{source: source, outcome: "active"}
}

// CancelRequested freezes the attempt as cancelled once the source reports a cancel
func (d *auxCancellationDecision) CancelRequested() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.outcome == "active" && auxCapturedAuxCancelRequested(d.source) {
		d.outcome = "cancelled"
	}
	return d.outcome == "cancelled", nil
}

// BeginTimeoutCleanup reports whether the timeout won and destructive cleanup is permitted
func (d *auxCancellationDecision) BeginTimeoutCleanup() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.outcome == "active" {
		if auxCapturedAuxCancelRequested(d.source) {
			d.outcome = "cancelled"
		} else {
			d.outcome = "timed_out"
		}
	}
	return d.outcome == "timed_out"
}

// AuxHookLocal names one of the per call auxiliary timing hooks
type AuxHookLocal int

const (
	// AuxProgressLocal is the forward progress hook
	AuxProgressLocal AuxHookLocal = iota
	// AuxDispatchLocal records an actual provider dispatch
	AuxDispatchLocal
	// AuxProviderResponseLocal records any provider frame including content free ones
	AuxProviderResponseLocal
)

type auxHookKey struct{ local AuxHookLocal }

// AuxThreadLocalHook installs one timing hook for calls under the context, a nil hook keeping the previous one
func AuxThreadLocalHook(ctx context.Context, local AuxHookLocal, hook func() error) context.Context {
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, auxHookKey{local}, hook)
}

func auxHook(ctx context.Context, local AuxHookLocal) func() error {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(auxHookKey{local}).(func() error)
	return h
}

// AuxProgressHook installs the forward progress callback for auxiliary calls under the context
func AuxProgressHook(ctx context.Context, hook func() error) context.Context {
	return AuxThreadLocalHook(ctx, AuxProgressLocal, hook)
}

// Calls the installed hook, a failing hook logging at debug and never raising
func auxTickHook(ctx context.Context, local AuxHookLocal, label string) {
	hook := auxHook(ctx, local)
	if hook == nil {
		return
	}
	if err := hook(); err != nil {
		slog.Debug(fmt.Sprintf("aux %s hook failed", label), "error", err)
	}
}

// Ticks the forward progress hook
func auxNotifyAuxProgress(ctx context.Context) { auxTickHook(ctx, AuxProgressLocal, "progress") }

// Records a provider dispatch without claiming response progress
func auxNotifyAuxDispatch(ctx context.Context) { auxTickHook(ctx, AuxDispatchLocal, "dispatch") }

// Records a content free frame that counts toward first progress timing but not the inactivity fence
func auxNotifyAuxTimingResponse(ctx context.Context) {
	auxTickHook(ctx, AuxProviderResponseLocal, "provider response")
}

// Records a provider response then ticks forward progress
func auxNotifyAuxProviderResponse(ctx context.Context) {
	auxNotifyAuxTimingResponse(ctx)
	auxNotifyAuxProgress(ctx)
}

// Whether a forward progress hook is installed
func auxProgressActive(ctx context.Context) bool { return auxHook(ctx, AuxProgressLocal) != nil }

// Field of a wire map with the default standing in for an absent or null value
func auxField(obj any, key string, def any) any {
	var val any
	if m, ok := obj.(map[string]any); ok {
		val = m[key]
	}
	if val == nil {
		return def
	}
	return val
}

// Whether an Anthropic stream event carries a non empty payload
func auxAnthropicEventHasContent(event any) bool {
	switch auxField(event, "type", nil) {
	case "content_block_delta":
		delta := auxField(event, "delta", nil)
		for _, f := range []string{"text", "thinking", "partial_json", "signature", "citation"} {
			if pyrepr.Truthy(auxField(delta, f, nil)) {
				return true
			}
		}
	case "content_block_start":
		block := auxField(event, "content_block", nil)
		return auxField(block, "type", nil) == "tool_use" &&
			(pyrepr.Truthy(auxField(block, "id", nil)) || pyrepr.Truthy(auxField(block, "name", nil)))
	}
	return false
}

// Per event Anthropic callback that ticks progress only for payloads and stops at the host deadline or cancel
func auxAnthropicAuxStreamEventHook(ctx context.Context) func(event any) error {
	hostDeadline, hasDeadline := auxCurrentAuxStreamDeadline(ctx)
	started := time.Now()
	return func(event any) error {
		if auxAnthropicEventHasContent(event) {
			auxNotifyAuxProviderResponse(ctx)
		} else {
			auxNotifyAuxTimingResponse(ctx)
		}
		if auxInterruptCancelRequested(ctx) {
			return &AuxiliaryExplicitCancellation{}
		}
		if hasDeadline && !time.Now().Before(hostDeadline) {
			return &AuxTimeoutError{Message: fmt.Sprintf("Anthropic auxiliary stream timed out at the host compression "+
				"deadline after %.0fs (the caller already stopped waiting)", time.Since(started).Seconds())}
		}
		return nil
	}
}

// AuxStreamDeadlineFunc reads the waiting host's absolute deadline live, false when none applies
type AuxStreamDeadlineFunc func() (time.Time, bool)

// AuxStreamDeadlineAt is a fixed host deadline
func AuxStreamDeadlineAt(deadline time.Time) AuxStreamDeadlineFunc {
	return func() (time.Time, bool) { return deadline, true }
}

type auxStreamDeadlineKey struct{}

// AuxStreamDeadline publishes the host deadline to stream consumers under the context, nil keeping the previous one
func AuxStreamDeadline(ctx context.Context, deadline AuxStreamDeadlineFunc) context.Context {
	if deadline == nil {
		return ctx
	}
	return context.WithValue(ctx, auxStreamDeadlineKey{}, deadline)
}

// The waiting host's absolute deadline read live, false when none is installed
func auxCurrentAuxStreamDeadline(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	read, _ := ctx.Value(auxStreamDeadlineKey{}).(AuxStreamDeadlineFunc)
	if read == nil {
		return time.Time{}, false
	}
	return read()
}

// Owner poll interval of a protected provider call
const auxProtectedPollInterval = 20 * time.Millisecond

// Runs a protected provider callback on an isolated goroutine while the owner polls the explicit cancel source
func auxRunProtectedSyncProviderCall(ctx context.Context, callback func(ctx context.Context, kwargs map[string]any) (any, error), kwargs map[string]any) (any, error) {
	source := auxCaptureAuxCancelCheck(ctx)
	if !auxInterruptProtected(ctx) || source == nil {
		return callback(ctx, kwargs)
	}
	decision := newAuxCancellationDecision(source)
	if auxCapturedAuxCancelRequested(decision) {
		return nil, &AuxiliaryExplicitCancellation{}
	}
	workerCtx := context.WithValue(ctx, auxInterruptKey{}, auxInterruptState{active: true, cancelCheck: decision})
	done := make(chan struct{})
	var result any
	var err error
	var panicked any
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		result, err = callback(workerCtx, kwargs)
	}()
	tick := time.NewTimer(auxProtectedPollInterval)
	defer tick.Stop()
	for {
		if auxCapturedAuxCancelRequested(decision) {
			return nil, &AuxiliaryExplicitCancellation{}
		}
		select {
		case <-done:
		case <-tick.C:
			tick.Reset(auxProtectedPollInterval)
			continue
		}
		if auxCapturedAuxCancelRequested(decision) {
			return nil, &AuxiliaryExplicitCancellation{}
		}
		if panicked != nil {
			panic(panicked)
		}
		return result, err
	}
}

// Implemented by clients that declare capability flags instead of being matched by type
type auxFlagDeclarer interface {
	AuxDeclares(flag string) bool
}

// Whether the client declares the flag truthy, an absent declaration being false
func auxClientDeclares(client any, flag string) bool {
	d, ok := client.(auxFlagDeclarer)
	return ok && d.AuxDeclares(flag)
}

// Whether the value is of one of the types or implements one of the interfaces, a nil type matching nothing
func auxSafeIsInstance(obj any, types ...reflect.Type) bool {
	t := reflect.TypeOf(obj)
	if t == nil {
		return false
	}
	for _, want := range types {
		if want == nil {
			continue
		}
		if want.Kind() == reflect.Interface && t.Implements(want) || t == want {
			return true
		}
	}
	return false
}

// URL without its query and the first value of each non blank query parameter, nil when the URL has no query
func auxExtractURLQueryParams(raw string) (string, map[string]string, error) {
	parsed, err := pyURLParse(raw, "")
	if err != nil {
		return "", nil, err
	}
	if parsed.query == "" {
		return raw, nil, nil
	}
	params := map[string]string{}
	for _, pair := range strings.Split(parsed.query, "&") {
		name, value, ok := strings.Cut(pair, "=")
		if pair == "" || !ok || value == "" {
			continue
		}
		name = pyUnquoteReplace(strings.ReplaceAll(name, "+", " "))
		if _, seen := params[name]; !seen {
			params[name] = pyUnquoteReplace(strings.ReplaceAll(value, "+", " "))
		}
	}
	parsed.query = ""
	return pyURLUnparse(parsed), params, nil
}

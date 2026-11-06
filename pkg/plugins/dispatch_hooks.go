package plugins

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	hookCallbackTimeoutSecs        = 30.0
	maxHookCallbackTimeoutSecs     = 600.0
	hookTimeoutSuppressionDefault  = 60 * time.Second
	hookMaxAbandonedWorkers        = 3
	preToolCallTimeoutBlockMessage = "pre_tool_call plugin callback timed out or is still running"
)

// Wall clock bound on an awaited plugin command result when the caller serves a running loop
var pluginCommandAwaitTimeout = 30 * time.Second

var (
	hookTimeoutBoundedHooks = map[string]bool{
		"post_tool_call": true, "transform_terminal_output": true, "transform_tool_result": true,
		"transform_llm_output": true, "pre_llm_call": true, "post_llm_call": true, "pre_api_request": true,
		"post_api_request": true, "api_request_error": true, "pre_auxiliary_call": true,
		"post_auxiliary_call": true, "pre_verify": true, "on_session_start": true, "on_session_end": true,
		"post_gateway_admission": true, "pre_command": true, "post_compaction": true,
	}
	hookCallIdentityFields     = map[string][]string{"pre_command": {"session_key"}, "post_compaction": {"session_id"}}
	defaultHookIdentityFields  = []string{"tool_call_id", "turn_id"}
	hookTimeoutFailClosedHooks = map[string]bool{"pre_tool_call": true}
	hookCallerThreadHooks      = map[string]bool{"subagent_stop": true}
	pyFloatRe                  = regexp.MustCompile(`^[+-]?(?:(?:\d(?:_?\d)*(?:\.(?:\d(?:_?\d)*)?)?|\.\d(?:_?\d)*)(?:[eE][+-]?\d(?:_?\d)*)?|(?i:inf|infinity|nan))$`)
)

// Deferred result a hook, event or command callback returns for the dispatcher to await
type Awaitable interface {
	Await(ctx context.Context) (any, error)
}

// Awaitable backed by a plain function
type AwaitFunc func(ctx context.Context) (any, error)

// Runs the wrapped function
func (f AwaitFunc) Await(ctx context.Context) (any, error) { return f(ctx) }

// Error raised when an awaited plugin command result outlives its bound
type PluginCommandTimeoutError struct{ Seconds float64 }

func (e *PluginCommandTimeoutError) Error() string {
	return fmt.Sprintf("Plugin command async handler did not complete within %.0fs", e.Seconds)
}

// Marks a context as served by a goroutine that must never block on an awaited plugin result
func WithRunningLoop(ctx context.Context) context.Context { return pyctx.WithRunningLoop(ctx) }

// Reports whether the caller marked its context as a running loop
func runningLoop(ctx context.Context) bool { return pyctx.RunningLoop(ctx) }

// Error carrying a recovered callback panic
type CallbackPanicError struct{ Value any }

func (e *CallbackPanicError) Error() string { return fmt.Sprint(e.Value) }

// Runs a callback converting a panic into a returned error
func guardedValue(fn func() (any, error)) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if escaped, ok := interruptFromPanic(r); ok {
				v, err = nil, escaped
				return
			}
			v, err = nil, &CallbackPanicError{Value: r}
		}
	}()
	return fn()
}

// Awaits an awaitable result, bounded on a running loop, returning any other value unchanged
func ResolvePluginCommandResult(ctx context.Context, result any) (any, error) {
	aw, ok := result.(Awaitable)
	if !ok {
		return result, nil
	}
	await := func() (any, error) { return aw.Await(ctx) }
	if !runningLoop(ctx) {
		return guardedValue(await)
	}
	type outcome struct {
		value any
		err   error
	}
	limit := pluginCommandAwaitTimeout
	done := make(chan outcome, 1)
	go func() {
		v, err := guardedValue(await)
		done <- outcome{v, err}
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case o := <-done:
		return o.value, o.err
	case <-timer.C:
		return nil, &PluginCommandTimeoutError{Seconds: limit.Seconds()}
	}
}

// Call identity a callback fires for, from the hook specific fields or the tool call and turn ids
func hookCallIdentity(kwargs map[string]any, hookName string) (string, bool) {
	fields, ok := hookCallIdentityFields[hookName]
	if !ok {
		fields = defaultHookIdentityFields
	}
	for _, field := range fields {
		if s, ok := kwargs[field].(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// Reports whether a hook runs on the abandoning timeout worker path
func hookUsesCallbackTimeout(hookName string, timeout float64) bool {
	if timeout <= 0 || hookCallerThreadHooks[hookName] {
		return false
	}
	return hookTimeoutBoundedHooks[hookName] || hookTimeoutFailClosedHooks[hookName]
}

// Float conversion following the Python float builtin for numbers, booleans and numeric strings
func pyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(x)
		if !pyFloatRe.MatchString(s) {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
		if err != nil && !strings.Contains(err.Error(), "value out of range") {
			return 0, false
		}
		return f, true
	}
	return config.AsFloat(v)
}

// Python percent g rendering of a float
func pyG(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

// Effective hook callback timeout in seconds from plugins.hook_callback_timeout, clamped to the maximum
func (m *Manager) resolveHookCallbackTimeout() float64 {
	def := hookCallbackTimeoutSecs
	section, ok := loadConfigOrEmpty(m.home)["plugins"].(map[string]any)
	if !ok || section["hook_callback_timeout"] == nil {
		return def
	}
	timeout, ok := pyFloat(section["hook_callback_timeout"])
	if !ok || math.IsNaN(timeout) {
		logWarn(fmt.Sprintf("plugins.hook_callback_timeout is not a number; using default %ss", pyG(def)))
		return def
	}
	if timeout < 0 {
		logWarn(fmt.Sprintf("plugins.hook_callback_timeout=%s is negative; using default %ss", pyG(timeout), pyG(def)))
		return def
	}
	if timeout > maxHookCallbackTimeoutSecs {
		logWarn(fmt.Sprintf("plugins.hook_callback_timeout=%s exceeds max %ss; clamping", pyG(timeout), pyG(maxHookCallbackTimeoutSecs)))
		return maxHookCallbackTimeoutSecs
	}
	return timeout
}

// Block directive naming the callback and its truncated error for a fail closed hook
func policyErrorBlockDirective(hookName, callbackName string, err error) map[string]any {
	msg := err.Error()
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200])
	}
	return map[string]any{"action": "block",
		"message": fmt.Sprintf("%s plugin callback %s raised %s: %s", hookName, callbackName, errorTypeName(err), msg)}
}

// Short type name of an error, the Go counterpart of the exception class name
func errorTypeName(err error) string {
	switch e := err.(type) {
	case *HostBuiltinError:
		return e.Type
	case *PluginHostError:
		if e.TypeName != "" {
			return e.TypeName
		}
	case interface{ PythonTypeName() string }:
		return e.PythonTypeName()
	}
	name := fmt.Sprintf("%T", err)
	name = strings.TrimLeft(name, "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Calls one hook callback with its own payload copy and awaits an awaitable result
func invokeHookCallback(ctx context.Context, cb *hookEntry, kwargs map[string]any) (any, error) {
	v, err := guardedValue(func() (any, error) { return cb.fn(ctx, copyKwargs(kwargs)) })
	if err != nil {
		return nil, err
	}
	return ResolvePluginCommandResult(ctx, v)
}

// Snapshot of the callbacks registered for a hook
func (m *Manager) hookCallbacks(hookName string) []*hookEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*hookEntry(nil), m.hooks[hookName]...)
}

// Calls every callback of a hook and returns their non nil results, empty inside a safe worker
func (m *Manager) InvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	if config.SafeWorkerEnabled() {
		return []any{}
	}
	kwargs = copyKwargs(kwargs)
	if hookName != "gateway_platform_event" {
		if _, ok := kwargs["telemetry_schema_version"]; !ok {
			kwargs["telemetry_schema_version"] = ObserverSchemaVersion
		}
	}
	return m.runHookCallbacks(ctx, hookName, m.hookCallbacks(hookName), kwargs, nil)
}

// Loop of InvokeHook over callbacks, appending each one that failed or timed out to failed when given
func (m *Manager) runHookCallbacks(ctx context.Context, hookName string, callbacks []*hookEntry, kwargs map[string]any, failed *[]*hookEntry) []any {
	results := []any{}
	timeout := m.resolveHookCallbackTimeout()
	useTimeout := hookUsesCallbackTimeout(hookName, timeout)
	failClosed := hookTimeoutFailClosedHooks[hookName]
	for _, cb := range callbacks {
		var ret any
		var err error
		if useTimeout {
			var skipped bool
			ret, skipped, err = m.runHookCallbackBounded(ctx, hookName, cb, kwargs, timeout)
			if skipped {
				if failed != nil {
					*failed = append(*failed, cb)
				}
				if failClosed {
					results = append(results, map[string]any{"action": "block", "message": preToolCallTimeoutBlockMessage})
				}
				continue
			}
		} else {
			ret, err = invokeHookCallback(ctx, cb, kwargs)
		}
		if isUserInterrupt(ctx, err) {
			raiseUserInterrupt(err)
		}
		if err != nil {
			m.reportHookFailure(hookName, cb.name, kwargs, err, "Hook")
			if failed != nil {
				*failed = append(*failed, cb)
			}
			if failClosed {
				results = append(results, policyErrorBlockDirective(hookName, cb.name, err))
			}
			continue
		}
		if ret != nil {
			results = append(results, ret)
		}
	}
	return results
}

// Runs one callback on an abandonable worker with a wall clock cap, skipped when suppressed, busy or hung
func (m *Manager) runHookCallbackBounded(ctx context.Context, hookName string, cb *hookEntry, kwargs map[string]any, timeout float64) (any, bool, error) {
	supKey := hookSuppressionKey{hook: hookName, cb: cb}
	identity, has := hookCallIdentity(kwargs, hookName)
	gateKey := hookGateKey{hookSuppressionKey: supKey, identity: identity, hasIdentity: has}
	token := &struct{}{}
	m.hookTimeoutMu.Lock()
	until, suppressed := m.hookTimeoutSuppressedUntil[supKey]
	if m.hookRunningCallbacks[gateKey] != nil || (suppressed && until.After(time.Now())) {
		m.hookTimeoutMu.Unlock()
		logWarn(fmt.Sprintf("Hook '%s' callback %s skipped after previous timeout or while still running", hookName, cb.name))
		return nil, true, nil
	}
	if abandoned := m.hookAbandoned[supKey]; len(abandoned) >= hookMaxAbandonedWorkers {
		m.hookTimeoutMu.Unlock()
		module := cb.module
		logWarn(fmt.Sprintf("Hook '%s' callback %s (%s) skipped: %d abandoned worker(s) still running — the plugin is hung; fix or disable it (retried when a worker finishes)",
			hookName, cb.name, module, len(abandoned)))
		return nil, true, nil
	}
	if suppressed {
		delete(m.hookTimeoutSuppressedUntil, supKey)
	}
	m.hookRunningCallbacks[gateKey] = token
	m.hookTimeoutMu.Unlock()

	var value any
	var callErr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		value, callErr = invokeHookCallback(ctx, cb, kwargs)
		m.hookTimeoutMu.Lock()
		if m.hookRunningCallbacks[gateKey] == token {
			delete(m.hookRunningCallbacks, gateKey)
			if abandoned := m.hookAbandoned[supKey]; abandoned != nil {
				delete(abandoned, gateKey)
				if len(abandoned) == 0 {
					delete(m.hookAbandoned, supKey)
				}
			}
		}
		m.hookTimeoutMu.Unlock()
	}()
	if waitHookWorker(ctx, finished, time.Duration(timeout*float64(time.Second))) {
		return value, false, callErr
	}
	if err := ctx.Err(); err != nil {
		raiseUserInterrupt(err)
	}
	m.hookTimeoutMu.Lock()
	m.hookTimeoutSuppressedUntil[supKey] = time.Now().Add(m.hookTimeoutSuppression)
	if m.hookRunningCallbacks[gateKey] == token {
		if m.hookAbandoned[supKey] == nil {
			m.hookAbandoned[supKey] = map[hookGateKey]bool{}
		}
		m.hookAbandoned[supKey][gateKey] = true
	}
	m.hookTimeoutMu.Unlock()
	logWarn(fmt.Sprintf("Hook '%s' callback %s timed out after %ss — skipping", hookName, cb.name, pyG(timeout)))
	return nil, true, nil
}

// Waits for a hook worker to finish within the timeout, false when the caller abandons it
var waitHookWorker = func(ctx context.Context, finished <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// Hook invocation for callers on a running loop, awaiting awaitable results inline under the hook timeout
func (m *Manager) AInvokeHook(ctx context.Context, hookName string, kwargs map[string]any) []any {
	kwargs = copyKwargs(kwargs)
	if hookName != "gateway_platform_event" {
		if _, ok := kwargs["telemetry_schema_version"]; !ok {
			kwargs["telemetry_schema_version"] = ObserverSchemaVersion
		}
	}
	results := []any{}
	timeout := m.resolveHookCallbackTimeout()
	useTimeout := hookUsesCallbackTimeout(hookName, timeout)
	failClosed := hookTimeoutFailClosedHooks[hookName]
	for _, cb := range m.hookCallbacks(hookName) {
		ret, err := guardedValue(func() (any, error) { return cb.fn(ctx, copyKwargs(kwargs)) })
		if aw, ok := ret.(Awaitable); ok && err == nil {
			awaitCtx, cancel := ctx, context.CancelFunc(func() {})
			if useTimeout {
				awaitCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
			}
			ret, err = guardedValue(func() (any, error) { return aw.Await(awaitCtx) })
			timedOut := useTimeout && awaitCtx.Err() == context.DeadlineExceeded
			cancel()
			if timedOut {
				logWarn(fmt.Sprintf("Hook '%s' callback %s timed out after %.0fs", hookName, cb.name, timeout))
				if failClosed {
					results = append(results, map[string]any{"action": "block", "message": preToolCallTimeoutBlockMessage})
				}
				continue
			}
		}
		if isUserInterrupt(ctx, err) {
			raiseUserInterrupt(err)
		}
		if err != nil {
			m.reportHookFailure(hookName, cb.name, kwargs, err, "Hook")
			if failClosed {
				results = append(results, policyErrorBlockDirective(hookName, cb.name, err))
			}
			continue
		}
		if ret != nil {
			results = append(results, ret)
		}
	}
	return results
}

// Stable snapshot of a hook's callbacks, empty inside a safe worker
func (m *Manager) IterHookCallbacks(hookName string) []HookFunc {
	if config.SafeWorkerEnabled() {
		return nil
	}
	entries := m.hookCallbacks(hookName)
	out := make([]HookFunc, len(entries))
	for i, e := range entries {
		out[i] = e.fn
	}
	return out
}

// Output transform callbacks a held turn runs, captured with the hold decision, nil when nothing holds
func (m *Manager) HeldTransforms() []*hookEntry {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.holdsStreamLocked() {
		return nil
	}
	return append([]*hookEntry(nil), m.hooks[StreamHoldingHook]...)
}

// Runs a held turn's snapshotted transforms returning the first non empty rewrite and whether all failed
func (m *Manager) RunHeldTransforms(ctx context.Context, callbacks []*hookEntry, kwargs map[string]any) (string, bool) {
	if config.SafeWorkerEnabled() {
		return "", false
	}
	kwargs = copyKwargs(kwargs)
	if _, ok := kwargs["telemetry_schema_version"]; !ok {
		kwargs["telemetry_schema_version"] = ObserverSchemaVersion
	}
	var failed []*hookEntry
	for _, ret := range m.runHookCallbacks(ctx, StreamHoldingHook, callbacks, kwargs, &failed) {
		if s, ok := ret.(string); ok && s != "" {
			return s, false
		}
	}
	return "", len(failed) > 0
}

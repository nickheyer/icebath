package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"reflect"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const (
	RelaySessionScope             = "icebath.session"
	RelayTurnScope                = "icebath.turn"
	RelayLogicalLLMScope          = "icebath.logical_llm_call"
	RelayRuntimeSchemaKey         = "icebath.relay.schema_version"
	RelayRuntimeSchemaVersion     = "icebath.relay.runtime.v1"
	RelayRuntimeInstanceKey       = "icebath.relay.runtime_instance"
	RelayPluginsExecutionConsumer = "icebath.nemo_relay.plugins"
)

// Seconds a native scope op gating turn or session completion may run before it is abandoned
var RelayScopeOpTimeout = 10.0

var relayLogger = func() *slog.Logger { return slog.Default().With("logger", "agent.relay_runtime") }

// Native Relay binding surface the runtime drives, a nil function is an attribute the binding lacks
type NemoRelay struct {
	Scope               *NemoRelayScope
	ScopeType           NemoRelayScopeTypes
	GetScopeStack       func(ctx context.Context) (any, error)
	Plugin              *NemoRelayPlugin
	Subscribers         *NemoRelaySubscribers
	Tools               *NemoRelayTools
	LLM                 *NemoRelayLLM
	LLMRequest          func(headers, content map[string]any) (any, error)
	Codecs              *NemoRelayCodecs
	ToolExecutionResult func(result any) (*RelayToolExecutionResult, error)
}

// Scope stack operations of the binding
type NemoRelayScope struct {
	Push          func(ctx context.Context, name string, scopeType any, kwargs map[string]any) (any, error)
	Pop           func(ctx context.Context, handle any, kwargs map[string]any) (any, error)
	PopParameters func() ([]RelayParameter, error)
	GetHandle     func(ctx context.Context) (any, error)
	Event         func(ctx context.Context, name string, kwargs map[string]any) (any, error)
}

// One parameter of the binding pop signature
type RelayParameter struct {
	Name       string
	VarKeyword bool
}

// Scope type members the runtime pushes
type NemoRelayScopeTypes struct {
	Agent    any
	Function any
}

// Plugin host operations of the binding
type NemoRelayPlugin struct {
	Initialize    func(ctx context.Context, config map[string]any, additionalPluginsTOML string) (any, error)
	Validate      func(ctx context.Context, config map[string]any, additionalPluginsTOML string) (map[string]any, error)
	ValidateExact func(ctx context.Context, config map[string]any) (map[string]any, error)
}

// Subscriber operations of the binding, register, deregister, flush and flush_async
type NemoRelaySubscribers struct {
	Register   func(name string, callback func(event *RelayEventView)) error
	Deregister func(name string) error
	Flush      func() error
	FlushAsync func(ctx context.Context) (any, error)
}

// One event a binding subscriber receives, each field the matching attribute of the binding's event object
type RelayEventView struct {
	Kind, Name, Category, ScopeCategory, CategoryProfile, Metadata, Data any
}

// Tool pipeline operations of the binding, request intercepts, managed execution and the manual call and call_end pair
type NemoRelayTools struct {
	RequestIntercepts func(ctx context.Context, toolName string, args map[string]any) (any, error)
	Execute           func(ctx context.Context, toolName string, args any, callback RelayProviderCallback, kwargs map[string]any) (any, error)
	Call              func(ctx context.Context, name string, args map[string]any, kwargs map[string]any) (any, error)
	CallEnd           func(ctx context.Context, handle, result any, kwargs map[string]any) error
}

// Activation handle plugin initialization returns
type RelayActivation struct {
	Report func() (any, error)
	Close  func(ctx context.Context) (any, error)
}

// Deferred result of the async plugin API, resolved synchronously by the host
type RelayAwaitable func(ctx context.Context) (any, error)

// Scope handle exposing the native uuid that identifies it
type RelayHandleWithUUID interface{ UUID() any }

// Error the binding raised with its Python exception class
type RelayError struct {
	Class   string
	Message string
}

func (e *RelayError) Error() string { return e.Message }

// Bounded native call the runtime abandoned
type RelayTimeoutError struct {
	Message string
	Err     error
}

func (e *RelayTimeoutError) Error() string { return e.Message }
func (e *RelayTimeoutError) Unwrap() error { return e.Err }

// Explicitly selected Relay plugin configuration that could not be loaded
type RelayPluginConfigurationLoadError struct {
	Message string
	Err     error
}

func (e *RelayPluginConfigurationLoadError) Error() string { return e.Message }
func (e *RelayPluginConfigurationLoadError) Unwrap() error { return e.Err }

func relayAttributeError(object, name string) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", object, name)
}

var (
	relayBindingMu     sync.Mutex
	relayBindingLoader func() (*NemoRelay, error)
)

// Installs the native binding loader the runtime imports on first use
func RegisterNemoRelayBinding(load func() (*NemoRelay, error)) {
	relayBindingMu.Lock()
	relayBindingLoader = load
	relayBindingMu.Unlock()
}

// Loads the binding only when a producer or consumer needs Relay
var loadNemoRelay = func() (*NemoRelay, error) {
	relayBindingMu.Lock()
	load := relayBindingLoader
	relayBindingMu.Unlock()
	if load == nil {
		return nil, errors.New("No module named 'nemo_relay'")
	}
	return load()
}

// Imports the registered native Relay binding, a No module named error when none is registered
func LoadNemoRelay() (*NemoRelay, error) { return loadNemoRelay() }

type relayLazy[T any] struct {
	factory func(ctx context.Context) T
	mu      sync.Mutex
	value   T
	set     bool
}

func (l *relayLazy[T]) get(ctx context.Context) T {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.set {
		l.value, l.set = l.factory(ctx), true
	}
	return l.value
}

func (l *relayLazy[T]) reset() {
	l.mu.Lock()
	var zero T
	l.value, l.set = zero, false
	l.mu.Unlock()
}

type relayFuture interface {
	Done() <-chan struct{}
	Result(timeout time.Duration) (any, error)
}

type relayExecutor interface {
	Submit(ctx context.Context, fn func(ctx context.Context) (any, error)) (relayFuture, error)
}

type relayDaemonExecutor struct{ pool *DaemonPool }

func (e relayDaemonExecutor) Submit(ctx context.Context, fn func(ctx context.Context) (any, error)) (relayFuture, error) {
	f, err := e.pool.Submit(ctx, fn)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Daemon workers so a wedged call abandoned at timeout never blocks exit
func newRelayScopeOpExecutor(context.Context) relayExecutor {
	return relayDaemonExecutor{pool: NewDaemonPool(8, "relay-scope-op")}
}

var relayScopeOpExecutorLazy = &relayLazy[relayExecutor]{factory: newRelayScopeOpExecutor}

var relayScopeOpExecutor = func() relayExecutor { return relayScopeOpExecutorLazy.get(context.Background()) }

func relaySeconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Waits for a future up to timeout seconds and reports whether it was abandoned
func relayFutureResult(f relayFuture, timeout float64) (any, bool, error) {
	timer := time.NewTimer(relaySeconds(timeout))
	defer timer.Stop()
	select {
	case <-f.Done():
	case <-timer.C:
		return nil, true, nil
	}
	v, err := f.Result(0)
	return v, false, err
}

// Scope metadata stamping every icebath owned Relay scope
func RelayRuntimeMetadata(runtimeID string, extra map[string]any) map[string]any {
	out := map[string]any{RelayRuntimeSchemaKey: RelayRuntimeSchemaVersion, RelayRuntimeInstanceKey: runtimeID}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Relay scope input for a known logical working directory
func relayScopeInput(cwd string) map[string]any {
	if s := pystr.Strip(cwd); s != "" {
		return map[string]any{"cwd": s}
	}
	return map[string]any{}
}

// Runs fn on a fresh goroutine and abandons it with a timeout error once timeout seconds pass
func RelayRunOnDaemonThread(fn func() (any, error), name string, timeout *float64, timeoutMessage string) (any, error) {
	type outcome struct {
		v   any
		err error
	}
	done := make(chan outcome, 1)
	go pprof.Do(context.Background(), pprof.Labels("thread", name), func(context.Context) {
		v, err := fn()
		done <- outcome{v, err}
	})
	if timeout == nil {
		o := <-done
		return o.v, o.err
	}
	timer := time.NewTimer(relaySeconds(*timeout))
	defer timer.Stop()
	select {
	case o := <-done:
		return o.v, o.err
	case <-timer.C:
		return nil, &RelayTimeoutError{Message: timeoutMessage}
	}
}

// Pops a Relay scope forwarding only the keyword arguments the live binding accepts
func PopRelayScope(ctx context.Context, relay *NemoRelay, handle, output, metadata, timestamp any) (any, error) {
	if relay.Scope == nil {
		return nil, relayAttributeError("NemoRelay", "scope")
	}
	if relay.Scope.Pop == nil {
		return nil, relayAttributeError("NemoRelayScope", "pop")
	}
	kwargs := map[string]any{}
	for _, kv := range []struct {
		k string
		v any
	}{{"output", output}, {"metadata", metadata}, {"timestamp", timestamp}} {
		if kv.v != nil {
			kwargs[kv.k] = kv.v
		}
	}
	var params []RelayParameter
	if relay.Scope.PopParameters != nil {
		if p, err := relay.Scope.PopParameters(); err == nil {
			params = p
		}
	}
	if len(params) > 0 && !slices.ContainsFunc(params, func(p RelayParameter) bool { return p.VarKeyword }) {
		for k := range kwargs {
			if !slices.ContainsFunc(params, func(p RelayParameter) bool { return p.Name == k }) {
				delete(kwargs, k)
			}
		}
	}
	return relay.Scope.Pop(ctx, handle, kwargs)
}

// Pops handle only while it tops the scope stack and reports whether it was popped
func PopRelayScopeIfTop(ctx context.Context, relay *NemoRelay, handle, output, metadata any) (bool, error) {
	top, err := relayCurrentTop(ctx, relay)
	if err != nil {
		return false, err
	}
	if top != nil && !relaySameHandle(top, handle) {
		return false, nil
	}
	if _, err := PopRelayScope(ctx, relay, handle, output, metadata, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Current top of stack scope handle preferring the binding handle getter
func relayCurrentTop(ctx context.Context, relay *NemoRelay) (any, error) {
	if relay.Scope != nil && relay.Scope.GetHandle != nil {
		if h, err := relay.Scope.GetHandle(ctx); err == nil {
			return h, nil
		}
	}
	if relay.GetScopeStack == nil {
		return nil, relayAttributeError("NemoRelay", "get_scope_stack")
	}
	top, err := relay.GetScopeStack(ctx)
	if err != nil {
		return nil, err
	}
	if list, ok := top.([]any); ok {
		if len(list) == 0 {
			return nil, nil
		}
		return list[len(list)-1], nil
	}
	return top, nil
}

func relayIdentical(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// Same scope handle by identity, equality or native uuid
func relaySameHandle(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if relayIdentical(a, b) || pyrepr.Equal(a, b) {
		return true
	}
	ua, ok := a.(RelayHandleWithUUID)
	if !ok || ua.UUID() == nil {
		return false
	}
	ub, ok := b.(RelayHandleWithUUID)
	return ok && pyrepr.Equal(ua.UUID(), ub.UUID())
}

type relayPluginState int

const (
	relayPluginUninitialized relayPluginState = iota
	relayPluginDisabled
	relayPluginActive
	relayPluginForeign
	relayPluginFailed
)

// Whether Relay activated any static or dynamic plugin behaviour
func relayActivationRequiresManagedExecution(ctx context.Context, activation any) bool {
	var report any
	if a, ok := activation.(*RelayActivation); ok && a != nil && a.Report != nil {
		if r, err := a.Report(); err == nil {
			report = r
		}
	}
	return relayReportRequiresManagedExecution(ctx, report)
}

// Plugins toml files an activation loaded, lowest precedence first
func relayActivationConfigPaths(activation any) []string {
	a, ok := activation.(*RelayActivation)
	if !ok || a == nil || a.Report == nil {
		return nil
	}
	report, err := a.Report()
	if err != nil {
		return nil
	}
	m, ok := report.(map[string]any)
	if !ok {
		return nil
	}
	paths, ok := m["config_paths"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, pyrepr.Str(p))
	}
	return out
}

// Whether a plugin host report enables any plugin, an unfamiliar report keeping managed execution on
func relayReportRequiresManagedExecution(ctx context.Context, report any) bool {
	keepEnabled := func() bool {
		relayLogger().WarnContext(ctx, "Icebath could not determine whether the Relay plugin configuration is empty; keeping managed execution enabled")
		return true
	}
	m, ok := report.(map[string]any)
	if !ok {
		return keepEnabled()
	}
	resolved, ok := m["resolved_config"].(map[string]any)
	dynamicPlugins, okDyn := m["dynamic_plugins"].([]any)
	if !ok || !okDyn {
		return keepEnabled()
	}
	components, ok := resolved["components"].([]any)
	if !ok {
		return keepEnabled()
	}
	for _, c := range components {
		component, ok := c.(map[string]any)
		if !ok {
			return keepEnabled()
		}
		enabled, ok := component["enabled"].(bool)
		if !ok {
			return keepEnabled()
		}
		if enabled {
			return true
		}
	}
	for _, p := range dynamicPlugins {
		plugin, ok := p.(map[string]any)
		if !ok {
			return keepEnabled()
		}
		raw, present := plugin["selected"]
		if !present {
			return keepEnabled()
		}
		selected, ok := raw.(bool)
		if !ok {
			return keepEnabled()
		}
		if selected {
			return true
		}
	}
	return false
}

// One icebath session's isolated Relay scope stack
type RelaySession struct {
	SessionID       string
	ParentSessionID string
	Mu              sync.Mutex
	Closing         bool
	Handle          any
	Context         *pyctx.Context
	Segment         int
	SegmentTurns    int
	RotatePending   bool
	ClosePending    bool
	Cwd             string
}

// Handle of the current session scope read under the session lock
func (s *RelaySession) CurrentHandle() any {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.Handle
}

// Reads gateway.telemetry.session_segments where both defaults off mean rotation never fires
var relayLoadUserConfigEffective = func(ctx context.Context) (map[string]any, error) {
	return config.LoadUserEffective(home.ConfigPath(home.Dir(ctx)), os.LookupEnv)
}

type relaySegmentsConfig struct {
	onCompaction bool
	maxTurns     int
}

func relayLoadSegmentsConfig(ctx context.Context) relaySegmentsConfig {
	out := relaySegmentsConfig{}
	cfg, err := relayLoadUserConfigEffective(ctx)
	if err != nil {
		return out
	}
	gateway, _ := cfg["gateway"].(map[string]any)
	telemetry, _ := gateway["telemetry"].(map[string]any)
	segments, _ := telemetry["session_segments"].(map[string]any)
	out.onCompaction = pyrepr.Truthy(segments["on_compaction"])
	raw := segments["max_turns"]
	if !pyrepr.Truthy(raw) {
		raw = 0
	}
	if n, err := pyint.Of(raw); err == nil && n.Sign() > 0 {
		out.maxTurns = math.MaxInt
		if n.IsInt64() && n.Int64() < math.MaxInt {
			out.maxTurns = int(n.Int64())
		}
	}
	return out
}

var relaySegmentsConfigLazy = &relayLazy[relaySegmentsConfig]{factory: relayLoadSegmentsConfig}

// Clears the cached segments configuration so the next read reloads it
func ResetRelaySegmentsConfigForTests() { relaySegmentsConfigLazy.reset() }

// Keeps process wide Relay plugins alive across a deferred operation
type RelayOperationLease struct {
	mu      sync.Mutex
	runtime *RelayRuntime
}

// Runs cleanup while this lease still owns the runtime lifetime
func (l *RelayOperationLease) RunInSession(ctx context.Context, session *RelaySession, callback func(ctx context.Context) (any, error), allowClosing bool, timeout *float64) (any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runtime == nil {
		return nil, errors.New("Icebath Relay operation lease is released")
	}
	return l.runtime.runInSessionUntracked(ctx, session, callback, allowClosing, timeout, false)
}

// Releases this lease exactly once
func (l *RelayOperationLease) Release() {
	l.mu.Lock()
	rt := l.runtime
	l.runtime = nil
	l.mu.Unlock()
	if rt != nil {
		rt.endOperation()
	}
}

// One Relay plugin configuration shared by every profile scoped host of the process
type relayPluginConfiguration struct {
	mu         sync.Mutex
	owners     map[*RelayRuntime]bool
	state      relayPluginState
	relay      *NemoRelay
	activation any
}

func (p *relayPluginConfiguration) acquire(ctx context.Context, owner *RelayRuntime, relay *NemoRelay) relayPluginState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.owners) == 0 {
		if st, terminal := p.preflight(ctx); terminal {
			p.state = st
		} else {
			p.state = p.activate(ctx, relay)
		}
		if p.state == relayPluginActive {
			paths := strings.Join(relayActivationConfigPaths(p.activation), "; ")
			if paths == "" {
				paths = "none reported"
			}
			relayLogger().InfoContext(ctx, "The Relay plugin host is active process-wide and applies to all profiles hosted by this Icebath process. Configuration files: "+paths)
		}
	}
	p.owners[owner] = true
	return p.state
}

func (p *relayPluginConfiguration) activate(ctx context.Context, relay *NemoRelay) relayPluginState {
	ok, err := p.initialize(ctx, relay)
	if err != nil {
		p.activation = nil
		if relayIsHostConflict(err) {
			relayLogger().WarnContext(ctx, "A process-global Relay plugin configuration is already active outside Icebath native ownership; leaving it unchanged and disabling Icebath-managed Relay middleware for this process")
			return relayPluginForeign
		}
		relayLogger().WarnContext(ctx, "Icebath Relay plugin initialization failed: "+err.Error(), "err", err)
		return relayPluginFailed
	}
	if !ok {
		return relayPluginDisabled
	}
	p.relay = relay
	return relayPluginActive
}

// Terminal state when the process cannot take ownership
func (p *relayPluginConfiguration) preflight(ctx context.Context) (relayPluginState, bool) {
	if p.relay != nil && !p.clearActive(ctx) {
		relayLogger().WarnContext(ctx, "Icebath Relay plugin cleanup is still pending; refusing to replace the process-global configuration")
		return relayPluginFailed, true
	}
	return relayPluginUninitialized, false
}

// Initializes Relay and reports whether the resolved configuration needs managed execution
func (p *relayPluginConfiguration) initialize(ctx context.Context, relay *NemoRelay) (bool, error) {
	configPath, err := relayConfiguredPluginInputs(ctx)
	if err != nil {
		return false, err
	}
	if relay.Plugin == nil {
		return false, relayAttributeError("NemoRelay", "plugin")
	}
	if relay.Plugin.Initialize == nil {
		return false, relayAttributeError("NemoRelayPlugin", "initialize")
	}
	raw, err := relay.Plugin.Initialize(ctx, map[string]any{}, configPath)
	if err != nil {
		return false, err
	}
	activation, err := relayResolvePluginAwaitable(ctx, raw)
	if err != nil {
		return false, err
	}
	if activation == nil {
		return false, errors.New("NeMo Relay plugin initialization returned no activation handle")
	}
	p.activation = activation
	p.relay = relay
	return relayActivationRequiresManagedExecution(ctx, activation), nil
}

// Releases one host and clears Relay after the final host exits
func (p *relayPluginConfiguration) release(ctx context.Context, owner *RelayRuntime) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owners[owner] {
		delete(p.owners, owner)
		p.retryPendingCleanupLocked(ctx)
	}
}

func (p *relayPluginConfiguration) resetForTests(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.owners)
	p.retryPendingCleanupLocked(ctx)
}

// Retries a failed final cleanup without disrupting live owners
func (p *relayPluginConfiguration) retryPendingCleanup(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retryPendingCleanupLocked(ctx)
}

func (p *relayPluginConfiguration) retryPendingCleanupLocked(ctx context.Context) {
	if len(p.owners) == 0 && p.clearActive(ctx) {
		p.state = relayPluginUninitialized
	}
}

func (p *relayPluginConfiguration) clearActive(ctx context.Context) bool {
	relay, activation := p.relay, p.activation
	if relay == nil {
		return true
	}
	steps := []struct {
		what string
		run  func() error
	}{
		{"subscriber flush", func() error {
			if relay.Subscribers == nil || relay.Subscribers.FlushAsync == nil {
				return relayAttributeError("NemoRelaySubscribers", "flush_async")
			}
			v, err := relay.Subscribers.FlushAsync(ctx)
			if err != nil {
				return err
			}
			_, err = relayResolvePluginAwaitable(ctx, v)
			return err
		}},
		{"configuration cleanup", func() error {
			a, ok := activation.(*RelayActivation)
			if !ok || a == nil || a.Close == nil {
				return errors.New("NeMo Relay plugin activation has no close method")
			}
			v, err := a.Close(ctx)
			if err != nil {
				return err
			}
			_, err = relayResolvePluginAwaitable(ctx, v)
			return err
		}},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			relayLogger().WarnContext(ctx, "Icebath Relay plugin "+step.what+" failed", "err", err)
			return false
		}
	}
	p.relay, p.activation = nil, nil
	return true
}

var relayPlugins = &relayPluginConfiguration{owners: map[*RelayRuntime]bool{}}

type relayAtExitEntry struct{ fn func() }

var (
	relayAtExitMu    sync.Mutex
	relayAtExitFuncs []*relayAtExitEntry
)

func relayAtExitRegister(fn func()) *relayAtExitEntry {
	e := &relayAtExitEntry{fn: fn}
	relayAtExitMu.Lock()
	relayAtExitFuncs = append(relayAtExitFuncs, e)
	relayAtExitMu.Unlock()
	return e
}

func relayAtExitUnregister(e *relayAtExitEntry) {
	relayAtExitMu.Lock()
	relayAtExitFuncs = slices.DeleteFunc(relayAtExitFuncs, func(x *relayAtExitEntry) bool { return x == e })
	relayAtExitMu.Unlock()
}

// Runs the Relay exit handlers newest first, every runtime shutdown then the final plugin cleanup retry
func RelayAtExit() {
	relayAtExitMu.Lock()
	funcs := slices.Clone(relayAtExitFuncs)
	relayAtExitFuncs = nil
	relayAtExitMu.Unlock()
	for i := len(funcs) - 1; i >= 0; i-- {
		funcs[i].fn()
	}
}

var _ = relayAtExitRegister(func() { relayPlugins.retryPendingCleanup(context.Background()) })

// Owns Relay session scopes and the process plugin configuration
type RelayRuntime struct {
	Relay                 *NemoRelay
	ProfileKey            string
	RuntimeID             string
	sessionsMu            sync.Mutex
	consumersMu           sync.Mutex
	sessions              *pydict.Dict[string, *RelaySession]
	subagentParents       map[string]string
	subagentParentHandles map[string]any
	executionConsumers    map[string]bool
	deferredMu            sync.Mutex
	deferredScopeOutputs  map[string]map[string]any
	closing               bool
	shutdownStarted       bool
	shutdownComplete      *pythread.Event
	operationsIdle        *pythread.Event
	activeOperations      int
	pluginState           relayPluginState
	pluginRegistered      bool
	atExit                *relayAtExitEntry
}

// Builds a runtime over relay, loading the binding when nil, keyed by profileKey or the active profile
func NewRelayRuntime(ctx context.Context, relay *NemoRelay, profileKey string) (*RelayRuntime, error) {
	if relay == nil {
		loaded, err := loadNemoRelay()
		if err != nil {
			return nil, err
		}
		relay = loaded
	}
	if profileKey == "" {
		profileKey = CurrentRelayProfileKey(ctx)
	}
	r := &RelayRuntime{
		Relay: relay, ProfileKey: profileKey, RuntimeID: strings.ReplaceAll(uuid.NewString(), "-", ""),
		sessions: pydict.New[string, *RelaySession](), subagentParents: map[string]string{},
		subagentParentHandles: map[string]any{}, executionConsumers: map[string]bool{},
		deferredScopeOutputs: map[string]map[string]any{},
		shutdownComplete:     &pythread.Event{}, operationsIdle: &pythread.Event{},
	}
	r.operationsIdle.Set()
	r.pluginState = relayPlugins.acquire(ctx, r, relay)
	r.pluginRegistered = true
	if r.pluginsActive() {
		if err := r.RetainManagedExecution(RelayPluginsExecutionConsumer); err != nil {
			return nil, err
		}
	}
	r.atExit = relayAtExitRegister(r.Shutdown)
	return r, nil
}

func (r *RelayRuntime) pluginsActive() bool { return r.pluginState == relayPluginActive }

func relayHandleKey(handle any) (string, bool) {
	h, ok := handle.(RelayHandleWithUUID)
	if !ok || h.UUID() == nil {
		return "", false
	}
	return pyrepr.Str(h.UUID()), true
}

// Keeps the output of a finished scope a concurrent scope still covers for the orphan drain to close it with
func (r *RelayRuntime) DeferScopeOutput(handle any, output map[string]any) {
	if key, ok := relayHandleKey(handle); ok {
		r.deferredMu.Lock()
		r.deferredScopeOutputs[key] = output
		r.deferredMu.Unlock()
	}
}

func (r *RelayRuntime) takeDeferredScopeOutput(handle any) map[string]any {
	key, ok := relayHandleKey(handle)
	if !ok {
		return nil
	}
	r.deferredMu.Lock()
	defer r.deferredMu.Unlock()
	out, ok := r.deferredScopeOutputs[key]
	if ok {
		delete(r.deferredScopeOutputs, key)
	}
	return out
}

// Keeps managed LLM and tool execution active for one consumer
func (r *RelayRuntime) RetainManagedExecution(consumer string) error {
	if consumer == "" {
		return errors.New("Relay managed-execution consumer must not be empty")
	}
	r.consumersMu.Lock()
	r.executionConsumers[consumer] = true
	r.consumersMu.Unlock()
	return nil
}

// Releases a consumer's managed execution requirement
func (r *RelayRuntime) ReleaseManagedExecution(consumer string) {
	r.consumersMu.Lock()
	delete(r.executionConsumers, consumer)
	r.consumersMu.Unlock()
}

// Whether an icebath managed consumer needs the Relay pipeline
func (r *RelayRuntime) ManagedExecutionEnabled() bool {
	r.consumersMu.Lock()
	defer r.consumersMu.Unlock()
	return len(r.executionConsumers) > 0
}

// Pushes a fresh session scope bounded by the scope op timeout, parenting subagents under their spawner
func (r *RelayRuntime) openSessionScope(ctx context.Context, session *RelaySession, scopeMetadata map[string]any, resolveParent, exitFallback bool, data any) error {
	var parentHandle any
	if session.ParentSessionID != "" {
		r.sessionsMu.Lock()
		parentHandle = r.subagentParentHandles[session.SessionID]
		r.sessionsMu.Unlock()
		if parentHandle == nil && resolveParent {
			parent, err := r.EnsureSession(ctx, map[string]any{"session_id": session.ParentSessionID}, RelaySessionOptions{})
			if err != nil {
				return err
			}
			if parent != nil {
				parentHandle = parent.CurrentHandle()
			}
		}
		scopeMetadata["nemo_relay_scope_role"] = "subagent"
	}
	if r.Relay.Scope == nil || r.Relay.Scope.Push == nil {
		return relayAttributeError("NemoRelayScope", "push")
	}
	push := r.Relay.Scope.Push
	scopeCtx := pyctx.New()
	kwargs := map[string]any{"handle": parentHandle, "metadata": scopeMetadata, "input": relayScopeInput(session.Cwd)}
	if data != nil {
		kwargs["data"] = data
	}
	run := func(c context.Context) (any, error) {
		return push(pyctx.With(c, scopeCtx), RelaySessionScope, r.Relay.ScopeType.Agent, kwargs)
	}
	future, err := relayScopeOpExecutor().Submit(ctx, run)
	if err != nil {
		if !exitFallback {
			return err
		}
		h, err := run(ctx)
		if err != nil {
			return err
		}
		session.Handle = h
	} else {
		v, timedOut, err := relayFutureResult(future, RelayScopeOpTimeout)
		if timedOut {
			return &RelayTimeoutError{Message: ""}
		}
		if err != nil {
			return err
		}
		session.Handle = v
	}
	session.Context = scopeCtx
	return nil
}

// Optional session scope inputs of EnsureSession and RegisterSubagent
type RelaySessionOptions struct {
	Data     any
	Metadata map[string]any
	Cwd      *string
}

func relayEventSessionID(event map[string]any, key string) string {
	if v := event[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Existing session scope or one created once, nil for a missing id or a closing runtime or session
func (r *RelayRuntime) EnsureSession(ctx context.Context, event map[string]any, o RelaySessionOptions) (*RelaySession, error) {
	sessionID := relayEventSessionID(event, "session_id")
	if sessionID == "" {
		return nil, nil
	}
	r.sessionsMu.Lock()
	if r.closing {
		r.sessionsMu.Unlock()
		return nil, nil
	}
	session, _ := r.sessions.Get(sessionID)
	if session == nil {
		session = &RelaySession{SessionID: sessionID, ParentSessionID: r.subagentParents[sessionID]}
		r.sessions.Set(sessionID, session)
	}
	r.sessionsMu.Unlock()
	session.Mu.Lock()
	defer session.Mu.Unlock()
	if session.Closing {
		return nil, nil
	}
	if o.Cwd != nil {
		session.Cwd, _ = relayScopeInput(*o.Cwd)["cwd"].(string)
	}
	if session.Handle == nil {
		metadata := map[string]any{}
		for k, v := range o.Metadata {
			metadata[k] = v
		}
		for k, v := range RelayRuntimeMetadata(r.RuntimeID, nil) {
			metadata[k] = v
		}
		if err := r.openSessionScope(ctx, session, metadata, true, true, o.Data); err != nil {
			return nil, err
		}
	}
	return session, nil
}

// Closes the current session scope and opens the next segment, called only at a turn boundary
func (r *RelayRuntime) RotateSessionScope(ctx context.Context, session *RelaySession, reason string) {
	session.Mu.Lock()
	defer session.Mu.Unlock()
	if session.Closing || session.Handle == nil {
		return
	}
	oldHandle := session.Handle
	session.Segment++
	session.SegmentTurns = 0
	session.RotatePending = false
	timeout := RelayScopeOpTimeout
	var err error
	if r.Relay.Scope == nil || r.Relay.Scope.Pop == nil {
		err = relayAttributeError("NemoRelayScope", "pop")
	} else {
		pop := r.Relay.Scope.Pop
		_, err = r.runInSession(ctx, session, func(c context.Context) (any, error) {
			return pop(c, oldHandle, map[string]any{
				"output":   map[string]any{"icebath.session.segment_reason": reason},
				"metadata": RelayRuntimeMetadata(r.RuntimeID, nil),
			})
		}, false, &timeout, true)
	}
	if err != nil {
		relayLogger().WarnContext(ctx, fmt.Sprintf("Icebath Relay segment close failed (session=%s segment=%d); abandoning the old segment span", session.SessionID, session.Segment-1), "err", err)
	}
	metadata := RelayRuntimeMetadata(r.RuntimeID, map[string]any{"icebath.session.segment": session.Segment, "icebath.session.segment_reason": reason})
	if err := r.openSessionScope(ctx, session, metadata, false, false, nil); err != nil {
		relayLogger().WarnContext(ctx, fmt.Sprintf("Icebath Relay segment open failed (session=%s segment=%d); keeping the prior scope handle", session.SessionID, session.Segment), "err", err)
	}
}

// Opens a child agent scope under its spawning turn when one is live
func (r *RelayRuntime) RegisterSubagent(ctx context.Context, event map[string]any, o RelaySessionOptions) (*RelaySession, error) {
	parentID := relayEventSessionID(event, "parent_session_id")
	childID := relayEventSessionID(event, "child_session_id")
	if parentID == "" || childID == "" || parentID == childID {
		return nil, nil
	}
	parent, err := r.EnsureSession(ctx, map[string]any{"session_id": parentID}, RelaySessionOptions{})
	if err != nil {
		return nil, err
	}
	var parentHandle any
	if parent != nil {
		parentHandle = parent.CurrentHandle()
	}
	if turn := ActiveRelayTurn(ctx, &parentID); turn != nil && turn.Handle != nil && turn.Lease.Host == RelayHost(r) {
		parentHandle = turn.Handle
	}
	r.sessionsMu.Lock()
	if r.closing {
		r.sessionsMu.Unlock()
		return nil, nil
	}
	r.subagentParents[childID] = parentID
	if parentHandle != nil {
		r.subagentParentHandles[childID] = parentHandle
	}
	r.sessionsMu.Unlock()
	return r.EnsureSession(ctx, map[string]any{"session_id": childID}, RelaySessionOptions{Metadata: o.Metadata, Cwd: o.Cwd})
}

// Closes a delegated session and forgets its parent relationship
func (r *RelayRuntime) UnregisterSubagent(ctx context.Context, event map[string]any) {
	if childID := relayEventSessionID(event, "child_session_id"); childID != "" {
		r.CloseSession(ctx, map[string]any{"session_id": childID})
		r.forgetSubagent(childID)
	}
}

func (r *RelayRuntime) forgetSubagent(sessionID string) {
	r.sessionsMu.Lock()
	r.forgetSubagentLocked(sessionID)
	r.sessionsMu.Unlock()
}

func (r *RelayRuntime) forgetSubagentLocked(sessionID string) {
	delete(r.subagentParents, sessionID)
	delete(r.subagentParentHandles, sessionID)
}

// Registry lookup including closing sessions without creating one
func (r *RelayRuntime) LookupSession(sessionID string) *RelaySession {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	session, _ := r.sessions.Get(sessionID)
	return session
}

// Active session without creating one
func (r *RelayRuntime) GetSession(sessionID string) *RelaySession {
	r.sessionsMu.Lock()
	var session *RelaySession
	if !r.closing {
		session, _ = r.sessions.Get(sessionID)
	}
	r.sessionsMu.Unlock()
	if session == nil {
		return nil
	}
	session.Mu.Lock()
	defer session.Mu.Unlock()
	if session.Closing {
		return nil
	}
	return session
}

// Copy of the caller context overlaid with the session's saved Relay variables
func (r *RelayRuntime) sessionContext(ctx context.Context, session *RelaySession, allowClosing, held bool) (*pyctx.Context, error) {
	if !held {
		session.Mu.Lock()
		defer session.Mu.Unlock()
	}
	if session.Closing && !allowClosing {
		return nil, errors.New("Icebath Relay session is closing")
	}
	if session.Context == nil || session.Handle == nil {
		return nil, errors.New("Icebath Relay session context is unavailable")
	}
	out := pyctx.CopyContext(ctx)
	runCtx := pyctx.With(ctx, out)
	for _, item := range session.Context.Copy().Items() {
		item.Var.Set(runCtx, item.Value)
	}
	return out, nil
}

// Runs a Relay operation on a session's scope stack, timeout seconds bounding it on the executor
func (r *RelayRuntime) RunInSession(ctx context.Context, session *RelaySession, callback func(ctx context.Context) (any, error), allowClosing bool, timeout *float64) (any, error) {
	return r.runInSession(ctx, session, callback, allowClosing, timeout, false)
}

func (r *RelayRuntime) runInSession(ctx context.Context, session *RelaySession, callback func(ctx context.Context) (any, error), allowClosing bool, timeout *float64, held bool) (any, error) {
	if err := r.beginOperation(); err != nil {
		return nil, err
	}
	defer r.endOperation()
	return r.runInSessionUntracked(ctx, session, callback, allowClosing, timeout, held)
}

// Runs inside a session whose host level lifetime is already held
func (r *RelayRuntime) runInSessionUntracked(ctx context.Context, session *RelaySession, callback func(ctx context.Context) (any, error), allowClosing bool, timeout *float64, held bool) (any, error) {
	scopeCtx, err := r.sessionContext(ctx, session, allowClosing, held)
	if err != nil {
		return nil, err
	}
	invoke := func(c context.Context) (any, error) {
		c = pyctx.With(c, scopeCtx)
		if r.Relay.GetScopeStack == nil {
			return nil, relayAttributeError("NemoRelay", "get_scope_stack")
		}
		if _, err := r.Relay.GetScopeStack(c); err != nil {
			return nil, err
		}
		return callback(c)
	}
	if timeout == nil {
		return invoke(ctx)
	}
	exceeded := "Relay scope operation exceeded " + pyrepr.Repr(*timeout) + "s"
	future, err := relayScopeOpExecutor().Submit(ctx, invoke)
	if err != nil {
		return RelayRunOnDaemonThread(func() (any, error) { return invoke(ctx) }, "relay-scope-op-exit", timeout,
			exceeded+" during interpreter shutdown; abandoning the native call so process exit can proceed")
	}
	v, timedOut, err := relayFutureResult(future, *timeout)
	if timedOut {
		return nil, &RelayTimeoutError{Message: exceeded + " (session=" + session.SessionID + "); abandoning the native call so the agent can continue — the span for this scope is lost", Err: context.DeadlineExceeded}
	}
	return v, err
}

// Runs and awaits an operation inside the session's saved context
func (r *RelayRuntime) RunInSessionAsync(ctx context.Context, session *RelaySession, callback func(ctx context.Context) (any, error), allowClosing bool) (any, error) {
	if err := r.beginOperation(); err != nil {
		return nil, err
	}
	defer r.endOperation()
	scopeCtx, err := r.sessionContext(ctx, session, allowClosing, false)
	if err != nil {
		return nil, err
	}
	c := pyctx.With(ctx, scopeCtx)
	if r.Relay.GetScopeStack == nil {
		return nil, relayAttributeError("NemoRelay", "get_scope_stack")
	}
	if _, err := r.Relay.GetScopeStack(c); err != nil {
		return nil, err
	}
	v, err := callback(c)
	if err != nil {
		return nil, err
	}
	if aw, ok := v.(RelayAwaitable); ok {
		return aw(c)
	}
	return v, nil
}

// Admits one Relay call while keeping process plugins alive
func (r *RelayRuntime) beginOperation() error {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.closing {
		return errors.New("Icebath Relay runtime is shutting down")
	}
	r.activeOperations++
	r.operationsIdle.Clear()
	return nil
}

func (r *RelayRuntime) endOperation() {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	r.activeOperations--
	if r.activeOperations == 0 {
		r.operationsIdle.Set()
	}
}

// Retains plugin lifetime for work that outlives one Relay call
func (r *RelayRuntime) AcquireOperationLease() (*RelayOperationLease, error) {
	if err := r.beginOperation(); err != nil {
		return nil, err
	}
	return &RelayOperationLease{runtime: r}, nil
}

// Applies Relay request rewriting before icebath authorizes a tool call
func (r *RelayRuntime) ApplyToolRequestIntercepts(ctx context.Context, sessionID, toolName string, args map[string]any) (map[string]any, error) {
	var intercepts func(ctx context.Context, toolName string, args map[string]any) (any, error)
	if r.Relay.Tools != nil {
		intercepts = r.Relay.Tools.RequestIntercepts
	}
	if !r.ManagedExecutionEnabled() || intercepts == nil {
		return args, nil
	}
	session, err := r.EnsureSession(ctx, map[string]any{"session_id": sessionID}, RelaySessionOptions{})
	if err != nil {
		return nil, err
	}
	if session == nil {
		return args, nil
	}
	result, err := r.RunInSession(ctx, session, func(c context.Context) (any, error) { return intercepts(c, toolName, args) }, false, nil)
	if err != nil {
		return nil, err
	}
	if m, ok := result.(map[string]any); ok {
		return m, nil
	}
	return args, nil
}

// Pops handle, draining orphans above it and retrying once, inside one session callback
func (r *RelayRuntime) popWithDrain(ctx context.Context, handle any, output, metadata map[string]any, sessionRoot any, drainLimit int) error {
	if _, err := PopRelayScope(ctx, r.Relay, handle, output, metadata, nil); err == nil {
		return nil
	}
	drained := 0
	for range drainLimit {
		top, err := relayCurrentTop(ctx, r.Relay)
		if err != nil {
			return err
		}
		if top == nil || relaySameHandle(top, handle) {
			break
		}
		if sessionRoot != nil && relaySameHandle(top, sessionRoot) && !relayIdentical(handle, sessionRoot) {
			break
		}
		orphanOutput := r.takeDeferredScopeOutput(top)
		if orphanOutput == nil {
			orphanOutput = map[string]any{"outcome": "cancelled", "icebath.orphan_drain": true}
		}
		if _, err := PopRelayScope(ctx, r.Relay, top, orphanOutput, metadata, nil); err != nil {
			relayLogger().WarnContext(ctx, "Icebath Relay orphaned scope drain failed", "err", err)
			break
		}
		drained++
	}
	if drained > 0 {
		relayLogger().WarnContext(ctx, fmt.Sprintf("Icebath Relay drained %d orphaned scope(s) before closing %s", drained, pyrepr.Str(handle)))
	}
	_, err := PopRelayScope(ctx, r.Relay, handle, output, metadata, nil)
	return err
}

// Pops handle with the bounded orphan drain and returns the labelled failure or empty
func (r *RelayRuntime) closeScopeHandle(ctx context.Context, session *RelaySession, handle any, output map[string]any, allowClosing bool, failureLabel string, operationHeld, sessionHeld bool) string {
	if handle == nil {
		return ""
	}
	if output == nil {
		output = map[string]any{}
	}
	var sessionRoot any
	if sessionHeld {
		sessionRoot = session.Handle
	} else {
		sessionRoot = session.CurrentHandle()
	}
	timeout := RelayScopeOpTimeout
	callback := func(c context.Context) (any, error) {
		return nil, r.popWithDrain(c, handle, output, RelayRuntimeMetadata(r.RuntimeID, nil), sessionRoot, 32)
	}
	var err error
	if operationHeld {
		_, err = r.runInSessionUntracked(ctx, session, callback, allowClosing, &timeout, sessionHeld)
	} else {
		_, err = r.runInSession(ctx, session, callback, allowClosing, &timeout, sessionHeld)
	}
	if err == nil {
		return ""
	}
	return failureLabel + ": " + err.Error()
}

// Closes one session scope and removes it from the registry, doing nothing once shutting down
func (r *RelayRuntime) CloseSession(ctx context.Context, event map[string]any) {
	if r.beginOperation() != nil {
		return
	}
	defer r.endOperation()
	r.closeSession(ctx, event)
}

// Closes one session already admitted by the host lifecycle gate
func (r *RelayRuntime) closeSession(ctx context.Context, event map[string]any) {
	sessionID := relayEventSessionID(event, "session_id")
	session := r.LookupSession(sessionID)
	if session == nil {
		r.forgetSubagent(sessionID)
		return
	}
	failure := ""
	session.Mu.Lock()
	if session.Closing {
		session.Mu.Unlock()
		return
	}
	session.Closing = true
	if session.Handle != nil {
		failure = r.closeScopeHandle(ctx, session, session.Handle, map[string]any{}, true, "session scope close failed", true, true)
	}
	session.Mu.Unlock()
	r.sessionsMu.Lock()
	if current, _ := r.sessions.Get(sessionID); current == session {
		r.sessions.Delete(sessionID)
	}
	r.forgetSubagentLocked(sessionID)
	r.sessionsMu.Unlock()
	if failure != "" {
		relayLogger().WarnContext(ctx, fmt.Sprintf("Icebath Relay session %s closed with errors: %s", sessionID, failure))
	}
}

// Closes core scopes and releases the process plugin configuration once operations drain
func (r *RelayRuntime) Shutdown() {
	r.sessionsMu.Lock()
	if r.shutdownStarted {
		r.sessionsMu.Unlock()
		return
	}
	r.shutdownStarted, r.closing = true, true
	busy := r.activeOperations > 0
	r.sessionsMu.Unlock()
	if !busy {
		r.finishShutdown()
		return
	}
	go pprof.Do(context.Background(), pprof.Labels("thread", "icebath-nemo-relay-shutdown-"+r.RuntimeID[:8]), func(context.Context) {
		r.operationsIdle.Wait(-1)
		r.finishShutdown()
	})
}

func (r *RelayRuntime) finishShutdown() {
	ctx := context.Background()
	r.sessionsMu.Lock()
	ids := r.sessions.Keys()
	r.sessionsMu.Unlock()
	for _, id := range ids {
		r.closeSession(ctx, map[string]any{"session_id": id})
	}
	if r.pluginRegistered {
		if r.pluginsActive() {
			r.ReleaseManagedExecution(RelayPluginsExecutionConsumer)
		}
		relayPlugins.release(ctx, r)
		r.pluginRegistered = false
		relayAtExitUnregister(r.atExit)
	}
	r.sessionsMu.Lock()
	r.shutdownComplete.Set()
	r.sessionsMu.Unlock()
}

// Whether shutdown finished closing scopes and releasing plugins
func (r *RelayRuntime) ShutdownComplete() bool { return r.shutdownComplete.IsSet() }

// Process wide cache of resolved profile keys
var (
	relayProfileKeyMu    sync.Mutex
	relayProfileKeyCache = map[string]string{}
)

// Canonical profile identity used for runtime isolation
func CurrentRelayProfileKey(ctx context.Context) string {
	h := pypath.PathStr(pypath.Expanduser(home.Dir(ctx)))
	if !pypath.IsAbs(h) {
		resolved, _ := pypath.Realpath(h, pypath.Lax)
		return resolved
	}
	relayProfileKeyMu.Lock()
	defer relayProfileKeyMu.Unlock()
	if k := relayProfileKeyCache[h]; k != "" {
		return k
	}
	resolved, _ := pypath.Realpath(h, pypath.Lax)
	relayProfileKeyCache[h] = resolved
	return resolved
}

// Explicit plugins file override, empty to use Relay discovery
func relayConfiguredPluginInputs(ctx context.Context) (string, error) {
	configPath, err := relayExplicitPluginsTOML()
	if err != nil {
		return "", err
	}
	if configPath == "" {
		environ := map[string]any{}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				environ[k] = v
			}
		}
		if legacy := ConfiguredLegacyRelayEnvVars(environ); len(legacy) > 0 {
			relayLogger().WarnContext(ctx, fmt.Sprintf("Legacy NeMo Relay exporter variables are set but no %s was provided. %s no longer configure Relay exporters; any standard user or system plugins.toml still applies. Run `icebath migrate relay` (or `icebath update`, which runs it for every profile) to generate %s and select it in .env.",
				RelayPluginsConfigEnv, strings.Join(legacy, ", "), pypath.PathJoin(home.Dir(ctx), "relay-plugins.toml")))
		}
	}
	return configPath, nil
}

// File the plugins toml variable selects after checking that it parses
func relayExplicitPluginsTOML() (string, error) {
	configured := pystr.Strip(os.Getenv(RelayPluginsConfigEnv))
	if configured == "" {
		return "", nil
	}
	configPath := pypath.PathStr(pypath.Expanduser(configured))
	fail := func(err error) (string, error) {
		return "", &RelayPluginConfigurationLoadError{Message: "Icebath Relay plugin configuration could not be loaded from " + configPath + "; continuing without Relay plugins", Err: err}
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fail(err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return fail(err)
	}
	if _, ok := doc["dynamic_plugins"]; ok {
		return fail(errors.New("Icebath [[dynamic_plugins]] records are unsupported; use Relay [[plugins.dynamic]] records"))
	}
	return configPath, nil
}

// Plugin configuration Relay resolves for this process as its validator reports it
type RelayPluginSources struct {
	ConfigPaths []string
	Enabled     bool
	Errors      []string
	Warnings    []string
}

// Resolves the plugins toml layers this process would load without loading plugin code
func ResolveRelayPluginSources(ctx context.Context, relay *NemoRelay) (RelayPluginSources, error) {
	if relay == nil {
		loaded, err := loadNemoRelay()
		if err != nil {
			return RelayPluginSources{}, err
		}
		relay = loaded
	}
	explicit, err := relayExplicitPluginsTOML()
	if err != nil {
		return RelayPluginSources{}, err
	}
	if relay.Plugin == nil || relay.Plugin.Validate == nil {
		return RelayPluginSources{}, relayAttributeError("NemoRelayPlugin", "validate")
	}
	report, err := relay.Plugin.Validate(ctx, map[string]any{}, explicit)
	if err != nil {
		return RelayPluginSources{}, err
	}
	cfg, _ := report["config"].(map[string]any)
	rawDiagnostics, _ := cfg["diagnostics"].([]any)
	out := RelayPluginSources{Enabled: relayReportRequiresManagedExecution(ctx, report)}
	if paths, ok := report["config_paths"].([]any); ok {
		for _, p := range paths {
			out.ConfigPaths = append(out.ConfigPaths, pyrepr.Str(p))
		}
	}
	for _, d := range rawDiagnostics {
		diag, ok := d.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case pyrepr.Equal(diag["level"], "error"):
			out.Errors = append(out.Errors, pyrepr.Str(diag["message"]))
		case pyrepr.Equal(diag["level"], "warning") && !pyrepr.Equal(diag["code"], "plugin.configuration_inherited"):
			out.Warnings = append(out.Warnings, pyrepr.Str(diag["message"]))
		}
	}
	return out, nil
}

// Initialize's two conflict messages, the only signal of a foreign active host
var relayHostConflictMessages = map[string]bool{
	"conflict: a static plugin configuration is already active; to combine static and dynamic plugins, provide the static components as the base configuration to dynamic plugin activation before calling plugin initialization": true,
	"conflict: plugin configuration is owned by an active dynamic plugin host": true,
}

func relayIsHostConflict(err error) bool {
	re, ok := err.(*RelayError)
	return ok && re.Class == "RuntimeError" && relayHostConflictMessages[re.Message]
}

// Resolves the async plugin API result from synchronous host construction
func relayResolvePluginAwaitable(ctx context.Context, value any) (any, error) {
	if aw, ok := value.(RelayAwaitable); ok {
		return aw(ctx)
	}
	return value, nil
}

// Registers an exit handler beside the Relay runtimes and returns its unregister function
func RegisterRelayAtExit(fn func()) (unregister func()) {
	e := relayAtExitRegister(fn)
	return func() { relayAtExitUnregister(e) }
}

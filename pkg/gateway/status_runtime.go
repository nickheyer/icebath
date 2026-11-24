package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/monitoring"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Persists the latest status snapshot on one background goroutine, newer submissions replacing the pending one
type RuntimeStatusWriter struct {
	writeFn             func(path string, payload *jsonutil.Object) error
	mu                  sync.Mutex
	changed             chan struct{}
	pending             *statusSnapshot
	writingGeneration   int
	submittedGeneration int
	completedGeneration int
	successfulGen       int
	lastError           error
	failureLogged       bool
	started             bool
}

type statusSnapshot struct {
	generation int
	path       string
	payload    *jsonutil.Object
}

// Writer persisting through writeFn, the merged atomic file write when writeFn is nil
func NewRuntimeStatusWriter(writeFn func(path string, payload *jsonutil.Object) error) *RuntimeStatusWriter {
	return &RuntimeStatusWriter{writeFn: writeFn, changed: make(chan struct{})}
}

func (w *RuntimeStatusWriter) notifyLocked() {
	close(w.changed)
	w.changed = make(chan struct{})
}

// Queues a deep copy of payload for path and returns its generation
func (w *RuntimeStatusWriter) Submit(ctx context.Context, path string, payload *jsonutil.Object) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.submittedGeneration++
	generation := w.submittedGeneration
	w.pending = &statusSnapshot{generation, path, any(anyutil.Clone(payload)).(*jsonutil.Object)}
	if !w.started {
		w.started = true
		go w.run(ctx)
	}
	w.notifyLocked()
	return generation
}

// Blocks until generation or a later snapshot persisted, false on timeout or failure, the failure raised without a timeout
func (w *RuntimeStatusWriter) Wait(generation int, timeout *time.Duration) (bool, error) {
	var deadline time.Time
	if timeout != nil {
		deadline = time.Now().Add(max(*timeout, 0))
	}
	w.mu.Lock()
	for {
		state, settled := w.settledLocked(generation)
		if settled {
			err := w.lastError
			w.mu.Unlock()
			if !state && timeout == nil && err != nil {
				return false, err
			}
			return state, nil
		}
		ch := w.changed
		w.mu.Unlock()
		if timeout == nil {
			<-ch
		} else {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false, nil
			}
			select {
			case <-ch:
			case <-time.After(remaining):
			}
		}
		w.mu.Lock()
	}
}

// Waits boundedly for every snapshot submitted so far
func (w *RuntimeStatusWriter) Flush(timeout time.Duration) bool {
	w.mu.Lock()
	generation := w.submittedGeneration
	w.mu.Unlock()
	if generation == 0 {
		return true
	}
	ok, _ := w.Wait(generation, &timeout)
	return ok
}

func (w *RuntimeStatusWriter) settledLocked(generation int) (bool, bool) {
	if w.successfulGen >= generation {
		return true, true
	}
	if w.completedGeneration >= generation && w.writingGeneration == 0 && w.pending == nil {
		return false, true
	}
	return false, false
}

func (w *RuntimeStatusWriter) run(ctx context.Context) {
	for {
		w.mu.Lock()
		for w.pending == nil {
			ch := w.changed
			w.mu.Unlock()
			<-ch
			w.mu.Lock()
		}
		snap := w.pending
		w.pending = nil
		w.writingGeneration = snap.generation
		w.mu.Unlock()
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			merged := mergeOverOnDisk(snap.path, snap.payload)
			if w.writeFn != nil {
				err = w.writeFn(snap.path, merged)
			} else {
				err = writeJSONFile(ctx, snap.path, merged)
			}
		}()
		w.mu.Lock()
		w.writingGeneration = 0
		w.completedGeneration = max(w.completedGeneration, snap.generation)
		if err == nil {
			w.successfulGen = max(w.successfulGen, snap.generation)
			w.lastError = nil
		} else {
			w.lastError = err
		}
		w.notifyLocked()
		logged := w.failureLogged
		switch {
		case err == nil && logged:
			w.failureLogged = false
		case err != nil && !logged:
			w.failureLogged = true
		}
		w.mu.Unlock()
		switch {
		case err == nil && logged:
			pkgLog().Info("Gateway runtime-status persistence recovered")
		case err != nil && !logged:
			pkgLog().Warn(fmt.Sprintf("Failed to persist gateway runtime status; later updates will retry: %v", err))
		case err != nil:
			pkgLog().Debug(fmt.Sprintf("Failed to persist gateway runtime status: %v", err))
		}
	}
}

// Canonical snapshot laid over the record on disk so out of process fields survive
func mergeOverOnDisk(path string, payload *jsonutil.Object) *jsonutil.Object {
	existing := readJSONFile(path, false)
	if existing == nil {
		return payload
	}
	home.AdoptRecordKey(existing)
	merged := jsonutil.NewObject()
	for _, k := range existing.Keys() {
		merged.Set(k, recGet(existing, k))
	}
	for _, k := range payload.Keys() {
		merged.Set(k, recGet(payload, k))
	}
	return merged
}

var (
	runtimeStatusStateMu   sync.Mutex
	runtimeStatusStatePath string
	runtimeStatusState     *jsonutil.Object
	runtimeStatusWriter    *RuntimeStatusWriter
)

func runtimeStatusWriterLocked() *RuntimeStatusWriter {
	if runtimeStatusWriter == nil {
		runtimeStatusWriter = NewRuntimeStatusWriter(nil)
	}
	return runtimeStatusWriter
}

// Waits boundedly for every runtime status update submitted so far
func FlushRuntimeStatus(timeout time.Duration) bool {
	runtimeStatusStateMu.Lock()
	w := runtimeStatusWriter
	runtimeStatusStateMu.Unlock()
	return w == nil || w.Flush(timeout)
}

// Waits until every runtime status update submitted so far was written or failed
func SettleRuntimeStatus() {
	runtimeStatusStateMu.Lock()
	w := runtimeStatusWriter
	runtimeStatusStateMu.Unlock()
	if w == nil {
		return
	}
	w.mu.Lock()
	generation := w.submittedGeneration
	w.mu.Unlock()
	if generation > 0 {
		_, _ = w.Wait(generation, nil)
	}
}

// Longest a stopping adapter waits for its final runtime status to reach disk
const adapterStopStatusWait = 2 * time.Second

// Waits boundedly until the snapshot of the generation or a later one settled, at once for generation zero
func awaitRuntimeStatus(generation int, timeout time.Duration) {
	if generation <= 0 {
		return
	}
	runtimeStatusStateMu.Lock()
	w := runtimeStatusWriter
	runtimeStatusStateMu.Unlock()
	if w != nil {
		_, _ = w.Wait(generation, &timeout)
	}
}

// Flush that returns early with the context error when the caller stops waiting
func FlushRuntimeStatusAsync(ctx context.Context, timeout time.Duration) (bool, error) {
	runtimeStatusStateMu.Lock()
	w := runtimeStatusWriter
	runtimeStatusStateMu.Unlock()
	if w == nil {
		return true, nil
	}
	done := make(chan bool, 1)
	go func() { done <- w.Flush(max(timeout, 0)) }()
	select {
	case ok := <-done:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// One runtime status update, Fields holding every explicitly passed keyword with nil standing for None
type RuntimeStatusUpdate struct {
	Fields                map[string]any
	ClearProfilePlatforms bool
	DropProfilePlatforms  string
	KeepPlatforms         []string
}

var runtimeStatusFieldNames = map[string]bool{
	"gateway_state": true, "exit_reason": true, "restart_requested": true, "active_agents": true,
	"active_work": true, "platform": true, "platform_state": true, "error_code": true, "error_message": true,
	"needs_attention": true, "retrying_since": true, "served_profiles": true, "session_store": true,
	"parked_profiles": true, "multiplex_standalone_reason": true, "platform_metrics": true,
	"ingress_url": true, "listener_base": true,
}

func coerceSessionStore(v any) any {
	state := ""
	if m, ok := v.(*jsonutil.Object); ok {
		if s := recGet(m, "status"); anyutil.IsTrue(s) {
			state = anyutil.String(s)
		}
	} else if m, ok := v.(map[string]any); ok {
		if s := m["status"]; anyutil.IsTrue(s) {
			state = anyutil.String(s)
		}
	}
	if state != "ok" && state != "unavailable" && state != "retrying" {
		state = "unknown"
	}
	out := jsonutil.NewObject()
	out.Set("status", state)
	return out
}

// Python list of an iterable keyword value
func pyList(v any) ([]any, error) {
	switch t := v.(type) {
	case nil:
		return []any{}, nil
	case []any:
		return append([]any{}, t...), nil
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, nil
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a list, got %s", errchain.Kind(v))
}

// Coerces one explicitly passed field the way the writer does
func coerceStatusField(key string, v any) (any, error) {
	switch key {
	case "restart_requested", "needs_attention":
		return anyutil.IsTrue(v), nil
	case "active_agents":
		return ParseActiveAgents(v)
	case "active_work":
		if !anyutil.IsTrue(v) {
			return nil, nil
		}
		return pyList(v)
	case "served_profiles":
		if !anyutil.IsTrue(v) {
			v = nil
		}
		return pyList(v)
	case "parked_profiles":
		out := jsonutil.NewObject()
		switch t := v.(type) {
		case nil:
		case map[string]string:
			for k, r := range t {
				out.Set(k, r)
			}
		case map[string]any:
			for k, r := range t {
				out.Set(k, anyutil.String(r))
			}
		case *jsonutil.Object:
			for _, k := range t.Keys() {
				out.Set(k, anyutil.String(recGet(t, k)))
			}
		default:
			if anyutil.IsTrue(v) {
				return nil, fmt.Errorf("parked_profiles must map each profile to its reason, got %s", errchain.Kind(v))
			}
		}
		return out, nil
	case "multiplex_standalone_reason":
		if !anyutil.IsTrue(v) {
			return nil, nil
		}
		return anyutil.String(v), nil
	case "session_store":
		return coerceSessionStore(v), nil
	}
	return v, nil
}

// One status field an update may pass, its snapshot key, its keyword and the coercion of its value
type statusSetField struct {
	key     string
	keyword string
	coerce  func(any) (any, error)
}

// Assigns each field whose keyword was explicitly passed, coerced when the field names a coercion
func applySetFields(target *jsonutil.Object, passed map[string]any, fields []statusSetField) error {
	for _, field := range fields {
		value, set := passed[field.keyword]
		if !set {
			continue
		}
		if field.coerce != nil {
			coerced, err := field.coerce(value)
			if err != nil {
				return err
			}
			value = coerced
		}
		target.Set(field.key, value)
	}
	return nil
}

// Merges one update into the process wide canonical snapshot, the state lock held by the caller
func prepareRuntimeStatusUpdateLocked(u RuntimeStatusUpdate, loadExisting, reloadExisting bool) (string, *jsonutil.Object, *jsonutil.Object, error) {
	for key := range u.Fields {
		if !runtimeStatusFieldNames[key] {
			return "", nil, nil, fmt.Errorf("unknown runtime status field %s", key)
		}
	}
	path := runtimeStatusPath()
	if reloadExisting || runtimeStatusStatePath != path || runtimeStatusState == nil {
		runtimeStatusStatePath = path
		var loaded *jsonutil.Object
		if loadExisting {
			loaded = readJSONFile(path, false)
		}
		if loaded == nil || loaded.Len() == 0 {
			loaded = buildRuntimeStatusRecord()
		}
		home.AdoptRecordKey(loaded)
		runtimeStatusState = loaded
	}
	previous := runtimeStatusState
	payload := any(anyutil.Clone(previous)).(*jsonutil.Object)
	current := statusBuildPidRecord()
	if _, ok := payload.Get("platforms"); !ok {
		payload.Set("platforms", jsonutil.NewObject())
	}
	platforms, ok := recGet(payload, "platforms").(*jsonutil.Object)
	if !ok {
		platforms = jsonutil.NewObject()
		payload.Set("platforms", platforms)
	}
	if u.ClearProfilePlatforms || u.DropProfilePlatforms != "" {
		dropPrefix := ""
		if u.DropProfilePlatforms != "" {
			dropPrefix = u.DropProfilePlatforms + ":"
		}
		keep := map[string]bool{}
		for _, k := range u.KeepPlatforms {
			keep[k] = true
		}
		kept := jsonutil.NewObject()
		for _, k := range platforms.Keys() {
			if !strings.Contains(k, ":") || (dropPrefix != "" && !strings.HasPrefix(k, dropPrefix)) || keep[k] {
				kept.Set(k, recGet(platforms, k))
			}
		}
		platforms = kept
		payload.Set("platforms", platforms)
	}
	for _, key := range []string{"kind", "pid", "argv", "start_time"} {
		payload.Set(key, recGet(current, key))
	}
	payload.Set("updated_at", utcNowISO())
	for _, kv := range codeIdentityFields() {
		payload.Set(kv[0].(string), kv[1])
	}
	coerced := func(key string) func(any) (any, error) {
		return func(v any) (any, error) { return coerceStatusField(key, v) }
	}
	if err := applySetFields(payload, u.Fields, []statusSetField{
		{"gateway_state", "gateway_state", nil}, {"exit_reason", "exit_reason", nil},
		{"restart_requested", "restart_requested", coerced("restart_requested")},
		{"active_agents", "active_agents", coerced("active_agents")},
		{"active_work", "active_work", coerced("active_work")},
		{"served_profiles", "served_profiles", coerced("served_profiles")},
		{"parked_profiles", "parked_profiles", coerced("parked_profiles")},
		{"multiplex_standalone_reason", "multiplex_standalone_reason", coerced("multiplex_standalone_reason")},
		{"session_store", "session_store", coerced("session_store")},
	}); err != nil {
		return "", nil, nil, err
	}
	if platformValue, set := u.Fields["platform"]; set {
		platform := jsonKey(platformValue)
		entry, ok := any(anyutil.Clone(recGet(platforms, platform))).(*jsonutil.Object)
		if !ok {
			entry = jsonutil.NewObject()
		}
		fields := map[string]any{}
		for k, v := range u.Fields {
			fields[k] = v
		}
		if state, set := fields["platform_state"]; set && pyEq(state, "connected") {
			if _, set := fields["needs_attention"]; !set {
				fields["needs_attention"] = false
			}
			if _, set := fields["retrying_since"]; !set {
				fields["retrying_since"] = nil
			}
		}
		if err := applySetFields(entry, fields, []statusSetField{
			{"state", "platform_state", nil}, {"error_code", "error_code", nil}, {"error_message", "error_message", nil},
			{"needs_attention", "needs_attention", coerced("needs_attention")}, {"retrying_since", "retrying_since", nil},
			{"metrics", "platform_metrics", nil}, {"ingress_url", "ingress_url", nil}, {"listener_base", "listener_base", nil},
		}); err != nil {
			return "", nil, nil, err
		}
		entry.Set("updated_at", utcNowISO())
		entry.Set("writer_pid", recGet(current, "pid"))
		entry.Set("writer_start_time", recGet(current, "start_time"))
		platforms.Set(platform, entry)
	}
	runtimeStatusState = payload
	return path, payload, previous, nil
}

// Durably persists one status update for CLI callers, waiting at most waitTimeout when it is set
func WriteRuntimeStatus(ctx context.Context, u RuntimeStatusUpdate, reloadExisting bool, waitTimeout *time.Duration) (bool, error) {
	runtimeStatusStateMu.Lock()
	path, payload, previous, err := prepareRuntimeStatusUpdateLocked(u, true, reloadExisting)
	if err != nil {
		runtimeStatusStateMu.Unlock()
		return false, err
	}
	writer := runtimeStatusWriterLocked()
	generation := writer.Submit(ctx, path, payload)
	runtimeStatusStateMu.Unlock()
	emitRuntimeStatusTransition(previous, payload)
	return writer.Wait(generation, waitTimeout)
}

// Merges and queues a status update without waiting for persistence
func PublishRuntimeStatus(ctx context.Context, u RuntimeStatusUpdate) (int, error) {
	runtimeStatusStateMu.Lock()
	path, payload, previous, err := prepareRuntimeStatusUpdateLocked(u, false, false)
	if err != nil {
		runtimeStatusStateMu.Unlock()
		return 0, err
	}
	generation := runtimeStatusWriterLocked().Submit(ctx, path, payload)
	runtimeStatusStateMu.Unlock()
	emitRuntimeStatusTransition(previous, payload)
	return generation, nil
}

// Reports a status transition to monitoring, a failure of the observer never reaching the writer
func emitRuntimeStatusTransition(previous, payload *jsonutil.Object) {
	defer func() { _ = recover() }()
	monitoring.EmitRuntimeStatusTransition(previous, payload)
}

// Exposes the active agents contract to gateway health monitoring
func init() { monitoring.GatewayParseActiveAgents = ParseActiveAgents }

// Reads gateway_state.json of this process home, or of another home when path is set
func ReadRuntimeStatus(path string) *jsonutil.Object {
	if path == "" {
		path = runtimeStatusPath()
	}
	return readJSONFile(path, false)
}

// Seconds a snapshot may age before its liveness claim is suspect
const RuntimeStatusStaleTTL = 120

// Whether the snapshot updated_at is older than ttl or missing
func RuntimeStatusIsStale(record *jsonutil.Object, ttl int) bool {
	if record == nil {
		return true
	}
	stamp := recGet(record, "updated_at")
	if !anyutil.IsTrue(stamp) {
		stamp = ""
	}
	return markerIsStale(stamp, ttl)
}

// Whole seconds since the snapshot updated_at, false when missing or unparseable
func RuntimeStatusHeartbeatAgeS(record *jsonutil.Object) (int, bool, error) {
	if record == nil {
		return 0, false, nil
	}
	updatedAt, ok, err := NormalizeUpdatedAt(recGet(record, "updated_at"))
	if err != nil || !ok || updatedAt == "" {
		return 0, false, err
	}
	stamp, _, err := timeutil.ParseISOZone(updatedAt, time.UTC)
	if err != nil {
		return 0, false, err
	}
	return max(0, int(math.Trunc(time.Since(stamp).Seconds()))), true, nil
}

// Whether the snapshot pid is alive and passes the start time reuse guard
func RuntimeStatusPidIsLive(record *jsonutil.Object) bool {
	_, ok := liveFromRecord(record)
	return ok
}

// Non negative active_agents of a persisted value, zero when int raises TypeError or ValueError
func ParseActiveAgents(raw any) (any, error) {
	// A whole number past what an int64 holds reads as the bound on its side, an infinite one is refused
	n, err := anyutil.ToInt(raw)
	if f, isFloat := raw.(float64); isFloat && math.IsInf(f, 0) {
		return nil, err
	}
	if err != nil && !errors.Is(err, anyutil.ErrRange) {
		return int64(0), nil
	}
	if n < 0 {
		return int64(0), nil
	}
	return int(n), nil
}

// Whether a live serving gateway has active agents
func DeriveGatewayBusy(gatewayRunning bool, gatewayState, activeAgents any) (bool, error) {
	if !DeriveGatewayDrainable(gatewayRunning, gatewayState) {
		return false, nil
	}
	n, err := ParseActiveAgents(activeAgents)
	if err != nil {
		return false, err
	}
	return pyIsInt(n) && !pyEq(n, int64(0)), nil
}

// Whether a gateway is live and in a serving state
func DeriveGatewayDrainable(gatewayRunning bool, gatewayState any) bool {
	return gatewayRunning && (pyEq(gatewayState, "running") || pyEq(gatewayState, "degraded"))
}

// Object key json.dumps writes for a Python dict key
func jsonKey(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return t
	}
	return anyutil.Quote(v)
}

package dashboard

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
)

// Log tail reader and debug share of the CLI layers above this package, assigned by the command wiring
var StatusCLI struct {
	ReadLogTail            func(path string, numLines int, hasFilters bool, minLevel *string, componentPrefixes []string, hasComponent bool) ([]string, error)
	BuildDebugShare        func(ctx context.Context, logLines int, redact bool) (*pyjson.Object, error)
	RedactDebugSupportText func(value any) string
}

const (
	statusActiveSessionsTimeout = 750 * time.Millisecond
	gatewayHealthRouteTimeout   = 1 * time.Second
)

var healthyPlatformStates = map[string]bool{"connected": true, "running": true, "ok": true}

// Probes and readers of the status route, the module attributes hermes tests patch
var (
	statusProbeGatewayHealth = probeGatewayHealth
	statusPidProbe           = func(pidPath string) (int, bool, error) {
		return gateway.GetRunningPidCached(pidPath, true, time.Second)
	}
	statusReadRuntimeStatus  = gateway.ReadRuntimeStatus
	statusRuntimePidProbe    = gateway.GetRuntimeStatusRunningPid
	statusServedRuntime      = gateway.ServedProfileRuntime
	statusTopologyCached     = collectProfileGatewayTopologyCached
	statusConfiguredPlatform = loadConfiguredGatewayPlatforms
	statusOpenSessionDB      = openSessionDBForProfile
	statusVersionInfo        = updaterecord.GetVersionInfo
	statusCheckConfigVersion = config.CheckConfigVersion
	statusCollectMemory      = func(ctx context.Context, h string) (*pyjson.Object, error) {
		return gateway.CollectMemoryStatus(ctx, h, nil), nil
	}
	statusCollectDisk = func(ctx context.Context, h string) (*pyjson.Object, error) {
		return gateway.CollectDiskStatus(ctx, h), nil
	}
)

// Object holding the named keys of a result map in the given order, absent keys left out
func orderedObject(m map[string]any, keys ...string) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out.Set(k, v)
		}
	}
	return out
}

// Float of a stored number, false for anything arithmetic refuses
func statusNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	}
	return 0, false
}

// Recently active open sessions of the process store read through a read only handle, zero for a missing store
func countStatusActiveSessions(ctx context.Context) (int, error) {
	if _, err := os.Stat(state.ResolveDefaultDBPath(ctx)); err != nil {
		return 0, nil
	}
	db, err := statusOpenSessionDB(ctx, nil, true)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	limit := 50
	sessions, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &limit, CompactRows: true})
	if err != nil {
		return 0, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	count := 0
	for _, s := range sessions {
		if s.GetOr("ended_at", nil) != nil {
			continue
		}
		last, ok := statusNumber(s.GetOr("last_active", s.GetOr("started_at", int64(0))))
		if !ok {
			return 0, fmt.Errorf("TypeError: unsupported operand type(s) for -: 'float' and '%s'", pyrepr.TypeName(s.GetOr("last_active", nil)))
		}
		if now-last < 300 {
			count++
		}
	}
	return count, nil
}

// Active session count bounded by the status budget, zero when the count is late or fails
func statusActiveSessions(ctx context.Context) int {
	type outcome struct {
		n   int
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		n, err := countStatusActiveSessions(ctx)
		done <- outcome{n, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			routersLog().Debug(fmt.Sprintf("/api/status active session count unavailable: %s", got.err))
			return 0
		}
		return got.n
	case <-time.After(statusActiveSessionsTimeout):
		routersLog().Debug(fmt.Sprintf("/api/status active session count exceeded %.2fs; returning 0", statusActiveSessionsTimeout.Seconds()))
		return 0
	}
}

// Nonce and runtime state of the SSH launch that owns this backend, 404 when none does
func getSSHOwnership(req *Request) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	webStateMu.RLock()
	nonce := sshOwnerNonce
	webStateMu.RUnlock()
	if nonce == "" {
		return nil, NewHTTPException(404, "SSH ownership is not active")
	}
	return pyjson.ObjectOf("ok", true, "sshOwnerNonce", nonce, "protocolVersion", int64(1), "runtimeIntact", sshRuntimeIntact()), nil
}

// Liveness answer with the version and commit this process booted from
func getHealth(req *Request) (*pyjson.Object, error) {
	info, err := statusVersionInfo()
	if err != nil {
		return nil, err
	}
	var commit any
	if info.Commit != nil {
		commit = *info.Commit
	}
	return pyjson.ObjectOf("ok", true, "version", info.BaseVersion, "displayVersion", info.DisplayVersion(), "commit", commit,
		"auth_required", req.App().State.Bool("auth_required")), nil
}

// Pid and host role of the process that owns this port, for an attaching serve or dashboard to compare
func getHostIdentity(req *Request) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	role := req.App().State.String("host_role")
	if role == "" {
		role = "serve"
	}
	return pyjson.ObjectOf("ok", true, "protocolVersion", int64(1), "pid", int64(os.Getpid()), "role", role,
		"servesSpa", req.App().State.Bool("serves_spa")), nil
}

// Token gated idle snapshot of the live ledgers, a diagnostic and never a retirement permit
func getHealthIdle(req *Request) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	out := pyjson.ObjectOf("ok", true)
	out.Update(orderedObject(IdleProof(nil, nil), "idle", "reason", "detail"))
	return out, nil
}

// Runs one step of the retirement fence, prepare granting a permit and commit or cancel settling its token
func postHealthRetirement(ctx context.Context, req *Request) (*pyjson.Object, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	raw, err := req.Body(ctx)
	if err != nil {
		return nil, err
	}
	parsed, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, NewHTTPException(400, "JSON body required")
	}
	body, ok := parsed.(*pyjson.Object)
	action, _ := body.GetOr("action", nil).(string)
	if !ok || !slices.Contains([]string{"prepare", "commit", "cancel"}, action) {
		return nil, NewHTTPException(400, "action must be prepare, commit, or cancel")
	}
	if action == "prepare" {
		return orderedObject(provider.Retirement.Prepare(), "ok", "idle", "token"), nil
	}
	token, _ := body.GetOr("token", nil).(string)
	if token == "" {
		return nil, NewHTTPException(400, "token required")
	}
	if action == "commit" {
		return orderedObject(provider.Retirement.Commit(token), "ok"), nil
	}
	return orderedObject(provider.Retirement.Cancel(token), "ok"), nil
}

var profilePlatformStatusKeyRE = regexp.MustCompile(`\A[a-z0-9][a-z0-9_-]{0,63}:[a-z0-9][a-z0-9_-]{0,63}\z`)

// Whether a key has the profile and platform grammar the runner publishes
func isProfilePlatformStatusKey(key string) bool { return profilePlatformStatusKeyRE.MatchString(key) }

// Whether a runtime platform key may be shown, namespaced keys by grammar and plain keys by the configured set
func statusPlatformKeyAllowed(key string, configured map[string]bool) bool {
	if strings.Contains(key, ":") {
		return isProfilePlatformStatusKey(key)
	}
	return configured == nil || configured[key]
}

// Platform entry without the writer identity stamps, any value that is no mapping unchanged
func publicPlatformEntry(value any) any {
	entry, ok := value.(*pyjson.Object)
	if !ok {
		return value
	}
	out := pyjson.NewObject()
	for _, k := range entry.Keys {
		if k != "writer_pid" && k != "writer_start_time" {
			out.Set(k, entry.Vals[k])
		}
	}
	return out
}

// Gateway platforms with the platform states of every other profile folded in under namespaced keys
func mergeProfileGatewayPlatforms(ctx context.Context, gatewayPlatforms *pyjson.Object, profilePlatforms any) *pyjson.Object {
	active := profiles.GetActiveProfileName(ctx)
	merged := gatewayPlatforms.Copy()
	byProfile, _ := profilePlatforms.(*pyjson.Object)
	if byProfile == nil {
		return merged
	}
	for _, prof := range byProfile.Keys {
		plats, ok := byProfile.Vals[prof].(*pyjson.Object)
		if prof == active || !ok {
			continue
		}
		for _, key := range plats.Keys {
			value, isDict := plats.Vals[key].(*pyjson.Object)
			if strings.Contains(key, ":") || !isDict {
				continue
			}
			namespaced := prof + ":" + key
			if !isProfilePlatformStatusKey(namespaced) {
				continue
			}
			if _, present := merged.Get(namespaced); !present {
				merged.Set(namespaced, publicPlatformEntry(value))
			}
		}
	}
	return merged
}

// Gateway health probe held to the blocking budget of the status route, not alive when it is late
func boundedHealthProbe(ctx context.Context) func() (bool, *pyjson.Object, error) {
	return func() (bool, *pyjson.Object, error) {
		type outcome struct {
			alive bool
			body  any
		}
		done := make(chan outcome, 1)
		go func() {
			alive, body := statusProbeGatewayHealth(ctx)
			done <- outcome{alive, body}
		}()
		select {
		case got := <-done:
			body, _ := got.body.(*pyjson.Object)
			return got.alive, body, nil
		case <-time.After(gatewayHealthRouteTimeout):
			routersLog().Warn(fmt.Sprintf("/api/status gateway health probe exceeded %.2fs; using local status", gatewayHealthRouteTimeout.Seconds()))
			return false, nil, nil
		}
	}
}

// Public platform map of a runtime, emptied for a stopped gateway and reduced to fatal entries after a failed start
func projectGatewayPlatforms(gatewayPlatforms *pyjson.Object, configured map[string]bool, gatewayRunning bool, gatewayState any) *pyjson.Object {
	platforms := pyjson.NewObject()
	for _, key := range gatewayPlatforms.Keys {
		if statusPlatformKeyAllowed(key, configured) {
			platforms.Set(key, publicPlatformEntry(gatewayPlatforms.Vals[key]))
		}
	}
	if gatewayRunning {
		return platforms
	}
	out := pyjson.NewObject()
	if gatewayState == "startup_failed" {
		for _, key := range platforms.Keys {
			if entry, ok := platforms.Vals[key].(*pyjson.Object); ok && entry.GetOr("state", nil) == "fatal" {
				out.Set(key, entry)
			}
		}
	}
	return out
}

// Gateway liveness and runtime state one status request reports
type gatewayStatus struct {
	runtime         *pyjson.Object
	running         bool
	pid             any
	state           any
	platforms       *pyjson.Object
	exitReason      any
	updatedAt       any
	heartbeatStaleS any
	sharedWith      any
}

// Gateway liveness with the state, platforms and stamps of its runtime record, scoped to a profile dir when given
func resolveGatewayStatus(ctx context.Context, profileDir string, healthURL string) (*gatewayStatus, error) {
	statusPath := ""
	if profileDir != "" {
		statusPath = pypath.PathJoin(profileDir, "gateway_state.json")
	}
	localRuntime := statusReadRuntimeStatus(statusPath)
	opts := gateway.LivenessOptions{ProfileDir: profileDir, Runtime: localRuntime, RuntimeGiven: true, PidProbe: statusPidProbe,
		RuntimeReader:   func(p string) (*pyjson.Object, error) { return statusReadRuntimeStatus(p), nil },
		RuntimePidProbe: statusRuntimePidProbe}
	if healthURL != "" {
		opts.HealthProbe = boundedHealthProbe(ctx)
	}
	liveness := gateway.ResolveGatewayLiveness(opts)
	remoteHealthBody := liveness.HealthBody
	configured, err := statusConfiguredPlatform(ctx)
	if err != nil {
		configured = nil
	}
	runtime := localRuntime
	if runtime == nil && remoteHealthBody != nil && pyrepr.Truthy(remoteHealthBody.GetOr("gateway_state", nil)) {
		runtime = remoteHealthBody
	}
	ownHome := profileDir
	if ownHome == "" {
		ownHome = home.Process()
	}
	if liveness.Runtime != nil {
		served := liveness.Runtime.Copy()
		served.Set("platforms", gateway.ProfilePlatformsFromMultiplexer(liveness.Runtime, home.ProfileNameFor(ownHome)))
		runtime = served
	} else if pid, ok := gateway.LocalLivenessPID(liveness); ok {
		record, err := statusServedRuntime(ownHome, localRuntime, pid)
		if err != nil {
			routersLog().Warn("/api/status multiplexer record lookup failed", "exc_info", err)
		} else if record != nil && record.Len() > 0 {
			runtime = record
		}
	}
	out := &gatewayStatus{runtime: runtime, running: liveness.Running, pid: liveness.PID, platforms: pyjson.NewObject()}
	if runtime != nil && runtime.Len() > 0 {
		out.state = runtime.GetOr("gateway_state", nil)
		switch {
		case !liveness.Running:
			out.state = gateway.RetainedGatewayState(runtime)
		case remoteHealthBody != nil && (out.state == nil || out.state == "stopped"):
			out.state = "running"
		case (out.state == "running" || out.state == "degraded" || out.state == "starting") &&
			gateway.RuntimeStatusIsStale(runtime, gateway.RuntimeStatusStaleTTL):
			age, ok, err := gateway.RuntimeStatusHeartbeatAgeS(runtime)
			if err != nil {
				return nil, err
			}
			if ok {
				out.heartbeatStaleS = int64(age)
			}
		}
		platforms := pyjson.NewObject()
		if raw := runtime.GetOr("platforms", nil); pyrepr.Truthy(raw) {
			obj, ok := raw.(*pyjson.Object)
			if !ok {
				return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'items'", pyrepr.TypeName(raw))
			}
			platforms = obj
		}
		out.platforms = projectGatewayPlatforms(platforms, configured, liveness.Running, out.state)
		if out.state != "stopped" {
			out.exitReason = runtime.GetOr("exit_reason", nil)
		}
		updatedAt, ok, err := gateway.NormalizeUpdatedAt(runtime.GetOr("updated_at", nil))
		if err != nil {
			return nil, err
		}
		if ok {
			out.updatedAt = updatedAt
		}
	}
	if liveness.Running && out.state == nil && remoteHealthBody != nil {
		out.state = "running"
	}
	if liveness.Running && runtime != nil {
		if shared, ok := runtime.GetOr("served_profiles", nil).([]any); ok {
			names := make([]any, 0, len(shared))
			for _, p := range shared {
				names = append(names, pyrepr.Str(p))
			}
			out.sharedWith = names
		}
	}
	return out, nil
}

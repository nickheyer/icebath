package backend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/desktoplog"
)

const (
	descriptorLogLines           = 80
	gatewayRestartClientTimeout  = 120 * time.Second
	bootProgressStartingMessage  = "Waiting to start Icebath backend"
	nonPrimaryInstanceBootFailed = "Icebath Desktop is already running in another window."
	localTeardownTimeout         = 7 * time.Second
)

// Host is the shell surface the backend handlers drive, windows and events
type Host interface {
	// Live window state of a window, false when it is gone
	WindowState(windowID string) (map[string]any, bool)
	// Window that receives boot progress, backend exit and connection applied
	MainWindowID() string
	// Sends one event to one window
	Send(windowID, event string, payload any)
	// Whether this process holds the single instance lock
	IsPrimaryInstance() bool
	// Drops the preview reach cache of a window whose route changed
	ResetPreviewReach(windowID string)
}

// ResolvedBackend is the icebath command the desktop runs, or a bootstrap the runtime must install first
type ResolvedBackend struct {
	BackendCommand
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Bootstrap bool   `json:"bootstrap"`
}

// Runtime is the update gate and bootstrap surface the updater group serves
type Runtime interface {
	// Installs the runtime a bootstrap-needed backend names, passing a usable one through
	EnsureRuntime(b ResolvedBackend, assertStillOwned func() error) (ResolvedBackend, error)
	// Blocks while an update holds the local backend
	WaitForUpdate() error
	// Blocks while an update holds a pool profile, cancelled stopping the wait
	WaitForPoolUpdate(profile string, cancelled func() bool) error
	// Update hold the boot screen shows, nil when none
	UpdateHold() any
	// Latched bootstrap failure that refuses every start until a reset or repair, nil when none
	BootstrapFailure() error
	// Latches a bootstrap failure, the fake boot error included
	SetBootstrapFailure(err error)
	// Zeroes the bootstrap repair attempt count once a local gateway is ready
	ResetBootstrapRepair()
}

// BootstrapFailureError is a bootstrap failure the renderer shows on the boot screen
type BootstrapFailureError struct{ Message string }

func (e *BootstrapFailureError) Error() string { return e.Message }

// IsBootstrapFailure marks the error for the bootstrap failure screen
func (e *BootstrapFailureError) IsBootstrapFailure() bool { return true }

// RouteRequest is the REST call a profile route is resolved for
type RouteRequest struct {
	Method string
	Path   string
}

// RemoteResolveOptions say which saved remote a profile resolves to
type RemoteResolveOptions struct {
	Primary              bool
	ForceRegistryPrimary bool
	PoolKey              string
}

// PoolBackend is one pooled backend a revalidation sweep probes
type PoolBackend struct {
	Key           string
	RemoteBaseURL string
	Connection    *Future[Connection]
}

// EnsureOptions shape a backend request, passive reads never spawning one
type EnsureOptions struct {
	InheritsPrimary bool
	Passive         bool
	Request         *RouteRequest
}

// PrimaryRemoteBoot is what a remote primary boot needs to restore a remembered profile the remote does not serve
type PrimaryRemoteBoot struct {
	RequestedProfile     string
	InheritsPrimary      bool
	OnProfileRestored    func(profile string)
	Context              context.Context
	AssertCurrentAttempt func() error
}

// RevalidateResult is the reply of a primary revalidation
type RevalidateResult struct {
	OK      bool `json:"ok"`
	Rebuilt bool `json:"rebuilt"`
}

// Routes is the connections registry surface the backend handlers read and dial through
type Routes interface {
	// Remembered profile of the primary, empty for none
	ActiveProfile() string
	// Connection and profile a renderer request resolves to and whether it follows the primary
	ResolveRequest(requested *string, source *WindowConnectionRoute, primaryKey string) (connectionID, profile string, inheritsPrimary bool, err error)
	// Registry id a primary or pooled descriptor belongs to, empty when unregistered
	ResolvedConnectionID(conn Connection) (string, error)
	// Backend of a registry scoped connection and profile
	EnsureRegistryBackend(connectionID, profile string) (Connection, error)
	// Which backend serves a profile key for a request, nil for a descriptor request
	ProfileRoute(key string, request *RouteRequest) ProfileBackendRoute
	// Refuses a profile whose deletion is in flight
	AssertCanStart(profile string) error
	// Saved remote of a profile, nil when it runs locally, a forced registry primary rereading the registry
	ResolveRemote(profile string, opts RemoteResolveOptions) (Connection, error)
	// Dials a saved remote for profile, boot set only for the primary and its profile restore
	ConnectRemote(remote Connection, profile string, boot *PrimaryRemoteBoot) (Connection, error)
	// Waits for the first run choice a bootstrap-needed backend gates on
	WaitForFirstRunDecision(b ResolvedBackend) (string, error)
	// Probes a remote primary and drops it when dead, reset clearing the cached descriptor
	RevalidatePrimary(connection *Future[Connection], reset func()) (RevalidateResult, error)
	// Probes pooled remote descriptors and retires the dead ones through stop
	RevalidatePool(entries []PoolBackend, stop func(key string) error) error
	// Socket URL of a remote descriptor, minting its ticket where it needs one
	RemoteWsURL(conn Connection) (string, error)
	// Whether err is a gateway authentication rejection the renderer answers with a login
	IsAuthRejection(err error) bool
	// Tears down the SSH state of a scope, the primary for an empty scope
	TeardownSSH(scope string) error
	// Records the profile whose home the WSL path bridge maps for native paths
	SetActiveGatewayProfile(profile string)
	// Records whether a profile is served by this machine for the WSL path bridge
	SetBridgeProfileState(profile string, local bool)
	// Moves a legacy active_profile file into the desktop profile config once
	MigrateActiveProfileIfMissing() error
	// Whether the primary boots remote, a managed restore in flight or a remote override or global remote
	PrimaryIsRemote() bool
	// Forgets the remote liveness failures counted against the primary
	ClearRemoteLiveness()
}

// Merges the login shell PATH before a primary spawn, the shared process wide merge in production
var ensureLoginShellPath = EnsureLoginShellPath

type poolEntry struct {
	future        *Future[Connection]
	remoteBaseURL string
}

// ServiceOptions name where the backend lives, how it is resolved and where the desktop logs
type ServiceOptions struct {
	IcebathHome string
	Cwd         func() string
	Env         func() map[string]string
	Resolve     func(args []string) (ResolvedBackend, error)
	FakeMode    bool
	DesktopLog  *desktoplog.Log
	Ownership   *Ownership
	PackageRoot string
	// Window route table and primary pin the shell also reads for the per window boot payload
	WindowRoutes *WindowConnectionRoutes
	PrimaryPin   *PrimaryProfilePin
}

// Service holds the primary connection, the profile pool and boot progress behind the backend preload functions
type Service struct {
	host    Host
	routes  Routes
	runtime Runtime
	opts    ServiceOptions

	state        ConnectionState[int, Connection]
	claims       DialClaims
	pin          *PrimaryProfilePin
	windowRoutes *WindowConnectionRoutes
	dials        *LocalGatewayDials
	restartStale StaleGatewayRestart
	revalidation DialClaims

	quit     context.Context
	abort    context.CancelCauseFunc
	starts   sync.WaitGroup
	inflight atomic.Int64
	shutdown *ShutdownCoordinator

	reap          sync.Once
	bootFakeError string

	mu            sync.Mutex
	pool          map[string]*poolEntry
	boot          map[string]any
	startFailure  error
	reauthFailure error
	heldFor       error
	softRehome    bool
	routeOwners   map[string]bool
}

// Backend service over the shell, the connections registry and the update runtime
func NewService(host Host, routes Routes, runtime Runtime, opts ServiceOptions) *Service {
	if opts.WindowRoutes == nil || opts.PrimaryPin == nil {
		panic("backend: ServiceOptions needs the WindowRoutes and PrimaryPin the shell shares")
	}
	s := &Service{host: host, routes: routes, runtime: runtime, opts: opts, bootFakeError: os.Getenv("ICEBATH_DESKTOP_BOOT_FAKE_ERROR"),
		pin: opts.PrimaryPin, windowRoutes: opts.WindowRoutes, dials: NewLocalGatewayDials(), pool: map[string]*poolEntry{}, routeOwners: map[string]bool{}}
	s.quit, s.abort = context.WithCancelCause(context.Background())
	s.shutdown = NewShutdownCoordinator(s.shutdownBackends)
	s.boot = map[string]any{"error": nil, "fakeMode": opts.FakeMode, "isCloudBackendDown": false, "message": bootProgressStartingMessage,
		"phase": "idle", "progress": 0, "retryable": false, "running": false, "statusCode": nil, "timestamp": time.Now().UnixMilli(), "updateHold": nil}
	s.restartStale = CreateStaleGatewayRestarter(func(owner string) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.restartLocalGatewayOwner(owner)
		}()
		return done
	}, s.Log)
	return s
}

// Quits the backends this app owns within the teardown budget, reaps package stragglers and verifies the Windows close
func (s *Service) shutdownBackends() error {
	primary, hasPrimary := s.state.Process()
	s.abort(errors.New("Icebath Desktop is quitting."))
	finished := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			s.starts.Wait()
		}()
		go func() {
			defer wg.Done()
			s.TeardownPrimary(true)
		}()
		go func() {
			defer wg.Done()
			s.StopAllPoolBackends()
		}()
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(localTeardownTimeout):
	}
	var exclude []int
	if hasPrimary && primary > 0 {
		exclude = []int{primary}
	}
	s.reapInstallRootedStragglers(exclude)
	var owned []int
	if runtime.GOOS == "windows" && hasPrimary && primary > 0 && IsPidAlive(primary) {
		owned = []int{primary}
	}
	if err := s.windowsCloseStopOwnedBackends(owned); err != nil {
		s.Log("[close-stop] " + err.Error())
		return err
	}
	return nil
}

// Remembers a line in desktop.log and the ring descriptors carry
func (s *Service) Log(line string) {
	s.opts.DesktopLog.Remember(line)
}

// RecentLogs is the tail of the backend log a connection descriptor carries
func (s *Service) RecentLogs() []string { return s.recentLogs() }

// MainWindowState is the main window state a connection descriptor carries
func (s *Service) MainWindowState() map[string]any { return s.mainWindowState() }

// ReleaseReauthLatch forgets the latched remote reauth failure once a sign-in is confirmed
func (s *Service) ReleaseReauthLatch() {
	s.mu.Lock()
	s.reauthFailure = nil
	s.mu.Unlock()
}

func (s *Service) recentLogs() []string {
	return s.opts.DesktopLog.Recent(descriptorLogLines)
}

// Reaps backends a crashed earlier desktop run left behind, once per process
func (s *Service) reapOrphansOnce() {
	s.reap.Do(func() {
		if pids := s.opts.Ownership.ReapOrphans(); len(pids) > 0 {
			parts := make([]string, len(pids))
			for i, pid := range pids {
				parts[i] = fmt.Sprint(pid)
			}
			s.Log("Reaped orphaned desktop backend PID(s): " + strings.Join(parts, ", "))
		}
	})
}

func (s *Service) primaryProfileKey() string {
	return s.pin.Resolve(s.routes.ActiveProfile)
}

// PrimaryProfileKey is the profile the primary launched as, the remembered profile while none is live
func (s *Service) PrimaryProfileKey() string { return s.primaryProfileKey() }

func (s *Service) mainWindowState() map[string]any {
	state, _ := s.host.WindowState(s.host.MainWindowID())
	return state
}

func clampBootProgress(v any) int {
	f, ok := v.(float64)
	if i, isInt := v.(int); isInt {
		f, ok = float64(i), true
	}
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int(math.Max(0, math.Min(100, math.Round(f))))
}

// Merges an update into boot progress, never lowering progress unless allowed, and pushes it to the main window
func (s *Service) UpdateBootProgress(update map[string]any, allowDecrease bool) {
	s.mu.Lock()
	var latched *string
	if s.reauthFailure != nil {
		message := s.reauthFailure.Error()
		latched = &message
	}
	var updateError *string
	if e, ok := update["error"].(string); ok {
		updateError = &e
	}
	if latched != nil && ShouldHoldBootProgressForReauth(*latched, updateError) {
		first := s.heldFor != s.reauthFailure
		s.heldFor = s.reauthFailure
		s.mu.Unlock()
		if first {
			s.Log("[boot] remote reauth latched: holding the recovery overlay against a stale boot-progress update")
		}
		return
	}
	s.heldFor = nil
	prev := s.boot
	next := make(map[string]any, len(prev)+len(update))
	for k, v := range prev {
		next[k] = v
	}
	for k, v := range update {
		next[k] = v
	}
	progress := prev["progress"].(int)
	if _, ok := update["progress"]; ok {
		progress = clampBootProgress(update["progress"])
	}
	if !allowDecrease {
		progress = max(prev["progress"].(int), progress)
	}
	if _, ok := update["error"]; !ok {
		next["error"] = prev["error"]
	}
	fake, _ := update["fakeMode"].(bool)
	next["fakeMode"] = s.opts.FakeMode || fake
	next["progress"] = progress
	if r, ok := update["retryable"]; ok {
		next["retryable"] = r == true
	} else {
		_, hasErr := update["error"]
		next["retryable"] = !hasErr && prev["retryable"] == true
	}
	next["timestamp"] = time.Now().UnixMilli()
	s.boot = next
	s.mu.Unlock()
	if msg, _ := update["message"].(string); msg != "" {
		s.Log("[boot] " + msg)
	}
	s.BroadcastBootProgress()
}

// Moves boot progress to a phase with its message and floor
func (s *Service) AdvanceBootProgress(phase, message string, progress int) {
	s.UpdateBootProgress(map[string]any{"phase": phase, "message": message, "progress": progress, "running": true, "error": nil}, false)
}

func (s *Service) bootSnapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.boot)+1)
	for k, v := range s.boot {
		out[k] = v
	}
	out["updateHold"] = s.runtime.UpdateHold()
	return out
}

// Pushes boot progress to the main window
func (s *Service) BroadcastBootProgress() {
	if id := s.host.MainWindowID(); id != "" {
		s.host.Send(id, "boot-progress", s.bootSnapshot())
	}
}

// Boot progress snapshot, the update hold only for the primary window
func (s *Service) GetBootProgress(windowID string) map[string]any {
	state := s.bootSnapshot()
	if windowID != s.host.MainWindowID() {
		state["updateHold"] = nil
	}
	return state
}

// Tells the main window the backend exited unless a soft rehome is in progress
func (s *Service) SendBackendExit(payload any) {
	s.mu.Lock()
	soft := s.softRehome
	s.mu.Unlock()
	if id := s.host.MainWindowID(); !soft && id != "" {
		s.host.Send(id, "backend-exit", payload)
	}
}

// Tells the main window a connection change was applied
func (s *Service) SendConnectionApplied() {
	if id := s.host.MainWindowID(); id != "" {
		s.host.Send(id, "connection:applied", nil)
	}
}

// Records the route a window published and resets its preview reach when the route changed
func (s *Service) SetActiveConnectionRoute(windowID string, route any) {
	previous := s.windowRoutes.Get(windowID)
	next := s.windowRoutes.Set(windowID, route)
	if previous.connectionID() != next.connectionID() || previous.profile() != next.profile() ||
		(previous != nil && previous.RegistryScoped) != (next != nil && next.RegistryScoped) {
		s.host.ResetPreviewReach(windowID)
	}
	s.mu.Lock()
	s.routeOwners[windowID] = true
	s.mu.Unlock()
}

// Forgets the route of a destroyed window and resets its preview reach
func (s *Service) WindowClosed(windowID string) {
	s.mu.Lock()
	owned := s.routeOwners[windowID]
	delete(s.routeOwners, windowID)
	s.mu.Unlock()
	if owned {
		s.windowRoutes.Delete(windowID)
		s.host.ResetPreviewReach(windowID)
	}
}

// Descriptor of the backend a window asks for, carrying that window live state
func (s *Service) GetConnection(windowID string, profile any) (Connection, error) {
	var named *string
	if p, ok := profile.(string); ok {
		named = &p
	}
	source := s.windowRoutes.Get(windowID)
	connectionID, routeProfile, inherits, err := s.routes.ResolveRequest(named, source, s.primaryProfileKey())
	if err != nil {
		return nil, err
	}
	return s.connectRoute(windowID, connectionID, routeProfile, inherits)
}

func (s *Service) connectRoute(windowID, connectionID, profile string, inherits bool) (Connection, error) {
	inheritsPrimary := connectionID == "" && inherits
	scopeKey := PrimaryDialClaimKey(BackendScopeKey(connectionID, profile), inheritsPrimary)
	connection, err := RunDial(&s.claims, scopeKey, func() (Connection, error) {
		if connectionID != "" {
			return s.routes.EnsureRegistryBackend(connectionID, profile)
		}
		return s.EnsureBackend(profile, EnsureOptions{InheritsPrimary: inheritsPrimary})
	})
	if err != nil {
		return nil, err
	}
	windowState := LiveWindowState(func() (map[string]any, bool) { return s.host.WindowState(windowID) },
		func() (map[string]any, bool) { return s.host.WindowState(s.host.MainWindowID()) })
	if connectionID != "" {
		out := connection.Clone()
		out["connectionId"] = connectionID
		out["registryScoped"] = true
		return OverlayWindowState(out, windowState), nil
	}
	id, err := s.routes.ResolvedConnectionID(connection)
	if err != nil {
		return nil, err
	}
	if id != "" {
		out := connection.Clone()
		out["connectionId"] = id
		return OverlayWindowState(out, windowState), nil
	}
	return OverlayWindowState(connection, windowState), nil
}

// Backend of a profile, joining the primary boot or the profile pool, a passive read never spawning one
func (s *Service) EnsureBackend(profile string, opts EnsureOptions) (Connection, error) {
	var named *string
	if profile != "" {
		named = &profile
	}
	request := NewPrimaryBootRequest(named, s.primaryProfileKey(), opts.InheritsPrimary)
	if err := s.routes.AssertCanStart(request.Key); err != nil {
		return nil, err
	}
	route := s.routes.ProfileRoute(request.Key, opts.Request)
	if route.Backend == "primary" {
		resolveRoute := func(next string) ProfileBackendRoute { return s.routes.ProfileRoute(next, opts.Request) }
		joined, err := JoinPrimaryBoot(request, route, s.primaryProfileKey, resolveRoute, func(boot PrimaryBootRequest) (Connection, error) {
			return s.startPrimary(boot.Key, boot.InheritsPrimary)
		})
		if err != nil {
			return nil, err
		}
		s.routes.SetBridgeProfileState(joined.Key, joined.Connection.str("mode") != "remote")
		return joined.Descriptor, nil
	}
	s.mu.Lock()
	existing := s.pool[request.Key]
	if existing == nil {
		if err := AssertNotPassiveSpawn(opts.Passive, request.Key); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		existing = &poolEntry{}
		entry := existing
		existing.future = NewFuture(func() (Connection, error) {
			conn, err := s.dialPoolBackend(request.Key, entry, poolDialOptions{})
			if err != nil {
				s.Log(fmt.Sprintf("Icebath backend for profile \"%s\" failed to start: %s", request.Key, err.Error()))
				s.mu.Lock()
				if s.pool[request.Key] == entry {
					delete(s.pool, request.Key)
				}
				s.mu.Unlock()
			}
			return conn, err
		})
		s.pool[request.Key] = existing
	}
	s.mu.Unlock()
	conn, err := existing.future.Wait()
	if err != nil {
		return nil, err
	}
	s.routes.SetBridgeProfileState(request.Key, conn.str("mode") != "remote")
	return conn, nil
}

func (s *Service) poolEntryOwned(key string, entry *poolEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pool[key] != entry {
		return SupersededError{}
	}
	return nil
}

func (s *Service) dialPoolBackend(profile string, entry *poolEntry, o poolDialOptions) (Connection, error) {
	// wd_conn_ssh: a registry local dial skips remote resolution and owns a composite pool key
	poolKey := o.PoolKey
	if poolKey == "" {
		poolKey = profile
	}
	if err := s.assertCanStart(); err != nil {
		return nil, err
	}
	s.beginStart()
	defer s.endStart()
	s.reapOrphansOnce()
	if err := s.routes.AssertCanStart(profile); err != nil {
		return nil, err
	}
	var remote Connection
	if !o.ForceLocal {
		var err error
		if remote, err = s.routes.ResolveRemote(profile, RemoteResolveOptions{PoolKey: poolKey}); err != nil {
			return nil, err
		}
	}
	if err := s.routes.AssertCanStart(profile); err != nil {
		return nil, err
	}
	if remote != nil {
		connected, err := s.routes.ConnectRemote(remote, profile, nil)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		entry.remoteBaseURL = remote.str("baseUrl")
		s.mu.Unlock()
		out := connected.Clone()
		out["profile"] = profile
		out["logs"] = s.recentLogs()
		return OverlayWindowState(out, s.mainWindowState()), nil
	}
	if err := s.assertLocalProfileCanStart(profile); err != nil {
		return nil, err
	}
	connection, err := EnsureLocalGateway(func() (EnsureResult, error) {
		backend, err := s.resolveBackend([]string{"--profile", profile, "gateway", "ensure", "--json"})
		if err != nil {
			return EnsureResult{}, err
		}
		backend, err = s.runtime.EnsureRuntime(backend, func() error { return s.poolEntryOwned(poolKey, entry) })
		if err != nil {
			return EnsureResult{}, err
		}
		if err := s.routes.AssertCanStart(profile); err != nil {
			return EnsureResult{}, err
		}
		if err := s.poolEntryOwned(poolKey, entry); err != nil {
			return EnsureResult{}, err
		}
		return s.runEnsure(backend, profile, EnsureRunOptions{})
	}, func() error {
		if err := s.runtime.WaitForPoolUpdate(profile, func() bool { return s.poolEntryOwned(poolKey, entry) != nil }); err != nil {
			return err
		}
		if err := s.poolEntryOwned(poolKey, entry); err != nil {
			return err
		}
		return s.assertLocalProfileCanStart(profile)
	}, s.restartStale)
	if err != nil {
		return nil, err
	}
	if err := s.poolEntryOwned(poolKey, entry); err != nil {
		return nil, err
	}
	out := connection.Clone()
	out["profile"] = profile
	out["logs"] = s.recentLogs()
	return OverlayWindowState(out, s.mainWindowState()), nil
}

// Refuses a profile whose deletion is in flight or whose home directory is gone
func (s *Service) assertLocalProfileCanStart(profile string) error {
	key := strings.ToLower(strings.TrimSpace(profile))
	if err := s.routes.AssertCanStart(key); err != nil {
		return err
	}
	if key != "" && key != "default" {
		if info, err := os.Stat(PathsFor(electronPlatformName()).Join(s.opts.IcebathHome, "profiles", key)); err != nil || !info.IsDir() {
			return fmt.Errorf("Profile \"%s\" no longer exists.", key)
		}
	}
	return nil
}

func (s *Service) resolveBackend(args []string) (ResolvedBackend, error) {
	return s.opts.Resolve(args)
}

func (s *Service) runEnsure(backend ResolvedBackend, profile string, opts EnsureRunOptions) (EnsureResult, error) {
	command := backend.BackendCommand
	command.Env = DesktopBackendSpawnEnv(command.Env)
	parent := ProfileBackendParentEnv(s.opts.IcebathHome, profile, s.opts.Env(), electronPlatformName(), ReadTextOrEmpty)
	return RunGatewayEnsure(command, s.opts.Cwd(), s.opts.IcebathHome, parent, opts)
}

func (s *Service) restartLocalGatewayOwner(owner string) {
	result, err := RunGatewayOwnerRestart(func(args []string) (EnsureResult, error) {
		backend, err := s.resolveBackend(args)
		if err != nil {
			return EnsureResult{}, err
		}
		backend, err = s.runtime.EnsureRuntime(backend, func() error { return nil })
		if err != nil {
			return EnsureResult{}, err
		}
		return s.runEnsure(backend, owner, EnsureRunOptions{Timeout: gatewayRestartClientTimeout, Label: "icebath gateway restart"})
	}, owner)
	if err != nil {
		s.Log(fmt.Sprintf("[gateway] icebath --profile %s gateway restart exited 7: %s", owner, err.Error()))
		return
	}
	reason := ""
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout+"\n"+result.Stderr), "\n") {
		if strings.TrimSpace(line) != "" {
			reason = strings.TrimRight(line, "\r")
		}
	}
	if reason != "" {
		reason = ": " + reason
	}
	s.Log(fmt.Sprintf("[gateway] icebath --profile %s gateway restart exited %d%s", owner, result.Code, reason))
}

func (s *Service) beginStart() {
	s.starts.Add(1)
	s.inflight.Add(1)
}

func (s *Service) endStart() {
	s.inflight.Add(-1)
	s.starts.Done()
}

// Refuses a backend start once the desktop began quitting
func (s *Service) assertCanStart() error {
	return context.Cause(s.quit)
}

// Refuses a start once the desktop is quitting, with the quit cause
func (s *Service) AssertCanStart() error {
	return s.assertCanStart()
}

// Context cancelled with the quit cause when the desktop starts quitting
func (s *Service) QuitContext() context.Context {
	return s.quit
}

// Aborts every backend start, waiting for the pending ones up to the teardown timeout
func (s *Service) Shutdown() error {
	return s.shutdown.Run()
}

// Whether a backend start or the quit teardown is still in flight
func (s *Service) HasPending() bool {
	return s.inflight.Load() > 0 || s.state.PendingPromise() != nil || s.shutdown.IsPending()
}

func (s *Service) startPrimary(requestedProfile string, inheritsPrimary bool) (Connection, error) {
	if !s.host.IsPrimaryInstance() {
		s.Log("[boot] non-primary instance: skipping backend machinery")
		return nil, errors.New(nonPrimaryInstanceBootFailed)
	}
	s.reapOrphansOnce()
	if err := s.assertCanStart(); err != nil {
		return nil, err
	}
	if latched := s.latchedBootFailure(); latched != nil {
		return nil, latched
	}
	if s.bootFakeError != "" {
		s.AdvanceBootProgress("backend.resolve", "Resolving Icebath backend", 8)
		err := &BootstrapFailureError{Message: s.bootFakeError}
		s.runtime.SetBootstrapFailure(err)
		return nil, err
	}
	if existing := s.state.Promise(); existing != nil {
		return existing.Wait()
	}
	if err := s.routes.MigrateActiveProfileIfMissing(); err != nil {
		return nil, err
	}
	attempt, err := s.state.StartAttempt()
	if err != nil {
		return nil, err
	}
	launch := LaunchProfile{ArgvProfile: requestedProfile, RoutingProfile: requestedProfile}
	if requestedProfile == "" {
		launch = ResolveLaunchProfile(s.routes.ActiveProfile)
	}
	primaryProfile := s.pin.Pin(launch.RoutingProfile)
	s.routes.SetActiveGatewayProfile(primaryProfile)
	attemptedRemote := s.routes.PrimaryIsRemote()
	boot := PrimaryRemoteBoot{RequestedProfile: requestedProfile, InheritsPrimary: inheritsPrimary,
		Context: s.quit, AssertCurrentAttempt: func() error { return s.state.AssertCurrentAttempt(attempt) },
		OnProfileRestored: func(profile string) {
			s.Log(fmt.Sprintf("[profile] remote primary does not serve \"%s\"; booting it as \"%s\"", primaryProfile, profile))
			primaryProfile = s.pin.Pin(profile)
			s.routes.SetActiveGatewayProfile(profile)
		}}
	published := make(chan struct{})
	s.beginStart()
	future := NewFuture(func() (Connection, error) {
		defer s.endStart()
		<-published
		conn, err := s.bootPrimary(attempt, launch, &primaryProfile, &attemptedRemote, boot)
		if err == nil {
			return conn, nil
		}
		return nil, s.failPrimary(attempt, err, attemptedRemote)
	})
	s.state.SetPromise(attempt, future)
	close(published)
	return future.Wait()
}

func (s *Service) bootPrimary(attempt *ConnectionAttempt[Connection], launch LaunchProfile, profile *string, attemptedRemote *bool, boot PrimaryRemoteBoot) (Connection, error) {
	assert := func() error { return s.state.AssertCurrentAttempt(attempt) }
	s.AdvanceBootProgress("backend.resolve", "Resolving Icebath backend", 8)
	if loginPath := ensureLoginShellPath(); loginPath.Applied {
		s.Log("[env] merged login-shell PATH into process.env for backend spawn")
	} else if loginPath.Reason != "" && loginPath.Reason != "win32" && loginPath.Reason != "unchanged" {
		s.Log(fmt.Sprintf("[env] login-shell PATH resolution unavailable (%s); keeping inherited PATH", loginPath.Reason))
	}
	args := []string{"gateway", "ensure", "--json"}
	if launch.ArgvProfile != "" {
		args = append([]string{"--profile", launch.ArgvProfile}, args...)
	}
	setup, err := RunPrimaryBackendStartup(PrimaryStartupOptions[ResolvedBackend, Connection, Connection]{
		Context:              s.quit,
		AssertCurrentAttempt: assert,
		ConnectRemote:        func(remote Connection) (Connection, error) { return s.routes.ConnectRemote(remote, *profile, &boot) },
		EnsureLocalRuntime: func(b ResolvedBackend) (ResolvedBackend, error) {
			if err := s.assertCanStart(); err != nil {
				return b, err
			}
			return s.runtime.EnsureRuntime(b, assert)
		},
		PrepareLocalBackend: func() (ResolvedBackend, error) {
			s.AdvanceBootProgress("backend.runtime", "Resolving Icebath runtime", 28)
			return s.resolveBackend(args)
		},
		ResolveRemote: func() (*Connection, error) {
			*attemptedRemote = s.routes.PrimaryIsRemote()
			remote, err := s.routes.ResolveRemote(*profile, RemoteResolveOptions{Primary: true})
			if remote == nil {
				return nil, err
			}
			return &remote, err
		},
		SelectRegistryPrimary: func() (*Connection, error) {
			*attemptedRemote = true
			remote, err := s.routes.ResolveRemote(*profile, RemoteResolveOptions{Primary: true, ForceRegistryPrimary: true})
			if remote == nil {
				return nil, err
			}
			return &remote, err
		},
		WaitForDecision:   s.routes.WaitForFirstRunDecision,
		WaitForLocalStart: s.runtime.WaitForUpdate,
	})
	if err != nil {
		return nil, err
	}
	if err := assert(); err != nil {
		return nil, err
	}
	if setup.Kind == StartupRemote {
		s.routes.SetBridgeProfileState(*profile, false)
		return setup.Connection, nil
	}
	if setup.Kind == StartupAttached {
		return nil, errors.New("Unexpected attached backend outcome without a host attach step.")
	}
	s.routes.SetBridgeProfileState(*profile, true)
	connection, err := EnsureLocalGateway(func() (EnsureResult, error) {
		return s.runEnsure(setup.Backend, *profile, EnsureRunOptions{})
	}, nil, s.restartStale)
	if err != nil {
		return nil, err
	}
	if err := assert(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.startFailure = nil
	s.mu.Unlock()
	s.Log("[backend] Icebath gateway is ready")
	s.UpdateBootProgress(map[string]any{"phase": "backend.ready", "message": "Icebath gateway is ready", "progress": 94, "running": true, "error": nil}, false)
	s.runtime.ResetBootstrapRepair()
	out := connection.Clone()
	out["profile"] = *profile
	out["logs"] = s.recentLogs()
	return OverlayWindowState(out, s.mainWindowState()), nil
}

func (s *Service) failPrimary(attempt *ConnectionAttempt[Connection], err error, attemptedRemote bool) error {
	if !s.state.ClearPromiseForAttempt(attempt) {
		return err
	}
	s.pin.Clear()
	var reset FirstRunSetupResetError
	if errors.As(err, &reset) {
		return err
	}
	hostKeyChanged := IsHostKeyChangedBootFailure(err)
	sshAuthFailed := IsSSHAuthFailedBootFailure(err)
	sshClientFailed := IsSSHClientFailedBootFailure(err)
	var cloud *CloudBackendDownError
	isCloudDown := errors.As(err, &cloud)
	var statusCode any
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		statusCode = httpErr.StatusCode
	}
	s.mu.Lock()
	if ShouldLatchBackendStartFailure(BackendStartFailureContext{AttemptedRemote: attemptedRemote}) ||
		ShouldLatchHostKeyChangedFailure(RemoteBootRetryContext{AttemptedRemote: attemptedRemote, IsHostKeyChanged: hostKeyChanged}) ||
		ShouldLatchSSHAuthFailure(RemoteBootRetryContext{AttemptedRemote: attemptedRemote, IsSSHAuthFailed: sshAuthFailed}) ||
		ShouldLatchSSHClientFailure(RemoteBootRetryContext{AttemptedRemote: attemptedRemote, IsSSHClientFailed: sshClientFailed}) {
		s.startFailure = err
	}
	if ShouldLatchRemoteReauthFailure(RemoteBootRetryContext{AttemptedRemote: attemptedRemote, IsReauth: IsReauthRequiredError(err)}) {
		s.reauthFailure = err
	}
	s.mu.Unlock()
	var cloudFlag any
	if isCloudDown {
		cloudFlag = true
	}
	s.UpdateBootProgress(map[string]any{
		"error":              err.Error(),
		"isCloudBackendDown": cloudFlag,
		"message":            "Desktop boot failed: " + err.Error(),
		"phase":              "backend.error",
		"retryable": IsRetryableRemoteBootFailure(RemoteBootRetryContext{AttemptedRemote: attemptedRemote, IsReauth: IsReauthRequiredError(err),
			IsHostKeyChanged: hostKeyChanged, IsSSHAuthFailed: sshAuthFailed, IsSSHClientFailed: sshClientFailed}),
		"running":    false,
		"statusCode": statusCode,
	}, true)
	return err
}

// Drops the cached primary after a dead remote so the next dial rebuilds it
func (s *Service) RevalidateConnection() (RevalidateResult, error) {
	pending := s.state.Promise()
	if pending == nil {
		if err := s.revalidatePool(); err != nil {
			return RevalidateResult{}, err
		}
		return RevalidateResult{OK: true}, nil
	}
	return RunDial(&s.revalidation, fmt.Sprintf("%p", pending), func() (RevalidateResult, error) { return s.revalidatePrimary(pending) })
}

func (s *Service) revalidatePrimary(pending *Future[Connection]) (RevalidateResult, error) {
	var result RevalidateResult
	var primaryErr, poolErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result, primaryErr = s.routes.RevalidatePrimary(pending, func() { s.resetConnectionState(true) })
	}()
	go func() {
		defer wg.Done()
		poolErr = s.revalidatePool()
	}()
	wg.Wait()
	if primaryErr != nil {
		return result, primaryErr
	}
	if poolErr != nil {
		return result, poolErr
	}
	if result.Rebuilt {
		if conn, err := pending.Wait(); err == nil && conn.str("remoteKind") == "ssh" {
			if err := s.routes.TeardownSSH(BackendScopeKey("", s.primaryProfileKey())); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

// Terminal boot failure latched in this process, cleared only by reset, repair, apply or sign in
func (s *Service) latchedBootFailure() error {
	if err := s.runtime.BootstrapFailure(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startFailure != nil {
		return s.startFailure
	}
	return s.reauthFailure
}

func (s *Service) resetConnectionState(soft bool) {
	s.mu.Lock()
	s.startFailure = nil
	s.reauthFailure = nil
	s.mu.Unlock()
	s.routes.ClearRemoteLiveness()
	s.pin.Clear()
	s.state.Invalidate()
	if !soft {
		s.UpdateBootProgress(map[string]any{"error": nil, "message": "Restarting desktop connection", "phase": "backend.resolve", "progress": 4, "running": true}, true)
	}
}

// Drops the primary connection and its latched failures, a soft teardown keeping boot progress and backend exit quiet
func (s *Service) TeardownPrimary(soft bool) error {
	if soft {
		s.mu.Lock()
		s.softRehome = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.softRehome = false
			s.mu.Unlock()
		}()
	}
	s.resetConnectionState(soft)
	return nil
}

func (s *Service) forgetLocalGatewayDescriptor(profile string) error {
	key := strings.TrimSpace(profile)
	if key == "" {
		key = s.primaryProfileKey()
	}
	if s.routes.ProfileRoute(key, nil).Backend == "primary" {
		if attempt, err := s.state.StartAttempt(); err == nil {
			s.state.SetPromise(attempt, s.state.Promise())
			s.state.ClearPromiseForAttempt(attempt)
		}
		s.Log(fmt.Sprintf("[gateway] forgot stale canonical endpoint for primary \"%s\"; re-ensuring", key))
		return nil
	}
	s.mu.Lock()
	_, ok := s.pool[key]
	delete(s.pool, key)
	s.mu.Unlock()
	if ok {
		s.Log(fmt.Sprintf("[gateway] forgot stale canonical endpoint for profile \"%s\"; re-ensuring", key))
	}
	return nil
}

// GatewayWsURLResult is the reply of getGatewayWsUrl
type GatewayWsURLResult struct {
	OK              bool   `json:"ok"`
	WsURL           string `json:"wsUrl,omitempty"`
	Error           string `json:"error,omitempty"`
	NeedsOauthLogin bool   `json:"needsOauthLogin,omitempty"`
}

// Fresh gateway socket URL for a profile, a private one use grant for a native local gateway
func (s *Service) GetGatewayWsURL(windowID string, profile any) GatewayWsURLResult {
	name, _ := profile.(string)
	url, err := s.freshGatewayWsURL(windowID, name)
	if err != nil {
		return GatewayWsURLResult{Error: err.Error(), NeedsOauthLogin: s.routes.IsAuthRejection(err)}
	}
	return GatewayWsURLResult{OK: true, WsURL: url}
}

func (s *Service) freshGatewayWsURL(windowID, profile string) (string, error) {
	connection, err := s.EnsureBackend(profile, EnsureOptions{})
	if err != nil {
		return "", err
	}
	if connection.Endpoint() != nil {
		return LocalGatewayDial(LocalDial{
			Ensure:     func() (Connection, error) { return s.EnsureBackend(profile, EnsureOptions{}) },
			Forget:     func() error { return s.forgetLocalGatewayDescriptor(profile) },
			LaunchHome: s.opts.IcebathHome,
			Prepare: func(baseURL, ticket string) (string, error) {
				return s.dials.Prepare(baseURL, ticket, windowID)
			},
			Profile: profile,
		})
	}
	return s.routes.RemoteWsURL(connection)
}

// Handshake headers the shell applies to a reserved native socket dial, nil when the dial is not reserved
func (s *Service) DialHeaders(req DialRequest) map[string]string {
	return s.dials.Headers(req)
}

// LocalProbe is the reply of probeLocalBackend
type LocalProbe struct {
	BootstrapNeeded bool `json:"bootstrapNeeded"`
}

// Whether this machine has no icebath runtime yet, resolution only
func (s *Service) ProbeLocalBackend() (LocalProbe, error) {
	backend, err := s.resolveBackend(nil)
	if err != nil {
		return LocalProbe{}, err
	}
	return LocalProbe{BootstrapNeeded: backend.Kind == "bootstrap-needed"}, nil
}

// Recycles the owned backend of a profile after a code skew 503, keeping the renderer mounted
func (s *Service) RecycleBackend(profile any) (map[string]bool, error) {
	name, _ := profile.(string)
	_, err := RecycleOwnedBackend(RecycleDeps{
		NotifyApplied:       s.SendConnectionApplied,
		PrimaryProfile:      s.primaryProfileKey(),
		Profile:             name,
		RestartLocalGateway: s.restartAttachedLocalGateway,
		TeardownPool:        s.TeardownPoolBackend,
		TeardownPrimary:     func() error { return s.TeardownPrimary(true) },
		TeardownSSH:         s.routes.TeardownSSH,
	})
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

// Drops the pool backends of a local profile and tears down their SSH state, logging teardown failures
func (s *Service) TeardownPoolBackend(profile string) error {
	keys := LocalProfilePoolKeys(profile)
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.stopPoolBackend(key)
			if err := s.routes.TeardownSSH(key); err != nil {
				s.Log(fmt.Sprintf("[ssh-teardown] %s: %s", key, err.Error()))
			}
		}()
	}
	wg.Wait()
	return nil
}

func (s *Service) stopPoolBackend(key string) error {
	s.mu.Lock()
	delete(s.pool, key)
	s.mu.Unlock()
	return nil
}

// Forgets every pooled backend
func (s *Service) StopAllPoolBackends() {
	s.mu.Lock()
	s.pool = map[string]*poolEntry{}
	s.mu.Unlock()
}

func (s *Service) poolBackends() []PoolBackend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PoolBackend, 0, len(s.pool))
	for key, entry := range s.pool {
		out = append(out, PoolBackend{Key: key, RemoteBaseURL: entry.remoteBaseURL, Connection: entry.future})
	}
	return out
}

func (s *Service) revalidatePool() error {
	return s.routes.RevalidatePool(s.poolBackends(), s.stopPoolBackend)
}

func (s *Service) restartAttachedLocalGateway(profile string) error {
	var pending *Future[Connection]
	if profile == s.primaryProfileKey() {
		pending = s.state.Promise()
	} else {
		s.mu.Lock()
		for _, key := range LocalProfilePoolKeys(profile) {
			if entry := s.pool[key]; entry != nil && entry.future != nil {
				pending = entry.future
				break
			}
		}
		s.mu.Unlock()
	}
	if pending == nil {
		return nil
	}
	conn, err := pending.Wait()
	if err != nil {
		return nil
	}
	if ep := conn.Endpoint(); ep != nil {
		s.restartLocalGatewayOwner(GatewayOwnerProfile(ownerEndpoint(*ep)))
	}
	return nil
}

func ownerEndpoint(ep GatewayEndpoint) map[string]any {
	m := map[string]any{"profile_id": ep.ProfileID}
	if ep.ControlHome != nil {
		m["control_home"] = *ep.ControlHome
	}
	return m
}

// Node platform name of the running OS, the spelling the desktop path helpers take
func electronPlatformName() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	}
	return runtime.GOOS
}

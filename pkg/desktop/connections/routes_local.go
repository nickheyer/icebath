package connections

import (
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/files"
)

// BootstrapSurface is the updater bootstrap state the first run setup choice shows through
type BootstrapSurface interface {
	// Sends a bootstrap event to every window and folds it into the bootstrap snapshot
	BroadcastBootstrapEvent(ev map[string]any)
	// Whether the bootstrap snapshot holds a setup choice
	SetupChoiceShown() bool
	// Shape of the running install, bundled or checkout
	InstallShape() string
}

// Backend, bootstrap surface and profile gates the registry routes read and hold
type localSlice struct {
	backend            *backend.Service
	boot               BootstrapSurface
	deletion           ProfileDeletionGate
	firstRunOnce       sync.Once
	firstRun           *FirstRunSetupGate
	migrationAttempted atomic.Bool
	mcpOAuth           mcpOAuthListeners
	remoteOwners       *RemoteOwnerCache
	primaryInstallOnce sync.Once
	primaryInstall     *PrimaryInstallIDResolver
	broadcast          func(event string, payload any)
	flushLog           func()
	reloadMainWindow   func()
}

// Milliseconds a first run setup choice waits before it is reported stuck
const firstRunStuckAfterMs = 120000

// Whether the app launched with a private backend per profile
var isolatedBackend = os.Getenv("ICEBATH_DESKTOP_ISOLATED_BACKEND") == "1"

// Attach wires the backend service the routes read the primary and boot progress from, and the updater bootstrap surface
func (s *Service) Attach(b *backend.Service, boot BootstrapSurface) {
	s.local.backend = b
	s.local.boot = boot
}

// The v1 connection settings as a JSON object
func (s *Service) lockedConnectionConfig() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readDesktopConnectionConfig().object()
}

// The v2 registry
func (s *Service) lockedConnectionsRegistry() (ConnectionRegistry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readDesktopConnectionsRegistry()
}

// Whether one remote backend serves every profile through the env override, the global mode or a remote registry primary
func (s *Service) globalRemoteActive() bool {
	if os.Getenv("ICEBATH_DESKTOP_REMOTE_URL") != "" {
		return true
	}
	mode := s.lockedConnectionConfig()["mode"]
	if ModeIsRemoteLike(mode) || mode == "ssh" {
		return true
	}
	return s.registryPrimaryIsRemote()
}

// Whether the registry primary names a remote, cloud or SSH connection
func (s *Service) registryPrimaryIsRemote() bool {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return false
	}
	entry := registry.Find(registry.Primary)
	return entry != nil && (entry.Kind == "remote" || entry.Kind == "cloud" || entry.Kind == "ssh")
}

// Whether the primary profile backend resolves to a remote host
func (s *Service) primaryBackendIsRemote() bool {
	return ProfileHasRemoteConnection(s.lockedConnectionConfig(), s.local.backend.PrimaryProfileKey()) || s.globalRemoteActive()
}

// Connection facts a profile backend route is decided from
func (s *Service) profileRouteOptions(profile string, request *backend.RouteRequest) ProfileRouteOptions {
	config := s.lockedConnectionConfig()
	sshOverride := ProfileSSHOverride(config, profile)
	urlOverride := ProfileRemoteOverride(config, profile)
	primary := s.local.backend.PrimaryProfileKey()
	key := ConnectionScopeKey(profile)
	if key == "" {
		key = primary
	}
	o := ProfileRouteOptions{
		GlobalRemote:          s.globalRemoteActive(),
		PrimaryProfile:        primary,
		ProfileRemoteOverride: urlOverride != nil || sshOverride != nil,
		PrimaryRemoteActive:   s.primaryBackendIsRemote(),
		OwnEntry:              jsTruthy(jsObject(config["profiles"])[key]),
		IsolatedBackend:       isolatedBackend,
	}
	if sshOverride != nil && sshOverride.RemoteProfile != "" {
		o.BackendProfile = sshOverride.RemoteProfile
	} else if urlOverride != nil {
		o.BackendProfile = urlOverride.RemoteProfile
	}
	if request != nil {
		o.RequestMethod = request.Method
		o.RequestPath = request.Path
	}
	return o
}

// ProfileRoute picks the primary or a pooled backend for a profile key, request nil for a descriptor request
func (s *Service) ProfileRoute(key string, request *backend.RouteRequest) backend.ProfileBackendRoute {
	route := ResolveProfileBackendRoute(key, s.profileRouteOptions(key, request))
	return backend.ProfileBackendRoute{Backend: route.Backend, DescriptorProfile: route.DescriptorProfile, ScopePath: route.ScopePath}
}

// IsAuthRejection reports a gateway authentication rejection the renderer answers with a login
func (s *Service) IsAuthRejection(err error) bool { return IsGatewayAuthRejection(err) }

// SetActiveGatewayProfile records the profile whose home the WSL path bridge maps for native paths
func (s *Service) SetActiveGatewayProfile(profile string) { files.SetActiveGatewayProfile(&profile) }

// SetBridgeProfileState records whether a profile is served by this machine for the WSL path bridge
func (s *Service) SetBridgeProfileState(profile string, local bool) {
	files.SetWslBridgeProfileState(&profile, local)
}

// ResolveRequest settles a renderer request on its window route when it names no profile, else on the named or primary profile
func (s *Service) ResolveRequest(requested *string, source *backend.WindowConnectionRoute, primaryKey string) (string, string, bool, error) {
	name := ""
	if requested != nil {
		name = jsTrim(*requested)
	}
	sourceProfile := source != nil && source.Profile != nil && *source.Profile != ""
	inherits := name == "" && !sourceProfile
	if sourceProfile && name == "" {
		var connectionID any
		if source.RegistryScoped && source.ConnectionID != nil {
			connectionID = *source.ConnectionID
		}
		route, err := RequireDesktopProfileRoute(map[string]any{"connectionId": connectionID, "profile": *source.Profile})
		if err != nil {
			return "", "", false, err
		}
		id := ""
		if route.ConnectionID != nil {
			id = *route.ConnectionID
		}
		return id, route.Profile, inherits, nil
	}
	if name == "" {
		name = primaryKey
	}
	return "", name, inherits, nil
}

// ResolvedConnectionID is the registry id a primary or pooled descriptor belongs to, empty when unregistered
func (s *Service) ResolvedConnectionID(conn backend.Connection) (string, error) {
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return "", err
	}
	return ResolvedConnectionID(registry, conn), nil
}

// AssertCanStart refuses a profile whose deletion is in flight
func (s *Service) AssertCanStart(profile string) error {
	return s.local.deletion.AssertCanStart(profile)
}

// FirstRunSetupGate is the first run setup choice gate the boot, the updater and the connection apply share
func (s *Service) FirstRunSetupGate() *FirstRunSetupGate {
	s.local.firstRunOnce.Do(func() {
		s.local.firstRun = NewFirstRunSetupGate(FirstRunSetupGateOptions{
			HideChoice: s.hideFirstRunSetupChoice,
			Log:        s.log,
			OnStuck: func(_ FirstRunSetupBackend, stuckAfterMs float64) {
				s.local.backend.UpdateBootProgress(map[string]any{
					"error":    nil,
					"message":  "Still waiting for first-run setup choice after " + jsNumber(jsMathRound(stuckAfterMs/1000)) + " seconds",
					"phase":    "bootstrap.choice",
					"progress": 12,
					"running":  true,
				}, true)
			},
			PromptChoice: s.promptFirstRunSetupChoice,
			StuckAfterMs: firstRunStuckAfterMs,
		})
	})
	return s.local.firstRun
}

// Math.round of x, halves rounding toward positive infinity
func jsMathRound(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == math.Trunc(x) {
		return x
	}
	r := math.Floor(x)
	if x-r >= 0.5 {
		return r + 1
	}
	return r
}

// Shows the setup choice card for a gated bootstrap-needed backend
func (s *Service) promptFirstRunSetupChoice(b FirstRunSetupBackend) {
	platform := b.Platform
	if platform == "" {
		platform = s.platform
	}
	activeRoot := b.ActiveRoot
	if activeRoot == "" {
		activeRoot = filepath.Join(s.icebathHome, "icebath-agent")
	}
	local := b.Local
	if local == "" {
		local = "none"
	}
	s.local.boot.BroadcastBootstrapEvent(map[string]any{
		"type":       "setup-choice",
		"active":     true,
		"platform":   platform,
		"activeRoot": activeRoot,
		"local":      local,
		"bundled":    s.local.boot.InstallShape() == "bundled",
	})
}

// Hides a shown setup choice card
func (s *Service) hideFirstRunSetupChoice() {
	if s.local.boot.SetupChoiceShown() {
		s.local.boot.BroadcastBootstrapEvent(map[string]any{"type": "setup-choice", "active": false})
	}
}

// WaitForFirstRunDecision holds a bootstrap-needed primary boot until the user picks local setup or a remote
func (s *Service) WaitForFirstRunDecision(b backend.ResolvedBackend) (string, error) {
	gate := s.FirstRunSetupGate()
	gated := &FirstRunSetupBackend{Kind: b.Kind}
	if !gate.ShouldGate(gated) {
		return string(FirstRunContinueLocal), nil
	}
	s.local.backend.UpdateBootProgress(map[string]any{
		"error":    nil,
		"message":  "Waiting for first-run setup choice",
		"phase":    "bootstrap.choice",
		"progress": 12,
		"running":  true,
	}, true)
	return string(gate.Wait(gated)), nil
}

// ContinueFirstRunLocalBootstrap confirms local setup and resumes the gated boot into it
func (s *Service) ContinueFirstRunLocalBootstrap() { s.FirstRunSetupGate().ContinueLocal() }

// AbandonFirstRunSetupChoiceForRemoteApply resumes the gated boot with an applied remote, false when no boot waited
func (s *Service) AbandonFirstRunSetupChoiceForRemoteApply() bool {
	gate := s.FirstRunSetupGate()
	if !gate.HasWaiter() {
		return false
	}
	resumed := gate.AbandonForRemoteApply()
	if resumed {
		s.local.boot.BroadcastBootstrapEvent(map[string]any{"type": "dismissed"})
	}
	return resumed
}

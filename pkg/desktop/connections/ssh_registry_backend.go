package connections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// SSHConnectionState is the published SSH connection of one scope with the serve it owns
type SSHConnectionState struct {
	SSH                  *SSHConnection
	Fingerprint          string
	OwnershipID          string
	LocalPort            int
	RemotePort           int
	PID                  int
	Host                 string
	HostLabel            string
	HermesVersion        string
	RemotePlatform       string
	Reused               bool
	SpawnNonce           string
	CreationTimeNs       string
	CreationTime         string
	StartedAt            string
	HermesPath           string
	HermesHome           string
	ExecutablePath       string
	RemoteProfile        string
	RegistryConnectionID string
	PrimaryRegistryScope bool

	published uint64
}

// TerminalSessions is the pty group surface an SSH teardown closes the terminals of its scope through
type TerminalSessions interface {
	DisposeTerminalSessionsForSSHScope(scope string)
}

type sshSlice struct {
	mu          sync.Mutex
	connections map[string]*SSHConnectionState
	keepalives  *SSHIsolatedKeepaliveRegistry
	coordinator *SSHBootstrapCoordinator
	teardowns   *SSHTeardownTracker
	gate        *ManagedConnectionUpdateGate
	dispatch    *RemoteRevalidationCoordinator
	terminals   TerminalSessions
	published   uint64
	updates     map[string]*managedOperation
	recoveries  map[string]*managedOperation

	quitJoinStarted bool
	quitJoinDone    bool

	installOnce    sync.Once
	installationID string
	installErr     error
}

func newSSHSlice(s *Service) *sshSlice {
	return &sshSlice{
		connections: map[string]*SSHConnectionState{},
		keepalives:  NewSSHIsolatedKeepaliveRegistry(func(line string) { s.sshRememberLog(line) }),
		coordinator: NewSSHBootstrapCoordinator(),
		teardowns:   NewSSHTeardownTracker(),
		gate:        NewManagedConnectionUpdateGate(s.managedSSHDurableOwner),
		dispatch:    NewRemoteRevalidationCoordinator(),
		updates:     map[string]*managedOperation{},
		recoveries:  map[string]*managedOperation{},
	}
}

// Wires the pty group whose terminal sessions an SSH teardown closes
func (s *Service) AttachTerminalSessions(t TerminalSessions) {
	s.ssh.mu.Lock()
	s.ssh.terminals = t
	s.ssh.mu.Unlock()
}

// RemoteRevalidationCoordinator shares one in flight revalidation per cached connection future
type RemoteRevalidationCoordinator struct {
	mu       sync.Mutex
	inflight map[*backend.Future[backend.Connection]]*backend.Future[backend.Connection]
}

// NewRemoteRevalidationCoordinator is an idle coordinator
func NewRemoteRevalidationCoordinator() *RemoteRevalidationCoordinator {
	return &RemoteRevalidationCoordinator{inflight: map[*backend.Future[backend.Connection]]*backend.Future[backend.Connection]{}}
}

// Joins the revalidation in flight for connection, else runs task and forgets it once settled
func (c *RemoteRevalidationCoordinator) Run(connection *backend.Future[backend.Connection], task func() (backend.Connection, error)) (backend.Connection, error) {
	c.mu.Lock()
	if existing := c.inflight[connection]; existing != nil {
		c.mu.Unlock()
		return existing.Wait()
	}
	pending := backend.NewFuture(task)
	c.inflight[connection] = pending
	c.mu.Unlock()
	result, err := pending.Wait()
	c.mu.Lock()
	if c.inflight[connection] == pending {
		delete(c.inflight, connection)
	}
	c.mu.Unlock()
	return result, err
}

func (s *Service) backendService() *backend.Service { return s.local.backend }

// Redacted line into desktop.log
func (s *Service) sshRememberLog(chunk string) {
	if s.log != nil {
		s.log(RedactSecrets(chunk))
	}
}

func (s *Service) desktopInstallationID() (string, error) {
	s.ssh.installOnce.Do(func() {
		s.ssh.installationID, s.ssh.installErr = backend.LoadOrCreateInstallationID(filepath.Join(s.userData, "desktop-installation.json"))
	})
	return s.ssh.installationID, s.ssh.installErr
}

// Remote ownership id of a scope under this desktop installation
func (s *Service) sshOwnershipKey(profile string) (string, error) {
	id, err := s.desktopInstallationID()
	if err != nil {
		return "", err
	}
	return backend.SSHOwnershipID(id, ConnectionScopeKey(profile))
}

var authRejected = regexp.MustCompile(`^(401|403|404):`)

// Reuse classification of an existing serve from its authenticated ownership proof
func (s *Service) sshProbeReuseProof(baseURL, token, spawnNonce string) (string, error) {
	proof, err := s.fetchJSON(context.Background(), baseURL+"/api/ssh/ownership", &token, JSONOptions{})
	if err != nil {
		if authRejected.MatchString(err.Error()) {
			return "authenticated-stale", nil
		}
		return "", err
	}
	return ClassifySSHReuseProof(toJSONValue(proof), spawnNonce), nil
}

// Kills the owned remote serve before closing the transport of a scope, Windows remotes keeping theirs
func (s *Service) teardownSSHConnection(profile string) error {
	run, err := s.beginSSHTeardown(profile)
	if err != nil {
		return err
	}
	if run != nil {
		run()
	}
	return nil
}

// Unpublishes a scope and holds its transport, returning the remote kill and close, nil when nothing is published
func (s *Service) beginSSHTeardown(profile string) (func(), error) {
	scope := ConnectionScopeKey(profile)
	s.ssh.keepalives.Stop(scope)
	s.ssh.mu.Lock()
	state := s.ssh.connections[scope]
	if state == nil {
		s.ssh.mu.Unlock()
		return nil, nil
	}
	delete(s.ssh.connections, scope)
	terminals := s.ssh.terminals
	s.ssh.mu.Unlock()
	if terminals != nil {
		terminals.DisposeTerminalSessionsForSSHScope(scope)
	}
	ownershipID := state.OwnershipID
	if ownershipID == "" {
		key, err := s.sshOwnershipKey(profile)
		if err != nil {
			return nil, err
		}
		ownershipID = key
	}
	release := s.ssh.teardowns.Begin(state.SSH)
	return func() {
		defer release()
		backend.TeardownSSHState(backend.SSHTeardownState{SSH: state.SSH, OwnershipID: ownershipID, LocalPort: state.LocalPort, RemotePort: state.RemotePort},
			func(transport backend.SSHTransport, ownership string) error {
				if state.RemotePlatform == "Windows" {
					s.sshRememberLog("[ssh] skip remote serve teardown on Windows remotes; POSIX disconnect does not apply")
					return nil
				}
				return RemoteDisconnect(state.SSH, ownership)
			})
	}, nil
}

// Seals the bootstrap coordinator so no reconnect spawns a serve for a quitting app
func (s *Service) SealSSHForQuit() { s.ssh.coordinator.Shutdown() }

// Whether quit must wait for published SSH scopes, bootstraps or settling teardowns
func (s *Service) SSHQuitNeedsWait() bool {
	s.ssh.mu.Lock()
	connections := len(s.ssh.connections)
	s.ssh.mu.Unlock()
	return connections > 0 || len(s.ssh.coordinator.Promises()) > 0 || s.ssh.teardowns.HasPending()
}

// Published SSH scopes, each a backend this app owns
func (s *Service) SSHConnectionCount() int {
	s.ssh.mu.Lock()
	defer s.ssh.mu.Unlock()
	return len(s.ssh.connections)
}

// Starts every scope teardown, then waits the grace and force closes what is still held
func (s *Service) TeardownSSHForQuit() {
	s.ssh.mu.Lock()
	scopes := make([]string, 0, len(s.ssh.connections))
	for scope := range s.ssh.connections {
		scopes = append(scopes, scope)
	}
	s.ssh.mu.Unlock()
	for _, scope := range scopes {
		run, err := s.beginSSHTeardown(scope)
		if err != nil {
			if s.log != nil {
				s.log("SSH teardown failed: " + err.Error())
			}
			continue
		}
		if run != nil {
			go run()
		}
	}
	s.ssh.teardowns.Finish(s.ssh.coordinator.Promises(), s.ssh.coordinator.ForceCleanupAll)
}

// Closes every keepalive socket when the app will quit
func (s *Service) StopSSHKeepalives() { s.ssh.keepalives.StopAll() }

// TeardownSSH drains the bootstrap of a scope and tears its SSH connection down inside the drain barrier
func (s *Service) TeardownSSH(scope string) error {
	return s.ssh.coordinator.CancelAndWait(scope, func() error { return s.teardownSSHConnection(scope) })
}

func (s *Service) desktopSSHBinary() string {
	return DesktopSSHBinary(s.platform, s.sshPathOverride)
}

// execFile text of a short probe with its Node error wording, stdin closed and the console hidden
func nodeExecText(command string, args []string, timeout time.Duration) (string, error) {
	cmd := exec.Command(command, args...)
	hideChildWindow(cmd)
	var stdout, stderr lockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", nodeSpawnError(command, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	killed := false
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		killed = true
		_ = terminateChild(cmd.Process)
		err = <-done
	}
	if err != nil {
		return "", fmt.Errorf("Command failed: %s\n%s", strings.Join(append([]string{command}, args...), " "), stderr.String())
	}
	if killed {
		return "", fmt.Errorf("%s timed out after %dms", command, timeout.Milliseconds())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// SHA256 of the effective ssh -G configuration, a local client failure tagged terminal
func (s *Service) effectiveSSHConfigFingerprint(cfg SSHConfig) (string, error) {
	ssh := s.desktopSSHBinary()
	args := []string{"-G"}
	if cfg.Port != 0 {
		args = append(args, "-p", fmt.Sprint(cfg.Port))
	}
	if cfg.KeyPath != "" {
		args = append(args, "-i", cfg.KeyPath)
	}
	args = append(args, "--", SSHTarget(cfg.User, cfg.Host))
	output, err := nodeExecText(ssh, args, 10*time.Second)
	if err != nil {
		return "", backend.SSHClientFailedError(ssh, err, s.platform)
	}
	sum := sha256.Sum256([]byte(output))
	return hex.EncodeToString(sum[:]), nil
}

// Joins or starts the SSH bootstrap of a scope under the coordinator, the fingerprint resolved by ssh -G when not given
func (s *Service) bootstrapSSHConnection(profile string, cfg SSHConfig, reuseToken, source, resolvedFingerprint string, metadata SSHBootstrapMetadata) (backend.Connection, error) {
	scope := ConnectionScopeKey(profile)
	effective := resolvedFingerprint
	if effective == "" {
		fp, err := s.effectiveSSHConfigFingerprint(cfg)
		if err != nil {
			return nil, err
		}
		effective = fp
	}
	fingerprint := SSHConfigFingerprint(scope, cfg, effective)
	return s.ssh.coordinator.Start(scope, fingerprint, func(lease *BootstrapLease) (backend.Connection, error) {
		return s.bootstrapSSHConnectionInner(profile, cfg, effective, reuseToken, source, metadata, fingerprint, lease)
	}, metadata).Wait()
}

// Exact terminates the serve of a bootstrap that lost the managed update fence, then drops its forward and transport
func (s *Service) rollbackSSHBootstrapResult(ssh *SSHConnection, result RemoteConnectResult, profile string, cfg SSHConfig, boundary error) error {
	var cleanup []string
	scope := ConnectionScopeKey(profile)
	expected := ManagedUpdateExpected{
		OwnershipID: result.OwnershipID, PID: result.PID, SpawnNonce: result.SpawnNonce,
		Profile:    ResolveRemoteSSHDashboardProfile(cfg.RemoteProfile, profile),
		HermesPath: result.HermesPath, HermesHome: result.HermesHome, StartedAt: result.StartedAt,
		CreationTimeNs: result.CreationTimeNs, CreationTime: result.CreationTime,
	}
	var err error
	switch result.Platform.OS {
	case "Windows":
		_, err = TerminateOwnedWindowsDashboardForUpdate(ssh, &WindowsRuntime{HermesPath: result.HermesPath, HermesHome: result.HermesHome, Executable: result.ExecutablePath}, expected)
	case "Linux", "Darwin":
		_, err = TerminateOwnedDashboardForUpdate(ssh, expected)
	default:
		name := result.Platform.OS
		if name == "" {
			name = "unknown"
		}
		cleanup = append(cleanup, "unsupported remote platform "+name)
	}
	if err != nil {
		cleanup = append(cleanup, err.Error())
	}
	if err := ssh.CancelForward(result.LocalPort, result.RemotePort); err != nil {
		cleanup = append(cleanup, err.Error())
	}
	if err := ssh.Close(); err != nil {
		cleanup = append(cleanup, err.Error())
	}
	s.ssh.mu.Lock()
	if current := s.ssh.connections[scope]; current != nil && current.SSH == ssh {
		delete(s.ssh.connections, scope)
		s.ssh.mu.Unlock()
		s.ssh.keepalives.Stop(scope)
	} else {
		s.ssh.mu.Unlock()
	}
	if len(cleanup) > 0 {
		return &managedFenceError{Message: "An SSH bootstrap crossed the managed-update gate and its exact owned serve could not be fenced: " + strings.Join(cleanup, "; "), Cause: boundary}
	}
	return nil
}

// Bootstrap error carrying the lifecycle kind as sshError, the way the boot failure screen reads it
func sshBootstrapError(err error) error {
	kind := errKind(err)
	if kind == "" {
		var k *backend.KindError
		if errors.As(err, &k) && error(k) == err {
			kind = k.Kind
		}
	}
	if kind == "" {
		kind = "unknown"
	}
	return &backend.KindError{SSHError: kind, Message: err.Error()}
}

// Opens or reuses the SSH transport of a scope, connects the remote lifecycle and publishes it behind the update fence
func (s *Service) bootstrapSSHConnectionInner(profile string, cfg SSHConfig, effective, reuseToken, source string, metadata SSHBootstrapMetadata, fingerprint string, lease *BootstrapLease) (backend.Connection, error) {
	scope := ConnectionScopeKey(profile)
	hostLabel := SSHTarget(cfg.User, cfg.Host)
	s.ssh.mu.Lock()
	existing := s.ssh.connections[scope]
	s.ssh.mu.Unlock()
	if existing != nil && existing.Fingerprint != fingerprint {
		if err := s.teardownSSHConnection(profile); err != nil {
			return nil, err
		}
	}
	s.ssh.mu.Lock()
	var ssh *SSHConnection
	if current := s.ssh.connections[scope]; current != nil {
		ssh = current.SSH
	}
	s.ssh.mu.Unlock()
	if ssh != nil {
		alive, err := ssh.IsAlive(nil)
		if err != nil {
			return nil, err
		}
		if !alive {
			_ = ssh.Close()
			ssh = nil
			s.ssh.keepalives.Stop(scope)
			s.ssh.mu.Lock()
			delete(s.ssh.connections, scope)
			s.ssh.mu.Unlock()
		}
	}
	created := ssh == nil
	removeForceCleanup := func() {}
	ownershipID, err := s.sshOwnershipKey(profile)
	if err != nil {
		return nil, err
	}
	if created {
		ssh, err = NewSSHConnection(SSHConnectionConfig{Host: cfg.Host, User: cfg.User, Port: cfg.Port, KeyPath: cfg.KeyPath}, SSHConnectionOptions{
			RememberLog: s.sshRememberLog, SSHBinary: s.desktopSSHBinary(), OwnershipID: ownershipID, Scope: scope, EffectiveConfigFingerprint: effective,
		})
		if err != nil {
			return nil, err
		}
		removeForceCleanup = lease.OnForceCleanup(ssh.Close)
		if err := ssh.Open(lease.Signal()); err != nil {
			return nil, err
		}
	}
	dashboardProfile := ResolveRemoteSSHDashboardProfile(cfg.RemoteProfile, profile)
	result, err := func() (RemoteConnectResult, error) {
		if metadata.RegistryConnectionID != "" {
			if err := s.ssh.gate.AssertCanDial(metadata.RegistryConnectionID, metadata.ManagedUpdateCorrelation); err != nil {
				return RemoteConnectResult{}, err
			}
		}
		platform, err := DetectRemotePlatform(ssh, cfg.RemoteHermesPath)
		if err != nil {
			return RemoteConnectResult{}, err
		}
		deps := RemoteConnectDeps{
			ForwardDeps:      ForwardDeps{Forward: ssh.Forward, CancelForward: ssh.CancelForward, PickLocalPort: PickLocalPort},
			SSH:              ssh,
			Platform:         &platform,
			Profile:          dashboardProfile,
			RemoteHermesPath: cfg.RemoteHermesPath,
			OwnershipID:      ownershipID,
			ReuseToken:       reuseToken,
			WaitForHermes: func(baseURL, token string) error {
				return s.waitForHermes(lease.Signal(), baseURL, &token, "token", nil, false)
			},
			ProbeReuseProof:  s.sshProbeReuseProof,
			AdoptServedToken: backend.AdoptServedDashboardToken,
			RememberLog:      s.sshRememberLog,
			DevServer:        os.Getenv("ICEBATH_DESKTOP_DEV_SERVER"),
			Signal:           lease.Signal(),
		}
		if platform.OS == "Windows" {
			return ConnectWindowsRemote(deps)
		}
		return RemoteConnect(deps)
	}()
	if err != nil {
		if created {
			_ = ssh.Close()
		} else {
			_ = s.teardownSSHConnection(profile)
		}
		return nil, sshBootstrapError(err)
	}
	if err := lease.AssertCurrent(); err != nil {
		if rollbackErr := s.rollbackSSHBootstrapResult(ssh, result, profile, cfg, err); rollbackErr != nil {
			return nil, rollbackErr
		}
		return nil, err
	}
	if err := FenceManagedSSHBootstrapPublication(func() error {
		if metadata.RegistryConnectionID != "" {
			return s.ssh.gate.AssertCanDial(metadata.RegistryConnectionID, metadata.ManagedUpdateCorrelation)
		}
		return nil
	}, func() {
		s.persistSSHConnectionToken(profile, source, result.Token, metadata.RegistryConnectionID)
		removeForceCleanup()
		owner := result.OwnershipID
		if owner == "" {
			owner = ownershipID
		}
		registryID := metadata.RegistryConnectionID
		if registryID == "" && strings.HasPrefix(source, "registry:") {
			registryID = source[len("registry:"):]
		}
		s.ssh.mu.Lock()
		s.ssh.connections[scope] = &SSHConnectionState{
			SSH: ssh, Fingerprint: fingerprint, OwnershipID: owner, LocalPort: result.LocalPort, RemotePort: result.RemotePort, PID: result.PID,
			Host: cfg.Host, HostLabel: hostLabel, HermesVersion: result.HermesVersion, RemotePlatform: result.Platform.OS, Reused: result.Reused,
			SpawnNonce: result.SpawnNonce, CreationTimeNs: result.CreationTimeNs, CreationTime: result.CreationTime, StartedAt: result.StartedAt,
			HermesPath: result.HermesPath, HermesHome: result.HermesHome, ExecutablePath: result.ExecutablePath, RemoteProfile: dashboardProfile,
			RegistryConnectionID: registryID, PrimaryRegistryScope: metadata.PrimaryRegistryScope, published: s.ssh.publishRank(scope),
		}
		s.ssh.mu.Unlock()
		s.ssh.keepalives.Start(scope, result.BaseURL, result.Token)
	}, func(cause error) error {
		return s.rollbackSSHBootstrapResult(ssh, result, profile, cfg, cause)
	}); err != nil {
		return nil, err
	}
	mode, version, path := "spawned", result.HermesVersion, result.HermesPath
	if result.Reused {
		mode = "REUSED"
	}
	if version == "" {
		version = "icebath (version unknown)"
	}
	if path == "" {
		path = "?"
	}
	s.sshRememberLog("[ssh] connection " + mode + " dashboard: " + version + " at " + path)
	connection, err := s.buildRemoteConnection(context.Background(), result.BaseURL, "token", result.Token, source, hostLabel, "ssh", result.OwnershipID, nil)
	if err != nil {
		return nil, err
	}
	out := connection.Clone()
	out["remoteHermesVersion"] = result.HermesVersion
	sshInfo := map[string]any{"effectiveConfigFingerprint": effective, "host": cfg.Host}
	for k, v := range map[string]string{"keyPath": cfg.KeyPath, "remoteHermesPath": cfg.RemoteHermesPath, "remoteProfile": cfg.RemoteProfile, "user": cfg.User} {
		if v != "" {
			sshInfo[k] = v
		}
	}
	if cfg.Port != 0 {
		sshInfo["port"] = cfg.Port
	}
	out["ssh"] = sshInfo
	return out, nil
}

// Mirrors an adopted per serve token into the registry entry and the legacy store the source names
func (s *Service) persistSSHConnectionToken(profile, source, token, registryConnectionID string) {
	persistence := ManagedSSHTokenPersistencePlan(source, registryConnectionID)
	encrypted, err := s.encryptDesktopSecret(token, false)
	if err != nil {
		s.sshRememberLog("[ssh] could not persist served token: " + err.Error())
		return
	}
	if err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if id := persistence.RegistryConnectionID; id != "" {
			registry, err := s.readDesktopConnectionsRegistry()
			if err != nil {
				return err
			}
			for _, entry := range registry.Connections {
				if entry.ID == id && entry.Kind == "ssh" {
					entry.Token = encrypted
					if err := s.writeDesktopConnectionsRegistry(UpsertConnection(registry, entry)); err != nil {
						return err
					}
					break
				}
			}
		}
		if persistence.LegacySource == "" {
			return nil
		}
		config := s.readDesktopConnectionConfig().clone()
		if persistence.LegacySource == "profile" {
			key := ConnectionScopeKey(profile)
			entry := jsObject(config.Profiles[key])
			if key != "" && entry != nil && entry["mode"] == "ssh" {
				updated := jsSpread(entry, map[string]any{"token": toJSONValue(encrypted)})
				config.Profiles[key] = updated
				return s.writeDesktopConnectionConfig(config)
			}
			return nil
		}
		if remote := jsObject(config.Remote); config.Mode == "ssh" && jsTruthy(config.Remote) && remote != nil {
			config.Remote = jsSpread(remote, map[string]any{"token": toJSONValue(encrypted)})
			return s.writeDesktopConnectionConfig(config)
		}
		return nil
	}(); err != nil {
		s.sshRememberLog("[ssh] could not persist served token: " + err.Error())
	}
}

// Whether a profile has its own remote or SSH override in connection.json
func (s *Service) profileHasRemoteOverride(profile string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ProfileHasRemoteConnection(s.readDesktopConnectionConfig().object(), profile)
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// EnsureRegistryBackend dials the backend of a registry connection and profile
func (s *Service) EnsureRegistryBackend(connectionID, profile string) (backend.Connection, error) {
	return s.ensureRegistryBackend(connectionID, profile, "", false)
}

func primaryEnsure(b *backend.Service, passive bool) func(profile string) (map[string]any, error) {
	return func(profile string) (map[string]any, error) {
		conn, err := b.EnsureBackend(profile, backend.EnsureOptions{Passive: passive})
		return map[string]any(conn), err
	}
}

// Registry dial reusing the matching primary, the shared SSH default backend or the pool, else connecting the source
func (s *Service) ensureRegistryBackend(connectionID, profile, managedUpdateCorrelation string, passive bool) (backend.Connection, error) {
	b := s.backendService()
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return nil, err
	}
	id, err := RegistryDialConnectionID(connectionID)
	if err != nil {
		return nil, err
	}
	var source *RegistryConnection
	for i := range registry.Connections {
		if registry.Connections[i].ID == id {
			source = &registry.Connections[i]
			break
		}
	}
	if source == nil {
		return nil, fmt.Errorf("No connection with id \"%s\".", id)
	}
	if source.Kind == "ssh" {
		if err := s.ssh.gate.AssertCanDial(id, managedUpdateCorrelation); err != nil {
			return nil, err
		}
	}
	profileKey := strings.TrimSpace(profile)
	if profileKey == "" {
		profileKey = "default"
	}
	var cfgOnce, fpOnce sync.Once
	var sshCfg *SSHConfig
	var fp string
	var fpErr error
	resolveSSHConfig := func() *SSHConfig {
		if source.Kind != "ssh" {
			return nil
		}
		cfgOnce.Do(func() { sshCfg = ManagedSSHConfig(*source, profileKey) })
		return sshCfg
	}
	resolveFingerprint := func() (string, error) {
		fpOnce.Do(func() {
			cfg := resolveSSHConfig()
			if cfg == nil {
				fpErr = fmt.Errorf("SSH connection \"%s\" has no host configured.", source.Label)
				return
			}
			fp, fpErr = s.effectiveSSHConfigFingerprint(*cfg)
		})
		return fp, fpErr
	}
	primary, err := ReuseMatchingPrimarySSHBackend(ReuseMatchingPrimarySSHBackendOptions{
		ConnectionID:         id,
		EffectiveFingerprint: func(RegistryConnection) (string, error) { return resolveFingerprint() },
		EnsurePrimary:        func() (map[string]any, error) { return primaryEnsure(b, passive)(profile) },
		Profile:              profile,
		Registry:             registry,
		Source:               *source,
	})
	if err != nil {
		return nil, err
	}
	if primary != nil {
		return backend.Connection(jsSpread(primary, map[string]any{"profile": profileKey, "connectionId": id})), nil
	}
	sharedPrimary, err := ReuseMatchingPrimaryRemoteBackend(id, primaryEnsure(b, false), profile, registry, *source)
	if err != nil {
		return nil, err
	}
	if sharedPrimary != nil {
		return backend.Connection(sharedPrimary), nil
	}
	if source.Kind == "local" {
		return s.ensureRegistryLocalBackend(b, profile, profileKey, passive)
	}
	key := backend.BackendScopeKey(id, profile)
	existing := b.PoolConnection(key)
	if existing == nil {
		shared, err := ResolveSharedSSHProfileBackend(*source, profile, func() (backend.Connection, error) {
			return s.ensureRegistryBackend(id, "default", managedUpdateCorrelation, passive)
		}, func(descriptor backend.Connection) (any, error) {
			return s.requestDescriptorJSON(context.Background(), descriptor, "/api/profiles", JSONOptions{TimeoutMs: 8_000})
		})
		if err != nil {
			return nil, err
		}
		if shared != nil {
			s.SetBridgeProfileState(jsOrString(shared["profile"]), false)
			return shared, nil
		}
	} else {
		return s.ssh.dispatch.Run(existing, func() (backend.Connection, error) {
			return EnsureHealthyPooledRemoteBackendForDispatch(DispatchHealthOptions{
				Connection: existing,
				Current:    func() *backend.Future[backend.Connection] { return b.PoolConnection(key) },
				Probe: func(conn backend.Connection, path string, timeout time.Duration) error {
					_, err := s.requestDescriptorJSON(context.Background(), conn, path, JSONOptions{TimeoutMs: float64(timeout.Milliseconds())})
					return err
				},
				Reconnect: func() (backend.Connection, error) { return s.ensureRegistryBackend(id, profile, "", passive) },
				Retire: func(cause error) error {
					if b.PoolConnection(key) != existing {
						return nil
					}
					if s.log != nil {
						s.log(fmt.Sprintf("Pooled remote backend \"%s\" failed its dispatch probe (%s); reconnecting on demand.", key, cause.Error()))
					}
					if err := b.StopPoolBackend(key); err != nil {
						return err
					}
					if source.Kind == "ssh" {
						return s.ssh.coordinator.CancelAndWait(key, func() error { return s.teardownSSHConnection(key) })
					}
					return nil
				},
			})
		})
	}
	if err := backend.AssertNotPassiveSpawn(passive, key); err != nil {
		return nil, err
	}
	src := *source
	cfg := resolveSSHConfig()
	var fingerprint func() (string, error)
	if src.Kind == "ssh" {
		fingerprint = resolveFingerprint
	}
	return b.StartPoolEntry(key, func(slot backend.PoolSlot) (backend.Connection, error) {
		return s.connectRegistryBackend(src, profile, key, slot, cfg, fingerprint, managedUpdateCorrelation, "")
	}).Wait()
}

// The registry local entry always means this machine, delegating to the primary path only when that route is local
func (s *Service) ensureRegistryLocalBackend(b *backend.Service, profile, profileKey string, passive bool) (backend.Connection, error) {
	if err := s.AssertCanStart(profileKey); err != nil {
		return nil, err
	}
	rawProfile := strings.TrimSpace(profile)
	opts := LocalRouteOptions{GlobalRemote: s.globalRemoteActive(), ProfileRemoteOverride: s.profileHasRemoteOverride(profileKey)}
	if rawProfile != "" {
		exists := directoryExists(filepath.Join(s.icebathHome, "profiles", strings.ToLower(rawProfile)))
		opts.LocalProfileExists = &exists
	}
	route := ResolveRegistryLocalRoute(rawProfile, opts)
	if route.Refuse != "" {
		return nil, errors.New(route.Refuse)
	}
	if route.Delegate {
		return b.EnsureBackend(profile, backend.EnsureOptions{Passive: passive})
	}
	if existing := b.PoolConnection(route.PoolKey); existing != nil {
		return existing.Wait()
	}
	if err := backend.AssertNotPassiveSpawn(passive, route.PoolKey); err != nil {
		return nil, err
	}
	return b.StartForcedLocalPoolEntry(profileKey, route.PoolKey).Wait()
}

// Dials a non local registry connection for one profile, SSH through its own scope and remote or cloud by URL
func (s *Service) connectRegistryBackend(source RegistryConnection, profile, key string, slot backend.PoolSlot, cfg *SSHConfig, fingerprint func() (string, error), managedUpdateCorrelation, tokenPersistenceSource string) (backend.Connection, error) {
	b := s.backendService()
	profileKey := strings.TrimSpace(profile)
	if profileKey == "" {
		profileKey = "default"
	}
	if source.Kind == "ssh" {
		if cfg == nil {
			return nil, fmt.Errorf("SSH connection \"%s\" has no host configured.", source.Label)
		}
		effective := ""
		if fingerprint != nil {
			fp, err := fingerprint()
			if err != nil {
				return nil, err
			}
			effective = fp
		}
		label := tokenPersistenceSource
		if label == "" {
			label = "registry:" + source.ID
		}
		s.mu.Lock()
		token := s.decryptDesktopSecret(source.Token)
		s.mu.Unlock()
		connection, err := s.bootstrapSSHConnection(key, *cfg, token, label, effective, SSHBootstrapMetadata{
			ManagedScope: "pool", ManagedUpdateCorrelation: managedUpdateCorrelation, PoolKey: key, RegistryConnectionID: source.ID,
		})
		if err != nil {
			return nil, err
		}
		slot.SetRemoteBaseURL(jsOrString(connection["baseUrl"]))
		s.SetBridgeProfileState(profileKey, false)
		out := connection.Clone()
		out["profile"], out["connectionId"], out["remoteProfile"], out["logs"] = profileKey, source.ID, cfg.RemoteProfile, b.RecentLogs()
		return backend.OverlayWindowState(out, b.MainWindowState()), nil
	}
	token := ""
	if source.AuthMode != "oauth" {
		s.mu.Lock()
		token = s.decryptDesktopSecret(source.Token)
		s.mu.Unlock()
	}
	remoteKind := "url"
	if source.Kind == "cloud" {
		remoteKind = "cloud"
	}
	connection, err := s.buildRemoteConnection(context.Background(), source.URL, NormAuthMode(source.AuthMode), token, "registry:"+source.ID, "", remoteKind, nil, source.Headers)
	if err != nil {
		return nil, err
	}
	if err := s.waitForRemoteHermes(context.Background(), connection); err != nil {
		return nil, err
	}
	slot.SetRemoteBaseURL(jsOrString(connection["baseUrl"]))
	s.SetBridgeProfileState(profileKey, false)
	out := connection.Clone()
	out["profile"], out["connectionId"], out["sharedRemote"], out["logs"] = profileKey, source.ID, true, b.RecentLogs()
	return backend.OverlayWindowState(out, b.MainWindowState()), nil
}

// Stops every pooled backend and SSH scope a removed registry connection owned
func (s *Service) stopRegistryConnectionBackends(connectionID string) error {
	b := s.backendService()
	prefix := backend.BackendScopePrefix(connectionID)
	for _, key := range b.PoolKeys() {
		if strings.HasPrefix(key, prefix) {
			_ = b.StopPoolBackend(key)
		}
	}
	scopes := map[string]bool{}
	s.ssh.mu.Lock()
	for scope := range s.ssh.connections {
		if strings.HasPrefix(scope, prefix) {
			scopes[scope] = true
		}
	}
	s.ssh.mu.Unlock()
	for _, e := range s.ssh.coordinator.Active() {
		if strings.HasPrefix(e.Scope, prefix) {
			scopes[e.Scope] = true
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(scopes))
	for scope := range scopes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ssh.coordinator.CancelAndWait(scope, func() error { return s.teardownSSHConnection(scope) }); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}

// GetConnectionFor dials the registry connection and profile a window asks for
func (s *Service) GetConnectionFor(windowID string, payload any) (backend.Connection, error) {
	obj := jsObject(toJSONValue(payload))
	if _, err := s.lockedConnectionsRegistry(); err != nil {
		return nil, err
	}
	id, err := RegistryDialConnectionID(obj["connectionId"])
	if err != nil {
		return nil, err
	}
	profile := strings.TrimSpace(jsNullishString(obj["profile"]))
	if profile == "" {
		profile = "default"
	}
	return s.backendService().ConnectProfileRoute(windowID, id, profile)
}

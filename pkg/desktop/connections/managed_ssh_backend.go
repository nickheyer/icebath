package connections

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Managed update or recovery in flight for one registry connection
type managedOperation struct {
	done   chan struct{}
	result ManagedConnectionUpdateResult
}

// CapturedSSHScope is one SSH scope a managed update drains and restores
type CapturedSSHScope struct {
	Drained            bool
	Entry              *backend.Future[backend.Connection]
	ForwardRestored    bool
	Key                string
	Primary            bool
	Profile            string
	RegistryScoped     bool
	ReuseToken         string
	State              *SSHConnectionState
	UnsafeDrainFailure bool
}

// Profile the scope restores
func (c *CapturedSSHScope) ScopeProfile() string { return c.Profile }

// RFC 4122 version 4 identifier the way crypto.randomUUID writes it
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Every pool and primary scope of a registry SSH connection with the token it was served
func (s *Service) captureManagedSSHScopes(source RegistryConnection) ([]*CapturedSSHScope, error) {
	b := s.backendService()
	prefix := backend.BackendScopePrefix(source.ID)
	s.mu.Lock()
	config := s.readDesktopConnectionConfig().object()
	registry, err := s.readDesktopConnectionsRegistry()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	routeForProfile := func(profile string) (*DesktopRemoteRoute, error) {
		return ResolveDesktopRemoteRoute(DesktopRemoteRouteInput{Config: config, EnvToken: os.Getenv("ICEBATH_DESKTOP_REMOTE_TOKEN"), EnvURL: os.Getenv("ICEBATH_DESKTOP_REMOTE_URL"), Profile: profile, Registry: registry})
	}
	stateOf := func(key string) *SSHConnectionState {
		s.ssh.mu.Lock()
		defer s.ssh.mu.Unlock()
		return s.ssh.connections[key]
	}
	var pooled []*CapturedSSHScope
	pooledKeys := map[string]bool{}
	for _, key := range b.PoolKeys() {
		route, err := routeForProfile(key)
		if err != nil {
			return nil, err
		}
		routeID := ""
		if route != nil && route.Kind == "ssh" {
			routeID = route.ConnectionID
		}
		if ManagedSSHScopeRole(source.ID, key, prefix, routeID, stateOf(key)) != "pool" {
			continue
		}
		profile := key
		if strings.HasPrefix(key, prefix) {
			profile = key[len(prefix):]
			if profile == "" {
				profile = "default"
			}
		}
		pooled = append(pooled, &CapturedSSHScope{Entry: b.PoolConnection(key), Key: key, Profile: profile, RegistryScoped: strings.HasPrefix(key, prefix)})
		pooledKeys[key] = true
	}
	primaryProfile := b.PrimaryProfileKey()
	if primaryProfile == "" {
		primaryProfile = "default"
	}
	var primary []*CapturedSSHScope
	s.ssh.mu.Lock()
	for key, state := range s.ssh.connections {
		if !pooledKeys[key] && ManagedSSHScopeRole(source.ID, key, prefix, "", state) == "primary" {
			primary = append(primary, &CapturedSSHScope{Entry: b.PrimaryConnection(), Key: key, Primary: true, Profile: primaryProfile, State: state})
		}
	}
	s.ssh.mu.Unlock()
	primaryFuture := b.PrimaryConnection()
	primaryRoute, err := routeForProfile(primaryProfile)
	if err != nil {
		return nil, err
	}
	if len(primary) == 0 && primaryFuture != nil && primaryRoute != nil && primaryRoute.Kind == "ssh" && primaryRoute.ConnectionID == source.ID {
		key := ConnectionScopeKey("")
		if primaryRoute.Source == "profile" {
			key = ConnectionScopeKey(primaryProfile)
		}
		primary = append(primary, &CapturedSSHScope{Entry: primaryFuture, Key: key, Primary: true, Profile: primaryProfile})
	}
	if len(primary) > 1 {
		return nil, errors.New("Managed SSH update found multiple primary scopes; refusing an ambiguous drain.")
	}
	captured := append(pooled, primary...)
	if err := WaitForManagedSSHBootstrapFence(s.ssh.coordinator.Active(), source.ID); err != nil {
		return nil, err
	}
	for _, scope := range captured {
		if scope.Entry != nil {
			descriptor, err := scope.Entry.Wait()
			if err != nil {
				if _, unsafe := err.(*managedFenceError); unsafe {
					return nil, err
				}
			} else {
				scope.ReuseToken = jsOrString(descriptor["token"])
			}
		}
		scope.State = stateOf(scope.Key)
	}
	return captured, nil
}

// Update target of a published scope, refusing one without a complete runtime identity
func remoteUpdateTargetFromState(state *SSHConnectionState) (RemoteUpdateTarget, error) {
	if state == nil || state.SSH == nil || state.HermesPath == "" || state.HermesHome == "" {
		return RemoteUpdateTarget{}, errors.New("The managed SSH scope does not carry a complete remote runtime identity.")
	}
	switch state.RemotePlatform {
	case "Darwin", "Linux", "Windows":
	default:
		name := state.RemotePlatform
		if name == "" {
			name = "unknown"
		}
		return RemoteUpdateTarget{}, fmt.Errorf("Unsupported managed SSH update platform: %s.", name)
	}
	return RemoteUpdateTarget{SSH: state.SSH, Platform: state.RemotePlatform, HermesPath: state.HermesPath, HermesHome: state.HermesHome, ExecutablePath: state.ExecutablePath}, nil
}

// Opens a per call SSH transport to the connection install when no published scope can carry the update
func (s *Service) openManagedSSHUpdateTransport(source RegistryConnection) (*SSHConnection, RemoteUpdateTarget, error) {
	cfg := ManagedSSHConfig(source, "")
	if cfg == nil {
		return nil, RemoteUpdateTarget{}, fmt.Errorf("SSH connection \"%s\" has no host configured.", source.Label)
	}
	ssh, err := NewSSHProbeConnection(SSHConnectionConfig{Host: cfg.Host, User: cfg.User, Port: cfg.Port, KeyPath: cfg.KeyPath}, SSHConnectionOptions{RememberLog: s.sshRememberLog, SSHBinary: s.desktopSSHBinary()})
	if err != nil {
		return nil, RemoteUpdateTarget{}, err
	}
	if err := ssh.Open(nil); err != nil {
		return nil, RemoteUpdateTarget{}, err
	}
	target, err := func() (RemoteUpdateTarget, error) {
		platform, err := DetectRemotePlatform(ssh, cfg.RemoteHermesPath)
		if err != nil {
			return RemoteUpdateTarget{}, err
		}
		if platform.OS == "Windows" {
			runtime := platform.Windows
			if runtime == nil || runtime.HermesPath == "" {
				if runtime, err = probeWindowsRemote(ssh, cfg.RemoteHermesPath); err != nil {
					return RemoteUpdateTarget{}, err
				}
			}
			return RemoteUpdateTarget{SSH: ssh, Platform: "Windows", HermesPath: runtime.HermesPath, HermesHome: runtime.HermesHome, ExecutablePath: runtime.Executable}, nil
		}
		hermesPath, err := LocateHermes(ssh, cfg.RemoteHermesPath)
		if err != nil {
			return RemoteUpdateTarget{}, err
		}
		hermesHome, err := ProbeRemoteHermesHome(ssh)
		if err != nil {
			return RemoteUpdateTarget{}, err
		}
		return RemoteUpdateTarget{SSH: ssh, Platform: platform.OS, HermesPath: hermesPath, HermesHome: hermesHome}, nil
	}()
	if err != nil {
		_ = ssh.Close()
		return nil, RemoteUpdateTarget{}, err
	}
	return ssh, target, nil
}

// Drops the forward and exact terminates the owned serve of a scope, restoring the original forward on a refusal
func (s *Service) drainManagedSSHScope(scope *CapturedSSHScope) (err error) {
	state := scope.State
	forwardClosed := false
	defer func() {
		if scope.UnsafeDrainFailure {
			return
		}
		scope.Drained = true
		b := s.backendService()
		if scope.Primary {
			b.InvalidatePrimary()
		} else if b.PoolConnection(scope.Key) == scope.Entry {
			_ = b.StopPoolBackend(scope.Key)
		}
		s.ssh.mu.Lock()
		published := state != nil && s.ssh.connections[scope.Key] == state
		if published {
			delete(s.ssh.connections, scope.Key)
		}
		s.ssh.mu.Unlock()
		if published {
			s.ssh.keepalives.Stop(scope.Key)
		}
	}()
	drainErr := func() error {
		if state == nil {
			return nil
		}
		s.ssh.mu.Lock()
		terminals := s.ssh.terminals
		s.ssh.mu.Unlock()
		if terminals != nil {
			terminals.DisposeTerminalSessionsForSSHScope(scope.Key)
		}
		if state.LocalPort != 0 && state.RemotePort != 0 {
			if err := state.SSH.CancelForward(state.LocalPort, state.RemotePort); err != nil {
				return err
			}
			forwardClosed = true
		}
		expected := ManagedUpdateExpected{OwnershipID: state.OwnershipID, PID: state.PID, SpawnNonce: state.SpawnNonce, Profile: state.RemoteProfile,
			HermesPath: state.HermesPath, HermesHome: state.HermesHome, StartedAt: state.StartedAt, CreationTimeNs: state.CreationTimeNs, CreationTime: state.CreationTime}
		switch state.RemotePlatform {
		case "Windows":
			_, err := TerminateOwnedWindowsDashboardForUpdate(state.SSH, &WindowsRuntime{HermesPath: state.HermesPath, HermesHome: state.HermesHome, Executable: state.ExecutablePath}, expected)
			return err
		case "Linux", "Darwin":
			_, err := TerminateOwnedDashboardForUpdate(state.SSH, expected)
			return err
		}
		name := state.RemotePlatform
		if name == "" {
			name = "unknown"
		}
		return fmt.Errorf("Unsupported managed SSH update platform: %s.", name)
	}()
	if drainErr == nil {
		return nil
	}
	scope.UnsafeDrainFailure = true
	if state != nil && state.LocalPort != 0 && state.RemotePort != 0 && scope.ReuseToken != "" {
		url := fmt.Sprintf("http://127.0.0.1:%d", state.LocalPort)
		token := scope.ReuseToken
		restoreErr := func() error {
			if forwardClosed {
				if err := state.SSH.Forward(state.LocalPort, state.RemotePort); err != nil {
					return err
				}
			}
			return s.waitForHermes(context.Background(), url, &token, "token", nil, false)
		}()
		if restoreErr == nil {
			scope.ForwardRestored = true
		} else {
			drainErr = &CodedError{Message: drainErr.Error() + " The original forward also failed to recover: " + restoreErr.Error(), Code: errKind(drainErr), Cause: drainErr}
		}
	}
	return drainErr
}

// Restores one scope under the still held gate from the connection snapshot taken before the drain
func (s *Service) ensureManagedSSHBackendAtKey(source RegistryConnection, profile, key, correlationID, tokenPersistenceSource string) (backend.Connection, error) {
	if err := s.ssh.gate.AssertCanDial(source.ID, correlationID); err != nil {
		return nil, err
	}
	b := s.backendService()
	if existing := b.PoolConnection(key); existing != nil {
		return existing.Wait()
	}
	cfg := ManagedSSHConfig(source, profile)
	return b.StartPoolEntry(key, func(slot backend.PoolSlot) (backend.Connection, error) {
		return s.connectRegistryBackend(source, profile, key, slot, cfg, nil, correlationID, tokenPersistenceSource)
	}).Wait()
}

func (s *Service) ensureManagedSSHBackend(source RegistryConnection, profile, correlationID string) (backend.Connection, error) {
	return s.ensureManagedSSHBackendAtKey(source, profile, backend.BackendScopeKey(source.ID, profile), correlationID, "")
}

// Boots the primary again under the gate, one connection owning the primary restore at a time
func (s *Service) restoreManagedPrimarySSHBackend(source RegistryConnection, profile, correlationID string) (backend.Connection, error) {
	if err := s.ssh.gate.AssertCanDial(source.ID, correlationID); err != nil {
		return nil, err
	}
	profileKey := strings.TrimSpace(profile)
	if profileKey == "" {
		profileKey = "default"
	}
	s.cloud.mu.Lock()
	if _, own := s.cloud.managedPrimaryRestores[source.ID]; len(s.cloud.managedPrimaryRestores) > 0 && !own {
		s.cloud.mu.Unlock()
		return nil, errors.New("Another managed SSH primary restore is already in progress.")
	}
	s.cloud.managedPrimaryRestores[source.ID] = ManagedPrimaryRestore{CorrelationID: correlationID, Profile: profileKey, Source: source}
	s.cloud.mu.Unlock()
	defer func() {
		s.cloud.mu.Lock()
		if r, ok := s.cloud.managedPrimaryRestores[source.ID]; ok && r.CorrelationID == correlationID {
			delete(s.cloud.managedPrimaryRestores, source.ID)
		}
		s.cloud.mu.Unlock()
	}()
	return s.backendService().RestartPrimary()
}

// One managed update transaction of a registry SSH connection over its captured scopes
func (s *Service) updateManagedSSHConnection(source RegistryConnection, correlationID string) (ManagedConnectionUpdateResult, error) {
	snapshot := source
	scopes, err := s.captureManagedSSHScopes(snapshot)
	if err != nil {
		return ManagedConnectionUpdateResult{}, err
	}
	if reason, message := ManagedSSHDrainBlocker(scopes); reason != "" {
		return RefusedManagedSSHUpdate(source.ID, correlationID, message, reason), nil
	}
	var ephemeral *SSHConnection
	launchAttempted := false
	var target RemoteUpdateTarget
	var firstState *SSHConnectionState
	for _, scope := range scopes {
		if scope.State != nil {
			firstState = scope.State
			break
		}
	}
	if firstState != nil {
		if target, err = remoteUpdateTargetFromState(firstState); err != nil {
			return ManagedConnectionUpdateResult{}, err
		}
	} else if ephemeral, target, err = s.openManagedSSHUpdateTransport(snapshot); err != nil {
		return ManagedConnectionUpdateResult{}, err
	}
	inputs := make([]ManagedSSHScopeInput, len(scopes))
	for i, scope := range scopes {
		inputs[i] = ManagedSSHScopeInput{Key: scope.Key, Primary: scope.Primary, Profile: scope.Profile}
	}
	return RunManagedSSHUpdate(ManagedUpdateDeps[*CapturedSSHScope]{
		ConnectionID:    source.ID,
		CorrelationID:   correlationID,
		Scopes:          scopes,
		PreflightRemote: func() error { return AssertManagedUpdatePreflightClear(target, correlationID) },
		DrainScope:      s.drainManagedSSHScope,
		UpdateRemote: func() (RemoteUpdateProof, error) {
			return ExecuteManagedRemoteUpdate(target, correlationID, func() error {
				if err := s.markManagedSSHRecoveryLaunching(source.ID, correlationID); err != nil {
					return err
				}
				launchAttempted = true
				return nil
			})
		},
		AwaitRestoreClearance: func() error {
			_, err := WaitForManagedRemoteClearance(target, correlationID, launchAttempted)
			return err
		},
		CloseTransports: func() error {
			transports := map[*SSHConnection]bool{}
			for _, scope := range scopes {
				if scope.Drained && scope.State != nil && scope.State.SSH != nil {
					transports[scope.State.SSH] = true
				}
			}
			var wg sync.WaitGroup
			for ssh := range transports {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = ssh.Close()
				}()
			}
			wg.Wait()
			if ephemeral != nil {
				return ephemeral.Close()
			}
			return nil
		},
		RestoreScope: func(scope *CapturedSSHScope) error {
			if scope.UnsafeDrainFailure {
				if !scope.ForwardRestored {
					return fmt.Errorf("The original %s SSH forward could not be restored safely.", scope.Profile)
				}
				if scope.Entry == nil {
					return nil
				}
				_, err := scope.Entry.Wait()
				return err
			}
			scoped := snapshot
			if scope.ReuseToken != "" {
				token, err := s.encryptDesktopSecret(scope.ReuseToken, false)
				if err != nil {
					return err
				}
				scoped.Token = token
			}
			var err error
			switch {
			case scope.Primary:
				_, err = s.restoreManagedPrimarySSHBackend(scoped, scope.Profile, correlationID)
			case scope.RegistryScoped:
				_, err = s.ensureManagedSSHBackend(scoped, scope.Profile, correlationID)
			default:
				_, err = s.ensureManagedSSHBackendAtKey(scoped, scope.Profile, scope.Key, correlationID, "profile")
			}
			return err
		},
		PrepareRecovery:  func() error { return s.persistManagedSSHRecovery(snapshot, correlationID, inputs) },
		CompleteRecovery: func() error { return s.clearManagedSSHRecovery(source.ID, correlationID) },
		ReleaseGate:      func() { s.ssh.gate.Release(source.ID, correlationID) },
	}), nil
}

// Registry connection of a journaled source snapshot
func recordSource(record ManagedSSHRecoveryRecord) (RegistryConnection, error) {
	var source RegistryConnection
	text, err := json.Marshal(record.Source)
	if err != nil {
		return source, err
	}
	err = json.Unmarshal(text, &source)
	return source, err
}

// Retries a crash interrupted managed update from its journal once the remote marker is clear
func (s *Service) recoverManagedSSHUpdate(record ManagedSSHRecoveryRecord) {
	connectionID := record.ConnectionID
	s.ssh.mu.Lock()
	_, recovering := s.ssh.recoveries[connectionID]
	_, updating := s.ssh.updates[connectionID]
	s.ssh.mu.Unlock()
	if recovering || updating {
		return
	}
	correlation := record.CorrelationID
	claimed, err := s.ssh.gate.Claim(connectionID, correlation)
	if err != nil || !claimed {
		return
	}
	op := &managedOperation{done: make(chan struct{})}
	s.ssh.mu.Lock()
	s.ssh.recoveries[connectionID] = op
	s.ssh.mu.Unlock()
	defer func() {
		s.ssh.gate.Release(connectionID, correlation)
		s.ssh.mu.Lock()
		delete(s.ssh.recoveries, connectionID)
		s.ssh.mu.Unlock()
		close(op.done)
	}()
	var transport *SSHConnection
	defer func() {
		if transport != nil {
			_ = transport.Close()
		}
	}()
	err = func() error {
		source, err := recordSource(record)
		if err != nil {
			return err
		}
		ssh, target, err := s.openManagedSSHUpdateTransport(source)
		if err != nil {
			return err
		}
		transport = ssh
		attempts := 0
		if record.Attempts != nil {
			attempts = *record.Attempts
		}
		total, disposition, results, err := RecoverManagedSSHScopes(record.Scopes, attempts,
			func() (RemoteUpdateObservation, error) {
				return WaitForManagedRemoteClearance(target, correlation, record.Phase == "launching")
			},
			func() error {
				closeErr := transport.Close()
				transport = nil
				return closeErr
			},
			func(scope ManagedSSHRecoveryScope) error {
				var err error
				switch scope.Kind {
				case "primary":
					_, err = s.restoreManagedPrimarySSHBackend(source, scope.Profile, correlation)
				case "legacy":
					_, err = s.ensureManagedSSHBackendAtKey(source, scope.Profile, scope.Key, correlation, "profile")
				default:
					_, err = s.ensureManagedSSHBackend(source, scope.Profile, correlation)
				}
				return err
			},
			func() error { return s.clearManagedSSHRecovery(connectionID, correlation) },
			func(next int) error { return s.recordManagedSSHRecoveryAttempt(connectionID, correlation, next) })
		if err != nil {
			return err
		}
		var unrestored []string
		for i, scope := range record.Scopes {
			if i < len(results) && results[i] != nil {
				unrestored = append(unrestored, scope.Profile)
			}
		}
		switch disposition {
		case "complete":
			s.sshRememberLog(fmt.Sprintf("[ssh-update] restored %d scope(s) from durable recovery for %s", len(record.Scopes), connectionID))
		case "abandon":
			s.sshRememberLog(fmt.Sprintf("[ssh-update] durable recovery for %s stopped after %d attempt(s); connection released with %d scope(s) not restored (%s). Reconnecting starts them again.",
				connectionID, total, len(unrestored), strings.Join(unrestored, ", ")))
		default:
			s.sshRememberLog(fmt.Sprintf("[ssh-update] durable recovery for %s left %d scope(s) pending (attempt %d of %d); will retry next launch",
				connectionID, len(unrestored), total, MaxManagedSSHRecoveryAttempts))
		}
		return nil
	}()
	if err != nil {
		s.sshRememberLog(fmt.Sprintf("[ssh-update] durable recovery for %s remains pending: %s", connectionID, err.Error()))
	}
}

// Resumes every journaled managed update at launch, all at once, an unreadable journal returned
func (s *Service) ResumeManagedSSHRecoveries() error {
	records, err := s.readManagedSSHRecoveryRecords()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, record := range records {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.recoverManagedSSHUpdate(record)
		}()
	}
	wg.Wait()
	return nil
}

// ConnectionsUpdateManaged runs or joins the managed update of a registry SSH connection
func (s *Service) ConnectionsUpdateManaged(rawID any) (ManagedConnectionUpdateResult, error) {
	connectionID := strings.TrimSpace(jsOrString(rawID))
	s.ssh.mu.Lock()
	existing := s.ssh.updates[connectionID]
	s.ssh.mu.Unlock()
	if existing != nil {
		<-existing.done
		return existing.result, nil
	}
	correlationID := randomUUID()
	registry, err := s.lockedConnectionsRegistry()
	if err != nil {
		return ManagedConnectionUpdateResult{}, err
	}
	var source *RegistryConnection
	for i := range registry.Connections {
		if registry.Connections[i].ID == connectionID {
			source = &registry.Connections[i]
			break
		}
	}
	if source == nil {
		return RefusedManagedSSHUpdate(connectionID, correlationID, fmt.Sprintf("No connection with id \"%s\".", connectionID), ""), nil
	}
	if source.Kind != "ssh" {
		return RefusedManagedSSHUpdate(connectionID, correlationID, "Only registered Desktop-managed SSH connections can use this update lifecycle.", ""), nil
	}
	claimed, err := s.ssh.gate.Claim(connectionID, correlationID)
	if err != nil {
		return ManagedConnectionUpdateResult{}, err
	}
	if !claimed {
		return RefusedManagedSSHUpdate(connectionID, correlationID, "A managed update is already in progress.", ""), nil
	}
	op := &managedOperation{done: make(chan struct{})}
	s.ssh.mu.Lock()
	s.ssh.updates[connectionID] = op
	s.ssh.mu.Unlock()
	src := *source
	go func() {
		defer func() {
			s.ssh.gate.Release(connectionID, correlationID)
			s.ssh.mu.Lock()
			delete(s.ssh.updates, connectionID)
			s.ssh.mu.Unlock()
			close(op.done)
		}()
		result, err := s.updateManagedSSHConnection(src, correlationID)
		if err != nil {
			result = RefusedManagedSSHUpdate(connectionID, correlationID, err.Error(), "")
		}
		op.result = result
	}()
	<-op.done
	return op.result, nil
}

func (s *Service) managedOperationsDone() []<-chan struct{} {
	s.ssh.mu.Lock()
	defer s.ssh.mu.Unlock()
	out := make([]<-chan struct{}, 0, len(s.ssh.updates)+len(s.ssh.recoveries))
	for _, op := range s.ssh.updates {
		out = append(out, op.done)
	}
	for _, op := range s.ssh.recoveries {
		out = append(out, op.done)
	}
	return out
}

// JoinManagedUpdatesForApply waits two minutes for managed updates and recoveries, refusing the local apply while one runs
func (s *Service) JoinManagedUpdatesForApply(log func(string)) (bool, string, string) {
	if WaitForManagedUpdateOperations(s.managedOperationsDone, managedUpdateApplyJoin) {
		return true, "", ""
	}
	log("[updates] a managed SSH update is still running; local update hand-off refused until it finishes")
	return false, "managed-update-running", "A remote update is still running. Try again when it finishes."
}

// Whether quit must first join the managed updates, starting the join once and quitting again when it settles
func (s *Service) DeferQuitForManagedUpdates(quit func()) bool {
	s.ssh.mu.Lock()
	if s.ssh.quitJoinDone || (!s.ssh.quitJoinStarted && len(s.ssh.updates) == 0 && len(s.ssh.recoveries) == 0) {
		s.ssh.mu.Unlock()
		return false
	}
	start := !s.ssh.quitJoinStarted
	s.ssh.quitJoinStarted = true
	s.ssh.mu.Unlock()
	if start {
		go func() {
			WaitForManagedUpdateOperations(s.managedOperationsDone, 0)
			s.ssh.mu.Lock()
			s.ssh.quitJoinDone = true
			s.ssh.mu.Unlock()
			quit()
		}()
	}
	return true
}

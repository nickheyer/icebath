package backend

import "errors"

// ConnectionChange is what applying a saved connection change cancels, tears down and announces
type ConnectionChange struct {
	CancelAndWait   func(scope string) error
	IsPrimary       bool
	RehomePrimary   func() error
	Scope           string
	SendApplied     func()
	StopPool        func(scope string) error
	TeardownPrimary func() error
	TeardownSSH     func(scope string) error
}

// Cancels the scope bootstrap and SSH state, then stops its pool entry or re-homes or tears down the primary
func ApplyConnectionChange(change ConnectionChange) error {
	if err := change.CancelAndWait(change.Scope); err != nil {
		return err
	}
	if err := change.TeardownSSH(change.Scope); err != nil {
		return err
	}
	if !change.IsPrimary {
		change.StopPool(change.Scope)
		return nil
	}
	if change.RehomePrimary != nil {
		return change.RehomePrimary()
	}
	if err := change.TeardownPrimary(); err != nil {
		return err
	}
	change.SendApplied()
	return nil
}

// Commits a start failure only when the attempt that failed is still the current one
func CommitConnectionFailure[T comparable](current, starting T, commit func()) bool {
	if current != starting {
		return false
	}
	commit()
	return true
}

// Terminal target of a connection, ensuring the backend once while the target is still pending
func ResolveTerminalConnection[T any](getTarget func() (T, bool), ensureBackend func() error) (T, error) {
	target, pending := getTarget()
	if !pending {
		return target, nil
	}
	if err := ensureBackend(); err != nil {
		var zero T
		return zero, err
	}
	target, pending = getTarget()
	if pending {
		var zero T
		return zero, errors.New("Remote connection is not ready yet. Try again in a moment.")
	}
	return target, nil
}

// Terminal target of the window a request came from
func ResolveTerminalConnectionForSender[T any](windowID string, getTarget func(string) (T, bool), ensureBackend func(string) error) (T, error) {
	return ResolveTerminalConnection(func() (T, bool) { return getTarget(windowID) }, func() error { return ensureBackend(windowID) })
}

// Whether a repeated quit must keep waiting for SSH connections, bootstraps or a remote kill in flight
func SSHQuitShouldBlock(teardownDone bool, connectionCount, bootstrapPending int, inFlight bool) bool {
	if teardownDone {
		return false
	}
	return connectionCount > 0 || bootstrapPending > 0 || inFlight
}

// SSHTransport is the SSH channel a teardown kills the remote through, then unforwards and closes
type SSHTransport interface {
	CancelForward(localPort, remotePort int) error
	Close() error
}

// SSHTeardownState is the SSH connection of one scope with its forward and remote ownership id
type SSHTeardownState struct {
	SSH         SSHTransport
	OwnershipID string
	LocalPort   int
	RemotePort  int
}

// Kills the remote backend, drops the forward and closes the transport, no failed step trapping a quit
func TeardownSSHState(state SSHTeardownState, cleanupRemote func(ssh SSHTransport, ownershipID string) error) {
	_ = cleanupRemote(state.SSH, state.OwnershipID)
	if state.LocalPort != 0 && state.RemotePort != 0 {
		_ = state.SSH.CancelForward(state.LocalPort, state.RemotePort)
	}
	_ = state.SSH.Close()
}

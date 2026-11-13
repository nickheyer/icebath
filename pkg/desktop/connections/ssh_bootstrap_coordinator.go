package connections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// SHA256 of the scope and every connection field a bootstrap depends on
func SSHConfigFingerprint(scope string, cfg SSHConfig, effectiveConfigFingerprint string) string {
	port := any("")
	if cfg.Port != 0 {
		port = float64(cfg.Port)
	}
	text, _ := jsonMarshal([]any{scope, cfg.Host, cfg.User, port, cfg.KeyPath, cfg.RemoteHermesPath, cfg.RemoteProfile, effectiveConfigFingerprint})
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// SSHBootstrapMetadata names the pool, registry connection and managed update a bootstrap belongs to
type SSHBootstrapMetadata struct {
	ManagedScope             string
	ManagedUpdateCorrelation string
	PoolKey                  string
	RegistryConnectionID     string
	PrimaryRegistryScope     bool
}

// BootstrapLease is the cancel signal and force cleanups of one bootstrap generation
type BootstrapLease struct {
	ctx        context.Context
	cancel     context.CancelFunc
	coord      *SSHBootstrapCoordinator
	scope      string
	generation int

	mu       sync.Mutex
	cleanups map[int]func() error
	next     int
}

// Cancelled when a newer bootstrap or a drain replaces this one
func (l *BootstrapLease) Signal() context.Context { return l.ctx }

// Registers a resource closer a drain runs to unblock this bootstrap, returning its removal
func (l *BootstrapLease) OnForceCleanup(cleanup func() error) func() {
	l.mu.Lock()
	id := l.next
	l.next++
	l.cleanups[id] = cleanup
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.cleanups, id)
		l.mu.Unlock()
	}
}

func (l *BootstrapLease) forceCleanups() []func() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]func() error, 0, len(l.cleanups))
	for _, c := range l.cleanups {
		out = append(out, c)
	}
	return out
}

func (l *BootstrapLease) clearCleanups() {
	l.mu.Lock()
	l.cleanups = map[int]func() error{}
	l.mu.Unlock()
}

// Whether this lease is still the live generation of its scope
func (l *BootstrapLease) IsCurrent() bool {
	if l.ctx.Err() != nil {
		return false
	}
	l.coord.mu.Lock()
	defer l.coord.mu.Unlock()
	return l.coord.generations[l.scope] == l.generation
}

// Refuses a lease a newer setting superseded
func (l *BootstrapLease) AssertCurrent() error {
	if !l.IsCurrent() {
		return backend.SupersededError{}
	}
	return nil
}

// BootstrapEntry is one active bootstrap, its scope, metadata and result
type BootstrapEntry struct {
	Scope       string
	Metadata    SSHBootstrapMetadata
	Future      *backend.Future[backend.Connection]
	fingerprint string
	generation  int
	lease       *BootstrapLease
}

// SSHBootstrapCoordinator serializes bootstraps per scope behind supersession and drain barriers
type SSHBootstrapCoordinator struct {
	mu                sync.Mutex
	active            map[*BootstrapEntry]bool
	pending           map[string]*BootstrapEntry
	generations       map[string]int
	drains            map[string]chan struct{}
	shutdownRequested bool
}

// NewSSHBootstrapCoordinator is an empty coordinator
func NewSSHBootstrapCoordinator() *SSHBootstrapCoordinator {
	return &SSHBootstrapCoordinator{
		active:      map[*BootstrapEntry]bool{},
		pending:     map[string]*BootstrapEntry{},
		generations: map[string]int{},
		drains:      map[string]chan struct{}{},
	}
}

func settled[T any](f *backend.Future[T]) chan struct{} {
	done := make(chan struct{})
	go func() {
		_, _ = f.Wait()
		close(done)
	}()
	return done
}

func resolvedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// Joins a pending bootstrap of the same fingerprint, else supersedes it and runs after it and any drain settle
func (c *SSHBootstrapCoordinator) Start(scope, fingerprint string, run func(*BootstrapLease) (backend.Connection, error), metadata SSHBootstrapMetadata) *backend.Future[backend.Connection] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shutdownRequested {
		return backend.NewFuture(func() (backend.Connection, error) {
			return nil, supersededSSHError("SSH bootstrap was cancelled because Desktop is quitting.")
		})
	}
	current := c.pending[scope]
	if current != nil && current.fingerprint == fingerprint {
		return current.Future
	}
	if current != nil {
		current.lease.cancel()
	}
	generation := c.generations[scope] + 1
	c.generations[scope] = generation
	ctx, cancel := context.WithCancel(context.Background())
	lease := &BootstrapLease{ctx: ctx, cancel: cancel, coord: c, scope: scope, generation: generation, cleanups: map[int]func() error{}}
	drain := c.drains[scope]
	if drain == nil {
		drain = resolvedChan()
	}
	var predecessor chan struct{}
	if current != nil {
		predecessor = settled(current.Future)
	}
	entry := &BootstrapEntry{Scope: scope, Metadata: metadata, fingerprint: fingerprint, generation: generation, lease: lease}
	entry.Future = backend.NewFuture(func() (backend.Connection, error) {
		defer func() {
			lease.clearCleanups()
			c.mu.Lock()
			delete(c.active, entry)
			if p := c.pending[scope]; p != nil && p.generation == generation {
				delete(c.pending, scope)
			}
			c.mu.Unlock()
		}()
		if predecessor != nil {
			<-predecessor
		}
		<-drain
		if err := lease.AssertCurrent(); err != nil {
			return nil, err
		}
		return run(lease)
	})
	c.active[entry] = true
	c.pending[scope] = entry
	return entry.Future
}

// Aborts the pending bootstrap of a scope
func (c *SSHBootstrapCoordinator) Cancel(scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pending[scope]; p != nil {
		p.lease.cancel()
	}
}

// Drains a scope, aborting and force cleaning its bootstraps and running afterCancel inside the barrier
func (c *SSHBootstrapCoordinator) CancelAndWait(scope string, afterCancel func() error) error {
	own := make(chan struct{})
	c.mu.Lock()
	prior := c.drains[scope]
	barrier := make(chan struct{})
	go func() {
		if prior != nil {
			<-prior
		}
		<-own
		close(barrier)
	}()
	c.drains[scope] = barrier
	var entries []*BootstrapEntry
	for e := range c.active {
		if e.Scope == scope {
			entries = append(entries, e)
		}
	}
	for _, e := range entries {
		e.lease.cancel()
	}
	c.mu.Unlock()
	go func() {
		<-barrier
		c.mu.Lock()
		if c.drains[scope] == barrier {
			delete(c.drains, scope)
		}
		c.mu.Unlock()
	}()
	err := func() error {
		defer close(own)
		var wg sync.WaitGroup
		for _, e := range entries {
			for _, cleanup := range e.lease.forceCleanups() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = cleanup()
				}()
			}
		}
		wg.Wait()
		for _, e := range entries {
			_, _ = e.Future.Wait()
		}
		if afterCancel != nil {
			return afterCancel()
		}
		return nil
	}()
	if err != nil {
		return err
	}
	<-barrier
	return nil
}

// Aborts every active bootstrap
func (c *SSHBootstrapCoordinator) CancelAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := range c.active {
		e.lease.cancel()
	}
}

// Refuses every later bootstrap and aborts the active ones, the quit path
func (c *SSHBootstrapCoordinator) Shutdown() {
	c.mu.Lock()
	c.shutdownRequested = true
	c.mu.Unlock()
	c.CancelAll()
}

// Runs every registered force cleanup of the active bootstraps and waits for them
func (c *SSHBootstrapCoordinator) ForceCleanupAll() {
	c.mu.Lock()
	var cleanups []func() error
	for e := range c.active {
		cleanups = append(cleanups, e.lease.forceCleanups()...)
	}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, cleanup := range cleanups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cleanup()
		}()
	}
	wg.Wait()
}

// Results of the active bootstraps
func (c *SSHBootstrapCoordinator) Promises() []*backend.Future[backend.Connection] {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*backend.Future[backend.Connection], 0, len(c.active))
	for e := range c.active {
		out = append(out, e.Future)
	}
	return out
}

// Snapshot of the active bootstraps
func (c *SSHBootstrapCoordinator) Active() []*BootstrapEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*BootstrapEntry, 0, len(c.active))
	for e := range c.active {
		out = append(out, e)
	}
	return out
}

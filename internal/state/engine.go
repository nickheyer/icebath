package state

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/internal/home"
)

// Process wide storage state every store and database of one process shares, made once per process
type Engine struct {
	home   *home.Resolver
	live   liveRegistry
	probes headerProbes
	reads  readBudgets
	guards guardCounts
	shared registry
	pidns  pidNamespace
	mu     sync.Mutex
	// Most recent store open failure, surfaced by the unavailable-store message
	initErr string
	// Log kinds already emitted for one database label
	logged map[string]bool
	// Quarantined connections whose close-time checkpoint could not be switched off, open until the process exits
	retained []*conn
	// Database paths whose one in-place schema repair this process already attempted
	repairs map[string]bool
	// Cross-VM filesystem verdicts per resolved database directory
	crossVM map[string]bool
	// Observer of every committed session delete and deliberate archive in this process, nil when none is set
	sessionDeleted func(ctx context.Context, ev SessionDeleted)
	browse         browseScans
}

// Engine resolving homes through r, with no connection opened yet
func New(r *home.Resolver) (*Engine, error) {
	if r == nil {
		return nil, &MissingHookError{Name: "home resolver"}
	}
	return &Engine{
		home:    r,
		live:    liveRegistry{count: map[string]int{}},
		probes:  newHeaderProbes(),
		reads:   newReadBudgets(),
		guards:  guardCounts{handles: map[fileIdentity]int{}},
		shared:  newRegistry(),
		logged:  map[string]bool{},
		repairs: map[string]bool{},
		crossVM: map[string]bool{},
	}, nil
}

// Records the most recent store open failure, never cleared by a later success
func (e *Engine) setInitError(msg string) {
	e.mu.Lock()
	e.initErr = msg
	e.mu.Unlock()
}

// Most recent store open failure of this process, empty when none happened
func (e *Engine) LastInitError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initErr
}

// Reports true the first time kind is logged for label in this process
func (e *Engine) firstLog(kind, label string) bool {
	key := kind + "\x00" + label
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logged[key] {
		return false
	}
	e.logged[key] = true
	return true
}

// Keeps a quarantined connection reachable and open until the process exits
func (e *Engine) retain(c *conn) {
	e.mu.Lock()
	e.retained = append(e.retained, c)
	e.mu.Unlock()
}

// Claims the one in-place schema repair of path, true only for the first caller in this process
func (e *Engine) claimRepair(path string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.repairs[path] {
		return false
	}
	e.repairs[path] = true
	return true
}

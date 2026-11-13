package connections

import (
	"math"
	"sync"
	"time"
)

// FirstRunSetupDecision is how a gated first run boot resumes
type FirstRunSetupDecision string

const (
	FirstRunContinueLocal FirstRunSetupDecision = "continue-local"
	FirstRunRemoteApplied FirstRunSetupDecision = "remote-applied"
	FirstRunReset         FirstRunSetupDecision = "reset"
)

// FirstRunSetupBackend is the part of a resolved backend the setup choice reads
type FirstRunSetupBackend struct {
	ActiveRoot string
	Kind       string
	Platform   string
	// What the local setup card represents, none for the installer offer
	Local string
}

// FirstRunSetupGateOptions hold the hooks the choice is shown, hidden, logged and reported stuck through
type FirstRunSetupGateOptions struct {
	HideChoice   func()
	Log          func(message string)
	OnStuck      func(backend FirstRunSetupBackend, stuckAfterMs float64)
	PromptChoice func(backend FirstRunSetupBackend)
	// Milliseconds before a pending choice is reported stuck, zero or less never reporting
	StuckAfterMs float64
}

type firstRunWaiter struct {
	done     chan struct{}
	decision FirstRunSetupDecision
}

// FirstRunSetupGate holds a bootstrap-needed boot until the user picks local setup or a remote
type FirstRunSetupGate struct {
	mu                      sync.Mutex
	o                       FirstRunSetupGateOptions
	localBootstrapConfirmed bool
	waiter                  *firstRunWaiter
	stuckTimer              *time.Timer
}

// NewFirstRunSetupGate builds a gate with no pending choice and local setup unconfirmed
func NewFirstRunSetupGate(o FirstRunSetupGateOptions) *FirstRunSetupGate {
	return &FirstRunSetupGate{o: o}
}

func (g *FirstRunSetupGate) clearStuckTimerLocked() {
	if g.stuckTimer != nil {
		g.stuckTimer.Stop()
		g.stuckTimer = nil
	}
}

func (g *FirstRunSetupGate) armStuckTimerLocked(backend FirstRunSetupBackend) {
	g.clearStuckTimerLocked()
	after := g.o.StuckAfterMs
	if math.IsNaN(after) || math.IsInf(after, 0) || after <= 0 || g.o.Log == nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(time.Duration(after*float64(time.Millisecond)), func() {
		g.mu.Lock()
		current := g.stuckTimer == timer
		if current {
			g.stuckTimer = nil
		}
		g.mu.Unlock()
		if !current {
			return
		}
		if g.o.OnStuck != nil {
			g.o.OnStuck(backend, after)
		}
		platform := backend.Platform
		if platform == "" {
			platform = "unknown"
		}
		g.o.Log("[bootstrap] still waiting for first-run setup choice after " + jsNumber(jsMathRound(after/1000)) + "s " +
			"(platform=" + platform + ")")
	})
	g.stuckTimer = timer
}

func (g *FirstRunSetupGate) shouldGateLocked(backend *FirstRunSetupBackend) bool {
	return backend != nil && backend.Kind == "bootstrap-needed" && !g.localBootstrapConfirmed
}

// ShouldGate reports a bootstrap-needed backend whose local setup the user has not confirmed
func (g *FirstRunSetupGate) ShouldGate(backend *FirstRunSetupBackend) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shouldGateLocked(backend)
}

// Wait blocks a gated boot until a decision settles it, every concurrent boot sharing one prompt
func (g *FirstRunSetupGate) Wait(backend *FirstRunSetupBackend) FirstRunSetupDecision {
	g.mu.Lock()
	if !g.shouldGateLocked(backend) {
		g.mu.Unlock()
		return FirstRunContinueLocal
	}
	if w := g.waiter; w != nil {
		g.mu.Unlock()
		<-w.done
		return w.decision
	}
	if g.o.PromptChoice != nil {
		g.o.PromptChoice(*backend)
	}
	g.armStuckTimerLocked(*backend)
	w := &firstRunWaiter{done: make(chan struct{})}
	g.waiter = w
	g.mu.Unlock()
	<-w.done
	return w.decision
}

func (g *FirstRunSetupGate) settleWaiterLocked(decision FirstRunSetupDecision) bool {
	g.clearStuckTimerLocked()
	if g.waiter == nil {
		return false
	}
	active := g.waiter
	g.waiter = nil
	active.decision = decision
	close(active.done)
	return true
}

// ContinueLocal confirms local setup and resumes a pending boot into it
func (g *FirstRunSetupGate) ContinueLocal() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.localBootstrapConfirmed = true
	g.settleWaiterLocked(FirstRunContinueLocal)
}

// ResetForRetry settles a pending boot with reset so a reload starts over
func (g *FirstRunSetupGate) ResetForRetry() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settleWaiterLocked(FirstRunReset)
}

// ResetForRepair settles a pending boot with reset and asks for the setup choice again
func (g *FirstRunSetupGate) ResetForRepair() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settleWaiterLocked(FirstRunReset)
	g.localBootstrapConfirmed = false
}

// AbandonForRemoteApply resumes a pending boot with the applied remote, false when no boot waited
func (g *FirstRunSetupGate) AbandonForRemoteApply() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.settleWaiterLocked(FirstRunRemoteApplied) {
		return false
	}
	g.localBootstrapConfirmed = false
	if g.o.HideChoice != nil {
		g.o.HideChoice()
	}
	return true
}

// IsLocalBootstrapConfirmed reports whether the user chose local setup
func (g *FirstRunSetupGate) IsLocalBootstrapConfirmed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.localBootstrapConfirmed
}

// HasWaiter reports a boot waiting on the choice
func (g *FirstRunSetupGate) HasWaiter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiter != nil
}

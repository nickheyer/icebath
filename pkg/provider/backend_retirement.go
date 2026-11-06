package provider

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Process admission fence a Desktop backend closes before it retires
type RetirementFence struct {
	mu        sync.Mutex
	active    int
	preparing bool
	token     string
	committed bool
	expiresAt float64
	now       func() float64
}

var retirementEpoch = time.Now()

// Seconds on the monotonic clock since the process loaded the package
func retirementMonotonic() float64 { return time.Since(retirementEpoch).Seconds() }

// Work snapshot prepare freezes admission for, registered by the web server package
var RetirementIdleProof func() (map[string]any, error)

// Fence every source of new work in this process reserves admission through
var Retirement = NewRetirementFence()

// Fresh open fence with no permit and no admitted work
func NewRetirementFence() *RetirementFence {
	return &RetirementFence{now: retirementMonotonic}
}

// Drops an uncommitted permit once its thirty second window has passed
func (r *RetirementFence) expire() {
	now := r.now
	if now == nil {
		now = retirementMonotonic
	}
	if r.token != "" && !r.committed && now() >= r.expiresAt {
		r.token = ""
	}
}

// Admits one unit of work unless a prepare is running or a permit is outstanding
func (r *RetirementFence) Acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire()
	if r.preparing || r.token != "" {
		return false
	}
	r.active++
	return true
}

// Returns one admitted unit of work
func (r *RetirementFence) Release() {
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
}

// Number of admitted units of work still running
func (r *RetirementFence) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Reserves admission and hands back a release that returns it only when it was granted
func (r *RetirementFence) Work() (bool, func()) {
	admitted := r.Acquire()
	return admitted, func() {
		if admitted {
			r.Release()
		}
	}
}

// Calls the registered idle proof and reads its idle verdict, raising like the Python lookup does
func retirementIdleVerdict() (verdict any, err error) {
	defer func() {
		if p := recover(); p != nil {
			verdict, err = nil, fmt.Errorf("%v", p)
		}
	}()
	probe := RetirementIdleProof
	if probe == nil {
		return nil, fmt.Errorf("No module named 'hermes_cli.web_server_idle_proof'")
	}
	proof, err := probe()
	if err != nil {
		return nil, err
	}
	v, ok := proof["idle"]
	if !ok {
		return nil, fmt.Errorf("KeyError: 'idle'")
	}
	return v, nil
}

// Freezes admission, takes the idle snapshot and grants a thirty second permit only when it is idle
func (r *RetirementFence) Prepare() map[string]any {
	r.mu.Lock()
	r.expire()
	if r.committed {
		token := r.token
		r.mu.Unlock()
		return map[string]any{"ok": true, "idle": true, "token": token}
	}
	if r.preparing || r.token != "" || r.active != 0 {
		r.mu.Unlock()
		return map[string]any{"ok": false, "idle": false}
	}
	r.preparing = true
	r.mu.Unlock()
	verdict, err := retirementIdleVerdict()
	if err != nil {
		slog.Warn("Retirement idle probe unavailable", "err", err)
	}
	idle, _ := verdict.(bool)
	var token string
	r.mu.Lock()
	r.preparing = false
	if idle {
		token = retirementToken()
		r.token = token
		now := r.now
		if now == nil {
			now = retirementMonotonic
		}
		r.expiresAt = now() + 30.0
	}
	r.mu.Unlock()
	if !idle {
		return map[string]any{"ok": false, "idle": verdict}
	}
	return map[string]any{"ok": true, "idle": true, "token": token}
}

// Closes admission for good when the token names the live permit
func (r *RetirementFence) Commit(token string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire()
	if token == "" || token != r.token {
		return map[string]any{"ok": false}
	}
	r.committed = true
	return map[string]any{"ok": true}
}

// Withdraws an uncommitted permit so admission reopens
func (r *RetirementFence) Cancel(token string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire()
	if token == "" || token != r.token || r.committed {
		return map[string]any{"ok": false}
	}
	r.token = ""
	return map[string]any{"ok": true}
}

// Random URL safe token from thirty two bytes of system entropy
func retirementToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

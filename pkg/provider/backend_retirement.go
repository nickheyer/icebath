package provider

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Process admission fence that counts the units of work every work source reserves
type RetirementFence struct {
	mu     sync.Mutex
	active int
}

// Fence every source of new work in this process reserves admission through
var Retirement = NewRetirementFence()

// Process fence handed to work sources, read at each call so a swapped fence takes effect like the module attribute
func BackendRetirement() tools.RetirementWork { return processRetirement{} }

type processRetirement struct{}

func (processRetirement) Work() (bool, func()) { return Retirement.Work() }

func (processRetirement) Acquire() bool { return Retirement.Acquire() }

func (processRetirement) Release() { Retirement.Release() }

// Fresh fence with no admitted work
func NewRetirementFence() *RetirementFence {
	return &RetirementFence{}
}

// Admits and counts one unit of work
func (r *RetirementFence) Acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
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

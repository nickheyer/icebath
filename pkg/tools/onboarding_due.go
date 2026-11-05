package tools

import (
	"sync"
	"time"
)

// OnboardingBootWait is how long the desktop's first question waits for the boot computation
const OnboardingBootWait = 8 * time.Second

// The boot's first computation can write a config skeleton itself, so its files move once
const onboardingBootAttempts = 2

// OnboardingDue is the boot answer, whether onboarding is due and what can carry it
type OnboardingDue map[string]bool

// OnboardingStamp fingerprints the files an answer reads so edits nobody saw are caught
type OnboardingStamp func() string

var (
	onbDueMu         sync.Mutex
	onbDueValue      OnboardingDue
	onbDueStamp      string
	onbDueGeneration int
	onbBootDone      chan struct{}
)

// Copy of a due map
func (d OnboardingDue) clone() OnboardingDue {
	out := make(OnboardingDue, len(d))
	for k, v := range d {
		out[k] = v
	}
	return out
}

// Stamp or the empty fingerprint when none is given
func onbStamp(stamp OnboardingStamp) string {
	if stamp == nil {
		return ""
	}
	return stamp()
}

// OnboardingBeginBoot marks a boot computation in flight and returns the call that finishes it with its answer
func OnboardingBeginBoot(stamp OnboardingStamp) func(compute func() OnboardingDue) {
	done := make(chan struct{})
	onbDueMu.Lock()
	onbBootDone = done
	onbDueMu.Unlock()
	return func(compute func() OnboardingDue) {
		defer close(done)
		for i := 0; i < onboardingBootAttempts; i++ {
			onbDueMu.Lock()
			generation := onbDueGeneration
			onbDueMu.Unlock()
			seen := onbStamp(stamp)
			value := compute()
			if onbDueStore(value, generation, seen, onbStamp(stamp)) {
				return
			}
		}
	}
}

// OnboardingDueRead returns the cached answer, waiting a bounded time for a boot computation, else computing live
func OnboardingDueRead(compute func() OnboardingDue, stamp OnboardingStamp, wait time.Duration) OnboardingDue {
	onbDueMu.Lock()
	done := onbBootDone
	onbDueMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(wait):
		}
	}
	seen := onbStamp(stamp)
	onbDueMu.Lock()
	if onbDueValue != nil && onbDueStamp == seen {
		value := onbDueValue.clone()
		onbDueMu.Unlock()
		return value
	}
	generation := onbDueGeneration
	onbDueMu.Unlock()
	value := compute()
	onbDueStore(value, generation, seen, onbStamp(stamp))
	return value.clone()
}

// OnboardingDueInvalidate drops the cached answer, every writer that can move it calls this
func OnboardingDueInvalidate() {
	onbDueMu.Lock()
	onbDueValue = nil
	onbDueGeneration++
	onbDueMu.Unlock()
}

// OnboardingDueResetForTests clears the boot marker and the cached answer
func OnboardingDueResetForTests() {
	OnboardingDueInvalidate()
	onbDueMu.Lock()
	onbBootDone = nil
	onbDueMu.Unlock()
}

// Caches the value computed between the two stamps, true when it was cached
func onbDueStore(value OnboardingDue, generation int, before, after string) bool {
	if before != after {
		return false
	}
	onbDueMu.Lock()
	defer onbDueMu.Unlock()
	if generation != onbDueGeneration {
		return false
	}
	onbDueValue, onbDueStamp = value.clone(), after
	return true
}

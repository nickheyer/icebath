package dashboard

import (
	"fmt"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

const DefaultOwnerPollS = 15.0

// Retire only when a valid lock names another spawn of this slot and the process outlived the lock settle window
func ShouldRetireSuperseded(lock map[string]any, myNonce string, ageS float64) bool {
	if lock == nil {
		return false
	}
	nonce, isStr := lock["spawnNonce"].(string)
	return (!isStr || nonce != myNonce) && ageS >= gateway.ReapMinAgeSeconds
}

// Goroutine setting the exit flag once this backend is provably superseded and provably idle
func StartOwnerWatchdog(shouldExit *atomic.Bool, lockPath, nonce string, readLock func(string) map[string]any, fence RetirementFence,
	pollS float64, now func() float64, maxPolls int) <-chan struct{} {
	if readLock == nil {
		readLock = tools.ReadValidBackendLock
	}
	if now == nil {
		now = monotonic
	}
	started := now()
	observe := func() string {
		lock := readLock(lockPath)
		if lock == nil || !ShouldRetireSuperseded(lock, nonce, now()-started) {
			return ""
		}
		return fmt.Sprintf("SSH-isolated backend %s was superseded by spawn %v; retiring.", nonce, lock["spawnNonce"])
	}
	return runRetirementWatchdog(shouldExit, observe, fence, pollS, maxPolls)
}

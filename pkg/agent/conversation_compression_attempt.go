package agent

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Global claim lock shared by every compressor, cheap because the slow rollback waits on the per compressor lock
var compressorAttemptLock sync.Mutex

type compressorAttemptGenerationKey struct{}

// Attempt ownership attributes a compressor holds, created on first claim
type compressorAttemptSlots struct {
	serial              sync.Mutex
	generation          int
	workingGeneration   *int
	cancelledCheckOwner *int
}

// Generation counters of the slots on fresh locks, nil for nil
func (s *compressorAttemptSlots) copySlots() *compressorAttemptSlots {
	if s == nil {
		return nil
	}
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	out := &compressorAttemptSlots{generation: s.generation}
	if s.workingGeneration != nil {
		v := *s.workingGeneration
		out.workingGeneration = &v
	}
	if s.cancelledCheckOwner != nil {
		v := *s.cancelledCheckOwner
		out.cancelledCheckOwner = &v
	}
	return out
}

// Compressor that can hold attempt ownership attributes
type attemptOwnedCompressor interface {
	attemptSlotsLocked(create bool) *compressorAttemptSlots
}

func (c *ContextCompressor) attemptSlotsLocked(create bool) *compressorAttemptSlots {
	if c.attempt == nil && create {
		c.attempt = &compressorAttemptSlots{}
	}
	return c.attempt
}

// Calling attempt generation riding the context, nil outside the dispatch machinery
func withCompressorAttemptGeneration(ctx context.Context, generation int) context.Context {
	return context.WithValue(ctx, compressorAttemptGenerationKey{}, generation)
}

func compressorAttemptGeneration(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	g, _ := ctx.Value(compressorAttemptGenerationKey{}).(int)
	return g
}

// Per compressor lock fencing the durable rollback against claims, nil for an attribute less compressor
func compressorAttemptSerialLock(compressor any) *sync.Mutex {
	owned, ok := compressor.(attemptOwnedCompressor)
	if !ok {
		return nil
	}
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	return &owned.attemptSlotsLocked(true).serial
}

// Claims the compressor for a new attempt and returns its generation, zero disables the guard
func claimCompressorAttempt(compressor any) int {
	serial := compressorAttemptSerialLock(compressor)
	if serial == nil {
		return 0
	}
	serial.Lock()
	defer serial.Unlock()
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	slots := compressor.(attemptOwnedCompressor).attemptSlotsLocked(true)
	slots.generation++
	return slots.generation
}

// Whether the generation still owns the compressor, true when the guard is off
func compressorAttemptIsCurrent(compressor any, generation int) bool {
	if generation == 0 {
		return true
	}
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	return entryGenerationLocked(compressor) == generation
}

func entryGenerationLocked(compressor any) int {
	owned, ok := compressor.(attemptOwnedCompressor)
	if !ok {
		return 0
	}
	if slots := owned.attemptSlotsLocked(false); slots != nil {
		return slots.generation
	}
	return 0
}

// Publishes the generation of the attempt that is running summary work
func markCompressorWorkingAttempt(compressor any, generation int) {
	owned, ok := compressor.(attemptOwnedCompressor)
	if !ok {
		return
	}
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	g := generation
	owned.attemptSlotsLocked(true).workingGeneration = &g
}

// Explicit cancellation when a newer attempt has begun summary work on the compressor
func raiseIfStaleAttempt(ctx context.Context, compressor any) error {
	if !callerAttemptIsCurrent(ctx, compressor) {
		return &provider.AuxiliaryExplicitCancellation{}
	}
	return nil
}

// Working attempt check for the generation the calling context carries, unguarded without one
func callerAttemptIsCurrent(ctx context.Context, compressor any) bool {
	return workingAttemptIsCurrent(compressor, compressorAttemptGeneration(ctx))
}

// Whether the generation is the last to begin summary work, the entry claim deciding before any dispatch
func workingAttemptIsCurrent(compressor any, generation int) bool {
	if generation == 0 {
		return true
	}
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	var marker *int
	if owned, ok := compressor.(attemptOwnedCompressor); ok {
		if slots := owned.attemptSlotsLocked(false); slots != nil {
			marker = slots.workingGeneration
		}
	}
	if marker == nil {
		entry := entryGenerationLocked(compressor)
		return entry == 0 || entry == generation
	}
	return *marker == generation
}

// Installs the cancellation consult stamped with its owner attempt
func installCompressionCancelledCheck(compressor any, check func() bool, generation int) {
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	if setter, ok := compressor.(interface{ SetCompressionCancelledCheck(func() bool) }); ok {
		setter.SetCompressionCancelledCheck(check)
	}
	if owned, ok := compressor.(attemptOwnedCompressor); ok {
		g := generation
		owned.attemptSlotsLocked(true).cancelledCheckOwner = &g
	}
}

// Clears the cancellation consult only for the attempt that installed it, true when cleared
func clearCompressionCancelledCheckIfOwner(compressor any, generation int) bool {
	compressorAttemptLock.Lock()
	defer compressorAttemptLock.Unlock()
	var slots *compressorAttemptSlots
	if owned, ok := compressor.(attemptOwnedCompressor); ok {
		slots = owned.attemptSlotsLocked(false)
	}
	if slots != nil && slots.cancelledCheckOwner != nil && generation != 0 && *slots.cancelledCheckOwner != generation {
		return false
	}
	if setter, ok := compressor.(interface{ SetCompressionCancelledCheck(func() bool) }); ok {
		setter.SetCompressionCancelledCheck(nil)
	}
	if slots != nil {
		slots.cancelledCheckOwner = nil
	}
	return true
}

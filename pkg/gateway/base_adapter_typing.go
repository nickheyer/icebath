package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// Settable flag goroutines wait on, the asyncio Event of a session's interrupt signal
type InterruptEvent struct {
	mu         sync.Mutex
	ch         chan struct{}
	set        bool
	generation any
}

// Run generation the agent cache bound to this turn's interrupt event, nil when none was bound
func (e *InterruptEvent) RunGeneration() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// Binds the run generation of the turn this interrupt event belongs to
func (e *InterruptEvent) SetRunGeneration(generation int) {
	e.mu.Lock()
	e.generation = generation
	e.mu.Unlock()
}

// Unset event
func NewInterruptEvent() *InterruptEvent { return &InterruptEvent{ch: make(chan struct{})} }

// Sets the flag and wakes every waiter
func (e *InterruptEvent) Set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

// Resets the flag so later waiters block again
func (e *InterruptEvent) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}

// Whether the flag is set
func (e *InterruptEvent) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

// Channel closed once the flag is set
func (e *InterruptEvent) Done() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ch
}

// Deferred callback fired after a turn's reply was delivered
type PostDeliveryCallback func(ctx context.Context) error

// One post delivery slot, the generation nil for a callback registered without one
type postDeliveryEntry struct {
	generation *int
	callback   PostDeliveryCallback
}

// Generation and callback of a post delivery slot
func splitPostDeliveryEntry(entry postDeliveryEntry) (*int, PostDeliveryCallback) {
	return entry.generation, entry.callback
}

// Runs the callback bounded by the timeout with every error swallowed, nothing for a nil callback
func RunPostDeliveryCallback(ctx context.Context, callback PostDeliveryCallback, timeout float64) {
	if callback == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, secondsDuration(timeout))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		_ = callback(cctx)
	}()
	select {
	case <-done:
	case <-cctx.Done():
	}
}

// Optional reaction surface an adapter provides for the outcome reaction swap
type ReactionAdapter interface {
	AddReaction(ctx context.Context, chatID, messageID, emoji string) error
	RemoveReaction(ctx context.Context, chatID, messageID string) error
}

// Optional runtime switch for the outcome reactions
type ReactionsEnabler interface {
	ReactionsEnabled(ctx context.Context) bool
}

// Refreshes the typing indicator every interval until cancelled or stopped, skipping paused chats
func (b *BasePlatformAdapter) KeepTyping(ctx context.Context, chatID string, interval float64, metadata map[string]any, stopEvent *InterruptEvent) {
	self := b.Self()
	sendTimeout := secondsDuration(max(0.25, min(1.5, interval-0.25)))
	defer func() {
		b.stopTypingQuietly(context.WithoutCancel(ctx), chatID, metadata)
		b.mu.Lock()
		delete(lazyAttr(&b.typingPaused), chatID)
		delete(b.statusText, chatID)
		b.mu.Unlock()
	}()
	for {
		if stopEvent != nil && stopEvent.IsSet() {
			return
		}
		b.mu.Lock()
		_, paused := b.typingPaused[chatID]
		b.mu.Unlock()
		if !paused {
			tctx, cancel := context.WithTimeout(ctx, sendTimeout)
			done := make(chan error, 1)
			go func() { done <- self.SendTyping(tctx, chatID, metadata) }()
			select {
			case err := <-done:
				if err != nil && ctx.Err() == nil && tctx.Err() == nil {
					slog.Debug(fmt.Sprintf("[%s] send_typing error (non-fatal): %v", self.Name(), err))
				}
			case <-tctx.Done():
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
		}
		var stopped <-chan struct{}
		if stopEvent != nil {
			stopped = stopEvent.Done()
		}
		timer := time.NewTimer(secondsDuration(interval))
		select {
		case <-timer.C:
		case <-stopped:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// Stops the refresh task and the platform typing state as one operation
func (b *BasePlatformAdapter) stopTypingRefresh(ctx context.Context, chatID string, typingTask *adapterTask, metadata map[string]any, timeout float64, stopAttempts int) {
	b.mu.Lock()
	lazyAttr(&b.typingPaused)[chatID] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.typingPaused, chatID)
		b.mu.Unlock()
	}()
	if typingTask != nil && !typingTask.finished() {
		typingTask.cancel()
		timer := time.NewTimer(secondsDuration(timeout))
		select {
		case <-typingTask.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	for attempt := 0; attempt < max(1, stopAttempts); attempt++ {
		if attempt > 0 {
			runtime.Gosched()
		}
		b.stopTypingQuietly(ctx, chatID, metadata)
	}
}

// Platform stop typing with adapter errors swallowed
func (b *BasePlatformAdapter) stopTypingQuietly(ctx context.Context, chatID string, metadata map[string]any) {
	defer func() { _ = recover() }()
	_ = b.stopTypingWithMetadata(ctx, chatID, metadata)
}

// Pauses the typing refresh for a chat while an approval waits
func (b *BasePlatformAdapter) PauseTypingForChat(chatID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lazyAttr(&b.typingPaused)[chatID] = struct{}{}
}

// Resumes the typing refresh for a chat once the approval resolves
func (b *BasePlatformAdapter) ResumeTypingForChat(chatID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.typingPaused, chatID)
}

// Signals the active session loop to stop and clears typing at once
func (b *BasePlatformAdapter) InterruptSessionActivity(ctx context.Context, sessionKey, chatID string, metadata map[string]any) {
	b.mu.Lock()
	ev := b.activeSessions[sessionKey]
	b.mu.Unlock()
	if sessionKey != "" && ev != nil {
		ev.Set()
	}
	b.stopTypingQuietly(ctx, chatID, metadata)
}

// Registers a callback after the main response, chaining same generation slots and dropping stale ones
func (b *BasePlatformAdapter) RegisterPostDeliveryCallback(sessionKey string, callback PostDeliveryCallback, generation *int) {
	if sessionKey == "" || callback == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	slots := lazyAttr(&b.postDeliveryCallbacks)
	if existing, ok := slots[sessionKey]; ok {
		existingGen, existingCb := splitPostDeliveryEntry(existing)
		if existingGen != nil && generation != nil && *generation < *existingGen {
			return
		}
		if existingCb != nil && (existingGen == nil || generation == nil || *existingGen == *generation) {
			callback = chainCallbacks(existingCb, callback)
		}
	}
	entry := postDeliveryEntry{callback: callback}
	if generation != nil {
		g := *generation
		entry.generation = &g
	}
	slots[sessionKey] = entry
}

// Callback running each callback in order with per callback error isolation
func chainCallbacks(callbacks ...PostDeliveryCallback) PostDeliveryCallback {
	return func(ctx context.Context) error {
		for _, cb := range callbacks {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Debug(fmt.Sprintf("Post-delivery callback failed: %v", r))
					}
				}()
				if err := cb(ctx); err != nil {
					slog.Debug(fmt.Sprintf("Post-delivery callback failed: %v", err))
				}
			}()
		}
		return nil
	}
}

// Pops the deferred callback, requiring the generation to own the slot when one is given
func (b *BasePlatformAdapter) PopPostDeliveryCallback(sessionKey string, generation *int) PostDeliveryCallback {
	if sessionKey == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.postDeliveryCallbacks[sessionKey]
	if !ok {
		return nil
	}
	entryGen, callback := splitPostDeliveryEntry(entry)
	if generation != nil && (entryGen == nil || *entryGen != *generation) {
		return nil
	}
	delete(b.postDeliveryCallbacks, sessionKey)
	return callback
}

// Hook run when background processing begins, nothing by default
func (b *BasePlatformAdapter) OnProcessingStart(ctx context.Context, event *MessageEvent) error {
	return nil
}

// Swaps the in progress reaction for the outcome one when the adapter opts in, cancelled stays unreacted
func (b *BasePlatformAdapter) OnProcessingComplete(ctx context.Context, event *MessageEvent, outcome ProcessingOutcome) error {
	if b.Traits.OkEmoji == "" && b.Traits.FailEmoji == "" {
		return nil
	}
	reactor, ok := b.Self().(ReactionAdapter)
	if !ok {
		return nil
	}
	if enabler, ok := b.Self().(ReactionsEnabler); ok && !enabler.ReactionsEnabled(ctx) {
		return nil
	}
	chatID := ""
	if event.Source != nil {
		chatID = event.Source.ChatID
	}
	if chatID == "" || event.MessageID == nil || *event.MessageID == "" {
		return nil
	}
	messageID := *event.MessageID
	if err := reactor.RemoveReaction(ctx, chatID, messageID); err != nil {
		return err
	}
	emoji := map[ProcessingOutcome]string{ProcessingSuccess: b.Traits.OkEmoji, ProcessingFailure: b.Traits.FailEmoji}[outcome]
	if emoji != "" {
		return reactor.AddReaction(ctx, chatID, messageID, emoji)
	}
	return nil
}

// Runs a lifecycle hook, logging a failure instead of breaking the message flow
func (b *BasePlatformAdapter) runProcessingHook(ctx context.Context, hookName string, hook func(ctx context.Context) error) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return hook(ctx)
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] %s hook failed: %v", b.Self().Name(), hookName, err))
	}
}

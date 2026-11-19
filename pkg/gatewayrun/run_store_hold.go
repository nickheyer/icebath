package gatewayrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/state"
)

// Store outage met while running, its described cause and raw detail
type storeOutage struct {
	failure state.StorageFailure
	detail  string
}

// Inbound user messages a store outage stopped, re-run in arrival order once the store takes writes again
type storeHeldQueue struct {
	mu     sync.Mutex
	events []*gateway.MessageEvent
	// Spool copy of each message re-dispatched at start, deleted once its turn settles
	copies map[*gateway.MessageEvent]string
	// Signal of a re-run message whose handling returned, so the next one of its chat follows it
	handled map[*gateway.MessageEvent]chan struct{}
}

// Seconds a re-run held message may take before the next one of its chat follows anyway
const storeHeldTurnWaitS = 600

// Puts the store outage copies of the spool at the front of the held queue, before row recovery could add them twice
func (r *GatewayRunner) requeueStoreOutageMessages(ctx context.Context) {
	taken := gateway.TakeStoreOutageMessages(ctx, r.sessionStore)
	if len(taken) == 0 {
		return
	}
	events := make([]*gateway.MessageEvent, len(taken))
	r.storeHeld.mu.Lock()
	if r.storeHeld.copies == nil {
		r.storeHeld.copies = map[*gateway.MessageEvent]string{}
	}
	for i, message := range taken {
		r.storeHeld.copies[message.Event] = message.Path
		events[i] = message.Event
	}
	r.storeHeld.events = append(events, r.storeHeld.events...)
	r.storeHeld.mu.Unlock()
}

// Deletes the spool copy of a re-dispatched message whose turn ran and lets the next one of its chat follow
func (r *GatewayRunner) settleStoreOutageCopy(event *gateway.MessageEvent) {
	r.storeHeld.mu.Lock()
	path, ok := r.storeHeld.copies[event]
	delete(r.storeHeld.copies, event)
	r.storeHeld.mu.Unlock()
	if ok {
		gateway.SettleStoreOutageMessage(path)
	}
	r.markStoreHeldHandled(event)
}

// Wakes the chain waiting on a re-run message's handling
func (r *GatewayRunner) markStoreHeldHandled(event *gateway.MessageEvent) {
	r.storeHeld.mu.Lock()
	done, ok := r.storeHeld.handled[event]
	delete(r.storeHeld.handled, event)
	r.storeHeld.mu.Unlock()
	if ok {
		close(done)
	}
}

// Platform and chat of an event for the hold log lines
func heldEventLabel(event *gateway.MessageEvent) string {
	if event.Source == nil {
		return "an unknown chat"
	}
	return fmt.Sprintf("%s:%s", event.Source.Platform, event.Source.ChatID)
}

// Holds a user message whose turn met a store outage, returning the reply that tells the user it waits, nil when not held
func (r *GatewayRunner) holdForStoreOutage(ctx context.Context, event *gateway.MessageEvent, err error) *string {
	if event == nil || event.Internal || !state.IsStoreFailure(err) {
		return nil
	}
	failure := state.DescribeStorageFailure(ctx, err)
	if selfClearingStorageCauses[failure.Cause] || sessionScopedStorageCauses[failure.Cause] {
		return nil
	}
	r.noteSessionStoreOutage(ctx, failure, state.StorageFailureDetails(err, 200))
	return r.holdStoreEvent(ctx, event, failure, state.StorageFailureDetails(err, 200))
}

// Holds a user message that arrives while a noted outage lasts, so it never enters the failing store, nil otherwise
func (r *GatewayRunner) holdDuringStoreOutage(ctx context.Context, event *gateway.MessageEvent) *string {
	if event == nil || event.Internal {
		return nil
	}
	r.sessionStoreOutageMu.Lock()
	outage := r.sessionStoreOutage
	r.sessionStoreOutageMu.Unlock()
	if outage == nil {
		return nil
	}
	return r.holdStoreEvent(ctx, event, outage.failure, outage.detail)
}

// Appends the event to the held queue and returns the reply telling the user it waits
func (r *GatewayRunner) holdStoreEvent(ctx context.Context, event *gateway.MessageEvent, failure state.StorageFailure, detail string) *string {
	r.storeHeld.mu.Lock()
	r.storeHeld.events = append(r.storeHeld.events, event)
	count := len(r.storeHeld.events)
	r.storeHeld.mu.Unlock()
	pkgLog().Warn(fmt.Sprintf("Holding the message from %s until session storage takes writes again (%d held): %s",
		heldEventLabel(event), count, detail))
	r.markStoreHeldHandled(event)
	reply := i18n.T(ctx, "gateway.errors.session_storage_held", "", map[string]any{"cause": failure.Gloss})
	return &reply
}

// Re-runs the held messages through their adapters, each chat in arrival order and one message after the other
func (r *GatewayRunner) replayStoreHeldEvents(ctx context.Context) int {
	if !r.running.Load() || r.draining.Load() {
		return 0
	}
	r.storeHeld.mu.Lock()
	events := r.storeHeld.events
	r.storeHeld.events = nil
	r.storeHeld.mu.Unlock()
	if len(events) == 0 {
		return 0
	}
	var order []string
	chains := map[string][]*gateway.MessageEvent{}
	for _, event := range events {
		chat := heldEventLabel(event)
		if _, seen := chains[chat]; !seen {
			order = append(order, chat)
		}
		chains[chat] = append(chains[chat], event)
	}
	pkgLog().Info(fmt.Sprintf("Session storage takes writes; re-running %d held message(s) of %d chat(s)", len(events), len(order)))
	settled := context.WithoutCancel(ctx)
	for _, chat := range order {
		chain := chains[chat]
		go r.replayStoreHeldChain(settled, chain)
	}
	return len(events)
}

// Re-runs one chat's held messages in order, each after the previous one's handling returned
func (r *GatewayRunner) replayStoreHeldChain(ctx context.Context, chain []*gateway.MessageEvent) {
	for i, event := range chain {
		adapter, err := r.intakeAdapterFor(ctx, event.Source)
		if err == nil && adapter == nil {
			err = fmt.Errorf("no connected adapter serves %s", heldEventLabel(event))
		}
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("Held message(s) of %s stay held: %v", heldEventLabel(event), err))
			r.storeHeld.mu.Lock()
			r.storeHeld.events = append(append([]*gateway.MessageEvent(nil), chain[i:]...), r.storeHeld.events...)
			r.storeHeld.mu.Unlock()
			return
		}
		done := make(chan struct{})
		r.storeHeld.mu.Lock()
		if r.storeHeld.handled == nil {
			r.storeHeld.handled = map[*gateway.MessageEvent]chan struct{}{}
		}
		r.storeHeld.handled[event] = done
		r.storeHeld.mu.Unlock()
		if err := adapter.Base().HandleMessage(ctx, event); err != nil {
			r.markStoreHeldHandled(event)
			pkgLog().Warn(fmt.Sprintf("Held message of %s failed to re-run: %v", heldEventLabel(event), err))
			continue
		}
		select {
		case <-done:
		case <-time.After(storeHeldTurnWaitS * time.Second):
			r.markStoreHeldHandled(event)
		case <-ctx.Done():
			return
		}
	}
}

// Writes the messages still held at shutdown to the pending_messages spool, which the next start adds to their sessions
func (r *GatewayRunner) spoolStoreHeldEvents(ctx context.Context) int {
	r.storeHeld.mu.Lock()
	events := r.storeHeld.events
	r.storeHeld.events = nil
	r.storeHeld.mu.Unlock()
	bySession := map[string][]any{}
	for _, event := range events {
		r.storeHeld.mu.Lock()
		_, copied := r.storeHeld.copies[event]
		r.storeHeld.mu.Unlock()
		if copied {
			continue
		}
		key, err := r.sessionKeyForSource(ctx, event.Source)
		if err != nil {
			pkgLog().Error(fmt.Sprintf("Held message from %s has no session key (%v); its text was: %s", heldEventLabel(event), err, event.Text))
			continue
		}
		bySession[key] = append(bySession[key], event)
	}
	if len(bySession) == 0 {
		return 0
	}
	written, err := gateway.FlushOverflowToFile(ctx, bySession, gateway.StoreOutageReason)
	if err != nil {
		for key, held := range bySession {
			for _, event := range held {
				pkgLog().Error(fmt.Sprintf("Held message of %s could not be spooled at shutdown (%v); its text was: %s", key, err, event.(*gateway.MessageEvent).Text))
			}
		}
	}
	return written
}

package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/observability"
)

// One shot outcome many goroutines await without owning it, the asyncio Future of hermes
type busyFuture struct {
	mu        sync.Mutex
	done      chan struct{}
	err       error
	cancelled bool
}

func newBusyFuture() *busyFuture { return &busyFuture{done: make(chan struct{})} }

// Settles the future once with an error or a cancellation, false when it was already settled
func (f *busyFuture) settle(err error, cancelled bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.done:
		return false
	default:
	}
	f.err, f.cancelled = err, cancelled
	close(f.done)
	return true
}

// Whether the future has settled
func (f *busyFuture) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Settled outcome, context canceled for a cancelled future
func (f *busyFuture) result() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled {
		return context.Canceled
	}
	return f.err
}

// Waits for the settled outcome unless the caller's context ends first, leaving the future untouched
func (f *busyFuture) wait(ctx context.Context) error {
	select {
	case <-f.done:
		return f.result()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Whether an error is the cancellation of a task, the asyncio CancelledError of hermes
func isCancelled(err error) bool { return errors.Is(err, context.Canceled) }

// Waits until every channel closes, returning the context error when the caller is cancelled first
func waitAll(ctx context.Context, pending []<-chan struct{}) error {
	for _, ch := range pending {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type busyAdmissionKey struct{}

// Futures of one busy dispatch, released once its input queues and once its row commits
type busyAdmission struct {
	accepted, committed *busyFuture
}

// Busy caller futures the messaging admission settles, both nil outside a busy shared runtime dispatch
func busyAdmissionOf(ctx context.Context) (accepted, committed *busyFuture) {
	a, _ := ctx.Value(busyAdmissionKey{}).(*busyAdmission)
	if a == nil {
		return nil, nil
	}
	return a.accepted, a.committed
}

// Per adapter shared runtime burst state with held bursts, commit tails, uncommitted inputs and a deferred persist
type busyIngressState struct {
	mu             sync.Mutex
	bursts         map[string]*busyBurst
	commits        map[string]*adapterTask
	uncommitted    map[*busyFuture]struct{}
	durablePersist func(ctx context.Context) error
}

// Busy arrival accessor view of a message event for the observability busy metrics
type busyEventView struct{ e *MessageEvent }

func busyEventOf(e *MessageEvent) busyEventView { return busyEventView{e} }

func (v busyEventView) BusyArrival() *observability.Arrival {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return v.e.busyArrival
}

func (v busyEventView) SetBusyArrival(a *observability.Arrival) {
	eventTurnStateMu.Lock()
	v.e.busyArrival = a
	eventTurnStateMu.Unlock()
}

func (v busyEventView) Internal() bool { return v.e.Internal }

func (v busyEventView) Command() string {
	cmd, _ := v.e.GetCommand()
	return cmd
}

func (v busyEventView) Source() any { return v.e.Source }

func (v busyEventView) ChatID() any {
	if v.e.Source == nil {
		return nil
	}
	return v.e.Source.ChatID
}

// Copy of the event over a copy of its source with only the dataclass fields, as dataclasses replace builds it
func replaceMessageEvent(event *MessageEvent) *MessageEvent {
	gatewayAcceptedMu.Lock()
	eventTurnStateMu.Lock()
	copied := *event
	eventTurnStateMu.Unlock()
	gatewayAcceptedMu.Unlock()
	copied.gatewayAccepted, copied.OnCommit, copied.admittedBeforeDrain = false, nil, false
	copied.notificationReplyMuted, copied.pendingSTTText, copied.pendingSTTTranscripts = nil, nil, nil
	copied.turnMarkerHandoff, copied.activeTurnToken, copied.markerReleased = false, nil, nil
	copied.heartbeatSessionID, copied.lastChunkLen, copied.busyArrival = nil, 0, nil
	if event.Source != nil {
		source := *event.Source
		source.identity, source.transportAdapterRef = nil, nil
		source.authorizationProfileHome, source.rejectedRouteProfile = "", nil
		copied.Source = &source
	}
	return &copied
}

// Moves the durable turn marker from one event onto another, clearing it on the source
func moveMarker(source, target *MessageEvent) {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	if source.activeTurnToken != nil {
		target.activeTurnToken = source.activeTurnToken
		source.activeTurnToken = nil
	}
}

// Sends a turn reply, true when nothing was owed or at least one send succeeded
func (b *BasePlatformAdapter) deliverResponse(ctx context.Context, event *MessageEvent, sessionKey string, response any) (bool, error) {
	unwrapped, ttl := b.unwrapEphemeral(ctx, response)
	text := handlerResponseText(unwrapped)
	if text == "" {
		return true, nil
	}
	extracted, err := b.extractResponseContent(ctx, text, event, sessionKey, ttl > 0)
	if err != nil {
		return false, err
	}
	metadata := markNotifyMetadata(threadMetadataForEvent(event))
	var results []SendResult
	record := func(result SendResult) { results = append(results, result) }
	if extracted.textContent != "" {
		if err := b.sendFinalText(ctx, event, sessionKey, extracted.textContent, metadata, ttl > 0, ttl, record); err != nil {
			return false, err
		}
	}
	if err := b.deliverAttachments(ctx, event, extracted, metadata, len(results) > 0, record); err != nil {
		return false, err
	}
	for _, result := range results {
		if result.Success {
			return true, nil
		}
	}
	return false, nil
}

// Returns once a busy input is queued, or committed when durable, while a tracked observer delivers its reply
func dispatchSharedBusy(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, sessionKey string, durable bool) error {
	b := adapter.Base()
	deliveryEvent := replaceMessageEvent(event)
	accepted, committed := newBusyFuture(), newBusyFuture()
	tctx := context.WithValue(ctx, busyAdmissionKey{}, &busyAdmission{accepted: accepted, committed: committed})
	task := spawnAdapterTask(tctx, func(tctx context.Context) error {
		return b.observeSharedBusy(tctx, adapter, event, deliveryEvent, sessionKey, accepted)
	})
	b.trackBackgroundTask(task)
	cancelled := func() error {
		if !accepted.isDone() {
			task.cancel()
		}
		return ctx.Err()
	}
	select {
	case <-accepted.done:
	case <-task.done:
	case <-ctx.Done():
		return cancelled()
	}
	if durable && !task.finished() {
		select {
		case <-committed.done:
		case <-task.done:
		case <-ctx.Done():
			return cancelled()
		}
	}
	if task.finished() {
		return task.err
	}
	if !committed.isDone() {
		holdUntilCommitted(b, committed, task)
	}
	return nil
}

// Runs the handler for a busy input and delivers its reply, failures after acceptance notified to the chat
func (b *BasePlatformAdapter) observeSharedBusy(ctx context.Context, adapter PlatformAdapter, event, deliveryEvent *MessageEvent, sessionKey string, accepted *busyFuture) (retErr error) {
	event.setTurnMarkerHandoff(b.GatewayRunner != nil)
	fctx := context.WithoutCancel(ctx)
	defer func() {
		observability.SettleBusyArrival(ctx, busyEventOf(event), false)
		if err := b.releaseTurnMarker(fctx, deliveryEvent); err != nil {
			retErr = err
			return
		}
		if err := b.releaseTurnMarker(fctx, event); err != nil {
			retErr = err
		}
	}()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		b.mu.Lock()
		handler := b.messageHandler
		b.mu.Unlock()
		if handler == nil {
			return errors.New("TypeError: 'NoneType' object is not callable")
		}
		response, err := handler(ctx, event)
		if err != nil {
			return err
		}
		moveMarker(event, deliveryEvent)
		deliveryEvent.setTurnMarkerHandoff(event.TurnMarkerHandoff())
		_, err = b.deliverResponse(ctx, deliveryEvent, sessionKey, response)
		return err
	}()
	if err == nil {
		return nil
	}
	if isCancelled(err) || !accepted.isDone() {
		return err
	}
	slog.Error(fmt.Sprintf("[%s] Queued busy follow-up failed: %v", adapter.Name(), err), "error", err)
	b.notifyTurnError(ctx, deliveryEvent, err)
	return nil
}

// Holds an input released before its commit until it commits or its handler ends, for a serial reader's next command
func holdUntilCommitted(b *BasePlatformAdapter, committed *busyFuture, task *adapterTask) {
	settled := newBusyFuture()
	bi := &b.busyIngress
	bi.mu.Lock()
	lazyAttr(&bi.uncommitted)[settled] = struct{}{}
	bi.mu.Unlock()
	go func() {
		select {
		case <-committed.done:
		case <-task.done:
		}
		settled.settle(nil, false)
		bi.mu.Lock()
		delete(bi.uncommitted, settled)
		bi.mu.Unlock()
	}()
}

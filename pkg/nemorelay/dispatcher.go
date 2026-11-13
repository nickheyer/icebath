package nemorelay

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/nemorelay/logging"
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const runtimeLogTarget = "nemo_relay.runtime"

// Rewrites a queued event on the publication path before sanitizers run
type eventTransformFn func(ctx context.Context, event *types.Event) *types.Event

type dispatcherMessageKind int

const (
	messageDeliver dispatcherMessageKind = iota
	messageFlush
	messageRegisterPending
	messageCompletePending
	messageBarrier
)

// One unit of work on the serial publication path
type dispatcherMessage struct {
	kind           dispatcherMessageKind
	event          *types.Event
	transform      eventTransformFn
	injectors      []registryRecord[EventMetadataInjectorFn]
	sanitizers     []registryRecord[EventSanitizeFn]
	subscribers    []EventSubscriberFn
	scopeStack     *ScopeStack
	permit         *publicationPermit
	completion     chan struct{}
	done           chan struct{}
	includePending bool
	lineage        *publicationLineage
	publications   chan []*dispatcherMessage
}

// Group of publications a flush waits on until none are outstanding
type publicationLineage struct {
	outstanding     atomic.Int64
	pendingTerminal bool
}

// One outstanding publication counted against its lineage until released
type publicationPermit struct {
	lineage  *publicationLineage
	released atomic.Bool
}

func newPublicationPermit(lineage *publicationLineage) *publicationPermit {
	lineage.outstanding.Add(1)
	return &publicationPermit{lineage: lineage}
}

func (p *publicationPermit) release() {
	if p != nil && p.released.CompareAndSwap(false, true) {
		p.lineage.outstanding.Add(-1)
	}
}

// Collects publications made inside event middleware so they publish right after the event
type PublicationBuffer struct {
	mu       sync.Mutex
	messages []*dispatcherMessage
	active   bool
	lineage  *publicationLineage
}

func newPublicationBuffer(ctx context.Context, active bool) *PublicationBuffer {
	return &PublicationBuffer{active: active, lineage: currentPublicationLineage(ctx)}
}

func (b *PublicationBuffer) push(message *dispatcherMessage) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.active {
		return false
	}
	b.messages = append(b.messages, message)
	return true
}

func (b *PublicationBuffer) take() []*dispatcherMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	messages := b.messages
	b.messages = nil
	b.active = false
	return messages
}

func (b *PublicationBuffer) isActive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active
}

func publicationBufferFromContext(ctx context.Context) *PublicationBuffer {
	if ctx == nil {
		return nil
	}
	buffer, _ := ctx.Value(publicationBufferKey).(*PublicationBuffer)
	return buffer
}

// Returns the active nested publication buffer of the caller
func CaptureNestedPublicationBuffer(ctx context.Context) *PublicationBuffer {
	if buffer := publicationBufferFromContext(ctx); buffer != nil && buffer.isActive() {
		return buffer
	}
	return nil
}

// Routes publications made with the returned context into the buffer
func WithNestedPublicationBuffer(ctx context.Context, buffer *PublicationBuffer) context.Context {
	return context.WithValue(ctx, publicationBufferKey, buffer)
}

func withPublicationLineage(ctx context.Context, lineage *publicationLineage) context.Context {
	return context.WithValue(ctx, publicationLineageKey, lineage)
}

// Resolves the lineage new publications join from the caller bindings
func currentPublicationLineage(ctx context.Context) *publicationLineage {
	if buffer := publicationBufferFromContext(ctx); buffer != nil && buffer.lineage != nil {
		return buffer.lineage
	}
	if ctx != nil {
		if lineage, ok := ctx.Value(publicationLineageKey).(*publicationLineage); ok {
			return lineage
		}
	}
	if binding := currentDeliveryBinding(); binding != nil {
		return binding.lineage
	}
	return nil
}

// Attaches a lineage permit to delivers and barriers and returns their lineage
func attachPublicationLineage(ctx context.Context, message *dispatcherMessage, inherited *publicationLineage) *publicationLineage {
	current := inherited
	if current == nil {
		current = currentPublicationLineage(ctx)
	}
	if current == nil {
		current = &publicationLineage{}
	}
	switch message.kind {
	case messageDeliver, messageBarrier:
		if message.permit != nil {
			return message.permit.lineage
		}
		message.permit = newPublicationPermit(current)
	}
	return current
}

// Unbounded FIFO feeding the single dispatcher goroutine
type dispatcherQueue struct {
	mu       sync.Mutex
	ready    *sync.Cond
	messages []*dispatcherMessage
}

var (
	dispatcherOnce     sync.Once
	dispatcherInstance atomic.Pointer[dispatcherQueue]
)

// Starts the dispatcher on first use and returns its queue
func dispatcherSender() *dispatcherQueue {
	dispatcherOnce.Do(func() {
		queue := &dispatcherQueue{}
		queue.ready = sync.NewCond(&queue.mu)
		dispatcherInstance.Store(queue)
		started := make(chan struct{})
		go runDispatcher(queue, started)
		<-started
		logging.Info(runtimeLogTarget, "Subscriber dispatcher started", map[string]any{"event": "subscriber_dispatcher_started"})
	})
	return dispatcherInstance.Load()
}

func dispatcherStarted() *dispatcherQueue {
	return dispatcherInstance.Load()
}

func (q *dispatcherQueue) send(message *dispatcherMessage) {
	q.mu.Lock()
	q.messages = append(q.messages, message)
	q.mu.Unlock()
	q.ready.Signal()
}

func (q *dispatcherQueue) receive() *dispatcherMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.messages) == 0 {
		q.ready.Wait()
	}
	message := q.messages[0]
	q.messages[0] = nil
	q.messages = q.messages[1:]
	return message
}

func sendDispatchMessage(ctx context.Context, message *dispatcherMessage) bool {
	attachPublicationLineage(ctx, message, nil)
	dispatcherSender().send(message)
	return true
}

// Queues a message into the caller's nested buffer or onto the dispatcher
func enqueueDispatchMessage(ctx context.Context, message *dispatcherMessage) bool {
	attachPublicationLineage(ctx, message, nil)
	if buffer := publicationBufferFromContext(ctx); buffer != nil && buffer.push(message) {
		return true
	}
	return sendDispatchMessage(ctx, message)
}

// Stamps propagation identity and W3C headers from the emitting stack and active event
func preparePublication(ctx context.Context, event *types.Event, stack *ScopeStack) {
	event.SetPropagationRootUUID(stack.eventPropagationRootUUID())
	event.SetPropagationParentUUID(stack.eventPropagationParentUUID())
	var traceparent, tracestate *string
	resolved := false
	if uuidPtrEqual(event.ParentUUID(), activeEventUUID(ctx)) {
		if active := activeEventTraceContext(ctx); active != nil {
			value := active.traceparent
			traceparent, tracestate = &value, cloneStringPtr(active.tracestate)
			resolved = true
		}
	}
	if !resolved {
		traceparent, tracestate = stack.eventW3CHeaders(event.ParentUUID())
	}
	event.SetPropagationTraceparent(traceparent)
	event.SetPropagationTracestate(tracestate)
}

func newDeliverMessage(ctx context.Context, event *types.Event, transform eventTransformFn, sanitizers []registryRecord[EventSanitizeFn], subscribers []EventSubscriberFn, stack *ScopeStack) *dispatcherMessage {
	snapshot := stack.Snapshot()
	preparePublication(ctx, event, snapshot)
	return &dispatcherMessage{
		kind:        messageDeliver,
		event:       event,
		transform:   transform,
		injectors:   snapshotEventMetadataInjectors(snapshot),
		sanitizers:  sanitizers,
		subscribers: append([]EventSubscriberFn(nil), subscribers...),
		scopeStack:  snapshot,
	}
}

// Queues an event for sanitization and delivery to a subscriber snapshot
func dispatchSanitizedEvent(ctx context.Context, event *types.Event, sanitizers []registryRecord[EventSanitizeFn], subscribers []EventSubscriberFn, stack *ScopeStack) bool {
	if len(subscribers) == 0 {
		return true
	}
	return enqueueDispatchMessage(ctx, newDeliverMessage(ctx, event, nil, sanitizers, subscribers, stack))
}

// Queues an event and returns a receipt completed once subscribers saw it
func dispatchSanitizedEventWithDelivery(ctx context.Context, event *types.Event, sanitizers []registryRecord[EventSanitizeFn], subscribers []EventSubscriberFn, stack *ScopeStack) (*SubscriberDelivery, error) {
	if len(subscribers) == 0 {
		return completedSubscriberDelivery(), nil
	}
	message := newDeliverMessage(ctx, event, nil, sanitizers, subscribers, stack)
	completion := make(chan struct{})
	message.completion = completion
	if !enqueueDispatchMessage(ctx, message) {
		return nil, internalError("failed to queue tracked subscriber delivery")
	}
	return &SubscriberDelivery{completion: completion}, nil
}

// Queues an event whose position in the publication order was already reserved
func dispatchReservedSanitizedEvent(ctx context.Context, event *types.Event, sanitizers []registryRecord[EventSanitizeFn], subscribers []EventSubscriberFn, stack *ScopeStack) bool {
	return dispatchSanitizedEvent(ctx, event, sanitizers, subscribers, stack)
}

// Queues an event that an asynchronous transform completes on the publication path
func dispatchTransformedEvent(ctx context.Context, event *types.Event, transform eventTransformFn, sanitizers []registryRecord[EventSanitizeFn], subscribers []EventSubscriberFn, stack *ScopeStack) bool {
	return enqueueDispatchMessage(ctx, newDeliverMessage(ctx, event, transform, sanitizers, subscribers, stack))
}

// Receipt for one tracked subscriber delivery
type SubscriberDelivery struct {
	completion chan struct{}
}

func completedSubscriberDelivery() *SubscriberDelivery {
	completion := make(chan struct{})
	close(completion)
	return &SubscriberDelivery{completion: completion}
}

// Blocks until every subscriber of the tracked event returned
func (d *SubscriberDelivery) Wait(ctx context.Context) error {
	select {
	case <-d.completion:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ordered slot reserved for publications produced later by background work
type asyncPublication struct {
	sender chan []*dispatcherMessage
	once   sync.Once
}

func (p *asyncPublication) publish(messages []*dispatcherMessage) {
	p.once.Do(func() {
		p.sender <- messages
		close(p.sender)
	})
}

func (p *asyncPublication) abandon() {
	p.once.Do(func() { close(p.sender) })
}

// Reserves the current position in the publication order for later events
func registerAsyncPublication(ctx context.Context) *asyncPublication {
	publications := make(chan []*dispatcherMessage, 1)
	enqueueDispatchMessage(ctx, &dispatcherMessage{kind: messageBarrier, publications: publications})
	return &asyncPublication{sender: publications}
}

// Keeps a flush waiting until in-flight managed work publishes its terminal events
type pendingPublication struct {
	queue  *dispatcherQueue
	permit *publicationPermit
	once   sync.Once
}

func registerPendingPublication() *pendingPublication {
	queue := dispatcherSender()
	lineage := &publicationLineage{pendingTerminal: true}
	permit := newPublicationPermit(lineage)
	queue.send(&dispatcherMessage{kind: messageRegisterPending, lineage: lineage})
	return &pendingPublication{queue: queue, permit: permit}
}

// Hands the permit back to the dispatcher so waiting flushes can complete
func (p *pendingPublication) complete() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.queue.send(&dispatcherMessage{kind: messageCompletePending, permit: p.permit})
	})
}

// Runs work whose publications join the pending lineage and then completes it
func withPendingPublication(ctx context.Context, pending *pendingPublication, work func(context.Context)) {
	if pending == nil {
		work(ctx)
		return
	}
	defer pending.complete()
	work(withPublicationLineage(ctx, pending.permit.lineage))
}

// Buffers publications of background work and releases them at the reserved slot
func withAsyncPublicationContext(ctx context.Context, publication *asyncPublication, work func(context.Context)) {
	if publicationBufferFromContext(ctx) != nil {
		work(ctx)
		if publication != nil {
			publication.abandon()
		}
		return
	}
	buffer := newPublicationBuffer(ctx, publication != nil)
	defer func() {
		if publication != nil {
			publication.publish(buffer.take())
		}
	}()
	work(WithNestedPublicationBuffer(ctx, buffer))
}

// Runs background publication work off the caller
func spawnBackgroundPublication(work func()) {
	go work()
}

// Reports whether the caller runs inside subscriber delivery or event middleware
func InDispatcherCallback(ctx context.Context) bool {
	if currentDeliveryBinding() != nil {
		return true
	}
	return publicationBufferFromContext(ctx) != nil
}

// Waits until every event queued before the call, and in-flight managed work, reached subscribers
func FlushSubscribers(ctx context.Context) error {
	return flushSubscribersInner(ctx, true)
}

// Waits only for publications already queued, not for in-flight managed work
func flushQueuedSubscribers(ctx context.Context) error {
	return flushSubscribersInner(ctx, false)
}

func flushSubscribersInner(ctx context.Context, includePending bool) error {
	if InDispatcherCallback(ctx) {
		return nil
	}
	queue := dispatcherStarted()
	if queue == nil {
		return nil
	}
	if lineage := currentPublicationLineage(ctx); lineage != nil && lineage.pendingTerminal {
		includePending = false
	}
	done := make(chan struct{})
	queue.send(&dispatcherMessage{kind: messageFlush, done: done, includePending: includePending})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type pendingFlush struct {
	done     chan struct{}
	lineages []*publicationLineage
}

type dispatcherLoopState struct {
	activeLineages []*publicationLineage
	pendingFlushes []pendingFlush
}

func (s *dispatcherLoopState) register(lineage *publicationLineage) {
	for _, active := range s.activeLineages {
		if active == lineage {
			return
		}
	}
	s.activeLineages = append(s.activeLineages, lineage)
}

func (s *dispatcherLoopState) deferOrCompleteFlush(done chan struct{}, includePending bool) {
	waiting := make([]*publicationLineage, 0)
	for _, lineage := range s.activeLineages {
		if (includePending || !lineage.pendingTerminal) && lineage.outstanding.Load() > 0 {
			waiting = append(waiting, lineage)
		}
	}
	if len(waiting) == 0 {
		close(done)
		return
	}
	s.pendingFlushes = append(s.pendingFlushes, pendingFlush{done: done, lineages: waiting})
}

// Releases flushes in order once every lineage they wait on drained
func (s *dispatcherLoopState) completeReadyFlushes() {
	retained := s.activeLineages[:0]
	for _, lineage := range s.activeLineages {
		if lineage.outstanding.Load() > 0 {
			retained = append(retained, lineage)
		}
	}
	clear(s.activeLineages[len(retained):])
	s.activeLineages = retained
	ready := 0
	for _, flush := range s.pendingFlushes {
		drained := true
		for _, lineage := range flush.lineages {
			if lineage.outstanding.Load() != 0 {
				drained = false
				break
			}
		}
		if !drained {
			break
		}
		ready++
	}
	for _, flush := range s.pendingFlushes[:ready] {
		close(flush.done)
	}
	s.pendingFlushes = s.pendingFlushes[ready:]
}

func runDispatcher(queue *dispatcherQueue, started chan struct{}) {
	dispatcherGoroutine.Store(goroutineID())
	close(started)
	state := &dispatcherLoopState{}
	for {
		handleDispatcherMessage(queue.receive(), state, nil)
		state.completeReadyFlushes()
	}
}

func handleDispatcherMessage(message *dispatcherMessage, state *dispatcherLoopState, inherited *publicationLineage) {
	switch message.kind {
	case messageFlush:
		state.deferOrCompleteFlush(message.done, message.includePending)
		return
	case messageRegisterPending:
		state.register(message.lineage)
		return
	case messageCompletePending:
		state.register(message.permit.lineage)
		message.permit.release()
		return
	}
	lineage := attachPublicationLineage(context.Background(), message, inherited)
	state.register(lineage)
	switch message.kind {
	case messageDeliver:
		nested := deliverEvent(message, lineage)
		message.permit.release()
		for _, publication := range nested {
			handleDispatcherMessage(publication, state, lineage)
		}
		if message.completion != nil {
			close(message.completion)
		}
	case messageBarrier:
		if publications, ok := <-message.publications; ok {
			for _, publication := range publications {
				handleDispatcherMessage(publication, state, lineage)
			}
		}
		message.permit.release()
	}
	state.completeReadyFlushes()
}

// Sanitizes one event and hands it to each subscriber of its snapshot
func deliverEvent(message *dispatcherMessage, lineage *publicationLineage) []*dispatcherMessage {
	binding := &deliveryBinding{stack: message.scopeStack, lineage: lineage}
	previous := activeDelivery.Swap(binding)
	defer activeDelivery.Store(previous)
	middleware := WithScopeStack(withPublicationLineage(context.Background(), lineage), message.scopeStack)
	event, nested := sanitizeEventSnapshot(middleware, message.event, message.transform, message.injectors, message.sanitizers)
	if event == nil {
		return nested
	}
	if scopeType := event.ScopeType(); scopeType != nil && *scopeType == types.ScopeTypeTool {
		traceContext, err := traceContextForManagedSpan(middleware, event.UUID(), event.ParentUUID())
		if err != nil {
			traceContext = nil
		}
		stack, anchor := scopeStackIdentityAndAnchor(middleware)
		toolBinding := &deliveryBinding{
			stack:   message.scopeStack,
			lineage: lineage,
			activeEvent: &threadActiveEventBinding{
				event:        &anchoredActiveEvent{eventUUID: event.UUID(), scopeStack: stack, anchorScopeUUID: anchor},
				traceContext: traceContext,
			},
		}
		activeDelivery.Store(toolBinding)
		deliverToSubscribers(message.subscribers, event)
		activeDelivery.Store(binding)
	} else {
		deliverToSubscribers(message.subscribers, event)
	}
	return nested
}

func deliverToSubscribers(subscribers []EventSubscriberFn, event *types.Event) {
	for _, subscriber := range subscribers {
		callSubscriber(subscriber, event)
	}
}

func callSubscriber(subscriber EventSubscriberFn, event *types.Event) {
	defer func() {
		if recover() != nil {
			logging.Error(runtimeLogTarget, "Event subscriber callback panicked", map[string]any{"event": "subscriber_callback_panicked"})
		}
	}()
	subscriber(event)
}

// Runs middleware with a nested buffer and returns what it published
func runWithNestedPublicationBuffer[T any](ctx context.Context, work func(context.Context) T) (value T, panicked bool, nested []*dispatcherMessage) {
	buffer := newPublicationBuffer(ctx, true)
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		value = work(WithNestedPublicationBuffer(ctx, buffer))
	}()
	return value, panicked, buffer.take()
}

// Applies the transform, metadata injectors and sanitizers to a queued event
func sanitizeEventSnapshot(ctx context.Context, event *types.Event, transform eventTransformFn, injectors []registryRecord[EventMetadataInjectorFn], sanitizers []registryRecord[EventSanitizeFn]) (*types.Event, []*dispatcherMessage) {
	rootUUID := event.PropagationRootUUID()
	parentUUID := event.PropagationParentUUID()
	traceparent := event.PropagationTraceparent()
	tracestate := event.PropagationTracestate()
	nested := make([]*dispatcherMessage, 0)
	transformed := event
	if transform != nil {
		result, panicked, published := runWithNestedPublicationBuffer(ctx, func(ctx context.Context) *types.Event {
			return transform(ctx, event)
		})
		nested = append(nested, published...)
		if panicked || result == nil {
			logging.Error(runtimeLogTarget, "Event transform panicked; dropping the event", map[string]any{"event": "event_transform_panicked"})
			return nil, nested
		}
		transformed = result
	}
	transformed.SetPropagationRootUUID(rootUUID)
	transformed.SetPropagationParentUUID(parentUUID)
	transformed.SetPropagationTraceparent(traceparent)
	transformed.SetPropagationTracestate(tracestate)
	injected := transformed
	if len(injectors) > 0 {
		fallback := transformed.Clone()
		result, panicked, published := runWithNestedPublicationBuffer(ctx, func(ctx context.Context) *types.Event {
			return eventMetadataInjectionChain(ctx, transformed, injectors)
		})
		nested = append(nested, published...)
		if panicked {
			logging.Error(runtimeLogTarget, "Event metadata injector chain panicked; continuing without injection", map[string]any{"event": "event_metadata_injector_chain_panicked"})
			injected = fallback
		} else {
			injected = result
		}
	}
	if len(sanitizers) == 0 {
		return injected, nested
	}
	fallback := injected.Clone()
	sanitized, panicked, published := runWithNestedPublicationBuffer(ctx, func(ctx context.Context) *types.Event {
		return eventSanitizeChain(ctx, injected, sanitizers)
	})
	nested = append(nested, published...)
	if panicked {
		logging.Error(runtimeLogTarget, "Event sanitizer panicked; clearing observability fields", map[string]any{"event": "event_sanitizer_panicked"})
		fallback.ApplySanitizeFields(types.EventSanitizeFields{})
		return fallback, nested
	}
	return sanitized, nested
}

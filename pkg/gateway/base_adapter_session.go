package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Guards the admission acceptance flag that admission writes and the turn owner reads across goroutines
var gatewayAcceptedMu sync.Mutex

// Quiet window and hard cap of the queue mode busy text debounce in seconds
const (
	DefaultBusyTextDebounceSeconds = 0.35
	DefaultBusyTextHardCapSeconds  = 1.0
)

// Buffered queue mode busy text burst of one session with its flush timer
type TextDebounceState struct {
	Event   *MessageEvent
	Task    *adapterTask
	FirstTS float64
	LastTS  float64
}

// Cancels the pending flush timer when it is live and is not the given task
func (s *TextDebounceState) CancelTimer(unless *adapterTask) {
	if s.Task != nil && s.Task != unless && !s.Task.finished() {
		s.Task.cancel()
	}
}

// Runner exposing its session authority, the shared runtime owning non command input when present
type SessionAuthorityHolder interface {
	SessionAuthority() *SessionAuthority
}

// Installed by the process wiring over pkg/commands, the busy policy of a typed command name
var CommandBusyPolicy func(name string) (bypassActiveSession, interruptThenDispatch bool)

// Busy policy of a command name through the wiring hook, which panics when the process never installed it
func commandBusyPolicy(name string) (bool, bool) {
	if CommandBusyPolicy == nil {
		panic("gateway.CommandBusyPolicy unassigned: the process wiring installs commands.ShouldBypassActiveSession and commands.IsInterruptThenDispatch")
	}
	return CommandBusyPolicy(name)
}

// Pending clarify prompt lookup of a session, swapped by tests the way hermes tests patch clarify_gateway
var pendingClarifyForSession = func(sessionKey string) bool {
	return tools.GetPendingClarifyForSession(sessionKey, true) != nil
}

// Installs a handler bound to its owner so the shared runtime check can read the owner's session authority
func (b *BasePlatformAdapter) SetBoundMessageHandler(owner any, handler MessageHandler) {
	b.mu.Lock()
	b.messageHandler = handler
	b.messageHandlerOwner = owner
	b.mu.Unlock()
}

// Installs the busy follow up policy the runner configures, mode plus debounce window and hard cap
func (b *BasePlatformAdapter) SetBusyTextPolicy(mode string, debounceSeconds, hardCapSeconds float64) {
	b.mu.Lock()
	b.busyTextMode, b.busyTextDebounceSeconds, b.busyTextHardCapSeconds = mode, debounceSeconds, hardCapSeconds
	b.mu.Unlock()
}

// Installs the human delay range in milliseconds, nil keeping the configured env policy
func (b *BasePlatformAdapter) SetHumanDelayRangeMs(r *[2]int) {
	b.mu.Lock()
	b.humanDelayRangeMs = r
	b.mu.Unlock()
}

// Whether the session currently holds an active processing guard
func (b *BasePlatformAdapter) HasActiveSession(sessionKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.activeSessions[sessionKey]
	return ok
}

// Whether normal text is eligible for the queue mode busy debounce
func (b *BasePlatformAdapter) isQueueTextDebounceCandidate(event *MessageEvent) bool {
	b.mu.Lock()
	mode := b.busyTextMode
	b.mu.Unlock()
	result := mode == "queue" && event.MessageType == MessageTypeText && !event.Internal && !event.IsCommand() &&
		pystr.Strip(event.Text) != ""
	if result {
		slog.Debug(fmt.Sprintf("[%s] Queue-text debounce candidate accepted: session=%s text_len=%d", b.Self().Name(), "?",
			pystr.Len(event.Text)))
	}
	return result
}

// Sender identity of a debounce event, platform plus sender or dm chat, nil when anonymous
func textDebounceIdentity(candidate *MessageEvent) []string {
	source := candidate.Source
	if source == nil {
		return nil
	}
	platform := platformName(source.Platform)
	sender := ""
	if source.UserIDAlt != nil && *source.UserIDAlt != "" {
		sender = *source.UserIDAlt
	} else if source.UserID != nil {
		sender = *source.UserID
	}
	if sender != "" {
		return []string{platform, sender}
	}
	if (source.ChatType == "dm" || source.ChatType == "private") && source.ChatID != "" {
		return []string{platform, "dm", source.ChatID}
	}
	return nil
}

// Whether two text debounce events came from the same sender
func (b *BasePlatformAdapter) canMergeTextDebounceEvents(existing, event *MessageEvent) bool {
	a, c := textDebounceIdentity(existing), textDebounceIdentity(event)
	if a == nil || len(a) != len(c) {
		return false
	}
	for i := range a {
		if a[i] != c[i] {
			return false
		}
	}
	return true
}

// Remaining bounded debounce delay of a session, zero when nothing is buffered
func (b *BasePlatformAdapter) textDebounceDelay(sessionKey string) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.textDebounceDelayLocked(sessionKey)
}

func (b *BasePlatformAdapter) textDebounceDelayLocked(sessionKey string) float64 {
	state := b.textDebounce[sessionKey]
	if state == nil {
		return 0
	}
	deadline := min(state.LastTS+b.busyTextDebounceSeconds, state.FirstTS+b.busyTextHardCapSeconds)
	return max(0, deadline-pythread.Monotonic())
}

// Buffers queue mode busy text and schedules a bounded flush, flushing first when the sender changed
func (b *BasePlatformAdapter) queueTextDebounce(ctx context.Context, sessionKey string, event *MessageEvent) {
	b.mu.Lock()
	state := b.textDebounce[sessionKey]
	b.mu.Unlock()
	if state != nil && !b.canMergeTextDebounceEvents(state.Event, event) {
		b.flushTextDebounceNow(ctx, sessionKey)
		b.mu.Lock()
		state = b.textDebounce[sessionKey]
		b.mu.Unlock()
		if state != nil && !b.canMergeTextDebounceEvents(state.Event, event) {
			b.mu.Lock()
			existing := b.pendingMessages[sessionKey]
			b.mu.Unlock()
			if existing != nil && b.canMergeTextDebounceEvents(existing, event) {
				b.mu.Lock()
				MergePendingMessageEvent(lazyAttr(&b.pendingMessages), sessionKey, event, true)
				b.mu.Unlock()
			}
			return
		}
	}
	now := pythread.Monotonic()
	b.mu.Lock()
	defer b.mu.Unlock()
	state = b.textDebounce[sessionKey]
	if state == nil {
		state = &TextDebounceState{Event: event, FirstTS: now, LastTS: now}
		lazyAttr(&b.textDebounce)[sessionKey] = state
	} else {
		if event.Text != "" {
			state.Event.Text = appendText(state.Event.Text, event.Text)
		}
		state.Event.AbsorbReplyExpected(event)
		latestAnchor := event.MessageID
		if latestAnchor == nil {
			latestAnchor = event.ReplyToMessageID
		}
		if event.MessageID != nil {
			id := *event.MessageID
			state.Event.MessageID = &id
		}
		if latestAnchor != nil {
			anchor := *latestAnchor
			state.Event.ReplyToMessageID = &anchor
		}
		state.LastTS = now
	}
	state.CancelTimer(nil)
	delay := b.textDebounceDelayLocked(sessionKey)
	state.Task = spawnAdapterTask(ctx, func(tctx context.Context) error {
		b.flushTextDebounce(tctx, sessionKey, delay)
		return nil
	})
}

// Timer task flushing the debounced burst after the delay, clearing its own task slot on exit
func (b *BasePlatformAdapter) flushTextDebounce(ctx context.Context, sessionKey string, delay float64) {
	current := currentAdapterTask(ctx)
	defer func() {
		b.mu.Lock()
		if state := b.textDebounce[sessionKey]; state != nil && state.Task == current {
			state.Task = nil
		}
		b.mu.Unlock()
	}()
	timer := time.NewTimer(time.Duration(delay * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}
	b.flushTextDebounceNow(ctx, sessionKey)
}

// Force flushes one debounced burst into the pending slot, false when nothing moved
func (b *BasePlatformAdapter) flushTextDebounceNow(ctx context.Context, sessionKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.textDebounce[sessionKey]
	if state == nil {
		return false
	}
	state.CancelTimer(currentAdapterTask(ctx))
	state.Task = nil
	if pending := b.pendingMessages[sessionKey]; pending != nil && !b.canMergeTextDebounceEvents(pending, state.Event) {
		return false
	}
	delete(b.textDebounce, sessionKey)
	MergePendingMessageEvent(lazyAttr(&b.pendingMessages), sessionKey, state.Event, true)
	return true
}

// Cancels and drops the session's debounce state and any held shared runtime burst
func (b *BasePlatformAdapter) discardTextDebounce(sessionKey string) {
	b.mu.Lock()
	state := b.textDebounce[sessionKey]
	delete(b.textDebounce, sessionKey)
	b.mu.Unlock()
	if state != nil {
		state.CancelTimer(nil)
	}
	discardBusyBurst(b.Self(), sessionKey)
}

// Releases the session guard, only when it is still the given guard if one is named
func (b *BasePlatformAdapter) releaseSessionGuard(sessionKey string, guard *InterruptEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current, ok := b.activeSessions[sessionKey]
	if !ok || current == nil || (guard != nil && current != guard) {
		return
	}
	delete(b.activeSessions, sessionKey)
}

// Whether the recorded owner task of the session has exited, false when no owner was recorded
func (b *BasePlatformAdapter) sessionTaskIsStale(sessionKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	task := b.sessionTasks[sessionKey]
	return task != nil && task.finished()
}

// Clears a guard whose owner task already exited, true when healed
func (b *BasePlatformAdapter) healStaleSessionLock(sessionKey string) bool {
	if !b.HasActiveSession(sessionKey) || !b.sessionTaskIsStale(sessionKey) {
		return false
	}
	slog.Warn(fmt.Sprintf("[%s] Healing stale session lock for %s (owner task is done/absent)", b.Self().Name(), sessionKey))
	b.mu.Lock()
	delete(b.activeSessions, sessionKey)
	delete(b.pendingMessages, sessionKey)
	delete(b.requeueCounts, sessionKey)
	delete(b.sessionTasks, sessionKey)
	b.mu.Unlock()
	b.discardTextDebounce(sessionKey)
	return true
}

// Spawns the background processing task under a freshly installed session guard
func (b *BasePlatformAdapter) StartSessionProcessing(ctx context.Context, event *MessageEvent, sessionKey string, interruptEvent *InterruptEvent) bool {
	guard := interruptEvent
	if guard == nil {
		guard = NewInterruptEvent()
	}
	b.mu.Lock()
	lazyAttr(&b.activeSessions)[sessionKey] = guard
	b.mu.Unlock()
	self := b.Self()
	ready := make(chan struct{})
	task := spawnAdapterTask(ctx, func(tctx context.Context) error {
		<-ready
		return self.ProcessMessageBackground(tctx, event, sessionKey)
	})
	b.trackSessionTask(sessionKey, task)
	close(ready)
	return true
}

// Records the task as the session owner and tracks it for shutdown until it finishes
func (b *BasePlatformAdapter) trackSessionTask(sessionKey string, task *adapterTask) {
	b.mu.Lock()
	lazyAttr(&b.sessionTasks)[sessionKey] = task
	lazyAttr(&b.backgroundTasks)[task] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-task.done
		b.mu.Lock()
		delete(b.backgroundTasks, task)
		delete(b.expectedCancelledTasks, task)
		b.mu.Unlock()
	}()
}

// Tracks a background task for shutdown until it finishes
func (b *BasePlatformAdapter) trackBackgroundTask(task *adapterTask) {
	b.mu.Lock()
	lazyAttr(&b.backgroundTasks)[task] = struct{}{}
	b.mu.Unlock()
	task.addDoneCallback(func() {
		b.mu.Lock()
		delete(b.backgroundTasks, task)
		b.mu.Unlock()
	})
}

// Cancels in flight processing of one session within five seconds, keeping the guard or pending input on request
func (b *BasePlatformAdapter) CancelSessionProcessing(ctx context.Context, sessionKey string, releaseGuard, discardPending bool) {
	b.mu.Lock()
	delete(b.requeueCounts, sessionKey)
	task := b.sessionTasks[sessionKey]
	delete(b.sessionTasks, sessionKey)
	b.mu.Unlock()
	if task != nil && !task.finished() {
		slog.Debug(fmt.Sprintf("[%s] Cancelling active processing for session %s", b.Self().Name(), sessionKey))
		b.mu.Lock()
		lazyAttr(&b.expectedCancelledTasks)[task] = struct{}{}
		b.mu.Unlock()
		task.cancel()
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-task.done:
			if task.err != nil && !errors.Is(task.err, context.Canceled) {
				slog.Debug(fmt.Sprintf("[%s] Session cancellation raised while unwinding %s: %v", b.Self().Name(), sessionKey, task.err))
			}
		case <-timer.C:
			slog.Warn(fmt.Sprintf("[%s] Cancelled task for %s did not exit within 5s; "+
				"unblocking dispatch and letting the task unwind in the background", b.Self().Name(), sessionKey))
		case <-ctx.Done():
		}
		timer.Stop()
	}
	if discardPending {
		b.mu.Lock()
		delete(b.pendingMessages, sessionKey)
		b.mu.Unlock()
		b.discardTextDebounce(sessionKey)
	}
	if releaseGuard {
		b.releaseSessionGuard(sessionKey, nil)
	}
}

// Releases the command scoped guard of a reset like command and starts any follow up queued meanwhile
func (b *BasePlatformAdapter) drainPendingAfterSessionCommand(ctx context.Context, sessionKey string, commandGuard *InterruptEvent) {
	b.flushTextDebounceNow(ctx, sessionKey)
	b.mu.Lock()
	pending := b.pendingMessages[sessionKey]
	delete(b.pendingMessages, sessionKey)
	b.mu.Unlock()
	b.releaseSessionGuard(sessionKey, commandGuard)
	if pending != nil {
		b.StartSessionProcessing(ctx, pending, sessionKey, nil)
	}
}

// Dispatches a reset like bypass command under its own guard, cancelling the old task after the reply
func (b *BasePlatformAdapter) dispatchActiveSessionCommand(ctx context.Context, event *MessageEvent, sessionKey, cmd string) error {
	slog.Debug(fmt.Sprintf("[%s] Command '/%s' bypassing active-session guard for %s", b.Self().Name(), cmd, sessionKey))
	commandGuard := NewInterruptEvent()
	b.mu.Lock()
	currentGuard := b.activeSessions[sessionKey]
	lazyAttr(&b.activeSessions)[sessionKey] = commandGuard
	b.mu.Unlock()
	err := b.dispatchInlineReply(ctx, event, &cmd)
	if err == nil {
		b.CancelSessionProcessing(ctx, sessionKey, false, false)
	}
	if err != nil {
		b.mu.Lock()
		if b.activeSessions[sessionKey] == commandGuard {
			if _, owned := b.sessionTasks[sessionKey]; owned && currentGuard != nil {
				b.activeSessions[sessionKey] = currentGuard
				b.mu.Unlock()
			} else {
				b.mu.Unlock()
				b.releaseSessionGuard(sessionKey, commandGuard)
			}
		} else {
			b.mu.Unlock()
		}
		return err
	}
	b.drainPendingAfterSessionCommand(ctx, sessionKey, commandGuard)
	return nil
}

// Admits an incoming message, spawning its background turn or routing it as a busy follow up
func (b *BasePlatformAdapter) HandleMessage(ctx context.Context, event *MessageEvent) error {
	self := b.Self()
	event.setGatewayAccepted(false)
	b.mu.Lock()
	handler := b.messageHandler
	if handler == nil {
		logged := b.noMessageHandlerLogged
		b.noMessageHandlerLogged = true
		b.mu.Unlock()
		if !logged {
			slog.Error(fmt.Sprintf("[%s] Dropping inbound message: no gateway message handler "+
				"is installed on this adapter. The adapter is connected and "+
				"can send, but every inbound message is discarded.", self.Name()))
		}
		return nil
	}
	recovery := b.topicRecoveryFn
	b.mu.Unlock()
	if event.AllowGatewayControl {
		CoercePlaintextGatewayCommand(event)
	}
	if b.dropUnresolved(ctx, event) {
		return nil
	}
	expected := ""
	if event.Metadata != nil {
		if v := event.Metadata["gateway_session_key"]; pyrepr.Truthy(v) {
			expected = pystr.Strip(fmt.Sprint(v))
		}
	}
	if expected == "" && recovery != nil && event.Source != nil && event.Source.Platform == PlatformTelegram &&
		event.Source.ChatType == "dm" {
		b.applyTopicRecovery(event)
	}
	sessionKey, err := b.EventSessionKey(ctx, event)
	if err != nil {
		return err
	}
	if expected != "" && sessionKey != expected {
		slog.Warn(fmt.Sprintf("Dropping internally routed event: expected session=%s derived=%s", expected, sessionKey))
		return nil
	}
	behindBurst, err := burstGate(ctx, self, event, sessionKey)
	if err != nil {
		return err
	}
	if b.HasActiveSession(sessionKey) {
		b.healStaleSessionLock(sessionKey)
	}
	if b.HasActiveSession(sessionKey) || behindBurst {
		return b.handleMessageWhileActive(ctx, event, sessionKey)
	}
	event.setGatewayAccepted(b.StartSessionProcessing(ctx, event, sessionKey, nil))
	return nil
}

// Whether a non command input belongs to the shared runtime session authority of the handler's owner
func (b *BasePlatformAdapter) sharedRuntimeOwnsInput(event *MessageEvent) bool {
	b.mu.Lock()
	owner := b.messageHandlerOwner
	b.mu.Unlock()
	holder, ok := owner.(SessionAuthorityHolder)
	if !ok || holder == nil || holder.SessionAuthority() == nil {
		return false
	}
	_, isCommand := event.GetCommand()
	return !isCommand
}

// Adapters whose pending slot may hold the event, this one plus a guard free replacement delivery adapter
func (b *BasePlatformAdapter) pendingRecoveryOwners(ctx context.Context, event *MessageEvent, sessionKey string) []*BasePlatformAdapter {
	owners := []*BasePlatformAdapter{b}
	if b.GatewayRunner == nil {
		return owners
	}
	var delivery PlatformAdapter
	var err error
	if runner, ok := b.GatewayRunner.(DeliveryAdapterRunner); ok {
		delivery, err = runner.DeliveryAdapterFor(ctx, event.Source)
	} else {
		err = fmt.Errorf("AttributeError: '%T' object has no attribute '_delivery_adapter_for'", b.GatewayRunner)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Delivery-adapter lookup failed during pending recovery: %v", b.Self().Name(), err))
		return owners
	}
	if delivery == nil {
		return owners
	}
	if other := delivery.Base(); other != nil && other != b && !other.HasActiveSession(sessionKey) {
		owners = append(owners, other)
	}
	return owners
}

// Routes a message arriving while the session is busy, bypass commands and clarify replies inline, the rest queued
func (b *BasePlatformAdapter) handleMessageWhileActive(ctx context.Context, event *MessageEvent, sessionKey string) error {
	self := b.Self()
	if b.sharedRuntimeOwnsInput(event) {
		return dispatchBusyBurst(ctx, self, event, sessionKey)
	}
	b.canonicalize(ctx, event.Source)
	cmd, _ := event.GetCommand()
	if bypass, interrupt := commandBusyPolicy(cmd); bypass {
		var err error
		if cmd != "" && interrupt {
			b.discardTextDebounce(sessionKey)
			err = b.dispatchActiveSessionCommand(ctx, event, sessionKey, cmd)
		} else {
			slog.Debug(fmt.Sprintf("[%s] Command '/%s' bypassing active-session guard for %s", self.Name(), cmd, sessionKey))
			err = b.dispatchInlineReply(ctx, event, nil)
		}
		if err != nil {
			slog.Error(fmt.Sprintf("[%s] Command '/%s' dispatch failed: %v", self.Name(), cmd, err))
		}
		return nil
	}
	if cmd == "" && event.AllowGatewayControl && pendingClarifyForSession(sessionKey) {
		slog.Debug(fmt.Sprintf("[%s] Routing message to clarify text-intercept for %s", self.Name(), sessionKey))
		if err := b.dispatchInlineReply(ctx, event, nil); err != nil {
			slog.Error(fmt.Sprintf("[%s] Clarify text-intercept dispatch failed: %v", self.Name(), err))
		}
		return nil
	}
	b.mu.Lock()
	busy := b.busySessionHandler
	b.mu.Unlock()
	if busy != nil {
		handled, err := busy(ctx, event, sessionKey)
		if err != nil {
			slog.Error(fmt.Sprintf("[%s] Busy-session handler failed: %v", self.Name(), err))
			handled = event.GatewayAccepted()
		}
		if !b.HasActiveSession(sessionKey) {
			var orphan *MessageEvent
			for _, owner := range b.pendingRecoveryOwners(ctx, event, sessionKey) {
				if orphan = owner.GetPendingMessage(sessionKey); orphan != nil {
					owner.StartSessionProcessing(ctx, orphan, sessionKey, nil)
					break
				}
			}
			if orphan == nil && !handled {
				event.setGatewayAccepted(b.StartSessionProcessing(ctx, event, sessionKey, nil))
				return nil
			}
		}
		if handled {
			return nil
		}
	}
	b.mu.Lock()
	_, occupied := b.pendingMessages[sessionKey]
	b.mu.Unlock()
	if event.Internal && occupied {
		return nil
	}
	if event.MessageType == MessageTypePhoto {
		slog.Debug(fmt.Sprintf("[%s] Queuing photo follow-up for session %s without interrupt", self.Name(), sessionKey))
		b.mu.Lock()
		MergePendingMessageEvent(lazyAttr(&b.pendingMessages), sessionKey, event, false)
		b.mu.Unlock()
		event.setGatewayAccepted(true)
		return nil
	}
	if b.isQueueTextDebounceCandidate(event) {
		b.mu.Lock()
		window := b.busyTextDebounceSeconds
		b.mu.Unlock()
		slog.Debug(fmt.Sprintf("[%s] New text message while session %s is active — "+
			"debouncing follow-up (busy_text_mode=queue, window=%.2fs)", self.Name(), sessionKey, window))
		b.queueTextDebounce(ctx, sessionKey, event)
		return nil
	}
	slog.Debug(fmt.Sprintf("[%s] New message while session %s is active — queuing follow-up "+
		"(no interrupt, will cascade after current turn)", self.Name(), sessionKey))
	b.mu.Lock()
	MergePendingMessageEvent(lazyAttr(&b.pendingMessages), sessionKey, event, event.MessageType == MessageTypeText)
	b.mu.Unlock()
	event.setGatewayAccepted(true)
	return nil
}

// Takes and clears the pending follow up of a session
func (b *BasePlatformAdapter) GetPendingMessage(sessionKey string) *MessageEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	event := b.pendingMessages[sessionKey]
	delete(b.pendingMessages, sessionKey)
	return event
}

// Whether the session's active guard has been signalled to interrupt
func (b *BasePlatformAdapter) HasPendingInterrupt(sessionKey string) bool {
	b.mu.Lock()
	guard := b.activeSessions[sessionKey]
	b.mu.Unlock()
	return guard != nil && guard.IsSet()
}

// Whether the event was accepted into a turn or a pending slot by the last admission
func (e *MessageEvent) GatewayAccepted() bool {
	gatewayAcceptedMu.Lock()
	defer gatewayAcceptedMu.Unlock()
	return e.gatewayAccepted
}

func (e *MessageEvent) setGatewayAccepted(v bool) {
	gatewayAcceptedMu.Lock()
	e.gatewayAccepted = v
	gatewayAcceptedMu.Unlock()
}

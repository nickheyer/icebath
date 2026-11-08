package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
)

// One held same sender busy burst, its merged head, hold window, timer, commit task and absorbed inputs
type busyBurst struct {
	head     *MessageEvent
	done     *busyFuture
	firstTS  float64
	lastTS   float64
	timer    *adapterTask
	commit   *adapterTask
	absorbed []*MessageEvent
}

// Burst kind of a busy input, photo for a non internal photo, text for a queue mode debounce candidate
func (b *BasePlatformAdapter) burstKind(event *MessageEvent) string {
	if event.MessageType == MessageTypePhoto && !event.Internal {
		return "photo"
	}
	if b.isQueueTextDebounceCandidate(event) {
		return "text"
	}
	return ""
}

// Whether the adapter reads its next input only after handle message returns
func serialReceiveLoop(adapter PlatformAdapter) bool { return adapter.Base().Traits.SerialReceiveLoop }

// Whether a burst is held or committing for the route, so later input queues behind it
func holdsBusyBurst(adapter PlatformAdapter, sessionKey string) bool {
	bi := &adapter.Base().busyIngress
	bi.mu.Lock()
	defer bi.mu.Unlock()
	_, held := bi.bursts[sessionKey]
	_, committing := bi.commits[sessionKey]
	return held || committing
}

// Entry check of handle message, a serial reader's command first waits until every input it returned on commits
var burstGate = func(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, sessionKey string) (bool, error) {
	b := adapter.Base()
	if b.sharedRuntimeOwnsInput(event) {
		return holdsBusyBurst(adapter, sessionKey), nil
	}
	cmd, _ := event.GetCommand()
	if cmd == "" || !serialReceiveLoop(adapter) {
		return false, nil
	}
	if _, interrupt := commandBusyPolicy(cmd); interrupt {
		discardBusyBurst(adapter, sessionKey)
	}
	bi := &b.busyIngress
	bi.mu.Lock()
	held := make(map[string]*busyBurst, len(bi.bursts))
	for key, burst := range bi.bursts {
		held[key] = burst
	}
	for key, burst := range held {
		b.flushBurstLocked(ctx, adapter, key, burst)
	}
	commits := make([]<-chan struct{}, 0, len(bi.commits))
	for _, commit := range bi.commits {
		commits = append(commits, commit.done)
	}
	bi.mu.Unlock()
	if err := waitAll(ctx, commits); err != nil {
		return false, err
	}
	bi.mu.Lock()
	uncommitted := make([]<-chan struct{}, 0, len(bi.uncommitted))
	for settled := range bi.uncommitted {
		uncommitted = append(uncommitted, settled.done)
	}
	bi.mu.Unlock()
	if err := waitAll(ctx, uncommitted); err != nil {
		return false, err
	}
	return false, nil
}

// Shared runtime dispatch of one busy input, merging a same sender photo or queue mode text burst first
func dispatchBusyBurst(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, sessionKey string) error {
	b := adapter.Base()
	observability.StampBusyArrival(adapter, busyEventOf(event))
	kind := b.burstKind(event)
	bi := &b.busyIngress
	for {
		bi.mu.Lock()
		burst := bi.bursts[sessionKey]
		if burst == nil {
			break
		}
		if kind != "" && kind == b.burstKind(burst.head) && b.canMergeTextDebounceEvents(burst.head, event) {
			MergePendingMessageEvent(map[string]*MessageEvent{sessionKey: burst.head}, sessionKey, event, kind == "text")
			burst.absorbed = append(burst.absorbed, event)
			observability.NoteBusyMerged(busyEventOf(burst.head), busyEventOf(event))
			burst.lastTS = pythread.Monotonic()
			b.armBurstLocked(ctx, adapter, sessionKey, burst)
			bi.mu.Unlock()
			return burstAccepted(ctx, adapter, burst, event)
		}
		commit := b.flushBurstLocked(ctx, adapter, sessionKey, burst)
		bi.mu.Unlock()
		select {
		case <-commit.done:
			if commit.err != nil {
				return commit.err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	previous := bi.commits[sessionKey]
	if kind != "" && previous != nil && !previous.finished() && serialReceiveLoop(adapter) {
		bi.mu.Unlock()
		if err := waitAll(ctx, []<-chan struct{}{previous.done}); err != nil {
			return err
		}
		return dispatchBusyBurst(ctx, adapter, event, sessionKey)
	}
	if kind == "" {
		bi.mu.Unlock()
		if previous != nil {
			if err := waitAll(ctx, []<-chan struct{}{previous.done}); err != nil {
				return err
			}
		}
		return dispatchSharedBusy(ctx, adapter, event, sessionKey, false)
	}
	now := pythread.Monotonic()
	burst := &busyBurst{head: event, done: newBusyFuture(), firstTS: now, lastTS: now}
	lazyAttr(&bi.bursts)[sessionKey] = burst
	b.armBurstLocked(ctx, adapter, sessionKey, burst)
	bi.mu.Unlock()
	return burstAccepted(ctx, adapter, burst, event)
}

// Waits for the merged admission and copies its acceptance, at once on a serial reader so it reads on
func burstAccepted(ctx context.Context, adapter PlatformAdapter, burst *busyBurst, event *MessageEvent) error {
	if serialReceiveLoop(adapter) {
		return nil
	}
	if err := burst.done.wait(ctx); err != nil {
		return err
	}
	event.setGatewayAccepted(burst.head.GatewayAccepted())
	return nil
}

// Restarts the hold timer at the debounce window after the last input, capped from the first
func (b *BasePlatformAdapter) armBurstLocked(ctx context.Context, adapter PlatformAdapter, sessionKey string, burst *busyBurst) {
	if burst.timer != nil {
		burst.timer.cancel()
	}
	b.mu.Lock()
	debounce, hardCap := b.busyTextDebounceSeconds, b.busyTextHardCapSeconds
	b.mu.Unlock()
	delay := max(0.0, min(burst.lastTS+debounce, burst.firstTS+hardCap)-pythread.Monotonic())
	bi := &b.busyIngress
	burst.timer = spawnAdapterTask(ctx, func(tctx context.Context) error {
		if err := sleepSeconds(tctx, delay); err != nil {
			return err
		}
		bi.mu.Lock()
		if err := tctx.Err(); err != nil {
			bi.mu.Unlock()
			return err
		}
		commit := b.flushBurstLocked(tctx, adapter, sessionKey, burst)
		bi.mu.Unlock()
		select {
		case <-commit.done:
			return commit.err
		case <-tctx.Done():
			return tctx.Err()
		}
	})
	b.trackBackgroundTask(burst.timer)
}

// Starts committing the burst once behind any earlier commit of the route, returning its commit task
func (b *BasePlatformAdapter) flushBurstLocked(ctx context.Context, adapter PlatformAdapter, sessionKey string, burst *busyBurst) *adapterTask {
	if burst.commit != nil {
		return burst.commit
	}
	bi := &b.busyIngress
	if bi.bursts[sessionKey] == burst {
		delete(bi.bursts, sessionKey)
	}
	if burst.timer != currentAdapterTask(ctx) {
		burst.timer.cancel()
	}
	previous := bi.commits[sessionKey]
	commit := spawnAdapterTask(ctx, func(cctx context.Context) error {
		return commitBurst(cctx, adapter, sessionKey, burst, previous)
	})
	burst.commit = commit
	lazyAttr(&bi.commits)[sessionKey] = commit
	b.trackBackgroundTask(commit)
	go func() {
		<-commit.done
		bi.mu.Lock()
		if bi.commits[sessionKey] == commit {
			delete(bi.commits, sessionKey)
		}
		bi.mu.Unlock()
		burst.done.settle(commit.err, isCancelled(commit.err))
	}()
	return commit
}

// Durably dispatches the merged head after the previous commit, a serial reader's failure logged instead
func commitBurst(ctx context.Context, adapter PlatformAdapter, sessionKey string, burst *busyBurst, previous *adapterTask) error {
	if previous != nil {
		if err := waitAll(ctx, []<-chan struct{}{previous.done}); err != nil {
			return err
		}
	}
	err := dispatchSharedBusy(ctx, adapter, burst.head, sessionKey, true)
	if err == nil || isCancelled(err) || !serialReceiveLoop(adapter) {
		return err
	}
	slog.Error(fmt.Sprintf("[%s] Busy burst for %s was not queued", adapter.Name(), sessionKey), "error", err)
	return nil
}

// Channels of inputs the reader returned on that are not committed yet, held bursts, commits and released inputs
func (bi *busyIngressState) undurableLocked() []<-chan struct{} {
	var pending []<-chan struct{}
	for _, burst := range bi.bursts {
		if !burst.done.isDone() {
			pending = append(pending, burst.done.done)
		}
	}
	for _, commit := range bi.commits {
		if !commit.finished() {
			pending = append(pending, commit.done)
		}
	}
	for settled := range bi.uncommitted {
		if !settled.isDone() {
			pending = append(pending, settled.done)
		}
	}
	return pending
}

// Runs persist once every input a serial reader returned on is committed, the newest deferred persist winning
func WhenReturnedInputsDurable(ctx context.Context, adapter PlatformAdapter, persist func(ctx context.Context) error) error {
	b := adapter.Base()
	bi := &b.busyIngress
	bi.mu.Lock()
	if bi.durablePersist != nil {
		bi.durablePersist = persist
		bi.mu.Unlock()
		return nil
	}
	if len(bi.undurableLocked()) == 0 {
		bi.mu.Unlock()
		return persist(ctx)
	}
	bi.durablePersist = persist
	bi.mu.Unlock()
	b.trackBackgroundTask(spawnAdapterTask(ctx, func(tctx context.Context) error {
		for {
			bi.mu.Lock()
			pending := bi.undurableLocked()
			if len(pending) == 0 {
				latest := bi.durablePersist
				bi.durablePersist = nil
				bi.mu.Unlock()
				return latest(tctx)
			}
			bi.mu.Unlock()
			if err := waitAll(tctx, pending); err != nil {
				bi.mu.Lock()
				bi.durablePersist = nil
				bi.mu.Unlock()
				return err
			}
		}
	}))
	return nil
}

// Drops the route's held burst as hermes main dropped its debounce buffer, nothing was acknowledged
func discardBusyBurst(adapter PlatformAdapter, sessionKey string) {
	bi := &adapter.Base().busyIngress
	bi.mu.Lock()
	burst := bi.bursts[sessionKey]
	delete(bi.bursts, sessionKey)
	if burst != nil {
		burst.timer.cancel()
	}
	bi.mu.Unlock()
	if burst == nil {
		return
	}
	observability.SettleBusyArrival(context.Background(), busyEventOf(burst.head), true)
	burst.done.settle(nil, false)
}

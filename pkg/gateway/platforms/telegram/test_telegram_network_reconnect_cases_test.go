package telegram

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Python exception double with a class name, a message and explicit cause and context links
type pyExc struct {
	class, msg     string
	cause, context error
}

func (e *pyExc) Error() string      { return e.msg }
func (e *pyExc) PyTypeName() string { return e.class }
func (e *pyExc) Unwrap() []error {
	var out []error
	if e.cause != nil {
		out = append(out, e.cause)
	}
	if e.context != nil {
		out = append(out, e.context)
	}
	return out
}

func countErr(seen []error, target error) int {
	n := 0
	for _, e := range seen {
		if e == target {
			n++
		}
	}
	return n
}

func TestIterExceptionGraphFlatSingle(t *testing.T) {
	err := &pyExc{class: "ValueError", msg: "boom"}
	if got := iterExceptionGraph(err); len(got) != 1 || got[0] != error(err) {
		t.Fatalf("got %v", got)
	}
}

func TestIterExceptionGraphWalksCauseChain(t *testing.T) {
	root := &pyExc{class: "ValueError", msg: "root"}
	mid := &pyExc{class: "RuntimeError", msg: "mid", cause: root}
	top := &pyExc{class: "Exception", msg: "top", cause: mid}
	seen := iterExceptionGraph(top)
	if !slices.Contains(seen, error(top)) || !slices.Contains(seen, error(mid)) || !slices.Contains(seen, error(root)) {
		t.Fatalf("got %v", seen)
	}
}

func TestIterExceptionGraphWalksContextChain(t *testing.T) {
	root := &pyExc{class: "ValueError", msg: "during handling"}
	top := &pyExc{class: "RuntimeError", msg: "top", context: root}
	seen := iterExceptionGraph(top)
	if !slices.Contains(seen, error(top)) || !slices.Contains(seen, error(root)) {
		t.Fatalf("got %v", seen)
	}
}

func TestIterExceptionGraphCycleGuard(t *testing.T) {
	a := &pyExc{class: "ValueError", msg: "a"}
	b := &pyExc{class: "RuntimeError", msg: "b", cause: a}
	a.cause = b
	seen := iterExceptionGraph(a)
	if countErr(seen, a) != 1 || countErr(seen, b) != 1 {
		t.Fatalf("got %v", seen)
	}
}

func TestIterExceptionGraphDiamondNoDuplicates(t *testing.T) {
	root := &pyExc{class: "ValueError", msg: "root"}
	left := &pyExc{class: "RuntimeError", msg: "left", cause: root}
	right := &pyExc{class: "TypeError", msg: "right", cause: root}
	top := &pyExc{class: "Exception", msg: "top", cause: left, context: right}
	if seen := iterExceptionGraph(top); countErr(seen, root) != 1 {
		t.Fatalf("got %v", seen)
	}
}

func TestPoolTimeoutClassifierPtbPoolTimeoutMessage(t *testing.T) {
	err := &pyExc{class: "Exception", msg: "Pool timeout: All connections in the connection pool are occupied. Request was *not* sent to Telegram."}
	if !looksLikePoolTimeout(err) {
		t.Fatal("want pool timeout")
	}
}

func TestPoolTimeoutClassifierWrappedHttpxPooltimeoutClass(t *testing.T) {
	inner := &pyExc{class: "ConnectionError", msg: "inner"}
	err := &pyExc{class: "Exception", msg: "Timed out", context: inner}
	if looksLikePoolTimeout(err) {
		t.Fatal("want no pool timeout")
	}
}

func TestPoolTimeoutClassifierHttpxPooltimeoutClassName(t *testing.T) {
	err := &pyExc{class: "Exception", msg: "Timed out", cause: &pyExc{class: "FakePoolTimeout", msg: "x"}}
	if !looksLikePoolTimeout(err) {
		t.Fatal("want pool timeout")
	}
}

func TestPoolTimeoutClassifierGenericTimeoutNegative(t *testing.T) {
	if looksLikePoolTimeout(&pyExc{class: "Exception", msg: "Timed out"}) || looksLikePoolTimeout(&pyExc{class: "Exception", msg: "Bad Gateway"}) {
		t.Fatal("want no pool timeout")
	}
}

func TestPoolTimeoutClassifierOccupiedConnectionPoolSubstring(t *testing.T) {
	if !looksLikePoolTimeout(&pyExc{class: "Exception", msg: "All connections in the connection pool are occupied"}) {
		t.Fatal("want pool timeout")
	}
	if looksLikePoolTimeout(&pyExc{class: "Exception", msg: "seat was occupied"}) {
		t.Fatal("occupied alone must not match")
	}
	if looksLikePoolTimeout(&pyExc{class: "Exception", msg: "connection pool sizing"}) {
		t.Fatal("connection pool alone must not match")
	}
}

func TestConnectTimeoutClassifierClassNameMatch(t *testing.T) {
	if !looksLikeConnectTimeout(&pyExc{class: "FakeConnectTimeout", msg: "x"}) {
		t.Fatal("want connect timeout")
	}
}

func TestConnectTimeoutClassifierMessageMatch(t *testing.T) {
	if !looksLikeConnectTimeout(&pyExc{class: "Exception", msg: "connect timeout"}) || !looksLikeConnectTimeout(&pyExc{class: "Exception", msg: "connect timed out"}) {
		t.Fatal("want connect timeout")
	}
}

func TestConnectTimeoutClassifierWrappedInCause(t *testing.T) {
	if !looksLikeConnectTimeout(&pyExc{class: "Exception", msg: "Timed out", cause: &pyExc{class: "FakeConnectTimeout", msg: "x"}}) {
		t.Fatal("want connect timeout")
	}
}

func TestConnectTimeoutClassifierGenericNegative(t *testing.T) {
	if looksLikeConnectTimeout(&pyExc{class: "Exception", msg: "Timed out"}) {
		t.Fatal("want no connect timeout")
	}
}

func TestReconnectSelfSchedulesOnStartPollingFailure(t *testing.T) {
	app := newMockApp()
	var calls atomic.Int32
	app.updater.startFn = func(ctx context.Context, _ bool, _ func(error)) error {
		if calls.Add(1) == 1 {
			return &pyExc{class: "Exception", msg: "Timed out"}
		}
		<-ctx.Done()
		return ctx.Err()
	}
	p, host := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "Bad Gateway"})
	if len(host.pendingTasks()) < 1 {
		t.Fatal("expected a self rescheduled retry task after start_polling failure")
	}
}

func TestInitializeStillRunsWhenShutdownFails(t *testing.T) {
	app := newMockApp()
	app.polling.shutdownFn = func(context.Context) error { return errors.New("shutdown boom") }
	p, _ := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "Bad Gateway"})
	if _, inits := app.polling.counts(); inits != 1 {
		t.Fatalf("initialize calls %d", inits)
	}
	if s, i := app.general.counts(); s != 0 || i != 0 {
		t.Fatalf("general pool touched %d %d", s, i)
	}
	if app.updater.startCount() != 1 {
		t.Fatalf("start_polling calls %d", app.updater.startCount())
	}
}

func TestReconnectDrainsGeneralPoolAfterPoolTimeout(t *testing.T) {
	app := newMockApp()
	p, _ := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception",
		msg: "Pool timeout: All connections in the connection pool are occupied. Request was not sent to Telegram."})
	if s, i := app.general.counts(); s != 1 || i != 1 {
		t.Fatalf("general pool %d %d", s, i)
	}
	if s, i := app.polling.counts(); s != 1 || i != 1 {
		t.Fatalf("polling pool %d %d", s, i)
	}
	if app.updater.startCount() != 1 {
		t.Fatalf("start_polling calls %d", app.updater.startCount())
	}
	completeCurrentPollingGeneration(t, p)
}

func TestGeneralPoolDrainIsBoundedWhenCloseHangs(t *testing.T) {
	app := newMockApp()
	app.general.shutdownFn, app.general.initializeFn = hang, hang
	p, _ := newTestLifecycle(t, app)
	p.timings.drainTimeout = 0.05
	done := make(chan struct{})
	go func() {
		p.drainGeneralConnectionsAfterPoolTimeout(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("general drain did not finish within the bound")
	}
	if s, i := app.general.counts(); s != 1 || i != 1 {
		t.Fatalf("general pool %d %d", s, i)
	}
}

func TestReconnectContinuesIfDrainHangs(t *testing.T) {
	app := newMockApp()
	app.polling.shutdownFn, app.polling.initializeFn = hang, hang
	p, _ := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	p.timings.drainTimeout = 0.01
	done := make(chan struct{})
	go func() {
		p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "Timed out"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect ladder froze on a hung drain")
	}
	if app.updater.startCount() != 1 {
		t.Fatalf("start_polling calls %d", app.updater.startCount())
	}
	if p.networkErrorCount != 2 {
		t.Fatalf("network error count %d", p.networkErrorCount)
	}
	if p.recoveryInFlight() {
		t.Fatal("recovery task must be done")
	}
}

func TestReconnectStopDeadlineDoesNotWaitForCancelCleanup(t *testing.T) {
	app := newMockApp()
	app.polling, app.general = nil, nil
	release := make(chan struct{})
	stopCancelled := make(chan struct{})
	var lifecycleLock sync.Mutex
	app.updater.stopFn = func(ctx context.Context) error {
		lifecycleLock.Lock()
		defer lifecycleLock.Unlock()
		<-ctx.Done()
		close(stopCancelled)
		<-release
		return nil
	}
	app.updater.startFn = func(context.Context, bool, func(error)) error {
		lifecycleLock.Lock()
		defer lifecycleLock.Unlock()
		return nil
	}
	defer close(release)
	p, host := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	p.timings.updaterStopTimeout = 0.01
	done := make(chan struct{})
	go func() {
		p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "Timed out"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("reconnect remained blocked waiting for cancellation-shielded updater.stop() cleanup")
	}
	select {
	case <-stopCancelled:
	case <-time.After(time.Second):
		t.Fatal("stop was never cancelled")
	}
	if !host.HasFatalError() || host.notifyCount() != 1 || app.updater.startCount() != 0 {
		t.Fatalf("fatal %v notifies %d starts %d", host.HasFatalError(), host.notifyCount(), app.updater.startCount())
	}
}

func TestReconnectDrainSurvivesCancellationResistantClose(t *testing.T) {
	app := newMockApp()
	release := make(chan struct{})
	closeCancelled := make(chan struct{})
	app.polling.shutdownFn = func(ctx context.Context) error {
		<-ctx.Done()
		close(closeCancelled)
		<-release
		return ctx.Err()
	}
	defer close(release)
	p, _ := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	p.timings.drainTimeout = 0.05
	done := make(chan struct{})
	go func() {
		p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "Timed out"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect remained blocked waiting for cancellation-shielded polling-pool shutdown() cleanup")
	}
	select {
	case <-closeCancelled:
	case <-time.After(time.Second):
		t.Fatal("drain must have cancelled the close")
	}
	if _, inits := app.polling.counts(); inits != 1 || app.updater.startCount() != 1 || p.recoveryInFlight() {
		t.Fatalf("inits %d starts %d", inits, app.updater.startCount())
	}
	completeCurrentPollingGeneration(t, p)
}

func TestHeartbeatForceEscalatesWedgedRecoveryTask(t *testing.T) {
	app := newMockApp()
	app.updater = nil
	p, host := newTestLifecycle(t, app)
	wedged := gateway.SpawnAdapterTask(context.Background(), hang)
	defer wedged.Cancel()
	p.errorTask = wedged
	p.timings.heartbeatInterval, p.timings.errorTaskStuckTimeout = 0.02, 0.01
	done := make(chan struct{})
	go func() {
		p.pollingHeartbeatLoop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat never escalated")
	}
	if !host.HasFatalError() || host.notifyCount() == 0 {
		t.Fatal("wedged recovery task must force a fatal escalation")
	}
}

func TestConflictRetryAlsoDrainsPollingConnections(t *testing.T) {
	app := newMockApp()
	p, _ := newTestLifecycle(t, app)
	if err := p.handlePollingConflict(context.Background(), &pyExc{class: "Exception", msg: "Conflict: terminated by other getUpdates"}); err != nil {
		t.Fatal(err)
	}
	if s, i := app.polling.counts(); s != 1 || i != 1 || app.updater.startCount() != 1 {
		t.Fatalf("shutdown %d initialize %d starts %d", s, i, app.updater.startCount())
	}
}

// Lifecycle log lines captured for the duration of the test
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHeartbeatProbeReentersLadderWhenUpdaterNotRunning(t *testing.T) {
	logs := captureLogs(t)
	app := newMockApp()
	app.updater.running = false
	p, _ := newTestLifecycle(t, app)
	p.timings.networkBaseDelay, p.timings.networkMaxDelay = 30, 30
	generation, progress := p.beginPollingGeneration()
	p.timings.progressTimeout = 0
	p.verifyPollingAfterReconnect(context.Background(), &generation, progress)
	if app.bot.getMeCalls() != 0 {
		t.Fatal("get_me must not run")
	}
	p.mu.Lock()
	task := p.errorTask
	p.mu.Unlock()
	if task == nil {
		t.Fatal("expected a recovery task")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "Telegram network error (attempt 1/10)") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "Telegram network error (attempt 1/10)") ||
		!strings.Contains(strings.ToLower(logs.String()), "not running") {
		t.Fatalf("ladder not entered with the not running RuntimeError: %s", logs.String())
	}
}

func TestHeartbeatProbeIgnoresAuthErrors(t *testing.T) {
	app := newMockApp()
	app.bot.getMeFn = func(context.Context) (*models.User, error) {
		return nil, &pyExc{class: "InvalidToken", msg: "token revoked"}
	}
	p, _ := newTestLifecycle(t, app)
	generation, progress := p.beginPollingGeneration()
	p.timings.progressTimeout = 0
	p.verifyPollingAfterReconnect(context.Background(), &generation, progress)
	if p.errorTask != nil {
		t.Fatal("auth errors must not schedule recovery")
	}
}

func TestHeartbeatProbeDefersToInflightRecovery(t *testing.T) {
	app := newMockApp()
	app.bot.getMeFn = func(context.Context) (*models.User, error) {
		return nil, &pyExc{class: "ConnectionError", msg: "pool wedged"}
	}
	p, _ := newTestLifecycle(t, app)
	inflight := gateway.SpawnAdapterTask(context.Background(), hang)
	defer inflight.Cancel()
	p.errorTask = inflight
	generation, progress := p.beginPollingGeneration()
	p.timings.progressTimeout = 0
	p.verifyPollingAfterReconnect(context.Background(), &generation, progress)
	if p.errorTask != inflight {
		t.Fatal("in flight recovery must stay the owner")
	}
}

func TestReconnectSchedulesHeartbeatProbeOnSuccess(t *testing.T) {
	app := newMockApp()
	app.polling, app.general = nil, nil
	p, host := newTestLifecycle(t, app)
	p.networkErrorCount = 1
	initial := len(host.tasks)
	p.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "Bad Gateway"})
	if len(host.tasks) <= initial {
		t.Fatal("expected a heartbeat probe task after a successful reconnect start_polling()")
	}
}

func TestHeartbeatLoopSkipsReconnectIfAlreadyInProgress(t *testing.T) {
	app := newMockApp()
	app.updater = nil
	app.bot.getMeFn = func(context.Context) (*models.User, error) { return nil, &deadlineTimeoutError{} }
	p, _ := newTestLifecycle(t, app)
	existing := gateway.SpawnAdapterTask(context.Background(), func(ctx context.Context) error {
		sleepCtx(ctx, 0.2)
		return nil
	})
	defer existing.Cancel()
	p.errorTask = existing
	p.timings.heartbeatInterval = 0.01
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	p.pollingHeartbeatLoop(ctx)
	if p.errorTask != existing || p.networkErrorCount != 0 {
		t.Fatal("heartbeat must not start a second recovery while one is in flight")
	}
}

func TestPollingBootstrapConflictSchedulesConflictRecoveryTask(t *testing.T) {
	app := newMockApp()
	app.updater.startFn = func(context.Context, bool, func(error)) error {
		return &pyExc{class: "Exception", msg: "Conflict: terminated by other getUpdates request"}
	}
	p, host := newTestLifecycle(t, app)
	p.timings.conflictRetryDelayBase = 30
	ok, err := p.startPollingResilient(context.Background(), true, func(error) {}, false)
	if ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
	if len(host.pendingTasks()) == 0 {
		t.Fatal("expected background conflict recovery task")
	}
	if host.HasFatalError() {
		t.Fatal("bootstrap conflict must not go fatal")
	}
}

func TestHandlePollingNetworkErrorUpdaterStopTimeout(t *testing.T) {
	app := newMockApp()
	app.updater.stopFn = func(ctx context.Context) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	p, host := newTestLifecycle(t, app)
	p.timings.updaterStopTimeout = 0.05
	p.handlePollingNetworkError(context.Background(), &osError{msg: "CLOSE-WAIT test"})
	if !host.HasFatalError() || host.notifyCount() != 1 {
		t.Fatal("stop timeout must go fatal and notify once")
	}
	if s, i := app.polling.counts(); s != 0 || i != 0 || app.updater.startCount() != 0 {
		t.Fatal("no drain and no start_polling after a stop timeout")
	}
}

func TestDrainRebuildDoesNotBlockLoopOrLeakCleanupTask(t *testing.T) {
	app := newMockApp()
	app.polling.clientOpen = true
	app.polling.shutdownFn = hang
	app.polling.orphanFn = func(ctx context.Context) error {
		for range 20 {
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	p, host := newTestLifecycle(t, app)
	p.timings.drainTimeout = 0.02
	var ticks atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			ticks.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	p.drainPollingConnections(context.Background())
	close(stop)
	if app.polling.rebuilt == 0 {
		t.Fatal("stale polling client must be replaced")
	}
	if ticks.Load() < 2 {
		t.Fatal("a wedged close must not block other work")
	}
	time.Sleep(350 * time.Millisecond)
	if len(host.pendingTasks()) != 0 {
		t.Fatal("stale-client cleanup must finish or abandon its own wedged close")
	}
}

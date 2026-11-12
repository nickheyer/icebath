package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Bot API calls the polling health probes make on the general request path
type pollingBot interface {
	GetMe(ctx context.Context) (*models.User, error)
	GetWebhookInfo(ctx context.Context) (*models.WebhookInfo, error)
	DeleteWebhook(ctx context.Context, dropPendingUpdates bool) (bool, error)
}

// Long poll lifecycle the recovery ladder drives, the PTB Updater surface
type pollingUpdater interface {
	Running() bool
	StartPolling(ctx context.Context, dropPendingUpdates bool, errorCallback func(error)) error
	Stop(ctx context.Context) error
	DisarmRetryLoop() (disarmed, found bool)
}

// Connection pool of one Bot API request path, the HTTPXRequest surface the drains use
type requestPool interface {
	Shutdown(ctx context.Context) error
	Initialize(ctx context.Context) error
	RebuildClient() (orphan func(ctx context.Context) error, rebuilt bool, err error)
}

// Application surface the polling lifecycle reads, any part nil when absent
type pollingApp interface {
	PollingBot() pollingBot
	Updater() pollingUpdater
	Requests() (polling, general requestPool)
	Shutdown(ctx context.Context) error
}

// Adapter surface the polling lifecycle reports health and fatal errors through
type pollingHost interface {
	Name() string
	HasFatalError() bool
	FatalErrorCode() *string
	SetFatalError(ctx context.Context, code, message string, retryable bool)
	IsConnected() bool
	MarkDegraded(ctx context.Context)
	WriteRuntimeStatusSafe(ctx context.Context, statusContext string, fields map[string]any)
	NotifyFatalError(ctx context.Context) error
	TrackBackgroundTask(task *gateway.AdapterTask)
	NoteBotIdentity(user *models.User)
}

// Timing bounds of the polling lifecycle, the hermes module constants
type pollingTimings struct {
	updaterStopTimeout       float64
	updaterStartTimeout      float64
	initialProgressTimeout   float64
	drainTimeout             float64
	errorTaskStuckTimeout    float64
	progressTimeout          float64
	stallTimeout             float64
	ingressStallHeartbeats   int
	heartbeatInterval        float64
	heartbeatProbeTimeout    float64
	verifierProbeTimeout     float64
	networkBaseDelay         float64
	networkMaxDelay          float64
	maxNetworkRetries        int
	maxConflictRetries       int
	conflictRetryDelayBase   float64
	conflictRetryDelayFactor float64
}

// Hermes polling timings
func defaultPollingTimings() pollingTimings {
	return pollingTimings{updaterStopTimeout: 15, updaterStartTimeout: 30, initialProgressTimeout: 60, drainTimeout: 15,
		errorTaskStuckTimeout: 300, progressTimeout: 60, stallTimeout: 150, ingressStallHeartbeats: 4, heartbeatInterval: 90,
		heartbeatProbeTimeout: 15, verifierProbeTimeout: 10, networkBaseDelay: 5, networkMaxDelay: 60, maxNetworkRetries: 10,
		maxConflictRetries: 5, conflictRetryDelayBase: 10, conflictRetryDelayFactor: 10}
}

// One shot event that wakes every waiter once set, the asyncio Event of hermes
type pollEvent struct {
	once sync.Once
	ch   chan struct{}
}

func newPollEvent() *pollEvent { return &pollEvent{ch: make(chan struct{})} }

func (e *pollEvent) set() { e.once.Do(func() { close(e.ch) }) }

func (e *pollEvent) isSet() bool {
	select {
	case <-e.ch:
		return true
	default:
		return false
	}
}

// Python OSError raised by the lifecycle with its triggering error chained
type osError struct {
	msg   string
	cause error
}

func (e *osError) Error() string      { return e.msg }
func (e *osError) Unwrap() error      { return e.cause }
func (e *osError) PyTypeName() string { return "OSError" }

// Python RuntimeError raised by the lifecycle
type runtimeError struct{ msg string }

func (e *runtimeError) Error() string      { return e.msg }
func (e *runtimeError) PyTypeName() string { return "RuntimeError" }

type pollingGenerationKey struct{}

// Context carrying the polling generation its getUpdates requests belong to, nil clearing it
func withPollingGeneration(ctx context.Context, generation *int) context.Context {
	return context.WithValue(ctx, pollingGenerationKey{}, generation)
}

// Polling generation a request context belongs to, nil outside a polling generation
func pollingGenerationFrom(ctx context.Context) *int {
	g, _ := ctx.Value(pollingGenerationKey{}).(*int)
	return g
}

var processStart = time.Now()

// Monotonic seconds since process start, the time.monotonic of hermes
var monotonicNow = func() float64 { return time.Since(processStart).Seconds() }

// Seconds as a duration
func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Sleeps for d seconds, false when ctx ended first
func sleepCtx(ctx context.Context, d float64) bool {
	timer := time.NewTimer(seconds(d))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Runs fn with a timeout, an empty TimeoutError when the timeout fired first
func waitForTimeout[T any](ctx context.Context, timeout float64, fn func(ctx context.Context) (T, error)) (T, error) {
	tctx, cancel := context.WithTimeout(ctx, seconds(timeout))
	defer cancel()
	type outcome struct {
		v   T
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		v, err := fn(tctx)
		done <- outcome{v, err}
	}()
	select {
	case o := <-done:
		if o.err != nil && tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			var zero T
			return zero, &deadlineTimeoutError{}
		}
		return o.v, o.err
	case <-tctx.Done():
		var zero T
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return zero, &deadlineTimeoutError{}
	}
}

// Polling generations, progress proof, recovery ladders and watchdogs of the getUpdates long poll
type pollingLifecycle struct {
	host    pollingHost
	timings pollingTimings

	mu                         sync.Mutex
	app                        pollingApp
	webhookMode                bool
	teardownStarted            bool
	errorTask                  *gateway.AdapterTask
	verifierTask               *gateway.AdapterTask
	heartbeatTask              *gateway.AdapterTask
	conflictCount              int
	networkErrorCount          int
	generation                 int
	conflictRecoveryGeneration *int
	progress                   *pollEvent
	progressAccepting          bool
	errorCallbackRef           func(error)
	generationStarted          *float64
	lastProgress               *float64
	updatesReceived            int
	updatesDispatched          int
	ingressDispatchedSeen      int
	ingressStalledHeartbeats   int
	pendingStuckCount          int
	notRunningCount            int
	sendPathDegraded           bool
	generalDrainLock           sync.Mutex
	recoveryCtx                context.Context
}

// Polling lifecycle reporting through the host with the hermes timings
func newPollingLifecycle(host pollingHost) *pollingLifecycle {
	return &pollingLifecycle{host: host, timings: defaultPollingTimings(), progress: newPollEvent(),
		recoveryCtx: context.Background()}
}

// Live application, nil while torn down
func (p *pollingLifecycle) currentApp() pollingApp {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.app
}

// Whether disconnect fenced polling
func (p *pollingLifecycle) isTeardownStarted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.teardownStarted
}

// Whether connect succeeded but no getUpdates round trip is proven yet
func (p *pollingLifecycle) isSendPathDegraded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sendPathDegraded
}

// Marks the send path degraded until polling proves progress again
func (p *pollingLifecycle) setSendPathDegraded() {
	p.mu.Lock()
	p.sendPathDegraded = true
	p.mu.Unlock()
}

// Releases a half built app's transports after an abandoned init, shutting each request pool directly
func shutdownAbandonedApp(ctx context.Context, app pollingApp) {
	if app == nil {
		return
	}
	if err := app.Shutdown(ctx); err != nil {
		slog.Debug("Abandoned Telegram app.shutdown() failed", "error", err)
	}
	pollingReq, generalReq := app.Requests()
	for _, request := range []requestPool{pollingReq, generalReq} {
		if request == nil {
			continue
		}
		if err := request.Shutdown(ctx); err != nil {
			slog.Debug("Abandoned Telegram request shutdown failed", "error", err)
		}
	}
}

// Resets the getUpdates pool before a reconnect, swapping in a fresh client when the close hangs
func (p *pollingLifecycle) drainPollingConnections(ctx context.Context) {
	app := p.currentApp()
	if app == nil || app.PollingBot() == nil {
		return
	}
	pollingReq, _ := app.Requests()
	if pollingReq == nil {
		return
	}
	if !p.boundedRequestStep(ctx, pollingReq.Shutdown, "Polling request shutdown failed/timed out (non-fatal)") {
		p.orphanAndRebuildPollingClient(ctx, pollingReq)
	}
	if p.boundedRequestStep(ctx, pollingReq.Initialize, "Polling request re-initialize failed/timed out (non-fatal)") {
		slog.Debug(fmt.Sprintf("[%s] Polling request pool drained before reconnect", p.host.Name()))
	} else {
		p.orphanAndRebuildPollingClient(ctx, pollingReq)
	}
}

// Runs one request shutdown or initialize under the drain deadline, false after a debug log on failure
func (p *pollingLifecycle) boundedRequestStep(ctx context.Context, step func(ctx context.Context) error, failureMsg string) bool {
	_, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return nil, step(ctx) },
		p.timings.drainTimeout, nil, "telegram", true)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] "+failureMsg, p.host.Name()), "error", err)
		return false
	}
	return true
}

// Swaps a wedged polling client for a fresh one and closes the old one in a detached bounded task
func (p *pollingLifecycle) orphanAndRebuildPollingClient(ctx context.Context, pollingReq requestPool) {
	orphan, rebuilt, err := pollingReq.RebuildClient()
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to rebuild polling HTTP client after hung drain", p.host.Name()), "error", err)
		return
	}
	if !rebuilt {
		return
	}
	slog.Warn(fmt.Sprintf("[%s] Replaced wedged getUpdates HTTP client after drain timeout (likely CLOSE-WAIT socket)", p.host.Name()))
	task := gateway.SpawnAdapterTask(ctx, func(ctx context.Context) error {
		if orphan == nil {
			return nil
		}
		if _, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return nil, orphan(ctx) },
			p.timings.drainTimeout, nil, "telegram", true); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Orphan polling client aclose failed (non-fatal)", p.host.Name()), "error", err)
		}
		return nil
	})
	p.host.TrackBackgroundTask(task)
}

// Closes polling to progress and marks the send path degraded, caller holding the lock
func (p *pollingLifecycle) fencePollingLocked() {
	p.progressAccepting = false
	p.sendPathDegraded = true
}

// Starts accepting progress for a new getUpdates generation, the fenced current one during teardown
func (p *pollingLifecycle) beginPollingGeneration() (int, *pollEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.teardownStarted {
		p.fencePollingLocked()
		return p.generation, p.progress
	}
	if p.verifierTask != nil && !p.verifierTask.Done() {
		p.verifierTask.Cancel()
	}
	p.verifierTask = nil
	p.generation++
	p.progress = newPollEvent()
	p.progressAccepting = true
	p.sendPathDegraded = true
	started := monotonicNow()
	p.generationStarted = &started
	p.lastProgress = nil
	p.updatesReceived, p.updatesDispatched = 0, 0
	p.ingressDispatchedSeen, p.ingressStalledHeartbeats = 0, 0
	return p.generation, p.progress
}

// Records a getUpdates round trip of the current generation, true when accepted
func (p *pollingLifecycle) recordPollingProgress(ctx context.Context, generation int) bool {
	p.mu.Lock()
	if p.teardownStarted || !p.progressAccepting || generation != p.generation {
		p.mu.Unlock()
		return false
	}
	if !p.progress.isSet() {
		state := "confirmed healthy"
		if p.networkErrorCount != 0 {
			state = "recovered"
		}
		slog.Info(fmt.Sprintf("[%s] Telegram polling %s: getUpdates progressing (generation %d)", p.host.Name(), state, generation))
	}
	p.progress.set()
	now := monotonicNow()
	p.lastProgress = &now
	p.networkErrorCount = 0
	if p.conflictRecoveryGeneration != nil && generation == *p.conflictRecoveryGeneration {
		p.conflictRecoveryGeneration = nil
	} else {
		p.conflictCount = 0
	}
	wasDegraded := p.sendPathDegraded
	p.sendPathDegraded = false
	p.mu.Unlock()
	if wasDegraded && p.host.IsConnected() && !p.host.HasFatalError() {
		p.host.WriteRuntimeStatusSafe(ctx, "connected", map[string]any{"platform_state": "connected", "error_code": nil,
			"error_message": nil})
	}
	return true
}

// Records getUpdates progress from an observed response without touching what the caller parses
func (p *pollingLifecycle) observePollingRequestResult(ctx context.Context, generation *int, statusCode int, payload []byte) {
	if generation == nil || !(200 <= statusCode && statusCode < 300) {
		return
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return
	}
	if string(envelope["ok"]) != "true" {
		return
	}
	result, ok := envelope["result"]
	if !ok {
		return
	}
	if p.recordPollingProgress(ctx, *generation) {
		p.recordUpdatesReceived(result)
	}
}

// Counts updates Telegram handed over on the getUpdates wire for the accepted generation
func (p *pollingLifecycle) recordUpdatesReceived(result json.RawMessage) {
	var updates []json.RawMessage
	if err := json.Unmarshal(result, &updates); err != nil || len(updates) == 0 {
		return
	}
	p.mu.Lock()
	p.updatesReceived += len(updates)
	p.mu.Unlock()
}

// Counts one update the dispatcher admitted, the ingress side of the backlog accounting
func (p *pollingLifecycle) recordUpdateDispatched() {
	p.mu.Lock()
	p.updatesDispatched++
	p.mu.Unlock()
}

// Starts one polling generation and schedules its progress verifier, returning the generation bound to it
func (p *pollingLifecycle) startPollingOnce(ctx context.Context, app pollingApp, dropPendingUpdates bool, errorCallback func(error),
	abandonAppOnTimeout, scheduleVerifier bool) (int, *pollEvent, error) {
	abort := &pollingLifecycleAbort{msg: "Telegram polling teardown started"}
	if p.isTeardownStarted() {
		return 0, nil, abort
	}
	generation, progress := p.beginPollingGeneration()
	p.mu.Lock()
	accepting := p.progressAccepting
	p.mu.Unlock()
	if !accepting {
		return 0, nil, abort
	}
	generationCallback := func(err error) {
		p.mu.Lock()
		stale := p.teardownStarted || generation != p.generation
		p.mu.Unlock()
		if stale || errorCallback == nil {
			return
		}
		errorCallback(err)
	}
	var onAbandon func(ctx context.Context) error
	if abandonAppOnTimeout {
		onAbandon = func(ctx context.Context) error {
			shutdownAbandonedApp(ctx, app)
			return nil
		}
	}
	gen := generation
	pollCtx := withPollingGeneration(context.WithoutCancel(ctx), &gen)
	_, err := awaitWithThreadDeadline(ctx, func(context.Context) (any, error) {
		return nil, app.Updater().StartPolling(pollCtx, dropPendingUpdates, generationCallback)
	}, p.timings.updaterStartTimeout, onAbandon, "telegram", true)
	if err != nil {
		return 0, nil, err
	}
	if p.isTeardownStarted() {
		p.mu.Lock()
		p.fencePollingLocked()
		p.mu.Unlock()
		return 0, nil, abort
	}
	if scheduleVerifier {
		p.schedulePollingProgressVerifier(ctx, generation, progress)
	}
	return generation, progress, nil
}

// Owns exactly one tracked progress verifier for the current generation
func (p *pollingLifecycle) schedulePollingProgressVerifier(ctx context.Context, generation int, progress *pollEvent) {
	p.mu.Lock()
	if p.teardownStarted {
		p.fencePollingLocked()
		p.mu.Unlock()
		return
	}
	if previous := p.verifierTask; previous != nil && !previous.Done() {
		previous.Cancel()
	}
	task := gateway.SpawnAdapterTask(ctx, func(ctx context.Context) error {
		p.verifyPollingAfterReconnect(ctx, &generation, progress)
		return nil
	})
	p.verifierTask = task
	p.mu.Unlock()
	p.host.TrackBackgroundTask(task)
	task.AddDoneCallback(func() {
		p.mu.Lock()
		if p.verifierTask == task {
			p.verifierTask = nil
		}
		p.mu.Unlock()
	})
}

// Resets the general Bot API pool after a confirmed pool timeout, when the request was never sent
func (p *pollingLifecycle) drainGeneralConnectionsAfterPoolTimeout(ctx context.Context) {
	app := p.currentApp()
	if app == nil || app.PollingBot() == nil {
		return
	}
	_, generalReq := app.Requests()
	if generalReq == nil {
		return
	}
	p.generalDrainLock.Lock()
	defer p.generalDrainLock.Unlock()
	p.boundedRequestStep(ctx, generalReq.Shutdown, "General request shutdown failed/timed out after pool timeout (non-fatal)")
	if p.boundedRequestStep(ctx, generalReq.Initialize, "General request re-initialize failed/timed out after pool timeout (non-fatal)") {
		slog.Warn(fmt.Sprintf("[%s] General request pool drained after Telegram pool timeout", p.host.Name()))
	}
}

// Starts fn as the tracked in flight recovery task, the reentrancy guard of every recovery path
func (p *pollingLifecycle) spawnPollingRecovery(fn func(ctx context.Context) error) *gateway.AdapterTask {
	p.mu.Lock()
	ctx := p.recoveryCtx
	task := gateway.SpawnAdapterTask(ctx, fn)
	p.errorTask = task
	p.mu.Unlock()
	p.host.TrackBackgroundTask(task)
	return task
}

// Whether a recovery task is still running
func (p *pollingLifecycle) recoveryInFlight() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errorTask != nil && !p.errorTask.Done()
}

// Schedules background polling recovery so a transient failure degrades only this adapter
func (p *pollingLifecycle) schedulePollingRecovery(ctx context.Context, err error, reason string) {
	if p.isTeardownStarted() || p.host.HasFatalError() {
		return
	}
	if p.recoveryInFlight() {
		slog.Debug(fmt.Sprintf("[%s] Telegram polling recovery already scheduled; ignoring %s: %s", p.host.Name(), reason,
			redactTelegramErrorText(err)))
		return
	}
	p.setSendPathDegraded()
	if p.host.IsConnected() {
		p.host.MarkDegraded(ctx)
	}
	if _, stall := err.(*PollingStallError); stall {
		slog.Warn(fmt.Sprintf("[%s] Telegram polling stall confirmed (%s); handing off to the supervisor for an adapter rebuild. Error: %s",
			p.host.Name(), reason, redactTelegramErrorText(err)))
	} else {
		slog.Warn(fmt.Sprintf("[%s] Telegram polling degraded (%s); gateway stays alive and will retry. Error: %s", p.host.Name(),
			reason, redactTelegramErrorText(err)))
	}
	p.spawnPollingRecovery(func(ctx context.Context) error {
		p.handlePollingNetworkError(ctx, err)
		return nil
	})
}

// Clears a stale webhook, raising on cold start so the runner disposes the partial adapter
func (p *pollingLifecycle) deleteStaleWebhook(ctx context.Context, requireSuccess bool) (bool, error) {
	app := p.currentApp()
	if app == nil || app.PollingBot() == nil {
		return false, nil
	}
	b := app.PollingBot()
	_, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return b.DeleteWebhook(ctx, false) },
		p.timings.updaterStartTimeout, nil, "telegram", true)
	if err == nil {
		return true, nil
	}
	if !looksLikeNetworkError(err) {
		return false, err
	}
	if requireSuccess {
		return false, &osError{msg: "Telegram deleteWebhook did not complete during initial connect", cause: err}
	}
	slog.Warn(fmt.Sprintf("[%s] deleteWebhook failed with a recoverable network error; continuing to polling so getUpdates/retry can recover: %s",
		p.host.Name(), redactTelegramErrorText(err)))
	p.setSendPathDegraded()
	return false, nil
}

// Waits for this generation's first getUpdates success or first polling error on cold start
func (p *pollingLifecycle) awaitColdStartReadiness(ctx context.Context, progress, strictErrorEvent *pollEvent, strictError func() error) error {
	_, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) {
		select {
		case <-progress.ch:
		case <-strictErrorEvent.ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, nil
	}, p.timings.initialProgressTimeout, nil, "telegram", true)
	if err != nil {
		if _, timeout := err.(*deadlineTimeoutError); timeout {
			return &osError{msg: fmt.Sprintf("Telegram getUpdates made no progress within %.0fs during initial connect — failing startup so the gateway retries with a fresh adapter (#67498)",
				p.timings.initialProgressTimeout), cause: err}
		}
		return err
	}
	if first := strictError(); first != nil && !progress.isSet() {
		return &osError{msg: "Telegram polling errored before first getUpdates success during initial connect: " +
			redactTelegramErrorText(first), cause: first}
	}
	if !progress.isSet() {
		return &osError{msg: "Telegram getUpdates did not become ready during initial connect"}
	}
	return nil
}

// Starts polling, demanding real readiness on cold start and recovering in the background otherwise
func (p *pollingLifecycle) startPollingResilient(ctx context.Context, dropPendingUpdates bool, errorCallback func(error), requireProgress bool) (bool, error) {
	if p.isTeardownStarted() {
		return false, nil
	}
	app := p.currentApp()
	if app == nil || app.Updater() == nil {
		return false, &runtimeError{msg: "Telegram application/updater not initialized"}
	}
	var strictMu sync.Mutex
	var strictErr error
	strictErrorEvent := newPollEvent()
	strictGateOpen := true
	effective := errorCallback
	if requireProgress {
		effective = func(err error) {
			strictMu.Lock()
			open := strictGateOpen
			if open && strictErr == nil {
				strictErr = err
			}
			strictMu.Unlock()
			if !open {
				if errorCallback != nil {
					errorCallback(err)
				}
				return
			}
			strictErrorEvent.set()
		}
	}
	_, progress, err := p.startPollingOnce(ctx, app, dropPendingUpdates, effective, requireProgress, !requireProgress)
	if err == nil && requireProgress {
		err = p.awaitColdStartReadiness(ctx, progress, strictErrorEvent, func() error {
			strictMu.Lock()
			defer strictMu.Unlock()
			return strictErr
		})
		if err == nil {
			strictMu.Lock()
			strictGateOpen = false
			strictMu.Unlock()
			p.mu.Lock()
			p.errorCallbackRef = errorCallback
			p.mu.Unlock()
		}
	}
	if err == nil {
		return true, nil
	}
	if _, abort := err.(*pollingLifecycleAbort); abort {
		return false, nil
	}
	if p.isTeardownStarted() {
		return false, nil
	}
	if requireProgress {
		return false, err
	}
	if looksLikePollingConflict(err) {
		slog.Warn(fmt.Sprintf("[%s] Telegram polling bootstrap conflict; gateway stays alive while conflict retry runs: %s",
			p.host.Name(), redactTelegramErrorText(err)))
		p.spawnPollingRecovery(func(ctx context.Context) error { return p.handlePollingConflict(ctx, err) })
		return false, nil
	}
	if looksLikeNetworkError(err) {
		p.schedulePollingRecovery(ctx, err, "polling bootstrap")
		return false, nil
	}
	return false, err
}

// Retryable telegram_network_error fatal handed to the runner so the supervisor rebuilds the adapter
func (p *pollingLifecycle) goFatalNetwork(ctx context.Context, message, logMessage string) {
	slog.Error(logMessage)
	p.host.SetFatalError(ctx, "telegram_network_error", message, true)
	p.handoffPollingFatalError(ctx)
}

// Bounded updater stop before a recovery restart, false once it went fatal and the caller must return
func (p *pollingLifecycle) stopUpdaterOrGoFatal(ctx context.Context, app pollingApp, what string) bool {
	if app == nil || app.Updater() == nil || !app.Updater().Running() {
		return true
	}
	updater := app.Updater()
	_, err := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return nil, updater.Stop(ctx) },
		p.timings.updaterStopTimeout, nil, "telegram", true)
	if _, timeout := err.(*deadlineTimeoutError); timeout {
		message := fmt.Sprintf("Telegram updater.stop() did not finish before the %s deadline; rebuilding the adapter instead of reusing an Updater whose lifecycle lock may still be held.", what)
		p.goFatalNetwork(ctx, message, fmt.Sprintf("[%s] %s (likely CLOSE-WAIT socket)", p.host.Name(), message))
		return false
	}
	return true
}

// Runs a recovery function as the tracked in flight recovery task
func (p *pollingLifecycle) restartPollingInTask(fn func(ctx context.Context) error) {
	p.mu.Lock()
	ctx := p.recoveryCtx
	task := gateway.SpawnAdapterTask(ctx, fn)
	p.errorTask = task
	p.mu.Unlock()
}

// Reconnects polling after a transient network error with exponential backoff, then goes retryable fatal
func (p *pollingLifecycle) handlePollingNetworkError(ctx context.Context, err error) {
	if p.isTeardownStarted() || p.host.HasFatalError() {
		return
	}
	if _, stall := err.(*PollingStallError); stall {
		message := fmt.Sprintf("Telegram polling stall confirmed (%s); rebuilding the adapter instead of reusing a wedged Updater/dispatcher in place.",
			redactTelegramErrorText(err))
		p.goFatalNetwork(ctx, message, fmt.Sprintf("[%s] %s (rebuilding adapter via supervisor)", p.host.Name(), message))
		return
	}
	maxRetries := p.timings.maxNetworkRetries
	p.mu.Lock()
	p.networkErrorCount++
	p.sendPathDegraded = true
	attempt := p.networkErrorCount
	p.mu.Unlock()
	if attempt > maxRetries {
		message := fmt.Sprintf("Telegram polling could not reconnect after %d network error retries. Escalating to gateway recovery.", maxRetries)
		p.goFatalNetwork(ctx, message, fmt.Sprintf("[%s] %s Last error: %s", p.host.Name(), message, redactTelegramErrorText(err)))
		return
	}
	delay := math.Min(p.timings.networkBaseDelay*math.Pow(2, float64(attempt-1)), p.timings.networkMaxDelay)
	slog.Warn(fmt.Sprintf("[%s] Telegram network error (attempt %d/%d), reconnecting in %ds. Error: %s", p.host.Name(), attempt,
		maxRetries, int(delay), redactTelegramErrorText(err)))
	if !sleepCtx(ctx, delay) || p.isTeardownStarted() {
		return
	}
	app := p.currentApp()
	if !p.stopUpdaterOrGoFatal(ctx, app, "network-recovery") || p.isTeardownStarted() {
		return
	}
	if looksLikePoolTimeout(err) {
		p.drainGeneralConnectionsAfterPoolTimeout(ctx)
	}
	if p.isTeardownStarted() {
		return
	}
	p.drainPollingConnections(ctx)
	if p.isTeardownStarted() {
		return
	}
	var retryErr error
	if app == nil {
		retryErr = &runtimeError{msg: "Telegram application was torn down during reconnect"}
	} else {
		p.mu.Lock()
		callback := p.errorCallbackRef
		p.mu.Unlock()
		_, _, retryErr = p.startPollingOnce(ctx, app, false, callback, false, true)
	}
	if retryErr == nil {
		slog.Info(fmt.Sprintf("[%s] Telegram polling restarted after network error (attempt %d); health pending getUpdates progress",
			p.host.Name(), attempt))
		return
	}
	if _, abort := retryErr.(*pollingLifecycleAbort); abort || p.isTeardownStarted() || ctx.Err() != nil {
		return
	}
	slog.Warn(fmt.Sprintf("[%s] Telegram polling reconnect failed: %s", p.host.Name(), redactTelegramErrorText(retryErr)))
	if !p.host.HasFatalError() && !p.isTeardownStarted() {
		task := gateway.SpawnAdapterTask(ctx, func(ctx context.Context) error {
			p.handlePollingNetworkError(ctx, retryErr)
			return nil
		})
		p.host.TrackBackgroundTask(task)
		p.mu.Lock()
		p.errorTask = task
		p.mu.Unlock()
	}
}

// Probes the general path periodically and escalates wedged recovery, stuck queues and polling stalls
func (p *pollingLifecycle) pollingHeartbeatLoop(ctx context.Context) {
	var stuckTaskRef *gateway.AdapterTask
	stuckTaskSince := 0.0
	for {
		if !sleepCtx(ctx, p.timings.heartbeatInterval) {
			return
		}
		if p.isTeardownStarted() || p.host.HasFatalError() {
			return
		}
		p.mu.Lock()
		recoveryTask := p.errorTask
		p.mu.Unlock()
		if recoveryTask != nil && !recoveryTask.Done() {
			now := monotonicNow()
			if recoveryTask != stuckTaskRef {
				stuckTaskRef, stuckTaskSince = recoveryTask, now
			} else if now-stuckTaskSince > p.timings.errorTaskStuckTimeout {
				stuckFor := now - stuckTaskSince
				slog.Error(fmt.Sprintf("[%s] Telegram reconnect task wedged for %.0fs with no ladder progress; forcing retryable-fatal so the gateway reconnects instead of staying silently deaf.",
					p.host.Name(), stuckFor))
				recoveryTask.Cancel()
				p.host.SetFatalError(ctx, "telegram_network_error",
					fmt.Sprintf("Telegram reconnect task wedged for %.0fs; forcing gateway reconnect.", stuckFor), true)
				p.handoffPollingFatalError(ctx)
				return
			}
		} else {
			stuckTaskRef = nil
		}
		app := p.currentApp()
		var b pollingBot
		if app != nil {
			b = app.PollingBot()
		}
		if b == nil {
			continue
		}
		probeErr := p.heartbeatProbe(ctx, b)
		if probeErr == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if className(probeErr) == "TimeoutError" || isOSError(probeErr) || looksLikeNetworkError(probeErr) {
			p.schedulePollingRecovery(ctx, probeErr, "heartbeat probe")
		}
	}
}

// One heartbeat round of getMe, the pending update probe and the stall checks
func (p *pollingLifecycle) heartbeatProbe(ctx context.Context, b pollingBot) error {
	user, err := waitForTimeout(ctx, p.timings.heartbeatProbeTimeout, b.GetMe)
	if err != nil {
		return err
	}
	p.host.NoteBotIdentity(user)
	if err := p.probePendingUpdates(ctx, b, p.timings.heartbeatProbeTimeout); err != nil {
		return err
	}
	p.checkPollingStall(ctx)
	p.checkIngressDispatchStall(ctx)
	return nil
}

// Detects a wedged or stopped getUpdates consumer through pending_update_count and updater state
func (p *pollingLifecycle) probePendingUpdates(ctx context.Context, b pollingBot, probeTimeout float64) error {
	p.mu.Lock()
	skip := p.teardownStarted || p.webhookMode
	p.mu.Unlock()
	if skip {
		return nil
	}
	if p.recoveryInFlight() {
		p.mu.Lock()
		p.notRunningCount = 0
		p.mu.Unlock()
		return nil
	}
	app := p.currentApp()
	var updater pollingUpdater
	if app != nil {
		updater = app.Updater()
	}
	if updater == nil {
		p.mu.Lock()
		p.pendingStuckCount = 0
		p.mu.Unlock()
		return nil
	}
	if !updater.Running() {
		p.mu.Lock()
		p.pendingStuckCount = 0
		p.notRunningCount++
		count := p.notRunningCount
		p.mu.Unlock()
		slog.Warn(fmt.Sprintf("[%s] Telegram polling heartbeat: updater stopped while in polling mode (stuck probe %d/2)", p.host.Name(), count))
		if count >= 2 {
			p.mu.Lock()
			p.notRunningCount = 0
			p.mu.Unlock()
			p.escalateStuckConsumer("[%s] Telegram updater is not running (long-poll task gone); triggering polling restart",
				"Telegram updater stopped while in polling mode")
		}
		return nil
	}
	p.mu.Lock()
	p.notRunningCount = 0
	p.mu.Unlock()
	info, err := waitForTimeout(ctx, probeTimeout, b.GetWebhookInfo)
	if err != nil {
		if className(err) == "TimeoutError" || isOSError(err) {
			return nil
		}
		return err
	}
	pending := 0
	if info != nil {
		pending = info.PendingUpdateCount
	}
	p.mu.Lock()
	if pending <= 0 {
		p.pendingStuckCount = 0
		p.mu.Unlock()
		return nil
	}
	p.pendingStuckCount++
	count := p.pendingStuckCount
	p.mu.Unlock()
	slog.Warn(fmt.Sprintf("[%s] Telegram polling heartbeat: %d update(s) queued but not consumed (stuck probe %d/2)", p.host.Name(),
		pending, count))
	if count >= 2 {
		p.mu.Lock()
		p.pendingStuckCount = 0
		p.mu.Unlock()
		p.escalateStuckConsumer("[%s] getUpdates consumer appears wedged (queue not draining); triggering polling restart",
			"getUpdates consumer wedged: pending updates not draining")
	}
	return nil
}

// Restarts polling through the network error ladder after the second consecutive stuck probe
func (p *pollingLifecycle) escalateStuckConsumer(logMessage, reason string) {
	if p.isTeardownStarted() {
		return
	}
	slog.Warn(fmt.Sprintf(logMessage, p.host.Name()))
	p.restartPollingInTask(func(ctx context.Context) error {
		p.handlePollingNetworkError(ctx, &runtimeError{msg: reason})
		return nil
	})
}

// Hands a dispatcher that stopped admitting fetched updates to the supervisor once per stall
func (p *pollingLifecycle) checkIngressDispatchStall(ctx context.Context) {
	if p.recoveryInFlight() || p.host.HasFatalError() {
		return
	}
	p.mu.Lock()
	if p.webhookMode || p.teardownStarted {
		p.mu.Unlock()
		return
	}
	received, dispatched := p.updatesReceived, p.updatesDispatched
	if received <= dispatched || dispatched != p.ingressDispatchedSeen {
		p.ingressDispatchedSeen = dispatched
		p.ingressStalledHeartbeats = 0
		p.mu.Unlock()
		return
	}
	stalled := p.ingressStalledHeartbeats
	limit := p.timings.ingressStallHeartbeats
	if stalled >= limit {
		p.mu.Unlock()
		return
	}
	p.ingressStalledHeartbeats = stalled + 1
	generation := p.generation
	p.mu.Unlock()
	if stalled+1 < limit {
		return
	}
	p.schedulePollingRecovery(ctx, &PollingStallError{Msg: fmt.Sprintf(
		"ingress healthy but deaf: PTB dispatcher made no progress for %d heartbeats with %d update(s) fetched but not dispatched (%d received, %d dispatched, generation %d)",
		limit, received-dispatched, received, dispatched, generation)}, "ingress dispatch stall watchdog")
}

// Hands a long poll with no successful round trip past the stall timeout to the supervisor
func (p *pollingLifecycle) checkPollingStall(ctx context.Context) {
	if p.recoveryInFlight() || p.host.HasFatalError() {
		return
	}
	p.mu.Lock()
	if p.webhookMode || p.teardownStarted {
		p.mu.Unlock()
		return
	}
	now := monotonicNow()
	var stalledFor float64
	switch {
	case p.lastProgress != nil:
		stalledFor = now - *p.lastProgress
	case p.generationStarted != nil:
		stalledFor = now - *p.generationStarted
	default:
		p.mu.Unlock()
		return
	}
	generation := p.generation
	p.mu.Unlock()
	if stalledFor <= p.timings.stallTimeout {
		return
	}
	p.schedulePollingRecovery(ctx, &PollingStallError{Msg: fmt.Sprintf(
		"getUpdates made no progress for %.0fs (generation %d; polling stall watchdog)", stalledFor, generation)},
		"polling stall watchdog")
}

// Whether a verifier's generation progressed, went fatal, was replaced or torn down
func (p *pollingLifecycle) verifierStale(generation int, progress *pollEvent) bool {
	if p.host.HasFatalError() {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.teardownStarted || progress.isSet() || !p.progressAccepting || generation != p.generation || progress != p.progress
}

// Requires getUpdates progress, using getMe only to classify a failure into the recovery ladder
func (p *pollingLifecycle) verifyPollingAfterReconnect(ctx context.Context, generation *int, progress *pollEvent) {
	if p.isTeardownStarted() {
		return
	}
	p.mu.Lock()
	if generation == nil {
		g := p.generation
		generation = &g
	}
	if progress == nil {
		progress = p.progress
	}
	p.mu.Unlock()
	timer := time.NewTimer(seconds(p.timings.progressTimeout))
	select {
	case <-progress.ch:
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		return
	}
	timer.Stop()
	if p.verifierStale(*generation, progress) {
		return
	}
	app := p.currentApp()
	if app == nil || app.Updater() == nil || !app.Updater().Running() {
		slog.Warn(fmt.Sprintf("[%s] Updater made no getUpdates progress and is not running", p.host.Name()))
		p.schedulePollingRecovery(ctx, &runtimeError{msg: "Updater not running after polling progress deadline"},
			"polling progress verifier: updater not running")
		return
	}
	if _, err := waitForTimeout(ctx, p.timings.verifierProbeTimeout, app.PollingBot().GetMe); err != nil {
		if ctx.Err() != nil || p.verifierStale(*generation, progress) {
			return
		}
		if !looksLikeNetworkError(err) {
			slog.Warn(fmt.Sprintf("[%s] Polling progress verifier hit a non-connectivity error (not retrying): %s", p.host.Name(),
				redactTelegramErrorText(err)))
			return
		}
		slog.Warn(fmt.Sprintf("[%s] Polling progress verifier connectivity probe failed: %s", p.host.Name(), redactTelegramErrorText(err)))
		p.schedulePollingRecovery(ctx, err, "polling progress verifier connectivity failure")
		return
	}
	if p.verifierStale(*generation, progress) {
		return
	}
	p.schedulePollingRecovery(ctx, &PollingStallError{Msg: "getUpdates made no progress before verifier deadline"},
		"polling progress verifier: general path healthy but getUpdates stalled")
}

// Stops the updater's internal retry loop at once so a 409 cannot keep a second session polling
func (p *pollingLifecycle) disarmPTBRetryLoop() {
	app := p.currentApp()
	if app == nil || app.Updater() == nil {
		return
	}
	disarmed, found := app.Updater().DisarmRetryLoop()
	if !found {
		slog.Debug(fmt.Sprintf("[%s] Could not disarm PTB polling retry loop (stop_event not found on this PTB version); falling back to async stop()",
			p.host.Name()))
		return
	}
	if disarmed {
		slog.Debug(fmt.Sprintf("[%s] Disarmed PTB polling retry loop via %s", p.host.Name(), "_Updater__polling_task_stop_event"))
	}
}

// Recovers a 409 Conflict by stopping, waiting a growing delay, draining and restarting, then goes fatal
func (p *pollingLifecycle) handlePollingConflict(ctx context.Context, err error) error {
	if p.isTeardownStarted() {
		return nil
	}
	if p.host.HasFatalError() && fatalCodeIs(p.host, "telegram_polling_conflict") {
		return nil
	}
	maxRetries := p.timings.maxConflictRetries
	p.mu.Lock()
	p.conflictCount++
	count := p.conflictCount
	p.mu.Unlock()
	retryDelay := p.timings.conflictRetryDelayBase + float64(count)*p.timings.conflictRetryDelayFactor
	if count <= maxRetries {
		slog.Warn(fmt.Sprintf("[%s] Telegram polling conflict (%d/%d) — previous session still held open on Telegram's servers. Waiting %ds for it to expire. Error: %s",
			p.host.Name(), count, maxRetries, int(retryDelay), redactTelegramErrorText(err)))
		if !p.stopUpdaterOrGoFatal(ctx, p.currentApp(), "conflict-retry") {
			return nil
		}
		if !sleepCtx(ctx, retryDelay) || p.isTeardownStarted() {
			return nil
		}
		p.drainPollingConnections(ctx)
		if p.isTeardownStarted() {
			return nil
		}
		app := p.currentApp()
		p.mu.Lock()
		expected := p.generation + 1
		p.mu.Unlock()
		if app == nil {
			return &runtimeError{msg: "Telegram application was torn down during conflict reconnect"}
		}
		p.mu.Lock()
		p.conflictRecoveryGeneration = &expected
		callback := p.errorCallbackRef
		p.mu.Unlock()
		_, _, retryErr := p.startPollingOnce(ctx, app, true, callback, false, true)
		p.mu.Lock()
		if p.conflictRecoveryGeneration != nil && *p.conflictRecoveryGeneration == expected {
			p.conflictRecoveryGeneration = nil
		}
		p.mu.Unlock()
		if retryErr == nil {
			slog.Info(fmt.Sprintf("[%s] Telegram polling restarted after conflict retry %d/%d; health pending getUpdates progress",
				p.host.Name(), count, maxRetries))
			return nil
		}
		if _, abort := retryErr.(*pollingLifecycleAbort); abort || p.isTeardownStarted() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Telegram polling retry %d/%d failed: %s. Scheduling next attempt.", p.host.Name(), count,
			maxRetries, redactTelegramErrorText(retryErr)))
		if count < maxRetries && !p.isTeardownStarted() {
			p.restartPollingInTask(func(ctx context.Context) error { return p.handlePollingConflict(ctx, retryErr) })
			return nil
		}
	}
	if p.isTeardownStarted() {
		return nil
	}
	totalWait := 0.0
	for i := 1; i <= maxRetries; i++ {
		totalWait += p.timings.conflictRetryDelayBase + float64(i)*p.timings.conflictRetryDelayFactor
	}
	message := fmt.Sprintf("Telegram polling could not recover after %d retries (%ds total wait). The previous gateway session is still held open on Telegram's servers, or another process is using the same bot token. To recover: ensure no other Icebath or OpenClaw instance is running with this token, then restart the gateway with 'icebath gateway restart'.",
		maxRetries, int(totalWait))
	slog.Error(fmt.Sprintf("[%s] %s Original error: %s", p.host.Name(), message, redactTelegramErrorText(err)))
	alreadyFatal := p.host.HasFatalError() && fatalCodeIs(p.host, "telegram_polling_conflict")
	p.host.SetFatalError(ctx, "telegram_polling_conflict", message, false)
	if app := p.currentApp(); app != nil && app.Updater() != nil {
		updater := app.Updater()
		_, stopErr := awaitWithThreadDeadline(ctx, func(ctx context.Context) (any, error) { return nil, updater.Stop(ctx) },
			p.timings.updaterStopTimeout, nil, "telegram", true)
		if _, timeout := stopErr.(*deadlineTimeoutError); timeout {
			slog.Warn(fmt.Sprintf("[%s] updater.stop() timed out after exhausting conflict retries (likely CLOSE-WAIT socket); proceeding to fatal notify", p.host.Name()))
		} else if stopErr != nil {
			slog.Warn(fmt.Sprintf("[%s] Failed stopping Telegram updater after exhausting conflict retries: %s", p.host.Name(), stopErr.Error()), "error", stopErr)
		}
	}
	if !alreadyFatal {
		p.handoffPollingFatalError(ctx)
	}
	return nil
}

// Whether the host's fatal error code equals code
func fatalCodeIs(host pollingHost, code string) bool {
	current := host.FatalErrorCode()
	return current != nil && *current == code
}

// Notifies the runner after releasing the current task from the recovery and heartbeat slots
func (p *pollingLifecycle) handoffPollingFatalError(ctx context.Context) {
	current := gateway.CurrentAdapterTask(ctx)
	p.mu.Lock()
	if current != nil && p.errorTask == current {
		p.errorTask = nil
	}
	if current != nil && p.heartbeatTask == current {
		p.heartbeatTask = nil
	}
	p.mu.Unlock()
	if err := p.host.NotifyFatalError(ctx); err != nil {
		slog.Debug(fmt.Sprintf("[%s] fatal error notification failed", p.host.Name()), "error", err)
	}
}

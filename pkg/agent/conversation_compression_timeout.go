package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	StallInterruptedFailureClass         = "stall_interrupted"
	commitOverrunWaitSliceSeconds        = 30.0
	cancelledWorkerTeardownGraceSeconds  = 5.0
	compressExecutorMaxWorkers           = 4
	compressionCancelPollIntervalSeconds = 0.025
)

var (
	compressTimeoutExecutor     *tools.DaemonPool
	compressTimeoutExecutorOnce sync.Once
	compressAdmissionLock       sync.Mutex
	compressAdmittedCount       int
	errCompressionDeadlineStart = fmt.Errorf("compression deadline expired before worker start: %w", context.DeadlineExceeded)
)

// One compression worker run against its fence, the rows and the system prompt it settled on
type compressionWorker func(ctx context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error)

func secondsDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func futureDone(f *tools.PoolFuture) bool {
	select {
	case <-f.Done():
		return true
	default:
		return false
	}
}

// Bounded join of a fence cancelled worker, true when it settled so the thread provably exited
func joinCancelledWorker(future *tools.PoolFuture, graceSeconds float64) bool {
	if !waitFuture(future, math.Max(graceSeconds, 0)) {
		return false
	}
	if _, err := future.Result(0); err != nil && !errors.Is(err, tools.ErrFutureCancelled) {
		compressionLog().Debug("cancelled compression worker exited with an exception", "error", err.Error())
	}
	return true
}

// Reserves one bounded compression pool admission slot
func tryAdmitCompressionJob() bool {
	compressAdmissionLock.Lock()
	defer compressAdmissionLock.Unlock()
	if compressAdmittedCount >= compressExecutorMaxWorkers {
		return false
	}
	compressAdmittedCount++
	return true
}

// Frees an admission slot once a future settled or a submit failed
func releaseCompressionAdmission() {
	compressAdmissionLock.Lock()
	defer compressAdmissionLock.Unlock()
	if compressAdmittedCount > 0 {
		compressAdmittedCount--
	}
}

type compressionExecutor interface {
	Submit(ctx context.Context, fn func(ctx context.Context) (any, error)) (*tools.PoolFuture, error)
}

// Executor the timed runner submits to, the hermes module level executor getter tests replace
var compressTimeoutExecutorFor = func() compressionExecutor { return getCompressTimeoutExecutor() }

// Process wide pool every timed compression worker runs on
func getCompressTimeoutExecutor() *tools.DaemonPool {
	compressTimeoutExecutorOnce.Do(func() {
		compressTimeoutExecutor = tools.NewDaemonPool(compressExecutorMaxWorkers, "compress-ctx-timeout")
	})
	return compressTimeoutExecutor
}

// Whether a pre commit cancel landed after the inactivity budget so it counts as a stalled attempt
func compressionAttemptStalled(ctx context.Context, fence *CompressionCommitFence, startedAt float64, idleTimeoutSeconds *float64) bool {
	var idle float64
	if idleTimeoutSeconds != nil {
		idle = *idleTimeoutSeconds
	} else {
		idle, _ = ResolveContextCompressionTimeouts(ctx, nil)
	}
	if math.IsNaN(idle) || idle <= 0 {
		return false
	}
	if fence != nil {
		return fence.SecondsSinceProgress() >= idle
	}
	return pythread.Monotonic()-startedAt >= idle
}

// Identity of the stalled source context and summary strategy
func stallSourceFingerprint(a *AIAgent, messages []map[string]any, approxTokens *int) string {
	model := ""
	if a != nil {
		if sm, ok := a.contextCompressor.(interface{ SummaryModel() string }); ok && !isNilEngine(a.contextCompressor) {
			model = sm.SummaryModel()
		}
		if model == "" {
			model = a.model
		}
	}
	tokens := 0
	if approxTokens != nil {
		tokens = *approxTokens
	}
	return fmt.Sprintf("msgs=%d:tokens=%d:model=%s", len(messages), tokens, model)
}

type timeoutFailureRecorder interface {
	RecordTimeoutFailure(errText, failureKind string)
}

// Persists a stall interrupted cooldown after the snapshot restore, true when the backoff was recorded
func recordStallInterruptedBackoff(ctx context.Context, a *AIAgent, fence *CompressionCommitFence, startedAt float64, messages []map[string]any, approxTokens *int) (recorded bool) {
	if !compressionAttemptStalled(ctx, fence, startedAt, nil) {
		return false
	}
	record, ok := a.contextCompressor.(timeoutFailureRecorder)
	if !ok || isNilEngine(a.contextCompressor) {
		return false
	}
	errText := StallInterruptedFailureClass + ":" + stallSourceFingerprint(a, messages, approxTokens)
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Debug("stall-interrupted compression cooldown persist failed", "error", fmt.Sprint(r))
			recorded = false
		}
	}()
	record.RecordTimeoutFailure(errText, "stall_interrupted")
	compressionLog().Info(fmt.Sprintf("Recorded stall-interrupted compression backoff (session=%s, %s)", pyStrOr(a.sessionID, "none"), errText))
	return true
}

// First usable compression fallback chain entry pinned onto one bounded retry, nil when none is usable
func resolveCompressionFallbackRoute(ctx context.Context) map[string]any {
	cfg, err := provider.AuxiliaryTaskConfig(ctx, "compression")
	if err != nil {
		compressionLog().Debug("compression fallback_chain lookup failed", "error", err.Error())
		return nil
	}
	chain, ok := cfg["fallback_chain"].([]any)
	if !ok {
		return nil
	}
	for index, raw := range chain {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		prov := strings.TrimSpace(pyStr(entry["provider"]))
		model := strings.TrimSpace(pyStr(entry["model"]))
		if prov == "" || model == "" {
			continue
		}
		route := map[string]any{"label": fmt.Sprintf("fallback_chain[%d](%s)", index, prov), "provider": prov, "model": model,
			"base_url": nil, "api_key": nil, "api_mode": nil, "timeout": nil}
		if v := strings.TrimSpace(pyStr(entry["base_url"])); v != "" {
			route["base_url"] = v
		}
		if v := provider.AuxFallbackEntryAPIKey(ctx, entry); v != "" {
			route["api_key"] = v
		}
		mode := pyStr(entry["api_mode"])
		if mode == "" {
			mode = pyStr(entry["transport"])
		}
		if v := strings.TrimSpace(mode); v != "" {
			route["api_mode"] = v
		}
		if t, ok := provider.CoercePositiveTimeout(entry["timeout"]); ok {
			route["timeout"] = t
		}
		return route
	}
	return nil
}

// String form of a config value the way str(value or "") reads it
func pyStr(v any) string {
	if v == nil || v == false {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Pinned stall retry routes, the chain entry then the deterministic summary once a stall already burned a window
func stallRetryRoutes(ctx context.Context, escalateDeterministic bool) []map[string]any {
	var routes []map[string]any
	if r := resolveCompressionFallbackRoute(ctx); r != nil {
		routes = append(routes, r)
	}
	if escalateDeterministic {
		routes = append(routes, DeterministicSummaryRoute())
	}
	return routes
}

// Timeout class failures no healthy summary has cleared this session
func priorTimeoutFailures(a *AIAgent) int {
	if a == nil {
		return 0
	}
	if c, ok := a.contextCompressor.(*ContextCompressor); ok && c != nil {
		return c.consecutiveTimeoutFailures
	}
	return 0
}

// Whether the request exceeds the model window, nil when the window is unknown
func requestExceedsModelWindow(a *AIAgent, requestTokens int) *bool {
	if a == nil || isNilEngine(a.contextCompressor) {
		return nil
	}
	window := a.contextCompressor.Base().ContextLength
	if window <= 0 {
		return nil
	}
	exceeds := requestTokens > window
	return &exceeds
}

// Keyword set of one timed compression run
type compressionTimeoutRun struct {
	Worker                     compressionWorker
	Messages                   []map[string]any
	SystemPromptFallback       func() string
	IdleTimeoutSeconds         float64
	TotalCeilingSeconds        float64
	OnTimeout                  func(idle, waited, sinceProgress float64)
	OnTimeoutCause             func(totalExhausted, progressObserved bool)
	OnCommitOverrun            func(waited, ceiling float64)
	Fence                      *CompressionCommitFence
	TelemetryAgent             *AIAgent
	NoStallFallback            bool
	NewFence                   func() (*CompressionCommitFence, error)
	FallbackWorker             compressionWorker
	RequestExceedsWindow       bool
	PrecommitDeadlineMonotonic *float64
}

func (o *compressionTimeoutRun) fallbackPrompt() string {
	if o.SystemPromptFallback == nil {
		return ""
	}
	return o.SystemPromptFallback()
}

// True when the result is the caller's own list, the Python identity check
func sameMessageList(a, b []map[string]any) bool {
	return len(a) == len(b) && unsafe.SliceData(a) == unsafe.SliceData(b)
}

// Reruns an aborted compression with the summary route pinned on the chain entry then the deterministic summary
func retryCompressionOnFallbackChain(ctx context.Context, o compressionTimeoutRun, precommitDeadline float64, escalateDeterministic bool) ([]map[string]any, string, bool) {
	if a := o.TelemetryAgent; a != nil && a.hardInterruptRequested != nil && a.hardInterruptRequested.IsSet() {
		return nil, "", false
	}
	for _, route := range stallRetryRoutes(ctx, escalateDeterministic) {
		if msgs, prompt, ok := runPinnedCompressionRetry(ctx, route, o, precommitDeadline); ok {
			return msgs, prompt, true
		}
	}
	return nil, "", false
}

// One bounded rerun of the worker with the route pinned, false when it produced no compression
func runPinnedCompressionRetry(ctx context.Context, route map[string]any, o compressionTimeoutRun, precommitDeadline float64) ([]map[string]any, string, bool) {
	deterministic := route["deterministic"] == true
	label := pyStr(route["label"])
	if !deterministic && pythread.Monotonic() >= precommitDeadline {
		compressionLog().Info(fmt.Sprintf("Skipping compression stall fallback on %s: the shared pre-commit ceiling is spent", label))
		return nil, "", false
	}
	var retryFence *CompressionCommitFence
	if o.NewFence != nil {
		f, err := callFenceFactory(o.NewFence)
		if err != nil {
			compressionLog().Warn("compression stall-fallback fence factory failed; the retry will run on an unpublished fence (a /stop mid-retry cannot serialize against its commit boundary)", "error", err.Error())
		}
		retryFence = f
	}
	if retryFence == nil {
		compressionLog().Warn("compression stall-fallback retry running on an unpublished fence; hard-interrupt admission will read the aborted attempt's fence rather than the retry's commit boundary")
		retryFence = NewCompressionCommitFence()
	}
	idle := o.IdleTimeoutSeconds
	if t, ok := route["timeout"].(float64); ok && t != 0 {
		idle = t
	}
	var ceiling float64
	var deadline *float64
	if deterministic {
		ceiling = math.Max(o.TotalCeilingSeconds, idle)
	} else {
		ceiling = precommitDeadline - pythread.Monotonic()
		d := precommitDeadline
		deadline = &d
		if ceiling <= 0 {
			retryFence.RevokeCommitAdmission()
			compressionLog().Info(fmt.Sprintf("Skipping compression stall fallback on %s: its setup spent the shared pre-commit ceiling", label))
			return nil, "", false
		}
		idle = math.Min(idle, ceiling)
	}
	if deterministic {
		compressionLog().Warn(fmt.Sprintf("Context compression stalled on every summary route — committing the %s (no summary model) before continuing without compression", label))
	} else {
		compressionLog().Warn(fmt.Sprintf("Context compression stalled on the configured summary route — retrying once on %s (%s) within %.1fs of the shared pre-commit budget before continuing without compression", label, pyStr(route["model"]), ceiling))
	}
	streakBefore := fallbackStreakOf(o.TelemetryAgent)
	var msgs []map[string]any
	var prompt string
	err := PinSummaryRoute(ctx, route, func() error {
		var runErr error
		msgs, prompt, runErr = runCompressContextWithProgressTimeout(ctx, compressionTimeoutRun{
			Worker: o.Worker, Messages: o.Messages, SystemPromptFallback: o.SystemPromptFallback, IdleTimeoutSeconds: idle,
			TotalCeilingSeconds: ceiling, OnCommitOverrun: o.OnCommitOverrun, OnTimeoutCause: o.OnTimeoutCause, Fence: retryFence,
			TelemetryAgent: o.TelemetryAgent, NoStallFallback: true, PrecommitDeadlineMonotonic: deadline,
		})
		return runErr
	})
	if err != nil {
		compressionLog().Warn(fmt.Sprintf("Context compression fallback attempt on %s failed", label), "error", err.Error())
		return nil, "", false
	}
	if sameMessageList(msgs, o.Messages) {
		compressionLog().Warn(fmt.Sprintf("Context compression fallback attempt on %s produced no compression; continuing without compression", label))
		return nil, "", false
	}
	if deterministic || fallbackStreakOf(o.TelemetryAgent) > streakBefore {
		compressionLog().Warn(fmt.Sprintf("Context compression committed a deterministic fallback summary on %s after the primary summary route stalled (no summary model produced output)", label))
	} else {
		compressionLog().Info(fmt.Sprintf("Context compression recovered on %s after the primary summary route stalled", label))
	}
	return msgs, prompt, true
}

// Calls the fence factory, turning a panic into the error the hermes except branch logs
func callFenceFactory(newFence func() (*CompressionCommitFence, error)) (f *CompressionCommitFence, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return newFence()
}

func fallbackStreakOf(a *AIAgent) int {
	if a == nil {
		return 0
	}
	if c, ok := a.contextCompressor.(*ContextCompressor); ok && c != nil {
		return c.fallbackCompressionStreak
	}
	return 0
}

type workerOutcome struct {
	messages []map[string]any
	prompt   string
}

// Waits up to seconds for the future and reports whether it settled, the result(timeout) wait
func waitFuture(f *tools.PoolFuture, seconds float64) bool {
	if seconds <= 0 {
		return futureDone(f)
	}
	timer := time.NewTimer(secondsDuration(seconds))
	defer timer.Stop()
	select {
	case <-f.Done():
		return true
	case <-timer.C:
		return futureDone(f)
	}
}

// Outcome of a settled future, the worker error included
func settledOutcome(f *tools.PoolFuture) (workerOutcome, error) {
	res, err := f.Result(0)
	if err != nil {
		return workerOutcome{}, err
	}
	out, _ := res.(workerOutcome)
	return out, nil
}

// Polls the worker under the idle budget and ceiling, settled true with its outcome or the error it raised
func awaitWorkerWithinBudget(future *tools.PoolFuture, fence *CompressionCommitFence, idle, ceiling, waitStarted float64) (bool, workerOutcome, error) {
	for {
		deadline, armed := fence.DeadlineMonotonic()
		if !armed || deadline == 0 {
			deadline = waitStarted + ceiling
		}
		remainingCeiling := deadline - pythread.Monotonic()
		if remainingCeiling <= 0 {
			return false, workerOutcome{}, nil
		}
		waitSlice := math.Min(math.Max(idle-fence.SecondsSinceProgress(), 0.005), remainingCeiling)
		timer := time.NewTimer(secondsDuration(waitSlice))
		select {
		case <-future.Done():
			timer.Stop()
			out, err := settledOutcome(future)
			if err != nil && (isPyTimeoutError(err) || errors.Is(err, context.DeadlineExceeded)) {
				compressionLog().Info(fmt.Sprintf("Context compression worker exited with %v — taking the stall path", err))
				return false, workerOutcome{}, nil
			}
			return err == nil, out, err
		case <-timer.C:
		}
		if futureDone(future) {
			out, err := settledOutcome(future)
			if err == nil {
				return true, out, nil
			}
			compressionLog().Info(fmt.Sprintf("Context compression worker exited with %v — taking the stall path", err))
			return false, workerOutcome{}, nil
		}
		waited := pythread.Monotonic() - waitStarted
		sinceProgress := fence.SecondsSinceProgress()
		if !fence.DeadlineExceeded() && sinceProgress < idle {
			compressionLog().Info(fmt.Sprintf("Context compression still streaming after %.0fs (last progress %.1fs ago) — extending wait (ceiling %.0fs)", waited, sinceProgress, ceiling))
			continue
		}
		return false, workerOutcome{}, nil
	}
}

// Waits out an admitted commit in bounded slices, surfacing an overrun once and never abandoning it
func awaitInFlightCommit(future *tools.PoolFuture, ceiling, waitStarted float64, onCommitOverrun func(waited, ceiling float64)) (workerOutcome, error) {
	overrunSurfaced := false
	overrunReports := 0
	for {
		waited := pythread.Monotonic() - waitStarted
		remaining := ceiling - waited
		if remaining <= 0 {
			remaining = math.Min(commitOverrunWaitSliceSeconds, math.Max(ceiling, 0.05))
			overrunReports++
			msg := fmt.Sprintf("Context compression SessionDB commit still running %.1fs past the total ceiling (waited %.1fs, ceiling %.1fs); commit cannot be abandoned mid-flight — continuing to wait (check SessionDB health if this persists)", waited-ceiling, waited, ceiling)
			if overrunReports <= 2 {
				compressionLog().Warn(msg)
			} else {
				compressionLog().Error(msg)
			}
			if !overrunSurfaced && onCommitOverrun != nil {
				overrunSurfaced = true
				swallowCompression("compress_context commit-overrun callback failed", true, func() error {
					onCommitOverrun(waited, ceiling)
					return nil
				})
			}
		}
		if waitFuture(future, remaining) {
			return settledOutcome(future)
		}
	}
}

// Cancels pre commit, false when an admitted commit owns the fence and the caller must wait
func cancelOrJoinWorker(fence *CompressionCommitFence) bool {
	for {
		if fence.CommitInFlight() {
			return false
		}
		if cancelled := fence.TryCancelBeforeCommit(); cancelled != nil {
			return *cancelled
		}
		time.Sleep(secondsDuration(compressionCancelPollIntervalSeconds))
	}
}

// Frees the timed out worker's lease, joining it within a grace only when the total ceiling ran out
func releaseCancelledWorker(future *tools.PoolFuture, fence *CompressionCommitFence, totalExhausted bool, ceiling float64) {
	if totalExhausted {
		grace := math.Min(cancelledWorkerTeardownGraceSeconds, ceiling)
		if joinCancelledWorker(future, grace) {
			fence.AllowCancelledLockRelease()
		} else {
			compressionLog().Warn(fmt.Sprintf("Cancelled compression worker did not exit within %.1fs grace — orphaning it behind the poison fence (late result will be discarded); retaining the session compression lease until it exits so no new attempt overlaps it", grace))
		}
	}
	fence.ReleaseCancelledCompressionLock()
}

// Runs the worker under an idle and ceiling timeout, detaching it on a pre commit cancel and retrying a stall once
func runCompressContextWithProgressTimeout(ctx context.Context, o compressionTimeoutRun) (_ []map[string]any, _ string, retErr error) {
	if o.IdleTimeoutSeconds <= 0 {
		return nil, "", errors.New("run_compress_context_with_progress_timeout requires idle_timeout_seconds > 0; call compress_context directly to disable")
	}
	stallFallback := !o.NoStallFallback
	ceiling := math.Max(o.TotalCeilingSeconds, o.IdleTimeoutSeconds)
	idle := o.IdleTimeoutSeconds
	if o.RequestExceedsWindow {
		ceiling = idle
	}
	waitStarted := pythread.Monotonic()
	var precommitDeadline float64
	if o.PrecommitDeadlineMonotonic == nil {
		precommitDeadline = waitStarted + ceiling
	} else {
		precommitDeadline = *o.PrecommitDeadlineMonotonic
		ceiling = math.Min(ceiling, precommitDeadline-waitStarted)
		if ceiling <= 0 {
			if o.Fence != nil {
				o.Fence.RevokeCommitAdmission()
			}
			return o.Messages, o.fallbackPrompt(), nil
		}
		idle = math.Min(idle, ceiling)
	}
	fence := o.Fence
	if fence == nil {
		fence = NewCompressionCommitFence()
	}
	if err := fence.SetPrecommitDeadlineMonotonic(precommitDeadline); err != nil {
		return nil, "", err
	}
	escalateDeterministic := stallFallback && (o.RequestExceedsWindow || priorTimeoutFailures(o.TelemetryAgent) >= 1)
	executor := compressTimeoutExecutorFor()
	if !tryAdmitCompressionJob() {
		compressionLog().Warn(fmt.Sprintf("Context compression pool saturated (%d workers busy) — refusing new compression this cycle and continuing without compression. Wedged workers are fence-cancelled and free their slot when they return; if this persists, check the summary provider health.", compressExecutorMaxWorkers))
		if o.TelemetryAgent != nil {
			emitCompressionAttemptTelemetry(ctx, o.TelemetryAgent, attemptTelemetryEmit{StartedAt: time.Now(), CommitStatus: "aborted", SplitStatus: "aborted",
				FailureClass: "pool_saturated", ExcludeLastTelemetry: true})
		}
		return o.Messages, o.fallbackPrompt(), nil
	}
	worker := o.Worker
	future, err := executor.Submit(ctx, func(ctx context.Context) (any, error) {
		if fence.DeadlineExceeded() {
			return nil, errCompressionDeadlineStart
		}
		if fence.IsCancelled() {
			compressionLog().Info("Skipping stale compression job: fence cancelled before start")
			return workerOutcome{messages: o.Messages}, nil
		}
		msgs, prompt, err := worker(ctx, fence)
		if err != nil {
			return nil, err
		}
		return workerOutcome{messages: msgs, prompt: prompt}, nil
	})
	if err != nil {
		releaseCompressionAdmission()
		return nil, "", err
	}
	go func() {
		<-future.Done()
		releaseCompressionAdmission()
	}()
	handledExit := false
	defer func() {
		if !handledExit {
			fence.RevokeCommitAdmission()
		}
	}()
	recoverFromStall := func() ([]map[string]any, string, error) {
		waited := pythread.Monotonic() - waitStarted
		sinceProgress := fence.SecondsSinceProgress()
		if stallFallback {
			retry := o
			if o.FallbackWorker != nil {
				retry.Worker = o.FallbackWorker
			}
			retry.IdleTimeoutSeconds, retry.TotalCeilingSeconds = idle, ceiling
			deadline, armed := fence.DeadlineMonotonic()
			if !armed || deadline == 0 {
				deadline = precommitDeadline
			}
			if msgs, prompt, ok := retryCompressionOnFallbackChain(ctx, retry, deadline, escalateDeterministic); ok {
				return msgs, prompt, nil
			}
		}
		if o.OnTimeout != nil {
			swallowCompression("compress_context timeout callback failed", true, func() error {
				o.OnTimeout(idle, waited, sinceProgress)
				return nil
			})
		} else {
			compressionLog().Warn(fmt.Sprintf("Context compression made no progress for %.1fs (total wait %.1fs, ceiling %.1fs); continuing without compression", sinceProgress, waited, ceiling))
		}
		return o.Messages, o.fallbackPrompt(), nil
	}
	timeoutCause := func(totalExhausted bool) {
		if o.OnTimeoutCause != nil {
			swallowCompression("compress_context timeout-cause callback failed", true, func() error {
				o.OnTimeoutCause(totalExhausted, fence.ProgressObserved())
				return nil
			})
		}
	}
	settled, result, err := awaitWorkerWithinBudget(future, fence, idle, ceiling, waitStarted)
	if err != nil {
		return nil, "", err
	}
	if settled {
		handledExit = true
		if stallFallback && fence.DeadlineExceeded() && sameMessageList(result.messages, o.Messages) {
			timeoutCause(true)
			return recoverFromStall()
		}
		return result.messages, result.prompt, nil
	}
	future.Cancel()
	totalExhausted := fence.DeadlineExceeded()
	if totalExhausted {
		fence.RetainCompressionLockUntilWorkerDone()
	}
	timeoutCause(totalExhausted)
	if !cancelOrJoinWorker(fence) {
		deadline, _ := fence.DeadlineMonotonic()
		result, err := awaitInFlightCommit(future, math.Max(ceiling, deadline-waitStarted), waitStarted, o.OnCommitOverrun)
		if err != nil {
			return nil, "", err
		}
		handledExit = true
		if stallFallback && sameMessageList(result.messages, o.Messages) {
			return recoverFromStall()
		}
		return result.messages, result.prompt, nil
	}
	handledExit = true
	releaseCancelledWorker(future, fence, totalExhausted, ceiling)
	return recoverFromStall()
}

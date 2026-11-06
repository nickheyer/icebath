package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	ownershipLostInterrupted = "Interrupted by shutdown before terminal completion."
	deliveryDrainLimit       = 20
	externalWorkerEnv        = "_ICEBATH_CRON_EXTERNAL_WORKER"
)

var blockedConfigTagRE = regexp.MustCompile(`\[blocked_config[^\]]*\]\s*`)

// Markers one run sets on its job for the bookkeeping tail, never persisted
type runMarks struct {
	modelUnreachable                 bool
	quotaHoldSeconds                 float64
	failureIncidentID                *string
	notificationAllTargetsSuppressed bool
}

// Incident id a queued failure notice carries through the delivery queue payload
func queuedFailureIncidentID(job *Job) *string {
	if job.run.failureIncidentID != nil {
		return job.run.failureIncidentID
	}
	raw, ok := job.Extra["_failure_incident_id"]
	if !ok {
		return nil
	}
	var id string
	if json.Unmarshal(raw, &id) != nil {
		return nil
	}
	return &id
}

// Transport cancel channel viewed as a settable cancel event
type chanCancel struct {
	ch  <-chan struct{}
	evt latchEvent
}

func (c *chanCancel) IsSet() bool {
	if c.evt.IsSet() {
		return true
	}
	select {
	case <-c.ch:
		return true
	default:
		return false
	}
}
func (c *chanCancel) Set()           { c.evt.Set() }
func (c *chanCancel) Reason() string { return "" }

// Run one job as the tick and providers reach it, swapped by tests like a patched scheduler.run_one_job
var runOneJobSeam = runOneJob

// Run job as the run one job body reaches it, swapped by tests like a patched scheduler.run_job
var runJobSeam = func(ctx context.Context, s *Store, job *Job, o runJobOptions) (jobRunOutcome, error) {
	return s.runJob(ctx, job, o)
}

// Runs one due job end to end, true when processed and false only when processing raised
func runOneJob(ctx context.Context, s *Store, job *Job, o runOneJobOptions) (bool, error) {
	ledger := openExecutionsLedger(s)
	if job.ExecutionID == nil || *job.ExecutionID == "" {
		execution, err := ledger.Create(ctx, job.ID, "direct", job.ScheduledInstant)
		if err != nil {
			return false, err
		}
		job.ExecutionID = strPtr(pyStrOrEmpty(execution["id"]))
	}
	executionID := *job.ExecutionID
	observability.NoteCronExecution(job)
	if os.Getenv(externalWorkerEnv) != executionID {
		handedOff, handoffErr := s.launchExternalCronWorker(ctx, job)
		if handoffErr != nil {
			var postErr *externalWorkerPostHandoffError
			postHandoff := errors.As(handoffErr, &postErr)
			stage := "dispatch failed"
			if postHandoff {
				stage = "failed after handoff"
			}
			errText := fmt.Sprintf("Restart-safe cron worker %s: %s", stage, handoffErr.Error())
			if postHandoff {
				recorded, err := s.recordUnknownWorkerOutcome(ctx, job, strPtr(handoffErr.Error()), o.Adapters, o.Loop)
				if err != nil {
					return false, err
				}
				if recorded {
					return true, nil
				}
			}
			s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, errText))
			owner := ""
			if job.FireClaim != nil {
				owner = job.FireClaim.By
			}
			var deliveryError, deliveryOutcome *string
			if !postHandoff {
				var outcome string
				deliveryError, outcome = s.deliverCrashFailure(s.installFireSecretScope(ctx), job, errText, o.Adapters, o.Loop)
				deliveryOutcome = &outcome
			}
			mark := MarkOptions{Error: &errText, DeliveryError: deliveryError, LadderRung: unreachableRetryIsRetryRun(s, job)}
			if owner != "" {
				mark.ExpectedFireOwner = &owner
			}
			_, markErr := s.markJobRun(ctx, job.ID, false, mark)
			_, finishErr := ledger.Finish(ctx, executionID, false, &errText, deliveryOutcome, false)
			if markErr != nil {
				return false, markErr
			}
			return true, finishErr
		}
		if handedOff {
			if _, err := s.recordUnknownWorkerOutcome(ctx, job, nil, o.Adapters, o.Loop); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	extraPrompt := o.ExtraPrompt
	if extraPrompt == nil && job.ManualRunPrompt != nil && *job.ManualRunPrompt != "" && job.ManualRunAt != nil && *job.ManualRunAt != "" {
		extraPrompt = strPtr(*job.ManualRunPrompt)
	}
	var fireOwner *string
	if job.FireClaim != nil && job.FireClaim.By != "" {
		fireOwner = strPtr(job.FireClaim.By)
	}
	token := new(int)
	profileHome := s.home
	if abs, err := filepath.Abs(s.home); err == nil {
		profileHome = abs
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			profileHome = real
		}
	}
	key := s.inflightKey(job.ID, "")
	inflight.Lock()
	if inflight.fireOwners[key] == nil {
		inflight.fireOwners[key] = map[any]fireOwnerEntry{}
	}
	inflight.fireOwners[key][token] = fireOwnerEntry{Owner: fireOwner, Home: profileHome}
	inflight.Unlock()
	defer func() {
		inflight.Lock()
		if owners := inflight.fireOwners[key]; owners != nil {
			delete(owners, token)
			if len(owners) == 0 {
				delete(inflight.fireOwners, key)
			}
		}
		inflight.Unlock()
	}()
	scoped, _ := selfRemovalDeliveryScope(ctx, job.ID)
	var transport CancelSource
	if o.Cancel != nil {
		transport = &chanCancel{ch: o.Cancel}
	}
	return s.runWithFireClaimHeartbeat(scoped, job, func(lost CancelSource) (bool, error) {
		return s.runOneJobBody(scoped, job, o, extraPrompt, lost, transport, token)
	})
}

// Bookkeeping after fire claim loss, a still owned claim records the interruption and a lost one is discarded
func (s *Store) recordFireOwnershipLost(ctx context.Context, job *Job, fireOwner *string, executionID string) error {
	ledger := openExecutionsLedger(s)
	if fireOwner != nil {
		owns, err := s.heartbeatFireClaim(job.ID, *fireOwner)
		if err != nil {
			return err
		}
		if owns {
			if _, err := s.markJobRun(ctx, job.ID, false, MarkOptions{
				Error: strPtr(ownershipLostInterrupted), ExpectedFireOwner: fireOwner, LadderRung: unreachableRetryIsRetryRun(s, job),
			}); err != nil {
				return err
			}
			_, err = ledger.Finish(ctx, executionID, false, strPtr(ownershipLostInterrupted), nil, false)
			return err
		}
	}
	_, err := s.finishLostExecution(ctx, executionID, "Fire claim ownership lost; stale result was discarded.")
	return err
}

// Composed notice of a finished run with its blocked, alert once, acked and incident flags
type composedDelivery struct {
	content           string
	blockedConfig     bool
	silentAlert       bool
	incidentAcked     bool
	failureIncidentID *string
}

// Text to deliver for a finished run, empty when an acked incident withholds the failure ping
func (s *Store) composeRunDelivery(job *Job, success bool, errText *string, finalResponse string, outputFile *string, agentDeclared bool) composedDelivery {
	err := ""
	if errText != nil {
		err = *errText
	}
	var out composedDelivery
	out.silentAlert = strings.Contains(err, BlockedConfigSilentMarker)
	out.blockedConfig = out.silentAlert || strings.Contains(err, BlockedConfigMarker)
	name := job.Name
	if name == "" {
		name = job.ID
	}
	switch {
	case out.blockedConfig && !success:
		out.content = blockedConfigNotice(name, pystr.Strip(blockedConfigTagRE.ReplaceAllString(err, "")))
	case success:
		out.content = finalResponse
		s.resolveIncidentsForRecoveredJob(job)
	default:
		out.incidentAcked, out.failureIncidentID = s.upsertIncidentForFailure(job, err, outputFile)
		switch {
		case out.incidentAcked:
			out.content = ""
		case agentDeclared:
			out.content = genericFailureNotice(s.home, name, job.ID, strings.TrimRight(pystr.Strip(err), ".")) + s.failureStreakNudge(job)
		default:
			out.content = s.summarizeCronFailureForDelivery(job, errText) + s.failureStreakNudge(job) + quotaHoldNotice(s, job, job.run.quotaHoldSeconds)
		}
	}
	return out
}

// Raised inside a side effect fence when the durable fire claim is no longer ours
var errFireClaimLostDuringSideEffect = errors.New("fire claim lost during side effect")

// Fire claim ownership checks for one run, owner nil when the job carries no claim
type fireOwnership struct {
	s               *Store
	job             *Job
	claimLost       CancelSource
	transportCancel CancelSource
	operatorCancel  CancelSource
	cancelEvent     CancelSource
	owner           *string
}

func (s *Store) newFireOwnership(job *Job, claimLost, transportCancel, operatorCancel CancelSource) *fireOwnership {
	f := &fireOwnership{s: s, job: job, claimLost: claimLost, transportCancel: transportCancel, operatorCancel: operatorCancel}
	present := []CancelSource{}
	for _, c := range []CancelSource{claimLost, transportCancel, operatorCancel} {
		if c != nil {
			present = append(present, c)
		}
	}
	if len(present) > 0 {
		f.cancelEvent = NewCombinedCancelEvent(present...)
	}
	if job.FireClaim != nil {
		f.owner = strPtr(job.FireClaim.By)
	}
	return f
}

func (f *fireOwnership) transportCancelled() bool {
	return f.transportCancel != nil && f.transportCancel.IsSet()
}

func (f *fireOwnership) operatorCancelled() bool {
	return f.operatorCancel != nil && f.operatorCancel.IsSet()
}

// Runs fn under the job's fire claim fence, always owned when the job carries no claim
func (f *fireOwnership) sideEffectFence(ctx context.Context, fn func(ctx context.Context, owns bool) error) error {
	if f.owner == nil {
		return fn(ctx, true)
	}
	return f.s.fireClaimFence(ctx, f.job.ID, *f.owner, fn)
}

// True when a transport cancel arrived or the durable claim no longer names this run's owner
func (f *fireOwnership) lost(ctx context.Context) bool {
	if f.transportCancelled() {
		return true
	}
	if f.owner == nil {
		return false
	}
	if allowed, _ := f.s.selfRemovalDeliveryAllowed(ctx, nil, f.job.ID); allowed {
		return false
	}
	latched := f.claimLost != nil && f.claimLost.IsSet()
	owns, err := f.s.heartbeatFireClaim(f.job.ID, *f.owner)
	if err != nil {
		f.s.log.Debug(fmt.Sprintf("Job '%s': fire_claim ownership validation failed", f.job.ID), "error", err)
		return latched
	}
	if owns {
		return false
	}
	if f.claimLost != nil {
		f.claimLost.Set()
	}
	return true
}

// Mutable outcome of the save, compose and deliver phase read back by the bookkeeping tail
type runDelivery struct {
	job                     *Job
	success                 bool
	err                     *string
	deliveryAttempted       bool
	deliveryError           *string
	shouldDeliver           bool
	unresolvedOrigin        bool
	blockedConfig           bool
	agentDeclared           bool
	incidentAcked           bool
	failureIncidentID       *string
	sideEffectOwnershipLost bool
}

// Saves output, composes the notice and delivers it, both side effects under the fire claim fence
func (s *Store) saveComposeDeliver(ctx context.Context, d *runDelivery, fence *fireOwnership, finalResponse, output string, o runOneJobOptions, token any) error {
	job := d.job
	var outputFile *string
	err := fence.sideEffectFence(ctx, func(ctx context.Context, owns bool) error {
		if !owns {
			return errFireClaimLostDuringSideEffect
		}
		allowed, err := s.selfRemovalDeliveryAllowed(ctx, nil, job.ID)
		if err != nil {
			return err
		}
		if allowed {
			return nil
		}
		path, err := s.SaveJobOutput(job.ID, output)
		if err != nil {
			return err
		}
		outputFile = &path
		return nil
	})
	if err != nil {
		return err
	}
	if o.Verbose && outputFile != nil {
		s.log.Info(fmt.Sprintf("Output saved to: %s", *outputFile))
	}
	if d.success && s.isInterrupted(job.ID, token) {
		d.success = false
		d.err = strPtr("Interrupted by gateway shutdown before the run finished (tool subprocess was killed mid-flight).")
	}
	composed := s.composeRunDelivery(job, d.success, d.err, finalResponse, outputFile, d.agentDeclared)
	d.blockedConfig, d.incidentAcked, d.failureIncidentID = composed.blockedConfig, composed.incidentAcked, composed.failureIncidentID
	d.shouldDeliver = pystr.Strip(composed.content) != "" && !composed.silentAlert
	if d.shouldDeliver && !d.success && job.run.modelUnreachable {
		willRetry, err := unreachableRetryWillRetry(s, job)
		if err != nil {
			return err
		}
		if willRetry {
			d.shouldDeliver = false
			s.log.Info(fmt.Sprintf("Job '%s': suppressing failure notice — automatic re-run pending", job.ID))
		}
	}
	if d.shouldDeliver && d.success && isCronSilenceResponse(composed.content) {
		s.log.Info(fmt.Sprintf("Job '%s': agent returned %s — skipping delivery", job.ID, SilentMarker))
		d.shouldDeliver = false
	}
	if d.shouldDeliver && fence.lost(ctx) {
		d.shouldDeliver = false
		s.log.Warn(fmt.Sprintf("Job '%s': skipping delivery after fire claim ownership loss", job.ID))
	}
	if !d.shouldDeliver {
		return nil
	}
	lane := normalizeDeliverValue(deliveryLaneValue(job, !d.success))
	d.unresolvedOrigin = lane == "origin" && len(resolveDeliveryTargets(s, job, !d.success)) == 0
	if job.ExecutionID != nil && *job.ExecutionID != "" && !job.NoAgent {
		executionID := *job.ExecutionID
		if d.unresolvedOrigin || lane == "local" {
			return nil
		}
		queuedJob := *job
		if d.failureIncidentID != nil {
			queuedJob.Extra = make(map[string]json.RawMessage, len(job.Extra)+1)
			for k, v := range job.Extra {
				queuedJob.Extra[k] = v
			}
			raw, _ := json.Marshal(*d.failureIncidentID)
			queuedJob.Extra["_failure_incident_id"] = raw
			queuedJob.keyOrder = append(append([]string(nil), job.keyOrder...), "_failure_incident_id")
		}
		var queued map[string]any
		err := fence.sideEffectFence(ctx, func(ctx context.Context, owns bool) error {
			if !owns {
				return errFireClaimLostDuringSideEffect
			}
			var err error
			queued, err = openDeliveryQueue(s).Enqueue(ctx, executionID, &queuedJob, composed.content, !d.success)
			if err != nil {
				return err
			}
			d.deliveryAttempted = true
			return nil
		})
		if err != nil {
			return err
		}
		job.LastDeliveryQueued = map[string]any{"canonical": map[string]any{"status": queued["status"], "execution_id": executionID}}
		return nil
	}
	err = fence.sideEffectFence(ctx, func(ctx context.Context, owns bool) error {
		if !owns {
			return errFireClaimLostDuringSideEffect
		}
		d.deliveryAttempted = true
		d.deliveryError = s.deliverResult(ctx, job, composed.content, o.Adapters, o.Loop, !d.success)
		return nil
	})
	if err != nil {
		if errors.Is(err, errFireClaimLostDuringSideEffect) {
			return err
		}
		d.deliveryError = strPtr(err.Error())
		s.log.Error(fmt.Sprintf("Delivery failed for job %s: %v", job.ID, err))
	}
	return nil
}

// Failure notice for a run that raised out of run_job, the delivery error and ledger outcome
func (s *Store) deliverCrashFailure(ctx context.Context, job *Job, errText string, adapters, loop any) (*string, string) {
	normalized := normalizeDeliverValue(deliveryLaneValue(job, true))
	acked, incidentID := s.upsertIncidentForFailure(job, errText, nil)
	if acked {
		return nil, "suppressed_acked"
	}
	deliveryError := func() (out *string) {
		defer func() {
			if r := recover(); r != nil {
				out = strPtr(fmt.Sprint(r))
				s.log.Error(fmt.Sprintf("Delivery failed for job %s: %v", job.ID, r))
			}
		}()
		return s.deliverResult(ctx, job, s.summarizeCronFailureForDelivery(job, &errText)+s.failureStreakNudge(job), adapters, loop, true)
	}()
	unresolved := deliveryError == nil && normalized == "origin" && len(resolveDeliveryTargets(s, job, true)) == 0
	outcome := classifyDeliveryOutcome(deliveryError, true, unresolved, normalized, false, false, job.LastDeliveryQueued, job.run.notificationAllTargetsSuppressed)
	if outcome == "delivered" || outcome == "not_configured" {
		s.markIncidentAlerted(incidentID)
	}
	return deliveryError, outcome
}

// Firing profile's secret scope for the run span, hydrated first, multiplexed for a routed profile fire
func (s *Store) installFireSecretScope(ctx context.Context) context.Context {
	provider.HydrateProfileSecretSources(s.home)
	ctx = provider.WithSecretScope(ctx, provider.OwnerSecretScope(ctx, s.home), s.home)
	if routedProfileFire(ctx, strPtr(s.home)) {
		ctx = provider.WithMultiplexContext(ctx, true)
	}
	return ctx
}

// Wins the claimed to running transition and binds the cron identity, false when ownership was lost first
func (s *Store) startOwnedRun(ctx context.Context, job *Job, executionID string) (context.Context, bool, error) {
	ledger := openExecutionsLedger(s)
	var record map[string]any
	var err error
	if os.Getenv(externalWorkerEnv) == executionID {
		record, err = ledger.Get(ctx, executionID)
	} else {
		record, err = ledger.MarkRunning(ctx, executionID)
		if err == nil && record == nil {
			return ctx, false, nil
		}
	}
	if err != nil {
		return ctx, false, err
	}
	if record == nil {
		record = map[string]any{}
	}
	return enterCronExecution(ctx, job, executionID, record), true, nil
}

// Turns a completed agent turn that declared a cron failure marker into a failure
func applyAgentFailureMarker(job *Job, success bool, errText *string, finalResponse string) (bool, *string, bool) {
	if success && !job.NoAgent {
		if marker, ok := cronFailureMarkerError(finalResponse); ok {
			return false, &marker, true
		}
	}
	return success, errText, false
}

// Execute, save, deliver and mark one run that already owns its dispatch slot
func (s *Store) runOneJobBody(ctx context.Context, job *Job, o runOneJobOptions, extraPrompt *string, claimLost, transportCancel CancelSource, token any) (processed bool, err error) {
	ledger := openExecutionsLedger(s)
	executionID := ""
	if job.ExecutionID != nil {
		executionID = *job.ExecutionID
	}
	if executionID == "" {
		execution, err := ledger.Create(ctx, job.ID, "direct", job.ScheduledInstant)
		if err != nil {
			return false, err
		}
		executionID = pyStrOrEmpty(execution["id"])
	}
	cp := *job
	cp.ExecutionID = strPtr(executionID)
	job = &cp
	fence := s.newFireOwnership(job, claimLost, transportCancel, s.newCancelRequest(executionID))
	fireOwner := fence.owner
	deliveryAttempted := false
	var deliveryError *string
	defer s.clearCancel(executionID)
	fail := func(cause error, contained bool) (bool, error) {
		var unknown *CronExecutionUnknown
		if errors.As(cause, &unknown) {
			if perr := s.pauseForUnknown(job.ID, executionID, cause.Error()); perr != nil {
				s.log.Error(fmt.Sprintf("Cron %s pause for unknown admission failed: %v", job.ID, perr))
			}
			if _, gerr := ledger.GiveUp(ctx, executionID, cause.Error()); gerr != nil {
				s.log.Error(fmt.Sprintf("Cron %s give up failed: %v", job.ID, gerr))
			}
			s.log.Error(fmt.Sprintf("Cron %s paused with unverified admission: %s", job.ID, cause.Error()))
			return false, nil
		}
		errText := cause.Error()
		if errText == "" {
			errText = cronExceptionName(cause)
		}
		s.log.Error(fmt.Sprintf("Error processing job %s: %s", job.ID, errText))
		outcome := "suppressed"
		if contained && !deliveryAttempted && !errors.Is(cause, errFireClaimLostDuringSideEffect) && !fence.lost(ctx) {
			deliveryError, outcome = s.deliverCrashFailure(ctx, job, errText, o.Adapters, o.Loop)
		}
		if !s.consumeInterruptedFlag(job.ID, token) {
			if allowed, _ := s.selfRemovalDeliveryAllowed(ctx, nil, job.ID); !allowed {
				mark := MarkOptions{Error: &errText, ExpectedFireOwner: fireOwner, LadderRung: unreachableRetryIsRetryRun(s, job)}
				if contained {
					mark.DeliveryError = deliveryError
				}
				if _, merr := s.markJobRun(ctx, job.ID, false, mark); merr != nil {
					s.log.Error(fmt.Sprintf("Failed to record interrupted run for job %s: %v", job.ID, merr))
				}
			}
		}
		if _, ferr := ledger.Finish(ctx, executionID, false, &errText, &outcome, false); ferr != nil {
			s.log.Error(fmt.Sprintf("Failed to finish execution record for job %s: %v", job.ID, ferr))
		}
		return false, nil
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Errorf("%v", r), false)
			panic(r)
		}
	}()
	processed, bodyErr := func() (bool, error) {
		runCtx := s.installFireSecretScope(ctx)
		claimed, err := s.claimDispatch(job.ID)
		if err != nil {
			return false, err
		}
		if !claimed {
			name := job.Name
			if name == "" {
				name = job.ID
			}
			s.log.Info(fmt.Sprintf("Job '%s': one-shot dispatch limit reached — skipping", name))
			_, err := ledger.Finish(ctx, executionID, false, strPtr("Dispatch claim rejected; execution was not started."), nil, false)
			return true, err
		}
		runCtx, owned, err := s.startOwnedRun(runCtx, job, executionID)
		if err != nil {
			return false, err
		}
		if !owned {
			s.log.Warn(fmt.Sprintf("Cron job %s lost execution ownership before start; skipping", job.ID))
			return true, nil
		}
		runCtx = tools.WithProfileTerminalScope(runCtx, s.home, nil)
		var deferredAgents []CronAgent
		teardownDeferred := func() {
			for _, a := range deferredAgents {
				s.teardownCronAgent(a, job.ID, nil)
			}
			deferredAgents = nil
		}
		ro := runJobOptions{DeferAgentTeardown: &deferredAgents, ExtraPrompt: extraPrompt, ExecutionID: &executionID}
		if fence.cancelEvent != nil {
			ro.Cancel = fence.cancelEvent
		}
		outcome, ran, err := runJobUnlessCancelled(runCtx, s, func() (jobRunOutcome, error) { return runJobSeam(runCtx, s, job, ro) },
			job, teardownDeferred, fence, executionID, fireOwner)
		if err != nil {
			return false, err
		}
		if !ran {
			return true, nil
		}
		if fence.lost(runCtx) {
			teardownDeferred()
			return true, s.recordFireOwnershipLost(ctx, job, fireOwner, executionID)
		}
		success, errText, declared := applyAgentFailureMarker(job, outcome.Success, outcome.Error, outcome.FinalResponse)
		d := &runDelivery{job: job, success: success, err: errText, agentDeclared: declared}
		sideErr := s.saveComposeDeliver(runCtx, d, fence, outcome.FinalResponse, outcome.Output, o, token)
		deliveryAttempted, deliveryError = d.deliveryAttempted, d.deliveryError
		teardownDeferred()
		if sideErr != nil {
			if !errors.Is(sideErr, errFireClaimLostDuringSideEffect) {
				return false, sideErr
			}
			d.sideEffectOwnershipLost = true
		}
		if d.sideEffectOwnershipLost {
			return true, s.recordFireOwnershipLost(ctx, job, fireOwner, executionID)
		}
		failEmptyResponse(d, outcome.FinalResponse)
		if fence.lost(runCtx) {
			transportCancelled := fence.transportCancelled()
			if d.deliveryAttempted && d.deliveryError == nil && !transportCancelled {
				s.log.Warn(fmt.Sprintf("Job '%s': fire claim ownership lost after completed delivery; recording the delivered run's terminal status", job.ID))
			} else {
				if transportCancelled {
					s.log.Warn(fmt.Sprintf("Job '%s': transport cancellation arrived before terminal completion; recording the interrupted run", job.ID))
				}
				return true, s.recordFireOwnershipLost(ctx, job, fireOwner, executionID)
			}
		}
		if s.consumeInterruptedFlag(job.ID, token) {
			return true, s.finishInterruptedRun(ctx, job, executionID, deliveryError)
		}
		return s.finishCompletedRun(ctx, d, fireOwner, executionID, false)
	}()
	if bodyErr != nil {
		return fail(bodyErr, true)
	}
	return processed, nil
}

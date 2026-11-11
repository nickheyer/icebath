package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var jobIDRE = regexp.MustCompile(`^[a-f0-9]{12}$`)

// Fields a job update may carry
var updateAllowedFields = map[string]bool{"name": true, "schedule": true, "prompt": true, "deliver": true,
	"skills": true, "skill": true, "repeat": true, "enabled": true}

const (
	maxJobNameLength   = 200
	maxJobPromptLength = 5000
)

// Plain JSON error body of the jobs API
func jobsError(message string, status int) *gateway.WebResponse {
	return jsonResponse(pyjson.ObjectOf("error", message), status, nil)
}

// Jobs API prelude of auth, the optional drain check and job id validation
func (a *APIServerAdapter) cronRequestGuard(ctx context.Context, request *gateway.WebRequest, needJobID, checkDraining bool) (string, *gateway.WebResponse) {
	if authErr := a.checkAuth(ctx, request); authErr != nil {
		return "", authErr
	}
	if checkDraining {
		if draining := a.drainingResponse(); draining != nil {
			return "", draining
		}
	}
	if !needJobID {
		return "", nil
	}
	jobID := request.MatchInfo["job_id"]
	if !jobIDRE.MatchString(jobID) {
		apiLog().Warn(fmt.Sprintf("Cron jobs API rejected invalid job_id %s: %s", pyrepr.Repr(jobID), a.requestAuditLogSuffix(request)))
		return jobID, jobsError("Invalid job ID format", http.StatusBadRequest)
	}
	return jobID, nil
}

// 500 carrying the redacted error text
func cronErrorResponse(err error) *gateway.WebResponse {
	return jobsError(redactAPIErrorText(err.Error(), -1), http.StatusInternalServerError)
}

// Length cap and injection scan of a cron prompt
func validateCronPrompt(prompt string) *gateway.WebResponse {
	if len([]rune(prompt)) > maxJobPromptLength {
		return jobsError(fmt.Sprintf("Prompt must be ≤ %d characters", maxJobPromptLength), http.StatusBadRequest)
	}
	if prompt != "" {
		if scanErr := cron.ScanCronPrompt(prompt); scanErr != "" {
			return jobsError(scanErr, http.StatusBadRequest)
		}
	}
	return nil
}

// Notifies the active scheduler provider of a REST mutation, a failure ignored
func notifyCronProviderJobsChanged(ctx context.Context) {
	defer func() { _ = recover() }()
	cron.NotifyProviderJobsChanged(home.Dir(ctx))
}

// Job lookup or mutation answered as the job, 404 when none and 500 on failure
func jobResponse(ctx context.Context, fn func(jobID string) (*cron.Job, error), jobID string, notify bool) *gateway.WebResponse {
	job, err := func() (job *cron.Job, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return fn(jobID)
	}()
	if err != nil {
		return cronErrorResponse(err)
	}
	if job == nil {
		return jobsError("Job not found", http.StatusNotFound)
	}
	if notify {
		notifyCronProviderJobsChanged(ctx)
	}
	return jsonResponse(pyjson.ObjectOf("job", job), http.StatusOK, nil)
}

// Guarded lookup or mutation of one job by id
func (a *APIServerAdapter) jobLookupOrMutate(ctx context.Context, request *gateway.WebRequest, fn func(jobID string) (*cron.Job, error), notify bool) (*gateway.WebResponse, error) {
	jobID, errResp := a.cronRequestGuard(ctx, request, true, false)
	if errResp != nil {
		return errResp, nil
	}
	return jobResponse(ctx, fn, jobID, notify), nil
}

// Every cron job, disabled ones when asked
func (a *APIServerAdapter) handleListJobs(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if _, errResp := a.cronRequestGuard(ctx, request, false, false); errResp != nil {
		return errResp, nil
	}
	flag := strings.ToLower(request.QueryGet("include_disabled"))
	jobs, err := cron.ListJobs(ctx, home.Dir(ctx), flag == "true" || flag == "1")
	if err != nil {
		return cronErrorResponse(err), nil
	}
	list := make([]any, 0, len(jobs))
	for _, j := range jobs {
		list = append(list, j)
	}
	return jsonResponse(pyjson.ObjectOf("jobs", list), http.StatusOK, nil), nil
}

// Creates a cron job registered with the active scheduler provider
func (a *APIServerAdapter) handleCreateJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	if _, errResp := a.cronRequestGuard(ctx, request, false, false); errResp != nil {
		return errResp, nil
	}
	resp, err := func() (resp *gateway.WebResponse, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		raw, err := requestJSON(request)
		if err != nil {
			return nil, err
		}
		body, ok := raw.(*pyjson.Object)
		if !ok {
			return nil, &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
		}
		name := strings.TrimSpace(pyrepr.Str(pyrepr.Or(body.GetOr("name", nil), "")))
		schedule := strings.TrimSpace(pyrepr.Str(pyrepr.Or(body.GetOr("schedule", nil), "")))
		prompt := body.GetOr("prompt", "")
		skills := body.GetOr("skills", nil)
		repeat := body.GetOr("repeat", nil)
		if name == "" {
			return jobsError("Name is required", http.StatusBadRequest), nil
		}
		if len([]rune(name)) > maxJobNameLength {
			return jobsError(fmt.Sprintf("Name must be ≤ %d characters", maxJobNameLength), http.StatusBadRequest), nil
		}
		if schedule == "" {
			return jobsError("Schedule is required", http.StatusBadRequest), nil
		}
		promptText, isStr := prompt.(string)
		if !isStr {
			return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("object of type '%s' has no len()", pyrepr.TypeName(prompt))}
		}
		if promptErr := validateCronPrompt(promptText); promptErr != nil {
			return promptErr, nil
		}
		if repeat != nil {
			n, isInt := repeat.(int64)
			if _, isBool := repeat.(bool); isBool {
				isInt, n = true, map[bool]int64{true: 1, false: 0}[repeat.(bool)]
			}
			if !isInt || n < 1 {
				return jobsError("Repeat must be a positive integer", http.StatusBadRequest), nil
			}
		}
		spec := pyjson.ObjectOf("prompt", prompt, "schedule", schedule, "name", name,
			"deliver", body.GetOr("deliver", "local"), "origin", a.cronOriginFromRequest(request))
		for _, key := range []string{"paused", "paused_reason"} {
			if v, ok := body.Get(key); ok {
				spec.Set(key, v)
			}
		}
		if pyrepr.Truthy(skills) {
			spec.Set("skills", skills)
		}
		if repeat != nil {
			spec.Set("repeat", repeat)
		}
		job, err := cron.CreateJobWithSchedulerRegistration(ctx, home.Dir(ctx), spec)
		if err != nil {
			return nil, err
		}
		return jsonResponse(pyjson.ObjectOf("job", job), http.StatusOK, nil), nil
	}()
	if err != nil {
		var regErr *cron.CronSchedulerRegistrationError
		if errors.As(err, &regErr) {
			return jsonResponse(regErr.ToDict(), http.StatusFailedDependency, nil), nil
		}
		var typed interface{ PyTypeName() string }
		if errors.As(err, &typed) && typed.PyTypeName() == "ValueError" {
			return jobsError(err.Error(), http.StatusBadRequest), nil
		}
		return cronErrorResponse(err), nil
	}
	return resp, nil
}

// One cron job
func (a *APIServerAdapter) handleGetJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return a.jobLookupOrMutate(ctx, request, func(id string) (*cron.Job, error) { return cron.GetJob(ctx, home.Dir(ctx), id) }, false)
}

// Updates the whitelisted fields of a cron job
func (a *APIServerAdapter) handleUpdateJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	jobID, errResp := a.cronRequestGuard(ctx, request, true, false)
	if errResp != nil {
		return errResp, nil
	}
	sanitized := map[string]any{}
	resp, err := func() (resp *gateway.WebResponse, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		raw, err := requestJSON(request)
		if err != nil {
			return nil, err
		}
		body, ok := raw.(*pyjson.Object)
		if !ok {
			return nil, &gateway.PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'items'", pyrepr.TypeName(raw))}
		}
		for _, k := range body.Keys {
			if updateAllowedFields[k] {
				v, _ := body.Get(k)
				sanitized[k] = pyjson.Plain(v)
			}
		}
		if len(sanitized) == 0 {
			return jobsError("No valid fields to update", http.StatusBadRequest), nil
		}
		if name, ok := sanitized["name"]; ok {
			if len([]rune(pyrepr.Str(name))) > maxJobNameLength {
				return jobsError(fmt.Sprintf("Name must be ≤ %d characters", maxJobNameLength), http.StatusBadRequest), nil
			}
		}
		if prompt, ok := sanitized["prompt"]; ok {
			text, isStr := prompt.(string)
			if !isStr {
				return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("object of type '%s' has no len()", pyrepr.TypeName(prompt))}
			}
			if promptErr := validateCronPrompt(text); promptErr != nil {
				return promptErr, nil
			}
		}
		return nil, nil
	}()
	if err != nil {
		return cronErrorResponse(err), nil
	}
	if resp != nil {
		return resp, nil
	}
	return jobResponse(ctx, func(id string) (*cron.Job, error) { return cron.UpdateJob(ctx, home.Dir(ctx), id, sanitized) }, jobID, true), nil
}

// Deletes a cron job
func (a *APIServerAdapter) handleDeleteJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	jobID, errResp := a.cronRequestGuard(ctx, request, true, false)
	if errResp != nil {
		return errResp, nil
	}
	removed, err := cron.RemoveJob(ctx, home.Dir(ctx), jobID)
	if err != nil {
		return cronErrorResponse(err), nil
	}
	if !removed {
		return jobsError("Job not found", http.StatusNotFound), nil
	}
	notifyCronProviderJobsChanged(ctx)
	return jsonResponse(pyjson.ObjectOf("ok", true), http.StatusOK, nil), nil
}

// Pauses a cron job
func (a *APIServerAdapter) handlePauseJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return a.jobLookupOrMutate(ctx, request, func(id string) (*cron.Job, error) { return cron.PauseJob(ctx, home.Dir(ctx), id, nil) }, true)
}

// Resumes a paused cron job
func (a *APIServerAdapter) handleResumeJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	return a.jobLookupOrMutate(ctx, request, func(id string) (*cron.Job, error) { return cron.ResumeJob(ctx, home.Dir(ctx), id) }, true)
}

// Triggers a cron job now with an optional extra prompt for this run
func (a *APIServerAdapter) handleRunJob(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	jobID, errResp := a.cronRequestGuard(ctx, request, true, true)
	if errResp != nil {
		return errResp, nil
	}
	extraPrompt := ""
	if raw, err := requestJSON(request); err == nil {
		if body, ok := raw.(*pyjson.Object); ok {
			if rawPrompt := body.GetOr("prompt", nil); rawPrompt != nil {
				extraPrompt = pyrepr.Str(rawPrompt)
				if promptErr := validateCronPrompt(extraPrompt); promptErr != nil {
					return promptErr, nil
				}
			}
		}
	}
	return jobResponse(ctx, func(id string) (*cron.Job, error) {
		return cron.TriggerJobWithPrompt(ctx, home.Dir(ctx), id, extraPrompt)
	}, jobID, false), nil
}

// Chronos fire webhook authenticated by a NAS minted token, the run detached behind a 202
func (a *APIServerAdapter) handleCronFire(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	receivedAt := time.Now()
	auth := request.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(auth[7:])
	}
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		return nil, err
	}
	setting := func(keys ...string) string {
		v, ok := config.CfgGet(cfg, keys...)
		if !ok || v == nil {
			return ""
		}
		return pyrepr.Str(v)
	}
	verifier := cron.GetFireVerifier()
	claims := func() (claims *pyjson.Object) {
		defer func() {
			if rec := recover(); rec != nil {
				apiLog().Error("cron fire: verifier crashed; rejecting token", "error", rec)
				claims = nil
			}
		}()
		return verifier(cron.NASFireTokenOptions{Token: token, ExpectedAudience: setting("cron", "chronos", "expected_audience"),
			JWKSOrKey: setting("cron", "chronos", "nas_jwks_url"), Issuer: setting("cron", "chronos", "portal_url")})
	}()
	if claims == nil {
		apiLog().Warn("cron fire: rejected invalid token: " + a.requestAuditLogSuffix(request))
		return jobsError("invalid fire token", http.StatusUnauthorized), nil
	}
	if draining := a.drainingResponse(); draining != nil {
		return draining, nil
	}
	reservation, release := a.reservePendingAPIWork()
	defer release()
	var jobID any
	if raw, err := requestJSON(request); err == nil {
		if body, ok := raw.(*pyjson.Object); ok {
			jobID = body.GetOr("job_id", nil)
		}
	}
	if !pyrepr.Truthy(jobID) {
		return jobsError("missing job_id", http.StatusBadRequest), nil
	}
	if tools.CheckPaused(ctx, "cron-webhook", apiLog()) {
		return jsonResponse(pyjson.ObjectOf("error", "icebath is paused (ESTOP)", "job_id", jobID), http.StatusServiceUnavailable,
			map[string]string{"Retry-After": strconv.Itoa(60)}), nil
	}
	jobIDText := pyrepr.Str(jobID)
	scheduler := cron.ResolveCronScheduler(h)
	refusal, adapters, err := gateway.APILiveAdaptersOnceStarted(ctx, a, a.app, jobIDText, receivedAt)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return refusal, nil
	}
	detach := func(fire func(ctx context.Context) error) *gateway.WebResponse {
		reservation.detached = true
		task := startAgentTask(context.WithoutCancel(ctx), func(taskCtx context.Context) {
			defer a.releasePendingAPIWork(reservation)
			result := <-submitAPIWorker(func() (struct{}, error) { return struct{}{}, fire(taskCtx) })
			if result.Panic != nil {
				apiLog().Error(fmt.Sprintf("cron fire for %s raised", jobIDText), "error", result.Panic)
			} else if result.Err != nil {
				apiLog().Error(fmt.Sprintf("cron fire for %s failed", jobIDText), "error", result.Err)
			}
		})
		a.trackBackgroundTask(task)
		return jsonResponse(pyjson.ObjectOf("status", "accepted", "job_id", jobID), http.StatusAccepted, nil)
	}
	opts := cron.FireOptions{Adapters: adapters}
	if !cron.ProviderSupportsSplitFire(scheduler) {
		return detach(func(fireCtx context.Context) error {
			_, err := cron.SchedulerFireDue(fireCtx, scheduler, jobIDText, opts)
			return err
		}), nil
	}
	claimed, err := cron.SchedulerClaimFire(ctx, scheduler, jobIDText)
	if err != nil {
		apiLog().Error(fmt.Sprintf("cron fire admission failed for %s: %v", jobIDText, err))
		return jsonResponse(pyjson.ObjectOf("error", "cron fire admission failed", "job_id", jobID), http.StatusServiceUnavailable, nil), nil
	}
	if claimed == nil {
		return jsonResponse(pyjson.ObjectOf("status", "duplicate", "job_id", jobID), http.StatusOK, nil), nil
	}
	return detach(func(fireCtx context.Context) error {
		_, err := cron.SchedulerFireClaimed(fireCtx, scheduler, claimed, opts)
		return err
	}), nil
}

// Keeps a background task referenced until it finishes
func (a *APIServerAdapter) trackBackgroundTask(task *agentTask) {
	a.mu.Lock()
	a.backgroundTasks[task] = struct{}{}
	a.mu.Unlock()
	go func() {
		task.Wait()
		a.mu.Lock()
		delete(a.backgroundTasks, task)
		a.mu.Unlock()
	}()
}

// Request body decoded as JSON the way aiohttp request.json does
func requestJSON(request *gateway.WebRequest) (any, error) {
	body, err := request.Read()
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(string(body))
}

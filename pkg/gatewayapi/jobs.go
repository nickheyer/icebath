package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	return jsonResponse(jsonutil.ObjectOf("error", message), status, nil)
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
		pkgLog().Warn(fmt.Sprintf("Cron jobs API rejected invalid job_id %s: %s", anyutil.Quote(jobID), a.requestAuditLogSuffix(request)))
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
	return jsonResponse(jsonutil.ObjectOf("job", job), http.StatusOK, nil)
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
	return jsonResponse(jsonutil.ObjectOf("jobs", list), http.StatusOK, nil), nil
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
		body, ok := raw.(*jsonutil.Object)
		if !ok {
			return nil, &gateway.PyError{Kind: "AttributeError", Msg: "expected an object, got " + anyutil.Kind(raw)}
		}
		name := strings.TrimSpace(anyutil.String(anyutil.Or(body.ValueOr("name", nil), "")))
		schedule := strings.TrimSpace(anyutil.String(anyutil.Or(body.ValueOr("schedule", nil), "")))
		prompt := body.ValueOr("prompt", "")
		skills := body.ValueOr("skills", nil)
		repeat := body.ValueOr("repeat", nil)
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
			return nil, &gateway.PyError{Kind: "TypeError", Msg: "prompt must be a string, got " + anyutil.Kind(prompt)}
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
		spec := jsonutil.ObjectOf("prompt", prompt, "schedule", schedule, "name", name,
			"deliver", body.ValueOr("deliver", "local"), "origin", a.cronOriginFromRequest(request))
		for _, key := range []string{"paused", "paused_reason"} {
			if v, ok := body.Get(key); ok {
				spec.Set(key, v)
			}
		}
		if anyutil.IsTrue(skills) {
			spec.Set("skills", skills)
		}
		if repeat != nil {
			spec.Set("repeat", repeat)
		}
		job, err := cron.CreateJobWithSchedulerRegistration(ctx, home.Dir(ctx), spec)
		if err != nil {
			return nil, err
		}
		return jsonResponse(jsonutil.ObjectOf("job", job), http.StatusOK, nil), nil
	}()
	if err != nil {
		var regErr *cron.CronSchedulerRegistrationError
		if errors.As(err, &regErr) {
			return jsonResponse(regErr.ToDict(), http.StatusFailedDependency, nil), nil
		}
		if errorutil.Kind(err) == "ValueError" {
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
		body, ok := raw.(*jsonutil.Object)
		if !ok {
			return nil, &gateway.PyError{Kind: "AttributeError", Msg: "expected an object, got " + anyutil.Kind(raw)}
		}
		for _, k := range body.Keys() {
			if updateAllowedFields[k] {
				v, _ := body.Get(k)
				sanitized[k] = jsonutil.Plain(v)
			}
		}
		if len(sanitized) == 0 {
			return jobsError("No valid fields to update", http.StatusBadRequest), nil
		}
		if name, ok := sanitized["name"]; ok {
			if len([]rune(anyutil.String(name))) > maxJobNameLength {
				return jobsError(fmt.Sprintf("Name must be ≤ %d characters", maxJobNameLength), http.StatusBadRequest), nil
			}
		}
		if prompt, ok := sanitized["prompt"]; ok {
			text, isStr := prompt.(string)
			if !isStr {
				return nil, &gateway.PyError{Kind: "TypeError", Msg: "prompt must be a string, got " + anyutil.Kind(prompt)}
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
	return jsonResponse(jsonutil.ObjectOf("ok", true), http.StatusOK, nil), nil
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
		if body, ok := raw.(*jsonutil.Object); ok {
			if rawPrompt := body.ValueOr("prompt", nil); rawPrompt != nil {
				extraPrompt = anyutil.String(rawPrompt)
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
	return jsonutil.Decode(string(body))
}

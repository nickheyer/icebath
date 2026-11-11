package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const cronFireRetryAfterSeconds = 60

// Job fields that decide whether a job still has something to run
var cronExecutionFields = []string{"prompt", "skill", "skills", "script", "no_agent"}

// 404 of a job no profile holds
func jobNotFound() error { return NewHTTPException(404, "Job not found") }

// Whether an error is a ValueError the cron store or a blueprint raised
func isCronValueError(err error) bool {
	var ve *cron.ValueError
	var fill *cron.BlueprintFillError
	var blocked *tools.GatewayLifecycleBlocked
	return errors.As(err, &ve) || errors.As(err, &fill) || errors.As(err, &blocked)
}

// Dashboard update payload in the storage shape update_job takes
func normalizeDashboardCronUpdates(updates *pyjson.Object, profileHome string) (*pyjson.Object, error) {
	normalized := pyjson.NewObject()
	if updates != nil {
		normalized = updates.Copy()
	}
	for _, key := range []string{"model", "provider", "workdir"} {
		if v, ok := normalized.Get(key); ok {
			normalized.Set(key, optAny(cronOptionalText(v, false)))
		}
	}
	if v, ok := normalized.Get("script"); ok {
		script, err := normalizeDashboardCronScript(v, profileHome)
		if err != nil {
			return nil, err
		}
		normalized.Set("script", script)
	}
	if v, ok := normalized.Get("base_url"); ok {
		normalized.Set("base_url", optAny(cronOptionalText(v, true)))
	}
	if v, ok := normalized.Get("deliver"); ok {
		deliver := cronOptionalText(v, false)
		if deliver == nil {
			normalized.Set("deliver", "local")
		} else {
			normalized.Set("deliver", *deliver)
		}
	}
	if v, ok := normalized.Get("failure_deliver"); ok {
		normalized.Set("failure_deliver", optAny(cronOptionalText(v, false)))
	}
	for _, key := range []string{"context_from", "enabled_toolsets"} {
		if v, ok := normalized.Get(key); ok {
			normalized.Set(key, listOrNone(cronStringList(v)))
		}
	}
	return normalized, nil
}

// Profile holding the job, the hint first when it holds it, empty when none does
func jobOwnerProfile(ctx context.Context, jobID, profile string) (string, error) {
	if profile != "" {
		jobs, err := cronListJobs(ctx, profile)
		if err != nil {
			return "", err
		}
		if cronJobsHold(jobs, jobID) {
			return profile, nil
		}
	}
	return findCronJobProfile(ctx, jobID)
}

// Owning profile of a job, 404 when no profile holds it
func jobProfile(ctx context.Context, jobID, profile string) (string, error) {
	selected, err := jobOwnerProfile(ctx, jobID, profile)
	if err != nil {
		return "", err
	}
	if selected == "" {
		return "", jobNotFound()
	}
	return selected, nil
}

// The job dict, 404 when the store returned none
func foundJob(result any, err error) (*pyjson.Object, error) {
	if err != nil {
		return nil, err
	}
	job, _ := result.(*pyjson.Object)
	if job == nil {
		return nil, jobNotFound()
	}
	return job, nil
}

// Jobs of one profile, or of every profile with duplicate ids resolved toward the default profile copy
func listCronJobsSync(ctx context.Context, profile string) ([]*pyjson.Object, error) {
	requested := profile
	if requested == "" {
		requested = "all"
	}
	requested = pystr.Strip(requested)
	if pystr.Lower(requested) != "all" {
		return cronListJobs(ctx, requested)
	}
	var allJobs []*pyjson.Object
	for _, item := range cronProfileDicts(ctx) {
		name := pyrepr.Str(pyrepr.Or(item.GetOr("name", nil), ""))
		if name == "" {
			continue
		}
		jobs, err := cronListJobs(ctx, name)
		if err != nil {
			webServerLog().Error(fmt.Sprintf("Failed to list cron jobs for profile %s", name), "exc", err)
			continue
		}
		allJobs = append(allJobs, jobs...)
	}
	var order []string
	byID := map[string]*pyjson.Object{}
	unkeyed := []*pyjson.Object{}
	for _, job := range allJobs {
		jid := job.GetOr("id", nil)
		if !pyrepr.Truthy(jid) {
			jid = job.GetOr("job_id", nil)
		}
		if !pyrepr.Truthy(jid) || jid == "unknown" {
			unkeyed = append(unkeyed, job)
			continue
		}
		key := pyrepr.Repr(jid)
		existing, seen := byID[key]
		if !seen {
			order = append(order, key)
			byID[key] = job
			continue
		}
		if pyrepr.Truthy(job.GetOr("is_default_profile", nil)) && !pyrepr.Truthy(existing.GetOr("is_default_profile", nil)) {
			byID[key] = job
		}
	}
	out := make([]*pyjson.Object, 0, len(order)+len(unkeyed))
	for _, key := range order {
		out = append(out, byID[key])
	}
	return append(out, unkeyed...), nil
}

// One job of its owning profile
func getCronJobSync(ctx context.Context, jobID, profile string) (*pyjson.Object, error) {
	selected, err := jobProfile(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	return foundJob(callCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.GetJob(ctx, h, jobID)
	}))
}

// Applies a validated dashboard update to a job of its owning profile
func updateCronJobSync(ctx context.Context, jobID string, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	selected, err := jobProfile(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	job, err := updateCronJobChecked(ctx, jobID, body, selected)
	if err != nil {
		if _, ok := asHTTPException(err); ok {
			return nil, err
		}
		if isCronValueError(err) {
			return nil, NewHTTPException(400, err.Error())
		}
		return nil, err
	}
	return foundJob(job, nil)
}

// Normalization, validation and store update of one job, raising every failure
func updateCronJobChecked(ctx context.Context, jobID string, body *pyjson.Object, selected string) (any, error) {
	profileName, profileHome, err := cronProfileHome(ctx, selected)
	if err != nil {
		return nil, err
	}
	existing, err := foundJob(callCronForProfile(ctx, profileName, func(ctx context.Context, h string) (any, error) {
		return cron.GetJob(ctx, h, jobID)
	}))
	if err != nil {
		return nil, err
	}
	rawUpdates, _ := body.GetOr("updates", nil).(*pyjson.Object)
	updates, err := normalizeDashboardCronUpdates(rawUpdates, profileHome)
	if err != nil {
		return nil, err
	}
	if v, ok := updates.Get("context_from"); ok {
		refs, _ := v.([]any)
		if err := validateDashboardCronContextFrom(ctx, refs, profileName); err != nil {
			return nil, err
		}
	}
	touchesExecution := false
	for _, key := range cronExecutionFields {
		if _, ok := updates.Get(key); ok {
			touchesExecution = true
		}
	}
	if touchesExecution {
		effective := map[string]any{}
		for _, k := range existing.Keys {
			effective[k] = existing.Vals[k]
		}
		for _, k := range updates.Keys {
			effective[k] = updates.Vals[k]
		}
		_, hasSkills := updates.Get("skills")
		if _, hasSkill := updates.Get("skill"); hasSkills && !hasSkill {
			effective["skill"] = nil
		}
		if err := validateDashboardCronEffectiveJob(effective); err != nil {
			return nil, err
		}
	}
	plain := make(map[string]any, updates.Len())
	for _, k := range updates.Keys {
		plain[k] = updates.Vals[k]
	}
	return mutateCronForProfile(ctx, profileName, func(ctx context.Context, h string) (any, error) {
		return cron.UpdateJob(ctx, h, jobID, plain)
	})
}

// Pauses a job of its owning profile
func pauseCronJobSync(ctx context.Context, jobID, profile string) (*pyjson.Object, error) {
	selected, err := jobProfile(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	return foundJob(mutateCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.PauseJob(ctx, h, jobID, nil)
	}))
}

// Resumes a job of its owning profile
func resumeCronJobSync(ctx context.Context, jobID, profile string) (*pyjson.Object, error) {
	selected, err := jobProfile(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	return foundJob(mutateCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.ResumeJob(ctx, h, jobID)
	}))
}

// Job of its owning profile resolved by id or name, 404 when absent
func resolveOwnedCronJob(ctx context.Context, jobID, profile string) (string, *pyjson.Object, error) {
	selected, err := jobProfile(ctx, jobID, profile)
	if err != nil {
		return "", nil, err
	}
	job, err := foundJob(callCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.ResolveJobRef(ctx, h, jobID)
	}))
	return selected, job, err
}

// Runs a job now through the shared durable claim, forcing a paused one, 409 when another runner holds it
func triggerCronJobSync(ctx context.Context, jobID, profile string) (*pyjson.Object, error) {
	selected, job, err := resolveOwnedCronJob(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	enabled, hasEnabled := job.Get("enabled")
	force := (hasEnabled && !pyrepr.Truthy(enabled)) || job.GetOr("state", nil) == "paused"
	canonical := pyrepr.Str(job.GetOr("id", nil))
	ran, err := fireCronJobForProfile(ctx, selected, canonical, force)
	if err != nil {
		return nil, err
	}
	refreshedAny, err := callCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.GetJob(ctx, h, canonical)
	})
	if err != nil {
		return nil, err
	}
	refreshed, _ := refreshedAny.(*pyjson.Object)
	if refreshed != nil && !pyjson.Equal(refreshed.GetOr("last_run_at", nil), job.GetOr("last_run_at", nil)) {
		return refreshed, nil
	}
	if !ran {
		return nil, NewHTTPException(409, "Job is already running or was claimed by another scheduler")
	}
	if refreshed != nil {
		return refreshed, nil
	}
	out := job.Copy()
	out.Set("enabled", false)
	out.Set("state", "completed")
	return out, nil
}

// Stops the live run of a job, cancelled false when nothing is in flight
func cancelCronJobSync(ctx context.Context, jobID, profile string) (*pyjson.Object, error) {
	selected, job, err := resolveOwnedCronJob(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	canonical := pyrepr.Str(job.GetOr("id", nil))
	result, err := callCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.CancelJobRun(h, canonical, "cancelled on request (Stop)")
	})
	if err != nil {
		return nil, err
	}
	out := result.(*pyjson.Object).Copy()
	out.Set("job_id", job.GetOr("id", nil))
	return out, nil
}

// Removes a job of its owning profile, 404 when nothing was removed
func deleteCronJobSync(ctx context.Context, jobID, profile string) (*pyjson.Object, error) {
	selected, err := jobProfile(ctx, jobID, profile)
	if err != nil {
		return nil, err
	}
	removed, err := mutateCronForProfile(ctx, selected, func(ctx context.Context, h string) (any, error) {
		return cron.RemoveJob(ctx, h, jobID)
	})
	if err != nil {
		if isCronValueError(err) {
			return nil, NewHTTPException(400, err.Error())
		}
		return nil, err
	}
	if !pyrepr.Truthy(removed) {
		return nil, jobNotFound()
	}
	return pyjson.ObjectOf("ok", true), nil
}

// Optional profile query value as text, empty for None
func profileQuery(c *Call) string {
	s, _ := c.Value("profile").(string)
	return s
}

// Text of a config value read with a default, the default for a missing or null value
func cfgText(cfg map[string]any, def string, keys ...string) string {
	v, ok := config.CfgGet(cfg, keys...)
	if !ok || v == nil {
		return def
	}
	return pyrepr.Str(v)
}

// Chronos fire webhook forwarded to the gateway api_server, 503 with Retry-After while it is unreachable
func cronFireWebhook(ctx context.Context, c *Call) (any, error) {
	auth := c.Request.Headers().Get("Authorization")
	token := ""
	if strings.HasPrefix(auth, "Bearer ") {
		token = pystr.Strip(auth[7:])
	}
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	claims := cron.GetFireVerifier()(cron.NASFireTokenOptions{
		Token:            token,
		ExpectedAudience: cfgText(cfg, "", "cron", "chronos", "expected_audience"),
		JWKSOrKey:        cfgText(cfg, "", "cron", "chronos", "nas_jwks_url"),
		Issuer:           cfgText(cfg, "", "cron", "chronos", "portal_url"),
	})
	if claims == nil {
		return JSONResponse(pyjson.ObjectOf("error", "invalid fire token"), 401)
	}
	var jobID any
	if body, err := c.Request.JSON(ctx); err == nil {
		if obj, ok := body.(*pyjson.Object); ok {
			jobID = obj.GetOr("job_id", nil)
		}
	}
	if !pyrepr.Truthy(jobID) {
		return JSONResponse(pyjson.ObjectOf("error", "missing job_id"), 400)
	}
	profile := ""
	if id, ok := jobID.(string); ok {
		if profile, err = findCronJobProfile(ctx, id); err != nil {
			return nil, err
		}
	}
	if profile == "" {
		return JSONResponse(pyjson.ObjectOf("status", "gone", "job_id", jobID), 200)
	}
	idText := jobID.(string)
	status, gatewayBody, err := forwardCronFireToGateway(ctx, profile, idText, auth)
	if err != nil {
		return nil, err
	}
	retryAfter := strconv.Itoa(cronFireRetryAfterSeconds)
	if status == nil {
		if _, err := callCronForProfile(ctx, profile, func(ctx context.Context, h string) (any, error) {
			return cron.NoteFireForwardFailure(ctx, h, idText, "scheduled fire could not be forwarded to the gateway "+
				"api_server (127.0.0.1 loopback unreachable); the gateway "+
				"process may be down or its api_server adapter not bound "+
				"(missing API_SERVER_KEY)")
		}); err != nil {
			webServerLog().Debug(fmt.Sprintf("could not stamp last_fire_error for %s", idText), "exc", err)
		}
		if gatewayIntentionallyStopped(ctx, profile) {
			webServerLog().Info(fmt.Sprintf("cron fire dropped: gateway for profile %s is deliberately stopped (desired_state=stopped); "+
				"job %s will resume via Chronos reconcile on next gateway start", pyrepr.Repr(profile), idText))
			return JSONResponse(pyjson.ObjectOf(
				"status", "gateway_stopped",
				"detail", "gateway deliberately stopped; fire dropped, jobs re-arm on next gateway start",
				"job_id", jobID,
				"profile", profile,
			), 200)
		}
		return JSONResponse(pyjson.ObjectOf("error", "gateway unreachable; retry", "job_id", jobID, "profile", profile), 503, "Retry-After", retryAfter)
	}
	if _, ok := gatewayBody.Get("job_id"); !ok {
		gatewayBody.Set("job_id", jobID)
	}
	if *status == 503 {
		return JSONResponse(gatewayBody, *status, "Retry-After", retryAfter)
	}
	return JSONResponse(gatewayBody, *status)
}

// Blueprint catalog of a profile with the deliver slot offering its configured gateway platforms
func blueprintCatalogSync(ctx context.Context, profile string) ([]any, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	catalog := cron.ListBlueprints(sctx)
	deliverOptions := []any{"origin", "local"}
	for _, t := range cron.CronDeliveryTargets(sctx) {
		if id := t.GetOr("id", nil); pyrepr.Truthy(id) {
			deliverOptions = append(deliverOptions, id)
		}
	}
	entries := make([]any, 0, len(catalog))
	for i := range catalog {
		entry := cron.BlueprintCatalogEntry(&catalog[i])
		fields, _ := entry.GetOr("fields", nil).([]any)
		for _, f := range fields {
			if field, ok := f.(*pyjson.Object); ok && field.GetOr("name", nil) == "deliver" {
				field.Set("options", append([]any(nil), deliverOptions...))
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Blueprint of a profile by key, nil when unknown
func blueprintForProfile(ctx context.Context, key, profile string) (*cron.AutomationBlueprint, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	return cron.GetBlueprint(sctx, key), nil
}

// Fills a blueprint form and creates its cron job in the profile
func instantiateBlueprint(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	key := pyrepr.Str(body.GetOr("blueprint", nil))
	blueprint, err := blueprintForProfile(ctx, key, profile)
	if err != nil {
		return nil, err
	}
	if blueprint == nil {
		return nil, NewHTTPException(404, fmt.Sprintf("Unknown blueprint: %s", key))
	}
	values := map[string]any{}
	if obj, ok := body.GetOr("values", nil).(*pyjson.Object); ok {
		for _, k := range obj.Keys {
			values[k] = obj.Vals[k]
		}
	}
	spec, err := cron.FillBlueprint(blueprint, values, nil)
	if err != nil {
		var fill *cron.BlueprintFillError
		if errors.As(err, &fill) {
			return nil, NewHTTPException(422, err.Error())
		}
		return nil, err
	}
	spec.Delete("origin")
	created, err := callCronForProfile(ctx, profile, func(ctx context.Context, h string) (any, error) {
		return cron.CreateJobWithSchedulerRegistration(ctx, h, spec)
	})
	if err != nil {
		return nil, err
	}
	notifyCronProviderForProfile(ctx, profile)
	return created, nil
}

// Router of the cron job, run history, fire webhook and blueprint routes
func cronRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	jobPath := PathParam("job_id", Str)
	r.Get("/api/cron/jobs", &Endpoint{
		Name:   "list_cron_jobs",
		Params: []Param{QueryParam("profile", Str, "all")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			jobs, err := listCronJobsSync(ctx, c.Str("profile"))
			if err != nil {
				return nil, err
			}
			out := make([]any, len(jobs))
			for i, j := range jobs {
				out[i] = j
			}
			return out, nil
		},
	})
	r.Get("/api/cron/jobs/{job_id}", &Endpoint{
		Name:   "get_cron_job",
		Params: []Param{jobPath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getCronJobSync(ctx, c.Str("job_id"), profileQuery(c))
		},
	})
	r.Get("/api/cron/jobs/{job_id}/runs", &Endpoint{
		Name:   "list_cron_job_runs",
		Params: []Param{jobPath, optProfile, QueryParam("limit", Int, int64(20))},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return listCronJobRunsSync(ctx, c.Str("job_id"), profileQuery(c), c.Int("limit"))
		},
	})
	r.Post("/api/cron/jobs", &Endpoint{
		Name:   "create_cron_job",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", cronJobCreateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return createCronJobSync(ctx, c.Object("body"), c.FieldsSet("body", cronJobCreateModel), profileQuery(c))
		},
	})
	r.Get("/api/cron/delivery-targets", &Endpoint{
		Name:   "get_cron_delivery_targets",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			targets := []any{pyjson.ObjectOf("id", "local", "name", "Local (save only)", "home_target_set", true, "home_env_var", nil)}
			sctx, _, err := configProfileScope(ctx, profileQuery(c))
			if err != nil {
				return nil, err
			}
			for _, t := range cron.CronDeliveryTargets(sctx) {
				targets = append(targets, t)
			}
			return pyjson.ObjectOf("targets", targets), nil
		},
	})
	r.Put("/api/cron/jobs/{job_id}", &Endpoint{
		Name:   "update_cron_job",
		Params: []Param{jobPath, optProfile},
		Body:   []BodyParam{Body("body", cronJobUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return updateCronJobSync(ctx, c.Str("job_id"), c.Object("body"), profileQuery(c))
		},
	})
	for _, op := range []struct {
		path, name string
		fn         func(context.Context, string, string) (*pyjson.Object, error)
	}{
		{"/api/cron/jobs/{job_id}/pause", "pause_cron_job", pauseCronJobSync},
		{"/api/cron/jobs/{job_id}/resume", "resume_cron_job", resumeCronJobSync},
		{"/api/cron/jobs/{job_id}/trigger", "trigger_cron_job", triggerCronJobSync},
		{"/api/cron/jobs/{job_id}/cancel", "cancel_cron_job", cancelCronJobSync},
	} {
		fn := op.fn
		r.Post(op.path, &Endpoint{
			Name:   op.name,
			Params: []Param{jobPath, optProfile},
			Handle: func(ctx context.Context, c *Call) (any, error) {
				return fn(ctx, c.Str("job_id"), profileQuery(c))
			},
		})
	}
	r.Delete("/api/cron/jobs/{job_id}", &Endpoint{
		Name:   "delete_cron_job",
		Params: []Param{jobPath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return deleteCronJobSync(ctx, c.Str("job_id"), profileQuery(c))
		},
	})
	r.Post("/api/cron/fire", &Endpoint{Name: "cron_fire_webhook", Handle: cronFireWebhook})
	r.Get("/api/cron/blueprints", &Endpoint{
		Name:   "list_cron_blueprints",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			entries, err := blueprintCatalogSync(ctx, profileQuery(c))
			if err != nil {
				if _, ok := asHTTPException(err); ok {
					return nil, err
				}
				webServerLog().Error("GET /api/cron/blueprints failed", "exc", err)
				return nil, NewHTTPException(500, pyrepr.ExceptionMessage(err))
			}
			return pyjson.ObjectOf("blueprints", entries), nil
		},
	})
	r.Post("/api/cron/blueprints/instantiate", &Endpoint{
		Name:   "instantiate_blueprint",
		Params: []Param{QueryParam("profile", Str, "default")},
		Body:   []BodyParam{Body("body", automationBlueprintInstantiateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			created, err := instantiateBlueprint(ctx, c.Object("body"), c.Str("profile"))
			if err != nil {
				if _, ok := asHTTPException(err); ok {
					return nil, err
				}
				if regErr := raiseIfCronRegistrationError(err); regErr != nil {
					return nil, regErr
				}
				webServerLog().Error("POST /api/cron/blueprints/instantiate failed", "exc", err)
				return nil, NewHTTPException(400, pyrepr.ExceptionMessage(err))
			}
			return created, nil
		},
	})
	return r
}

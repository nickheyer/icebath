package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	cronRunHeartbeatInterval = 10 * time.Second
	cronRunHeartbeatCeiling  = 6 * time.Hour
)

const alreadyRunningError = "Job is already running (a scheduler tick or another " +
	"manual run is executing it); not started again."

// GatewayRunner is the live gateway a manual run borrows adapters and the event loop from
type GatewayRunner interface {
	AdaptersForProfile(profile *string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	IsSharedBotSatellite(ctx context.Context, profileName string, platform gateway.Platform) bool
	GatewayLoop() gateway.EventLoop
}

var gatewayRunnerRef func() GatewayRunner

// SetGatewayRunnerRef installs the accessor of the running gateway in this process
func SetGatewayRunnerRef(fn func() GatewayRunner) { gatewayRunnerRef = fn }

// Running gateway of this process or nil outside the gateway
func currentGatewayRunner() GatewayRunner {
	if gatewayRunnerRef == nil {
		return nil
	}
	return gatewayRunnerRef()
}

// A null the model sent for an argument whose omitted default is not None
type explicitNone struct{}

// Model tool arguments of one cronjob call, nil standing for an omitted or null value
type cronjobArgs struct {
	Action, JobID, Prompt, Schedule, Name, Repeat, Deliver               any
	IncludeDisabled                                                      bool
	Skill, Skills, Model, Provider, BaseURL, Reason, Script, ContextFrom any
	Continuity, EnabledToolsets, Workdir, NoAgent, AttachToSession       any
	MonitorScript, MonitorURL, ReasoningEffort, FailureDeliver, CatchUp  any
	SessionID                                                            any
	Paused, PausedReason, Pinned, Interpreter, Timezone, GraceMinutes    any
	env                                                                  *tools.Env
}

// JSON with two space indent and ASCII escapes like json dumps indent 2
func cronDumps(payload *pyjson.Object) string {
	out, err := pyjson.Dumps(payload, pyjson.Indent(2))
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return out
}

// Tool error JSON with the bounded message and success false plus extra keys
func cronToolError(message string, extra *pyjson.Object) string {
	out := pyjson.NewObject()
	out.Set("error", tools.BoundErrorText(message))
	out.Set("success", false)
	if extra != nil {
		for _, k := range extra.Keys {
			out.Set(k, extra.Vals[k])
		}
	}
	text, err := pyjson.Dumps(out, pyjson.EnsureASCII(false))
	if err != nil {
		return fmt.Sprintf(`{"error": %q, "success": false}`, message)
	}
	return text
}

// Ordered object from alternating keys and values
func pyObj(kv ...any) *pyjson.Object {
	out := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		out.Set(kv[i].(string), kv[i+1])
	}
	return out
}

// Copies every key of src onto dst in order
func mergeObj(dst, src *pyjson.Object) *pyjson.Object {
	for _, k := range src.Keys {
		dst.Set(k, src.Vals[k])
	}
	return dst
}

// Delivery platforms of the job that the relay connector fronts
func (s *Store) relayFrontedDeliveryPlatforms(ctx context.Context, job *Job) map[string]bool {
	fronted := relayFrontedPlatformsSeam(ctx)
	out := map[string]bool{}
	if len(fronted) == 0 {
		return out
	}
	for _, t := range resolveDeliveryTargets(ctx, s, job, false) {
		if t.Platform != "" && fronted[t.Platform] {
			out[t.Platform] = true
		}
	}
	return out
}

// Base URL of the local api_server following its bind resolution
func (s *Store) apiServerBaseURL() string {
	port := 8642
	if raw := strings.TrimSpace(os.Getenv("API_SERVER_PORT")); raw != "" {
		if n, ok := pyint.Parse(raw); ok && n.IsInt64() {
			port = int(n.Int64())
		}
	}
	host := ""
	if cfg, err := s.loadConfig(); err == nil {
		var node any = map[string]any(cfg)
		for _, k := range []string{"platforms", "api_server", "extra", "host"} {
			m, ok := node.(map[string]any)
			if !ok {
				node = nil
				break
			}
			node = m[k]
		}
		if pyrepr.Truthy(node) {
			host = strings.TrimSpace(pyrepr.Str(node))
		}
	}
	if host == "" {
		host = strings.TrimSpace(os.Getenv("API_SERVER_HOST"))
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

var cronForwardHTTPClient = &http.Client{Timeout: 10 * time.Second}

// Relay fronted platform set of the running gateway, tests replace it
var relayFrontedPlatformsSeam = gateway.RelayFrontedPlatforms

// Forwards a manual run of a relay fronted job to the running gateway, empty when forwarding does not apply
func (s *Store) forwardRelayFrontedRun(ctx context.Context, job *Job, extraPrompt *string) string {
	if len(s.relayFrontedDeliveryPlatforms(ctx, job)) == 0 {
		return ""
	}
	key := provider.GetSecretStr(ctx, "API_SERVER_KEY", "")
	body := []byte("{}")
	if extraPrompt != nil && *extraPrompt != "" {
		body, _ = json.Marshal(map[string]string{"prompt": *extraPrompt})
	}
	ok := false
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiServerBaseURL()+"/api/jobs/"+job.ID+"/run", bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := cronForwardHTTPClient.Do(req); err == nil {
			resp.Body.Close()
			ok = resp.StatusCode < 300
		}
	}
	if ok {
		return cronDumps(pyObj("success", true, "forwarded_to_gateway", true,
			"note", "This job targets a relay-fronted platform; it was dispatched "+
				"to the running gateway, whose live relay adapter owns that "+
				"delivery."))
	}
	return cronDumps(pyObj("success", false,
		"error", "This job targets a relay-fronted platform, which has no "+
			"standalone sender. Start the gateway — its ticker will "+
			"deliver the job on schedule via the live relay adapter."))
}

// Delivery platforms this satellite reaches only through the primary gateway profile routes
func (s *Store) primaryRoutedDeliveryPlatforms(ctx context.Context, job *Job) map[string]bool {
	routed := map[string]bool{}
	for _, t := range resolveDeliveryTargets(ctx, s, job, false) {
		if t.Platform != "" && s.deliveryPlatformRoutedFromPrimaryGateway(ctx, t.Platform) {
			routed[t.Platform] = true
		}
	}
	if len(routed) == 0 {
		return routed
	}
	gcfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		return map[string]bool{}
	}
	for _, p := range gcfg.GetConnectedPlatforms(ctx) {
		delete(routed, string(p))
	}
	return routed
}

// Queues a manual run for the gateway ticker when only the primary gateway can deliver it, empty otherwise
func (s *Store) handOffPrimaryRoutedRun(ctx context.Context, job *Job, extraPrompt *string) (string, error) {
	if currentGatewayRunner() != nil || !isJobRunnable(job) {
		return "", nil
	}
	routed := s.primaryRoutedDeliveryPlatforms(ctx, job)
	if len(routed) == 0 {
		return "", nil
	}
	alive, known := s.builtinGatewayLiveness(ctx)
	if !(known && alive) {
		cause := "Could not determine whether a gateway serves this profile; check " +
			"`icebath cron status` and re-run."
		if known {
			cause = "Start the gateway — its ticker will deliver the job on schedule."
		}
		names := make([]string, 0, len(routed))
		for p := range routed {
			names = append(names, p)
		}
		sort.Strings(names)
		return cronDumps(pyObj("success", false, "error",
			fmt.Sprintf("This job delivers to %s through the primary ", strings.Join(names, ", "))+
				fmt.Sprintf("gateway's profile route, which has no standalone sender. %s", cause))), nil
	}
	extra := ""
	if extraPrompt != nil {
		extra = *extraPrompt
	}
	updated, err := s.triggerJob(home.WithOverride(ctx, s.home), job.ID, extra)
	if err != nil {
		return "", err
	}
	s.notifyProviderJobsChanged()
	return cronDumps(pyObj("success", true, "job", s.formatJob(updated),
		"note", "This job delivers through the primary gateway's profile route; it was "+
			"queued for that gateway's next scheduler tick, which runs and delivers it.")), nil
}

// Parenthetical delivery note of a manual run summary that never claims success over a failed delivery
func manualRunDeliveryNote(deliver string, refreshed *Job) string {
	if deliver == "" || deliver == "local" {
		return " (output saved locally only)"
	}
	errText := ""
	if refreshed != nil && refreshed.LastDeliveryError != nil {
		errText = strings.TrimSpace(*refreshed.LastDeliveryError)
	}
	if errText == "" {
		if refreshed != nil && pyrepr.Truthy(refreshed.LastDeliveryQueued) {
			return " (output queued for Bot Chat; completion unverified, do not resend)"
		}
		return " (output was delivered there by the job itself)"
	}
	return fmt.Sprintf(" (⚠ delivery FAILED: %s)", headRunes(errText, 200))
}

// Outcome of one manual run in the claimed, success and error shape
type manualRunResult struct {
	Claimed      bool
	Success      bool
	Error        any
	Dispatched   *bool
	DelegationID any
}

// At most once manual claim shared by the sync and background run paths
func (s *Store) claimForManualRun(ctx context.Context, jobID, logLabel string) (*Job, *manualRunResult) {
	claimed, ok, err := s.claimJobForFire(ctx, jobID, FireClaimOptions{Manual: true})
	if err == nil && ok && claimed != nil {
		return claimed, nil
	}
	if err == nil {
		refreshed, gerr := s.getJob(home.WithOverride(ctx, s.home), jobID)
		if gerr == nil {
			reason := "Job is already being fired by the scheduler; not run again."
			if refreshed == nil {
				reason = "Job no longer exists; nothing to run."
			} else if !isJobRunnable(refreshed) {
				reason = "Job is paused/disabled; resume it before running."
			}
			return nil, &manualRunResult{Claimed: false, Success: false, Error: reason}
		}
		err = gerr
	}
	s.log.Error(fmt.Sprintf("Failed to claim cron job %s for %s: %s", jobID, logLabel, err))
	msg := err.Error()
	_, _ = s.markJobRun(ctx, jobID, false, MarkOptions{Error: &msg})
	return nil, &manualRunResult{Claimed: true, Success: false, Error: msg}
}

// Claims and runs a job now through the shared run one job body
func (s *Store) executeJobNow(ctx context.Context, job *Job, extraPrompt *string) *manualRunResult {
	claimed, res := s.claimForManualRun(ctx, job.ID, "immediate run")
	if res != nil {
		return res
	}
	return s.runClaimedJob(ctx, claimed, extraPrompt)
}

// Heartbeats into the caller activity tracker while a manual run executes and returns the stop func
func (s *Store) runHeartbeat(ctx context.Context, jobName string) func() {
	cb := tools.ActivityCallbackOf(ctx)
	if cb == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		started := time.Now()
		ticker := time.NewTicker(cronRunHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			elapsed := time.Since(started)
			if elapsed > cronRunHeartbeatCeiling {
				s.log.Warn(fmt.Sprintf("cronjob run heartbeat ceiling reached for job '%s' (%.0fs) — stopping heartbeat; gateway watchdog regains authority", jobName, elapsed.Seconds()))
				return
			}
			func() {
				defer func() { _ = recover() }()
				cb(fmt.Sprintf("cronjob: running job '%s' (%ds elapsed)", jobName, int(elapsed.Seconds())))
			}()
		}
	}()
	return func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(cronRunHeartbeatInterval + time.Second):
		}
	}
}

// Fires an already claimed job through run one job and reads the outcome from the refreshed record
func (s *Store) runClaimedJob(ctx context.Context, job *Job, extraPrompt *string) (res *manualRunResult) {
	jobID := job.ID
	registered := false
	var fireOwner *string
	owner := new(struct{ byte })
	future := &poolFuture{done: make(chan struct{})}
	defer func() {
		if registered {
			s.releaseRunningJob(jobID, "", owner)
		}
		close(future.done)
	}()
	fail := func(err error) *manualRunResult {
		s.log.Error(fmt.Sprintf("Failed to execute cron job %s immediately: %s", jobID, err))
		msg := err.Error()
		_, _ = s.markJobRun(ctx, jobID, false, MarkOptions{Error: &msg, ExpectedFireOwner: fireOwner})
		return &manualRunResult{Claimed: true, Success: false, Error: msg}
	}
	if !s.tryRegisterRunningJob(jobID, owner, future) {
		return &manualRunResult{Claimed: true, Success: false, Error: alreadyRunningError}
	}
	registered = true
	if job.FireClaim != nil {
		by := job.FireClaim.By
		fireOwner = &by
	}
	var adapters, loop any
	if runner := currentGatewayRunner(); runner != nil {
		var profile *string
		if name, ok := gateway.ProfileNameForHome(s.home); ok {
			profile = &name
		}
		profileAdapters := runner.AdaptersForProfile(profile)
		if profileAdapters != nil {
			adapters = profileAdapters
		}
		if profile != nil && runner.IsSharedBotSatellite(ctx, *profile, "") {
			adapters = newSharedRouteAdapters(adapters, primaryProfileRoutesForCurrentHome(ctx))
		}
		loop = runner.GatewayLoop()
	}
	name := job.Name
	if name == "" {
		name = jobID
	}
	stopHeartbeat := s.runHeartbeat(ctx, name)
	processed, err := func() (bool, error) {
		defer func() {
			registered = false
			s.releaseRunningJob(jobID, "", owner)
		}()
		defer stopHeartbeat()
		return runOneJobSeam(ctx, s, job, runOneJobOptions{Adapters: adapters, Loop: loop, ExtraPrompt: extraPrompt})
	}()
	if err != nil {
		return fail(err)
	}
	refreshed, err := s.getJob(home.WithOverride(ctx, s.home), jobID)
	if err != nil {
		return fail(err)
	}
	if refreshed == nil {
		refreshed = &Job{}
	}
	var execution map[string]any
	if job.ExecutionID != nil && *job.ExecutionID != "" {
		execution, err = openExecutionsLedger(s).Get(ctx, *job.ExecutionID)
		if err != nil {
			return fail(err)
		}
	}
	lastStatus := strPtrAny(refreshed.LastStatus)
	runError := strPtrAny(refreshed.LastError)
	if lastStatus == "delivery_failed" && !pyrepr.Truthy(runError) {
		runError = strPtrAny(refreshed.LastDeliveryError)
	}
	ok := lastStatus == "ok" || lastStatus == "delivery_queued"
	if execution != nil && execution["status"] != "completed" {
		ok = false
		if pyrepr.Truthy(execution["error"]) {
			runError = execution["error"]
		} else {
			status := "unknown"
			if pyrepr.Truthy(execution["status"]) {
				status = pyrepr.Str(execution["status"])
			}
			runError = fmt.Sprintf("execution ended in %s state", status)
		}
	}
	return &manualRunResult{Claimed: true, Success: processed && ok, Error: runError}
}

// ExecuteJobForEvent fires an existing job for an external event through the manual run body
func ExecuteJobForEvent(ctx context.Context, h, jobRef string, extraPrompt *string) (map[string]any, error) {
	s := openCronStore(h, nil)
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), jobRef)
	var amb *AmbiguousJobReferenceError
	if errors.As(err, &amb) {
		return map[string]any{"claimed": false, "success": false, "error": amb.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	if job == nil {
		return map[string]any{"claimed": false, "success": false, "error": fmt.Sprintf("Cron job '%s' not found.", jobRef)}, nil
	}
	res := s.executeJobNow(ctx, job, extraPrompt)
	return map[string]any{"claimed": res.Claimed, "success": res.Success, "error": res.Error}, nil
}

// Excerpt of the newest saved output file of a job, empty when there is none
func (s *Store) latestJobOutputExcerpt(jobID string, maxChars int) string {
	dir, err := s.jobOutputDir(jobID)
	if err != nil {
		return ""
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	last := files[len(files)-1]
	data, err := os.ReadFile(last)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(strings.TrimPrefix(pystr.DecodeUTF8Replace(data), "\uFEFF"))
	if text == "" {
		return ""
	}
	if len([]rune(text)) > maxChars {
		text = headRunes(text, maxChars) + fmt.Sprintf("\n… (truncated; full output: %s)", last)
	}
	return text
}

// Reaps execution rows left claimed or running by a dead owner before a manual dispatch
func (s *Store) reapStaleExecutions(ctx context.Context, jobName string) {
	reclaimed, err := openExecutionsLedger(s).RecoverInterrupted(ctx)
	if err != nil {
		s.log.Debug(fmt.Sprintf("Stale execution reclaim failed: %s", err))
		return
	}
	if reclaimed > 0 {
		s.log.Warn(fmt.Sprintf("Reclaimed %d stale cron execution(s) from dead owner(s) before dispatching job '%s'", reclaimed, jobName))
	}
}

// Routing key of a detached completion, empty when no durable consumer exists
func backgroundSessionKey(env *tools.Env, sessionID any) string {
	if key := tools.CurrentSessionKeyOr(env, ""); key != "" {
		return key
	}
	if pyrepr.Truthy(sessionID) {
		return pyrepr.Str(sessionID)
	}
	return ""
}

// Model and API call totals the fire run session recorded, empty when no run session exists
func (s *Store) runSessionUsage(ctx context.Context, jobID string, executionID *string) *pyjson.Object {
	out := pyjson.NewObject()
	if executionID == nil || *executionID == "" {
		return out
	}
	db, err := state.NewSessionDB(ctx, home.StatePath(s.home), true)
	if err != nil {
		return out
	}
	defer db.Close()
	runs, err := db.ListCronJobRuns(ctx, jobID, 50)
	if err != nil {
		s.log.Debug(fmt.Sprintf("cronjob run: run-session usage lookup failed for %s: %s", jobID, err))
		return out
	}
	var run *pyjson.Object
	for _, r := range runs {
		if v, _ := r.Get("cron_execution_id"); v == *executionID {
			run = r
			break
		}
	}
	if run == nil {
		return out
	}
	runID, _ := run.Get("id")
	chain, err := db.GetCompressionChain(ctx, pyrepr.Str(runID))
	if err != nil {
		s.log.Debug(fmt.Sprintf("cronjob run: run-session usage lookup failed for %s: %s", jobID, err))
		return out
	}
	var rows []*pyjson.Object
	for _, sid := range chain {
		row, err := db.GetSession(ctx, sid)
		if err != nil {
			s.log.Debug(fmt.Sprintf("cronjob run: run-session usage lookup failed for %s: %s", jobID, err))
			return out
		}
		if row == nil {
			row = pyjson.NewObject()
		}
		rows = append(rows, row)
	}
	calls := new(big.Int)
	for _, r := range rows {
		if v, _ := r.Get("api_call_count"); pyrepr.Truthy(v) {
			n, err := pyint.Of(v)
			if err != nil {
				return out
			}
			calls.Add(calls, n)
		}
	}
	out.Set("api_calls", pyint.Value(calls))
	for i := len(rows) - 1; i >= 0; i-- {
		if m, _ := rows[i].Get("model"); pyrepr.Truthy(m) {
			out.Set("model", m)
			break
		}
	}
	return out
}

// Async delegation completion block of a finished background manual run
func (s *Store) manualRunCompletion(ctx context.Context, res *manualRunResult, jobID, jobName, deliver string, startedAt time.Time, executionID *string) *pyjson.Object {
	duration := math.Round(time.Since(startedAt).Seconds()*100) / 100
	refreshed, _ := s.getJob(home.WithOverride(ctx, s.home), jobID)
	result := "FAILED"
	if res.Success {
		result = "ok"
	}
	if pyrepr.Truthy(res.Error) {
		result += " — " + pyrepr.Str(res.Error)
	}
	lines := []string{
		fmt.Sprintf("Cron job '%s' (%s) finished its manual run.", jobName, jobID),
		"Result: " + result,
		"Delivery target: " + deliver + manualRunDeliveryNote(deliver, refreshed),
	}
	if refreshed != nil && nonEmpty(refreshed.NextRunAt) != nil {
		lines = append(lines, "Next scheduled run: "+*refreshed.NextRunAt)
	}
	if excerpt := s.latestJobOutputExcerpt(jobID, 2000); excerpt != "" {
		lines = append(lines, "--- JOB OUTPUT ---", excerpt)
	}
	status := "error"
	if res.Success {
		status = "completed"
	}
	out := pyObj("status", status, "summary", strings.Join(lines, "\n"), "error", res.Error, "duration_seconds", duration)
	return mergeObj(out, s.runSessionUsage(ctx, jobID, executionID))
}

// Claims synchronously then fires the run on the async delegation executor, nil when background delivery is unavailable
func (s *Store) tryDispatchBackgroundRun(ctx context.Context, env *tools.Env, job *Job, sessionID any, extraPrompt *string) *manualRunResult {
	jobID := job.ID
	jobName := job.Name
	if jobName == "" {
		jobName = jobID
	}
	s.reapStaleExecutions(ctx, jobName)
	if !tools.SessionAsyncDeliverySupported(env) {
		return nil
	}
	sessionKey := backgroundSessionKey(env, sessionID)
	if sessionKey == "" {
		return nil
	}
	if isJobRunning(s, jobID) {
		return &manualRunResult{Claimed: false, Success: false, Error: alreadyRunningError}
	}
	claimed, res := s.claimForManualRun(ctx, jobID, "background run")
	if res != nil {
		if res.Claimed {
			f := false
			res.Dispatched = &f
		}
		return res
	}
	originUISessionID := tools.SessionEnvValue(env, "ICEBATH_UI_SESSION_ID")
	originSessionID := tools.CurrentOriginSessionID(env)
	maxAsync := tools.MaxAsyncChildren(ctx)
	startedAt := time.Now()
	var deliverRaw any = "local"
	if claimed.Deliver != nil {
		deliverRaw = *claimed.Deliver
	} else if jobHasKey(claimed, "deliver") {
		deliverRaw = nil
	}
	deliver := normalizeDeliverValue(deliverRaw)
	var parent *string
	if pyrepr.Truthy(sessionID) {
		p := pyrepr.Str(sessionID)
		parent = &p
	}
	runCtx := context.WithoutCancel(ctx)
	dispatch := dispatchAsyncDelegationSeam(env, tools.AsyncDelegationRequest{
		Goal: fmt.Sprintf("Manual run of cron job '%s' (%s)", jobName, jobID),
		Context: "Triggered via cronjob(action='run'). The job executed in its own " +
			"fresh cron session; this block reports its outcome.",
		Role: "cron_run", SessionKey: sessionKey, ParentSessionID: parent,
		Runner: func() *pyjson.Object {
			r := s.runClaimedJob(runCtx, claimed, extraPrompt)
			return s.manualRunCompletion(runCtx, r, jobID, jobName, deliver, startedAt, claimed.ExecutionID)
		},
		OriginUISessionID: originUISessionID, OriginSessionID: originSessionID, MaxAsyncChildren: maxAsync,
	})
	if dispatch["status"] == "dispatched" {
		t := true
		return &manualRunResult{Claimed: true, Dispatched: &t, DelegationID: dispatch["delegation_id"]}
	}
	reason := any("rejected")
	if v, ok := dispatch["error"]; ok {
		reason = v
	}
	s.log.Info(fmt.Sprintf("cronjob run: background pool unavailable (%s); running job '%s' inline.", pyrepr.Str(reason), jobName))
	result := s.runClaimedJob(ctx, job, extraPrompt)
	f := false
	result.Dispatched = &f
	return result
}

// Attaches the mode and delivery guidance notes to a create or update result
func withGuidance(result *pyjson.Object, job *Job, deliver *string) *pyjson.Object {
	if notes := modeGuidanceNotes(job, deliver); len(notes) > 0 {
		result.Set("guidance", toAnyList(notes))
	}
	return result
}

// String form of an optional argument, empty for nil
func argText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Optional string argument as a pointer, nil for nil
func argPtr(v any) *string {
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

// Nil interface for a nil string pointer, the string otherwise
func ptrValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// List of refs from a string or list argument
func refsList(v any) []any {
	switch x := v.(type) {
	case string:
		return []any{x}
	case []any:
		return x
	case []string:
		return toAnyList(x)
	}
	return []any{v}
}

func (s *Store) actionCreate(ctx context.Context, a *cronjobArgs) (string, error) {
	prompt, script := argText(a.Prompt), argText(a.Script)
	deliver := normalizeDeliverParam(a.Deliver)
	if !pyrepr.Truthy(a.Schedule) {
		return cronToolError("schedule is required for create", nil), nil
	}
	skills := canonicalSkills(a.Skill, a.Skills)
	noAgent := pyrepr.Truthy(a.NoAgent)
	if noAgent {
		if script == "" {
			return cronToolError("create with no_agent=True requires a script — "+
				"the script is the job. In no_agent mode the LLM is "+
				"skipped entirely: prompt and skills are ignored, "+
				"non-empty stdout is delivered verbatim, empty stdout "+
				"sends nothing (watchdog pattern), and a non-zero exit or timeout sends an error alert.", nil), nil
		}
	} else if prompt == "" && len(skills) == 0 {
		return cronToolError("create requires either prompt or at least one skill", nil), nil
	}
	checks := []func() (string, error){
		func() (string, error) {
			if prompt == "" {
				return "", nil
			}
			return scanCronPrompt(prompt), nil
		},
		func() (string, error) { return s.validateCronScriptPath(script), nil },
		func() (string, error) { return s.validateCronScriptPath(argText(a.MonitorScript)), nil },
		func() (string, error) { return s.validateCronBaseURL(ctx, a.Provider, a.BaseURL) },
		func() (string, error) { return validateBotChatDeliver(deliver), nil },
		func() (string, error) { return validateBotChatDeliver(normalizeDeliverParam(a.FailureDeliver)), nil },
		func() (string, error) {
			if !pyrepr.Truthy(a.ContextFrom) {
				return "", nil
			}
			return s.validateContextFromRefs(home.WithOverride(ctx, s.home), refsList(a.ContextFrom))
		},
	}
	for _, check := range checks {
		msg, err := check()
		if err != nil {
			return "", err
		}
		if msg != "" {
			return cronToolError(msg, nil), nil
		}
	}
	contextFrom := a.ContextFrom
	if a.Continuity != nil {
		if refs := applyContinuity(contextFrom, pyrepr.Truthy(a.Continuity)); refs != nil {
			contextFrom = toAnyList(refs)
		} else {
			contextFrom = nil
		}
	}
	spec := pyObj("prompt", prompt, "schedule", a.Schedule, "name", a.Name, "repeat", a.Repeat,
		"deliver", ptrValue(resolveCronContextDeliver(a.env, deliver)))
	if origin := s.originFromEnv(a.env, a.Schedule); origin != nil {
		spec.Set("origin", origin)
	} else {
		spec.Set("origin", nil)
	}
	spec.Set("skills", toAnyList(skills))
	spec.Set("model", ptrValue(normalizeOptionalJobValue(a.Model, false)))
	spec.Set("provider", ptrValue(normalizeOptionalJobValue(a.Provider, false)))
	spec.Set("base_url", ptrValue(normalizeOptionalJobValue(a.BaseURL, true)))
	spec.Set("script", ptrValue(normalizeOptionalJobValue(a.Script, false)))
	spec.Set("context_from", contextFrom)
	spec.Set("enabled_toolsets", a.EnabledToolsets)
	spec.Set("workdir", ptrValue(normalizeOptionalJobValue(a.Workdir, false)))
	spec.Set("no_agent", noAgent)
	spec.Set("attach_to_session", a.AttachToSession)
	spec.Set("monitor_script", ptrValue(normalizeOptionalJobValue(a.MonitorScript, false)))
	spec.Set("monitor_url", ptrValue(normalizeOptionalJobValue(a.MonitorURL, false)))
	spec.Set("reasoning_effort", a.ReasoningEffort)
	spec.Set("interpreter", a.Interpreter)
	spec.Set("timezone", ptrValue(normalizeOptionalJobValue(a.Timezone, false)))
	spec.Set("grace_minutes", a.GraceMinutes)
	spec.Set("pinned", pyrepr.Truthy(a.Pinned))
	spec.Set("failure_deliver", ptrValue(resolveCronContextDeliver(a.env, normalizeDeliverParam(a.FailureDeliver))))
	spec.Set("catch_up", a.CatchUp)
	if a.Paused != false || a.PausedReason != nil {
		var pausedValue any = a.Paused
		if _, none := a.Paused.(explicitNone); none {
			pausedValue = nil
		}
		spec.Set("paused", pausedValue)
		spec.Set("paused_reason", a.PausedReason)
	}
	job, err := s.createJobWithSchedulerRegistration(home.WithOverride(ctx, s.home), spec)
	var regErr *CronSchedulerRegistrationError
	if errors.As(err, &regErr) {
		partial := regErr.ToDict()
		msg, _ := partial.Get("error")
		partial.Delete("error")
		return cronToolError(pyrepr.Str(msg), partial), nil
	}
	if err != nil {
		return "", err
	}
	parts := []string{fmt.Sprintf("Cron job '%s' created.", job.Name)}
	if !job.Enabled {
		parts = append(parts, "Created PAUSED — resume to schedule, or explicitly run now.")
	}
	if notice := s.localDeliveryNotice(ctx, a.env, job, deliver); notice != nil && *notice != "" {
		parts = append(parts, *notice)
	}
	var jobDeliver any = "local"
	if job.Deliver != nil {
		jobDeliver = *job.Deliver
	}
	skillsOut := []any{}
	if job.Skills != nil {
		skillsOut = toAnyList(job.Skills)
	}
	result := pyObj("success", true, "job_id", job.ID, "name", job.Name, "skill", strPtrAny(job.Skill),
		"skills", skillsOut, "schedule", job.ScheduleDisplay, "repeat", repeatDisplay(job),
		"deliver", jobDeliver, "next_run_at", strPtrAny(job.NextRunAt), "job", s.formatJob(job),
		"message", strings.Join(parts, " "))
	mergeObj(result, s.gatewayLivenessNotice(ctx, false))
	return cronDumps(withGuidance(result, job, deliver)), nil
}

func (s *Store) actionList(ctx context.Context, a *cronjobArgs) (string, error) {
	jobs, err := s.listJobs(home.WithOverride(ctx, s.home), a.IncludeDisabled)
	if err != nil {
		return "", err
	}
	formatted := make([]any, 0, len(jobs))
	for _, j := range jobs {
		formatted = append(formatted, s.formatJob(j))
	}
	result := pyObj("success", true, "count", len(formatted), "jobs", formatted)
	if len(formatted) > 0 {
		mergeObj(result, s.gatewayLivenessNotice(ctx, true))
	}
	return cronDumps(result), nil
}

func (s *Store) actionRemove(ctx context.Context, job *Job, a *cronjobArgs) (string, error) {
	removed, err := s.removeJob(ctx, job.ID)
	if err != nil {
		return "", err
	}
	if !removed {
		return cronToolError(fmt.Sprintf("Failed to remove job '%s'", job.ID), nil), nil
	}
	s.notifyProviderJobsChanged()
	var sched any
	if job.ScheduleDisplay != "" || jobHasKey(job, "schedule_display") {
		sched = job.ScheduleDisplay
	}
	return cronDumps(pyObj("success", true, "message", fmt.Sprintf("Cron job '%s' removed.", job.Name),
		"removed_job", pyObj("id", job.ID, "name", job.Name, "schedule", sched))), nil
}

// Success payload with the formatted job after a state change
func (s *Store) jobStateResult(updated *Job) string {
	s.notifyProviderJobsChanged()
	return cronDumps(pyObj("success", true, "job", s.formatJob(updated)))
}

// Formatted job reread so the response reflects the post run fields
func (s *Store) refreshedJobView(ctx context.Context, jobID string) (*pyjson.Object, error) {
	job, err := s.getJob(home.WithOverride(ctx, s.home), jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		job = &Job{ID: jobID, Enabled: true}
	}
	return s.formatJob(job), nil
}

func (s *Store) actionRun(ctx context.Context, job *Job, a *cronjobArgs) (string, error) {
	jobID := job.ID
	var extraPrompt *string
	if pyrepr.Truthy(a.Prompt) {
		extraPrompt = argPtr(a.Prompt)
		if scanErr := scanCronPrompt(*extraPrompt); scanErr != "" {
			return cronToolError(scanErr, nil), nil
		}
	}
	handed, err := s.handOffPrimaryRoutedRun(ctx, job, extraPrompt)
	if err != nil {
		return "", err
	}
	if handed != "" {
		return handed, nil
	}
	bg := s.tryDispatchBackgroundRun(ctx, a.env, job, a.SessionID, extraPrompt)
	if bg != nil && bg.Dispatched != nil && *bg.Dispatched {
		s.notifyProviderJobsChanged()
		result, err := s.refreshedJobView(home.WithOverride(ctx, s.home), jobID)
		if err != nil {
			return "", err
		}
		result.Set("executed", true)
		result.Set("execution_mode", "background")
		result.Set("delegation_id", bg.DelegationID)
		return cronDumps(pyObj("success", true, "job", result,
			"note", "The job is running in the background. You and the "+
				"user can keep working; its outcome re-enters the "+
				"conversation as a new message when it finishes. "+
				"Do not wait or poll — just continue.")), nil
	}
	execResult := bg
	if bg == nil {
		if forwarded := s.forwardRelayFrontedRun(ctx, job, extraPrompt); forwarded != "" {
			return forwarded, nil
		}
		execResult = s.executeJobNow(ctx, job, extraPrompt)
	}
	if execResult.Claimed {
		s.notifyProviderJobsChanged()
	}
	result, err := s.refreshedJobView(home.WithOverride(ctx, s.home), jobID)
	if err != nil {
		return "", err
	}
	result.Set("executed", execResult.Claimed)
	result.Set("execution_success", execResult.Success)
	if !execResult.Claimed {
		if pyrepr.Truthy(execResult.Error) {
			result.Set("execution_skipped", execResult.Error)
		} else {
			result.Set("execution_skipped", "Already being fired by the scheduler; not run again.")
		}
	} else if pyrepr.Truthy(execResult.Error) {
		result.Set("execution_error", execResult.Error)
	}
	return cronDumps(pyObj("success", true, "job", result)), nil
}

// Effective value of a key after this update, the pending update winning over the stored job
func pickUpdate(updates map[string]any, job *Job, key string) any {
	if v, ok := updates[key]; ok {
		return v
	}
	switch key {
	case "provider":
		return strPtrAny(job.Provider)
	case "base_url":
		return strPtrAny(job.BaseURL)
	case "script":
		return strPtrAny(job.Script)
	case "monitor_script":
		return strPtrAny(job.MonitorScript)
	case "monitor_url":
		return strPtrAny(job.MonitorURL)
	case "grace_minutes":
		if job.GraceMinutes == nil {
			return nil
		}
		return *job.GraceMinutes
	case "schedule":
		if job.Schedule == nil {
			return nil
		}
		return job.Schedule
	}
	return nil
}

// Prompt, name, deliver, skills and model pin updates with their validation errors
func (s *Store) updateCoreFields(ctx context.Context, job *Job, a *cronjobArgs, updates map[string]any) (string, error) {
	if a.Prompt != nil {
		prompt := pyrepr.Str(a.Prompt)
		if scanErr := scanCronPrompt(prompt); scanErr != "" {
			return scanErr, nil
		}
		updates["prompt"] = prompt
	}
	if a.Name != nil && strings.TrimSpace(pyrepr.Str(a.Name)) != "" {
		updates["name"] = a.Name
	}
	if a.Deliver != nil {
		if msg := validateBotChatDeliver(normalizeDeliverParam(a.Deliver)); msg != "" {
			return msg, nil
		}
		updates["deliver"] = ptrValue(resolveCronContextDeliver(a.env, normalizeDeliverParam(a.Deliver)))
	}
	if a.FailureDeliver != nil {
		fd := normalizeDeliverParam(a.FailureDeliver)
		if fd != nil {
			if msg := validateBotChatDeliver(fd); msg != "" {
				return msg, nil
			}
			fd = resolveCronContextDeliver(a.env, fd)
		}
		updates["failure_deliver"] = ptrValue(fd)
	}
	if a.CatchUp != nil {
		updates["catch_up"] = a.CatchUp
	}
	if a.Skills != nil || a.Skill != nil {
		skills := canonicalSkills(a.Skill, a.Skills)
		updates["skills"] = toAnyList(skills)
		if len(skills) > 0 {
			updates["skill"] = skills[0]
		} else {
			updates["skill"] = nil
		}
	}
	if a.Model != nil {
		updates["model"] = ptrValue(normalizeOptionalJobValue(a.Model, false))
	}
	if a.Provider != nil {
		updates["provider"] = ptrValue(normalizeOptionalJobValue(a.Provider, false))
	}
	if a.Pinned != nil {
		updates["pinned"] = pyrepr.Truthy(a.Pinned)
	}
	if a.BaseURL != nil {
		updates["base_url"] = ptrValue(normalizeOptionalJobValue(a.BaseURL, true))
	}
	if a.ReasoningEffort != nil {
		updates["reasoning_effort"] = a.ReasoningEffort
	}
	if a.Interpreter != nil {
		updates["interpreter"] = a.Interpreter
	}
	return s.validateCronBaseURL(ctx, pickUpdate(updates, job, "provider"), pickUpdate(updates, job, "base_url"))
}

// Script, monitor script and monitor URL updates where an empty string clears
func (s *Store) updateScriptFields(ctx context.Context, job *Job, a *cronjobArgs, updates map[string]any) (string, error) {
	for _, f := range []struct {
		field string
		value any
	}{{"script", a.Script}, {"monitor_script", a.MonitorScript}} {
		if f.value == nil {
			continue
		}
		if pyrepr.Truthy(f.value) {
			if msg := s.validateCronScriptPath(pyrepr.Str(f.value)); msg != "" {
				return msg, nil
			}
			updates[f.field] = ptrValue(normalizeOptionalJobValue(f.value, false))
		} else {
			updates[f.field] = nil
		}
	}
	if a.MonitorURL != nil {
		if pyrepr.Truthy(a.MonitorURL) {
			updates["monitor_url"] = ptrValue(normalizeOptionalJobValue(a.MonitorURL, false))
		} else {
			updates["monitor_url"] = nil
		}
	}
	if (a.MonitorScript != nil || a.MonitorURL != nil) &&
		pyrepr.Truthy(pickUpdate(updates, job, "monitor_script")) && pyrepr.Truthy(pickUpdate(updates, job, "monitor_url")) {
		return "monitor_script and monitor_url are mutually exclusive — clear one before setting the other.", nil
	}
	return "", nil
}

// Context refs and continuity updates where an empty value clears and every ref must exist
func (s *Store) updateContextFrom(ctx context.Context, job *Job, a *cronjobArgs, updates map[string]any) (string, error) {
	if a.ContextFrom == nil && a.Continuity == nil {
		return "", nil
	}
	contextFrom := a.ContextFrom
	if contextFrom == nil {
		contextFrom = toAnyList(job.ContextFrom)
	}
	refs := cleanStrList(contextFrom)
	if a.Continuity != nil {
		refs = applyContinuity(refs, pyrepr.Truthy(a.Continuity))
	}
	if len(refs) > 0 {
		msg, err := s.validateContextFromRefs(home.WithOverride(ctx, s.home), toAnyList(refs))
		if err != nil || msg != "" {
			return msg, err
		}
		updates["context_from"] = toAnyList(refs)
	} else {
		updates["context_from"] = nil
	}
	return "", nil
}

// Toolsets, attach, workdir, no agent, repeat, timezone, grace and schedule updates
func (s *Store) updateRunFields(ctx context.Context, job *Job, a *cronjobArgs, updates map[string]any) (string, error) {
	if a.EnabledToolsets != nil {
		updates["enabled_toolsets"] = a.EnabledToolsets
	}
	if a.AttachToSession != nil {
		updates["attach_to_session"] = pyrepr.Truthy(a.AttachToSession)
	}
	if a.Workdir != nil {
		updates["workdir"] = ptrValue(normalizeOptionalJobValue(a.Workdir, false))
	}
	if a.NoAgent != nil {
		target := pyrepr.Truthy(a.NoAgent)
		if target && !pyrepr.Truthy(pickUpdate(updates, job, "script")) {
			return "Cannot set no_agent=True on a job without a script. " +
				"Set `script` in the same update, or on the job first.", nil
		}
		updates["no_agent"] = target
	}
	if a.Repeat != nil {
		times, err := normalizeRepeatValue(a.Repeat)
		if err != nil {
			return "", err
		}
		completed := 0
		state := map[string]any{}
		if job.Repeat != nil {
			completed = job.Repeat.Completed
			state["completed"] = completed
		}
		state["times"] = intOrNil(times)
		updates["repeat"] = state
	}
	if a.Timezone != nil {
		if tz := normalizeOptionalJobValue(a.Timezone, false); tz != nil {
			updates["timezone"] = *tz
		} else {
			updates["timezone"] = ""
		}
	}
	if a.GraceMinutes != nil {
		if str, ok := a.GraceMinutes.(string); ok && strings.TrimSpace(str) == "" {
			updates["grace_minutes"] = nil
		} else {
			updates["grace_minutes"] = a.GraceMinutes
		}
		if pickUpdate(updates, job, "grace_minutes") != nil {
			kind := ""
			if sched, ok := pickUpdate(updates, job, "schedule").(*Schedule); ok && sched != nil {
				kind = sched.Kind
			}
			if kind != "cron" {
				return "grace_minutes only applies to cron schedules.", nil
			}
		}
	}
	if a.Schedule != nil {
		parsed, err := s.parseSchedule(pyrepr.Str(a.Schedule))
		if err != nil {
			return "", err
		}
		updates["schedule"] = parsed
		if parsed.Display != "" {
			updates["schedule_display"] = parsed.Display
		} else {
			updates["schedule_display"] = a.Schedule
		}
		if job.State != "paused" {
			updates["state"] = "scheduled"
			updates["enabled"] = true
		}
	}
	return "", nil
}

func (s *Store) actionUpdate(ctx context.Context, job *Job, a *cronjobArgs) (string, error) {
	updates := map[string]any{}
	for _, step := range []func(context.Context, *Job, *cronjobArgs, map[string]any) (string, error){
		s.updateCoreFields, s.updateScriptFields, s.updateContextFrom, s.updateRunFields,
	} {
		msg, err := step(ctx, job, a, updates)
		if err != nil {
			return "", err
		}
		if msg != "" {
			return cronToolError(msg, nil), nil
		}
	}
	if len(updates) == 0 {
		return cronToolError("No updates provided.", nil), nil
	}
	updated, err := s.updateJob(home.WithOverride(ctx, s.home), job.ID, updates)
	if err != nil {
		return "", err
	}
	s.notifyProviderJobsChanged()
	return cronDumps(withGuidance(pyObj("success", true, "job", s.formatJob(updated)), updated, normalizeDeliverParam(a.Deliver))), nil
}

// Stops the live run of a job without touching its schedule
func (s *Store) actionCancel(ctx context.Context, job *Job, a *cronjobArgs) (string, error) {
	res, err := cancelRun(s, job.ID, argText(a.Reason))
	if err != nil {
		return "", err
	}
	return cronDumps(mergeObj(pyObj("job", s.formatJob(job)), res)), nil
}

// Job for a reference or the JSON error naming the ambiguity or the miss
func (s *Store) resolveJobOrError(ctx context.Context, jobID string) (*Job, string, error) {
	job, err := s.resolveJobRef(home.WithOverride(ctx, s.home), jobID)
	var amb *AmbiguousJobReferenceError
	if errors.As(err, &amb) {
		matches := make([]any, 0, len(amb.Matches))
		for _, m := range amb.Matches {
			var sched any
			if m.ScheduleDisplay != "" {
				sched = m.ScheduleDisplay
			}
			matches = append(matches, pyObj("id", m.ID, "name", m.Name, "schedule", sched, "next_run_at", strPtrAny(m.NextRunAt)))
		}
		return nil, cronDumps(pyObj("success", false, "error", amb.Error(), "matches", matches)), nil
	}
	if err != nil {
		return nil, "", err
	}
	if job == nil {
		return nil, cronDumps(pyObj("success", false,
			"error", fmt.Sprintf("Job with ID or name '%s' not found. Use cronjob(action='list') to inspect jobs.", jobID))), nil
	}
	return job, "", nil
}

// Unified cron job management entry point returning the tool JSON
func (s *Store) cronjob(ctx context.Context, a *cronjobArgs) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = cronToolError(fmt.Sprint(r), nil)
		}
	}()
	if a.Paused == nil {
		a.Paused = false
	}
	normalized := strings.ToLower(strings.TrimSpace(argText(a.Action)))
	var text string
	var err error
	switch normalized {
	case "create":
		text, err = s.actionCreate(ctx, a)
	case "list":
		text, err = s.actionList(ctx, a)
	default:
		if !pyrepr.Truthy(a.JobID) {
			return cronToolError(fmt.Sprintf("job_id is required for action '%s'", normalized), nil)
		}
		job, errJSON, rerr := s.resolveJobOrError(home.WithOverride(ctx, s.home), pyrepr.Str(a.JobID))
		if rerr != nil {
			return cronToolError(rerr.Error(), nil)
		}
		if errJSON != "" {
			return errJSON
		}
		switch normalized {
		case "remove":
			text, err = s.actionRemove(ctx, job, a)
		case "update":
			text, err = s.actionUpdate(ctx, job, a)
		case "run", "run_now", "trigger":
			text, err = s.actionRun(ctx, job, a)
		case "pause":
			var updated *Job
			if updated, err = s.pauseJob(home.WithOverride(ctx, s.home), job.ID, argPtr(a.Reason)); err == nil {
				text = s.jobStateResult(updated)
			}
		case "resume":
			var updated *Job
			if updated, err = s.resumeJob(home.WithOverride(ctx, s.home), job.ID); err == nil {
				text = s.jobStateResult(updated)
			}
		case "cancel":
			text, err = s.actionCancel(ctx, job, a)
		default:
			return cronToolError(fmt.Sprintf("Unknown cron action '%s'", argText(a.Action)), nil)
		}
	}
	if err != nil {
		return cronToolError(err.Error(), nil)
	}
	return text
}

// Script parameter description naming the active profile home
func scriptDescription(h string) string {
	return "Optional script run each tick; stdout is injected into the agent's prompt as context (with no_agent=True " +
		fmt.Sprintf("the script IS the job). Relative paths resolve under %s/scripts/; .sh/.bash via bash, else Python. ", h) +
		"On update, '' clears."
}

// Parameters with the script path hint rebuilt from the active profile at every definition read
func cronjobSchemaOverrides(env *tools.Env) map[string]any {
	h := ""
	if env != nil {
		h = env.Home
	}
	if h == "" {
		h = home.Dir(nil)
	}
	params := pycopy.DeepCopy(cronjobSchema["parameters"]).(map[string]any)
	params["properties"].(map[string]any)["script"].(map[string]any)["description"] = scriptDescription(home.Display(h))
	return map[string]any{"parameters": params}
}

// Available in interactive CLI, gateway sessions, exec ask, and cron runs
func checkCronjobRequirements(env *tools.Env) bool {
	return truthyText(os.Getenv("ICEBATH_INTERACTIVE")) || truthyText(os.Getenv("ICEBATH_GATEWAY_SESSION")) ||
		truthyText(os.Getenv("ICEBATH_EXEC_ASK")) || truthyText(tools.SessionEnvValue(env, "ICEBATH_CRON_SESSION"))
}

// Store of the calling profile, the context store first
func cronToolStore(ctx context.Context, env *tools.Env) *Store {
	if s, ok := ctx.Value(cronStoreKey{}).(*Store); ok && s != nil {
		return s
	}
	if env != nil && env.Home != "" {
		return openCronStore(env.Home, nil)
	}
	return cronStoreFrom(ctx)
}

// Model tool dispatch resolving the monitor field into the stored script and URL pair
func cronjobHandler(ctx context.Context, call *tools.Call) string {
	args := call.Args
	get := func(k string) any { return args[k] }
	monScript, monURL := splitMonitorArg(get("monitor"), get("monitor_script"), get("monitor_url"))
	includeDisabled := true
	if v, ok := args["include_disabled"]; ok {
		includeDisabled = pyrepr.Truthy(v)
	}
	var paused any = false
	if v, ok := args["paused"]; ok {
		paused = v
		if v == nil {
			paused = explicitNone{}
		}
	}
	var sessionID any
	if call.SessionID != "" {
		sessionID = call.SessionID
	}
	action := any("")
	if v, ok := args["action"]; ok {
		action = v
	}
	a := &cronjobArgs{
		Action: action, IncludeDisabled: includeDisabled, MonitorScript: monScript, MonitorURL: monURL,
		SessionID: sessionID, Paused: paused, env: call.Env,
		JobID: get("job_id"), Prompt: get("prompt"), Schedule: get("schedule"), Name: get("name"),
		Repeat: get("repeat"), Deliver: get("deliver"), FailureDeliver: get("failure_deliver"),
		Skill: get("skill"), Skills: get("skills"), Reason: get("reason"), Script: get("script"),
		ContextFrom: get("context_from"), Continuity: get("continuity"), EnabledToolsets: get("enabled_toolsets"),
		Workdir: get("workdir"), NoAgent: get("no_agent"), AttachToSession: get("attach_to_session"),
		PausedReason: get("paused_reason"), Pinned: get("pinned"), Timezone: get("timezone"),
		GraceMinutes: get("grace_minutes"), CatchUp: get("catch_up"),
	}
	return cronToolStore(ctx, call.Env).cronjob(ctx, a)
}

// Registers the cronjob_manage tool with the shared registry
func init() { tools.Register(cronjobManageToolDef()) }

// Registration of the cronjob_manage tool
func cronjobManageToolDef() tools.Def {
	return tools.Def{
		Name: "cronjob_manage", Toolset: "cronjob", Schema: cronjobSchema, Handler: cronjobHandler,
		Check: checkCronjobRequirements, Emoji: "⏰", DynamicSchema: cronjobSchemaOverrides,
	}
}

// Async delegation dispatcher the background run hands its runner to, tests replace it
var dispatchAsyncDelegationSeam = tools.DispatchAsyncDelegation

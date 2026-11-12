package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var fireClaimHeartbeatGraceSecs = runClaimHeartbeatSeconds * 3

const (
	fireClaimMissConfirmSeconds  = 1.0
	cronDefaultCleanupTimeoutSec = 10.0
)

var cronDeliveryVars = []string{
	"ICEBATH_CRON_AUTO_DELIVER_PLATFORM",
	"ICEBATH_CRON_AUTO_DELIVER_CHAT_ID",
	"ICEBATH_CRON_AUTO_DELIVER_THREAD_ID",
}

// Seconds between watchdog polls of a running agent, shrunk by tests
var watchdogPollSeconds = 5.0

// Sends queued worker results through the live adapters, zero when no worker ever queued one

// Runtime resolution run_job reaches, swapped by tests like a patched resolve_runtime_provider
var resolveRuntimeProviderSeam = provider.ResolveRuntimeProvider

func (s *Store) drainDeliveryQueue(ctx context.Context, adapters, loop any) (int, error) {
	if _, err := os.Stat(QueuePath(s.home)); err != nil {
		return 0, nil
	}
	send := func(queued *Job, content string, forFailure bool) (*string, error) {
		errText, err := s.deliverResult(ctx, queued, content, adapters, loop, forFailure)
		if err != nil {
			return nil, err
		}
		if errText == nil && forFailure && !queued.run.notificationAllTargetsSuppressed {
			s.markIncidentAlerted(queuedFailureIncidentID(queued))
		}
		return errText, nil
	}
	return openDeliveryQueue(s).Drain(ctx, send, deliveryDrainLimit)
}

// Wall clock bound for post run cleanup from cron.cleanup_timeout_seconds, ten seconds by default
func (s *Store) cronCleanupTimeoutSeconds() float64 {
	cfg, err := s.loadConfig()
	if err != nil {
		s.log.Debug(fmt.Sprintf("Failed to load cron cleanup timeout from config: %v", err))
		return cronDefaultCleanupTimeoutSec
	}
	cronCfg, ok := cfg["cron"].(map[string]any)
	if !ok {
		return cronDefaultCleanupTimeoutSec
	}
	configured, present := cronCfg["cleanup_timeout_seconds"]
	if !present || configured == nil {
		return cronDefaultCleanupTimeoutSec
	}
	timeout, ok, convErr := anyFloat(configured)
	if convErr != nil || !ok {
		s.log.Debug(fmt.Sprintf("Failed to load cron cleanup timeout from config: could not convert %v to float", configured))
		return cronDefaultCleanupTimeoutSec
	}
	if timeout >= 0 {
		return timeout
	}
	return cronDefaultCleanupTimeoutSec
}

// Runs fallible cleanup under the timeout, false on failure or when the bound passed and the worker was abandoned
func (s *Store) runCronCleanupWithTimeout(jobID, label string, timeoutSeconds *float64, cleanup func() error) bool {
	timeout := s.cronCleanupTimeoutSeconds()
	if timeoutSeconds != nil {
		timeout = *timeoutSeconds
	}
	call := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return cleanup()
	}
	if timeout <= 0 {
		if err := call(); err != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': %s failed: %v", jobID, label, err))
			return false
		}
		return true
	}
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': %s failed: %v", jobID, label, err))
			return false
		}
		return true
	case <-time.After(time.Duration(timeout * float64(time.Second))):
		s.log.Error(fmt.Sprintf("Job '%s': %s exceeded %.1fs; abandoning cleanup so future runs remain dispatchable", jobID, label, timeout))
		return false
	}
}

// Session store surface the cron finalization uses
type cronSessionDB interface {
	CompressionTip(id string) (string, error)
	SetTitle(sessionID, title string) (bool, error)
	EndSession(id, reason string) error
	UpdateSessionCwd(id, cwd, repoRoot, branch string) error
	SessionLifecycleStatuses(ctx context.Context, sessionIDs []string) (map[string]string, error)
}

var _ cronSessionDB = (*state.DB)(nil)

// Session store whose cleanup calls run under the cleanup bound and fail fast after one failure
type boundedCronSessionDB struct {
	s        *Store
	db       cronSessionDB
	jobID    string
	mu       sync.Mutex
	disabled bool
}

// Runs one session store call under the bound, disabling the proxy once a call times out
func (b *boundedCronSessionDB) call(name string, fn func() error) error {
	b.mu.Lock()
	disabled := b.disabled
	b.mu.Unlock()
	if disabled {
		return errors.New("session finalization disabled after prior cleanup failure")
	}
	var callErr error
	ok := b.s.runCronCleanupWithTimeout(b.jobID, fmt.Sprintf("session finalization (%s)", name), nil, func() error {
		callErr = fn()
		return callErr
	})
	if ok {
		return nil
	}
	if callErr != nil {
		return callErr
	}
	b.mu.Lock()
	b.disabled = true
	b.mu.Unlock()
	return fmt.Errorf("session finalization method %s timed out", name)
}

func (b *boundedCronSessionDB) CompressionTip(id string) (tip string, err error) {
	err = b.call("get_compression_tip", func() (e error) { tip, e = b.db.CompressionTip(id); return e })
	return tip, err
}

func (b *boundedCronSessionDB) SetSessionTitle(ctx context.Context, sessionID, title string) (ok bool, err error) {
	err = b.call("set_session_title", func() (e error) { ok, e = b.db.SetTitle(sessionID, title); return e })
	return ok, err
}

func (b *boundedCronSessionDB) GetNextTitleInLineage(ctx context.Context, base string) (next string, err error) {
	lineage, has := b.db.(cronTitleLineageDB)
	if !has {
		return base, nil
	}
	err = b.call("get_next_title_in_lineage", func() (e error) { next, e = lineage.GetNextTitleInLineage(ctx, base); return e })
	return next, err
}

func (b *boundedCronSessionDB) sessionLifecycleStatuses(ctx context.Context, ids []string) (statuses map[string]string, err error) {
	err = b.call("session_lifecycle_statuses", func() (e error) { statuses, e = b.db.SessionLifecycleStatuses(ctx, ids); return e })
	return statuses, err
}

func (b *boundedCronSessionDB) UpdateSessionCwd(id, cwd string) error {
	return b.call("update_session_cwd", func() error { return b.db.UpdateSessionCwd(id, cwd, "", "") })
}

func (b *boundedCronSessionDB) EndSession(id, reason string) error {
	return b.call("end_session", func() error { return b.db.EndSession(id, reason) })
}

func (b *boundedCronSessionDB) release() error {
	return b.call("release_or_close", func() error {
		if db, ok := b.db.(*state.DB); ok {
			state.ReleaseOrClose(db)
		}
		return nil
	})
}

// Script is the job, stdout delivered verbatim, empty or wakeAgent false silent, failure alerted
func (s *Store) runNoAgentJob(ctx context.Context, job *Job, jobID, jobName string, cancel CancelSource) jobRunOutcome {
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: s.home}); err != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': no_agent .env reload failed", jobID), "error", err)
	}
	scriptPath := ""
	if job.Script != nil {
		scriptPath = *job.Script
	}
	if pystr.Strip(scriptPath) == "" {
		return s.blockAndPauseJob(home.WithOverride(ctx, s.home), jobID, jobName, noAgentWithoutScriptError)
	}
	workdir := ""
	if wd := s.resolveJobWorkdir(job, jobID); wd != nil {
		workdir = *wd
	}
	ok, output, err := s.runJobScriptWithClaimHeartbeat(ctx, job, scriptPath, workdir, cancel)
	if err != nil {
		s.log.Error(fmt.Sprintf("Job '%s': script execution raised unexpectedly", jobID), "error", err)
		ok, output = false, fmt.Sprintf("Script execution failed: %s", err.Error())
	}
	nowISO := config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S")
	header := jobDocHeader(jobName, jobID, nowISO, "no_agent (script)")
	if !ok {
		alert := fmt.Sprintf("⚠ Cron watchdog '%s' script failed\n\n%s\n\nTime: %s", jobName, output, nowISO)
		return jobRunOutcome{false, fmt.Sprintf("%s**Status:** script failed\n\n%s\n", header, output), alert, strPtr(output)}
	}
	if !parseWakeGate(output) {
		s.log.Info(fmt.Sprintf("Job '%s' (no_agent): wakeAgent=false gate — silent run", jobID))
		return jobRunOutcome{true, header + "**Status:** silent (wakeAgent=false)\n", SilentMarker, nil}
	}
	if pystr.Strip(output) == "" {
		s.log.Info(fmt.Sprintf("Job '%s' (no_agent): empty stdout — silent run", jobID))
		return jobRunOutcome{true, header + "**Status:** silent (empty output)\n", SilentMarker, nil}
	}
	return jobRunOutcome{true, fmt.Sprintf("%s\n---\n\n%s\n", header, output), output, nil}
}

// Hash suppressed change detection before any agent work, an early outcome or the monitor context block
func (s *Store) applyMonitorGate(ctx context.Context, job *Job, jobID, jobName string) (*jobRunOutcome, *string, error) {
	if !jobHasMonitor(job) {
		return nil, nil, nil
	}
	mon, err := s.checkMonitor(ctx, job)
	if err != nil {
		return nil, nil, err
	}
	now := config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S")
	header := jobDocHeader(jobName, jobID, now, "monitor")
	if !mon.OK {
		monErr := ""
		if mon.Error != nil {
			monErr = *mon.Error
		}
		s.log.Error(fmt.Sprintf("Job '%s': monitor source failed: %s", jobID, monErr))
		alert := fmt.Sprintf("⚠ Cron monitor '%s' source failed\n\n%s\n\nTime: %s", jobName, monErr, now)
		return &jobRunOutcome{false, fmt.Sprintf("%s**Status:** monitor source failed\n\n%s\n", header, monErr), alert, mon.Error}, nil, nil
	}
	if !mon.Changed {
		s.log.Info(fmt.Sprintf("Job '%s': monitor output unchanged — suppressing agent run", jobID))
		return &jobRunOutcome{true, header + "**Status:** no_change (agent run suppressed)\n", SilentMarker, nil}, nil, nil
	}
	return nil, mon.ContextBlock, nil
}

// Config derived inputs for one agent backed run
type cronJobConfig struct {
	cfg                 config.Tree
	model               string
	modelCfg            any
	cronDefaultProvider string
}

// Model for the run from the job pin, cron.model, the main model, then the claimed trial model
func (s *Store) loadCronJobConfig(ctx context.Context, job *Job, jobID, jobName string) (cronJobConfig, error) {
	model := ""
	if job.Model != nil && *job.Model != "" {
		model = *job.Model
	} else {
		model = s.cronEnvSetting(ctx, "ICEBATH_MODEL", "")
	}
	jc := cronJobConfig{cfg: config.Tree{}, modelCfg: map[string]any{}}
	cfgPath := filepath.Join(s.home, "config.yaml")
	exists, err := config.ConfigExists(ctx, cfgPath)
	if err == nil && exists {
		var cfg config.Tree
		cfg, err = s.loadConfig()
		if err == nil {
			if cfg == nil {
				cfg = config.Tree{}
			}
			jc.cfg = cfg
			if raw, ok := cfg["model"]; ok && pyTruthy(raw) {
				jc.modelCfg = raw
			}
			cronDefaultModel := ""
			if cronCfg, ok := cfg["cron"].(map[string]any); ok {
				cronDefaultModel = pystr.Strip(pyStrOrEmpty(cronCfg["model"]))
				jc.cronDefaultProvider = pystr.Strip(pyStrOrEmpty(cronCfg["model_provider"]))
			}
			if job.Model == nil || *job.Model == "" {
				if cronDefaultModel != "" {
					model = cronDefaultModel
				} else {
					var main any
					switch mc := jc.modelCfg.(type) {
					case string:
						main = mc
					case map[string]any:
						main = firstTruthy(mc["default"], mc["model"], mc["name"])
					default:
						main = ""
					}
					if m := pystr.Strip(pyStrOrEmpty(main)); m != "" {
						model = m
					}
				}
			}
		}
	}
	if err != nil {
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			return jc, err
		}
		s.log.Warn(fmt.Sprintf("Job '%s': failed to load config.yaml, using defaults: %v", jobID, err))
	}
	model = pystr.Strip(model)
	if model == "" {
		model = provider.TrialModel(ctx, s.home)
	}
	if model == "" {
		jobModel := "None"
		if job.Model != nil {
			jobModel = pyrepr.Repr(*job.Model)
		}
		envModel := s.cronEnvSetting(ctx, "ICEBATH_MODEL", "")
		return jc, fmt.Errorf("Cron job '%s' has no model configured (job.model=%s, ICEBATH_MODEL=%s, config.yaml model.default missing or empty). Set a per-job model via `icebath cron edit %s --model <name>` or set a default with `icebath model <name>`.",
			jobName, jobModel, pyrepr.Repr(envModel), jobID)
	}
	if netCfg, ok := jc.cfg["network"].(map[string]any); ok && pyTruthy(netCfg["force_ipv4"]) {
		home.ApplyIPv4Preference(true)
	}
	jc.model = model
	return jc, nil
}

// Prefill messages from the env override or config, nil when unset, missing or not a JSON list
func (s *Store) loadPrefillMessages(ctx context.Context, cfg config.Tree, jobID string) []any {
	agentCfg, _ := cfg["agent"].(map[string]any)
	prefillFile := s.cronEnvSetting(ctx, "ICEBATH_PREFILL_MESSAGES_FILE", "")
	if prefillFile == "" {
		prefillFile = pyStrOrEmpty(cfg["prefill_messages_file"])
	}
	if prefillFile == "" && agentCfg != nil {
		prefillFile = pyStrOrEmpty(agentCfg["prefill_messages_file"])
	}
	if prefillFile == "" {
		return nil
	}
	pfpath, _ := pypath.PathExpanduser(prefillFile)
	if !filepath.IsAbs(pfpath) {
		pfpath = filepath.Join(s.home, pfpath)
	}
	if _, err := os.Stat(pfpath); err != nil {
		return nil
	}
	raw, err := os.ReadFile(pfpath)
	if err == nil {
		raw = []byte(strings.TrimPrefix(string(raw), "\ufeff"))
		var parsed any
		if err = json.Unmarshal(raw, &parsed); err == nil {
			if list, ok := parsed.([]any); ok {
				return list
			}
			return nil
		}
	}
	s.log.Warn(fmt.Sprintf("Job '%s': failed to parse prefill messages file '%s': %v", jobID, pfpath, err))
	return nil
}

// Refuses an unrunnable job before the agent is built, nil when preflight passes or errors open
func (s *Store) preflightOrBlock(ctx context.Context, job *Job, jobID, jobName string, cfg config.Tree) *jobRunOutcome {
	reason := ""
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Debug(fmt.Sprintf("Job '%s': preflight validation errored — failing open", jobID), "error", r)
				reason = ""
			}
		}()
		if !s.cronPreflightEnabled(cfg) {
			return
		}
		r, err := s.preflightJobConfig(ctx, job, cfg)
		if err != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': preflight validation errored — failing open", jobID), "error", err)
			return
		}
		reason = r
		if reason == "" && job.PreflightAlerted {
			_ = s.clearPreflightAlerted(home.WithOverride(ctx, s.home), jobID)
		}
	}()
	if reason == "" {
		return nil
	}
	out := s.blockedConfigResult(home.WithOverride(ctx, s.home), jobID, jobName, reason)
	return &out
}

// Blocked configuration outcome for the reason, alerting once per job
func (s *Store) blockedConfigResult(ctx context.Context, jobID, jobName, reason string) jobRunOutcome {
	s.log.Warn(fmt.Sprintf("Job '%s' (ID: %s): BLOCKED by pre-dispatch config validation — %s (no LLM call was made)", jobName, jobID, reason))
	alreadyAlerted, err := s.markPreflightAlerted(home.WithOverride(ctx, s.home), jobID)
	if err != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': could not persist preflight alert marker", jobID), "error", err)
		alreadyAlerted = false
	}
	marker := BlockedConfigMarker
	if alreadyAlerted {
		marker = BlockedConfigSilentMarker
	}
	doc := fmt.Sprintf("# Cron Job: %s\n\n**Job ID:** %s\n**Run Time:** %s\n**Status:** BLOCKED (configuration)\n\n"+
		"The pre-run configuration check found a problem, so the agent did not run (nothing was charged).\n\n"+
		"**Reason:** %s\n\n"+
		"Icebath tries again at the next scheduled time and clears this state on the first healthy run; this alert is not repeated. "+
		"Check with `icebath cron doctor`. Set `cron.preflight: false` in config.yaml to disable this check.",
		jobName, jobID, config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S"), reason)
	return jobRunOutcome{false, doc, "", strPtr(marker + " " + reason)}
}

// Runtime for the run, walking the fallback chain on auth or transient network resolve failures
func (s *Store) resolveJobRuntime(ctx context.Context, job *Job, jobID string, jc cronJobConfig) (*provider.Runtime, string, *string, error) {
	model := jc.model
	requested := ""
	if job.Provider != nil && *job.Provider != "" {
		requested = *job.Provider
	} else {
		requested = jc.cronDefaultProvider
	}
	baseURL := ""
	if job.BaseURL != nil {
		baseURL = *job.BaseURL
	}
	rt, resolveErr := resolveRuntimeProviderSeam(ctx, s.home, requested, model, baseURL, "")
	if resolveErr == nil {
		return rt, model, nil, nil
	}
	var authErr *provider.AuthError
	isAuth := errors.As(resolveErr, &authErr)
	isTransient := isTransientProviderResolveError(resolveErr)
	if !isAuth && !isTransient {
		return nil, "", nil, fmt.Errorf("%s: %w", provider.FormatRuntimeProviderError(resolveErr), resolveErr)
	}
	chain := jobFallbackChain(job, jc.cfg)
	kind := "transient network"
	if isAuth {
		kind = "auth"
	}
	next := "no fallback configured"
	if len(chain) > 0 {
		next = "trying fallback"
	} else if jobRoutePinned(job) {
		next = "not falling back: the job is pinned"
	}
	s.log.Warn(fmt.Sprintf("Job '%s': primary provider resolve failed (%s: %v), %s", jobID, kind, resolveErr, next))
	for _, entry := range chain {
		if entry == nil {
			continue
		}
		fbProvider := pystr.Strip(pyStrOrEmpty(entry["provider"]))
		fbModel := pystr.Strip(pyStrOrEmpty(entry["model"]))
		if fbProvider == "" || fbModel == "" {
			continue
		}
		fbBase := ""
		if pyTruthy(entry["base_url"]) {
			fbBase = pyStrOrEmpty(entry["base_url"])
		}
		fbKey := provider.ResolveFallbackEntryAPIKey(ctx, s.home, entry)
		fbRT, err := resolveRuntimeProviderSeam(ctx, s.home, fbProvider, fbModel, fbBase, fbKey)
		if err != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': fallback %s failed: %v", jobID, fbProvider, err))
			continue
		}
		fbRT.Provider = provider.EffectiveRuntimeProvider(entry, fbRT)
		s.log.Info(fmt.Sprintf("Job '%s': fallback resolved to %s model %s", jobID, fbRT.Provider, fbModel))
		primary := requested
		if primary == "" {
			if mc, ok := jc.modelCfg.(map[string]any); ok {
				primary = pyStrOrEmpty(mc["provider"])
			}
		}
		notice := provider.PreAgentFallbackNotice(primary, model, fbRT.Provider, fbModel)
		return fbRT, fbModel, &notice, nil
	}
	return nil, "", nil, fmt.Errorf("%s: %w", provider.FormatRuntimeProviderError(resolveErr), resolveErr)
}

// Credential pool of the runtime provider when it holds credentials, nil otherwise
func (s *Store) loadCredentialPool(rt *provider.Runtime, jobID string) *provider.Pool {
	runtimeProvider := strings.ToLower(pystr.Strip(rt.Provider))
	if runtimeProvider == "" {
		return nil
	}
	env := provider.PoolEnv{Store: provider.NewStore(s.home), Secrets: config.NewSecrets(s.home), Config: provider.ConfigLoader(s.home), Log: s.log}
	pool := provider.LoadPool(env, runtimeProvider)
	if pool != nil && pool.HasCredentials() {
		s.log.Info(fmt.Sprintf("Job '%s': loaded credential pool for provider %s with %d entries", jobID, runtimeProvider, len(pool.Entries())))
		return pool
	}
	return nil
}

// Registers configured MCP servers for the agent tool registry, failures logged and never fatal
func (s *Store) initCronMCPTools(ctx context.Context, jobID string) {
	names, err := mcp.ConnectAll(ctx, &tools.Env{Home: s.home}, nil)
	if err != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': MCP initialization failed (non-fatal): %v", jobID, err))
		return
	}
	if len(names) > 0 {
		s.log.Info(fmt.Sprintf("Job '%s': %d MCP tool(s) available", jobID, len(names)))
	}
}

// Owner execution of a canonical cron run, the gateway session_cron binding
var currentOwnerExecution = func(ctx context.Context) (gateway.CronOwnerExecution, bool) {
	return gateway.CurrentCronExecution(ctx)
}

// Session store for the run, the owner's exact store for canonical runs, nil when the legacy path has none
func (s *Store) openCronSessionDB(ctx context.Context, job *Job) (*state.DB, error) {
	owner, isOwner := currentOwnerExecution(ctx)
	if !isOwner && tools.IsWorkerProcess() {
		return nil, tools.NewWorkerPersistenceError("worker_cron_registration_required")
	}
	timeout := s.getSessionDBTimeout(ctx)
	acquire := func() (*state.DB, error) {
		if !isOwner {
			return state.Acquire(ctx, home.StatePath(s.home))
		}
		db, err := state.Acquire(ctx, owner.DBPath)
		if err != nil {
			return nil, err
		}
		if db != owner.DB {
			state.ReleaseOrClose(db)
			return nil, errors.New("cron requires the canonical owner store")
		}
		return db, nil
	}
	jobLabel := job.ID
	if jobLabel == "" {
		jobLabel = "?"
	}
	var db *state.DB
	var err error
	if timeout <= 0 {
		db, err = acquire()
	} else {
		type acquired struct {
			db  *state.DB
			err error
		}
		ch := make(chan acquired, 1)
		go func() {
			d, e := acquire()
			ch <- acquired{d, e}
		}()
		select {
		case r := <-ch:
			db, err = r.db, r.err
		case <-time.After(time.Duration(timeout * float64(time.Second))):
			go closeLateSessionDBResult(func() (*state.DB, error) { r := <-ch; return r.db, r.err })
			if isOwner {
				return nil, &pyTimeoutError{"Canonical cron session store acquisition timed out"}
			}
			s.log.Error(fmt.Sprintf("Job '%s': SessionDB init did not return within %.0fs — proceeding without a session store for this run instead of blocking it forever", jobLabel, timeout))
			return nil, nil
		}
	}
	if err != nil {
		if isOwner {
			return nil, err
		}
		s.log.Debug(fmt.Sprintf("Job '%s': SQLite session store not available: %v", jobLabel, err))
		return nil, nil
	}
	return db, nil
}

// Python TimeoutError raised by the cron run path
type pyTimeoutError struct{ msg string }

func (e *pyTimeoutError) Error() string      { return e.msg }
func (e *pyTimeoutError) PyTypeName() string { return "TimeoutError" }
func (e *pyTimeoutError) Timeout() bool      { return true }

// Logs the agent's last activity, hard interrupts it and returns the inactivity TimeoutError
func (s *Store) raiseInactivityTimeout(a CronAgent, jobName string, limitS float64) error {
	activity := map[string]any{}
	if a != nil {
		if act := a.GetActivitySummary(); act != nil {
			activity = act
		}
	}
	lastDesc := any("unknown")
	if v, ok := activity["last_activity_desc"]; ok {
		lastDesc = v
	}
	secsAgo, _, _ := anyFloat(activity["seconds_since_activity"])
	tool := "none"
	if pyTruthy(activity["current_tool"]) {
		tool = pyStrOrEmpty(activity["current_tool"])
	}
	apiCalls := activity["api_call_count"]
	if apiCalls == nil {
		apiCalls = 0
	}
	maxIter := activity["max_iterations"]
	if maxIter == nil {
		maxIter = 0
	}
	s.log.Error(fmt.Sprintf("Job '%s' idle for %.0fs (inactivity limit %.0fs) | last_activity=%v | iteration=%v/%v | tool=%s",
		jobName, secsAgo, limitS, lastDesc, apiCalls, maxIter, tool))
	if a != nil {
		requestHardInterrupt(a, "Cron job timed out (inactivity)", "cron inactivity watchdog")
	}
	return &pyTimeoutError{fmt.Sprintf("Cron job '%s' idle for %ds (limit %ds) — last activity: %v", jobName, int(secsAgo), int(limitS), lastDesc)}
}

// Completion handle of an agent conversation running on its own goroutine
type agentFuture struct {
	mu        sync.Mutex
	done      chan struct{}
	finished  bool
	result    map[string]any
	err       error
	callbacks []func()
}

func newAgentFuture() *agentFuture { return &agentFuture{done: make(chan struct{})} }

func (f *agentFuture) Done() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished
}

func (f *agentFuture) finish(result map[string]any, err error) {
	f.mu.Lock()
	f.result, f.err, f.finished = result, err, true
	callbacks := f.callbacks
	f.callbacks = nil
	f.mu.Unlock()
	close(f.done)
	for _, cb := range callbacks {
		cb()
	}
}

// Runs the callback once the conversation ends, inline when it already has
func (f *agentFuture) addDoneCallback(cb func()) {
	f.mu.Lock()
	if !f.finished {
		f.callbacks = append(f.callbacks, cb)
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()
	cb()
}

// Runs the conversation under the inactivity watchdog, claim heartbeat and fire claim cancel checks
func (s *Store) runAgentWithWatchdog(ctx context.Context, a CronAgent, prompt string, job *Job, jobID, jobName, taskID string, cancel CancelSource, worker **agentFuture) (map[string]any, error) {
	cronTimeout := s.cronInactivitySeconds(ctx)
	limit := 0.0
	hasLimit := cronTimeout > 0
	if hasLimit {
		limit = cronTimeout
	}
	poll := time.Duration(watchdogPollSeconds * float64(time.Second))
	isOneshot := job.Schedule != nil && job.Schedule.Kind == "once"
	runClaimOwner := ""
	if job.RunClaim != nil {
		runClaimOwner = job.RunClaim.By
	}
	lastClaimHeartbeat := monoNow()
	abortIfFireClaimLost := func() error {
		if cancel == nil || !cancel.IsSet() {
			return nil
		}
		reason := cancelReason(cancel)
		if a != nil {
			a.Interrupt(fmt.Sprintf("Cron run stopped: %s", reason))
		}
		return fmt.Errorf("Cron job '%s' was stopped: %s", jobName, reason)
	}
	heartbeatRunClaimIfDue := func() {
		if !isOneshot || runClaimOwner == "" {
			return
		}
		mono := monoNow()
		if mono-lastClaimHeartbeat < runClaimHeartbeatSeconds {
			return
		}
		lastClaimHeartbeat = mono
		if _, err := s.heartbeatRunClaim(home.WithOverride(ctx, s.home), jobID, runClaimOwner); err != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': run_claim heartbeat failed", jobName), "error", err)
		}
	}
	future := newAgentFuture()
	if worker != nil {
		*worker = future
	}
	go func() {
		var result map[string]any
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = panicTraceback(r)
				}
			}()
			result, err = a.RunConversation(ctx, prompt, taskID)
		}()
		future.finish(result, err)
	}()
	idleSeconds := func() (float64, error) {
		act := a.GetActivitySummary()
		if act == nil {
			return 0, nil
		}
		v, _, err := anyFloat(act["seconds_since_activity"])
		if err != nil {
			return 0, nil
		}
		return v, nil
	}
	watchStop := make(chan struct{})
	inactivityTimeout := false
	var inactivityMu sync.Mutex
	if hasLimit {
		go func() {
			if inactivityWatchdogLoop(idleSeconds, limit, poll, watchStop, future.Done, state.NewAwakeIdleMeter()) {
				inactivityMu.Lock()
				inactivityTimeout = true
				inactivityMu.Unlock()
			}
		}()
	}
	timedOutNow := func() bool {
		inactivityMu.Lock()
		defer inactivityMu.Unlock()
		return inactivityTimeout
	}
	executionID := ""
	if job.ExecutionID != nil {
		executionID = *job.ExecutionID
	}
	progress := newExecutionProgressStamper(s, executionID, jobName, func() float64 { v, _ := idleSeconds(); return v }, runClaimHeartbeatSeconds)
	defer close(watchStop)
	if !hasLimit && !isOneshot && cancel == nil {
		<-future.done
	} else {
		for {
			select {
			case <-future.done:
			case <-time.After(poll):
			}
			if future.Done() {
				if err := abortIfFireClaimLost(); err != nil {
					return nil, err
				}
				break
			}
			if timedOutNow() {
				break
			}
			if err := abortIfFireClaimLost(); err != nil {
				return nil, err
			}
			heartbeatRunClaimIfDue()
			progress.tick(ctx)
		}
	}
	if timedOutNow() {
		return nil, s.raiseInactivityTimeout(a, jobName, limit)
	}
	if future.err != nil {
		return nil, future.err
	}
	if future.result == nil {
		return nil, errors.New("agent.run_conversation returned NoneType instead of dict: None")
	}
	return future.result, nil
}

// Deliverable reply of a conversation result, failing on a reported failure and dropping explainer only turns
func finalResponseFromResult(ctx context.Context, s *Store, result map[string]any, jobID, jobName string) (string, error) {
	turnExitReason := pyStrOrEmpty(result["turn_exit_reason"])
	finalText := pystr.Strip(pyStrOrEmpty(result["final_response"]))
	maxIterationSummary := isMaxIterationHandoff(result)
	failed, _ := result["failed"].(bool)
	completed, hasCompleted := result["completed"].(bool)
	if failed || (hasCompleted && !completed && !maxIterationSummary) {
		msg := pyStrOrEmpty(result["error"])
		if msg == "" {
			msg = finalText
		}
		if msg == "" {
			msg = "agent reported failure"
		}
		return "", errors.New(msg)
	}
	if maxIterationSummary {
		s.log.Warn(fmt.Sprintf("Job '%s' reached the iteration limit but produced a final fallback response; delivering the response instead of failing the cron run", jobName))
	}
	finalResponse := pyStrOrEmpty(result["final_response"])
	if finalResponse != "" {
		messages, _ := result["messages"].([]any)
		finalResponse = gateway.RepairExplicitComputerUseMediaPathsAt(ctx, finalResponse, messages, 0)
	}
	if pystr.Strip(finalResponse) == "(No response generated)" {
		finalResponse = ""
	}
	if pystr.Strip(finalResponse) != "" && turnExitReason != "" {
		model := pyStrOrEmpty(result["model"])
		causes := append([]*string{nil}, func() []*string {
			out := make([]*string, 0, len(state.PersistenceErrorCauses))
			for _, c := range state.PersistenceErrorCauses {
				out = append(out, strPtr(c))
			}
			return out
		}()...)
		variants := map[string]bool{}
		for _, cause := range causes {
			for _, withModel := range []bool{true, false} {
				var m *string
				if withModel {
					m = &model
				}
				if v := formatTurnCompletionExplanation(ctx, turnExitReason, cause, m); v != "" {
					variants[pystr.Strip(v)] = true
				}
			}
		}
		if variants[pystr.Strip(finalResponse)] {
			s.log.Info(fmt.Sprintf("Job '%s': abnormal empty turn (%s) — suppressing explainer for cron delivery", jobID, turnExitReason))
			finalResponse = ""
		}
	}
	return finalResponse, nil
}

// Titles, classifies, ends and releases the cron session after the agent turn returned
func (s *Store) finalizeCronSession(ctx context.Context, db cronSessionDB, a CronAgent, jobID, jobName, cronSessionID, workdir string) {
	b := &boundedCronSessionDB{s: s, db: db, jobID: jobID}
	finalID := cronSessionID
	if tip, err := b.CompressionTip(cronSessionID); err != nil {
		if a != nil {
			if sid := a.SessionID(); sid != "" {
				finalID = sid
			}
		}
		s.log.Debug(fmt.Sprintf("Job '%s': failed to resolve cron compression tip: %v", jobID, err))
	} else if tip != "" {
		finalID = tip
	}
	titleBase := pystr.Strip(truncateRunes(strings.Join(strings.Fields(jobName), " "), 60))
	if titleBase == "" {
		titleBase = "cron " + jobID
	}
	cronTitle := fmt.Sprintf("%s · %s", titleBase, config.SafeStrftime(s.clk.now(), "%b %d %H:%M"))
	titled, err := setCronSessionTitle(ctx, b, finalID, cronTitle)
	if err == nil && titled == "" {
		_, err = setCronSessionTitle(ctx, b, finalID, "cron "+jobID)
	}
	if err != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': failed to set cron session title: %v", jobID, err))
		lineageTitle, lerr := b.GetNextTitleInLineage(ctx, "cron "+jobID)
		fallbacks := []string{}
		if lerr == nil {
			fallbacks = append(fallbacks, lineageTitle)
		}
		fallbacks = append(fallbacks, fmt.Sprintf("cron %s %s", jobID, lastRunes(finalID, 6)))
		for _, fb := range fallbacks {
			if t, ferr := setCronSessionTitle(ctx, b, finalID, fb); ferr == nil && t != "" {
				break
			}
		}
	}
	endReason := "cron_complete"
	if statuses, serr := b.sessionLifecycleStatuses(ctx, []string{finalID}); serr != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': session lifecycle classification failed: %v", jobID, serr))
	} else if lc := statuses[finalID]; lc == "interrupted" || lc == "error" || lc == "empty" {
		endReason = "cron_incomplete_no_output"
		s.log.Warn(fmt.Sprintf("Job '%s': session ended without a final assistant message (lifecycle=%s) — booking run as %s", jobID, lc, endReason))
	}
	if workdir != "" {
		if werr := b.UpdateSessionCwd(finalID, workdir); werr != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': failed to stamp workdir on session row: %v", jobID, werr))
		}
	}
	if eerr := b.EndSession(finalID, endReason); eerr != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': failed to end session: %v", jobID, eerr))
	} else if a != nil {
		a.SetEndSessionOnClose(false)
	}
	if rerr := b.release(); rerr != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': failed to close SQLite session store: %v", jobID, rerr))
	}
}

// Record carries a schedule_display key, absent only on a loaded legacy record without one
func (j *Job) hasScheduleDisplay() bool {
	if j.ScheduleDisplay != "" || j.keyOrder == nil {
		return true
	}
	for _, k := range j.keyOrder {
		if k == "schedule_display" {
			return true
		}
	}
	return false
}

// CRLF and lone CR become LF, matching a text mode read of the archive
func normalizeNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Header of the persisted run document with title, ids, schedule and prompt
func (s *Store) runDocHeader(job *Job, title, jobID, prompt, promptStamp string) string {
	display := job.ScheduleDisplay
	if display == "" && !job.hasScheduleDisplay() {
		display = "N/A"
	}
	return fmt.Sprintf("# Cron Job: %s\n\n**Job ID:** %s\n**Run Time:** %s\n**Schedule:** %s\n\n%s%s%s%s",
		title, jobID, config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S"), display, promptStamp, promptHeading, prompt, promptSeparator)
}

// Runs every pre agent gate and builds the prompt, an early outcome short circuits the run
func (s *Store) prepareJobPrompt(ctx context.Context, job *Job, jobID, jobName string, extraPrompt *string, cancel CancelSource) (*jobRunOutcome, string, error) {
	if !job.NoAgent {
		if err := config.RequireParseableUserConfig(ctx, s.home, false); err != nil {
			var invalid *config.InvalidUserConfigError
			if !errors.As(err, &invalid) {
				return nil, "", err
			}
			s.log.Error(fmt.Sprintf("Job '%s': refusing to run — %v", jobID, err))
			return &jobRunOutcome{false, fmt.Sprintf("# Cron Job: %s\n\nError: %s\n", jobName, err.Error()), "", strPtr(err.Error())}, "", nil
		}
	}
	if job.NoAgent {
		out := s.runNoAgentJob(ctx, job, jobID, jobName, cancel)
		return &out, "", nil
	}
	if jobPayloadIsEmpty(job) {
		out := s.blockAndPauseJob(home.WithOverride(ctx, s.home), jobID, jobName, emptyPayloadError)
		return &out, "", nil
	}
	early, monitorContext, err := s.applyMonitorGate(ctx, job, jobID, jobName)
	if err != nil {
		return nil, "", err
	}
	if early != nil {
		return early, "", nil
	}
	var prerun *scriptRun
	if job.Script != nil && *job.Script != "" {
		workdir := ""
		if wd := s.resolveJobWorkdir(job, jobID); wd != nil {
			workdir = *wd
		}
		ok, output, err := s.runJobScriptWithClaimHeartbeat(ctx, job, *job.Script, workdir, cancel)
		if err != nil {
			return nil, "", err
		}
		prerun = &scriptRun{Success: ok, Output: output}
		if ok && !parseWakeGate(output) {
			s.log.Info(fmt.Sprintf("Job '%s' (ID: %s): wakeAgent=false, skipping agent run", jobName, jobID))
			observability.NoteCronSkipped(job)
			doc := fmt.Sprintf("# Cron Job: %s\n\n**Job ID:** %s\n**Run Time:** %s\n\nScript gate returned `wakeAgent=false` — agent skipped.\n",
				jobName, jobID, config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S"))
			return &jobRunOutcome{true, doc, SilentMarker, nil}, "", nil
		}
	}
	opts := buildPromptOptions{Prerun: prerun}
	if extraPrompt != nil {
		opts.ExtraPrompt = *extraPrompt
	}
	if monitorContext != nil {
		opts.RuntimeDataPrompt = *monitorContext
	}
	prompt, err := s.buildJobPrompt(ctx, job, opts)
	if err != nil {
		var blocked *CronPromptInjectionBlocked
		if !errors.As(err, &blocked) {
			return nil, "", err
		}
		s.log.Warn(fmt.Sprintf("Job '%s' (ID: %s): blocked by prompt-injection scanner — %s", jobName, jobID, blocked.Error()))
		doc := fmt.Sprintf("# Cron Job: %s\n\n**Job ID:** %s\n**Run Time:** %s\n**Status:** BLOCKED\n\n"+
			"The assembled prompt (user prompt + loaded skill content) tripped the cron injection scanner and the agent was NOT run.\n\n"+
			"**Scanner result:** %s\n\n"+
			"Audit the skill(s) attached to this job for prompt-injection payloads or invisible-unicode markers. "+
			"If the skill is legitimate and the match is a false positive, rephrase the content to avoid the threat pattern "+
			"(`tools/cronjob_tools.py::_CRON_THREAT_PATTERNS`).",
			jobName, jobID, config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S"), blocked.Error())
		return &jobRunOutcome{false, doc, "", strPtr(blocked.Error())}, "", nil
	}
	if prompt == nil {
		s.log.Info(fmt.Sprintf("Job '%s': script produced no output, skipping AI call.", jobName))
		observability.NoteCronSkipped(job)
		return &jobRunOutcome{true, "", SilentMarker, nil}, "", nil
	}
	return nil, *prompt, nil
}

// Per run session variables, tool cwd and spend binding, every setter undone by exit
type cronRunScope struct {
	s           *Store
	workdir     string
	taskID      string
	env         *tools.Env
	restoreGate func()
	cronPrev    *string
}

func (s *Store) newCronRunScope(job *Job, jobID string, executionID *string) *cronRunScope {
	scope := &cronRunScope{s: s, env: &tools.Env{Home: s.home, Values: &sync.Map{}}}
	if wd := s.resolveJobWorkdir(job, jobID); wd != nil {
		scope.workdir = *wd
	}
	tools.SetSessionVars(scope.env, tools.SessionVars{NoAsyncDelivery: true, Cwd: scope.workdir})
	for _, name := range cronDeliveryVars {
		tools.RestoreSessionVar(scope.env, name, "", true)
	}
	run := ""
	switch {
	case executionID != nil && *executionID != "":
		run = *executionID
	case job.ExecutionID != nil && *job.ExecutionID != "":
		run = *job.ExecutionID
	default:
		run = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	scope.taskID = fmt.Sprintf("cron:%s:%s", jobID, run)
	if scope.workdir != "" {
		tools.RecordSessionCwd(scope.env, scope.taskID, scope.workdir)
	}
	return scope
}

// Marks the context a cron session outside the dispatcher and binds MCP spend to the task
func (c *cronRunScope) enter(ctx context.Context) context.Context {
	if prev, had := tools.BoundSessionVar(c.env, "ICEBATH_CRON_SESSION"); had {
		c.cronPrev = &prev
	}
	tools.RestoreSessionVar(c.env, "ICEBATH_CRON_SESSION", "1", true)
	c.restoreGate = tools.EnterNonDispatcherOwnedContext(c.env)
	return mcp.BindRun(ctx, c.taskID)
}

// Clears the tool cwd and every session variable the scope set
func (c *cronRunScope) exit() {
	tools.ClearSessionCwd(c.env, c.taskID)
	tools.ClearSessionVars(c.env)
	tools.RestoreSessionVar(c.env, "ICEBATH_CRON_SESSION", deref(c.cronPrev), c.cronPrev != nil)
	if c.restoreGate != nil {
		c.restoreGate()
	}
	for _, name := range cronDeliveryVars {
		tools.RestoreSessionVar(c.env, name, "", true)
	}
}

// Rereads .env for the run and publishes the auto deliver target into the session variables
func (s *Store) reloadDotenvAndPublishDeliveryTarget(ctx context.Context, job *Job, scope *cronRunScope) error {
	provider.ResetSecretSourceCache(s.home)
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: s.home}); err != nil {
		return err
	}
	target := s.resolveDeliveryTarget(ctx, job)
	if target != nil {
		tools.RestoreSessionVar(scope.env, "ICEBATH_CRON_AUTO_DELIVER_PLATFORM", target.Platform, true)
		tools.RestoreSessionVar(scope.env, "ICEBATH_CRON_AUTO_DELIVER_CHAT_ID", target.ChatID, true)
		tools.RestoreSessionVar(scope.env, "ICEBATH_CRON_AUTO_DELIVER_THREAD_ID", target.ThreadID, true)
	}
	return nil
}

// Everything the agent needs that resolves from job and config, or the preflight block
type cronAgentSetup struct {
	blocked         *jobRunOutcome
	model           string
	runtime         *provider.Runtime
	prefill         []any
	maxIterations   int
	reasoningConfig map[string]any
	fallbackModel   []map[string]any
	credentialPool  *provider.Pool
	fallbackNotice  *string
}

// Exfil guard, preflight, runtime with fallback chain, credential pool then MCP, in the original gate order
func (s *Store) resolveCronAgentSetup(ctx context.Context, job *Job, jobID, jobName string, jc cronJobConfig) (*cronAgentSetup, error) {
	cfg := jc.cfg
	setup := &cronAgentSetup{model: jc.model}
	setup.prefill = s.loadPrefillMessages(ctx, cfg, jobID)
	var maxTurns any
	if agentCfg, ok := cfg["agent"].(map[string]any); ok {
		maxTurns = agentCfg["max_turns"]
	}
	if maxTurns == nil {
		maxTurns = cfg["max_turns"]
	}
	setup.maxIterations = config.ResolveTurnLimit(maxTurns, config.TurnLimitUnlimited)
	if err := s.guardJobCredentialExfil(job); err != nil {
		return nil, err
	}
	setup.blocked = s.preflightOrBlock(ctx, job, jobID, jobName, cfg)
	if setup.blocked != nil {
		return setup, nil
	}
	rt, model, notice, err := s.resolveJobRuntime(ctx, job, jobID, jc)
	if err != nil {
		return nil, err
	}
	setup.runtime, setup.model, setup.fallbackNotice = rt, model, notice
	setup.reasoningConfig = s.resolveJobReasoningConfig(job, cfg, setup.model)
	setup.fallbackModel = jobFallbackChain(job, cfg)
	setup.credentialPool = s.loadCredentialPool(setup.runtime, jobID)
	s.initCronMCPTools(ctx, jobID)
	if s.cronPreflightEnabled(cfg) {
		if reason := s.emptyRequestedMCPToolsets(ctx, job, cfg); reason != "" {
			out := s.blockedConfigResult(home.WithOverride(ctx, s.home), jobID, jobName, reason)
			setup.blocked = &out
		}
	}
	return setup, nil
}

// Agent construction arguments for the run, the AIAgent keyword set the cron path passes
func (s *Store) cronAgentSpecFor(ctx context.Context, job *Job, cfg config.Tree, setup *cronAgentSetup, scope *cronRunScope, sessionID string, db *state.DB) (AgentOptions, error) {
	pr, _ := cfg["provider_routing"].(map[string]any)
	if pr == nil {
		pr = map[string]any{}
	}
	openrouter, _ := cfg["openrouter"].(map[string]any)
	if openrouter == nil {
		openrouter = map[string]any{}
	}
	enabled, err := resolveCronEnabledToolsets(ctx, job, cfg)
	if err != nil {
		return AgentOptions{}, err
	}
	return AgentOptions{
		Model:                    setup.model,
		Runtime:                  setup.runtime,
		MaxIterations:            setup.maxIterations,
		ReasoningConfig:          setup.reasoningConfig,
		PrefillMessages:          setup.prefill,
		FallbackModel:            setup.fallbackModel,
		CredentialPool:           setup.credentialPool,
		ProvidersAllowed:         pr["only"],
		ProvidersIgnored:         pr["ignore"],
		ProvidersOrder:           pr["order"],
		ProviderSort:             pr["sort"],
		OpenRouterMinCodingScore: openrouter["min_coding_score"],
		EnabledToolsets:          enabled,
		DisabledToolsets:         resolveCronDisabledToolsets(cfg),
		QuietMode:                true,
		SkipContextFiles:         scope.workdir == "",
		LoadSoulIdentity:         true,
		SkipMemory:               false,
		SkipBackgroundReview:     true,
		Platform:                 "cron",
		SessionID:                sessionID,
		SessionDB:                db,
		Env:                      scope.env,
		Workdir:                  scope.workdir,
	}, nil
}

// One usage audit line per fire with the fire id and start clock
type fireAudit struct {
	s      *Store
	job    *Job
	jobID  string
	model  string
	fireID string
	start  float64
}

func (s *Store) newFireAudit(job *Job, jobID, model string) *fireAudit {
	return &fireAudit{s: s, job: job, jobID: jobID, model: model, fireID: strings.ReplaceAll(uuid.NewString(), "-", ""), start: monoNow()}
}

func (f *fireAudit) write(result map[string]any, responseSilent bool, errText *string) {
	rec := pyjson.NewObject()
	rec.Set("ts", utcnowISOms())
	rec.Set("job_id", f.jobID)
	rec.Set("fire_id", f.fireID)
	rec.Set("prompt_tokens", result["prompt_tokens"])
	rec.Set("completion_tokens", result["completion_tokens"])
	rec.Set("total_tokens", result["total_tokens"])
	rec.Set("response_silent", responseSilent)
	var deliver any
	if f.job.Deliver != nil {
		deliver = *f.job.Deliver
	}
	rec.Set("deliver_target", deliver)
	var model any
	if f.model != "" {
		model = f.model
	}
	rec.Set("model", model)
	rec.Set("duration_ms", int((monoNow()-f.start)*1000))
	var e any
	if errText != nil {
		e = *errText
	}
	rec.Set("error", e)
	f.s.writeUsageAudit(rec)
}

// Options of one run_job call
type runJobOptions struct {
	// Receives the live agent instead of tearing it down, the caller tears it down after delivery
	DeferAgentTeardown *[]CronAgent
	ExtraPrompt        *string
	Cancel             CancelSource
	ExecutionID        *string
}

// Agent setup resolver run_job calls, the module attribute tests patch
var resolveCronAgentSetupSeam = (*Store).resolveCronAgentSetup

// Canonical owner run run_job hands a non owner fire to, the module attribute tests patch
var runCanonicalJobSeam = (*Store).runCanonicalJob

// Executes one job and returns the success flag, run document, final response and error text
func (s *Store) runJob(ctx context.Context, job *Job, o runJobOptions) (jobRunOutcome, error) {
	owner, isOwner := currentOwnerExecution(ctx)
	if isOwner && (owner.JobID != job.ID || o.ExecutionID == nil || owner.ExecutionID != *o.ExecutionID) {
		isOwner = false
	}
	if !job.NoAgent && !isOwner {
		return runCanonicalJobSeam(s, ctx, job, o.ExtraPrompt, o.Cancel, o.ExecutionID)
	}
	jobID := job.ID
	jobName := job.Name
	if jobName == "" {
		jobName = job.Prompt
	}
	if jobName == "" {
		jobName = jobID
	}
	if jobName == "" {
		jobName = "cron job"
	}
	early, prompt, err := s.prepareJobPrompt(ctx, job, jobID, jobName, o.ExtraPrompt, o.Cancel)
	if err != nil {
		return jobRunOutcome{}, err
	}
	if early != nil {
		return *early, nil
	}
	cronSessionID := fmt.Sprintf("cron_%s_%s", jobID, config.SafeStrftime(s.clk.now(), "%Y%m%d_%H%M%S"))
	if isOwner {
		cronSessionID = owner.SessionID
	}
	s.log.Info(fmt.Sprintf("Running job '%s' (ID: %s)", jobName, jobID))
	s.log.Info(fmt.Sprintf("Prompt: %s", truncateRunes(prompt, 100)))
	var a CronAgent
	var sessionDB *state.DB
	var audit *fireAudit
	var worker *agentFuture
	scope := s.newCronRunScope(job, jobID, o.ExecutionID)
	runCtx := scope.enter(ctx)
	defer func() {
		deferred := deferTeardownToRunningWorker(s, worker, sessionDB, a, jobID, jobName, cronSessionID, scope.workdir)
		scope.exit()
		if sessionDB != nil && !deferred {
			s.finalizeCronSession(ctx, sessionDB, a, jobID, jobName, cronSessionID, scope.workdir)
		}
		if !deferred {
			if o.DeferAgentTeardown != nil {
				if a != nil {
					*o.DeferAgentTeardown = append(*o.DeferAgentTeardown, a)
				}
			} else {
				s.teardownCronAgent(a, jobID, nil)
			}
		}
	}()
	outcome, runErr := func() (jobRunOutcome, error) {
		if scope.workdir != "" {
			s.log.Info(fmt.Sprintf("Job '%s': using task-scoped workdir %s", jobID, scope.workdir))
		}
		if err := s.reloadDotenvAndPublishDeliveryTarget(runCtx, job, scope); err != nil {
			return jobRunOutcome{}, err
		}
		jc, err := s.loadCronJobConfig(runCtx, job, jobID, jobName)
		if err != nil {
			return jobRunOutcome{}, err
		}
		setup, err := resolveCronAgentSetupSeam(s, runCtx, job, jobID, jobName, jc)
		if err != nil {
			return jobRunOutcome{}, err
		}
		if setup.blocked != nil {
			return *setup.blocked, nil
		}
		sessionDB, err = s.openCronSessionDB(runCtx, job)
		if err != nil {
			return jobRunOutcome{}, err
		}
		spec, err := s.cronAgentSpecFor(runCtx, job, jc.cfg, setup, scope, cronSessionID, sessionDB)
		if err != nil {
			return jobRunOutcome{}, err
		}
		a, err = newCronAgent(runCtx, spec)
		if err != nil {
			a = nil
			return jobRunOutcome{}, err
		}
		audit = s.newFireAudit(job, jobID, setup.model)
		result, err := s.runAgentWithWatchdog(runCtx, a, prompt, job, jobID, jobName, scope.taskID, o.Cancel, &worker)
		if err != nil {
			return jobRunOutcome{}, err
		}
		finalResponse, err := finalResponseFromResult(ctx, s, result, jobID, jobName)
		if err != nil {
			return jobRunOutcome{}, err
		}
		if setup.fallbackNotice != nil && *setup.fallbackNotice != "" && pystr.Strip(finalResponse) != "" && !isCronSilenceResponse(finalResponse) {
			if _, marked := cronFailureMarkerError(finalResponse); !marked {
				finalResponse = *setup.fallbackNotice + "\n\n" + finalResponse
			}
		}
		logged := finalResponse
		if logged == "" {
			logged = "(No response generated)"
		}
		framedPrompt := normalizeNewlines(prompt)
		logged = normalizeNewlines(logged)
		output := s.runDocHeader(job, jobName, jobID, framedPrompt, fmt.Sprintf("%s%d\n", promptFrame, pystr.Len(framedPrompt))) +
			fmt.Sprintf("%s%d\n%s%s%s", responseFrame, pystr.Len(logged), responseHeading, logged, responseTerminator)
		s.log.Info(fmt.Sprintf("Job '%s' completed successfully", jobName))
		audit.write(result, isCronSilenceResponse(finalResponse), nil)
		return jobRunOutcome{true, output, finalResponse, nil}, nil
	}()
	if runErr == nil {
		return outcome, nil
	}
	errorMsg := fmt.Sprintf("%s: %s", cronExceptionName(runErr), runErr.Error())
	s.log.Error(fmt.Sprintf("Job '%s' failed: %s", jobName, errorMsg))
	apiCalls := 0
	if a != nil {
		apiCalls = a.SessionAPICalls()
	}
	if isModelUnreachableFailure(runErr, apiCalls) {
		job.run.modelUnreachable = true
	}
	if hold, ok := quotaHoldSecondsFromFailure(runErr); ok && hold != 0 {
		job.run.quotaHoldSeconds = hold
	}
	if audit != nil {
		audit.write(map[string]any{}, false, &errorMsg)
	}
	output := s.runDocHeader(job, jobName+" (FAILED)", jobID, prompt, "") + formatRunError(s, runErr)
	return jobRunOutcome{false, output, "", &errorMsg}, nil
}

// Releases an ephemeral cron agent's resources within the cleanup bound
func (s *Store) teardownCronAgent(a CronAgent, jobID string, timeoutSeconds *float64) {
	s.runCronCleanupWithTimeout(jobID, "agent resource teardown", timeoutSeconds, func() error {
		if a != nil {
			if err := a.Close(); err != nil {
				s.log.Debug(fmt.Sprintf("Job '%s': failed to close agent resources: %v", jobID, err))
			}
		}
		if err := cleanupStaleAsyncClients(); err != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': failed to reap stale auxiliary clients: %v", jobID, err))
		}
		return nil
	})
}

// Settable event the fire claim heartbeat latches when ownership is lost
type latchEvent struct {
	mu     sync.Mutex
	set    bool
	reason string
}

func (e *latchEvent) IsSet() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set
}

func (e *latchEvent) Set() {
	e.mu.Lock()
	e.set = true
	e.mu.Unlock()
}

func (e *latchEvent) Reason() string { return e.reason }

// Runs the body while a goroutine keeps the job's owned fire claim fresh, latching the event on loss
func (s *Store) runWithFireClaimHeartbeat(ctx context.Context, job *Job, run func(lost CancelSource) (bool, error)) (bool, error) {
	owner := ""
	if job.FireClaim != nil {
		owner = job.FireClaim.By
	}
	if owner == "" {
		return run(nil)
	}
	jobID := job.ID
	finishUnstarted := func(errText string) {
		if job.ExecutionID == nil || *job.ExecutionID == "" {
			return
		}
		if _, err := s.finishLostExecution(ctx, *job.ExecutionID, errText); err != nil {
			s.log.Warn(fmt.Sprintf("Job '%s': failed to close unstarted execution ledger row", jobID), "error", err)
		}
	}
	owns, err := s.heartbeatFireClaim(home.WithOverride(ctx, s.home), jobID, owner)
	if err != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': initial fire_claim validation failed", jobID), "error", err)
		finishUnstarted("Fire claim ownership could not be validated before execution started.")
		return true, nil
	}
	if !owns {
		s.log.Warn(fmt.Sprintf("Job '%s': fire claim ownership was already lost before execution", jobID))
		finishUnstarted("Fire claim ownership lost before execution started.")
		return true, nil
	}
	stop := make(chan struct{})
	lost := &latchEvent{}
	wait := func(seconds float64) bool {
		select {
		case <-stop:
			return true
		case <-time.After(time.Duration(seconds * float64(time.Second))):
			return false
		}
	}
	finished := startHeartbeatThread(func() {
		lastConfirmed := monoNow()
		for !wait(runClaimHeartbeatSeconds) {
			ok, herr := s.heartbeatFireClaim(home.WithOverride(ctx, s.home), jobID, owner)
			if herr == nil && !ok {
				if allowed, _ := s.selfRemovalDeliveryAllowed(ctx, nil, jobID); allowed {
					continue
				}
				if wait(fireClaimMissConfirmSeconds) {
					lastConfirmed = monoNow()
					continue
				}
				if again, aerr := s.heartbeatFireClaim(home.WithOverride(ctx, s.home), jobID, owner); aerr == nil && again {
					lastConfirmed = monoNow()
					continue
				} else if aerr != nil {
					herr = aerr
				} else {
					lost.Set()
					s.log.Warn(fmt.Sprintf("Job '%s': fire claim ownership lost; interrupting stale run", jobID))
					return
				}
			}
			if herr != nil {
				s.log.Debug(fmt.Sprintf("Job '%s': fire_claim heartbeat failed", jobID), "error", herr)
				if monoNow()-lastConfirmed >= fireClaimHeartbeatGraceSecs {
					lost.Set()
					s.log.Warn(fmt.Sprintf("Job '%s': fire_claim could not be renewed within %.1fs; interrupting uncertain run", jobID, fireClaimHeartbeatGraceSecs))
					return
				}
				continue
			}
			lastConfirmed = monoNow()
		}
	})
	defer func() {
		close(stop)
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	}()
	return run(lost)
}

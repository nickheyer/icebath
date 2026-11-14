package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Onboarding always runs in the default profile, whichever profile this backend was launched under
var onboardingProfile = map[string]any{"profile": "default"}

// Held through the opening submit so two windows pressing start at once get one chat and one opening
var onboardingStartMu sync.Mutex

// Whether this process already took its first local Desktop connection
var (
	localDesktopBootMu      sync.Mutex
	localDesktopBootStarted bool
)

// The claim was blocked and nothing else can carry the run's first chat
type noModelForTheRun struct{ message string }

func (e *noModelForTheRun) Error() string { return e.message }

var onboardingRegistry = &handlerRegistry{}

func init() {
	onboardingRegistry.method("onboarding.state", onboardingState)
	onboardingRegistry.method("onboarding.set_run", onboardingSetRun)
	onboardingRegistry.method("onboarding.start", onboardingStart)
	onboardingRegistry.method("onboarding.skip", onboardingSkip)
	onboardingRegistry.install()
	tools.OnOnboardingEnded(func(ended map[string]any) { tuigateway.BroadcastGlobalEvent("onboarding.ended", ended) })
}

// Whether onboarding is due and the chat in progress, read from the root profile's config
func onboardingState(ctx context.Context, rid any, params map[string]any) map[string]any {
	return ok(rid, onboardingRunState(ctx, truthy(params["assume_fresh"])))
}

// Writes onboarding.run in the root config and answers the new state
func onboardingSetRun(ctx context.Context, rid any, params map[string]any) map[string]any {
	run, isBool := params["run"].(bool)
	if !isBool {
		return rpcErr(rid, 4002, "onboarding.set_run requires a boolean 'run'", nil)
	}
	if err := tools.SetOnboardingRun(ctx, run, truthy(params["mark_profile_offered"])); err != nil {
		logger().Error("onboarding.set_run failed", "error", err)
		return rpcErr(rid, 5077, err.Error(), nil)
	}
	return ok(rid, onboardingRunState(ctx, false))
}

// Due answer merged with the run in progress, a remote backend answering at once and never eligible
func onboardingRunState(ctx context.Context, assumeFresh bool) map[string]any {
	out := map[string]any{}
	if !tools.ServesLocalDesktopNow(ctx) {
		out["run"], out["eligible"] = tools.OnboardingShouldRun(ctx, assumeFresh), false
	} else {
		var due tools.OnboardingDue
		if assumeFresh {
			due = onboardingDueNow(ctx, true)
		} else {
			due = tools.OnboardingDueRead(func() tools.OnboardingDue { return onboardingDueNow(ctx, false) }, onboardingDueStamp(ctx), tools.OnboardingBootWait)
		}
		for key, value := range due {
			out[key] = value
		}
	}
	for key, value := range onboardingActiveRun(ctx) {
		out[key] = value
	}
	return out
}

// Whether onboarding should run and whether the default profile can carry its first chat
func onboardingDueNow(ctx context.Context, assumeFresh bool) tools.OnboardingDue {
	defaultHome, err := profileHome("default")
	if err != nil {
		panic(err)
	}
	eligible, err := provider.WelcomeInferenceAvailable(scopedPoolEnv(profileRuntimeScope(ctx, defaultHome, true)), assumeFresh)
	if err != nil {
		panic(err)
	}
	return tools.OnboardingDue{"run": tools.OnboardingShouldRun(ctx, assumeFresh), "eligible": eligible}
}

// Fingerprint of every file the due answer reads, the root and default configs, credentials, shared store and managed layer
func onboardingDueStamp(ctx context.Context) tools.OnboardingStamp {
	return func() string {
		defaultHome, err := profileHome("default")
		if err != nil {
			panic(err)
		}
		if defaultHome == "" {
			defaultHome = home.Root()
		}
		paths := []string{filepath.Join(home.Root(), "config.yaml"), filepath.Join(defaultHome, "config.yaml"),
			filepath.Join(defaultHome, ".env"), filepath.Join(defaultHome, "auth.json"), provider.NousSharedStorePath(scopedPoolEnv(ctx))}
		if managed := config.ManagedDir(); managed != "" {
			paths = append(paths, filepath.Join(managed, "config.yaml"), filepath.Join(managed, ".env"))
		}
		parts := make([]string, len(paths))
		for i, path := range paths {
			version, err := config.FileCacheVersion(path, true)
			if err != nil {
				parts[i] = "None"
				continue
			}
			parts[i] = pyrepr.Repr(version)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
}

// Whether a raised failure is a config backend outage, the SystemExit no handler catches
func configOutage(failure any) bool {
	err, isError := failure.(error)
	if !isError {
		return false
	}
	var backendErr *config.ConfigBackendUnavailable
	var sourceErr *provider.ConfigBackendUnavailableError
	return errors.As(err, &backendErr) || errors.As(err, &sourceErr)
}

// Runs fn on a goroutine carrying ctx, a raised failure logged instead of ending the process
func spawnContextThread(ctx context.Context, name string, fn func(ctx context.Context)) {
	detached := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				logger().Error(fmt.Sprintf("Exception in thread %s", name), "error", failure)
			}
		}()
		fn(detached)
	}()
}

// Works out the local Desktop's due answer off the RPC path once the setup bootstrap recorded the install
func startDueBoot(ctx context.Context) {
	finish := tools.OnboardingBeginBoot(onboardingDueStamp(ctx))
	spawnContextThread(ctx, "onboarding-due", func(threadCtx context.Context) {
		finish(func() tools.OnboardingDue {
			provider.WaitForSetupRecord(scopedPoolEnv(threadCtx), provider.SetupReadyWait)
			return onboardingDueNow(threadCtx, false)
		})
	})
}

// Records a fresh install and settles the setup record and due answer once per process for the local Desktop
func beginLocalDesktopBoot(ctx context.Context) {
	localDesktopBootMu.Lock()
	first := !localDesktopBootStarted
	localDesktopBootStarted = true
	localDesktopBootMu.Unlock()
	scoped := tools.WithServingLocalDesktop(provider.WithLaunchProfileScopeIfMultiplexed(ctx), true)
	if !first {
		spawnContextThread(scoped, "free-tier-resettle", func(threadCtx context.Context) {
			provider.ResettleAfterFailedBoot(scopedPoolEnv(threadCtx))
		})
		return
	}
	provider.StartBackgroundBootstrap(scopedPoolEnv(context.WithoutCancel(scoped)))
	startDueBoot(scoped)
}

// Onboarding chat in progress and its run id, both nil when no run is open
func onboardingActiveRun(ctx context.Context) map[string]any {
	db, release, err := profileDB(ctx, onboardingProfile, false)
	if err != nil {
		panic(err)
	}
	defer release()
	var sessionID, runID any
	if db != nil {
		if active := tools.ActiveOnboardingSessionID(db); active != "" {
			sessionID = active
			if record := tools.LoadOnboardingRecord(db, active); record != nil {
				runID = record["run_id"]
			}
		}
	}
	return map[string]any{"session_id": sessionID, "run_id": runID}
}

// Claims the welcome trial with onboarding.run already on disk, refusing when nothing can carry the first chat
func claimBeforeRun(ctx context.Context, scopeHome string, assumeFresh bool) (map[string]any, error) {
	if !tools.ServesLocalDesktopNow(ctx) {
		return nil, &noModelForTheRun{"First-run onboarding starts only in the Desktop app on this machine."}
	}
	if err := tools.SetOnboardingRun(ctx, true, false); err != nil {
		return nil, err
	}
	env := scopedPoolEnv(profileRuntimeScope(ctx, scopeHome, true))
	claimed, err := provider.ClaimDesktopFreeTier(env, assumeFresh, false)
	if err != nil {
		return nil, err
	}
	if claimed == nil && !provider.FreeInferenceActive(env) {
		configured, err := provider.WelcomeProviderConfigured(env)
		if err != nil {
			return nil, err
		}
		if !configured {
			return nil, &noModelForTheRun{"The welcome trial is not available here, and no provider is set up."}
		}
	}
	return claimed, nil
}

// Chosen chat of one onboarding.start with what the opening and the response need
type onboardingStartPlan struct {
	stored   string
	created  bool
	runID    string
	replaced []string
	opening  *string
	claimed  map[string]any
}

// Opens or resumes the one onboarding chat of the default profile and submits its opening when it has none
func onboardingStart(ctx context.Context, rid any, params map[string]any) map[string]any {
	freshLaunch := truthy(params["fresh_launch"])
	onboardingStartMu.Lock()
	defer onboardingStartMu.Unlock()
	plan, refusal, err := planOnboardingStart(ctx, rid, params, freshLaunch)
	var authErr *provider.AuthError
	var noModel *noModelForTheRun
	switch {
	case errors.As(err, &authErr):
		logger().Info(fmt.Sprintf("onboarding.start: welcome trial not claimed: %s", err))
		return rpcErr(rid, 5079, err.Error(), nil)
	case errors.As(err, &noModel):
		logger().Info(fmt.Sprintf("onboarding.start: no chat opened: %s", err))
		return rpcErr(rid, 5080, err.Error(), nil)
	case err != nil:
		logger().Error("onboarding.start failed", "error", err)
		return rpcErr(rid, 5078, err.Error(), nil)
	case refusal != nil:
		return refusal
	}
	if plan.claimed != nil {
		provider.RefreshSetupRecord(scopedPoolEnv(ctx), true)
	}
	if freshLaunch {
		withdrawSetupCards(ctx, nil)
	} else {
		withdrawSetupCards(ctx, plan.replaced)
	}
	resumed := lookupMethod("session.resume")(ctx, rid, map[string]any{"profile": "default", "session_id": plan.stored, "source": "desktop"})
	if _, failed := resumed["error"]; failed {
		return resumed
	}
	result, _ := resumed["result"].(map[string]any)
	sid := result["session_id"]
	if plan.opening != nil {
		submitted := lookupMethod("prompt.submit")(ctx, rid, map[string]any{"session_id": sid, "text": *plan.opening, "display_kind": "hidden"})
		if _, failed := submitted["error"]; failed {
			return submitted
		}
	}
	return ok(rid, map[string]any{"session_id": sid, "stored_session_id": plan.stored, "resumed": !plan.created, "run_id": plan.runID})
}

// Picks or creates the run's chat in the default profile store, a raised failure returned as an error
func planOnboardingStart(ctx context.Context, rid any, params map[string]any, freshLaunch bool) (plan *onboardingStartPlan, refusal map[string]any, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			if raised, isError := failure.(error); isError {
				err = raised
				return
			}
			err = fmt.Errorf("%v", failure)
		}
	}()
	db, release, err := profileDB(ctx, onboardingProfile, true)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	if db == nil {
		return nil, dbUnavailableError(ctx, rid, 5000), nil
	}
	plan = &onboardingStartPlan{}
	openIDs, err := tools.OpenOnboardingSessionIDsErr(db)
	if err != nil {
		return nil, nil, err
	}
	if !truthy(params["rerun"]) && !freshLaunch && len(openIDs) > 0 {
		plan.stored = openIDs[0]
	}
	plan.created = plan.stored == ""
	var facts tools.OnboardingFacts
	if plan.created {
		defaultHome, err := profileHome("default")
		if err != nil {
			return nil, nil, err
		}
		scoped := profileRuntimeScope(ctx, defaultHome, false)
		source := "local"
		if truthy(params["facts_source"]) {
			source = pyrepr.Str(params["facts_source"])
		}
		locale, _ := params["locale"].(string)
		facts = tools.ReadOnboardingFacts(&tools.Env{Home: home.Dir(scoped), TerminalScope: tools.GetTerminalScope(scoped)}, source, locale)
		if !truthy(params["rerun"]) {
			if plan.claimed, err = claimBeforeRun(ctx, defaultHome, truthy(params["assume_fresh"])); err != nil {
				return nil, nil, err
			}
		}
		plan.stored = state.NewSessionID()
		if err := db.CreateSession(state.NewSession{ID: plan.stored, Source: "desktop"}); err != nil {
			return nil, nil, err
		}
		if _, err := tools.StartOnboarding(ctx, db, plan.stored, facts); err != nil {
			return nil, nil, err
		}
	} else {
		record := tools.LoadOnboardingRecord(db, plan.stored)
		if record == nil {
			return nil, nil, errors.New("'NoneType' object is not subscriptable")
		}
		stored, _ := record["facts"].(map[string]any)
		facts = tools.OnboardingFactsFromStored(stored)
	}
	record := tools.LoadOnboardingRecord(db, plan.stored)
	if record == nil {
		return nil, nil, errors.New("'NoneType' object has no attribute 'get'")
	}
	if truthy(record["run_id"]) {
		plan.runID = pyrepr.Str(record["run_id"])
	}
	for _, sid := range openIDs {
		if sid != plan.stored {
			plan.replaced = append(plan.replaced, sid)
		}
	}
	if freshLaunch {
		if err := discardUnfinishedRuns(ctx, db, plan.replaced); err != nil {
			return nil, nil, err
		}
	} else {
		for _, old := range plan.replaced {
			if _, err := tools.AdvanceOnboarding(ctx, db, old, tools.OnbEnd{Outcome: "replaced"}); err != nil {
				return nil, nil, err
			}
		}
	}
	if plan.created {
		title, err := db.GetNextTitleInLineage(ctx, tools.OnboardingChatTitle)
		if err != nil {
			return nil, nil, err
		}
		if _, err := db.SetSessionTitle(ctx, plan.stored, title); err != nil {
			return nil, nil, err
		}
	}
	row, err := db.GetSession(ctx, plan.stored)
	if err != nil {
		return nil, nil, err
	}
	if row == nil || !truthy(row.GetOr("message_count", nil)) {
		opening := tools.OnboardingOpeningPrompt(facts)
		plan.opening = &opening
	}
	return plan, nil, nil
}

// Ends the chat's run as skipped and closes the waiting card, skipped true only when every beat was skipped
func onboardingSkip(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	stored := strOr(session.Get("session_key"))
	if a := sessionAgent(session); a != nil && a.SessionID() != "" {
		stored = a.SessionID()
	}
	db, release := sessionDB(ctx, session)
	var step tools.OnboardingStep
	skipped, err := func() (bool, error) {
		defer release()
		var store tools.OnboardingStore
		if db != nil {
			store = db
		}
		if tools.LoadOnboardingRecord(store, stored) == nil {
			return false, nil
		}
		var err error
		step, err = tools.AdvanceOnboarding(ctx, store, stored, tools.OnbEnd{Outcome: "skipped"})
		return err == nil, err
	}()
	if err != nil {
		panic(err)
	}
	if !skipped {
		return ok(rid, map[string]any{"skipped": false})
	}
	cancelSessionRequests(ctx, strOr(params["session_id"]), "resolved", "setup_beat")
	return ok(rid, map[string]any{"skipped": truthy(step.Result["skipped_all"])})
}

// Live sessions here that show one of the stored ids
func liveStoredSessionIDs(storedIDs []string) []string {
	if len(storedIDs) == 0 {
		return nil
	}
	var out []string
	for _, live := range tuigateway.LiveSessions() {
		shown := strOr(live.Session.Get("session_key"))
		if a := sessionAgent(live.Session); a != nil && a.SessionID() != "" {
			shown = a.SessionID()
		}
		if slices.Contains(storedIDs, shown) {
			out = append(out, live.SID)
		}
	}
	return out
}

// Closes the card still waiting in each live chat whose run just ended
func withdrawSetupCards(ctx context.Context, storedIDs []string) {
	for _, sid := range liveStoredSessionIDs(storedIDs) {
		cancelSessionRequests(ctx, sid, "resolved", "setup_beat")
	}
}

// Closes and deletes the chats a new launch abandons, ending a run as replaced when its chat cannot be deleted
func discardUnfinishedRuns(ctx context.Context, db *state.DB, storedIDs []string) error {
	for _, sid := range liveStoredSessionIDs(storedIDs) {
		closeSessionByID(ctx, sid, "onboarding_restart", nil)
	}
	defaultHome, err := profileHome("default")
	if err != nil {
		return err
	}
	if defaultHome == "" {
		defaultHome = home.Dir(ctx)
	}
	sessionsDir := filepath.Join(defaultHome, "sessions")
	for _, old := range storedIDs {
		var deleteErr error
		deleted, err := tools.DeleteOnboardingChat(ctx, db, old, func() bool {
			removed, err := db.DeleteSession(ctx, old, state.DeleteSessionOptions{SessionsDir: sessionsDir, ExcludeActiveWriteGuards: true})
			deleteErr = err
			return err == nil && removed
		})
		if err == nil {
			err = deleteErr
		}
		if err != nil {
			logger().Error(fmt.Sprintf("onboarding.start could not delete unfinished chat %s", old), "error", err)
			deleted = false
		}
		if !deleted {
			if _, err := tools.AdvanceOnboarding(ctx, db, old, tools.OnbEnd{Outcome: "replaced"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Setup beat card server request answered by the renderer, nil when nobody answered in time
func setupBeatRequest(ctx context.Context, sid string, payload map[string]any) *pyjson.Object {
	timeout := float64(tools.OnboardingCardTimeout)
	return sendResult(ctx, tuigateway.Send)("setup_beat", sid, objectOf(copyDict(payload)), &timeout, nil)
}

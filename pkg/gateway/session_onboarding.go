package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Profile whose authority owns the onboarding chat
const onboardingProfile = "default"

// Refusal on the sidecar handler's wire, a store refusal carrying its JSON-RPC code and human message
type OnboardingRefusal struct {
	*state.RuntimeStoreError
	code    int
	message string
}

func (r *OnboardingRefusal) WireCode() int       { return r.code }
func (r *OnboardingRefusal) WireMessage() string { return r.message }
func (r *OnboardingRefusal) Unwrap() error       { return r.RuntimeStoreError }

// Store refusal onboarding_refused with the wire code and message, chained to the error that raised it
func onboardingRefusal(code int, message string, cause error) *OnboardingRefusal {
	refusal := state.NewRuntimeStoreError("onboarding_refused", nil, &message)
	refusal.Cause = cause
	return &OnboardingRefusal{RuntimeStoreError: refusal, code: code, message: message}
}

// The claim was blocked and nothing else can carry the run's first chat
type noModelForTheRun struct{ message string }

func (e *noModelForTheRun) Error() string { return e.message }

// What a start found and decided before any write
type onboardingPlan struct {
	stored   string
	claimed  map[string]any
	facts    tools.OnboardingFacts
	opening  *string
	replaced []string
}

// Verbs onboarding.start and onboarding.skip bound to the connection
func onboardingHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"onboarding.start": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return onboardingStart(ctx, c, ref, params)
		},
		"onboarding.skip": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return onboardingSkip(ctx, c, ref, params)
		},
	}
}

// Connection of the default profile's authority, this connection when it is that authority
func onboardingConnection(ctx context.Context, c *AuthorityConnection) (*AuthorityConnection, error) {
	sibling, err := c.siblingFor(ctx, onboardingProfile)
	if err != nil || sibling == nil {
		return c, err
	}
	return sibling, nil
}

// Validated contract params, a refusal invalid_params with the validation text otherwise
func onboardingValidate(model *pydantic.Model, params *pyjson.Object) (*pydantic.Instance, error) {
	request, invalid := model.Validate(pyjson.Plain(params))
	if invalid != nil {
		detail := invalid.Error()
		refusal := state.NewRuntimeStoreError("invalid_params", nil, &detail)
		refusal.Cause = invalid
		return nil, refusal
	}
	return request, nil
}

// Opens the onboarding chat in the default profile, writes its run record and admits the hidden opening turn
func onboardingStart(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	request, err := onboardingValidate(tuigateway.OnboardingStartParams, params)
	if err != nil {
		return nil, err
	}
	target, err := onboardingConnection(ctx, c)
	if err != nil {
		return nil, err
	}
	if target != c {
		return onboardingStart(ownerScope(ctx, target.authority, false), target, ref, params)
	}
	if !c.actor.Capabilities["session:create"] {
		return nil, runtimeRefusal("permission_denied")
	}
	a := c.authority
	if err := a.onboardingStartLock.Lock(ctx); err != nil {
		return nil, err
	}
	defer a.onboardingStartLock.Unlock()
	plan, err := onboardingChooseRun(ctx, a, request)
	if err != nil {
		return nil, err
	}
	stored := plan.stored
	if stored == "" {
		if stored, err = onboardingCreateChat(ctx, c); err != nil {
			return nil, err
		}
	}
	discards := onboardingDiscards(a)
	if request.Get("fresh_launch") == true {
		var unwatched []string
		for _, sid := range plan.replaced {
			if !hasViewers(a, sid) {
				unwatched = append(unwatched, sid)
			}
		}
		discards.idsMu.Lock()
		for _, sid := range unwatched {
			discards.ids[sid] = true
		}
		discards.idsMu.Unlock()
	}
	written, err := func() (any, error) {
		defer func() {
			discards.idsMu.Lock()
			for _, sid := range plan.replaced {
				delete(discards.ids, sid)
			}
			discards.idsMu.Unlock()
		}()
		return trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
			return onboardingSettleRuns(taskCtx, a, request, plan, stored, discards)
		}, func(taskCtx context.Context, result any) (any, error) {
			deleted, _ := result.([]string)
			if err := retireLiveSessions(taskCtx, a, deleted, false); err != nil {
				return nil, err
			}
			return deleted, nil
		}, false, nil)
	}()
	if err != nil {
		return nil, err
	}
	deleted, _ := written.([]string)
	if plan.claimed != nil {
		provider.RefreshSetupRecord(onboardingPoolEnv(ctx), true)
	}
	var kept []string
	for _, sid := range plan.replaced {
		if !slices.Contains(deleted, sid) {
			kept = append(kept, sid)
		}
	}
	onboardingWithdrawSetupCards(a, kept)
	chat := SessionRef{ProfileID: a.ProfileID, SessionID: stored}
	if _, err := c.resume(ctx, chat, pyjson.NewObject()); err != nil {
		return nil, err
	}
	opening := plan.opening
	if plan.stored == "" {
		if opening, err = onboardingOpeningFor(ctx, a, stored); err != nil {
			return nil, err
		}
	}
	if opening != nil {
		runID := onboardingRunID(a, stored)
		submission := runID
		if submission == "" {
			submission = stored
		}
		if _, err := c.submit(ctx, chat, pyjson.ObjectOf("session_id", stored, "text", *opening,
			"display_kind", "hidden", "submission_id", "onboarding-opening:"+submission)); err != nil {
			return nil, err
		}
	}
	return pyjson.ObjectOf("session_id", stored, "stored_session_id", stored, "resumed", plan.stored != "",
		"run_id", onboardingRunID(a, stored)), nil
}

// Credential environment of the scoped profile home for the welcome trial and setup record
func onboardingPoolEnv(ctx context.Context) provider.PoolEnv {
	env := provider.HomePoolEnv(home.Dir(ctx))
	env.Scope, env.Ctx = provider.SecretScopeFrom(ctx), ctx
	return env
}

// Facts of a stored run record, a TypeError when the chat holds no record
func onboardingStoredFacts(a *SessionAuthority, sessionID string) (tools.OnboardingFacts, error) {
	record := tools.LoadOnboardingRecord(a.DB, sessionID)
	if record == nil {
		return tools.OnboardingFacts{}, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
	}
	stored, _ := record["facts"].(map[string]any)
	return tools.OnboardingFactsFromStored(stored), nil
}

// Whether the stored session row already holds messages
func onboardingHasMessages(ctx context.Context, a *SessionAuthority, sessionID string) (bool, error) {
	row, err := a.DB.GetSession(ctx, sessionID)
	if err != nil || row == nil {
		return false, err
	}
	return pyTruthy(row.GetOr("message_count", nil)), nil
}

// Picks the open run to resume or measures facts and claims the trial, refusing 5079 or 5080 when no chat can open
func onboardingChooseRun(ctx context.Context, a *SessionAuthority, request *pydantic.Instance) (*onboardingPlan, error) {
	openIDs, err := tools.OpenOnboardingSessionIDsErr(a.DB)
	if err != nil {
		return nil, err
	}
	rerun := request.Get("rerun") == true
	plan := &onboardingPlan{}
	if !rerun && request.Get("fresh_launch") != true && len(openIDs) > 0 {
		plan.stored = openIDs[0]
	}
	for _, sid := range openIDs {
		if sid != plan.stored {
			plan.replaced = append(plan.replaced, sid)
		}
	}
	if plan.stored != "" {
		facts, err := onboardingStoredFacts(a, plan.stored)
		if err != nil {
			return nil, err
		}
		hasMessages, err := onboardingHasMessages(ctx, a, plan.stored)
		if err != nil {
			return nil, err
		}
		if !hasMessages {
			opening := tools.OnboardingOpeningPrompt(facts)
			plan.opening = &opening
		}
		plan.facts = facts
		return plan, nil
	}
	source, _ := request.Get("facts_source").(string)
	locale, _ := request.Get("locale").(string)
	plan.facts = tools.ReadOnboardingFacts(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, source, locale)
	if !rerun {
		claimed, err := onboardingClaimBeforeRun(ctx, request.Get("assume_fresh") == true)
		var authErr *provider.AuthError
		var noModel *noModelForTheRun
		switch {
		case errors.As(err, &authErr):
			slog.Info(fmt.Sprintf("onboarding.start: welcome trial not claimed: %s", err))
			return nil, onboardingRefusal(5079, err.Error(), err)
		case errors.As(err, &noModel):
			slog.Info(fmt.Sprintf("onboarding.start: no chat opened: %s", err))
			return nil, onboardingRefusal(5080, err.Error(), err)
		case err != nil:
			return nil, err
		}
		plan.claimed = claimed
	}
	return plan, nil
}

// Writes onboarding.run true before the welcome trial claim so a crash after the claim still opens onboarding
func onboardingClaimBeforeRun(ctx context.Context, assumeFresh bool) (map[string]any, error) {
	if !tools.ServesLocalDesktopNow(ctx) {
		return nil, &noModelForTheRun{"First-run onboarding starts only in the Desktop app on this machine."}
	}
	if err := tools.SetOnboardingRun(ctx, true, false); err != nil {
		return nil, err
	}
	env := onboardingPoolEnv(ctx)
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

// Stages and commits a fresh local GUI chat, refusing 5080 when its policy carries no model
func onboardingCreateChat(ctx context.Context, c *AuthorityConnection) (string, error) {
	a := c.authority
	desktopLocal := false
	if ws, ok := c.transport.(*tuigateway.WSTransport); ok && ws != nil {
		desktopLocal = ws.DesktopLocal
	}
	existing, staged, err := stageLocalSession(ctx, a, c.actor, map[string]any{"source": "gui", "request_id": newRequestID()},
		localCreateOptions{ClientLocal: c.nativeOwner, DesktopLocal: desktopLocal})
	if err != nil {
		return "", err
	}
	if staged == nil {
		return existing.SessionID, nil
	}
	rawPolicy, err := receiptField(staged.Receipt, "policy")
	if err != nil {
		return "", err
	}
	var model any
	if policy, ok := rawPolicy.(*pyjson.Object); ok && policy != nil {
		model = policy.GetOr("model", nil)
	}
	if !pyTruthy(model) {
		return "", onboardingRefusal(5080, "No model is set up to run onboarding on.", nil)
	}
	receipt := *staged
	if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return a.DB.CommitLocalSession(taskCtx, a.Epoch, receipt)
	}, nil, false, nil); err != nil {
		return "", err
	}
	sid, err := receiptField(receipt.Receipt, "session_id")
	if err != nil {
		return "", err
	}
	restored, err := restoreLocalSession(ctx, a, pyStrOf(sid))
	if err != nil {
		return "", err
	}
	return restored.SessionID, nil
}

// Discard set of the authority, created on first use
func onboardingDiscards(a *SessionAuthority) *onboardingDiscardSet {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.onboardingDiscards == nil {
		a.onboardingDiscards = &onboardingDiscardSet{ids: map[string]bool{}}
	}
	return a.onboardingDiscards
}

// Writes the new chat's record and title and ends every other open run, returning the chats it deleted
func onboardingSettleRuns(ctx context.Context, a *SessionAuthority, request *pydantic.Instance, plan *onboardingPlan,
	stored string, discards *onboardingDiscardSet) ([]string, error) {
	db := a.DB
	if plan.stored == "" {
		if _, err := tools.StartOnboarding(ctx, db, stored, plan.facts); err != nil {
			return nil, err
		}
	}
	deleted := []string{}
	if request.Get("fresh_launch") == true {
		deleted = onboardingDiscardUnfinishedRuns(ctx, db, plan.replaced, discards)
	}
	for _, old := range plan.replaced {
		if slices.Contains(deleted, old) {
			continue
		}
		if _, err := tools.AdvanceOnboarding(ctx, db, old, tools.OnbEnd{Outcome: "replaced"}); err != nil {
			return nil, err
		}
	}
	if plan.stored == "" {
		title, err := db.GetNextTitleInLineage(ctx, tools.OnboardingChatTitle)
		if err != nil {
			return nil, err
		}
		if _, err := db.SetSessionTitle(ctx, stored, title); err != nil {
			return nil, err
		}
	}
	return deleted, nil
}

// Deletes the unwatched chats still marked for discard the way session.delete does, keeping any a window reopened
func onboardingDiscardUnfinishedRuns(ctx context.Context, db *state.SessionDB, storedIDs []string, discards *onboardingDiscardSet) []string {
	sessionsDir := filepath.Join(filepath.Dir(db.Path()), "sessions")
	deleted := []string{}
	for _, old := range storedIDs {
		func() {
			discards.lock.Lock()
			defer discards.lock.Unlock()
			if !discards.holds(old) {
				return
			}
			var deleteErr error
			removed, err := tools.DeleteOnboardingChat(ctx, db, old, func() bool {
				ok, err := db.DeleteSession(ctx, old, state.DeleteSessionOptions{SessionsDir: sessionsDir, ExcludeActiveWriteGuards: true})
				deleteErr = err
				return err == nil && ok
			})
			if err == nil {
				err = deleteErr
			}
			if err != nil {
				slog.Error(fmt.Sprintf("onboarding.start could not delete unfinished chat %s", old), "error", err)
				return
			}
			if removed {
				deleted = append(deleted, old)
			}
		}()
	}
	return deleted
}

// Closes the setup card still waiting in each chat whose run just ended
func onboardingWithdrawSetupCards(a *SessionAuthority, storedIDs []string) {
	for _, sid := range storedIDs {
		if live, ok := a.Live(sid); ok && live != nil {
			WithdrawServerRequests(live.EventStream, sid, "setup_beat")
		}
	}
}

// Opening prompt of a chat that holds no messages yet, nil once it has any
func onboardingOpeningFor(ctx context.Context, a *SessionAuthority, stored string) (*string, error) {
	hasMessages, err := onboardingHasMessages(ctx, a, stored)
	if err != nil || hasMessages {
		return nil, err
	}
	facts, err := onboardingStoredFacts(a, stored)
	if err != nil {
		return nil, err
	}
	opening := tools.OnboardingOpeningPrompt(facts)
	return &opening, nil
}

// Run id of the chat's record, empty when it holds none
func onboardingRunID(a *SessionAuthority, stored string) string {
	runID, _ := tools.LoadOnboardingRecord(a.DB, stored)["run_id"].(string)
	return runID
}

// Ends the chat's run as skipped and closes the waiting card, skipped true only when every beat was skipped
func onboardingSkip(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	request, err := onboardingValidate(tuigateway.OnboardingSkipParams, params)
	if err != nil {
		return nil, err
	}
	if !pyTruthy(request.Get("session_id")) {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	if err := a.Authorize(ctx, c.actor, ref, "session:control"); err != nil {
		return nil, err
	}
	tip, err := a.DB.GetCompressionTip(ctx, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if tip == "" {
		tip = ref.SessionID
	}
	if tools.LoadOnboardingRecord(a.DB, tip) == nil {
		return pyjson.ObjectOf("skipped", false), nil
	}
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return tools.AdvanceOnboarding(taskCtx, a.DB, ref.SessionID, tools.OnbEnd{Outcome: "skipped"})
	}, nil, false, nil)
	if err != nil {
		return nil, err
	}
	step, _ := result.(tools.OnboardingStep)
	if live, ok := a.Live(ref.SessionID); ok && live != nil {
		WithdrawServerRequests(live.EventStream, ref.SessionID, "setup_beat")
	}
	return pyjson.ObjectOf("skipped", pyTruthy(step.Result["skipped_all"])), nil
}

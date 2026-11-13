package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Slice of a deferred prompt's build wait, the slow notice threshold and its replace in place key
const (
	agentBuildWaitSlice        = 5.0
	agentBuildSlowNoticeAfter  = 30.0
	agentBuildSlowNoticeKey    = "agent-build-slow"
	agentBuildDefaultWaitCapSe = 600.0
)

// Liveness handle of a session's agent build goroutine
type agentBuildThread struct{ done chan struct{} }

// Whether the build goroutine is still running
func (t *agentBuildThread) isAlive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// Error response once the build failed or timed out, nil when the agent is ready
func waitAgent(ctx context.Context, session *tuigateway.Session, rid any, timeout float64) map[string]any {
	if ready, isEvent := session.Get("agent_ready").(*pythread.Event); isEvent && ready != nil && !ready.Wait(timeout) {
		return rpcErr(rid, 5032, agentStillStarting, nil)
	}
	if failure := session.Get("agent_error"); truthy(failure) {
		return rpcErr(rid, 5032, strOr(failure), nil)
	}
	return nil
}

// Seconds a submitted prompt waits for the deferred build, agent build_wait_timeout overriding 600
func agentBuildWaitCap(ctx context.Context) (limit float64) {
	limit = agentBuildDefaultWaitCapSe
	suppress(func() {
		agentCfg, _ := loadCfg(ctx)["agent"].(map[string]any)
		if raw := agentCfg["build_wait_timeout"]; raw != nil {
			if value, ok := pyFloatOf(raw); ok && value > 0 {
				limit = value
			}
		}
	})
	return limit
}

// float() of a config value, false when it does not parse
func pyFloatOf(v any) (float64, bool) {
	value, err := pyFloat(v)
	return value, err == nil
}

// Patient build wait of a deferred prompt, nil on success or cancel, an error once the build died
func waitAgentForPrompt(ctx context.Context, session *tuigateway.Session, rid any, sid string) map[string]any {
	ready, isEvent := session.Get("agent_ready").(*pythread.Event)
	if !isEvent || ready == nil {
		return nil
	}
	start, limit, notifiedSlow := pythread.Monotonic(), agentBuildWaitCap(ctx), false
	for !ready.Wait(agentBuildWaitSlice) {
		unlock := lockHistory(session)
		cancelled := truthy(session.Get("_turn_cancel_requested")) || !truthy(session.Get("running"))
		unlock()
		if cancelled {
			return nil
		}
		waited := pythread.Monotonic() - start
		if waited >= limit {
			return rpcErr(rid, 5032, fmt.Sprintf("agent initialization timed out after %ds — your message was not sent; retry once the session is ready", int(waited)), nil)
		}
		if build, ok := session.Get("_agent_build_thread").(*agentBuildThread); ok && build != nil && !build.isAlive() && !ready.IsSet() {
			message := strOr(session.Get("agent_error"))
			if message == "" {
				message = "agent initialization failed before completing"
			}
			return rpcErr(rid, 5032, message, nil)
		}
		if !notifiedSlow && waited >= agentBuildSlowNoticeAfter {
			notifiedSlow = true
			tuigateway.Emit(ctx, "notification.show", sid, map[string]any{
				"text":  "Still starting the agent (tool discovery / model setup) — your message will be sent as soon as it's ready.",
				"level": "info", "kind": "agent", "ttl_ms": nil, "key": agentBuildSlowNoticeKey, "id": agentBuildSlowNoticeKey})
		}
	}
	if notifiedSlow {
		tuigateway.Emit(ctx, "notification.clear", sid, map[string]any{"key": agentBuildSlowNoticeKey})
	}
	if failure := session.Get("agent_error"); truthy(failure) {
		return rpcErr(rid, 5032, strOr(failure), nil)
	}
	return nil
}

// Binds the home, secret and terminal scopes of a build profile, home and terminal alone when secrets fail
func bindBuildProfileScopes(ctx context.Context, profileHome string) (scoped context.Context) {
	defer func() {
		if recover() != nil {
			scoped = ctx
			if profileHome != "" {
				scoped = home.WithOverride(ctx, profileHome)
				func() {
					defer func() { _ = recover() }()
					scoped = tools.WithProfileTerminalScope(scoped, profileHome, nil)
				}()
			}
		}
	}()
	return profileRuntimeScope(ctx, profileHome, true)
}

// Agent construction arguments of a deferred first prompt build
type makeAgentOptions struct {
	sessionID                  string
	sessionDB                  *state.DB
	modelOverride              any
	providerOverride           string
	reasoningConfigOverride    map[string]any
	reasoningOverrideSet       bool
	serviceTierOverride        *string
	platformOverride           string
	contextCwdIsLaunchArtifact *bool
	cwdOverride                string
	authUserID                 *string
}

// makeAgent arguments of a deferred build, a stored runtime identity restored whole when its provider routes
func deferredBuildAgentKwargs(ctx context.Context, current *tuigateway.Session, sessionDB *state.DB) makeAgentOptions {
	launch := contextCwdIsLaunchArtifact(current)
	kw := makeAgentOptions{sessionDB: sessionDB, contextCwdIsLaunchArtifact: &launch, platformOverride: sessionSource(current),
		cwdOverride: sessionCwd(ctx, current)}
	if resumeSID := strOr(current.Get("resume_session_id")); resumeSID != "" {
		kw.sessionID = resumeSID
	}
	resume, _ := current.Get("resume_runtime_overrides").(map[string]any)
	if len(resume) > 0 && overridesHaveRoutableProvider(ctx, resume) {
		kw.applyOverrides(resume)
		return kw
	}
	if override := current.Get("model_override"); truthy(override) {
		kw.modelOverride = override
	}
	if reasoning, isDict := current.Get("create_reasoning_override").(map[string]any); isDict {
		kw.reasoningConfigOverride, kw.reasoningOverrideSet = reasoning, true
	}
	if tier, present := current.Lookup("create_service_tier_override"); present && tier != nil {
		text := strOr(tier)
		kw.serviceTierOverride = &text
	}
	return kw
}

// Splats a stored runtime override dict onto the build arguments
func (kw *makeAgentOptions) applyOverrides(overrides map[string]any) {
	for key, value := range overrides {
		switch key {
		case "model_override":
			kw.modelOverride = value
		case "provider_override":
			kw.providerOverride = strOr(value)
		case "reasoning_config_override":
			if value != nil {
				reasoning, _ := value.(map[string]any)
				kw.reasoningConfigOverride, kw.reasoningOverrideSet = reasoning, true
			}
		case "service_tier_override":
			if value != nil {
				text := strOr(value)
				kw.serviceTierOverride = &text
			}
		}
	}
}

// Post build wiring, true when the approval notify got registered
func wireSessionAgent(ctx context.Context, sid, key string, built any) (notifyRegistered bool) {
	func() {
		defer func() { _ = recover() }()
		tools.RegisterGatewayNotify(key, func(data map[string]any) error { emitApprovalRequest(ctx, sid, data); return nil })
		notifyRegistered = true
		tools.LoadPermanentAllowlist(tools.ToolEnvFrom(ctx))
	}()
	wireCallbacks(ctx, sid)
	if a, isAgent := built.(*agent.AIAgent); isAgent && a != nil {
		suppress(func() {
			a.SetBackgroundReviewCallback(func(message string) {
				tuigateway.Emit(ctx, "review.summary", sid, map[string]any{"text": message})
			})
			a.SetMemoryNotifications(loadMemoryNotifications(ctx))
		})
	}
	return notifyRegistered
}

// Starts exactly one notification poller for the session and fires the session reset boundary hook
func startSessionServices(ctx context.Context, sid, key string, current *tuigateway.Session) {
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		if rec := tuigateway.SessionRecord(sid); rec != nil {
			if old, isEvent := rec.Get("_notif_stop").(*pythread.Event); isEvent && old != nil {
				old.Set()
			}
			rec.Set("_notif_stop", startNotificationPoller(ctx, sid, rec))
		}
	}()
	notifySessionBoundary(ctx, "on_session_reset", &key, sessionSource(current))
}

// Blocks on a cold resume's transcript hydration, false when the record was replaced meanwhile
func awaitResumeHistory(sid string, current *tuigateway.Session) (bool, error) {
	historyReady, isEvent := current.Get("resume_history_ready").(*pythread.Event)
	if !isEvent || historyReady == nil {
		return true, nil
	}
	if !historyReady.Wait(300.0) {
		return false, errors.New("session history hydration timed out")
	}
	if failure := current.Get("resume_history_error"); truthy(failure) {
		return false, errors.New(strOr(failure))
	}
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	return tuigateway.SessionRecord(sid) == current, nil
}

// Attaches a freshly built agent to its live record, false when the record was popped mid build
func attachBuiltAgent(ctx context.Context, sid string, current *tuigateway.Session, built any) bool {
	a, isAgent := built.(*agent.AIAgent)
	if hint := strings.TrimSpace(strOr(current.Get("pending_title"))); hint != "" && isAgent && a != nil {
		a.SetSessionTitleHint(hint)
	}
	attached := func() bool {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		if tuigateway.SessionRecord(sid) != current {
			return false
		}
		current.Set("agent", built)
		return true
	}()
	if !attached {
		return false
	}
	registerSessionCwd(ctx, current)
	sessionTodoState(current)
	model, providerName := configModelTarget(ctx)
	current.Set("config_model_seen", []any{model, providerName})
	return true
}

// Tail of a build, credits seed, session services, session info and the late MCP catch up
func announceBuiltAgent(ctx context.Context, sid, key string, current *tuigateway.Session, built any) {
	a, _ := built.(*agent.AIAgent)
	if a != nil {
		suppress(func() { provider.SeedCreditsAtSessionStart(a.CreditsCapture()) })
	}
	startSessionServices(ctx, sid, key, current)
	info := sessionInfo(ctx, a, current)
	if warning := probeConfigHealth(loadCfg(ctx)); warning != "" {
		info["config_warning"] = warning
		logger().Warn(warning)
	}
	tuigateway.Emit(ctx, "session.info", sid, info)
	if a != nil {
		scheduleMCPLateRefresh(ctx, sid, a)
	}
}

// Settles the late notify registration and the dedicated db handle once a build ends
func finishAgentBuild(ctx context.Context, sid, key string, current *tuigateway.Session, notifyRegistered bool, sessionDB *state.DB) {
	sessionsLock.Lock()
	replaced := tuigateway.SessionRecord(sid) != current
	sessionsLock.Unlock()
	if replaced && notifyRegistered {
		suppress(func() { tools.UnregisterGatewayNotify(key) })
	}
	if sessionDB == nil {
		return
	}
	var holder *agent.AIAgent
	if !replaced {
		holder, _ = current.Get("agent").(*agent.AIAgent)
	}
	if !transferDBToAgent(ctx, holder, sessionDB) {
		suppress(func() { state.ReleaseOrClose(sessionDB) })
	}
}

// Starts building the real agent of a session once, in the background, the ready and error contract unchanged
func startAgentBuild(ctx context.Context, sid string, session *tuigateway.Session) {
	ready, isEvent := session.Get("agent_ready").(*pythread.Event)
	if !isEvent || ready == nil {
		return
	}
	if truthy(session.Get("lazy")) && childRunActive(strOr(session.Get("session_key")), session.Get("profile_home")) {
		return
	}
	lock := session.SetDefault("agent_build_lock", &sync.Mutex{}).(*sync.Mutex)
	lock.Lock()
	if ready.IsSet() || truthy(session.Get("agent_build_started")) {
		lock.Unlock()
		return
	}
	session.Set("agent_build_started", true)
	session.Pop("lazy")
	lock.Unlock()
	key := strOr(session.Get("session_key"))
	build := &agentBuildThread{done: make(chan struct{})}
	session.Set("_agent_build_thread", build)
	go func() {
		defer threadExceptHook("agent-build-" + sid)
		defer close(build.done)
		runAgentBuild(context.Background(), sid, key, session, ready)
	}()
}

// Body of the build goroutine, ready always set on the way out
func runAgentBuild(ctx context.Context, sid, key string, session *tuigateway.Session, ready *pythread.Event) {
	sessionsLock.Lock()
	current := tuigateway.SessionRecord(sid)
	sessionsLock.Unlock()
	if current == nil {
		session.Set("agent_error", agentBuildAbandoned)
		ready.Set()
		return
	}
	notifyRegistered := false
	var sessionDB *state.DB
	profileHome := strOr(current.Get("profile_home"))
	defer func() {
		finishAgentBuild(ctx, sid, key, current, notifyRegistered, sessionDB)
		ready.Set()
	}()
	failure := func() (failure any) {
		defer func() {
			if r := recover(); r != nil {
				failure = r
			}
		}()
		live, err := awaitResumeHistory(sid, current)
		if err != nil {
			return err
		}
		if !live {
			current.Set("agent_error", agentBuildAbandoned)
			return nil
		}
		cwd := sessionCwd(ctx, current)
		sessionCtx, env := setSessionContext(ctx, key, &cwd, "")
		scoped := bindBuildProfileScopes(sessionCtx, profileHome)
		if profileHome != "" {
			opened, err := openProfileSessionDB(scoped, profileHome)
			if err != nil {
				clearSessionContext(env)
				return err
			}
			sessionDB = opened
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger().Warn("MCP discovery startup failed", "error", r)
				}
			}()
			ensureMCPDiscoveryStarted(scoped)
		}()
		built, err := func() (any, error) {
			defer clearSessionContext(env)
			return makeAgent(scoped, sid, key, deferredBuildAgentKwargs(scoped, current, sessionDB))
		}()
		if err != nil {
			return err
		}
		if !attachBuiltAgent(scoped, sid, current, built) {
			current.Set("agent_error", agentBuildAbandoned)
			if closer, ok := built.(interface{ Close() error }); ok {
				suppress(func() { _ = closer.Close() })
			}
			return nil
		}
		notifyRegistered = wireSessionAgent(scoped, sid, key, built)
		announceBuiltAgent(scoped, sid, key, current, built)
		return nil
	}()
	if failure == nil {
		return
	}
	current.Set("agent_error", failureText(failure))
	payload := map[string]any{"message": agentInitFailedMessage(failure)}
	var notConfigured *provider.ProviderNotConfiguredError
	if err, isErr := failure.(error); isErr && errors.As(err, &notConfigured) {
		payload["code"] = "provider_not_configured"
	}
	tuigateway.Emit(ctx, "error", sid, payload)
}

// Live session of a session scoped RPC, the 4001 error when the runtime id is no longer held
func sessNowait(ctx context.Context, params map[string]any, rid any) (*tuigateway.Session, map[string]any) {
	sid := strOr(params["session_id"])
	if session := tuigateway.SessionRecord(sid); session != nil {
		return session, nil
	}
	method := currentRPCMethod(ctx)
	if method == "" {
		method = "?"
	}
	logger().Warn(fmt.Sprintf("session-scoped RPC rejected: method=%s session_id=%s not in memory "+
		"(detached/reaped runtime; client should resume the stored session), rid=%s", method, pyrepr.Repr(sid), pyrepr.Repr(rid)))
	return nil, rpcErr(rid, 4001, "session not found", nil)
}

// Live session of an RPC with its agent built, waiting on the build
func sess(ctx context.Context, params map[string]any, rid any) (*tuigateway.Session, map[string]any) {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return nil, errResp
	}
	return session, waitAgent(ctx, session, rid, 30.0)
}

// Live session of an attach RPC with its agent build warmed but not awaited
func sessBuilding(ctx context.Context, params map[string]any, rid any) (*tuigateway.Session, map[string]any) {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return nil, errResp
	}
	startAgentBuild(ctx, strOr(params["session_id"]), session)
	return session, nil
}

// Builds the agent of a session, the synthetic turn agent when the certify seam is armed
func makeAgent(ctx context.Context, sid, key string, o makeAgentOptions) (any, error) {
	sessionKey := o.sessionID
	if sessionKey == "" {
		sessionKey = key
	}
	if synthetic := maybeBuildSyntheticAgent(sessionKey, o.modelOverride); synthetic != nil {
		return synthetic, nil
	}
	suppress(func() { mcpStartupWait(ctx) })
	suppress(func() { waitForEntryMCPDiscovery(ctx, nil) })
	cfg := loadCfg(ctx)
	if _, err := plugins.RegisterShellHooksFromConfig(ctx, cfg, false, false); err != nil {
		return nil, err
	}
	systemPrompt, err := startupSystemPrompt(ctx, cfg, sessionKey)
	if err != nil {
		return nil, err
	}
	model, rt, fallbackNotice, err := resolveAgentModelRuntime(ctx, o.modelOverride, o.providerOverride)
	if err != nil {
		return nil, err
	}
	routing, _ := loadProviderRouting(ctx).(map[string]any)
	platform := resolveAgentPlatform(o.platformOverride)
	ignoreRules := provider.IsTruthyValue(os.Getenv("ICEBATH_IGNORE_RULES"))
	sessionsLock.Lock()
	session := tuigateway.SessionRecord(sid)
	sessionsLock.Unlock()
	db := o.sessionDB
	if db == nil {
		db = getDB(ctx)
	}
	reasoning := o.reasoningConfigOverride
	if !o.reasoningOverrideSet {
		reasoning = loadReasoningConfig(ctx, model)
	}
	tier := ""
	if o.serviceTierOverride != nil {
		tier = *o.serviceTierOverride
	} else {
		tier = loadServiceTier(ctx)
	}
	userID := ""
	if o.authUserID != nil {
		userID = *o.authUserID
	} else if session != nil {
		if owner := tuigateway.SessionAuthUserID(session); owner != nil {
			userID = *owner
		}
	}
	opts := agent.AIAgentOptions{
		Model: model, MaxIterations: new(cfgMaxTurns(cfg, 500)), Provider: rt.Provider, RequestedProvider: rt.RequestedProvider,
		BaseURL: rt.BaseURL, APIKey: rt.APIKey, APIMode: string(rt.APIMode), ACPCommand: rt.Command, ACPArgs: rt.Args,
		CredentialPool: rt.CredentialPool, Runtime: rt, QuietMode: true, VerboseLogging: false, ReasoningConfig: reasoning,
		ServiceTier: tier, EnabledToolsets: tools.OnboardingToolsets(ctx, loadEnabledToolsets(ctx, platform), db, sessionKey),
		DisabledToolsets: loadDisabledToolsets(ctx), ProvidersAllowed: routing["only"], ProvidersIgnored: routing["ignore"],
		ProvidersOrder: routing["order"], ProviderSort: routing["sort"], ProviderRequireParameters: truthy(routing["require_parameters"]),
		ProviderDataCollection: routing["data_collection"], Platform: platform, SessionID: sessionKey, Cwd: o.cwdOverride,
		UserID: userID, SessionDB: db, EphemeralSystemPrompt: systemPrompt, CheckpointsEnabled: resolveCheckpointsEnabled(cfg),
		PassSessionID: provider.IsTruthyValue(os.Getenv("ICEBATH_TUI_PASS_SESSION_ID")), SkipContextFiles: ignoreRules,
		SkipMemory: ignoreRules, FallbackModel: loadFallbackModel(ctx), RequestOverrides: rt.RequestOverrides,
		PrefillMessages: loadPrefillMessages(ctx)}
	if len(opts.PrefillMessages) == 0 {
		opts.PrefillMessages = nil
	}
	agentCbs(ctx, sid).applyTo(&opts)
	a, err := agent.NewAIAgent(ctx, opts)
	if err != nil {
		return nil, err
	}
	launch := false
	if o.contextCwdIsLaunchArtifact != nil {
		launch = *o.contextCwdIsLaunchArtifact
	} else {
		launch = contextCwdIsLaunchArtifact(session)
	}
	a.SetContextCwdIsLaunchArtifact(launch)
	if fallbackNotice != "" {
		a.SetPendingFallbackNotice([]string{fallbackNotice})
	}
	return a, nil
}

// Shared owner MCP discovery wait the dashboard socket and desktop surfaces start
func mcpStartupWait(ctx context.Context) {
	mcp.WaitForDiscovery(ctx, configEnv(ctx), nil, false)
}

// Registers a live session record around a built agent, then wires it, starts its services and announces it
func initSession(ctx context.Context, sid, key string, built any, history []any, cols int, cwd string, sessionDB *state.DB, source, profileHome string, explicitCwd bool) {
	now := nowSeconds()
	if cwd == "" {
		resolved, err := completionCwd(ctx, nil)
		if err != nil {
			panic(err)
		}
		cwd = resolved
	}
	transport := tuigateway.CurrentTransport(ctx)
	if transport == nil {
		transport = tuigateway.ModuleStdioTransport()
	}
	var authUserID any
	if owner := tuigateway.TransportAuthUserID(tuigateway.CurrentTransport(ctx)); owner != nil {
		authUserID = *owner
	}
	var profile any
	if profileHome != "" {
		profile = profileHome
	}
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		record := tuigateway.NewSession(map[string]any{
			"agent": built, "session_key": key, "history": history, "history_lock": &sync.Mutex{},
			"history_version": int64(0), "inflight_turn": nil, "created_at": now, "last_active": now,
			"running": false, "attached_images": []any{}, "image_counter": 0, "cwd": cwd,
			"explicit_cwd": explicitCwd, "cols": cols, "slash_worker": nil,
			"show_reasoning": loadShowReasoning(ctx), "source": resolveSessionSource(source),
			"tool_progress_mode": loadToolProgressMode(ctx), "edit_snapshots": map[string]any{}, "tool_started_at": map[string]any{},
			"profile_home": profile, "model_override": nil, "transport": transport, "auth_user_id": authUserID})
		tuigateway.SetSessionRecord(sid, record)
		sessionTodoState(record)
	}()
	hydrateSessionCwd(ctx, sid, key, sessionDB, profileHome)
	registerSessionCwd(ctx, tuigateway.SessionRecord(sid))
	wireSessionAgent(ctx, sid, key, built)
	current := tuigateway.SessionRecord(sid)
	if current == nil {
		current = tuigateway.NewSession(nil)
	}
	startSessionServices(ctx, sid, key, current)
	a, _ := built.(*agent.AIAgent)
	tuigateway.Emit(ctx, "session.info", sid, sessionInfo(ctx, a, current))
	if a != nil {
		scheduleMCPLateRefresh(ctx, sid, a)
	}
}

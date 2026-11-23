package tuiserver

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Words config.set accepts for a boolean value
var boolWords = map[string]bool{"1": true, "on": true, "true": true, "yes": true, "0": false, "off": false, "false": false, "no": false}

// Renderer switches the agent must see, answered as booleans
var displayToggleKeys = map[string]bool{"display.message_reactions": true}

// Aliases whose answer reports the setting as on or off
var onOffAliases = map[string]bool{"density": true, "battery": true, "timestamps": true}

// Aliases whose answer names the setting's key instead of the alias sent
var answerKeys = map[string]string{"approval_mode": "approvals.mode"}

// Keys whose sessionless branch writes a wider scope than the session branch
var sessionScopedConfigKeys = map[string]bool{"model": true, "fast": true, "yolo": true, "reasoning": true}

// One config.set key handler
type configSetter func(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any

var configSetRegistry = &handlerRegistry{}

// Every config.set key and the setter that applies it
var configSetters map[string]configSetter

func init() {
	configSetters = map[string]configSetter{
		"model": cfgsetGuarded(setModel), "fast": setFast, "busy": setBusy,
		"yolo": cfgsetGuarded(setYolo), "reasoning": cfgsetGuarded(setReasoning),
		"cwd": setCwd, "terminal.cwd": setCwd, "workdir": setCwd,
		"prompt": cfgsetGuarded(setPrompt), "personality": cfgsetGuarded(setPersonality), "skin": cfgsetGuarded(setSkin),
		"language": setLanguage}
	configSetRegistry.install()
}

// Whether a failure is a config backend refusal of a locked key or a refused value
func isConfigWriteError(failure any) bool {
	err, isErr := failure.(error)
	var write *config.ConfigWriteError
	return isErr && errors.As(err, &write)
}

// Stripped lower text of a value, any falsy value blank
func cfgWord(value any) string {
	if !truthy(value) {
		return ""
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(value)))
}

// config.set answer of a typed result
func setAnswer(rid any, result tuigateway.ConfigSetResult) map[string]any {
	return ok(rid, tuigateway.Plain(result))
}

// config.set answer naming the key and the word it took
func cfgKV(rid any, key, word string) map[string]any {
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: tuigateway.TextValue(word)})
}

// Config value of a fitted config.set value, a flag or text
func configValueOf(v any) *tuigateway.ConfigValue {
	if on, isFlag := v.(bool); isFlag {
		return tuigateway.FlagValue(on)
	}
	return tuigateway.TextValue(strOr(v))
}

// Waits for an in progress agent build, its error envelope or 5032 when no agent came of it
func cfgsetAwaitAgent(ctx context.Context, session *tuihost.Session, rid any) map[string]any {
	if initErr := waitAgent(ctx, session, rid, 30.0); initErr != nil {
		return initErr
	}
	if sessionAgent(session) == nil {
		return rpcErr(rid, 5032, "agent initialization failed", nil)
	}
	return nil
}

// Model switch envelope whose confirm flag follows the confirm message
func cfgsetModelOK(rid any, key, model, warning, confirmMessage string, deferred bool) map[string]any {
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: tuigateway.TextValue(model), Warning: warning,
		ConfirmRequired: confirmMessage != "", ConfirmMessage: confirmMessage, Scope: "session", Deferred: deferred})
}

// Stashes a model pick for the next turn start, a guard warning answered instead of stashing
func stashPendingModelSwitch(ctx context.Context, rid any, key string, value any, session *tuihost.Session, confirmed bool, parsed provider.ModelSwitchRequest) map[string]any {
	pendingModel := parsed.ModelInput()
	pendingProvider := pystr.Strip(parsed.ExplicitProvider)
	isReset := parsed.IsReset()
	if isReset {
		pendingModel, pendingProvider = configModelTarget(ctx)
		if pendingModel == "" {
			pendingModel = pyrepr.Str(value)
		}
	}
	if !confirmed && !isReset {
		a := sessionAgent(session)
		guardProvider := pendingProvider
		if guardProvider == "" && a != nil {
			guardProvider = pystr.Strip(a.Provider())
		}
		if warning := pendingSwitchSelectionWarning(ctx, pendingModel, guardProvider, a); warning != "" {
			return cfgsetModelOK(rid, key, pendingModel, warning, warning, false)
		}
	}
	session.Set("pending_model_switch", map[string]any{"raw": value, "confirm_expensive_model": confirmed,
		"display_model": pendingModel, "display_provider": pendingProvider})
	return cfgsetModelOK(rid, key, pendingModel, "", "", true)
}

// Setter whose uncaught failure answers 5001, a config backend refusal passing through to config.set
func cfgsetGuarded(fn configSetter) configSetter {
	return func(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) (res map[string]any) {
		failure := catchPanic(func() { res = fn(ctx, rid, params, key, value, session) })
		if failure == nil {
			return res
		}
		if isConfigWriteError(failure) {
			panic(failure)
		}
		return rpcErr(rid, 5001, failureText(failure), nil)
	}
}

// Live or deferred model switch of a session, a sessionless once switch, else the live session refusal
func setModel(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	if !truthy(value) {
		return rpcErr(rid, 4002, "model value required", nil)
	}
	confirmed := truthy(params["confirm_expensive_model"])
	raw := pyrepr.Str(value)
	var result map[string]any
	if session != nil {
		sid := strOr(params["session_id"])
		parsedFlags := provider.ParseModelSwitchArgs(raw)
		if truthy(session.Get("running")) {
			return stashPendingModelSwitch(ctx, rid, key, value, session, confirmed, parsedFlags)
		}
		explicitProvider := parsedFlags.ExplicitProvider
		failedAgentInit := sessionAgent(session) == nil && session.Get("agent_error") != nil
		var failedReady *pythread.Event
		if failedAgentInit {
			failedReady, _ = session.Get("agent_ready").(*pythread.Event)
			if failedReady == nil {
				msg := strOr(session.Get("agent_error"))
				if !truthy(session.Get("agent_error")) {
					msg = "agent initialization failed"
				}
				return rpcErr(rid, 5032, msg, nil)
			}
			if !failedReady.Wait(30.0) {
				return rpcErr(rid, 5032, agentStillStarting, nil)
			}
		}
		currentReady, _ := session.Get("agent_ready").(*pythread.Event)
		failedAgentInit = failedAgentInit && sessionAgent(session) == nil && session.Get("agent_error") != nil &&
			currentReady == failedReady && failedReady.IsSet()
		if sessionAgent(session) == nil && pystr.Strip(explicitProvider) == "" && !failedAgentInit {
			startAgentBuild(ctx, sid, session)
			if initErr := cfgsetAwaitAgent(ctx, session, rid); initErr != nil {
				return initErr
			}
		}
		scoped := sessionProfileRuntimeScope(ctx, session, true)
		switched, err := applyModelSwitch(scoped, sid, session, raw, modelSwitchOptions{confirmExpensiveModel: confirmed,
			pinSessionOverride: true, parsedFlags: &parsedFlags})
		panicIf(err)
		result = switched
		if failedAgentInit && !truthy(result["confirm_required"]) {
			restartCompletedFailedAgentBuild(ctx, sid, session, failedReady)
			if initErr := cfgsetAwaitAgent(ctx, session, rid); initErr != nil {
				return initErr
			}
			persistLiveSessionRuntime(sessionProfileRuntimeScope(ctx, session, true), session)
		}
	} else {
		if !provider.ParseModelSwitchArgs(raw).IsOnce {
			return rpcErr(rid, 4001, "config.set model requires a live session; to change the "+
				"profile default run /setup or `icebath model`", nil)
		}
		switched, err := applyModelSwitch(ctx, "", tuihost.NewSession(map[string]any{"agent": nil}), raw,
			modelSwitchOptions{confirmExpensiveModel: confirmed, pinSessionOverride: true})
		panicIf(err)
		result = switched
	}
	confirmRequired, _ := result["confirm_required"].(bool)
	confirmMessage, _ := result["confirm_message"].(string)
	scope, _ := result["scope"].(string)
	if scope == "" {
		scope = "session"
	}
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: tuigateway.TextValue(strOr(result["value"])), Warning: strOr(result["warning"]),
		ConfirmRequired: confirmRequired, ConfirmMessage: confirmMessage, Scope: tuigateway.ConfigScope(scope)})
}

// Words /fast accepts and the mode each names
var fastWords = map[string]string{"fast": "fast", "on": "fast", "normal": "normal", "off": "normal",
	"auto": "auto", "cold": "cold", "ultrafast": "ultrafast", "ultra": "ultrafast"}

// Session scoped service tier switch, validated against the selected model and mirrored onto a live agent
func setFast(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	raw := cfgWord(value)
	a := sessionAgent(session)
	var currentTier string
	switch {
	case a != nil:
		currentTier = a.ServiceTier()
	case session != nil && session.Get("create_service_tier_override") != nil:
		currentTier = strOr(session.Get("create_service_tier_override"))
	default:
		currentTier = loadServiceTier(ctx)
	}
	if raw == "status" {
		return cfgKV(rid, key, provider.ServiceTierWord(currentTier))
	}
	nv, known := fastWords[raw]
	if !known {
		if raw != "" && raw != "toggle" {
			return rpcErr(rid, 4002, "unknown fast mode: "+pyrepr.Str(value), nil)
		}
		nv = "fast"
		if provider.StaticTiers[currentTier] {
			nv = "normal"
		}
	}
	var overrides map[string]any
	if nv == "fast" || nv == "ultrafast" {
		var targetModel, providerName, baseURL, apiKey string
		if a != nil {
			targetModel, providerName, baseURL, apiKey = a.Model(), a.Provider(), a.BaseURL(), a.APIKey()
		} else {
			if sessionOverride, isDict := session.Get("model_override").(map[string]any); isDict && truthy(sessionOverride["model"]) {
				targetModel = strOr(sessionOverride["model"])
			} else {
				targetModel = resolveModel(ctx)
			}
		}
		if targetModel == "" {
			return rpcErr(rid, 4002, "fast mode needs a selected model; pick one with /model first", nil)
		}
		tier := ""
		if nv == "ultrafast" {
			tier = "ultrafast"
		}
		overrides = provider.ResolveFastModeOverrides(targetModel, providerName, baseURL, tier)
		if overrides != nil && nv == "ultrafast" {
			offered, err := provider.UltrafastOffered(targetModel, providerName, baseURL, apiKey)
			panicIf(err)
			if !offered {
				overrides = nil
			}
		}
		if overrides == nil {
			return rpcErr(rid, 4002, "the provider of "+targetModel+" offers no "+nv+" tier for it; switch with /model to a model that has one", nil)
		}
	}
	sid := strOr(params["session_id"])
	scope := settings.Global
	if session != nil {
		scope = settings.Session
	}
	if _, err := settings.Set(sessionSettings(ctx, sid), settings.FastMode.Key, nv, scope); err != nil {
		panic(asConfigWriteError(err))
	}
	applySessionTier(sessionSettings(ctx, sid), sid, session, overrides)
	return cfgKV(rid, key, nv)
}

// Puts the fast mode in effect for a session onto its record and live agent, request overrides merged in
func applySessionTier(ctx context.Context, sid string, session *tuihost.Session, overrides map[string]any) {
	if session == nil {
		return
	}
	word := settings.FastMode.Get(ctx)
	tier := provider.ParseServiceTier(word)
	session.Set("create_service_tier_override", tier)
	a := sessionAgent(session)
	if a == nil {
		return
	}
	if tier == "" {
		a.SetServiceTier(nil)
	} else {
		a.SetServiceTier(&tier)
	}
	merged := map[string]any{}
	for k, v := range a.RequestOverrides() {
		if k != "service_tier" && k != "speed" {
			merged[k] = v
		}
	}
	for k, v := range overrides {
		merged[k] = v
	}
	a.SetRequestOverrides(merged)
	persistLiveSessionRuntime(ctx, session)
	emitSessionInfo(ctx, sid, session)
}

// Busy input mode status, else the registered setter
func setBusy(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	if w := cfgWord(value); w == "" || w == "status" {
		return cfgKV(rid, key, loadBusyInputMode(sessionSettings(ctx, strOr(params["session_id"]))))
	}
	return setRegistered(ctx, rid, params, key, value, session)
}

// Session yolo toggle, the global approvals.mode flip, or the process flag without a session
func setYolo(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	scopeRaw := params["scope"]
	if !truthy(scopeRaw) {
		scopeRaw = "session"
	}
	scope := cfgWord(scopeRaw)
	raw := cfgWord(value)
	word, known := boolWords[raw]
	var enable bool
	switch {
	case scope == "global":
		appr, isDict := loadCfg(ctx)["approvals"].(map[string]any)
		if !isDict {
			appr = map[string]any{}
		}
		mode, has := appr["mode"]
		if !has {
			mode = "manual"
		}
		enable = tools.NormalizeApprovalMode(mode) != "off"
		if known {
			enable = word
		}
		next := "manual"
		if enable {
			next = "off"
		}
		mustSet(ctx, "approvals.mode", next)
		emitAllSessionInfo(ctx)
	case session != nil:
		sessionKey := session.Get("session_key")
		rowID := pyrepr.Str(sessionKey)
		if a := sessionAgent(session); a != nil && a.SessionID() != "" {
			rowID = a.SessionID()
		}
		persist := func(on bool) error {
			db, release := sessionDB(ctx, session)
			defer release()
			if db != nil {
				return db.SetSessionYolo(ctx, rowID, on)
			}
			return nil
		}
		var requested *bool
		if known {
			requested = &word
		}
		enable = tools.ToggleSessionYolo(ctx, pyrepr.Str(sessionKey), requested, false, persist)
		emitSessionInfo(ctx, strOr(params["session_id"]), session)
	default:
		enable = !provider.IsTruthyValue(os.Getenv("ICEBATH_YOLO_MODE"))
		if known {
			enable = word
		}
		if enable {
			os.Setenv("ICEBATH_YOLO_MODE", "1")
		} else {
			os.Unsetenv("ICEBATH_YOLO_MODE")
		}
	}
	reported := "0"
	if enable {
		reported = "1"
	}
	reportedScope := tuigateway.ConfigScope("session")
	if scope == "global" {
		reportedScope = "global"
	}
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: tuigateway.TextValue(reported), Scope: reportedScope})
}

// Reasoning display words: the setting each writes, the value, and the word answered
var reasoningDisplayWords = []struct {
	words    []string
	key      string
	value    bool
	reported string
}{
	{[]string{"show", "on"}, "display.show_reasoning", true, "show"},
	{[]string{"hide", "off"}, "display.show_reasoning", false, "hide"},
	{[]string{"full", "all"}, "display.reasoning_full", true, "full"},
	{[]string{"clamp", "collapse", "short"}, "display.reasoning_full", false, "clamp"},
}

// Reasoning display words write display fields, effort levels pin the session or write the global effort
func setReasoning(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	arg := cfgWord(value)
	scope := cfgWord(params["scope"])
	for _, spec := range reasoningDisplayWords {
		if slices.Contains(spec.words, arg) {
			mustSet(ctx, spec.key, spec.value)
			return cfgKV(rid, key, spec.reported)
		}
	}
	parsed := config.ParseReasoningEffort(arg)
	if parsed == nil {
		return rpcErr(rid, 4002, "unknown reasoning value: "+pyrepr.Str(value), nil)
	}
	if scope == "global" || session == nil {
		mustSet(ctx, "agent.reasoning_effort", arg)
		if session != nil {
			session.Pop("create_reasoning_override")
		}
	} else {
		session.Set("create_reasoning_override", parsed)
	}
	if a := sessionAgent(session); session != nil && a != nil {
		a.SetReasoningConfig(parsed)
		persistLiveSessionRuntime(ctx, session)
		emitSessionInfo(ctx, strOr(params["session_id"]), session)
	}
	return cfgKV(rid, key, arg)
}

// Display language of the profile, a language a bundled catalog, the locale overlay or a language pack supplies, stored as its canonical id
func setLanguage(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	raw := pystr.Strip(strOr(value))
	loadLanguagePacks(ctx)
	i18n.ResetLanguageCache()
	id := i18n.ResolveLanguageID(ctx, raw, "")
	if id == "" {
		return rpcErr(rid, 4002, "unknown language: "+raw+"; pick one of "+strings.Join(i18n.SupportedLanguagesFor(ctx, ""), "|"), nil)
	}
	mustSet(ctx, settings.Language.Key, id)
	i18n.ResetLanguageCache()
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: tuigateway.TextValue(id)})
}

// Per section fold state, an empty value clearing it
func setDetailsSection(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	child := settings.DetailSectionModes.Key + "." + strings.TrimPrefix(key, "details_mode.")
	if settings.Find(child) == nil {
		return rpcErr(rid, 4002, "unknown section: "+strings.TrimPrefix(key, "details_mode."), nil)
	}
	if cfgWord(value) == "" {
		scope, err := requestScope(params)
		if err != nil {
			return rpcErr(rid, 4002, err.Error(), nil)
		}
		change, err := settings.Reset(scopedSettingsCtx(ctx, params, scope), child, scope)
		if err != nil {
			return rpcErr(rid, 4002, err.Error(), nil)
		}
		return registeredAnswer(rid, key, change.New.Value)
	}
	return setRegistered(ctx, rid, params, child, value, session)
}

// Persists an existing working directory, publishing it to the launch process only for the launch profile
func setCwd(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	raw := ""
	if truthy(value) {
		raw = pystr.Strip(pyrepr.Str(value))
	}
	if raw == "" {
		return rpcErr(rid, 4002, "cwd required", nil)
	}
	cwd, err := pypath.Abspath(pypath.Expanduser(raw))
	panicIf(err)
	if !isDir(cwd) {
		return rpcErr(rid, 4002, "working directory does not exist: "+raw, nil)
	}
	mustSet(ctx, "terminal.cwd", cwd)
	if resolveLax(home.Dir(ctx)) == resolveLax(importedHome()) {
		os.Setenv("TERMINAL_CWD", cwd)
	}
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: "terminal.cwd", Value: tuigateway.TextValue(cwd), Cwd: cwd, Branch: tools.GitProbeBranch(cwd)})
}

// Sets or clears the custom prompt, one explicit key when no config file backs the profile
func setPrompt(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	reported := configValueOf(value)
	if value == "clear" {
		reported = tuigateway.TextValue("")
	}
	answer := setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: reported})
	if value == "clear" {
		if _, err := settings.Reset(ctx, "custom_prompt", settings.Global); err != nil {
			panic(asConfigWriteError(err))
		}
		return answer
	}
	mustSet(ctx, "custom_prompt", value)
	return answer
}

// Persists a personality and applies its prompt to the session
func setPersonality(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	text := ""
	if truthy(value) {
		text = pyrepr.Str(value)
	}
	cfg, err := loadCfgRaw(ctx)
	panicIf(err)
	pname, newPrompt, err := validatePersonality(text, cfg)
	panicIf(err)
	mustSet(ctx, "display.personality", pname)
	historyReset, info := applyPersonalityToSession(ctx, strOr(params["session_id"]), session, newPrompt, pname)
	reported := "none"
	if truthy(value) {
		reported = pyrepr.Str(value)
	}
	result := tuigateway.ConfigSetResult{Key: key, Value: tuigateway.TextValue(reported), HistoryReset: historyReset}
	if info != nil {
		var typed tuigateway.SessionInfo
		if err := tuigateway.Loose(info, &typed); err != nil {
			tuigateway.ReportMisfit("result of", "config.set", err)
		} else {
			result.Info = &typed
		}
	}
	return setAnswer(rid, result)
}

// Persists the skin and repaints every surface, syncing the watcher baseline
func setSkin(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	mustSet(ctx, "display.skin", value)
	broadcastGlobalEvent("skin.changed", resolveSkin(ctx))
	tuihost.NoteSkinBroadcast(ctx)
	return setAnswer(rid, tuigateway.ConfigSetResult{Key: key, Value: configValueOf(value)})
}

// Applies one config key, a stale session refusing the session scoped keys and a backend refusal answering 4002
func configSet(ctx context.Context, rid any, params map[string]any) (res map[string]any) {
	key := strOr(params["key"])
	value, has := params["value"]
	if !has {
		value = ""
	}
	session := tuihost.SessionRecord(strOr(params["session_id"]))
	if session == nil && truthy(params["session_id"]) && sessionScopedConfigKeys[key] && cfgWord(params["scope"]) != "global" {
		_, errResp := sessNowait(ctx, params, rid)
		return errResp
	}
	handler := configSetters[key]
	if handler == nil && strings.HasPrefix(key, "details_mode.") {
		handler = setDetailsSection
	} else if handler == nil && settings.Find(key) != nil {
		handler = setRegistered
	}
	if handler == nil {
		return rpcErr(rid, 4002, "unknown config key: "+key, nil)
	}
	failure := catchPanic(func() { res = handler(ctx, rid, params, key, value, session) })
	if failure == nil {
		return res
	}
	if isConfigWriteError(failure) {
		return rpcErr(rid, 4002, failureText(failure), nil)
	}
	panic(failure)
}

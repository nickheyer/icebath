package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Words config.set accepts for a boolean value
var boolWords = map[string]bool{"1": true, "on": true, "true": true, "yes": true, "0": false, "off": false, "false": false, "no": false}

// Display sections whose details mode a client can override
var detailSectionNames = []string{"thinking", "tools", "subagents", "activity"}

// Renderer switches the agent must see, written as booleans
var displayToggleKeys = map[string]bool{"display.message_reactions": true, "display.in_app_tips": true, "display.in_app_tours": true}

// Keys whose sessionless branch writes a wider scope than the session branch
var sessionScopedConfigKeys = map[string]bool{"model": true, "fast": true, "yolo": true, "reasoning": true}

// One config.set key handler
type configSetter func(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any

var configSetRegistry = &handlerRegistry{}

// Every config.set key and the setter that applies it
var configSetters map[string]configSetter

func init() {
	configSetters = map[string]configSetter{
		"model": cfgsetGuarded(setModel), "fast": setFast, "busy": setBusy, "verbose": setVerbose, "focus": setFocus,
		"approval_mode": setApprovalMode, "approvals.mode": setWord, "yolo": cfgsetGuarded(setYolo),
		"reasoning": cfgsetGuarded(setReasoning), "details_mode": setWord, "thinking_mode": setWord,
		"density": setToggle, "battery": setToggle, "timestamps": setToggle, "theme": setWord,
		"statusbar": setToggle, "mouse": setToggle, "indicator": setWord, "voice.voice_chat_mode": setWord,
		"cwd": setCwd, "terminal.cwd": setCwd, "workdir": setCwd,
		"prompt": cfgsetGuarded(setPrompt), "personality": cfgsetGuarded(setPersonality), "skin": cfgsetGuarded(setSkin),
		"daybreak": setDaybreak}
	configSetRegistry.scopedMethod("config.set", configSet)
	configSetRegistry.install()
}

// Whether a failure is a config backend refusal of a locked key or a refused value
func isConfigWriteError(failure any) bool {
	err, isErr := failure.(error)
	var write *config.ConfigWriteError
	return isErr && errors.As(err, &write)
}

// Persists display fields and section edits, explicit keyed edits when no config file backs the profile
func writeDisplaySections(ctx context.Context, sections []config.KeyValue, dropSections []string, displayFields ...config.KeyValue) {
	fileTooling, err := config.SupportsFileTooling(ctx)
	panicIf(err)
	if !fileTooling {
		sets := make([]config.KeyValue, 0, len(displayFields)+len(sections))
		for _, f := range displayFields {
			sets = append(sets, config.KeyValue{Key: "display." + f.Key, Value: f.Value})
		}
		for _, s := range sections {
			sets = append(sets, config.KeyValue{Key: "display.sections." + s.Key, Value: s.Value})
		}
		unsets := make([]string, 0, len(dropSections))
		for _, name := range dropSections {
			unsets = append(unsets, "display.sections."+name)
		}
		panicIf(writeConfigChanges(ctx, sets, unsets))
		return
	}
	cfg, err := loadCfgRaw(ctx)
	panicIf(err)
	display, isDict := cfg["display"].(map[string]any)
	if !isDict {
		display = map[string]any{}
	}
	cur, isDict := display["sections"].(map[string]any)
	if !isDict {
		cur = map[string]any{}
	}
	for _, f := range displayFields {
		display[f.Key] = f.Value
	}
	for _, s := range sections {
		cur[s.Key] = s.Value
	}
	for _, name := range dropSections {
		delete(cur, name)
	}
	display["sections"] = cur
	cfg["display"] = display
	panicIf(saveCfg(ctx, cfg))
}

// Stripped lower text of a value, any falsy value blank
func cfgWord(value any) string {
	if !truthy(value) {
		return ""
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(value)))
}

// Stripped lower text of a value, only nil blank
func cfgRawWord(value any) string {
	if value == nil {
		return ""
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(value)))
}

// config.set answer naming the key, its value and any extra fields
func cfgKV(rid any, key string, value any, extra ...any) map[string]any {
	result := map[string]any{"key": key, "value": value}
	for i := 0; i+1 < len(extra); i += 2 {
		result[extra[i].(string)] = extra[i+1]
	}
	return ok(rid, result)
}

// Waits for an in progress agent build, its error envelope or 5032 when no agent came of it
func cfgsetAwaitAgent(ctx context.Context, session *tuigateway.Session, rid any) map[string]any {
	if initErr := waitAgent(ctx, session, rid, 30.0); initErr != nil {
		return initErr
	}
	if sessionAgent(session) == nil {
		return rpcErr(rid, 5032, "agent initialization failed", nil)
	}
	return nil
}

// Model switch envelope whose confirm flag follows the confirm message
func cfgsetModelOK(rid any, key string, value any, warning, confirmMessage, scope string, extra ...any) map[string]any {
	fields := append([]any{"warning", warning, "confirm_required", confirmMessage != "", "confirm_message", confirmMessage, "scope", scope}, extra...)
	return cfgKV(rid, key, value, fields...)
}

// Stashes a model pick for the next turn start, a guard warning answered instead of stashing
func stashPendingModelSwitch(ctx context.Context, rid any, key string, value any, session *tuigateway.Session, confirmed bool, parsed provider.ModelSwitchRequest) map[string]any {
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
			return cfgsetModelOK(rid, key, pendingModel, warning, warning, "session", "deferred", false)
		}
	}
	session.Set("pending_model_switch", map[string]any{"raw": value, "confirm_expensive_model": confirmed,
		"display_model": pendingModel, "display_provider": pendingProvider})
	return cfgsetModelOK(rid, key, pendingModel, "", "", "session", "deferred", true)
}

// Setter whose uncaught failure answers 5001, a config backend refusal passing through to config.set
func cfgsetGuarded(fn configSetter) configSetter {
	return func(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) (res map[string]any) {
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
func setModel(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	if !truthy(value) {
		return rpcErr(rid, 4002, "model value required", nil)
	}
	confirmed := truthy(params["confirm_expensive_model"])
	raw := pyrepr.Str(value)
	var result map[string]any
	if session != nil {
		sid := strOr(params["session_id"])
		parsedFlags := provider.ParseModelSwitchArgs(raw)
		if truthy(session.Get("running")) || truthy(session.Get("_compute_host_active")) {
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
			pinSessionOverride: true, parsedFlags: &parsedFlags, countSwitch: true})
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
				"profile default run /setup, or use the Models page (dashboard) / "+
				"Settings -> Models (Desktop)", nil)
		}
		switched, err := applyModelSwitch(ctx, "", tuigateway.NewSession(map[string]any{"agent": nil}), raw,
			modelSwitchOptions{confirmExpensiveModel: confirmed, pinSessionOverride: true, countSwitch: true})
		panicIf(err)
		result = switched
	}
	confirmRequired, has := result["confirm_required"]
	if !has {
		confirmRequired = false
	}
	confirmMessage, has := result["confirm_message"]
	if !has {
		confirmMessage = ""
	}
	scope, has := result["scope"]
	if !has {
		scope = "session"
	}
	return cfgKV(rid, key, result["value"], "warning", result["warning"], "confirm_required", confirmRequired,
		"confirm_message", confirmMessage, "scope", scope)
}

// Words /fast accepts and the mode each names
var fastWords = map[string]string{"fast": "fast", "on": "fast", "normal": "normal", "off": "normal",
	"auto": "auto", "cold": "cold", "ultrafast": "ultrafast", "ultra": "ultrafast"}

// Session scoped service tier switch, validated against the selected model and mirrored onto a live agent
func setFast(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
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
			return rpcErr(rid, 4002, "fast mode is not available without a selected model", nil)
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
			return rpcErr(rid, 4002, nv+" mode is not available for this model", nil)
		}
	}
	if session != nil {
		pinned := nv
		switch nv {
		case "fast":
			pinned = "priority"
		case "normal":
			pinned = ""
		}
		session.Set("create_service_tier_override", pinned)
	} else {
		panicIf(writeConfigKey(ctx, "agent.service_tier", nv))
	}
	if a != nil {
		switch nv {
		case "fast":
			tier := "priority"
			a.SetServiceTier(&tier)
		case "normal":
			a.SetServiceTier(nil)
		default:
			tier := nv
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
		emitSessionInfo(ctx, strOr(params["session_id"]), session)
	}
	return cfgKV(rid, key, nv)
}

// Busy input mode status, else the word setter
func setBusy(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	if w := cfgWord(value); w == "" || w == "status" {
		return cfgKV(rid, key, loadBusyInputMode(ctx))
	}
	return setWord(ctx, rid, params, key, value, session)
}

// Sets or cycles the tool progress mode, mirrored onto the session and its agent
func setVerbose(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	cycle := []string{"off", "new", "all", "verbose"}
	var nv string
	if truthy(value) && value != "cycle" {
		nv = pystr.Lower(pystr.Strip(pyrepr.Str(value)))
		if !slices.Contains(cycle, nv) {
			return rpcErr(rid, 4002, "unknown verbose mode: "+pyrepr.Str(value), nil)
		}
	} else {
		var cur any = loadToolProgressMode(ctx)
		if session != nil {
			if stored, has := session.Lookup("tool_progress_mode"); has {
				cur = stored
			}
		}
		idx := 2
		if text, isText := cur.(string); isText && slices.Contains(cycle, text) {
			idx = slices.Index(cycle, text)
		}
		nv = cycle[(idx+1)%len(cycle)]
	}
	panicIf(writeConfigKey(ctx, "display.tool_progress", nv))
	if session != nil {
		session.Set("tool_progress_mode", nv)
		if a := sessionAgent(session); a != nil {
			a.SetVerboseLogging(nv == "verbose")
		}
	}
	return cfgKV(rid, key, nv)
}

// Focus view on stashes the tool progress mode and pins it off, off restores the stashed mode
func setFocus(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	dF := displayCfg(ctx)
	curFocus := truthy(dF["focus_view"])
	arg := ""
	if truthy(value) {
		arg = pyrepr.Str(value)
	}
	action, target := tuigateway.ResolveFocusArg(arg, curFocus)
	if action == "usage" {
		return rpcErr(rid, 4002, fmt.Sprintf("unknown focus value: %s (use on|off|status)", pyrepr.Str(value)), nil)
	}
	if action == "status" || target == nil {
		state := "off"
		if curFocus {
			state = "on"
		}
		return cfgKV(rid, key, state, "tool_progress", loadToolProgressMode(ctx))
	}
	var updates []config.KeyValue
	var effective string
	if *target {
		var saved any
		if curFocus {
			saved = dF["focus_saved_tool_progress"]
		}
		if !truthy(saved) {
			saved = loadToolProgressMode(ctx)
		}
		updates = append(updates, config.KeyValue{Key: "display.focus_saved_tool_progress", Value: tuigateway.NormalizeToolProgressMode(saved, "all")})
		effective = "off"
	} else {
		saved := dF["focus_saved_tool_progress"]
		if !truthy(saved) {
			saved = "all"
		}
		effective = tuigateway.NormalizeToolProgressMode(saved, "all")
	}
	updates = append(updates, config.KeyValue{Key: "display.tool_progress", Value: effective}, config.KeyValue{Key: "display.focus_view", Value: *target})
	panicIf(writeConfigKeys(ctx, updates))
	if session != nil {
		session.Set("focus_view", *target)
		session.Set("tool_progress_mode", effective)
		if a := sessionAgent(session); a != nil {
			catchPanic(func() { a.SetToolProgressMode(effective) })
		}
	}
	state := "off"
	if *target {
		state = "on"
	}
	return cfgKV(rid, key, state, "tool_progress", effective)
}

// Legacy alias of approvals.mode reporting the real key
func setApprovalMode(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	return setWord(ctx, rid, params, "approvals.mode", value, session)
}

// Session yolo toggle, the global approvals.mode flip, or the process flag without a session
func setYolo(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
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
		panicIf(writeConfigKey(ctx, "approvals.mode", next))
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
		enable = tools.ToggleSessionYolo(pyrepr.Str(sessionKey), requested, false, persist)
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
	reportedScope := "session"
	if scope == "global" {
		reportedScope = scope
	}
	return cfgKV(rid, key, reported, "scope", reportedScope)
}

// One reasoning display word, the value it reports, its display field, thinking section and session flag
type reasoningDisplayWord struct {
	words    []string
	reported string
	field    config.KeyValue
	thinking string
	show     *bool
}

var (
	reasoningShow         = true
	reasoningHide         = false
	reasoningDisplayWords = []reasoningDisplayWord{
		{[]string{"show", "on"}, "show", config.KeyValue{Key: "show_reasoning", Value: true}, "expanded", &reasoningShow},
		{[]string{"hide", "off"}, "hide", config.KeyValue{Key: "show_reasoning", Value: false}, "hidden", &reasoningHide},
		{[]string{"full", "all"}, "full", config.KeyValue{Key: "reasoning_full", Value: true}, "expanded", nil},
		{[]string{"clamp", "collapse", "short"}, "clamp", config.KeyValue{Key: "reasoning_full", Value: false}, "collapsed", nil}}
)

// Reasoning display words write display fields, effort levels pin the session or write the global effort
func setReasoning(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	arg := cfgWord(value)
	scope := cfgWord(params["scope"])
	for _, spec := range reasoningDisplayWords {
		if slices.Contains(spec.words, arg) {
			writeDisplaySections(ctx, []config.KeyValue{{Key: "thinking", Value: spec.thinking}}, nil, spec.field)
			if spec.show != nil && session != nil {
				session.Set("show_reasoning", *spec.show)
			}
			return cfgKV(rid, key, spec.reported)
		}
	}
	parsed := config.ParseReasoningEffort(arg)
	if parsed == nil {
		return rpcErr(rid, 4002, "unknown reasoning value: "+pyrepr.Str(value), nil)
	}
	if scope == "global" || session == nil {
		panicIf(writeConfigKey(ctx, "agent.reasoning_effort", arg))
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

// One word setter, its normalizer, accepted words, error template and apply step
type wordSetter struct {
	norm    func(any) string
	allowed []string
	errText func(value any, raw string) string
	apply   func(ctx context.Context, w string)
}

// Word setters keyed by config.set key, the reported value being the accepted word
func wordSetters() map[string]wordSetter {
	named := func(prefix string) func(any, string) string {
		return func(value any, raw string) string { return prefix + pyrepr.Str(value) }
	}
	return map[string]wordSetter{
		"busy": {cfgWord, []string{"queue", "steer", "interrupt"}, named("unknown busy mode: "),
			func(ctx context.Context, w string) { panicIf(writeConfigKey(ctx, "display.busy_input_mode", w)) }},
		"approvals.mode": {cfgWord, []string{"manual", "smart", "off"},
			func(value any, raw string) string {
				return "unknown approval mode: " + pyrepr.Str(value) + "; pick one of manual|smart|off"
			},
			func(ctx context.Context, w string) {
				panicIf(writeConfigKey(ctx, "approvals.mode", w))
				emitAllSessionInfo(ctx)
			}},
		"details_mode": {cfgWord, []string{"hidden", "collapsed", "expanded"}, named("unknown details_mode: "),
			func(ctx context.Context, w string) {
				sections := make([]config.KeyValue, 0, len(detailSectionNames))
				for _, section := range detailSectionNames {
					sections = append(sections, config.KeyValue{Key: section, Value: w})
				}
				writeDisplaySections(ctx, sections, nil, config.KeyValue{Key: "details_mode", Value: w})
			}},
		"thinking_mode": {cfgWord, []string{"collapsed", "truncated", "full"}, named("unknown thinking_mode: "),
			func(ctx context.Context, w string) {
				details := "collapsed"
				if w == "full" {
					details = "expanded"
				}
				panicIf(writeConfigKeys(ctx, []config.KeyValue{{Key: "display.thinking_mode", Value: w}, {Key: "display.details_mode", Value: details}}))
			}},
		"theme": {cfgWord, []string{"auto", "light", "dark"},
			func(value any, raw string) string {
				return "unknown theme value: " + pyrepr.Str(value) + " (use auto|light|dark)"
			},
			func(ctx context.Context, w string) { panicIf(writeConfigKey(ctx, "display.tui_theme", w)) }},
		"indicator": {cfgRawWord, home.IndicatorStyles,
			func(value any, raw string) string {
				return "unknown indicator: " + pyrepr.Repr(raw) + "; pick one of " + strings.Join(home.IndicatorStyles, "|")
			},
			func(ctx context.Context, w string) { panicIf(writeConfigKey(ctx, "display.tui_status_indicator", w)) }},
		"voice.voice_chat_mode": {cfgWord, []string{"chained", "gpt-live"},
			func(value any, raw string) string {
				return "unknown voice chat mode: " + pyrepr.Str(value) + "; pick chained|gpt-live"
			},
			func(ctx context.Context, w string) { panicIf(writeConfigKey(ctx, "voice.voice_chat_mode", w)) }}}
}

// Applies an accepted word setter value, 4002 for any other word
func setWord(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	spec := wordSetters()[key]
	raw := spec.norm(value)
	if !slices.Contains(spec.allowed, raw) {
		return rpcErr(rid, 4002, spec.errText(value, raw), nil)
	}
	spec.apply(ctx, raw)
	return cfgKV(rid, key, raw)
}

// Per section details mode override, an empty value clearing it
func setDetailsSection(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	section := strings.SplitN(key, ".", 2)[1]
	if !slices.Contains(detailSectionNames, section) {
		return rpcErr(rid, 4002, "unknown section: "+section, nil)
	}
	nv := cfgWord(value)
	if nv != "" && !detailModes[nv] {
		return rpcErr(rid, 4002, "unknown details_mode: "+pyrepr.Str(value), nil)
	}
	if nv != "" {
		writeDisplaySections(ctx, []config.KeyValue{{Key: section, Value: nv}}, nil)
	} else {
		writeDisplaySections(ctx, nil, []string{section})
	}
	return cfgKV(rid, key, nv)
}

// One toggle setter, its normalizer, config key, alias words, flipped current value and report
type toggleSetter struct {
	norm    func(any) string
	cfgKey  string
	aliases map[string]any
	flipped func(ctx context.Context) any
	report  func(v any) any
}

// Toggle setters keyed by config.set key, empty or toggle flipping the current value
func toggleSetters() map[string]toggleSetter {
	onOff := func(v any) any {
		if truthy(v) {
			return "on"
		}
		return "off"
	}
	same := func(v any) any { return v }
	boolAliases := func(words map[string]bool) map[string]any {
		out := map[string]any{}
		for w, b := range words {
			out[w] = b
		}
		return out
	}
	statusbarAliases := map[string]any{"on": "top"}
	for mode := range statusbarModes {
		statusbarAliases[mode] = mode
	}
	mouseAliases := map[string]any{}
	for alias, mode := range tuigateway.MouseTrackingAliases {
		mouseAliases[alias] = mode
	}
	return map[string]toggleSetter{
		"density": {cfgWord, "display.tui_compact", map[string]any{"on": true, "off": false},
			func(ctx context.Context) any { return !truthy(displayCfg(ctx)["tui_compact"]) }, onOff},
		"battery": {cfgWord, "display.battery",
			map[string]any{"on": true, "true": true, "yes": true, "off": false, "false": false, "no": false},
			func(ctx context.Context) any { return !truthy(displayCfg(ctx)["battery"]) }, onOff},
		"timestamps": {cfgWord, "display.timestamps", boolAliases(boolWords),
			func(ctx context.Context) any { return !truthy(displayCfg(ctx)["timestamps"]) }, onOff},
		"statusbar": {cfgWord, "display.tui_statusbar", statusbarAliases,
			func(ctx context.Context) any {
				current, has := displayCfg(ctx)["tui_statusbar"]
				if !has {
					current = "top"
				}
				if coerceStatusbar(current) == "off" {
					return "top"
				}
				return "off"
			}, same},
		"mouse": {cfgRawWord, "display.mouse_tracking", mouseAliases,
			func(ctx context.Context) any {
				if displayMouseTracking(displayCfg(ctx)) == "off" {
					return "all"
				}
				return "off"
			}, same}}
}

// Flips or sets one display toggle, 4002 for an unknown word
func setToggle(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	spec := toggleSetters()[key]
	raw := spec.norm(value)
	var nv any
	if raw == "" || raw == "toggle" {
		nv = spec.flipped(ctx)
	} else {
		nv = spec.aliases[raw]
	}
	if nv == nil {
		return rpcErr(rid, 4002, fmt.Sprintf("unknown %s value: %s", key, pyrepr.Str(value)), nil)
	}
	panicIf(writeConfigKey(ctx, spec.cfgKey, nv))
	return cfgKV(rid, key, spec.report(nv))
}

// Persists an existing working directory, publishing it to the launch process only for the launch profile
func setCwd(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
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
	panicIf(writeConfigKey(ctx, "terminal.cwd", cwd))
	if resolveLax(home.Dir(ctx)) == resolveLax(hermesHome) {
		os.Setenv("TERMINAL_CWD", cwd)
	}
	return cfgKV(rid, "terminal.cwd", cwd, "cwd", cwd, "branch", tools.GitProbeBranch(cwd))
}

// Sets or clears the custom prompt, one explicit key when no config file backs the profile
func setPrompt(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	reported := value
	if value == "clear" {
		reported = ""
	}
	fileTooling, err := config.SupportsFileTooling(ctx)
	panicIf(err)
	if !fileTooling {
		if value == "clear" {
			panicIf(writeConfigChanges(ctx, nil, []string{"custom_prompt"}))
		} else {
			panicIf(writeConfigChanges(ctx, []config.KeyValue{{Key: "custom_prompt", Value: value}}, nil))
		}
		return cfgKV(rid, key, reported)
	}
	cfg, err := loadCfgRaw(ctx)
	panicIf(err)
	if value == "clear" {
		delete(cfg, "custom_prompt")
	} else {
		cfg["custom_prompt"] = value
	}
	panicIf(saveCfg(ctx, cfg))
	return cfgKV(rid, key, reported)
}

// Persists a personality and applies its prompt to the session
func setPersonality(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	text := ""
	if truthy(value) {
		text = pyrepr.Str(value)
	}
	cfg, err := loadCfgRaw(ctx)
	panicIf(err)
	pname, newPrompt, err := validatePersonality(text, cfg)
	panicIf(err)
	PersistPersonality(ctx, pname)
	historyReset, info := applyPersonalityToSession(ctx, strOr(params["session_id"]), session, newPrompt, pname)
	reported := "none"
	if truthy(value) {
		reported = pyrepr.Str(value)
	}
	if info != nil {
		return cfgKV(rid, key, reported, "history_reset", historyReset, "info", info)
	}
	return cfgKV(rid, key, reported, "history_reset", historyReset)
}

// Persists the skin and repaints every surface, syncing the watcher baseline
func setSkin(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	panicIf(writeConfigKey(ctx, "display.skin", value))
	broadcastGlobalEvent("skin.changed", resolveSkin(ctx))
	tuigateway.NoteSkinBroadcast(ctx)
	return cfgKV(rid, key, value)
}

// Boolean renderer switch the agent sees
func setDisplayToggle(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	on, known := boolWords[pystr.Lower(pystr.Strip(pyrepr.Str(value)))]
	if !known {
		return rpcErr(rid, 4002, key+" takes true or false", nil)
	}
	panicIf(writeConfigKey(ctx, key, on))
	return cfgKV(rid, key, on)
}

// Profile wide daybreak flag every turn reads
func setDaybreak(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuigateway.Session) map[string]any {
	on, known := boolWords[pystr.Lower(pystr.Strip(pyrepr.Str(value)))]
	if !known {
		return rpcErr(rid, 4002, key+" takes true or false", nil)
	}
	panicIf(writeConfigKey(ctx, "agent.daybreak", on))
	return cfgKV(rid, key, on)
}

// Applies one config key, a stale session refusing the session scoped keys and a backend refusal answering 4002
func configSet(ctx context.Context, rid any, params map[string]any) (res map[string]any) {
	key := strOr(params["key"])
	value, has := params["value"]
	if !has {
		value = ""
	}
	session := tuigateway.SessionRecord(strOr(params["session_id"]))
	if session == nil && truthy(params["session_id"]) && sessionScopedConfigKeys[key] && cfgWord(params["scope"]) != "global" {
		_, errResp := sessNowait(ctx, params, rid)
		return errResp
	}
	handler := configSetters[key]
	if handler == nil && strings.HasPrefix(key, "details_mode.") {
		handler = setDetailsSection
	} else if handler == nil && displayToggleKeys[key] {
		handler = setDisplayToggle
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

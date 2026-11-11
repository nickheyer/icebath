package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Declares the one shot, handoff and usage methods on the session module registry
func registerSessionUsageHandlers() {
	sessionRegistry.scopedMethod("llm.oneshot", llmOneshot)
	sessionMethod("handoff.request", false, handoffRequest)
	sessionRegistry.method("handoff.state", withSession(withSessionDB(5007, handoffState)))
	sessionRegistry.method("handoff.fail", handoffFail)
	sessionMethod("session.usage", false, sessionUsage)
	sessionMethod("session.context_breakdown", false, sessionContextBreakdown)
}

// Value of a param, the default when it is falsy
func truthyParam(params map[string]any, key string, def any) any {
	if value := params[key]; truthy(value) {
		return value
	}
	return def
}

// Stateless one shot LLM request, a live session lending its model and profile scope
func llmOneshot(ctx context.Context, rid any, params map[string]any) map[string]any {
	template := pystr.Strip(strOr(truthyParam(params, "template", "")))
	instructions := pyrepr.Str(truthyParam(params, "instructions", ""))
	userInput := pyrepr.Str(truthyParam(params, "input", ""))
	variables, isDict := params["variables"].(map[string]any)
	if !isDict {
		variables = map[string]any{}
	}
	temperature := 0.3
	if raw := params["temperature"]; raw != nil {
		parsed, err := pyint.Float(raw)
		var pyErr *pyint.Error
		switch {
		case err == nil:
			temperature = parsed
		case errors.As(err, &pyErr) && pyErr.Kind == "OverflowError":
			panic(err)
		}
	}
	if template == "" && pystr.Strip(instructions) == "" && pystr.Strip(userInput) == "" {
		return rpcErr(rid, 4030, "llm.oneshot requires a template or instructions/input", nil)
	}
	session := tuigateway.SessionRecord(strOr(truthyParam(params, "session_id", "")))
	var text string
	failure := catchPanic(func() {
		scoped := ctx
		var mainRuntime map[string]any
		if session != nil {
			scoped = sessionProfileRuntimeScope(ctx, session, true)
			mainRuntime = mainRuntimeFromAgent(session.Get("agent"))
		}
		task := pystr.Strip(pyrepr.Str(truthyParam(params, "task", "title_generation")))
		if task == "" {
			task = "title_generation"
		}
		maxTokens := intParam(params, "max_tokens", 1024)
		if maxTokens == 0 {
			maxTokens = 1024
		}
		reply, err := agent.RunOneshot(scoped, agent.OneshotRequest{Instructions: instructions, UserInput: userInput, Template: template,
			Variables: variables, Task: task, MaxTokens: maxTokens, Temperature: &temperature, Timeout: 60.0, MainRuntime: mainRuntime})
		panicIf(err)
		text = reply
	})
	if failure == nil {
		return ok(rid, map[string]any{"text": text})
	}
	err := failureError(failure)
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "KeyError: "):
		return rpcErr(rid, 4031, strings.TrimPrefix(message, "KeyError: "), nil)
	case strings.HasPrefix(message, "ValueError: "):
		return rpcErr(rid, 4032, strings.TrimPrefix(message, "ValueError: "), nil)
	case isValueError(err):
		return rpcErr(rid, 4032, message, nil)
	}
	logger().Warn(fmt.Sprintf("llm.oneshot failed: %s", message))
	return rpcErr(rid, 5030, "one-shot generation failed: "+message, nil)
}

// Queues a handoff of an idle session to a configured platform's home channel
func handoffRequest(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
	if truthy(session.Get("running")) {
		return rpcErr(rid, 4009, "session busy — wait for the current turn to finish, then retry the handoff", nil)
	}
	platformName := pystr.Lower(pystr.Strip(strOr(truthyParam(params, "platform", ""))))
	if platformName == "" {
		return rpcErr(rid, 4023, "platform required", nil)
	}
	platform, err := gateway.ParsePlatform(ctx, platformName)
	if err != nil {
		return rpcErr(rid, 4024, fmt.Sprintf("unknown platform '%s'", platformName), nil)
	}
	var config *gateway.GatewayConfig
	if failure := catchPanic(func() {
		loaded, err := gateway.LoadGatewayConfig(sessionProfileRuntimeScope(ctx, session, true))
		panicIf(err)
		config = loaded
	}); failure != nil {
		return rpcErr(rid, 5021, "could not load gateway config: "+failureText(failure), nil)
	}
	if platformConfig := config.Platforms[platform]; platformConfig == nil || !platformConfig.Enabled {
		return rpcErr(rid, 4025, fmt.Sprintf("platform '%s' is not configured/enabled in the gateway", platformName), nil)
	}
	homeChannel := config.GetHomeChannel(platform)
	if homeChannel == nil || homeChannel.ChatID == "" {
		return rpcErr(rid, 4026, fmt.Sprintf("no home channel configured for %s — set one with /sethome on the destination chat first", platformName), nil)
	}
	ensureSessionDBRow(ctx, session)
	key := strOr(session.Get("session_key"))
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return dbUnavailableError(ctx, rid, 5007)
	}
	var early map[string]any
	if failure := catchPanic(func() {
		row, err := db.GetSession(ctx, key)
		panicIf(err)
		if row == nil || row.Len() == 0 {
			_, err = db.SetSessionTitle(ctx, key, "handoff-"+pystr.Slice(key, 0, 8))
			panicIf(err)
		}
		queued, err := db.RequestHandoff(ctx, key, platformName)
		panicIf(err)
		if !queued {
			early = rpcErr(rid, 4027, "session is already in flight for handoff — wait for it to settle, then retry", nil)
		}
	}); failure != nil {
		return rpcErr(rid, 5007, failureText(failure), nil)
	}
	if early != nil {
		return early
	}
	return ok(rid, map[string]any{"queued": true, "session_key": key, "platform": platformName, "home_name": homeChannel.Name})
}

// Field of a handoff record, empty when unset
func handoffField(record map[string]any, field string) any {
	if value := record[field]; truthy(value) {
		return value
	}
	return ""
}

// Handoff state, platform and error of a session, empty fields when none was requested
func handoffState(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, db *state.DB) map[string]any {
	record := objectMap(db.GetHandoffState(ctx, strOr(session.Get("session_key"))))
	return ok(rid, map[string]any{"state": handoffField(record, "state"), "platform": handoffField(record, "platform"),
		"error": handoffField(record, "error")})
}

// Fails a handoff the watcher has not claimed yet
func handoffFail(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	reason := pystr.Strip(pyrepr.Str(truthyParam(params, "error", "handoff failed")))
	if utf8.RuneCountInString(reason) > 500 {
		reason = string([]rune(reason)[:500])
	}
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return dbUnavailableError(ctx, rid, 5007)
	}
	key := strOr(session.Get("session_key"))
	failed, err := db.FailHandoff(ctx, key, reason, []string{"pending"})
	panicIf(err)
	current := any("failed")
	if !failed {
		current = handoffField(objectMap(db.GetHandoffState(ctx, key)), "state")
	}
	return ok(rid, map[string]any{"failed": failed, "state": current})
}

// Token usage of a session with the Nous credits block, zeros before the agent exists
func sessionUsage(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
	usage := sessionUsageSnapshot(ctx, session)
	if session.Get("agent") == nil && len(usage) == 0 {
		usage = map[string]any{"calls": 0, "input": 0, "output": 0, "total": 0}
	}
	var credits *provider.CreditsState
	if a := sessionAgent(session); a != nil {
		credits = a.CreditsCapture().Credits()
	}
	for key, value := range provider.NousCreditsUsageFields(ctx, credits) {
		usage[key] = value
	}
	return ok(rid, usage)
}

// Usage field or its default when falsy
func usageOr(usage map[string]any, key string, def any) any {
	if value, present := usage[key]; present && truthy(value) {
		return value
	}
	return def
}

// Plain JSON value of a typed payload
func plainJSON(v any) any {
	data, err := json.Marshal(v)
	panicIf(err)
	var out any
	panicIf(json.Unmarshal(data, &out))
	return out
}

// Context window breakdown by category with the context files that fed it, a usage only shape before the agent exists
func sessionContextBreakdown(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session) map[string]any {
	if session.Get("agent") == nil {
		usage := sessionUsageSnapshot(ctx, session)
		if len(usage) == 0 {
			usage = getUsage(ctx, nil)
		}
		estimated, present := usage["context_estimated"]
		if !present {
			estimated = false
		}
		source, present := usage["context_source"]
		if !present {
			source = "provider_usage"
		}
		model, present := metadataMirror(session)["model"]
		if !present {
			model = ""
		}
		return ok(rid, map[string]any{"categories": []any{}, "context_max": usageOr(usage, "context_max", 0),
			"context_percent": usageOr(usage, "context_percent", 0), "context_used": usageOr(usage, "context_used", 0),
			"estimated_total": 0, "context_estimated": estimated, "context_source": source, "model": model})
	}
	unlock := lockHistory(session)
	history := historyDicts(append([]any(nil), historyList(session.Get("history"))...))
	unlock()
	scoped, env := setSessionContext(ctx, strOr(session.Get("session_key")), nil, "")
	defer clearSessionContext(env)
	var payload map[string]any
	if failure := catchPanic(func() {
		profileScoped := sessionProfileRuntimeScope(scoped, session, true)
		a := sessionAgent(session)
		breakdown, err := agent.ComputeSessionContextBreakdown(profileScoped, a, history)
		panicIf(err)
		payload, _ = plainJSON(breakdown).(map[string]any)
		sources, err := agent.ContextFileSourcesForAgent(profileScoped, a)
		panicIf(err)
		payload["context_files"] = plainJSON(sources)
	}); failure != nil {
		return rpcErr(rid, 5000, "Could not compute context breakdown: "+failureText(failure), nil)
	}
	return ok(rid, payload)
}

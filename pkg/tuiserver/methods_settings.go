package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var settingsRegistry = &handlerRegistry{}

func init() {
	settingsRegistry.install()
	settings.Subscribe(func(context.Context, settings.Change) { dropCfgCache() })
}

// Settings refusal as a config write error, so config.set answers it with 4002
func asConfigWriteError(err error) error {
	var write *config.ConfigWriteError
	if errors.As(err, &write) {
		return err
	}
	return config.NewConfigValueError(err.Error(), "")
}

// Writes one setting to the profile, a refusal raised as a config write error
func mustSet(ctx context.Context, key string, value any) {
	if _, err := settings.Set(ctx, key, value, settings.Global); err != nil {
		panic(asConfigWriteError(err))
	}
}

// Turns a live agent's verbose logging on or off with the tool progress mode its session runs
func applyVerboseLogging(ctx context.Context, sid string, session *tuihost.Session) {
	if a := sessionAgent(session); a != nil {
		a.SetVerboseLogging(sessionVerbose(ctx, sid))
	}
}

// Steps a settings write runs afterwards for the request's session, keyed by setting
var afterConfigSet = map[string]func(ctx context.Context, sid string, session *tuihost.Session){
	"approvals.mode":        func(ctx context.Context, _ string, _ *tuihost.Session) { emitAllSessionInfo(ctx) },
	"display.tool_progress": applyVerboseLogging,
	"display.detail_level":  applyVerboseLogging,
	"agent.service_tier": func(ctx context.Context, sid string, session *tuihost.Session) {
		applySessionTier(sessionSettings(ctx, sid), sid, session, nil)
	},
	"display.skin": func(ctx context.Context, _ string, _ *tuihost.Session) {
		broadcastGlobalEvent("skin.changed", resolveSkin(ctx))
		tuihost.NoteSkinBroadcast(ctx)
	},
}

// Scope a request names, zero when it names none and an error for a word that is no scope
func requestScope(params map[string]any) (settings.Scope, error) {
	word := cfgWord(params["scope"])
	if word == "" {
		return 0, nil
	}
	if scope := settings.ParseScope(word); scope != 0 {
		return scope, nil
	}
	return 0, fmt.Errorf("%w: %s", settings.ErrScope, word)
}

// Context of a write at a scope, bound to the request's session for a session write
func scopedSettingsCtx(ctx context.Context, params map[string]any, scope settings.Scope) context.Context {
	if scope == settings.Session {
		return sessionSettings(ctx, strOr(params["session_id"]))
	}
	return ctx
}

// config.set answer of a registered setting in the shape its alias answers with
func registeredAnswer(rid any, key string, value any) map[string]any {
	answerKey := key
	if k, ok := answerKeys[key]; ok {
		answerKey = k
	}
	on, isBool := value.(bool)
	switch {
	case isBool && onOffAliases[key]:
		word := "off"
		if on {
			word = "on"
		}
		return cfgKV(rid, answerKey, word)
	case isBool:
		return setAnswer(rid, tuigateway.ConfigSetResult{Key: answerKey, Value: tuigateway.FlagValue(on)})
	case value == nil:
		return cfgKV(rid, answerKey, "")
	}
	return cfgKV(rid, answerKey, pyrepr.Str(value))
}

// config.set of any registered setting through the settings writer, an empty or toggle value flipping a switch
func setRegistered(ctx context.Context, rid any, params map[string]any, key string, value any, session *tuihost.Session) map[string]any {
	e := settings.Find(key)
	if e == nil {
		return rpcErr(rid, 4002, "unknown config key: "+key, nil)
	}
	if w := cfgWord(value); w == "" || w == "toggle" {
		if next := settings.Flipped(ctx, e); next != nil {
			value = next
		}
	}
	scope, err := requestScope(params)
	if err != nil {
		return rpcErr(rid, 4002, err.Error(), nil)
	}
	change, err := settings.Set(scopedSettingsCtx(ctx, params, scope), e.Info().Key, value, scope)
	if err != nil {
		return rpcErr(rid, 4002, err.Error(), nil)
	}
	if after := afterConfigSet[change.Key]; after != nil {
		after(ctx, strOr(params["session_id"]), session)
	}
	return registeredAnswer(rid, key, change.New.Value)
}

// Every settings screen group in order
func settingsGroups(ctx context.Context, rid any, params map[string]any) map[string]any {
	return ok(rid, map[string]any{"groups": settings.Groups()})
}

// Rows of the settings a query selects with their values and sources, read for the request's session when it names one
func settingsList(ctx context.Context, rid any, params map[string]any) map[string]any {
	var q settings.Query
	if err := tuigateway.Loose(params, &q); err != nil {
		return rpcErr(rid, 4002, "settings.list: "+err.Error(), nil)
	}
	rows := settings.Rows(sessionSettings(ctx, strOr(params["session_id"])), q)
	if rows == nil {
		rows = []settings.Row{}
	}
	return ok(rid, map[string]any{"rows": rows})
}

// Answer of a settings write: the changed row, or the refusal with 4002 and 4003 for a locked setting
func settingsWriteAnswer(ctx context.Context, rid any, params map[string]any, change settings.Change, err error) map[string]any {
	if err != nil {
		code := 4002
		if errors.Is(err, settings.ErrLocked) {
			code = 4003
		}
		return rpcErr(rid, code, err.Error(), nil)
	}
	sid := strOr(params["session_id"])
	if after := afterConfigSet[change.Key]; after != nil {
		after(ctx, sid, tuihost.SessionRecord(sid))
	}
	return ok(rid, map[string]any{"row": settings.Describe(sessionSettings(ctx, sid), settings.Find(change.Key)), "apply": change.Apply.String()})
}

// Sets one setting at a scope through the settings writer
func settingsSet(ctx context.Context, rid any, params map[string]any) map[string]any {
	key := strOr(params["key"])
	e := settings.Find(key)
	if e == nil {
		return rpcErr(rid, 4002, fmt.Sprintf("unknown setting: %s", key), nil)
	}
	scope, err := requestScope(params)
	if err != nil {
		return rpcErr(rid, 4002, err.Error(), nil)
	}
	change, err := settings.Set(scopedSettingsCtx(ctx, params, scope), e.Info().Key, params["value"], scope)
	return settingsWriteAnswer(ctx, rid, params, change, err)
}

// Removes the value one scope holds so the next layer applies
func settingsReset(ctx context.Context, rid any, params map[string]any) map[string]any {
	key := strOr(params["key"])
	e := settings.Find(key)
	if e == nil {
		return rpcErr(rid, 4002, fmt.Sprintf("unknown setting: %s", key), nil)
	}
	scope, err := requestScope(params)
	if err != nil {
		return rpcErr(rid, 4002, err.Error(), nil)
	}
	change, err := settings.Reset(scopedSettingsCtx(ctx, params, scope), e.Info().Key, scope)
	return settingsWriteAnswer(ctx, rid, params, change, err)
}

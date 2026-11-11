package tuiserver

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Builds a replacement agent on the session profile, carrying its pins, and hands it the dedicated db handle
func rebuildSessionAgent(ctx context.Context, sid string, session *tuigateway.Session, kw makeAgentOptions, modelOverrideSet bool) (rebuilt *agent.AIAgent, err error) {
	oldAgent := sessionAgent(session)
	profileHome := strOr(session.Get("profile_home"))
	var sessionDB *state.DB
	if oldAgent != nil {
		sessionDB = oldAgent.SessionDB()
	}
	opened := sessionDB == nil && profileHome != ""
	scoped := bindBuildProfileScopes(ctx, profileHome)
	var configModelSeen []any
	built, buildErr := func() (built any, err error) {
		defer func() {
			r := recover()
			if (r != nil || err != nil) && opened && sessionDB != nil {
				suppress(func() { state.ReleaseOrClose(sessionDB) })
			}
			if r != nil {
				panic(r)
			}
		}()
		model, providerName := configModelTarget(scoped)
		configModelSeen = []any{model, providerName}
		if opened {
			if sessionDB, err = openProfileSessionDB(scoped, profileHome); err != nil {
				return nil, err
			}
		}
		if override, isDict := session.Get("model_override").(map[string]any); !modelOverrideSet && isDict {
			kw.modelOverride = override
		}
		if pin, present := session.Lookup("create_reasoning_override"); !kw.reasoningOverrideSet && present && pin != nil {
			kw.reasoningConfigOverride, _ = pin.(map[string]any)
			kw.reasoningOverrideSet = true
		}
		if pin, present := session.Lookup("create_service_tier_override"); kw.serviceTierOverride == nil && present && pin != nil {
			text := strOr(pin)
			kw.serviceTierOverride = &text
		}
		kw.sessionDB = sessionDB
		return makeAgent(scoped, sid, strOr(session.Get("session_key")), kw)
	}()
	if buildErr != nil {
		return nil, buildErr
	}
	a, _ := built.(*agent.AIAgent)
	closedMidbuild := func() bool {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		if truthy(session.Get("_closing")) {
			return true
		}
		if oldAgent != nil && a != nil {
			a.AdoptProcessOwnerTaskIDs(oldAgent.ProcessOwnerTaskIDs())
		}
		session.Set("agent", built)
		session.Set("config_model_seen", configModelSeen)
		owned := opened || (oldAgent != nil && oldAgent.OwnsSessionDB())
		if owned && transferDBToAgent(scoped, a, sessionDB) {
			if oldAgent != nil {
				oldAgent.SetOwnsSessionDB(false)
			}
		} else if opened {
			suppress(func() { state.ReleaseOrClose(sessionDB) })
		}
		return false
	}()
	if closedMidbuild {
		suppress(func() {
			scope := sessionProfileRuntimeScope(scoped, session, true)
			if closer, ok := built.(interface {
				CloseContext(context.Context) error
			}); ok {
				_ = closer.CloseContext(scope)
			}
		})
		if opened {
			suppress(func() { state.ReleaseOrClose(sessionDB) })
		}
		return nil, errors.New("session was closed while its agent was being rebuilt")
	}
	return a, nil
}

// Rebuilds a Bot Chat session agent for the same session when its capability fingerprint changed
func syncBotCapabilities(ctx context.Context, sid string, session *tuigateway.Session) {
	a := sessionAgent(session)
	if a == nil {
		return
	}
	changed := func() (changed bool) {
		defer func() {
			if recover() != nil {
				changed = false
			}
		}()
		title := strings.TrimSpace(a.SessionTitleHint())
		if title == "" {
			db, key := a.SessionDB(), strOr(session.Get("session_key"))
			if db != nil && key != "" {
				stored, _, err := db.GetSessionTitle(ctx, key)
				if err != nil {
					return false
				}
				title = strings.TrimSpace(stored)
			}
		}
		if title != "Bot Chat" {
			return false
		}
		current := botmode.CapabilityFingerprint(strOr(session.Get("profile_home")))
		if current == "unavailable" {
			return false
		}
		seen, present := session.Lookup("bot_caps_seen")
		session.Set("bot_caps_seen", current)
		return present && seen != nil && seen != current
	}()
	if !changed {
		return
	}
	failure := catchPanic(func() {
		cwd := sessionCwd(ctx, session)
		scoped, env := setSessionContext(ctx, sid, &cwd, "")
		rebuilt, err := func() (*agent.AIAgent, error) {
			defer clearSessionContext(env)
			return rebuildSessionAgent(scoped, sid, session, makeAgentOptions{
				sessionID:        strOr(session.Get("session_key")),
				platformOverride: sessionSource(session),
			}, false)
		}()
		panicIf(err)
		rebuilt.SetSessionTitleHint("Bot Chat")
		tuigateway.Emit(ctx, "notice", sid, map[string]any{"message": "Capabilities updated — this bot's tools and prompt were refreshed."})
	})
	if failure != nil {
		logger().Warn("Bot capability sync failed for " + sid + ": " + failureText(failure))
	}
}

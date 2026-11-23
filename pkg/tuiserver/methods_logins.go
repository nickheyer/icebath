package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)


// Handler that parses a login method's typed params, runs its body and answers its result or refusal
func loginHandler[P any](parse func(any) (P, error), body func(ctx context.Context, p P) (any, error)) func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	return func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
		p, err := parse(params)
		if err != nil {
			return rpcErr(rid, tuihost.LoginCodeBadParams, err.Error(), nil), nil
		}
		result, err := body(ctx, p)
		if err != nil {
			return loginRefusalFrame(rid, err), nil
		}
		return ok(rid, tuigateway.Plain(result)), nil
	}
}

// Error frame of a refused login request, its code and reason
func loginRefusalFrame(rid any, err error) map[string]any {
	refused := tuihost.LoginFailure(err)
	return rpcErr(rid, refused.Code, refused.Message, map[string]any{"reason": refused.Reason})
}

// ---- sessions ----

// Login sessions of this server, its live session records
type tuiLoginSessions struct{}

// Record key holding a session's own login choices
const loginChoicesKey = "login_choices"

// Login session of a live record, nil when the id names none
func (tuiLoginSessions) LoginSession(ctx context.Context, sessionID string) (*tuihost.LoginSession, error) {
	session := tuihost.SessionRecord(sessionID)
	if session == nil {
		return nil, nil
	}
	return loginSessionOf(ctx, sessionID, session)
}

// Login session of one live record
func loginSessionOf(ctx context.Context, sessionID string, session *tuihost.Session) (*tuihost.LoginSession, error) {
	db, err := sessionLoginDB(ctx, session)
	if err != nil {
		return nil, err
	}
	if profileHome := strOr(session.Get("profile_home")); profileHome != "" {
		ctx = profileRuntimeScope(ctx, profileHome, true)
	}
	out := &tuihost.LoginSession{ID: sessionID, DB: db, StoredID: strOr(session.Get("session_key")), PoolKey: sessionPoolKey(ctx, session),
		Adopt: func(choices provider.SessionLogins) {
			session.Set(loginChoicesKey, choices)
			if a := sessionAgent(session); a != nil {
				a.SetSessionLogins(choices)
			}
		},
		Publish: func(ctx context.Context) { emitSessionInfo(ctx, sessionID, session) },
		Retry:   func(ctx context.Context) error { return retryLastTurn(ctx, sessionID, session) }}
	if a := sessionAgent(session); a != nil {
		out.ActiveLoginID = a.CurrentLoginID()
	}
	return out, nil
}

// Store a session's row lives in, its agent's when built
func sessionLoginDB(ctx context.Context, session *tuihost.Session) (*state.DB, error) {
	if a := sessionAgent(session); a != nil && a.SessionDB() != nil {
		return a.SessionDB(), nil
	}
	return getDB(ctx)
}

// Pool key of a session's current route, the live agent's, else its model override's, else the configured provider's
func sessionPoolKey(ctx context.Context, session *tuihost.Session) string {
	env := provider.LoginEnv(ctx)
	if a := sessionAgent(session); a != nil {
		return provider.RoutePoolKey(env, a.Provider(), a.BaseURL(), a.RequestedProvider())
	}
	if override, isDict := session.Get("model_override").(map[string]any); isDict && strOr(override["provider"]) != "" {
		return provider.RoutePoolKey(env, strOr(override["provider"]), strOr(override["base_url"]), "")
	}
	_, configured := configModelTarget(ctx)
	return provider.LoginPoolKey(env.Config(), configured)
}

// Login choices a session holds, its record's copy, else its stored row's, kept on the record from then on
func sessionLoginChoices(ctx context.Context, session *tuihost.Session, db *state.DB) provider.SessionLogins {
	if held, ok := session.Get(loginChoicesKey).(provider.SessionLogins); ok {
		return held.Clone()
	}
	choices := provider.SessionLogins{}.Clone()
	if key := strOr(session.Get("session_key")); key != "" && db != nil {
		cfg, err := db.SessionModelConfig(ctx, key)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("could not read the logins of session %s: %v", key, err))
		} else {
			choices = provider.SessionLoginsFromModelConfig(cfg)
		}
	}
	session.Set(loginChoicesKey, choices)
	return choices
}

// Context carrying the session's login choices, so its agent build and runtime resolution use them
func withSessionLogin(ctx context.Context, sid string, db *state.DB) context.Context {
	session := tuihost.SessionRecord(sid)
	if session == nil {
		return ctx
	}
	if db == nil {
		db, _ = getDB(ctx)
	}
	return provider.WithSessionLogins(ctx, sessionLoginChoices(ctx, session, db))
}

// Gives a session.create's new record the login choices of the session it names in logins_from
func rememberCreateLogin(ctx context.Context, record *tuihost.Session, params map[string]any) {
	from := strings.TrimSpace(strOr(params["logins_from"]))
	if record == nil || from == "" {
		return
	}
	parent, err := tuiLoginSessions{}.LoginSession(ctx, from)
	if err != nil || parent == nil {
		pkgLog().Warn(fmt.Sprintf("the logins of session %s were not copied to the new session: it is not open here", from))
		return
	}
	choices, err := parent.Choices(ctx)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("the logins of session %s were not copied to the new session: %v", from, err))
		return
	}
	if held, ok := tuihost.SessionRecord(from).Get(loginChoicesKey).(provider.SessionLogins); ok {
		choices = held
	}
	record.Set(loginChoicesKey, choices.Clone())
}

// Writes the login choices and own settings a live session holds into the model config its row gets, a row that holds them already keeping its own
func storeSessionOwnedConfig(ctx context.Context, session *tuihost.Session, modelConfig map[string]any) {
	if held, ok := session.Get(loginChoicesKey).(provider.SessionLogins); ok {
		held.StoreInModelConfig(modelConfig)
	}
	if values := settings.SessionValues(ctx, strOr(session.Get("session_key"))); len(values) > 0 {
		modelConfig[state.SessionSettingsKey] = values
	}
}

// Re-runs a session's last turn: the last exchange is rewound and its text submitted again
func retryLastTurn(ctx context.Context, sid string, session *tuihost.Session) error {
	_, undone, removed, busy, err := rewindLastExchange(ctx, session)
	if busy {
		return &tuihost.LoginError{Code: tuihost.LoginCodeBusy, Reason: "session_busy", Message: "the session is running a turn; retry once it ends"}
	}
	if err != nil {
		return err
	}
	if removed == 0 || undone == nil {
		return errors.New("there is no earlier message to retry")
	}
	text, err := agent.RetryableUserText(undone["content"])
	if err != nil {
		return err
	}
	answer := promptSubmit(ctx, nil, map[string]any{"session_id": sid, "text": text})
	if failure, failed := answer["error"].(map[string]any); failed {
		return errors.New(strOr(failure["message"]))
	}
	return nil
}

// Hands a live agent's exhausted pinned logins to the session's clients as login.exhausted
func wireLoginExhausted(ctx context.Context, sid string, a *agent.AIAgent) {
	a.SetLoginExhaustedSink(func(evCtx context.Context, ev provider.LoginExhaustedEvent) {
		emitEvent(ctx, tuigateway.LoginExhausted.Name(), sid, tuigateway.LoginExhausted.Wire(tuihost.LoginExhaustedWire(ev)))
	})
}

// ---- model switch ----

// Login a /model --login names on the provider the switch resolved
type switchLoginTarget struct {
	provider string
	id       string
	name     string
}

// Login a /model --login names on one provider, @rotate passing through
func findSwitchLogin(ctx context.Context, key, login string) (*switchLoginTarget, error) {
	if login == provider.LoginRotateSelection {
		return &switchLoginTarget{provider: key, id: login, name: login}, nil
	}
	row, err := provider.FindLogin(provider.LoginEnv(ctx), key, login)
	if err != nil {
		return nil, err
	}
	return &switchLoginTarget{provider: key, id: row.ID, name: row.LoginName()}, nil
}

// Resolves a /model --login against the provider the switch landed on, refusing before anything changes
func resolveSwitchLogin(ctx context.Context, result *provider.ModelSwitchResult, login string) (*switchLoginTarget, error) {
	return findSwitchLogin(ctx, provider.RoutePoolKey(provider.LoginEnv(ctx), result.TargetProvider, result.BaseURL, ""), login)
}

// Pins the switched login for the session, or makes it the profile default and drops the session's pin for a --global switch
func pinSwitchLogin(ctx context.Context, sid string, target switchLoginTarget, global bool) error {
	pin := target.id
	if global {
		if _, err := provider.SetLoginDefault(provider.LoginEnv(ctx), target.provider, target.id); err != nil {
			return err
		}
		pin = ""
	}
	_, err := tuihost.SetSessionLogin(ctx, tuiLoginSessions{}, tuigateway.SessionSetLoginParams{SessionID: sid, Provider: target.provider, Login: &pin})
	return err
}

// /model --login <name> without a model, the session's current provider moving onto that login
func switchSessionLoginOnly(ctx context.Context, sid string, session *tuihost.Session, login string, global bool) (map[string]any, error) {
	key := sessionPoolKey(ctx, session)
	if key == "" {
		return nil, errors.New("the session has no provider yet. Pick a model with /model <model> --login " + login)
	}
	target, err := findSwitchLogin(ctx, key, login)
	if err != nil {
		return nil, err
	}
	if err := pinSwitchLogin(ctx, sid, *target, global); err != nil {
		return nil, err
	}
	scope, value := "session", ""
	if global {
		scope = "global"
	}
	if a := sessionAgent(session); a != nil {
		value = a.Model()
	}
	return map[string]any{"value": value, "warning": fmt.Sprintf("%s uses the login %s from the next message (%s).", key, target.name, scope),
		"confirm_required": false, "scope": scope}, nil
}

// ---- model.disconnect compatibility ----

// Removes one login of the provider for model.disconnect, unhandled when the provider holds no login to remove
func disconnectOneLogin(ctx context.Context, rid any, slug, target string, pconfig *provider.Profile) (map[string]any, bool, error) {
	env := provider.LoginEnv(ctx)
	key := provider.LoginPoolKey(env.Config(), slug)
	if target == "" {
		target = provider.DefaultLoginID(env, key)
	}
	if target == "" {
		return nil, false, nil
	}
	removal, err := provider.RemoveLogin(env, key, target)
	if err != nil {
		return loginRefusalFrame(rid, err), true, nil
	}
	remaining := len(provider.ListLogins(env, key).Logins)
	name := slug
	if pconfig != nil {
		name = pconfig.Label()
	}
	return ok(rid, tuigateway.Plain(tuigateway.ModelDisconnectResult{Slug: slug, Name: name, Disconnected: true,
		Login: removal.Login.Name, Remaining: int64(remaining)})), true, nil
}

// session.info pool key and login of a live session, nil before it has a route
func sessionInfoLogin(ctx context.Context, session *tuihost.Session) (string, *tuigateway.SessionLogin) {
	if session == nil {
		return "", nil
	}
	lookup, err := loginSessionOf(ctx, "", session)
	if err != nil {
		return "", nil
	}
	choices := sessionLoginChoices(ctx, session, lookup.DB)
	if profileHome := strOr(session.Get("profile_home")); profileHome != "" {
		ctx = profileRuntimeScope(ctx, profileHome, true)
	}
	return lookup.PoolKey, tuihost.SessionLoginInfo(ctx, lookup.PoolKey, choices, lookup.ActiveLoginID)
}

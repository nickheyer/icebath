package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Refusal on the in-process server's wire shape, its code and message beside the reason
type codedRefusal struct {
	*state.RuntimeStoreError
	code    int
	message string
}

func (r *codedRefusal) WireCode() int       { return r.code }
func (r *codedRefusal) WireMessage() string { return r.message }
func (r *codedRefusal) Unwrap() error       { return r.RuntimeStoreError }

// Refusal of a failed login request, an authority refusal passing through as it is
func loginRefusalOf(err error) error {
	var store *state.RuntimeStoreError
	if errors.As(err, &store) {
		return err
	}
	refused := tuihost.LoginFailure(err)
	detail := refused.Message
	refusal := state.NewRuntimeStoreError(refused.Reason, nil, &detail)
	refusal.Cause = err
	return &codedRefusal{RuntimeStoreError: refusal, code: refused.Code, message: refused.Message}
}

// Agent that reports the login its requests go out with
type loginReportingAgent interface {
	CurrentLoginID() string
}

// Agent that reports the provider a route asked for
type requestedProviderAgent interface {
	RequestedProvider() string
}

// Login sessions of an authority as one principal sees them, the session's owner when no principal is given
type authorityLoginSessions struct {
	a     *SessionAuthority
	actor *Principal
	// Reads only need session:read, writes session:control
	readOnly bool
}

// Principal that may act on any session of the authority, used to restore a session before its owner is known
func operatorPrincipal(a *SessionAuthority) Principal {
	return Principal{Subject: HostOwnerSubject(), ProfileID: a.ProfileID,
		Capabilities: map[string]bool{"session:read": true, "session:control": true, "session:submit": true, "session:operator": true}}
}

// Principal of a session's owner, the operator when its source names none
func ownerPrincipal(a *SessionAuthority, live *LiveSession) Principal {
	actor := operatorPrincipal(a)
	if live != nil && live.Source != nil && live.Source.UserID != nil && *live.Source.UserID != "" {
		actor.Subject = *live.Source.UserID
	}
	return actor
}

// Login session the id names, nil when the authority holds no such session
func (h authorityLoginSessions) LoginSession(ctx context.Context, sessionID string) (*tuihost.LoginSession, error) {
	a := h.a
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
	actor := operatorPrincipal(a)
	if h.actor != nil {
		actor = *h.actor
	}
	capability := "session:control"
	if h.readOnly {
		capability = "session:read"
	}
	if err := a.Authorize(ctx, actor, ref, capability); err != nil {
		if isRuntimeRefusal(err, "not_found") {
			return nil, nil
		}
		return nil, err
	}
	live, ok := a.live(sessionID)
	if !ok {
		return nil, nil
	}
	if h.actor == nil {
		actor = ownerPrincipal(a, live)
	}
	stored, err := a.PhysicalTarget(ctx, ref)
	if err != nil {
		return nil, err
	}
	session := &tuihost.LoginSession{ID: sessionID, DB: a.DB, StoredID: stored,
		PoolKey: sessionRoutePoolKey(profileRuntimeScope(ctx, a.ProfileID, true), a, ref, live),
		Publish: func(ctx context.Context) { publishLoginInfo(ctx, a, ref) },
		Retry:   func(ctx context.Context) error { return retryLastTurn(ctx, a, actor, ref) }}
	if agent, err := a.agent(ref); err == nil {
		if reporting, ok := agent.(loginReportingAgent); ok {
			session.ActiveLoginID = reporting.CurrentLoginID()
		}
	}
	return session, nil
}

// Pool key of the route a session's next turn runs, its resident agent's, else its frozen policy's, else the profile's
func sessionRoutePoolKey(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) string {
	env := provider.LoginEnv(ctx)
	if agent, err := a.agent(ref); err == nil && agent != nil {
		name, baseURL, requested := "", "", ""
		if held, ok := agent.(agentProvider); ok {
			name = held.Provider()
		}
		if held, ok := agent.(agentBaseURL); ok {
			baseURL = held.BaseURL()
		}
		if held, ok := agent.(requestedProviderAgent); ok {
			requested = held.RequestedProvider()
		}
		if name != "" {
			return provider.RoutePoolKey(env, name, baseURL, requested)
		}
	}
	if policy, err := policyForSource(ctx, a.Runner, live.Source); err == nil && policy != nil {
		block, _ := policy.frozenConfig()["model"].(map[string]any)
		if name := mapText(block, "provider"); name != "" && name != "auto" {
			return provider.RoutePoolKey(env, name, mapText(block, "base_url"), "")
		}
	}
	if name := strings.TrimSpace(settings.ModelProvider.Get(ctx)); name != "" && name != "auto" {
		return provider.RoutePoolKey(env, name, strings.TrimSpace(settings.ModelBaseURL.Get(ctx)), "")
	}
	return ""
}

// Pool key and login of a session for session.info, nil login before the session has a route
func sessionInfoLogin(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, activeID string) (string, *tuigateway.SessionLogin) {
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	poolKey := sessionRoutePoolKey(scoped, a, ref, live)
	if poolKey == "" {
		return "", nil
	}
	stored, err := a.PhysicalTarget(ctx, ref)
	if err != nil {
		return poolKey, nil
	}
	cfg, err := a.DB.SessionModelConfig(ctx, stored)
	if err != nil {
		return poolKey, nil
	}
	return poolKey, tuihost.SessionLoginInfo(scoped, poolKey, provider.SessionLoginsFromModelConfig(cfg), activeID)
}

// Republishes a session's info to its viewers after its logins changed
func publishLoginInfo(ctx context.Context, a *SessionAuthority, ref SessionRef) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return
	}
	info, err := localSessionInfo(ctx, a, ref)
	if err != nil {
		pkgLog().Debug("session.info after a login change failed: " + pyExceptionRepr(err))
		return
	}
	live.EventStream.Publish(ref.SessionID, info, "session.info", nil)
}

// Re-runs a session's last turn as actor: the newest user turn is rewound and its text submitted again
func retryLastTurn(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef) error {
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return err
	}
	busy := &tuihost.LoginError{Code: tuihost.LoginCodeBusy, Reason: "session_busy", Message: "the session is running a turn; retry once it ends"}
	if handle.ExecutionState == "running" {
		return busy
	}
	text, err := lastAdmissionText(ctx, a, ref)
	if err != nil {
		return err
	}
	if text == "" {
		return errors.New("there is no earlier message to retry")
	}
	if _, _, err := rewindLastUserTurn(ctx, a, actor, ref, text); err != nil {
		if isRuntimeRefusal(err, "session_busy") {
			return busy
		}
		return err
	}
	_, err = a.Submit(ctx, actor, Submission{RequestID: "login-retry:" + uuid.NewString(), Ref: ref, Payload: map[string]any{"text": text},
		Intent: "queue"}, SubmitOptions{})
	return err
}

// Text of the session's latest submitted input, the turn a retry runs again whether or not it reached the transcript
func lastAdmissionText(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	rows, err := a.DB.ListSessionAdmissions(ctx, ref.SessionID, false)
	if err != nil {
		return "", err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		payload, _ := pyjson.Plain(rows[i].GetOr("payload", nil)).(map[string]any)
		if text, _ := payload["text"].(string); strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return "", nil
}

// Login verb that parses its typed params, runs the shared login code in the owning profile off the loop and answers its result or refusal
func loginVerb[P any, R any](c *AuthorityConnection, parse func(any) (P, error), write bool,
	body func(ctx context.Context, sessions tuihost.LoginSessions, p P) (R, error)) controlHandler {
	return func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
		if write {
			if _, err := authorizeSettingsWrite(ctx, c, SessionRef{ProfileID: ref.ProfileID}, params); err != nil {
				return nil, err
			}
		} else if _, err := authorizeSettingsRead(ctx, c, SessionRef{ProfileID: ref.ProfileID}, params); err != nil {
			return nil, err
		}
		plain, _ := pyjson.Plain(params).(map[string]any)
		p, err := parse(plain)
		if err != nil {
			field, detail := "params", err.Error()
			return nil, state.NewRuntimeStoreError("invalid_params", &field, &detail)
		}
		actor := c.actor
		sessions := authorityLoginSessions{a: c.authority, actor: &actor, readOnly: !write}
		result, err := forwardOffThread(ctx, func() (R, error) {
			return body(profileRuntimeScope(ctx, c.authority.ProfileID, true), sessions, p)
		})
		if err != nil {
			return nil, loginRefusalOf(err)
		}
		return tuigateway.Plain(result), nil
	}
}

// Login and session login verbs of a connection keyed by method name
func loginHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"login.list":    loginVerb(c, tuigateway.LoginList.Params, false, tuihost.ListLogins),
		"login.add_key": loginVerb(c, tuigateway.LoginAddKey.Params, true, tuihost.AddKeyLogin),
		"login.oauth_start": loginVerb(c, tuigateway.LoginOAuthStart.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginOAuthStartParams) (tuigateway.LoginOAuthStartResult, error) {
				return tuihost.StartLoginOAuth(ctx, p)
			}),
		"login.oauth_poll": loginVerb(c, tuigateway.LoginOAuthPoll.Params, true, tuihost.PollLoginOAuth),
		"login.link":       loginVerb(c, tuigateway.LoginLink.Params, true, tuihost.LinkLogin),
		"login.rename": loginVerb(c, tuigateway.LoginRename.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginRenameParams) (tuigateway.LoginResult, error) {
				return tuihost.RenameLogin(ctx, p)
			}),
		"login.remove": loginVerb(c, tuigateway.LoginRemove.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginRemoveParams) (tuigateway.LoginRemoveResult, error) {
				return tuihost.RemoveLogin(ctx, p)
			}),
		"login.set_default": loginVerb(c, tuigateway.LoginSetDefault.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginSetDefaultParams) (tuigateway.LoginSetDefaultResult, error) {
				return tuihost.SetLoginDefault(ctx, p)
			}),
		"login.set_rotation": loginVerb(c, tuigateway.LoginSetRotation.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginSetRotationParams) (tuigateway.LoginResult, error) {
				return tuihost.SetLoginRotation(ctx, p)
			}),
		"login.convert": loginVerb(c, tuigateway.LoginConvert.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginConvertParams) (tuigateway.LoginResult, error) {
				return tuihost.ConvertLogin(ctx, p)
			}),
		"login.set_priority": loginVerb(c, tuigateway.LoginSetPriority.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginSetPriorityParams) (tuigateway.LoginResult, error) {
				return tuihost.SetLoginPriority(ctx, p)
			}),
		"login.reset": loginVerb(c, tuigateway.LoginReset.Params, true,
			func(ctx context.Context, _ tuihost.LoginSessions, p tuigateway.LoginResetParams) (tuigateway.LoginResetResult, error) {
				return tuihost.ResetLogin(ctx, p)
			}),
		"session.set_login":  loginVerb(c, tuigateway.SessionSetLogin.Params, true, tuihost.SetSessionLogin),
		"session.login_wait": loginVerb(c, tuigateway.SessionLoginWait.Params, true, tuihost.SessionLoginWait),
	}
}

// Gives a session just created the login choices of the session logins_from names, which the creator must be able to read
func inheritLogins(ctx context.Context, c *AuthorityConnection, from string, created SessionRef) error {
	a := c.authority
	parent := SessionRef{ProfileID: a.ProfileID, SessionID: from}
	if err := a.Authorize(ctx, c.actor, parent, "session:read"); err != nil {
		return err
	}
	source, err := a.PhysicalTarget(ctx, parent)
	if err != nil {
		return err
	}
	cfg, err := a.DB.SessionModelConfig(ctx, source)
	if err != nil {
		return err
	}
	choices := provider.SessionLoginsFromModelConfig(cfg)
	if choices.Empty() {
		return nil
	}
	target, err := a.PhysicalTarget(ctx, created)
	if err != nil {
		return err
	}
	return a.DB.UpdateSessionModelConfig(ctx, target, func(cfg map[string]any) bool {
		choices.StoreInModelConfig(cfg)
		return true
	})
}

// Arms again the login waits of an authority's sessions that a previous run of the service had armed
func rearmLoginWaits(ctx context.Context, a *SessionAuthority) {
	tuihost.RearmLoginWaits(profileRuntimeScope(ctx, a.ProfileID, true), authorityLoginSessions{a: a}, a.DB)
}

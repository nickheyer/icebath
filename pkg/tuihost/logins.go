package tuihost

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Wire codes of refused login requests, the background service carrying the reason beside them
const (
	LoginCodeBadParams       = 4000
	LoginCodeMissing         = 4001
	LoginCodeManaged         = 4006
	LoginCodeUnknownProvider = 4010
	LoginCodeAuthType        = 4011
	LoginCodeBadValue        = 4012
	LoginCodeUnknownLogin    = 4013
	LoginCodeBadName         = 4014
	LoginCodeUnknownJob      = 4015
	LoginCodeNotWaiting      = 4016
	LoginCodeUnknownSession  = 4017
	LoginCodeBusy            = 4018
	LoginCodeStore           = 5140
)

// LoginError is a refused login request with its wire code and the reason the background service reports
type LoginError struct {
	Code    int
	Reason  string
	Message string
}

func (e *LoginError) Error() string { return e.Message }

// Refusal of one code and reason
func loginRefusal(code int, reason, format string, args ...any) *LoginError {
	return &LoginError{Code: code, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// LoginFailure is the refusal of a failed provider login call, its fault kind picking the code and a store failure otherwise
func LoginFailure(err error) *LoginError {
	var refused *LoginError
	if errors.As(err, &refused) {
		return refused
	}
	var fault *provider.LoginFault
	if errors.As(err, &fault) {
		switch fault.Kind {
		case provider.LoginFaultUnknown:
			return &LoginError{Code: LoginCodeUnknownLogin, Reason: fault.Kind, Message: err.Error()}
		case provider.LoginFaultName:
			return &LoginError{Code: LoginCodeBadName, Reason: fault.Kind, Message: err.Error()}
		case provider.LoginFaultAuthType:
			return &LoginError{Code: LoginCodeAuthType, Reason: fault.Kind, Message: err.Error()}
		case provider.LoginFaultNotWaiting:
			return &LoginError{Code: LoginCodeNotWaiting, Reason: fault.Kind, Message: err.Error()}
		}
		return &LoginError{Code: LoginCodeBadValue, Reason: provider.LoginFaultValue, Message: err.Error()}
	}
	return &LoginError{Code: LoginCodeStore, Reason: "store_unavailable", Message: "the logins store could not be read or written: " + err.Error()}
}

// LoginSession is one session a login request names, as the backend that owns it sees it
type LoginSession struct {
	// Id the client named
	ID string
	// Store holding the session row its login choices live on
	DB *state.SessionDB
	// Row the choices live on
	StoredID string
	// Pool key of the session's current route, empty before it has one
	PoolKey string
	// Login the live agent sends requests with, empty without one
	ActiveLoginID string
	// Hands the new choices to a live agent, which moves onto them at its next turn start
	Adopt func(provider.SessionLogins)
	// Tells the session's viewers its info changed
	Publish func(ctx context.Context)
	// Re-runs the session's last turn
	Retry func(ctx context.Context) error
}

// LoginSessions finds the session a login request names on one backend
type LoginSessions interface {
	LoginSession(ctx context.Context, sessionID string) (*LoginSession, error)
}

// Choices reads the session's own login choices from its stored row
func (s *LoginSession) Choices(ctx context.Context) (provider.SessionLogins, error) {
	if s.DB == nil || s.StoredID == "" {
		return provider.SessionLogins{}.Clone(), nil
	}
	cfg, err := s.DB.SessionModelConfig(ctx, s.StoredID)
	if err != nil {
		return provider.SessionLogins{}.Clone(), err
	}
	return provider.SessionLoginsFromModelConfig(cfg), nil
}

// Stores one pool key's pin and exhaustion override, the live agent taking them at its next turn start
func (s *LoginSession) storeChoice(ctx context.Context, pool string, pin, onExhausted *string) (provider.SessionLogins, error) {
	var stored provider.SessionLogins
	if s.DB == nil || s.StoredID == "" {
		current, _ := s.Choices(ctx)
		stored = current.With(pool, pin, onExhausted)
	} else if err := s.DB.UpdateSessionModelConfig(ctx, s.StoredID, func(cfg map[string]any) bool {
		current := provider.SessionLoginsFromModelConfig(cfg)
		stored = current.With(pool, pin, onExhausted)
		stored.StoreInModelConfig(cfg)
		return current.Key() != stored.Key()
	}); err != nil {
		return stored, LoginFailure(err)
	}
	if s.Adopt != nil {
		s.Adopt(stored)
	}
	return stored, nil
}

// ---- wire shapes ----

// LoginTime is the RFC 3339 time of an epoch, nil when unknown
func LoginTime(epoch float64) *string {
	if epoch <= 0 {
		return nil
	}
	text := time.Unix(int64(epoch), 0).UTC().Format(time.RFC3339)
	return &text
}

// WireLogin is the wire row of one login
func WireLogin(l provider.Login) tuigateway.LoginRow {
	row := tuigateway.LoginRow{ID: l.ID, Provider: l.Provider, Name: l.Name, Label: l.Label, AuthType: l.AuthType,
		Origin: tuigateway.LoginOrigin(l.Origin), Source: l.Source, Account: l.Account, Fingerprint: l.Fingerprint,
		Status: tuigateway.LoginStatus(l.Status), ResetsAt: LoginTime(l.ResetsAt), Reason: l.Reason, Priority: int64(l.Priority),
		Default: l.Default, Rotate: l.Rotate, Session: l.Session, Active: l.Active, SharedWith: l.SharedWith}
	for _, w := range l.Usage {
		window := tuigateway.ProviderUsageWindow{Label: w.Label, UsedPercent: w.UsedPercent, Scope: tuigateway.ProviderUsageScope(w.Scope)}
		if w.ResetsAt != nil {
			text := w.ResetsAt.UTC().Format(time.RFC3339)
			window.ResetsAt = &text
		}
		row.Usage = append(row.Usage, window)
	}
	return row
}

// Wire group of one provider's logins
func wireLoginGroup(g provider.LoginGroup) tuigateway.LoginProviderGroup {
	out := tuigateway.LoginProviderGroup{Provider: g.Provider, Slug: g.Slug, Name: g.Label, Default: g.Default, Strategy: g.Strategy,
		OnExhausted: g.OnExhausted, Logins: []tuigateway.LoginRow{},
		Selection: tuigateway.LoginSelection{Scope: tuigateway.LoginScope(g.Choice.Scope), Login: g.Choice.LoginID, Rotate: g.Choice.Rotate, Missing: g.Choice.Missing}}
	for _, l := range g.Logins {
		if l.ID == g.Choice.LoginID {
			out.Selection.Name = l.Name
		}
		out.Logins = append(out.Logins, WireLogin(l))
	}
	return out
}

// LoginExhaustedWire is the login.exhausted payload of an exhausted pinned login
func LoginExhaustedWire(ev provider.LoginExhaustedEvent) tuigateway.LoginExhaustedPayload {
	payload := tuigateway.LoginExhaustedPayload{SessionID: ev.SessionID, Provider: ev.Provider, Slug: ev.Slug,
		Login: tuigateway.LoginRef{ID: ev.Login.ID, Name: ev.Login.Name}, ResetsAt: LoginTime(ev.ResetsAt), Reason: ev.Reason,
		Ready: []tuigateway.LoginRef{}, Rotation: ev.Rotation, OnExhausted: ev.OnExhausted}
	if payload.OnExhausted == "" {
		payload.OnExhausted = provider.LoginOnExhaustedFail
	}
	for _, r := range ev.Ready {
		payload.Ready = append(payload.Ready, tuigateway.LoginRef{ID: r.ID, Name: r.Name})
	}
	return payload
}

// SessionLoginInfo is session.info's login for a session's current pool key, nil before the session has a route
func SessionLoginInfo(ctx context.Context, poolKey string, choices provider.SessionLogins, activeID string) *tuigateway.SessionLogin {
	if poolKey == "" {
		return nil
	}
	env := provider.LoginEnv(provider.WithSessionLogins(ctx, choices))
	group := provider.ListLogins(env, poolKey)
	info := &tuigateway.SessionLogin{Provider: poolKey, Slug: group.Slug, Scope: tuigateway.LoginScope(group.Choice.Scope),
		Rotate: group.Choice.Rotate, Missing: group.Choice.Missing}
	if reaction, scope := provider.SessionOnExhausted(env, poolKey); scope == provider.LoginScopeSession {
		info.OnExhausted = reaction
	}
	pick := activeID
	if pick == "" {
		pick = group.Choice.LoginID
	}
	for _, l := range group.Logins {
		if (pick != "" && l.ID == pick) || (pick == "" && l.Active) {
			info.ID, info.Name = l.ID, l.Name
		}
	}
	if info.ID == "" && len(group.Logins) == 0 {
		return nil
	}
	return info
}

// ---- requests ----

// Refuses credential writes on a managed install
func managedRefusal(ctx context.Context) error {
	if config.IsManaged(ctx, home.Dir(ctx)) {
		return loginRefusal(LoginCodeManaged, "permission_denied", "managed install, credentials are read-only")
	}
	return nil
}

// Pool key a client's provider names, the session's current one when it names none
func requestPoolKey(ctx context.Context, named string, session *LoginSession) (string, error) {
	if strings.TrimSpace(named) != "" {
		return provider.LoginPoolKey(provider.LoginEnv(ctx).Config(), named), nil
	}
	if session != nil && session.PoolKey != "" {
		return session.PoolKey, nil
	}
	return "", loginRefusal(LoginCodeMissing, "invalid_params", "name a provider; the session has no model route yet")
}

// Session a request names, nil when it names none
func requestSession(ctx context.Context, sessions LoginSessions, sessionID string) (*LoginSession, error) {
	if sessionID == "" || sessions == nil {
		return nil, nil
	}
	session, err := sessions.LoginSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, loginRefusal(LoginCodeUnknownSession, "not_found", "session not found: %s", sessionID)
	}
	return session, nil
}

// ListLogins answers login.list, every provider's logins with the session's, the default's and rotation's markers
func ListLogins(ctx context.Context, sessions LoginSessions, p tuigateway.LoginListParams) (tuigateway.LoginListResult, error) {
	result := tuigateway.LoginListResult{Providers: []tuigateway.LoginProviderGroup{}}
	session, err := requestSession(ctx, sessions, p.SessionID)
	if err != nil {
		return result, err
	}
	choices := provider.SessionLogins{}.Clone()
	if session != nil {
		if choices, err = session.Choices(ctx); err != nil {
			return result, LoginFailure(err)
		}
	}
	env := provider.LoginEnv(provider.WithSessionLogins(ctx, choices))
	filter := ""
	if p.Provider != "" {
		filter = provider.LoginPoolKey(env.Config(), p.Provider)
	}
	for _, group := range provider.ListAllLogins(env, filter) {
		wired := wireLoginGroup(group)
		if session != nil && group.Provider == session.PoolKey && session.ActiveLoginID != "" {
			for i := range wired.Logins {
				wired.Logins[i].Active = wired.Logins[i].ID == session.ActiveLoginID
			}
		}
		result.Providers = append(result.Providers, wired)
	}
	if session == nil {
		return result, nil
	}
	listed := &tuigateway.LoginListSession{SessionID: p.SessionID, PoolKey: session.PoolKey, Pins: choices.Pins, OnExhausted: choices.OnExhausted,
		Login: SessionLoginInfo(ctx, session.PoolKey, choices, session.ActiveLoginID)}
	if session.PoolKey != "" {
		listed.Slug = provider.LoginPoolSlug(env.Config(), session.PoolKey)
	}
	if wait := pendingLoginWait(ctx, session); wait != nil {
		listed.Wait = wait
	}
	result.Session = listed
	return result, nil
}

// Pins a login a request just stored for the request's session, returning why it could not
func pinForRequestSession(ctx context.Context, sessions LoginSessions, sessionID, poolKey, loginID string) string {
	if sessionID == "" || loginID == "" {
		return ""
	}
	login := loginID
	result, err := SetSessionLogin(ctx, sessions, tuigateway.SessionSetLoginParams{SessionID: sessionID, Provider: poolKey, Login: &login})
	if err != nil {
		return err.Error()
	}
	return result.Warning
}

// Refusal of an API key for a provider that signs in another way, nil when keys fit
func apiKeyProviderRefusal(ctx context.Context, slug string) error {
	key := strings.ToLower(strings.TrimSpace(slug))
	cfg := provider.LoginEnv(ctx).Config()
	if key == "openrouter" || provider.IsCustomPoolKey(cfg, provider.LoginPoolKey(cfg, key)) {
		return nil
	}
	row := provider.RegistryRow(ctx, key)
	if row == nil {
		return loginRefusal(LoginCodeUnknownProvider, "unknown_provider", "unknown provider: %s", slug)
	}
	if row.AuthType != provider.AuthTypeAPIKey {
		return loginRefusal(LoginCodeAuthType, "wrong_auth_type", "%s does not use an API key", row.Label())
	}
	return nil
}

// AddKeyLogin answers login.add_key, an API key stored as an owned login and never written to .env
func AddKeyLogin(ctx context.Context, sessions LoginSessions, p tuigateway.LoginAddKeyParams) (tuigateway.LoginResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginResult{}, err
	}
	if strings.TrimSpace(p.Provider) == "" || strings.TrimSpace(p.APIKey) == "" {
		return tuigateway.LoginResult{}, loginRefusal(LoginCodeMissing, "invalid_params", "provider and api_key are required")
	}
	if err := apiKeyProviderRefusal(ctx, p.Provider); err != nil {
		return tuigateway.LoginResult{}, err
	}
	poolKey := provider.LoginPoolKey(provider.LoginEnv(ctx).Config(), p.Provider)
	login, created, err := provider.AddKeyLogin(provider.LoginEnv(ctx), poolKey, p.APIKey,
		provider.AddLoginOptions{Name: strings.TrimSpace(p.Name), Rotate: p.Rotate, MakeDefault: p.MakeDefault})
	if err != nil {
		return tuigateway.LoginResult{}, LoginFailure(err)
	}
	result := tuigateway.LoginResult{Login: WireLogin(login), Created: created}
	if p.UseForSession {
		result.Warning = pinForRequestSession(ctx, sessions, p.SessionID, login.Provider, login.ID)
	}
	return result, nil
}

// Session a hosted OAuth sign in pins its login to once it finishes, by job id
var loginOAuthSessions sync.Map

// StartLoginOAuth answers login.oauth_start, a device code or PKCE sign in running in the background whose link the client opens
func StartLoginOAuth(ctx context.Context, p tuigateway.LoginOAuthStartParams) (tuigateway.LoginOAuthStartResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginOAuthStartResult{}, err
	}
	if strings.TrimSpace(p.Provider) == "" {
		return tuigateway.LoginOAuthStartResult{}, loginRefusal(LoginCodeMissing, "invalid_params", "provider is required")
	}
	job, err := provider.StartLoginOAuth(provider.LoginEnv(ctx), p.Provider,
		provider.LoginOAuthOptions{Name: strings.TrimSpace(p.Name), Rotate: p.Rotate, MakeDefault: p.MakeDefault, Browser: p.Browser})
	if err != nil {
		return tuigateway.LoginOAuthStartResult{}, LoginFailure(err)
	}
	snap := job.Snapshot()
	if p.UseForSession && p.SessionID != "" {
		loginOAuthSessions.Store(snap.ID, p.SessionID)
	}
	return tuigateway.LoginOAuthStartResult{JobID: snap.ID, Provider: snap.Provider, Loopback: snap.Loopback}, nil
}

// PollLoginOAuth answers login.oauth_poll, the sign in's output, prompt and result, answering or cancelling it first
func PollLoginOAuth(ctx context.Context, sessions LoginSessions, p tuigateway.LoginOAuthPollParams) (tuigateway.LoginOAuthPollResult, error) {
	job := provider.LoginOAuthJobByID(p.JobID)
	if job == nil {
		return tuigateway.LoginOAuthPollResult{}, loginRefusal(LoginCodeUnknownJob, "not_found", "unknown or expired sign in: %s", p.JobID)
	}
	if p.Cancel {
		job.Cancel()
	}
	if p.Input != nil {
		if err := job.Answer(*p.Input); err != nil {
			return tuigateway.LoginOAuthPollResult{}, LoginFailure(err)
		}
	}
	snap := job.Snapshot()
	result := tuigateway.LoginOAuthPollResult{JobID: snap.ID, State: tuigateway.LoginOAuthState(snap.State), Lines: snap.Lines,
		URL: snap.URL, UserCode: snap.UserCode, Prompt: snap.Prompt, Error: snap.Error, Open: snap.Open, Loopback: snap.Loopback}
	if result.Lines == nil {
		result.Lines = []string{}
	}
	if snap.Login != nil {
		wired := WireLogin(*snap.Login)
		result.Login = &wired
		if sessionID, pending := loginOAuthSessions.LoadAndDelete(snap.ID); pending {
			result.Warning = pinForRequestSession(ctx, sessions, sessionID.(string), snap.Login.Provider, snap.Login.ID)
		}
	} else if snap.State == provider.LoginOAuthFailed || snap.State == provider.LoginOAuthCancelled {
		loginOAuthSessions.Delete(snap.ID)
	}
	return result, nil
}

// LinkLogin answers login.link, a linked login over an env var or another tool's login
func LinkLogin(ctx context.Context, sessions LoginSessions, p tuigateway.LoginLinkParams) (tuigateway.LoginResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginResult{}, err
	}
	env := provider.LoginEnv(ctx)
	poolKey := provider.LoginPoolKey(env.Config(), p.Provider)
	var login provider.Login
	var created bool
	var err error
	switch {
	case p.EnvVar != "":
		login, created, err = provider.LinkEnvLogin(env, poolKey, p.EnvVar, strings.TrimSpace(p.Name))
	case p.Source != "":
		login, created, err = provider.LinkExternalLogin(env, poolKey, p.Source)
	default:
		return tuigateway.LoginResult{}, loginRefusal(LoginCodeMissing, "invalid_params", "env_var or source is required")
	}
	if err != nil {
		return tuigateway.LoginResult{}, LoginFailure(err)
	}
	result := tuigateway.LoginResult{Login: WireLogin(login), Created: created}
	if p.UseForSession {
		result.Warning = pinForRequestSession(ctx, sessions, p.SessionID, login.Provider, login.ID)
	}
	return result, nil
}

// RenameLogin answers login.rename
func RenameLogin(ctx context.Context, p tuigateway.LoginRenameParams) (tuigateway.LoginResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginResult{}, err
	}
	env := provider.LoginEnv(ctx)
	login, err := provider.RenameLogin(env, provider.LoginPoolKey(env.Config(), p.Provider), p.Login, p.Name)
	if err != nil {
		return tuigateway.LoginResult{}, LoginFailure(err)
	}
	return tuigateway.LoginResult{Login: WireLogin(login)}, nil
}

// RemoveLogin answers login.remove, one login and only its own source
func RemoveLogin(ctx context.Context, p tuigateway.LoginRemoveParams) (tuigateway.LoginRemoveResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginRemoveResult{}, err
	}
	env := provider.LoginEnv(ctx)
	removal, err := provider.RemoveLogin(env, provider.LoginPoolKey(env.Config(), p.Provider), p.Login)
	if err != nil {
		return tuigateway.LoginRemoveResult{}, LoginFailure(err)
	}
	return tuigateway.LoginRemoveResult{Login: WireLogin(removal.Login), Cleaned: removal.Cleaned, Hints: removal.Hints, Default: removal.NewDefault}, nil
}

// SetLoginDefault answers login.set_default, the profile default login, exhaustion policy and rotation strategy
func SetLoginDefault(ctx context.Context, p tuigateway.LoginSetDefaultParams) (tuigateway.LoginSetDefaultResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginSetDefaultResult{}, err
	}
	env := provider.LoginEnv(ctx)
	key := provider.LoginPoolKey(env.Config(), p.Provider)
	if key == "" {
		return tuigateway.LoginSetDefaultResult{}, loginRefusal(LoginCodeMissing, "invalid_params", "provider is required")
	}
	if p.Login != nil {
		if _, err := provider.SetLoginDefault(env, key, *p.Login); err != nil {
			return tuigateway.LoginSetDefaultResult{}, LoginFailure(err)
		}
	}
	if p.OnExhausted != "" {
		if err := provider.SetLoginOnExhausted(env, key, p.OnExhausted); err != nil {
			return tuigateway.LoginSetDefaultResult{}, LoginFailure(err)
		}
	}
	if p.Strategy != "" {
		if err := provider.SetLoginStrategy(env, key, p.Strategy); err != nil {
			return tuigateway.LoginSetDefaultResult{}, LoginFailure(err)
		}
	}
	return tuigateway.LoginSetDefaultResult{Provider: key, Default: provider.LoginDefault(ctx, key),
		OnExhausted: provider.LoginOnExhausted(ctx, key), Strategy: provider.GetPoolStrategy(ctx, key)}, nil
}

// SetLoginRotation answers login.set_rotation
func SetLoginRotation(ctx context.Context, p tuigateway.LoginSetRotationParams) (tuigateway.LoginResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginResult{}, err
	}
	env := provider.LoginEnv(ctx)
	login, err := provider.SetLoginRotation(env, provider.LoginPoolKey(env.Config(), p.Provider), p.Login, p.Rotate)
	if err != nil {
		return tuigateway.LoginResult{}, LoginFailure(err)
	}
	return tuigateway.LoginResult{Login: WireLogin(login)}, nil
}

// ConvertLogin answers login.convert, an env linked key moved into an owned login on explicit request
func ConvertLogin(ctx context.Context, p tuigateway.LoginConvertParams) (tuigateway.LoginResult, error) {
	if err := managedRefusal(ctx); err != nil {
		return tuigateway.LoginResult{}, err
	}
	env := provider.LoginEnv(ctx)
	login, err := provider.ConvertLinkedLogin(env, provider.LoginPoolKey(env.Config(), p.Provider), p.Login)
	if err != nil {
		return tuigateway.LoginResult{}, LoginFailure(err)
	}
	return tuigateway.LoginResult{Login: WireLogin(login), Created: true}, nil
}

// SetSessionLogin answers session.set_login, one provider's login pin and exhaustion reaction stored with the session
func SetSessionLogin(ctx context.Context, sessions LoginSessions, p tuigateway.SessionSetLoginParams) (tuigateway.SessionSetLoginResult, error) {
	result := tuigateway.SessionSetLoginResult{SessionID: p.SessionID}
	if p.SessionID == "" {
		return result, loginRefusal(LoginCodeMissing, "invalid_params", "session_id is required")
	}
	session, err := requestSession(ctx, sessions, p.SessionID)
	if err != nil {
		return result, err
	}
	if session == nil {
		return result, loginRefusal(LoginCodeUnknownSession, "not_found", "session not found: %s", p.SessionID)
	}
	poolKey, err := requestPoolKey(ctx, p.Provider, session)
	if err != nil {
		return result, err
	}
	env := provider.LoginEnv(ctx)
	result.Provider, result.Slug, result.Current = poolKey, provider.LoginPoolSlug(env.Config(), poolKey), poolKey == session.PoolKey
	var pin *string
	var row provider.PooledCredential
	if p.Login != nil {
		value := strings.TrimSpace(*p.Login)
		switch value {
		case "":
			result.Cleared = true
		case provider.LoginRotateSelection:
			if !slices.ContainsFunc(provider.ListLogins(env, poolKey).Logins, func(l provider.Login) bool { return l.Rotate }) {
				return result, loginRefusal(LoginCodeBadValue, provider.LoginFaultValue,
					"no %s login is in rotation yet; mark the logins to rotate through first", poolKey)
			}
			result.Rotate = true
		default:
			found, err := provider.FindLogin(env, poolKey, value)
			if err != nil {
				return result, LoginFailure(err)
			}
			row, value = found, found.ID
		}
		pin = &value
	}
	if p.OnExhausted != nil {
		switch v := strings.ToLower(strings.TrimSpace(*p.OnExhausted)); v {
		case "", provider.LoginOnExhaustedFail, provider.LoginOnExhaustedRotate:
		default:
			return result, loginRefusal(LoginCodeBadValue, provider.LoginFaultValue, "on_exhausted takes rotate, fail or nothing, got %q", v)
		}
	}
	choices, err := session.storeChoice(ctx, poolKey, pin, p.OnExhausted)
	if err != nil {
		return result, err
	}
	result.OnExhausted = choices.OnExhausted[poolKey]
	if pin == nil {
		result.Rotate = choices.Pins[poolKey] == provider.LoginRotateSelection
	}
	if row.ID != "" {
		group := provider.ListLogins(provider.LoginEnv(provider.WithSessionLogins(ctx, choices)), poolKey)
		for _, l := range group.Logins {
			if l.ID == row.ID {
				wired := WireLogin(l)
				result.Login = &wired
			}
		}
	}
	if result.Current {
		result.SessionLogin = SessionLoginInfo(ctx, poolKey, choices, "")
	}
	if p.Retry {
		cancelLoginWait(ctx, session)
		if session.Retry == nil {
			result.Warning = "this backend cannot retry the turn; send it again"
		} else if err := session.Retry(ctx); err != nil {
			result.Warning = err.Error()
		} else {
			result.Retried = true
		}
	}
	if session.Publish != nil {
		session.Publish(ctx)
	}
	return result, nil
}

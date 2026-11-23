package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Session login choices the agent selects its pools under, shared read only
func (a *AIAgent) sessionLogins() provider.SessionLogins {
	if held := a.loginChoices.Load(); held != nil {
		return *held
	}
	return provider.SessionLogins{}
}

// SessionLogins copies the session's own login choices, its pins and on_exhausted overrides per pool key
func (a *AIAgent) SessionLogins() provider.SessionLogins { return a.sessionLogins().Clone() }

// SetSessionLogins replaces the session's login choices; the live pool moves onto them at the start of the next turn, never under a running one
func (a *AIAgent) SetSessionLogins(choices provider.SessionLogins) {
	held := choices.Clone()
	a.loginChoices.Store(&held)
}

// Context carrying the session's login choices unless the context brings its own
func (a *AIAgent) loginContext(ctx context.Context) context.Context {
	if _, held := provider.SessionLoginsFrom(ctx); held {
		return ctx
	}
	return provider.WithSessionLogins(ctx, a.sessionLogins())
}

// Key of the choices and the stored logins the live pool was selected under
func (a *AIAgent) loginPoolKey(ctx context.Context) string {
	return a.sessionLogins().Key() + "#" + provider.LoginStateStamp(agentPoolEnv(ctx))
}

// Marks the live pool as selected under the current choices and stored logins
func (a *AIAgent) noteLoginPoolSelected(ctx context.Context) {
	key := a.loginPoolKey(ctx)
	a.loginPoolSelected.Store(&key)
}

// Moves the live pool onto the session's login choices at a turn start when the choices, a profile default, the rotation set or the stored logins
// changed since it was selected; a pinned login with nothing usable answers its typed refusal so the turn never runs on another account
func (a *AIAgent) syncLoginPool(ctx context.Context) error {
	if a.credentialPool == nil {
		return nil
	}
	key := a.loginPoolKey(ctx)
	if prev := a.loginPoolSelected.Load(); prev != nil && *prev == key {
		return nil
	}
	pool := provider.LoadRoutePoolWith(agentPoolEnv(ctx), agentLoadPool, a.provider, a.baseURL, a.requestedProvider)
	a.loginPoolSelected.Store(&key)
	if pool == nil || !pool.HasCredentials() {
		return nil
	}
	if current := a.CurrentLoginID(); current != "" {
		if entry, kept := pool.Keep(current, a.model); kept {
			a.credentialPool = pool
			inUse := a.apiKey
			if a.apiMode == "anthropic_messages" {
				inUse = a.anthropicAPIKey
			}
			if entry.RuntimeAPIKey() == "" || entry.RuntimeAPIKey() == inUse {
				return nil
			}
			_, err := a.swapCredential(ctx, entry)
			return err
		}
	}
	entry, ok := pool.Select(a.model)
	if ok && !provider.CredentialPoolEntryServesEndpoint(entry, a.baseURL) {
		return fmt.Errorf("the %s login %s serves another endpoint than this session's model", pool.Provider(), entry.LoginName())
	}
	a.credentialPool, a.credentialPoolEntryID = pool, ""
	a.loginExhaustedKey.Store(nil)
	if !ok {
		if err := pool.LoginUnavailable(a.model); err != nil {
			if ev := provider.LoginExhaustion(pool, ""); ev != nil {
				a.reportLoginExhausted(ctx, *ev)
			}
			return err
		}
		return nil
	}
	_, err := a.swapCredential(ctx, entry)
	return err
}

// CurrentLoginID is the id of the login the live pool serves requests with, empty without a pool
func (a *AIAgent) CurrentLoginID() string {
	if a.credentialPoolEntryID != "" {
		return a.credentialPoolEntryID
	}
	if a.credentialPool != nil {
		if cur, ok := a.credentialPool.Current(); ok {
			return cur.ID
		}
	}
	return ""
}

// SetLoginExhaustedSink installs where the agent reports a pinned login that ran out, the host's session event stream
func (a *AIAgent) SetLoginExhaustedSink(sink func(ctx context.Context, ev provider.LoginExhaustedEvent)) {
	a.loginExhaustedSink.Store(&sink)
}

// Pinned login a turn ran out of, described when the turn ends
type pendingExhaustion struct {
	pool         *provider.Pool
	credentialID string
}

// Holds a pinned login that ran out until the turn ends, reported only if the turn fails, since a later retry of the same login may still answer
func (a *AIAgent) noteLoginExhausted(ctx context.Context, pool *provider.Pool, credentialID string) {
	if pool != nil && pool.Pinned() {
		a.loginExhaustedPending.Store(&pendingExhaustion{pool: pool, credentialID: credentialID})
	}
}

// Reports the pinned login a failed turn ran out of with its latest reset time, once per login and reset time, to the host sink, the event callback and the status line
func (a *AIAgent) flushLoginExhausted(ctx context.Context, result map[string]any, err error) {
	pending := a.loginExhaustedPending.Swap(nil)
	if pending == nil || (err == nil && result != nil && result["failed"] != true) {
		return
	}
	if ev := provider.LoginExhaustion(pending.pool, pending.credentialID); ev != nil {
		a.reportLoginExhausted(ctx, *ev)
	}
}

// Sends one exhausted pinned login to the host sink, the event callback and the status line, once per login and reset time
func (a *AIAgent) reportLoginExhausted(ctx context.Context, ev provider.LoginExhaustedEvent) {
	key := fmt.Sprintf("%s:%s:%.0f", ev.Provider, ev.Login.ID, ev.ResetsAt)
	if prev := a.loginExhaustedKey.Swap(&key); prev != nil && *prev == key {
		return
	}
	ev.SessionID = a.sessionID
	if sink := a.loginExhaustedSink.Load(); sink != nil && *sink != nil {
		(*sink)(ctx, ev)
	}
	ready := make([]any, 0, len(ev.Ready))
	for _, r := range ev.Ready {
		ready = append(ready, map[string]any{"id": r.ID, "name": r.Name})
	}
	if a.eventCallback != nil {
		a.eventCallback("login:exhausted", map[string]any{"provider": ev.Provider, "login": map[string]any{"id": ev.Login.ID, "name": ev.Login.Name},
			"resets_at": ev.ResetsAt, "reason": ev.Reason, "ready": ready})
	}
	when := ""
	if ev.ResetsAt > 0 {
		when = " until " + time.Unix(int64(ev.ResetsAt), 0).Local().Format("15:04")
	}
	text := fmt.Sprintf("The %s login %s is unavailable%s.", ev.Provider, ev.Login.Name, when)
	if len(ev.Ready) > 0 {
		text += fmt.Sprintf(" Switch with /model --login %s.", ev.Ready[0].Name)
	}
	a.emitWarning(text)
}

// CurrentLogin is the login the live pool serves requests with and the selection that chose it, false without a pool
func (a *AIAgent) CurrentLogin() (provider.LoginRef, provider.LoginChoice, bool) {
	pool := a.credentialPool
	if pool == nil {
		return provider.LoginRef{}, provider.LoginChoice{}, false
	}
	choice, chosen := pool.LoginChoice()
	if !chosen {
		choice = provider.LoginChoice{Provider: pool.Provider(), Scope: provider.LoginScopeLegacy}
	}
	id := a.CurrentLoginID()
	if id == "" {
		id = choice.LoginID
	}
	for _, e := range pool.Entries() {
		if e.ID == id {
			return provider.LoginRef{ID: e.ID, Name: e.LoginName()}, choice, true
		}
	}
	return provider.LoginRef{}, choice, false
}

package tools

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Env.Values key of the bound async delivery capability, a bool, absent meaning supported
const sessionAsyncDeliveryKey = "session.async_delivery"

// Env.Values key of the declared session history delivery capability, absent meaning never declared
const sessionHistoryDeliveryKey = "session.history_delivery"

// Process latch set once any session is bound through SetSessionVars
var sessionContextLatch atomic.Bool

// Serializes the copy on write updates of every Env's bound session variable map
var sessionVarsMu sync.Mutex

// Session variables SetSessionVars binds and ClearSessionVars blanks, in binding order
var sessionVarNames = SessionContextEnvVars[:17]

// Every name GetSessionEnv reads from the bound context before the process environment
var sessionVarMap = setOf(SessionContextEnvVars...)

// Whether any session has been bound through SetSessionVars in this process
func SessionContextEngaged() bool { return sessionContextLatch.Load() }

// Session variables of one forked context, writes never reaching the context it was forked from
type sessionVarStore struct{ vars atomic.Pointer[map[string]string] }

// Copy of the env whose session variables start as the bound ones and change apart from them afterwards
func ForkSessionContext(env *Env) *Env {
	if env == nil {
		return nil
	}
	forked := *env
	vars, _ := sessionContextState(env)
	seeded := make(map[string]string, len(vars))
	for k, v := range vars {
		seeded[k] = v
	}
	forked.sessionVars = &sessionVarStore{}
	forked.sessionVars.vars.Store(&seeded)
	return &forked
}

// Applies edit to a fresh copy of the bound session variables, giving an env without a value store one first
func updateSessionVars(env *Env, edit func(vars map[string]string)) {
	if env == nil {
		return
	}
	sessionVarsMu.Lock()
	defer sessionVarsMu.Unlock()
	if env.sessionVars == nil && env.Values == nil {
		env.Values = &sync.Map{}
	}
	vars, _ := sessionContextState(env)
	next := make(map[string]string, len(vars)+1)
	for k, v := range vars {
		next[k] = v
	}
	edit(next)
	if env.sessionVars != nil {
		env.sessionVars.vars.Store(&next)
		return
	}
	env.Values.Store(sessionContextValuesKey, next)
}

// Bound value of one session variable and whether it was ever set in this context
func boundSessionVar(env *Env, name string) (string, bool) {
	vars, _ := sessionContextState(env)
	v, ok := vars[name]
	return v, ok
}

// Bound value of one session variable and whether this context ever set it
func BoundSessionVar(env *Env, name string) (string, bool) { return boundSessionVar(env, name) }

// Puts a session variable back to a saved value, deleting it when it was never set
func restoreSessionVar(env *Env, name, prev string, had bool) {
	updateSessionVars(env, func(vars map[string]string) {
		if had {
			vars[name] = prev
		} else {
			delete(vars, name)
		}
	})
}

// Sets one session variable to a value, or back to never set when had is false
func RestoreSessionVar(env *Env, name, prev string, had bool) {
	restoreSessionVar(env, name, prev, had)
}

// Writes the session id into the bound session variables and the process env, delegated children only the bound copy
func SetCurrentSessionID(ctx context.Context, env *Env, sessionID string) {
	updateSessionVars(env, func(vars map[string]string) { vars["ICEBATH_SESSION_ID"] = sessionID })
	if IsDelegatedChildContext(ctx) {
		return
	}
	os.Setenv("ICEBATH_SESSION_ID", sessionID)
}

// Binds a task local session id, nil only saving, and returns the restore of the prior value
func ScopedCurrentSessionID(env *Env, sessionID *string) func() {
	prev, had := boundSessionVar(env, "ICEBATH_SESSION_ID")
	if sessionID != nil {
		updateSessionVars(env, func(vars map[string]string) { vars["ICEBATH_SESSION_ID"] = *sessionID })
	}
	return func() { restoreSessionVar(env, "ICEBATH_SESSION_ID", prev, had) }
}

// Binds only the route key of the session a call works for and returns the restore of the prior value
func ScopedSessionKey(env *Env, sessionKey string) func() {
	prev, had := boundSessionVar(env, "ICEBATH_SESSION_KEY")
	updateSessionVars(env, func(vars map[string]string) { vars["ICEBATH_SESSION_KEY"] = sessionKey })
	return func() { restoreSessionVar(env, "ICEBATH_SESSION_KEY", prev, had) }
}

// Route key bound in this context, empty when none is, never read from the process environment
func BoundSessionKey(env *Env) string {
	v, _ := boundSessionVar(env, "ICEBATH_SESSION_KEY")
	return v
}

// Inbound source whose scope and parent chat anchors outlive it in durable delivery metadata
type RouteAnchorSource interface {
	RouteScopeID() string
	RouteParentChatID() string
}

// Metadata with the non empty route anchors of source merged over it, metadata itself when there are none
func SourceRouteMetadata(source RouteAnchorSource, metadata map[string]any) map[string]any {
	if source == nil {
		return metadata
	}
	anchors := map[string]string{}
	if v := source.RouteScopeID(); v != "" {
		anchors["scope_id"] = v
	}
	if v := source.RouteParentChatID(); v != "" {
		anchors["parent_chat_id"] = v
	}
	if len(anchors) == 0 {
		return metadata
	}
	merged := make(map[string]any, len(metadata)+len(anchors))
	for k, v := range metadata {
		merged[k] = v
	}
	for k, v := range anchors {
		merged[k] = v
	}
	return merged
}

// Session identity SetSessionVars binds, nil CronSession left unset and nil SessionHistoryDelivery never declared
type SessionVars struct {
	Platform, Source, ChatID, ChatType, ChatName, ThreadID string
	UserID, UserIDAlt, UserName, ScopeID, SessionKey       string
	SessionID, MessageID, Profile, Cwd, UISessionID        string
	ParentChatID                                           string
	NoAsyncDelivery                                        bool
	CronSession, SessionHistoryDelivery                    *string
}

// Binds every session variable of the turn, latches the process session context and pins the logical cwd
func SetSessionVars(env *Env, v SessionVars) {
	sessionContextLatch.Store(true)
	values := []string{
		v.Platform, v.Source, v.ChatID, v.ChatType, v.ChatName, v.ThreadID, v.UserID, v.UserIDAlt,
		v.UserName, v.ScopeID, v.SessionKey, v.SessionID, v.UISessionID, v.MessageID, v.Profile,
		"", v.ParentChatID,
	}
	updateSessionVars(env, func(vars map[string]string) {
		for i, name := range sessionVarNames {
			vars[name] = values[i]
		}
		if v.CronSession == nil {
			delete(vars, "ICEBATH_CRON_SESSION")
		} else {
			vars["ICEBATH_CRON_SESSION"] = *v.CronSession
		}
	})
	if env != nil && env.Values != nil {
		env.Values.Store(sessionContextEngagedKey, true)
		env.Values.Store(sessionAsyncDeliveryKey, !v.NoAsyncDelivery)
		if v.SessionHistoryDelivery == nil {
			env.Values.Delete(sessionHistoryDeliveryKey)
		} else {
			env.Values.Store(sessionHistoryDeliveryKey, *v.SessionHistoryDelivery)
		}
	}
	SetLogicalSessionCwd(env, v.Cwd)
}

// Blanks every session variable so reads stop falling back to the process env, capabilities back to undeclared
func ClearSessionVars(env *Env) {
	updateSessionVars(env, func(vars map[string]string) {
		for _, name := range sessionVarNames {
			vars[name] = ""
		}
	})
	dropSessionCapabilities(env)
	ClearLogicalSessionCwd(env)
}

// Returns every session variable of this context to never bound before a fresh task binds its own
func ResetSessionVars(env *Env) {
	updateSessionVars(env, func(vars map[string]string) {
		for _, name := range SessionContextEnvVars {
			delete(vars, name)
		}
	})
	dropSessionCapabilities(env)
	ClearLogicalSessionCwd(env)
}

// Drops the async and history delivery declarations of the context
func dropSessionCapabilities(env *Env) {
	if env != nil && env.Values != nil {
		env.Values.Delete(sessionAsyncDeliveryKey)
		env.Values.Delete(sessionHistoryDeliveryKey)
	}
}

// Session variable by its ICEBATH_SESSION name, the bound value even when empty, then the process env, then def
func GetSessionEnv(env *Env, name, def string) string {
	if sessionVarMap[name] {
		if v, ok := boundSessionVar(env, name); ok {
			return v
		}
	}
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Whether the turn is delivered over a human messaging channel by platform override, session platform or source
func SessionIsMessagingSurface(env *Env) bool {
	platform := os.Getenv("ICEBATH_PLATFORM")
	if platform == "" {
		platform = GetSessionEnv(env, "ICEBATH_SESSION_PLATFORM", "")
	}
	for _, ident := range []string{platform, GetSessionEnv(env, "ICEBATH_SESSION_SOURCE", "")} {
		ident = pystr.Lower(pystr.Strip(ident))
		if ident != "" && !NonMessagingSessionSurfaces[ident] {
			return true
		}
	}
	return false
}

// Marks the context unable to receive an async completion without latching the session context
func DeclareStatelessChannel(env *Env) {
	if env != nil && env.Values != nil {
		env.Values.Store(sessionAsyncDeliveryKey, false)
	}
}

// Whether the request declared a server history consumer for detached results, undeclared failing closed
func SessionHistoryDeliverySupported(env *Env) bool {
	if env == nil || env.Values == nil {
		return false
	}
	v, _ := env.Values.Load(sessionHistoryDeliveryKey)
	s, _ := v.(string)
	return s == "1"
}

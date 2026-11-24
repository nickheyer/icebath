package tuihost

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Wait before an armed retry that found its session busy tries again
const loginWaitBusyRetry = 30 * time.Second

// One armed retry of a session's last turn for when its login resets
type loginWait struct {
	timer    *time.Timer
	sessions LoginSessions
	id       string
	provider string
	login    tuigateway.LoginRef
	at       float64
}

// Armed retries by store path and stored session
var loginWaits = struct {
	sync.Mutex
	m map[string]*loginWait
}{m: map[string]*loginWait{}}

// Registry key of a session's armed retry
func loginWaitKey(session *LoginSession) string {
	path := ""
	if session.DB != nil {
		path = session.DB.Path()
	}
	return path + "|" + session.StoredID
}

// Stored record of a session's armed retry, nil when none is armed
func storedLoginWait(cfg map[string]any) (string, tuigateway.LoginRef, float64, bool) {
	record, _ := cfg[provider.ModelConfigLoginWait].(map[string]any)
	pool, _ := record["provider"].(string)
	id, _ := record["login"].(string)
	name, _ := record["name"].(string)
	at, _ := record["at"].(float64)
	if pool == "" || id == "" || at <= 0 {
		return "", tuigateway.LoginRef{}, 0, false
	}
	return pool, tuigateway.LoginRef{ID: id, Name: name}, at, true
}

// Writes or clears a session's armed retry record
func storeLoginWait(ctx context.Context, session *LoginSession, record map[string]any) error {
	if session.DB == nil || session.StoredID == "" {
		return nil
	}
	return session.DB.UpdateSessionModelConfig(ctx, session.StoredID, func(cfg map[string]any) bool {
		if record == nil {
			_, held := cfg[provider.ModelConfigLoginWait]
			delete(cfg, provider.ModelConfigLoginWait)
			return held
		}
		cfg[provider.ModelConfigLoginWait] = record
		return true
	})
}

// Armed retry of a session as login.list shows it, nil when none is armed
func pendingLoginWait(ctx context.Context, session *LoginSession) *tuigateway.LoginWait {
	if session.DB == nil || session.StoredID == "" {
		return nil
	}
	cfg, err := session.DB.SessionModelConfig(ctx, session.StoredID)
	if err != nil {
		return nil
	}
	pool, login, at, ok := storedLoginWait(cfg)
	if !ok {
		return nil
	}
	return &tuigateway.LoginWait{Provider: pool, Login: login, RetryAt: *LoginTime(at)}
}

// Drops a session's armed retry, its timer and its stored record
func cancelLoginWait(ctx context.Context, session *LoginSession) bool {
	loginWaits.Lock()
	wait := loginWaits.m[loginWaitKey(session)]
	delete(loginWaits.m, loginWaitKey(session))
	loginWaits.Unlock()
	if wait != nil {
		wait.timer.Stop()
	}
	if err := storeLoginWait(ctx, session, nil); err != nil {
		serverLog().Warn(fmt.Sprintf("could not clear the login wait of session %s: %v", session.StoredID, err))
	}
	return wait != nil
}

// Starts the timer of an armed retry a second past the reset, replacing any the session had
func armLoginWait(session *LoginSession, wait *loginWait) {
	key := loginWaitKey(session)
	delay := time.Until(time.Unix(int64(wait.at)+1, 0))
	if delay < 0 {
		delay = 0
	}
	loginWaits.Lock()
	if prev := loginWaits.m[key]; prev != nil {
		prev.timer.Stop()
	}
	wait.timer = time.AfterFunc(delay, func() { fireLoginWait(key, wait) })
	loginWaits.m[key] = wait
	loginWaits.Unlock()
}

// Retries the session's last turn once its login reset, trying again later while the session is busy
func fireLoginWait(key string, wait *loginWait) {
	ctx := context.Background()
	loginWaits.Lock()
	if loginWaits.m[key] != wait {
		loginWaits.Unlock()
		return
	}
	delete(loginWaits.m, key)
	loginWaits.Unlock()
	session, err := wait.sessions.LoginSession(ctx, wait.id)
	if err != nil || session == nil {
		serverLog().Info(fmt.Sprintf("login wait of session %s dropped: the session is gone", wait.id))
		return
	}
	if session.Retry == nil {
		cancelLoginWait(ctx, session)
		return
	}
	if err := session.Retry(ctx); err != nil {
		var refused *LoginError
		if asLoginError(err, &refused) && refused.Code == LoginCodeBusy {
			wait.at = float64(time.Now().Add(loginWaitBusyRetry).Unix())
			armLoginWait(session, wait)
			return
		}
		serverLog().Warn(fmt.Sprintf("login wait of session %s could not retry the turn: %v", wait.id, err))
	}
	if err := storeLoginWait(ctx, session, nil); err != nil {
		serverLog().Warn(fmt.Sprintf("could not clear the login wait of session %s: %v", session.StoredID, err))
	}
	if session.Publish != nil {
		session.Publish(ctx)
	}
}

// Whether err is a login refusal, filling target
func asLoginError(err error, target **LoginError) bool {
	refused, ok := err.(*LoginError)
	if ok {
		*target = refused
	}
	return ok
}

// SessionLoginWait answers session.login_wait, a retry of the last turn armed for the pinned login's reset, run at once, or dropped
func SessionLoginWait(ctx context.Context, sessions LoginSessions, p tuigateway.SessionLoginWaitParams) (tuigateway.SessionLoginWaitResult, error) {
	result := tuigateway.SessionLoginWaitResult{SessionID: p.SessionID, State: "idle"}
	session, err := requestSession(ctx, sessions, p.SessionID)
	if err != nil {
		return result, err
	}
	if session == nil {
		return result, loginRefusal(LoginCodeMissing, "invalid_params", "session_id is required")
	}
	poolKey, err := requestPoolKey(ctx, p.Provider, session)
	if err != nil {
		return result, err
	}
	result.Provider = poolKey
	if p.Cancel {
		if cancelLoginWait(ctx, session) {
			result.State = "cancelled"
		}
		return result, nil
	}
	if p.Now {
		cancelLoginWait(ctx, session)
		if session.Retry == nil {
			return result, loginRefusal(LoginCodeBadValue, "retry_unavailable", "this backend cannot retry the turn; send it again")
		}
		if err := session.Retry(ctx); err != nil {
			return result, err
		}
		result.State = "retried"
		return result, nil
	}
	choices, err := session.Choices(ctx)
	if err != nil {
		return result, LoginFailure(err)
	}
	env := provider.LoginEnv(provider.WithSessionLogins(ctx, choices))
	group := provider.ListLogins(env, poolKey)
	if !group.Choice.Pinned() {
		return result, loginRefusal(LoginCodeBadValue, "not_pinned", "the session uses no single %s login to wait for", poolKey)
	}
	login := tuigateway.LoginRef{ID: group.Choice.LoginID}
	for _, l := range group.Logins {
		if l.ID == login.ID {
			login.Name = l.Name
		}
	}
	result.Login = &login
	at, known := provider.LoginResetAt(env, poolKey, login.ID)
	if !known {
		return result, nil
	}
	record := map[string]any{"provider": poolKey, "login": login.ID, "name": login.Name, "at": at}
	if err := storeLoginWait(ctx, session, record); err != nil {
		return result, LoginFailure(err)
	}
	armLoginWait(session, &loginWait{sessions: sessions, id: session.ID, provider: poolKey, login: login, at: at})
	result.State, result.RetryAt = "armed", LoginTime(at)
	if session.Publish != nil {
		session.Publish(ctx)
	}
	return result, nil
}

// RearmLoginWaits arms again every retry a store's sessions had armed before this process started
func RearmLoginWaits(ctx context.Context, sessions LoginSessions, db *state.SessionDB) {
	ids, err := db.SessionsWithModelConfigKey(ctx, provider.ModelConfigLoginWait)
	if err != nil {
		serverLog().Warn("could not read the armed login waits: " + anyutil.String(err))
		return
	}
	for _, id := range ids {
		cfg, err := db.SessionModelConfig(ctx, id)
		if err != nil {
			continue
		}
		pool, login, at, ok := storedLoginWait(cfg)
		if !ok {
			continue
		}
		session := &LoginSession{ID: id, DB: db, StoredID: id}
		armLoginWait(session, &loginWait{sessions: sessions, id: id, provider: strings.ToLower(pool), login: login, at: at})
	}
}

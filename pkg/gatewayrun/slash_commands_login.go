package gatewayrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Registry slot of the one sign in attempt a gateway runs at a time
const loginSingle = "login"

// Platforms whose dm chat type is a broadcast topic, a channel or an agent peer
var loginBlockedPlatforms = map[string]bool{"ntfy": true, "raft": true, "a2a": true}

// One running sign in with its owning chat key, cancelled and saving under the login lock
type signInAttempt struct {
	attemptID string
	key       [3]string
	source    *gateway.SessionSource
	cancelled bool
	saving    bool
}

// Eight random hex characters
func shortHexID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Publishes a sticky notice to the viewers of a live local session, false when the source is not one
func publishLocalNotice(ctx context.Context, runner *GatewayRunner, source *gateway.SessionSource, text string) bool {
	if source == nil || source.Platform != gateway.PlatformLocal {
		return false
	}
	authority, err := gateway.ActiveAuthority(ctx, runner)
	if err != nil || authority == nil {
		return false
	}
	live, ok := authority.Live(source.ChatID)
	if !ok || live == nil {
		return false
	}
	live.EventStream.Publish(source.ChatID, pyjson.ObjectOf("text", text, "level", "info", "kind", "sticky", "key", "login:"+shortHexID()),
		"notification.show", nil)
	return true
}

// Single worker pool the sign in flow runs on, recreated after a shutdown
func (r *GatewayRunner) loginExecutor() *threadPoolExecutor {
	if r.loginExec == nil || r.loginExec.isShutdown() {
		r.loginExec = newThreadPoolExecutor(1, "icebath-login")
	}
	return r.loginExec
}

// Runs fn on the login worker with the caller's context values
func (r *GatewayRunner) runLoginBlocking(ctx context.Context, fn func(ctx context.Context) (any, error)) (any, error) {
	fut, err := r.loginExecutor().submit(func(workerCtx context.Context) (any, error) { return fn(onExecutorThread(ctx, workerCtx)) })
	if err != nil {
		return nil, err
	}
	return fut.await(ctx)
}

// Credential pool environment of the active profile home
func loginPoolEnv(ctx context.Context) provider.PoolEnv {
	h := home.Dir(ctx)
	return provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Scope: provider.SecretScopeFrom(ctx),
		Config: provider.ConfigLoader(h), Log: runLog(), Ctx: ctx}
}

// Starts a sign in from a paired DM, replacing this chat's own live attempt and refusing one elsewhere
func (r *GatewayRunner) handleLoginCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	src := event.Source
	platform := pystr.Lower(string(src.Platform))
	pairedDM := (src.ChatType == "dm" || src.ChatType == "private") && src.ChatID != "" && !loginBlockedPlatforms[platform]
	if !pairedDM {
		return provider.LoginDMOnly, nil
	}
	policy := gateway.PolicyForRunnerSource(r, src)
	if policy.Enabled && !policy.IsAdmin(sattr(src.UserID)) {
		return provider.LoginNotAllowed, nil
	}
	key := [3]string{string(src.Platform), src.ChatID, sattr(src.UserID)}
	claim := func() bool {
		r.loginLock.Lock()
		defer r.loginLock.Unlock()
		if r.loginAttempts == nil {
			r.loginAttempts = map[string]*signInAttempt{}
		}
		live := r.loginAttempts[loginSingle]
		if live != nil && (live.key != key || live.saving) {
			return false
		}
		if live != nil {
			live.cancelled = true
		}
		return true
	}
	if !claim() {
		return provider.LoginBusyElsewhere, nil
	}
	value, err := r.runLoginBlocking(ctx, func(workerCtx context.Context) (any, error) {
		return provider.CurrentNousState(loginPoolEnv(workerCtx)), nil
	})
	if err != nil {
		return "", err
	}
	if st, _ := value.(map[string]any); len(st) > 0 && !provider.IsGuestState(st) {
		return provider.UpgradeAlreadySignedIn, nil
	}
	r.loginLock.Lock()
	live := r.loginAttempts[loginSingle]
	if live != nil && (live.key != key || live.saving) {
		r.loginLock.Unlock()
		return provider.LoginBusyElsewhere, nil
	}
	if live != nil {
		live.cancelled = true
	}
	attempt := &signInAttempt{attemptID: shortHexID(), key: key, source: src}
	r.loginAttempts[loginSingle] = attempt
	r.loginLock.Unlock()
	r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), "login", func(taskCtx context.Context) (any, error) {
		return nil, r.runLogin(taskCtx, attempt)
	}))
	return provider.UpgradeStart, nil
}

// Drains the sign in states onto the chat until a terminal state, a failure pushing the not completed notice
func (r *GatewayRunner) runLogin(ctx context.Context, attempt *signInAttempt) (err error) {
	cancelled := func() bool {
		r.loginLock.Lock()
		defer r.loginLock.Unlock()
		return attempt.cancelled
	}
	persistGuard := func(body func(mayPersist bool) error) error {
		r.loginLock.Lock()
		if !attempt.cancelled {
			attempt.saving = true
		}
		mayPersist := attempt.saving
		r.loginLock.Unlock()
		return body(mayPersist)
	}
	var next func() (provider.SignInState, bool)
	var stop func()
	pushedTerminal := false
	defer func() {
		r.loginLock.Lock()
		if r.loginAttempts[loginSingle] == attempt {
			delete(r.loginAttempts, loginSingle)
		}
		r.loginLock.Unlock()
		if stop != nil {
			stop()
		}
	}()
	defer func() {
		if ctx.Err() != nil {
			r.loginLock.Lock()
			attempt.cancelled = true
			r.loginLock.Unlock()
			return
		}
		if err != nil {
			runLog().Warn(fmt.Sprintf("/login attempt %s failed", attempt.attemptID), "exc_info", err)
			if !pushedTerminal {
				r.pushLogin(ctx, attempt, provider.UpgradeNotCompleted)
			}
			err = nil
		}
	}()
	for {
		value, runErr := r.runLoginBlocking(ctx, func(workerCtx context.Context) (any, error) {
			if next == nil {
				var seq iter.Seq[provider.SignInState] = provider.RunSignIn(loginPoolEnv(workerCtx),
					provider.SignInOptions{Timeout: 15 * time.Second, Cancelled: cancelled, PersistGuard: persistGuard})
				next, stop = iter.Pull(seq)
			}
			state, ok := next()
			if !ok {
				return nil, nil
			}
			return state, nil
		})
		if runErr != nil {
			if pyrepr.ExceptionName(runErr) == "RuntimeError" {
				runLog().Warn(fmt.Sprintf("/login %s: executor unavailable, stopping: %s", attempt.attemptID, pyrepr.ExceptionMessage(runErr)))
				return nil
			}
			return runErr
		}
		state, _ := value.(provider.SignInState)
		if state == nil {
			return nil
		}
		if err := r.renderLoginState(ctx, attempt, state); err != nil {
			return err
		}
		if state.Terminal() {
			pushedTerminal = true
			return nil
		}
	}
}

// Pushes one notice to the attempt's chat, a transport failure only logged
func (r *GatewayRunner) pushLogin(ctx context.Context, attempt *signInAttempt, text string) {
	if publishLocalNotice(ctx, r, attempt.source, text) {
		return
	}
	if err := r.deliverPlatformNotice(ctx, attempt.source, text); err != nil {
		runLog().Warn(fmt.Sprintf("/login %s: push failed", attempt.attemptID), "exc_info", err)
	}
}

// Pushes the chat copy of one sign in state, a model change sweeping sessions off the free tier first
func (r *GatewayRunner) renderLoginState(ctx context.Context, attempt *signInAttempt, state provider.SignInState) error {
	switch s := state.(type) {
	case provider.SignInCode:
		r.pushLogin(ctx, attempt, s.Link)
		r.pushLogin(ctx, attempt, s.Code)
		r.pushLogin(ctx, attempt, s.CopyWithWait())
		return nil
	case provider.SignInWaiting:
		return nil
	case provider.SignInCompleted:
		text := s.Copy()
		if s.ModelChanged && s.Model != "" {
			if failed := r.sweepSessionsOffWelcome(ctx); failed != 0 {
				text += i18n.T(ctx, "gateway.login.free_tier_remaining", "", nil)
			}
		}
		r.pushLogin(ctx, attempt, text)
		return nil
	}
	r.pushLogin(ctx, attempt, state.Copy())
	return nil
}

// Clears free tier model pins and evicts the free tier agents, returning how many pins failed to clear
func (r *GatewayRunner) sweepSessionsOffWelcome(ctx context.Context) int {
	var keys []string
	if r.agentCache != nil {
		if r.agentCacheLock != nil {
			r.agentCacheLock.Lock()
		}
		for key, entry := range r.agentCache.All() {
			if a := firstAgent(&entry); a != nil && a.Provider() == "nous" && a.Model() == provider.GuestModel {
				keys = append(keys, key)
			}
		}
		if r.agentCacheLock != nil {
			r.agentCacheLock.Unlock()
		}
	}
	failed := 0
	for _, key := range keys {
		overrides := r.sessionModelOverrides()
		override, _ := overrides.Get(key)
		if override != nil && pyrepr.Str(pyrepr.Or(override["model"], "")) == provider.GuestModel {
			cleared := false
			for attempt := range 2 {
				err := r.slashStore().SetModelOverride(ctx, key, nil)
				if err == nil {
					cleared = true
					break
				}
				if attempt == 1 {
					runLog().Warn(fmt.Sprintf("/login: failed to clear free-tier override for %s", key), "exc_info", err)
				}
			}
			if !cleared {
				failed++
				continue
			}
			r.withPeekedSessionState(key, func(s *SessionState) {
				if held := s.conversation.modelOverride; held != nil && mapIdentity(held) == mapIdentity(override) {
					s.conversation.modelOverride = nil
				}
			})
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Warn(fmt.Sprintf("/login: failed to evict free-tier session %s", key), "exc_info", rec)
				}
			}()
			r.evictCachedAgent(ctx, key, false, false)
		}()
	}
	return failed
}

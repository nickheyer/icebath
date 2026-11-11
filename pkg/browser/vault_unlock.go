package browser

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Idle lifetime of a manager session token in nanoseconds, read at every call
var vaultIdleTTLNanos atomic.Int64

func init() { vaultIdleTTLNanos.Store(int64(30 * time.Minute)) }

func vaultIdleTTL() time.Duration { return time.Duration(vaultIdleTTLNanos.Load()) }

const vaultSessionSlot = "vault_current_session_id"

// Binds the session running on this thread so an unlock records its owner, nil clearing it
func SetVaultSessionID(ctx context.Context, sessionID *string) {
	if sessionID == nil {
		tools.ThreadLocalSet(ctx, vaultSessionSlot, nil)
		return
	}
	tools.ThreadLocalSet(ctx, vaultSessionSlot, *sessionID)
}

// The session bound on this thread, nil when none is bound
func VaultSessionID(ctx context.Context) *string {
	sid, ok := tools.ThreadLocalGet(ctx, vaultSessionSlot).(string)
	if !ok {
		return nil
	}
	return &sid
}

// Hands the unlock owner to propagated worker threads beside the three prompt callbacks
func init() {
	tools.RegisterThreadCallback(tools.ThreadCallback{
		Get: func(ctx context.Context) any {
			if sid := VaultSessionID(ctx); sid != nil {
				return *sid
			}
			return nil
		},
		Set: func(ctx context.Context, cb any) {
			if sid, ok := cb.(string); ok {
				SetVaultSessionID(ctx, &sid)
				return
			}
			SetVaultSessionID(ctx, nil)
		}})
}

// Profile home and backend a session token is scoped to
type vaultTokenKey struct{ home, backend string }

type vaultToken struct {
	value    string
	lastUsed time.Time
}

type vaultExpiryTimer struct{ timer *time.Timer }

// Closures over the token state, the only way to reach a session token
type vaultTokenOps struct {
	live           func(key vaultTokenKey, touch bool) (string, bool)
	begin          func(key vaultTokenKey) int
	commit         func(key vaultTokenKey, value string, generation *int, owner *string) bool
	lockProfile    func(home string, backend *string)
	release        func(sessionID string)
	lockEverything func()
}

// Builds the token state, every lock bumping the key generation so an earlier unlock cannot commit
func vaultTokenStore() vaultTokenOps {
	var guard sync.Mutex
	tokens := map[vaultTokenKey]*vaultToken{}
	owners := map[vaultTokenKey]*string{}
	generations := map[vaultTokenKey]int{}
	var timer *vaultExpiryTimer
	var expire func(mine *vaultExpiryTimer)

	forget := func(key vaultTokenKey) {
		delete(tokens, key)
		delete(owners, key)
		generations[key]++
	}
	arm := func(now time.Time) {
		if timer != nil || len(tokens) == 0 {
			return
		}
		first := true
		var earliest time.Time
		for _, t := range tokens {
			if first || t.lastUsed.Before(earliest) {
				earliest, first = t.lastUsed, false
			}
		}
		delay := max(earliest.Add(vaultIdleTTL()).Sub(now), 0) + 10*time.Millisecond
		mine := &vaultExpiryTimer{}
		timer = mine
		mine.timer = time.AfterFunc(delay, func() { expire(mine) })
	}
	settle := func() {
		if len(tokens) == 0 && timer != nil {
			timer.timer.Stop()
			timer = nil
		}
	}
	expire = func(mine *vaultExpiryTimer) {
		guard.Lock()
		defer guard.Unlock()
		if timer == mine {
			timer = nil
		}
		now := time.Now()
		for key, t := range tokens {
			if now.Sub(t.lastUsed) > vaultIdleTTL() {
				delete(tokens, key)
			}
		}
		arm(now)
	}
	live := func(key vaultTokenKey, touch bool) (string, bool) {
		guard.Lock()
		defer guard.Unlock()
		token := tokens[key]
		if token == nil {
			return "", false
		}
		now := time.Now()
		if now.Sub(token.lastUsed) > vaultIdleTTL() {
			delete(tokens, key)
			settle()
			return "", false
		}
		if touch {
			token.lastUsed = now
		}
		return token.value, true
	}
	begin := func(key vaultTokenKey) int {
		guard.Lock()
		defer guard.Unlock()
		return generations[key]
	}
	commit := func(key vaultTokenKey, value string, generation *int, owner *string) bool {
		guard.Lock()
		defer guard.Unlock()
		if generation != nil && *generation != generations[key] {
			return false
		}
		now := time.Now()
		tokens[key] = &vaultToken{value: value, lastUsed: now}
		owners[key] = owner
		arm(now)
		return true
	}
	lockProfile := func(home string, backend *string) {
		guard.Lock()
		defer guard.Unlock()
		keys := map[vaultTokenKey]bool{}
		named := func(k vaultTokenKey) {
			if k.home == home && (backend == nil || k.backend == *backend) {
				keys[k] = true
			}
		}
		for k := range tokens {
			named(k)
		}
		for k := range owners {
			named(k)
		}
		for k := range generations {
			named(k)
		}
		if backend != nil {
			keys[vaultTokenKey{home, *backend}] = true
		}
		for k := range keys {
			forget(k)
		}
		settle()
	}
	release := func(sessionID string) {
		guard.Lock()
		defer guard.Unlock()
		for k, owner := range owners {
			if owner != nil && *owner == sessionID {
				forget(k)
			}
		}
		settle()
	}
	lockEverything := func() {
		guard.Lock()
		defer guard.Unlock()
		keys := map[vaultTokenKey]bool{}
		for k := range tokens {
			keys[k] = true
		}
		for k := range owners {
			keys[k] = true
		}
		for k := range keys {
			forget(k)
		}
		settle()
	}
	return vaultTokenOps{live: live, begin: begin, commit: commit, lockProfile: lockProfile, release: release, lockEverything: lockEverything}
}

var vaultTokens = vaultTokenStore()

func vaultKey(ctx context.Context, backend string) vaultTokenKey {
	return vaultTokenKey{home: home.Dir(ctx), backend: backend}
}

// Token for a real manager call, refreshing the idle timer
func VaultSessionToken(ctx context.Context, backend string) (string, bool) {
	return vaultTokens.live(vaultKey(ctx, backend), true)
}

// Lock generation snapshot taken before spawning the manager CLI, passed to StoreVaultSessionToken
func BeginVaultUnlock(ctx context.Context, backend string) int {
	return vaultTokens.begin(vaultKey(ctx, backend))
}

// Commits an unlock, false and nothing stored when a lock happened since the generation snapshot
func StoreVaultSessionToken(ctx context.Context, backend, token string, generation *int) bool {
	return vaultTokens.commit(vaultKey(ctx, backend), token, generation, VaultSessionID(ctx))
}

// Forgets the current profile's session for one backend, or all of them when backend is nil
func LockVault(ctx context.Context, backend *string) {
	vaultTokens.lockProfile(home.Dir(ctx), backend)
}

// Drops only the tokens the ended gateway session unlocked
func ReleaseVaultSession(sessionID string) { vaultTokens.release(sessionID) }

// Drops every token of every profile at process shutdown
func LockAllVaultProfiles() { vaultTokens.lockEverything() }

// Status probe that leaves the idle timer alone
func IsVaultUnlocked(ctx context.Context, backend string) bool {
	_, ok := vaultTokens.live(vaultKey(ctx, backend), false)
	return ok
}

// False where no human can answer, cron, webhook, api_server and single query sessions
func CanPromptVaultHere(ctx context.Context, env *tools.Env) bool {
	if tools.NoUserCanAnswer(env) {
		return false
	}
	return GetUnlockPromptCallback(ctx) != nil
}

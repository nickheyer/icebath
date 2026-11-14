package portal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	KeepaliveInterval         = 15 * time.Minute
	keepaliveMinInterval      = 60 * time.Second
	keepaliveTicksPerLifetime = 4
	keepaliveInitialDelay     = 60 * time.Second
)

func (a *Account) keepaliveIntervalSetting(ctx context.Context) time.Duration {
	cfg, err := a.cfg.Load(a.bind(ctx))
	if err != nil {
		return KeepaliveInterval
	}
	return time.Duration(int64(cfg.Portal.KeepaliveIntervalSeconds)) * time.Second
}

func (a *Account) observedLifetime(ctx context.Context) (time.Duration, bool, error) {
	state, err := a.currentState(ctx)
	if err != nil {
		return 0, false, err
	}
	shortest, found := int64(0), false
	for _, key := range []string{"expires_in", "agent_key_expires_in"} {
		f, ok := floatOf(state.get(key))
		if !ok || int64(f) <= 0 {
			continue
		}
		if !found || int64(f) < shortest {
			shortest, found = int64(f), true
		}
	}
	return time.Duration(shortest) * time.Second, found, nil
}

func keepaliveTick(configured, lifetime time.Duration, known bool) time.Duration {
	if !known || lifetime <= 0 {
		return configured
	}
	derived := (lifetime / time.Second / keepaliveTicksPerLifetime) * time.Second
	return max(keepaliveMinInterval, min(configured, derived))
}

func refreshHorizon(tick, floor time.Duration) time.Duration {
	return max(floor, tick+auth.AccessTokenRefreshSkew)
}

func (a *Account) refreshSelectedPoolEntry(ctx context.Context, minKeyTTL, minAccessTTL time.Duration) (usable, pooled bool, err error) {
	pool, err := a.store.Pool(ctx, Provider)
	if err != nil {
		slog.Debug("Icebath Portal auth keepalive: credential pool unavailable", "error", err)
		return false, false, nil
	}
	if !pool.HasCredentials() {
		return false, false, nil
	}
	entry, ok, err := pool.Select(ctx, "")
	if err != nil {
		slog.Debug("Icebath Portal auth keepalive: credential pool selection failed", "error", err)
		return false, true, nil
	}
	if !ok {
		return false, true, nil
	}
	if minAccessTTL <= 0 {
		minAccessTTL = auth.AccessTokenRefreshSkew
	}
	accessExpiring := auth.IsExpiring(entry.ExpiresAt, minAccessTTL)
	keyUsable := agentKeyUsable(entry.AgentKey.Reveal(), entry.ExtraString("scope"), entry.AgentKeyExpiresAt, minKeyTTL)
	if accessExpiring || !keyUsable {
		_, ok, err := pool.TryRefreshCurrent(ctx)
		if err != nil {
			return false, true, err
		}
		if !ok {
			return false, true, nil
		}
		slog.Debug("Icebath Portal auth keepalive: refreshed credential pool entry")
	}
	return true, true, nil
}

func (a *Account) refreshKeepaliveOnce(ctx context.Context, minKeyTTL, minAccessTTL time.Duration) (bool, error) {
	ctx, err := a.detached(ctx)
	if err != nil {
		return false, err
	}
	usable, pooled, err := a.refreshSelectedPoolEntry(ctx, max(60*time.Second, minKeyTTL), minAccessTTL)
	if err != nil {
		return false, err
	}
	if pooled {
		return usable, nil
	}
	state, err := a.currentState(ctx)
	if err != nil {
		return false, err
	}
	if state.size() == 0 || isGuestState(state) {
		return false, nil
	}
	if _, err := a.RuntimeCredentials(WithBackground(ctx), ResolveOptions{Timeout: a.timeoutSetting(ctx)}); err != nil {
		if ae, ok := authErrorOf(err); ok && ae.Relogin {
			slog.Info(fmt.Sprintf("Icebath Portal auth keepalive requires re-login: %v", err))
		} else {
			slog.Debug("Icebath Portal auth keepalive failed", "error", err)
		}
		return false, nil
	}
	slog.Debug("Icebath Portal auth keepalive: refreshed singleton auth state")
	return true, nil
}

func (a *Account) keepalivePass(ctx context.Context, interval time.Duration) (time.Duration, error) {
	lifetime, known, err := a.observedLifetime(ctx)
	if err != nil {
		return 0, err
	}
	tick := keepaliveTick(interval, lifetime, known)
	horizon := refreshHorizon(tick, max(60*time.Second, auth.PortalInvokeJWTMinTTL))
	if _, err := a.refreshKeepaliveOnce(ctx, horizon, horizon); err != nil {
		return 0, err
	}
	return tick, nil
}

func (a *Account) StartKeepalive(ctx context.Context) bool {
	a.startWorker()
	interval := a.keepaliveIntervalSetting(ctx)
	if interval <= 0 {
		return false
	}
	if state, err := a.currentState(ctx); err == nil && isGuestState(state) {
		slog.Debug("Icebath Portal auth keepalive skipped: free tier has no refresh token")
		return false
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return false
	}
	if a.keepalive != nil && !a.keepalive.ended() {
		a.mu.Unlock()
		return true
	}
	l := newLoop()
	a.keepalive = l
	a.mu.Unlock()
	go func() {
		defer close(l.done)
		if l.wait(keepaliveInitialDelay) {
			return
		}
		for !l.wait(0) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-l.stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			tick, err := a.keepalivePass(ctx, interval)
			cancel()
			if err != nil {
				if !l.wait(0) {
					slog.Error("Icebath Portal auth keepalive ended on an unreadable credential store or a failed pool refresh", "error", err)
				}
				return
			}
			if l.wait(tick) {
				return
			}
		}
	}()
	slog.Debug("Icebath Portal auth keepalive started")
	return true
}

func (a *Account) StopKeepalive(timeout time.Duration) {
	a.mu.Lock()
	keepalive, worker := a.keepalive, a.worker
	a.keepalive, a.worker = nil, nil
	a.mu.Unlock()
	if worker != nil {
		worker.halt(timeout)
	}
	if keepalive != nil {
		keepalive.halt(timeout)
	}
}

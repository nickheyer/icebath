package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Authorizes source under the live transport's profile rather than the routed runtime
func (r *GatewayRunner) isUserAuthorizedForSource(ctx context.Context, source *gateway.SessionSource, allowAdapterDelegation bool) (bool, error) {
	return r.underAuthorizationProfile(ctx, source, func(ctx context.Context) (bool, error) {
		return r.isUserAuthorized(ctx, source, allowAdapterDelegation)
	})
}

// Counts a bot message under the profile that authorized it so the loop guard reads that profile's config
func (r *GatewayRunner) admitBotMessageForSource(ctx context.Context, source *gateway.SessionSource) (bool, error) {
	return r.underAuthorizationProfile(ctx, source, func(ctx context.Context) (bool, error) {
		return r.admitBotMessage(ctx, source), nil
	})
}

// Runs check inside the authorization profile's runtime scope, unscoped when the source has none
func (r *GatewayRunner) underAuthorizationProfile(ctx context.Context, source *gateway.SessionSource, check func(ctx context.Context) (bool, error)) (bool, error) {
	authorizationHome, err := r.authorizationHomeForSource(ctx, source)
	if err != nil {
		return false, err
	}
	if authorizationHome == nil {
		return check(ctx)
	}
	return check(gateway.ProfileRuntimeScope(ctx, *authorizationHome, true))
}

// Remembers a copy of the live source of a session as the most recent, evicting the oldest past the cap
func (r *GatewayRunner) cacheSessionSource(sessionKey string, source *gateway.SessionSource) {
	if sessionKey == "" || source == nil {
		return
	}
	if r.sessionSources == nil {
		r.sessionSources = pydict.New[string, *gateway.SessionSource]()
	}
	cp := *source
	r.sessionSources.Delete(sessionKey)
	r.sessionSources.Set(sessionKey, &cp)
	maxSize := r.sessionSourcesMax
	if maxSize == 0 {
		maxSize = 512
	}
	for r.sessionSources.Len() > maxSize {
		r.sessionSources.Delete(r.sessionSources.Keys()[0])
	}
}

// The one async facade over this runner's session store, rebuilt when the store was swapped
func (r *GatewayRunner) AsyncSessionStore() *gateway.AsyncSessionStore {
	if r.asyncSessionStore == nil || r.asyncSessionStore.Store() != r.sessionStore {
		r.asyncSessionStore = gateway.NewAsyncSessionStore(r.sessionStore)
	}
	return r.asyncSessionStore
}

// Live source cached for the session, marked most recent, nil when none is cached
func (r *GatewayRunner) getCachedSessionSource(sessionKey string) *gateway.SessionSource {
	if sessionKey == "" || r.sessionSources == nil || r.sessionSources.Len() == 0 {
		return nil
	}
	source, ok := r.sessionSources.Get(sessionKey)
	if !ok || source == nil {
		return nil
	}
	r.sessionSources.Delete(sessionKey)
	r.sessionSources.Set(sessionKey, source)
	return source
}

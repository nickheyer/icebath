package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Provider with its endpoint and key of the route a session runs, read when the account limits are fetched
type sessionRoute func() (string, provider.AccountUsageFetchOptions, error)

// Route of a session, its built agent's live one, else the one its next turn resolves from the frozen launch policy
func sessionRouteOf(ctx context.Context, a *SessionAuthority, ref SessionRef) (sessionRoute, error) {
	agent, err := authorityAgent(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	if agent != nil {
		view := agent.SessionRuntime()
		name, opts := view.Provider(), provider.AccountUsageFetchOptions{BaseURL: view.BaseURL(), APIKey: agent.APIKey()}
		return func() (string, provider.AccountUsageFetchOptions, error) { return name, opts, nil }, nil
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return func() (string, provider.AccountUsageFetchOptions, error) {
			return "", provider.AccountUsageFetchOptions{}, nil
		}, nil
	}
	return func() (string, provider.AccountUsageFetchOptions, error) {
		frozen, model, err := policy.effective(ctx, a, true)
		if err != nil {
			return "", provider.AccountUsageFetchOptions{}, err
		}
		key, err := routeLaunchKey(ctx, a, policy, frozen)
		if err != nil {
			return "", provider.AccountUsageFetchOptions{}, err
		}
		runtime, _, err := RouteRuntime(ctx, policy, frozen, model, key, false)
		if err != nil {
			return "", provider.AccountUsageFetchOptions{}, err
		}
		return runtime.Provider, provider.AccountUsageFetchOptions{BaseURL: runtime.BaseURL, APIKey: runtime.APIKey}, nil
	}, nil
}

// Account limit lines of the provider a session runs on, fetched off the caller's goroutine in the owning profile's scope
func accountUsage(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if paramsOutside(params, "session_id", "profile") {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	if _, err := a.Resolve(ctx, c.actor, ref); err != nil {
		return nil, err
	}
	route, err := sessionRouteOf(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	lines, err := forwardOffThread(ctx, func() ([]string, error) {
		name, opts, err := route()
		var signedOut *provider.AuthError
		if errors.As(err, &signedOut) {
			return []string{}, nil
		}
		if err != nil {
			return nil, err
		}
		if name == "" {
			return []string{}, nil
		}
		return provider.RenderAccountUsageLines(provider.FetchAccountUsage(ctx, name, opts), false), nil
	})
	if err != nil {
		return nil, err
	}
	return tuigateway.Plain(tuigateway.SessionAccountUsage{Lines: lines}), nil
}

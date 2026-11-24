package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Error frame of a refused login request, its code and reason
func loginRefusalFrame(rid any, err error) map[string]any {
	refused := tuihost.LoginFailure(err)
	return rpcErr(rid, refused.Code, refused.Message, map[string]any{"reason": refused.Reason})
}

// ---- sessions ----

// ---- model switch ----

// ---- model.disconnect compatibility ----

// Removes one login of the provider for model.disconnect, unhandled when the provider holds no login to remove
func disconnectOneLogin(ctx context.Context, rid any, slug, target string, pconfig *provider.Profile) (map[string]any, bool, error) {
	env := provider.LoginEnv(ctx)
	key := provider.LoginPoolKey(ctx, slug)
	if target == "" {
		target = provider.DefaultLoginID(env, key)
	}
	if target == "" {
		return nil, false, nil
	}
	removal, err := provider.RemoveLogin(env, key, target)
	if err != nil {
		return loginRefusalFrame(rid, err), true, nil
	}
	remaining := len(provider.ListLogins(env, key).Logins)
	name := slug
	if pconfig != nil {
		name = pconfig.Label()
	}
	return ok(rid, tuigateway.Plain(tuigateway.ModelDisconnectResult{Slug: slug, Name: name, Disconnected: true,
		Login: removal.Login.Name, Remaining: int64(remaining)})), true, nil
}

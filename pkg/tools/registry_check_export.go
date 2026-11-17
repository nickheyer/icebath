package tools

import "context"

// Availability verdict of a probe through the TTL cache, running it when the cache is cold
func (r *Registry) CheckCached(ctx context.Context, env *Env, fn func(*Env) bool) bool {
	return r.checkCached(ctx, env, fn)
}

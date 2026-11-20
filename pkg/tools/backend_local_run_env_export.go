package tools

import "context"

// Run environment the local backend hands a child process, the backend's own env on top
func MakeRunEnv(ctx context.Context, env *Env, childEnv map[string]string) (map[string]string, error) {
	return makeRunEnv(ctx, env, childEnv)
}

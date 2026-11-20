package tools

import "context"

// get_secret of one name through the call's bound scope, the tool env's secrets standing in when the context binds none
func SecretThroughScope(ctx context.Context, env *Env, name string) (*string, error) {
	return secretThroughScope(ctx, env, name)
}

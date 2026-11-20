package tools

import "context"

// Reports whether code runs for a delegate_task child on this context or on an env bound for its checks
func DelegatedChildActive(ctx context.Context, env *Env) bool {
	return IsDelegatedChildContext(ctx) || (env != nil && env.delegatedChild)
}

// Reports a delegate_task child in this process or a subprocess one spawned
func DelegatedChildProcessActive(ctx context.Context, env *Env) bool {
	return IsDelegatedChildContext(ctx) || isDelegatedChildProcessContext(env)
}

// False when a kanban identity is present but this execution does not own it
func DispatcherOwnedWorkerActive(ctx context.Context, env *Env) bool {
	return !IsDelegatedChildContext(ctx) && isDispatcherOwnedWorkerContext(env)
}

// Env the availability checks of a context read, a marked copy while the context runs a delegate_task child
func checkEnvFor(ctx context.Context, env *Env) *Env {
	if !IsDelegatedChildContext(ctx) || (env != nil && env.delegatedChild) {
		return env
	}
	bound := Env{unscopedChecks: true}
	if env != nil {
		bound = *env
	}
	bound.delegatedChild = true
	return &bound
}

// PyContainsKanban reports Python membership of kanban in a toolsets config value
func PyContainsKanban(v any) bool { return pyContainsKanban(v) }

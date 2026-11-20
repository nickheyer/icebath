package tools

// Profile home the env is scoped to, empty for an env of the launch home
func HomeOverride(env *Env) string { return homeOverride(env) }

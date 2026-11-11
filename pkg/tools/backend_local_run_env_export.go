package tools

// Run environment the local backend hands a child process, the backend's own env on top
func MakeRunEnv(env *Env, childEnv map[string]string) (map[string]string, error) {
	return makeRunEnv(env, childEnv)
}

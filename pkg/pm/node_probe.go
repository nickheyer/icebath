package pm

// Installed pm npm and its Node dependency composed over env without provisioning anything
func WithIcebathNodePath(env map[string]string) map[string]string {
	return EnvFor([]string{"npm"}, env)
}

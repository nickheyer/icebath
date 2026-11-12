package tools

// Writes the terminal event and result of one delegation the way its completion path does
func PersistCompletion(env *Env, event, result map[string]any) error {
	return persistCompletion(env, event, result)
}

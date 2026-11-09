package provider

// PersistPluginModelSelection saves the model on the provider as the default with base_url and api_mode when given
func PersistPluginModelSelection(env PoolEnv, model, providerID string, baseURL, apiMode *string) (map[string]any, error) {
	return persistModel(env, model, providerID, persistModelOptions{BaseURL: baseURL, APIMode: apiMode})
}

// NoteSetupFailure records why a plugin flow ends without a pick for the opt in provider setup metric
func NoteSetupFailure(failureClass string) { noteSetupFailure(failureClass) }

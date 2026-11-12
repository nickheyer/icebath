package dashboard

import "context"

// Writes the main model into a profile config after validating it against the profile itself
func WriteProfileModel(ctx context.Context, profileDir, providerName, model, baseURL string) error {
	return writeProfileModel(ctx, profileDir, providerName, model, "", baseURL)
}

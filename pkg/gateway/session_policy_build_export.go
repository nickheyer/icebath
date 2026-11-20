package gateway

import "context"

// BuildPolicy freezes launch params over the profile config, the config secrets it strips gathered into privateSecrets
func BuildPolicy(ctx context.Context, params, cfg map[string]any, privateSecrets *RecoveredSecrets) (*LocalSessionPolicy, error) {
	return buildPolicy(ctx, params, cfg, buildPolicyOptions{PrivateSecrets: privateSecrets})
}

// BindLaunchKey binds a launch key and frozen config secrets in the authority lifetime only
func BindLaunchKey(ctx context.Context, a *SessionAuthority, sid string, policy *LocalSessionPolicy, apiKey *string, configSecrets *RecoveredSecrets) (*LocalSessionPolicy, error) {
	return bindLaunchKey(ctx, a, sid, policy, apiKey, configSecrets)
}

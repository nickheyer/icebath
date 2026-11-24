package provider

import (
	"time"
)

// Signs in to xAI Grok OAuth from setup as an owned login, never the active provider, false on any failure
func runXAIOAuthLoginFromSetup(env PoolEnv) bool {
	SetupInfo(nil, "Signing in to xAI Grok OAuth (SuperGrok / Premium+)...")
	if _, err := SignInOAuthLogin(env, setupUI().Out, "xai-oauth", OAuthSignIn{Timeout: 20 * time.Second}); err != nil {
		setupUI().PrintWarning("xAI Grok OAuth login failed: " + err.Error())
		return false
	}
	return true
}

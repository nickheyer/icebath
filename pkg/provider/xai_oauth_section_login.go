package provider

import (
	"fmt"
	"io"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// XAIOAuthSectionLogin runs the xAI Grok OAuth device sign in printing to out, stored as an owned login without making xAI the active provider
func XAIOAuthSectionLogin(env PoolEnv, out io.Writer) bool {
	fmt.Fprintln(out, "Signing in to xAI Grok OAuth (SuperGrok / Premium+)...")
	if _, err := SignInOAuthLogin(env, out, "xai-oauth", OAuthSignIn{Timeout: 20 * time.Second}); err != nil {
		fmt.Fprintln(out, "xAI Grok OAuth login failed: "+pyrepr.ExceptionMessage(err))
		return false
	}
	return true
}

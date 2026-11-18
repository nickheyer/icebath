package tools

import (
	"fmt"
	"strings"
)

// VercelAuthStatus describes the Vercel auth configuration without exposing secret values
type VercelAuthStatus struct {
	OK          bool
	Label       string
	DetailLines []string
}

var vercelTokenTupleVars = []string{"VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"}

// DescribeVercelAuth reports the auth mode from the secret scope, OIDC first then the token tuple
func DescribeVercelAuth(env *Env) VercelAuthStatus {
	var present, missing []string
	for _, name := range vercelTokenTupleVars {
		if secretValue(env, name) != "" {
			present = append(present, name)
		} else {
			missing = append(missing, name)
		}
	}
	if secretValue(env, "VERCEL_OIDC_TOKEN") != "" {
		details := []string{"mode: OIDC", "active env: VERCEL_OIDC_TOKEN",
			"note: OIDC tokens are development-only; use access-token auth for deployments and long-running processes"}
		if len(present) > 0 {
			details = append(details, "also present: "+strings.Join(present, ", "))
		}
		return VercelAuthStatus{true, "OIDC token via VERCEL_OIDC_TOKEN", details}
	}
	if len(missing) == 0 {
		return VercelAuthStatus{true, "access token + project/team via VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID",
			[]string{"mode: access token", "active env: VERCEL_TOKEN, VERCEL_PROJECT_ID, VERCEL_TEAM_ID"}}
	}
	if len(present) > 0 {
		return VercelAuthStatus{false, fmt.Sprintf("partial access-token auth (missing %s)", strings.Join(missing, ", ")),
			[]string{"mode: incomplete access token", "present env: " + strings.Join(present, ", "), "missing env: " + strings.Join(missing, ", "),
				"recommended: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together"}}
	}
	return VercelAuthStatus{false, "not configured", []string{"recommended: set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID",
		"development-only alternative: set VERCEL_OIDC_TOKEN"}}
}

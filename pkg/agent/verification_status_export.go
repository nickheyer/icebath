package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Best known verification state of a session workspace for read only callers outside the agent
func VerificationStatus(ctx context.Context, env *tools.Env, sessionID, cwd string) (map[string]any, error) {
	return verificationStatus(ctx, env, sessionID, cwd)
}

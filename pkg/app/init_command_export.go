package app

import "github.com/nickheyer/icebath/pkg/tools"

// The init prompt for an explicit existing cwd or the session's active directory, read by the gateway
func BuildInitPromptForCwd(env *tools.Env, cwd, extra string, sessionKey *string) string {
	return buildInitPromptForCwd(env, cwd, extra, sessionKey)
}

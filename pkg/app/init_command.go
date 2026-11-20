package app

import (
	"github.com/nickheyer/icebath/pkg/initprompt"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The /init prompt for an explicit existing cwd or the session's active directory with its AGENTS.md
func buildInitPromptForCwd(env *tools.Env, cwd, extra string, sessionKey *string) string {
	return initprompt.BuildInitPromptForCwd(env, cwd, extra, sessionKey)
}

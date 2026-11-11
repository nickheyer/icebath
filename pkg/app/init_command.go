package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/initprompt"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The /init prompt, an existing AGENTS.md switching it to merge discipline and extra carrying the user's notes
func buildInitPrompt(cwd string, existingFile *string, extra string) string {
	return initprompt.BuildInitPrompt(cwd, existingFile, extra)
}

// The session's active directory, its recorded terminal cwd, else the agent cwd, else the process cwd
func resolveSessionCwd(env *tools.Env, sessionKey *string) string {
	return initprompt.ResolveSessionCwd(env, sessionKey)
}

// The /init prompt for an explicit existing cwd or the session's active directory with its AGENTS.md
func buildInitPromptForCwd(env *tools.Env, cwd, extra string, sessionKey *string) string {
	return initprompt.BuildInitPromptForCwd(env, cwd, extra, sessionKey)
}

// Handles /init, generating or updating AGENTS.md through the live agent's read only scan
func (c *CLI) handleInitCommand(cmd string) {
	key := ""
	msg := buildInitPromptForCwd(c.env, "", commandArg(cmd, false), &key)
	ack := "init.generating"
	if strings.Contains(msg, "UPDATE the existing AGENTS.md") {
		ack = "init.updating"
	}
	c.queuePromptTurn(msg, "/init", c.commandsT(ack, nil))
}

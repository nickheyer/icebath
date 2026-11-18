package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/initprompt"
	"github.com/nickheyer/icebath/pkg/tools"
)

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

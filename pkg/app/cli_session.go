package app

import "os"

// Records this terminal's live session so a later bare -c in this terminal resumes it
func (c *CLI) writeTerminalBreadcrumb() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	WriteBreadcrumb(c.ctx, c.sessionID, cwd)
}

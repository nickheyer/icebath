package mcp

// Reaps orphaned MCP child processes the way the cron scheduler sweep asks for them
func KillOrphanedMCPChildren(includeActive bool, serverName string) {
	killOrphanedMCPChildren(includeActive, serverName)
}

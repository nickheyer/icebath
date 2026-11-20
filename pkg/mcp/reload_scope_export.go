package mcp

// Names of the live connections visible from scope, every connection for a nil scope
func ScopedServerNames(scope *string) map[string]bool {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	out := map[string]bool{}
	for key := range mcpServers {
		if scope == nil || serverVisibleInScope(key, *scope) {
			out[key.Name] = true
		}
	}
	return out
}

// Server that registered the MCP tool, false when no server did
func ToolServerName(tool string) (string, bool) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	name, ok := mcpToolServerNames[tool]
	return name, ok
}

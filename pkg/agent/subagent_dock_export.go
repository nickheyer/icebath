package agent

// Drops every live subagent record
func ClearActiveSubagents() {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	activeSubagents.keys, activeSubagents.m = nil, map[string]map[string]any{}
}

package agent

// Subagent rows of the spawn tree a session id owns, the owner seen only through its session id
func SubagentListRowsForSession(sessionID string) []map[string]any {
	return SubagentListRows(&AIAgent{sessionID: sessionID})
}

// Steer or stop control over the children a session id owns, the owner seen only through its session id
func SubagentControlForSession(action, subagentID string, message any, sessionID string) (map[string]any, error) {
	return SubagentControl(action, subagentID, message, &AIAgent{sessionID: sessionID})
}

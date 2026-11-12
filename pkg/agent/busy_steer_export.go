package agent

// Marks the agent as inside its tool execution phase the way the tool loop does
func (a *AIAgent) SetExecutingToolsForTests(on bool) { a.executingTools = on }

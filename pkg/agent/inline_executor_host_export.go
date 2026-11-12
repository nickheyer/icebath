package agent

import "github.com/nickheyer/icebath/pkg/tools"

// Live state of the agent an inline tool executor reads and mutates
func (a *AIAgent) InlineExecutorHost() tools.ExecutorHost { return agentExecutorHost{a} }

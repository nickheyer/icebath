package agent

import "github.com/nickheyer/icebath/pkg/tools"

// Input tokens the session has spent
func (a *AIAgent) SessionInputTokens() int { return a.sessionInputTokens }

// Output tokens the session has spent
func (a *AIAgent) SessionOutputTokens() int { return a.sessionOutputTokens }

// Seconds each of the recent API calls took, oldest first
func (a *AIAgent) APILatencyHistory() []float64 { return append([]float64{}, a.apiLatencyHistory...) }

// Output tokens each of the recent API calls produced, oldest first
func (a *AIAgent) APIOutputHistory() []int { return append([]int{}, a.apiOutputHistory...) }

// Priority tier the agent requests, empty for the normal tier
func (a *AIAgent) ServiceTier() string { return a.serviceTier }

// Queues a plugin notice for the next tool result or user turn, replacing an older one of the plugin
func (a *AIAgent) QueuePluginNotice(pluginKey, note string) { a.queuePluginNotice(pluginKey, note) }

// Live task plan store of the agent, nil before one exists
func (a *AIAgent) TodoStore() *tools.TodoStore { return a.todoStore }

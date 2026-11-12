package agent

import "context"

// Live transcript writer of one delegated task as the RPC readers of its tail see it
type LiveTranscriptWriter = liveTranscriptWriter

// Child agent carrying the delegation identity and live transcript path a registered subagent exposes
func NewSubagentChildForTests(subagentID string, depth int, delegationID, model, liveTranscriptPath string) *AIAgent {
	return &AIAgent{subagentID: subagentID, delegateDepth: depth, delegationID: delegationID, model: model,
		liveTranscriptPath: liveTranscriptPath}
}

// Parent agent whose tool progress callback receives what a child relay forwards
func NewProgressParentForTests(cb func(event, name, preview string, args, extra map[string]any)) *AIAgent {
	return &AIAgent{toolProgressCallback: toolProgressFn(cb)}
}

// Parent agent of one durable conversation whose lineage resolves through the given session store
func NewLineageParentForTests(sessionID string, db SessionStore) *AIAgent {
	return &AIAgent{sessionID: sessionID, sessionDB: db}
}

// Registers a running child under its subagent id with the exact owner authority it reports to
func RegisterChild(ctx context.Context, child, parent *AIAgent, goal, ownerSessionID string, ownerTransport, ownerSessionRecord any) string {
	return registerChild(ctx, child, parent, goal, ownerSessionID, ownerTransport, ownerSessionRecord)
}

// Retires a live child record into the recent roster
func UnregisterSubagent(subagentID string, child any) { unregisterSubagent(subagentID, child) }

// Closes steering of a live child and answers the steer text still queued for it
func CloseSubagentSteering(subagentID string, child any) string {
	return closeSubagentSteering(subagentID, child)
}

// Relay of one single task child's progress events, nil when the parent has no spinner or callback
func BuildChildProgressCallback(taskIndex int, goal string, parent *AIAgent, subagentID string) any {
	return buildChildProgressCallback(taskIndex, goal, parent, 1, subagentID, "", nil, "", nil, nil, nil)
}

// Fires one tool progress event through a progress callback the way a running agent does
func FireToolProgress(cb any, event, name string) {
	callToolProgress(&AIAgent{toolProgressCallback: cb}, event, name, nil)
}

// Opens the live transcript of one delegated task under the default transcript root
func NewLiveTranscriptWriter(ctx context.Context, delegationID string, taskIndex int, goal string) *LiveTranscriptWriter {
	return newLiveTranscriptWriter(ctx, delegationID, taskIndex, goal, nil, "")
}

// Appends one role line of free text to the live transcript
func (w *liveTranscriptWriter) Event(role, text string) { w.event(role, text) }

// Appends one tool result line with no duration and no error flag
func (w *liveTranscriptWriter) ToolResult(name string, result any) {
	w.toolResult(name, result, nil, false)
}

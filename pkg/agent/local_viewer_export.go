package agent

// Turn scoped sinks a local viewer turn installs on a cached agent and restores afterwards
type LocalViewerSinks struct {
	ToolGen         any
	PrintFn         func(string)
	Thinking        any
	Reaction        func(string)
	Reasoning       any
	ReadTerminal    any
	ReadPreview     any
	DrivePreview    any
	ReadWindowBelow any
	Theme           any
	Tour            any
	SetupBeat       any
	Connection      any
}

// Sinks currently installed on the agent
func (a *AIAgent) LocalViewerSinks() LocalViewerSinks {
	return LocalViewerSinks{ToolGen: a.toolGenCallback, PrintFn: a.printFn, Thinking: a.thinkingCallback,
		Reaction: a.reactionCallback, Reasoning: a.reasoningCallback, ReadTerminal: a.readTerminalCallback,
		ReadPreview: a.readPreviewCallback, DrivePreview: a.drivePreviewCallback, ReadWindowBelow: a.readWindowBelowCallback,
		Theme: a.themeCallback, Tour: a.tourCallback, SetupBeat: a.setupBeatCallback, Connection: a.connectionCallback}
}

// Installs every sink of the set on the agent
func (a *AIAgent) SetLocalViewerSinks(s LocalViewerSinks) {
	a.toolGenCallback, a.printFn, a.thinkingCallback, a.reactionCallback = s.ToolGen, s.PrintFn, s.Thinking, s.Reaction
	a.reasoningCallback, a.readTerminalCallback, a.readPreviewCallback = s.Reasoning, s.ReadTerminal, s.ReadPreview
	a.drivePreviewCallback, a.readWindowBelowCallback, a.themeCallback = s.DrivePreview, s.ReadWindowBelow, s.Theme
	a.tourCallback, a.setupBeatCallback, a.connectionCallback = s.Tour, s.SetupBeat, s.Connection
}

// Whether a registered active subagent, or for a queued spawn its registered parent, descends from the turn agent
func IsActiveSessionSubagent(turnAgent *AIAgent, subagentID, parentID string, spawnRequested bool) bool {
	if subagentID == "" || turnAgent == nil {
		return false
	}
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	record := activeSubagents.get(subagentID)
	if record == nil && spawnRequested && parentID != "" {
		record = activeSubagents.get(parentID)
	}
	return record != nil && isDescendantOf(record["agent"], turnAgent, 8)
}

// Installs the sink of auto title failures, nil restoring the auxiliary failure notice
func (a *AIAgent) SetTitleFailureCallback(cb func(task string, err error)) {
	if cb == nil {
		a.titleFailureCallback = nil
		return
	}
	a.titleFailureCallback = cb
}

// Installs the callback the titler hands a persisted title and its provenance
func (a *AIAgent) SetOnSessionTitle(cb func(title, source string)) {
	if cb == nil {
		a.onSessionTitle = nil
		return
	}
	a.onSessionTitle = cb
}

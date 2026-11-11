package agent

import "github.com/nickheyer/icebath/pkg/tools"

// Hands a submit time user row to the next turn under the persistence lock, leaving the persist overrides alone
func (a *AIAgent) AdoptPendingCLIUserMessage(staged map[string]any) {
	defer persistLock(a)()
	a.pendingCLIUserMessage = staged
}

// Moves the workspace the agent reports and resolves relative paths against
func (a *AIAgent) SetSessionCwd(cwd string) { a.sessionCwd = cwd }

// Re anchors subdirectory hint discovery at a new working directory when the agent tracks hints
func (a *AIAgent) RebindSubdirectoryHints(workingDir string) {
	if hints, ok := a.subdirectoryHints.(*tools.SubdirectoryHintTracker); ok && hints != nil {
		hints.RebindWorkingDir(workingDir)
	}
}

// User turns this agent has started
func (a *AIAgent) UserTurnCount() int { return a.userTurnCount }

// Model API calls of the current conversation loop
func (a *AIAgent) APICallCount() int { return a.apiCallCount }

// Marks the session store this agent holds as its own to close
func (a *AIAgent) SetOwnsSessionDB(owns bool) { a.ownsSessionDB = owns }

// Session title the system prompt uses before the store row carries one
func (a *AIAgent) SetSessionTitleHint(hint string) { a.sessionTitleHint = hint }

// Whether the context cwd came from the app launch rather than user intent
func (a *AIAgent) SetContextCwdIsLaunchArtifact(launch bool) { a.contextCwdIsLaunchArtifact = launch }

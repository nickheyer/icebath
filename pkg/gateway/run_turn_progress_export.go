package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Tool start payload of the lifecycle contract with the call id, name and bounded args
func ToolLifecyclePayload(callID, toolName, args any) *pyjson.Object {
	return toolLifecyclePayload(callID, toolName, args)
}

// Tool complete payload with the error flag and the verbose result block
func ToolCompletePayload(ctx context.Context, callID, toolName, args, result any, isError *bool, verbose bool) *pyjson.Object {
	return toolCompletePayload(ctx, callID, toolName, args, result, isError, verbose)
}

// Current physical session of a conversation, the local receipt entry for a local session
func PhysicalTarget(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	return physicalTarget(ctx, a, ref)
}

// Seeds the agent compression counter from the count the live session carried
func SeedCompressionCount(live *LiveSession, agent any) { seedCompressionCount(live, agent) }

// Captured result of the executing admission, nil outside an execution
func ExecutionResultOf(ctx context.Context) *pyjson.Object { return executionResultOf(ctx) }

// Whether the active launch policy shows reasoning for a session with no flag of its own
func LaunchShowsReasoningActive(ctx context.Context, a *SessionAuthority) (bool, error) {
	policy, _ := ActivePolicy(ctx).(*LocalSessionPolicy)
	return launchShowsReasoning(ctx, policy, a)
}

// Whether the active launch is an Ink or Desktop launch whose viewer defaults reasoning from config
func ConfigDefaultLaunch(ctx context.Context) bool {
	policy, _ := ActivePolicy(ctx).(*LocalSessionPolicy)
	return policy != nil && configDefaultSources[policy.Source]
}

// Committed CLI turn fields of the admission whose event id is the message id, empty for any other turn
func CLITurn(ctx context.Context, messageID any) *pyjson.Object { return cliTurn(ctx, messageID) }

// Generation the session is executing, false when no turn holds the running claim
func (a *SessionAuthority) ExecutingGeneration(sessionID string) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	generation, ok := a.executing[sessionID]
	return generation, ok
}

// Epoch seconds of the entry's last resume mark, false when it was never marked
func (e *SessionEntry) LastResumeMarkedTimestamp() (float64, bool) {
	if e.LastResumeMarkedAt == nil {
		return 0, false
	}
	return e.LastResumeMarkedAt.timestamp(), true
}

// Author of the input the executing admission carries, nil outside an execution
func AdmissionAuthorOf(ctx context.Context) any { return admissionAuthorOf(ctx) }

// Whether a request service tier value is the absent option sentinel
func IsAPIRequestOptionMissing(value any) bool { return value == apiRequestOptionMissing }

// Failed never run result of a session whose working folder is gone
func WorkspaceUnavailableResult(ctx context.Context, policy *LocalSessionPolicy) *pyjson.Object {
	return workspaceUnavailableResult(ctx, policy)
}

// Model the session's next turn runs, the profile's current default when following
func (p *LocalSessionPolicy) CurrentModel(ctx context.Context, a *SessionAuthority) (*string, error) {
	_, model, err := p.effective(ctx, a, false)
	return model, err
}

// Session info frame of a local session as its viewers render it
func LocalSessionInfo(ctx context.Context, a *SessionAuthority, ref SessionRef) (*pyjson.Object, error) {
	return localSessionInfo(ctx, a, ref)
}

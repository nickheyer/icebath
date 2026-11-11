package agent

import "github.com/nickheyer/icebath/pkg/tools"

// Persistence cursor of the current user turn in the message list, nil before any turn
func (a *AIAgent) PersistUserMessageIdx() *int { return a.persistUserMessageIdx }

// Replaces the tool environment the agent hands its tools, a session scoped env for a gateway turn
func (a *AIAgent) SetToolEnv(env *tools.Env) { a.env = env }

// Replaces the step callback fired after each model iteration
func (a *AIAgent) SetStepCallback(cb any) { a.stepCallback = cb }

// Replaces the interim assistant message callback
func (a *AIAgent) SetInterimAssistantCallback(cb any) { a.interimAssistantCallback = cb }

// Replaces the status callback
func (a *AIAgent) SetStatusCallback(cb any) { a.statusCallback = cb }

// Replaces the notice callback
func (a *AIAgent) SetNoticeCallback(cb any) { a.noticeCallback = cb }

// Replaces the notice clear callback
func (a *AIAgent) SetNoticeClearCallback(cb any) { a.noticeClearCallback = cb }

// Replaces the lifecycle event callback
func (a *AIAgent) SetEventCallback(cb func(string, map[string]any)) { a.eventCallback = cb }

// Replaces the background review callback
func (a *AIAgent) SetBackgroundReviewCallback(cb any) { a.backgroundReviewCallback = cb }

// Replaces the clarify callback
func (a *AIAgent) SetClarifyCallback(cb any) { a.clarifyCallback = cb }

// Sets the memory notification mode off, on or verbose
func (a *AIAgent) SetMemoryNotifications(mode string) { a.memoryNotifications = mode }

// Sets the service tier sent with each request, nil for the provider default
func (a *AIAgent) SetServiceTier(tier *string) {
	a.serviceTier = ""
	if tier != nil {
		a.serviceTier = *tier
	}
}

// Sets the per turn iteration budget
func (a *AIAgent) SetMaxIterations(n int) { a.maxIterations = n }

// Closes the codex app server child the agent holds
func (a *AIAgent) CloseCodexSession() { a.closeCodexSession() }

// Whether the interim message delivery already carried this text to the user
func (a *AIAgent) InterimTextWasDelivered(text string) bool { return a.interimTextWasDelivered(text) }

// Whether a subagent terminal status is a failure the parent surfaces
func IsSubagentFailureStatus(status any) bool {
	s, ok := status.(string)
	return ok && subagentFailureStatuses[s]
}

// One clean user facing line for a failed subagent
func FormatSubagentFailureLine(goal, status string, errText, durationSeconds, failureReason any) string {
	return formatSubagentFailureLine(goal, status, errText, durationSeconds, failureReason)
}

// Routing overrides the previous gateway turn layered onto the request overrides
func (a *AIAgent) GatewayTurnRequestOverrides() map[string]any { return a.gatewayTurnRequestOverrides }

// Records the routing overrides this gateway turn layered onto the request overrides
func (a *AIAgent) SetGatewayTurnRequestOverrides(v map[string]any) { a.gatewayTurnRequestOverrides = v }

// Status callback the agent holds
func (a *AIAgent) StatusCallback() any { return a.statusCallback }

// Interim assistant callback the agent holds
func (a *AIAgent) InterimAssistantCallback() any { return a.interimAssistantCallback }

// Tool progress callback the agent holds as installed
func (a *AIAgent) RawToolProgressCallback() any { return a.toolProgressCallback }

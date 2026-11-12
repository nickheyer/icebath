package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent the CLI holds and drives, the gateway agent in production and any object with its surface in a case
type cliAgent interface {
	StreamJSONAgent
	APIMode() string
	BaseURL() string
	ClearInterrupt()
	ConfigContextLength() *int
	ConnectionRespond(opID string, result any) (any, error)
	ConnectionRetry(name string) (any, error)
	CreditsCapture() *provider.CreditsCapture
	Interrupt()
	InterruptRequested() bool
	MaxIterations() int
	Model() string
	MuteNotificationReply() bool
	NotificationConfig() map[string]any
	PinReasoning(config map[string]any) bool
	Provider() string
	QuietMode() bool
	ReasoningConfig() map[string]any
	Redirect(text string) any
	ResetSessionState()
	RunConversation(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error)
	SessionID() string
	SessionMessages() []map[string]any
	SetPendingCLIUserMessage(row map[string]any)
	StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func())
	SetReasoningCallback(callback func(text string))
	SetReasoningConfig(config map[string]any)
	SetSessionID(value string)
	SetToolProgressMode(mode string)
	StageTurnReasoning(config map[string]any)
	StatusBarUsage(ctx context.Context) agent.StatusBarUsage
	Steer(text string) any
	SwitchModel(newModel, newProvider string, once, restore bool, alias string) (bool, error)
	HardInterrupt()
	NotificationPolicySnapshot(cfg map[string]any) func()
	NotificationTurn(ctx context.Context, muted bool, sessionID string) (context.Context, func())
	RestoreReasoning(config map[string]any)
	SetVoiceTurnPending(pending bool)
	SummarizeAPIError(err error) string
	TodoStore() *tools.TodoStore
	gateway() *GatewayAgent
	refreshStats(turn *gatewayTurn) bool
	setSessionMessages(messages []map[string]any)
	setTodoStore(store *tools.TodoStore)
}

var _ cliAgent = (*GatewayAgent)(nil)

// Stages the next turn's CLI user row and appends it to the transcript, the gateway agent holding no persist lock
func (a *GatewayAgent) StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func()) {
	a.SetPendingCLIUserMessage(staged)
	appendToTranscript()
}

// Python class name, the name an attribute error about the agent carries
func (a *GatewayAgent) PyTypeName() string { return "GatewayAgent" }

// Gateway agent behind the surface, whose session figures the CLI's usage and switch readers take
func (a *GatewayAgent) gateway() *GatewayAgent { return a }

// Gateway agent behind a held agent, nil when the CLI holds none
func agentGateway(a cliAgent) *GatewayAgent {
	if a == nil {
		return nil
	}
	return a.gateway()
}

// Replaces the transcript the agent mirrors with a rewound history
func (a *GatewayAgent) setSessionMessages(messages []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionMessages = messages
}

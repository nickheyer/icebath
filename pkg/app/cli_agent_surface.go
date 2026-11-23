package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One agent value the turn goroutine publishes for the render and signal goroutines to read
type cliAgentSlot struct{ a cliAgent }

// Agent this CLI holds, read from the slot the turn goroutine publishes into
func (c *CLI) agentRef() cliAgent {
	if s := c.agent.Load(); s != nil {
		return s.a
	}
	return nil
}

// Publishes the agent this CLI holds to every goroutine that reads it
func (c *CLI) setAgent(a cliAgent) { c.agent.Store(&cliAgentSlot{a}) }

// Agent the CLI holds and drives, the gateway agent in production and any object with its surface in a case
type cliAgent interface {
	StreamJSONAgent
	APIMode() string
	BaseURL() string
	ClearInterrupt()
	Interrupt()
	InterruptRequested() bool
	MaxIterations() int
	Model() string
	MuteNotificationReply() bool
	Provider() string
	RunConversation(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error)
	SessionID() string
	SessionMessages() []map[string]any
	SetPendingCLIUserMessage(row map[string]any)
	StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func())
	SetReasoningCallback(callback func(text string))
	SetToolProgressMode(mode string)
	SwitchModel(newModel, newProvider string) (bool, error)
	NotificationTurn(ctx context.Context, muted bool, sessionID string) (context.Context, func())
	SummarizeAPIError(err error) string
	TodoStore() *tools.TodoStore
	gateway() *GatewayAgent
	refreshStats(turn *gatewayTurn) bool
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

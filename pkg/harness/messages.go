// Bridge between the agent framework and a Bubble Tea program, turning callbacks into messages and prompts into answers
package harness

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

// Delivers a message into the program from any goroutine
type Sender func(msg any)

// Agent event of one run, text deltas merged per frame
type EventMsg struct {
	Run   string
	Event agent.Event
}

// Output or progress a running tool reported
type ProgressMsg struct {
	Run      string
	Progress tool.Progress
}

// Approval a person must answer
type ApprovalMsg struct {
	Pending *Pending
}

// Approval that was answered, cancelled or timed out
type ApprovalDoneMsg struct {
	ID      int
	Run     string
	Verdict policy.Verdict
	Err     error
}

// Run that reached its end
type RunDoneMsg struct {
	Run      string
	Response protocol.Response
	Err      error
	Duration time.Duration
}

// Background shell job change
type JobMsg struct {
	Event shell.JobEvent
}

// One provider attempt with the telemetry as it stood afterwards
type AttemptMsg struct {
	Attempt   provider.Attempt
	Telemetry Telemetry
}

// Model chain moved on from a failing model
type FailoverMsg struct {
	From int
	Err  error
}

// Model chain member that answered
type ServedMsg struct {
	Index int
}

// MCP server connection change
type MCPMsg struct {
	Server string
	State  mcp.State
	Err    error
	Tools  []string
}

// Log line an MCP server sent
type MCPLogMsg struct {
	Line LogLine
}

// Session written to the store
type SavedMsg struct {
	Session string
	Path    string
	// Listing entry of the session as saved, so a card need not read the store again
	Info session.Info
	Err  error
}

// Fact store change of one session
type FactMsg struct {
	Session string
	Change  facts.Change
}

// Mission store change of one session
type MissionMsg struct {
	Session string
	Mission mission.Mission
	Op      mission.Op
}

// Hook output that is not part of a verdict, such as plain text a hook printed
type HookOutputMsg struct {
	Run  string
	Text string
}

type runKey struct{}

// Carries the run id so tools and askers can name the run they serve
func WithRun(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runKey{}, id)
}

// Run id carried by the context, empty when none was set
func RunFrom(ctx context.Context) string {
	id, _ := ctx.Value(runKey{}).(string)
	return id
}

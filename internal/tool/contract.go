package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type Hooks interface {
	Has(event string) bool
	PreToolCall(ctx context.Context, c *Call, args Args) (block string, modified Args, err error)
	PostToolCall(ctx context.Context, ev Event)
	TransformResult(ctx context.Context, ev Event) (string, bool)
	RequestMiddleware(ctx context.Context, c *Call, args Args) (payload, original Args, trace []Trace, err error)
	ExecutionMiddleware(ctx context.Context, c *Call, args, original Args, next func(Args) Result) Result
	Approval(ctx context.Context, event string, h ApprovalHook)
	HumanInput(ctx context.Context, event string, r HumanInput)
}

type ApprovalHook struct {
	Prompt        ApprovalPrompt
	Choice        string
	TurnID        string
	ToolCallID    string
	SessionID     string
	RequestID     string
	RequestDigest string
	DecidedBy     string
	Coalesced     bool
	Cancelled     string
}

type Event struct {
	Call      *Call
	Args      Args
	Result    Result
	Duration  time.Duration
	Status    string
	ErrorType string
	ErrorText string
}

type EditGate interface {
	Check(ctx context.Context, c *Call, args Args) (block Result, blocked bool, err error)
}

type Connectors interface {
	Dispatch(ctx context.Context, c *Call, args Args) Result
	Batch(ctx context.Context, c *Call, entries []Args, each func(ctx context.Context, c *Call, args Args) Result) Result
	Search(ctx context.Context, c *Call, queries []string) ([][]ConnectorTool, string)
	Describe(ctx context.Context, c *Call, names []string) (map[string]ConnectorTool, string)
	Unavailable(ctx context.Context, failure, verb string, names []string) json.RawMessage
}

type ConnectorTool struct {
	Name        string
	Connector   string
	Slug        string
	Description string
	Parameters  json.RawMessage
}

type MCPServers interface {
	Unavailable(ctx context.Context, server string) (string, bool)
}

type SearchMetrics interface {
	ToolSearch(ctx context.Context, scope string, hits int, outcome string)
}

type CallObserver interface {
	Called(c *Call)
}

type HumanWait interface {
	Seconds(sessionKey string) float64
	Ceiling() time.Duration
}

type SideModel interface {
	Complete(ctx context.Context, task string, req wire.Request) (*wire.Response, error)
	MainModel() (provider, model string)
	Vision(ctx context.Context) (VisionRoute, error)
}

type VisionRoute struct {
	Provider string
	Model    string
	Native   bool
	Probe    string
}

type ProgressSink interface {
	Progress(c *Call, message string, fields map[string]any)
	Output(c *Call, chunk string)
}

type NoticeSink interface {
	Post(n Notice)
}

type Notice struct {
	Kind       string
	SessionKey string
	TaskID     string
	ProcessID  string
	Text       string
	Route      Target
}

type ManagedGateway interface {
	Enabled(ctx context.Context, fresh bool) bool
	UnavailableMessage(ctx context.Context, capability string, fresh bool) string
}

type Credentials interface {
	XAIPresent(ctx context.Context) (bool, error)
	Suppressed(ctx context.Context, provider, source string) bool
	Pooled(ctx context.Context, pool string) (string, error)
}

type PluginToolsets interface {
	Toolsets(ctx context.Context) ([]Configurable, error)
	Keys(ctx context.Context) ([]string, error)
	PortableMCPServers(ctx context.Context) ([]string, error)
}

package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	HookPreLLMCall      = "pre_llm_call"
	HookPreAPIRequest   = "pre_api_request"
	HookPostAPIRequest  = "post_api_request"
	HookAPIRequestError = "api_request_error"
	HookTransformOutput = "transform_llm_output"
	HookPostLLMCall     = "post_llm_call"
	HookSessionStart    = "on_session_start"
	HookSessionEnd      = "on_session_end"
	HookSessionFinalize = "on_session_finalize"
	HookSessionReset    = "on_session_reset"
	HookPostCompaction  = "post_compaction"
	HookPreVerify       = "pre_verify"
	HookStreamStart     = "on_stream_start"
	HookStreamDelta     = "on_stream_delta"
	HookStreamEnd       = "on_stream_end"
	HookInterimMessage  = "on_interim_message"

	StreamDeltaText      = "text"
	StreamDeltaReasoning = "reasoning"
)

type Hooks interface {
	Has(event string) bool
	PreLLMCall(ctx context.Context, e PreLLMCall) (contexts, blocks []string, err error)
	PreAPIRequest(ctx context.Context, e APIRequest) error
	PostAPIRequest(ctx context.Context, e APIResponse)
	APIRequestError(ctx context.Context, e APIFailure)
	TransformOutput(ctx context.Context, e OutputTransform) (text string, changed bool)
	HeldTransforms(ctx context.Context) []HeldTransform
	PostLLMCall(ctx context.Context, e TurnEnd)
	SessionStart(ctx context.Context, e SessionEvent)
	SessionEnd(ctx context.Context, e TurnEnd)
	SessionFinalize(ctx context.Context, e SessionEvent)
	SessionReset(ctx context.Context, e SessionEvent)
	PostCompaction(ctx context.Context, payload json.RawMessage)
	PreVerify(ctx context.Context, e PreVerify) (nudge string, ok bool)
	Stream(ctx context.Context, e StreamHook)
	SideQuestionGranted(ctx context.Context) bool
	LiveNotes(ctx context.Context, sessionID string) []string
}

type PreLLMCall struct {
	SessionID       string
	TaskID          string
	TurnID          string
	UserMessage     wire.Message
	History         []*Message
	FirstTurn       bool
	Model           string
	Platform        string
	ParentSessionID string
	SenderID        string
}

type APIRequest struct {
	TaskID            string
	TurnID            string
	RequestID         string
	SessionID         string
	UserMessage       wire.Message
	History           []*Message
	Platform          string
	Model             string
	Provider          string
	BaseURL           string
	Mode              wire.Mode
	APICalls          int
	RetryCount        int
	RequestMessages   []wire.Message
	SystemPrompt      string
	MessageCount      int
	ToolCount         int
	ApproxInputTokens int
	RequestChars      int
	MaxTokens         int
	StartedAt         time.Time
	Trace             []model.TraceEntry
	Request           json.RawMessage
	Call              *wire.Call
}

type APIResponse struct {
	TaskID         string
	TurnID         string
	RequestID      string
	SessionID      string
	Platform       string
	Model          string
	Provider       string
	BaseURL        string
	Mode           wire.Mode
	APICalls       int
	Duration       time.Duration
	StartedAt      time.Time
	EndedAt        time.Time
	FirstChunkAt   time.Time
	FinishReason   string
	MessageCount   int
	ResponseModel  string
	Response       json.RawMessage
	Usage          *wire.Usage
	ContextLength  int
	Assistant      *wire.Message
	AssistantChars int
	ToolCallCount  int
	MoAReferences  json.RawMessage
}

type APIFailure struct {
	TaskID       string
	TurnID       string
	RequestID    string
	SessionID    string
	Platform     string
	Model        string
	Provider     string
	BaseURL      string
	Mode         wire.Mode
	APICalls     int
	Duration     time.Duration
	StartedAt    time.Time
	EndedAt      time.Time
	Status       int
	RetryCount   int
	MaxRetries   int
	Retryable    *bool
	Reason       string
	ErrorType    string
	ErrorMessage string
	Request      json.RawMessage
}

type OutputTransform struct {
	ResponseText string
	SessionID    string
	Model        string
	Platform     string
	TurnID       string
}

type HeldTransform interface {
	Transform(ctx context.Context, e OutputTransform) (text string, err error)
}

type TurnEnd struct {
	SessionID         string
	TaskID            string
	TurnID            string
	UserMessage       wire.Message
	Response          string
	History           []*Message
	Model             string
	Platform          string
	SideQuestionToken string
	Completed         bool
	Failed            bool
	Interrupted       bool
	Exit              ExitReason
}

type SessionEvent struct {
	SessionID string
	Model     string
	Platform  string
}

type PreVerify struct {
	SessionID     string
	Platform      string
	Model         string
	Coding        bool
	Attempt       int
	FinalResponse string
	ChangedPaths  []string
}

type StreamHook struct {
	Event           string
	TurnID          string
	Iteration       int
	SessionID       string
	Model           string
	Provider        string
	Surface         string
	Delta           string
	Kind            string
	FinalText       string
	Finished        bool
	Error           string
	Text            string
	AlreadyStreamed bool
}

type Memory interface {
	TurnStart(ctx context.Context, e MemoryTurn) error
	Prefetch(ctx context.Context, sessionID, query string) (string, error)
	DescribeRecall() string
	SyncTurn(ctx context.Context, e MemorySync) error
	QueuePrefetch(ctx context.Context, sessionID, query string) error
	PreCompress(ctx context.Context, msgs []*Message) (string, error)
	SessionSwitch(ctx context.Context, oldID, newID string, reason string) error
	Commit(ctx context.Context, msgs []*Message) error
	Shutdown(ctx context.Context, msgs []*Message) error
	Tools() []tool.Tool
	NudgeInterval() int
	Scrub(text string) string
	NewStreamScrubber() StreamScrubber
}

type MemoryTurn struct {
	Number int
	Query  string
	Author *Author
}

type MemorySync struct {
	SessionID string
	User      string
	Response  string
	Messages  []*Message
	Author    *Author
}

type Skills interface {
	AutoLoad(ctx context.Context) (text string, loaded, missing []string, err error)
	NudgeInterval() int
	SetWriteOrigin(ctx context.Context, origin string, attended bool) context.Context
}

type LSP interface {
	Settle(ctx context.Context, sessionID string) []LSPNote
	Notes(ctx context.Context, c *tool.Call) []string
}

type LSPNote struct {
	Path string
	Text string
}

type StopGate interface {
	Name() string
	Check(ctx context.Context, s StopState) (StopNudge, bool)
}

type StopState struct {
	SessionID    string
	Platform     string
	Model        string
	Final        string
	Messages     []*Message
	ChangedPaths []string
	Attempts     int
	Coding       bool
}

type StopNudge struct {
	Text         string
	FinishReason string
	Scaffold     Scaffold
	HideAnswer   bool
	Status       string
}

type TurnRecovery interface {
	Recover(ctx context.Context, r *TurnResult) (retry bool, wait time.Duration)
}

type ToolObserver interface {
	Observe(ctx context.Context, o tool.Outcome)
	Metadata(ctx context.Context, o tool.Outcome) json.RawMessage
}

type Metrics interface {
	TurnStart(ctx context.Context, t TurnFactsMetric)
	TurnFinish(ctx context.Context, exit ExitReason, final string, interrupted, failed bool)
	APICall(ctx context.Context, m APICallMetric)
	ToolBatch(ctx context.Context, m ToolBatchMetric)
	Guardrail(ctx context.Context, d tool.GuardDecision, stall string)
	Compaction(ctx context.Context, payload json.RawMessage)
}

type TurnFactsMetric struct {
	SessionID  string
	TurnID     string
	Provider   string
	Model      string
	Background bool
}

type APICallMetric struct {
	Provider     string
	Model        string
	FinishReason wire.FinishReason
	Issue        string
}

type ToolBatchMetric struct {
	Provider string
	Model    string
	Outcomes []ToolOutcomeMetric
}

type ToolOutcomeMetric struct {
	Name   string
	Failed bool
}

const (
	ReplyIssueNone          = "none"
	ReplyIssueEmpty         = "empty"
	ReplyIssueReasoningOnly = "reasoning_only"
	ReplyIssueRefusal       = "refusal"
	ReplyIssueTruncated     = "truncated_length"
)

type Trace interface {
	BeginTurn(ctx context.Context, t TraceTurn) (context.Context, func(outcome string))
}

type TraceTurn struct {
	SessionID       string
	ParentSessionID string
	Platform        string
	Model           string
	SessionCwd      string
	TurnCwd         string
	TurnID          string
	TaskID          string
	Metadata        json.RawMessage
}

const (
	TraceSuccess   = "success"
	TraceFailed    = "failed"
	TraceCancelled = "cancelled"
	TraceTimedOut  = "timed_out"
)

type Catalog interface {
	T(ctx context.Context, key string, args map[string]any) string
}

type StreamScrubber interface {
	Feed(delta string) string
	Flush() string
}

type ActivityBridge interface {
	Touched(ctx context.Context, a *Agent)
}

type Mixture interface {
	DecodeTurn(m wire.Message) (wire.Message, *MixtureTurn)
	Prepare(ctx context.Context, t MixtureTurn, msgs []wire.Message, h MixtureHost) (*MixturePrepared, error)
	Rebase(p *MixturePrepared, msgs []wire.Message) *MixturePrepared
	Peel(msgs []wire.Message, p *MixturePrepared) []wire.Message
	ConsumeUsage() (*wire.Usage, float64)
	SaveTrace(ctx context.Context, sessionID, aggregatorOutput string) error
	Metrics() json.RawMessage
}

type MixtureTurn struct {
	Preset string
	Config json.RawMessage
}

type MixturePrepared struct {
	Messages   []wire.Message
	Guidance   string
	CacheKey   string
	Aggregator model.Target
}

type MixtureHost struct {
	SessionID     string
	Touch         func(desc string)
	Emit          func(MixtureEvent)
	Interrupted   func() bool
	CacheDisabled *bool
	CacheTTL      string
}

type MixtureEvent struct {
	Name      string
	Label     string
	Text      string
	Index     int
	Count     int
	RefsDone  int
	RefsTotal int
	Phase     string
}

type Reviewer interface {
	Spawn(ctx context.Context, parent *Agent, r ReviewRequest) bool
	CancelForLiveTurn(parent *Agent)
	TurnStarted()
	TurnFinished()
}

type ReviewRequest struct {
	Snapshot     []*Message
	ReviewMemory bool
	ReviewSkills bool
	TurnID       string
}

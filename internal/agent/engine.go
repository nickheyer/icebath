package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/codex"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	DefaultThresholdPercent = 0.75
	DefaultProtectFirst     = 3
	DefaultProtectLast      = 6

	MemoryContextMaxChars  = 6000
	MemoryContextHeadChars = 4000
	MemoryContextTailChars = 1500

	CompactionPhasePreflight = "preflight"
	CompactionPhaseCompress  = "compress"

	TriggerOverflow  = "overflow"
	TriggerPreAPI    = "pre_api"
	TriggerPostTool  = "post_tool"
	TriggerIdle      = "idle"
	TriggerPreflight = "preflight"
	TriggerManual    = "manual"
)

type ContextEngine interface {
	Name() string
	Counters() EngineCounters
	Settings() EngineSettings
	UpdateFromResponse(u *wire.Usage)
	NoteUsagelessResponse()
	ShouldCompress(promptTokens int) (compress bool, blocked string)
	ShouldCompressPreflight(msgs []*Message) bool
	ShouldDeferPreflight(roughTokens int) bool
	HasContent(msgs []*Message) bool
	Compress(ctx context.Context, in CompressInput) ([]*Message, error)
	PruneToolResults(ctx context.Context, msgs []*Message, currentTokens int) ([]*Message, int)
	Select(ctx context.Context, in SelectInput) ([]wire.Message, bool)
	TurnComplete(ctx context.Context, o TurnObservation)
	SessionStart(ctx context.Context, sessionID string, f SessionFacts)
	SessionEnd(ctx context.Context, sessionID string, msgs []*Message)
	SessionReset()
	CarryOver(ctx context.Context, oldSessionID, newSessionID string)
	Tools() []tool.Tool
	StatusMessage(phase string, def string, approx, threshold int) (string, bool)
	Clone() ContextEngine
	UpdateModel(m ModelFacts)
}

type EngineCounters struct {
	LastPromptTokens     int
	LastCompletionTokens int
	LastTotalTokens      int
	LastRealPromptTokens int
	ThresholdTokens      int
	ContextLength        int
	CompressionCount     int
}

func (c *EngineCounters) Reset() {
	c.LastPromptTokens = 0
	c.LastCompletionTokens = 0
	c.LastTotalTokens = 0
	c.CompressionCount = 0
}

type EngineStatus struct {
	LastPromptTokens int     `json:"last_prompt_tokens"`
	ThresholdTokens  int     `json:"threshold_tokens"`
	ContextLength    int     `json:"context_length"`
	UsagePercent     float64 `json:"usage_percent"`
	CompressionCount int     `json:"compression_count"`
}

func (c EngineCounters) Status() EngineStatus {
	last := max(c.LastPromptTokens, 0)
	s := EngineStatus{
		LastPromptTokens: last,
		ThresholdTokens:  c.ThresholdTokens,
		ContextLength:    c.ContextLength,
		CompressionCount: c.CompressionCount,
	}
	if c.ContextLength != 0 {
		s.UsagePercent = min(100, float64(last)/float64(c.ContextLength)*100)
	}
	return s
}

func (c EngineCounters) ResultPromptTokens() int {
	if c.LastPromptTokens > 0 {
		return c.LastRealPromptTokens
	}
	return c.LastPromptTokens
}

type EngineSettings struct {
	ThresholdPercent  float64
	ProtectFirst      int
	ProtectLast       int
	AnnounceAutomatic bool
}

type CompressInput struct {
	Messages      []*Message
	CurrentTokens int
	Focus         string
	Force         bool
	MemoryContext string
}

type SelectInput struct {
	Request      []wire.Message
	Conversation []*Message
	Incoming     *Message
	BudgetTokens int
}

type TurnObservation struct {
	Messages    []*Message
	Usage       *wire.Usage
	TurnID      string
	TaskID      string
	APICalls    int
	Interrupted bool
	Failed      bool
	Exit        ExitReason
}

type SessionFacts struct {
	OldSessionID   string
	CarryOver      bool
	Platform       string
	Model          string
	ContextLength  int
	ConversationID string
	Home           string
	Extra          map[string]json.RawMessage
}

type ModelFacts struct {
	Model, Provider, BaseURL string
	Mode                     wire.Mode
	ContextLength            int
	MaxTokens                int
}

func AutomaticCompactionStatus(e ContextEngine, phase, def string, approx, threshold int) (string, bool) {
	if !e.Settings().AnnounceAutomatic {
		return "", false
	}
	message, ok := e.StatusMessage(phase, def, approx, threshold)
	if !ok {
		return "", false
	}
	message = trimText(message)
	return message, message != ""
}

type Compactor interface {
	Compact(ctx context.Context, h CompactionHost, r CompactRequest) (*CompactResult, error)
	Micro(ctx context.Context, h CompactionHost, msgs []*Message, final string) ([]*Message, bool)
	PrepareAhead(ctx context.Context, h CompactionHost, msgs []*Message, origin string)
	Feasibility(ctx context.Context, h CompactionHost, estimated int) (warning string)
	RecoverRotated(ctx context.Context, h CompactionHost) ([]*Message, bool, error)
	CancelBeforeCommit(wait bool) bool
	Release()
}

type CompactRequest struct {
	Messages          []*Message
	System            string
	ApproxTokens      int
	TaskID            string
	Focus             string
	Force             bool
	BypassCooldown    bool
	DeferEngineNotice bool
	VerbatimTail      []*Message
	Trigger           string
	OverflowReason    string
	SnapshotCurrent   func() bool
}

type CompactResult struct {
	Messages       []*Message
	System         string
	Committed      bool
	InPlace        bool
	OldSessionID   string
	NewSessionID   string
	MadeProgress   bool
	SkippedForLock bool
	LockHolder     string
	TransientBlock bool
	BlockReason    string
	TimedOut       bool
	UsedFallback   bool
	UserTurn       string
	Warning        string
}

type SessionAdoption struct {
	OldSessionID string
	NewSessionID string
	Messages     []*Message
	InPlace      bool
}

type CompactionHost interface {
	SessionID() string
	ParentSessionID() string
	Source() string
	Platform() string
	Route() *model.Route
	Mode() wire.Mode
	Engine() ContextEngine
	Store() *state.Store
	TurnLease() (holder string, ttl time.Duration)
	RebuildSystemPrompt(ctx context.Context, system string) (string, error)
	AdoptSession(ctx context.Context, a SessionAdoption) error
	MemoryContext(ctx context.Context, msgs []*Message) (string, error)
	TodoSnapshot() (string, bool)
	RefreshTools(ctx context.Context) error
	ResetReadDedup(taskID string)
	Codex() *codex.Session
	InterruptState() (requested, hard bool)
	Touch(ctx context.Context, desc string, p state.ActivityProvenance)
	Emit(e Event)
	Hooks() Hooks
	Metrics() Metrics
}

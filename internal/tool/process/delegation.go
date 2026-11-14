package process

import (
	"context"
	"strings"
)

const EventAsyncDelegation = "async_delegation"

type Delegation interface {
	Restore(ctx context.Context, put func(Event)) (int, error)
	Hold(delegationID string)
	Unhold(delegationID string)
	Attribution(taskID string) (Attribution, bool)
	Model(ctx context.Context) (DelegationModel, error)
	HandoffParent(ctx context.Context, childTaskID string) (HandoffParent, bool)
	RecordHandoff(childTaskID string, h HandedOff)
}

type Lineage interface {
	CompressionTip(ctx context.Context, sessionID string) (string, error)
}

type TerminalSettings interface {
	TerminalEnv(ctx context.Context, name string) (string, error)
}

type Attribution struct {
	Goal         string
	DelegationID string
}

type DelegationModel struct {
	Model         string
	Provider      string
	FallbackChain bool
}

type HandoffParent struct {
	Owner      string
	TaskID     string
	SessionKey string
	Handed     int
}

type DelegationEvent struct {
	ID                   string
	OriginSessionID      string
	Goal                 string
	Goals                []string
	Context              string
	Toolsets             []string
	Role                 string
	Model                string
	Status               string
	Summary              string
	Error                string
	APICalls             *int
	DurationSeconds      *float64
	TotalDurationSeconds *float64
	DispatchedAt         *float64
	CompletedAt          float64
	ExitReason           string
	Truncated            bool
	IsBatch              bool
	TaskFailureNotice    bool
	NTasks               int
	Group                *string
	Results              []DelegationResult
	LastKnownStatus      string
	TaskTranscripts      []TaskTranscript
	TranscriptTails      map[string]string
	GitStateHint         string
	StartupReplay        bool
}

type TaskTranscript struct {
	Index string
	Path  string
}

type DelegationResult struct {
	TaskIndex       int
	Status          string
	Summary         string
	Error           string
	Goal            string
	APICalls        int
	DurationSeconds *float64
	LiveTranscript  string
	Truncated       bool
	ExitReason      string
	HandedOff       []HandedOff
	Orphaned        []Orphan
	Unread          []UnreadCompletion
}

type HandedOff struct {
	SessionID string
	Command   string
	Note      string
}

type Orphan struct {
	SessionID      string
	Command        string
	RuntimeSeconds int
}

type UnreadCompletion struct {
	SessionID  string
	Command    string
	ExitCode   *int
	OutputTail string
}

func (ev Event) interim() bool {
	return ev.Type == EventAsyncDelegation && ev.Delegation != nil && ev.Delegation.TaskFailureNotice
}

func (ev Event) CopyID() string {
	if ev.Type != EventAsyncDelegation || ev.Delegation == nil || ev.interim() {
		return ""
	}
	return ev.Delegation.ID
}

func (ev Event) Diagnostic() bool {
	if ev.Delegation != nil && ev.Delegation.TaskFailureNotice {
		return true
	}
	switch ev.Type {
	case EventWatchDisabled, EventOverflowTripped, EventOverflowReleased:
		return true
	}
	return false
}

func (ev Event) owner() string { return ev.OwnerTaskID }

func (ev Event) spawner() string {
	if ev.OwnerTaskID != "" {
		return ev.OwnerTaskID
	}
	return ev.TaskID
}

func IsChildNotification(ev Event) bool {
	if ev.Type == EventAsyncDelegation || ev.Type == EventDelegationCloseout {
		return false
	}
	return strings.HasPrefix(ev.spawner(), "sa-")
}

const EventDelegationCloseout = "async_delegation_work_closeout"

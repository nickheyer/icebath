package agent

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Calls of a loop manager the loop command and the CLI make, answered in process or by the gateway
type LoopAutomation interface {
	Session() string
	Refresh(ctx context.Context)
	State() *SessionLoopState
	IsActive() bool
	HasLoop() bool
	StatusLine() string
	IsDue() bool
	Set(ctx context.Context, prompt string, intervalSeconds *int64, times int64, until string, route *jsonutil.Object) (*SessionLoopState, error)
	Pause(ctx context.Context, reason string) (*SessionLoopState, error)
	Resume(ctx context.Context) *SessionLoopState
	Clear(ctx context.Context) bool
	FireTick(ctx context.Context) (string, bool)
	AbandonTick(ctx context.Context)
	CompleteTick(ctx context.Context, lastResponse string) LoopDecision
}

// Calls of a heartbeat manager the CLI makes, answered in process or by the gateway
type HeartbeatAutomation interface {
	Session() string
	State() *HeartbeatState
	HasHeartbeat() bool
	IsActive() bool
	StatusLine() (string, error)
	Set(prompt string, intervalSeconds any) (*HeartbeatState, error)
	Pause() *HeartbeatState
	Resume() *HeartbeatState
	Clear() bool
	DuePrompt(now *float64) (string, bool)
	AbandonFire() bool
}

var (
	_ LoopAutomation      = (*LoopManager)(nil)
	_ HeartbeatAutomation = (*HeartbeatManager)(nil)
)

// Session id the loop manager is bound to
func (m *LoopManager) Session() string { return m.SessionID }

// Session id the heartbeat manager is bound to
func (m *HeartbeatManager) Session() string { return m.SessionID }

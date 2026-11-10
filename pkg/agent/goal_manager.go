package agent

import "context"

// GoalManager holds the standing goal state of one session and reads it for the status surfaces
type GoalManager struct {
	ctx             context.Context
	SessionID       string
	DefaultMaxTurns int
	state           *GoalState
}

// NewGoalManager loads the goal of a session, a zero default turn budget reading as twenty
func NewGoalManager(ctx context.Context, sessionID string, defaultMaxTurns int) *GoalManager {
	if defaultMaxTurns == 0 {
		defaultMaxTurns = DefaultGoalMaxTurns
	}
	return &GoalManager{ctx: ctx, SessionID: sessionID, DefaultMaxTurns: defaultMaxTurns, state: LoadGoal(ctx, sessionID)}
}

// Refresh reads the state again after another writer moved it
func (m *GoalManager) Refresh() { m.state = LoadGoal(m.ctx, m.SessionID) }

// State returns the loaded goal, nil when the session has none
func (m *GoalManager) State() *GoalState { return m.state }

// IsActive reports a goal whose status is active
func (m *GoalManager) IsActive() bool { return m.state != nil && m.state.Status == "active" }

// HasGoal reports an active or paused goal
func (m *GoalManager) HasGoal() bool {
	return m.state != nil && (m.state.Status == "active" || m.state.Status == "paused")
}

// HasContract reports a goal that carries a contract
func (m *GoalManager) HasContract() bool { return m.state != nil && m.state.HasContract() }

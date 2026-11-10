package app

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Records that the agent moved this session's goal or loop state during the turn
func (c *CLI) markAutomationStale() {
	c.uiMu.Lock()
	c.automationStale = true
	c.uiMu.Unlock()
}

// Turn budget of a new goal from goals.max_turns, twenty when the config holds none or cannot be read
func (c *CLI) goalMaxTurns() int {
	cfg, err := config.Load(home.Dir(c.ctx), nil)
	if err != nil {
		return 20
	}
	goals, _ := cfg["goals"].(map[string]any)
	raw, ok := goals["max_turns"]
	if !ok || !pyrepr.Truthy(raw) {
		return 20
	}
	n, err := pyint.Of(raw)
	if err != nil || n.Sign() == 0 {
		return 20
	}
	return int(n.Int64())
}

// Goal manager bound to the current session id, rebuilt after the session changed, nil without a session
func (c *CLI) getGoalManager() *goals.GoalManager {
	c.uiMu.Lock()
	sessionID, existing := c.sessionID, c.goalManager
	c.uiMu.Unlock()
	if sessionID == "" {
		return nil
	}
	if existing != nil && existing.SessionID == sessionID {
		return existing
	}
	manager := goals.NewGoalManager(c.ctx, sessionID, int64(c.goalMaxTurns()))
	c.uiMu.Lock()
	c.goalManager = manager
	c.uiMu.Unlock()
	return manager
}

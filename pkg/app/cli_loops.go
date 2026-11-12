package app

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
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

// Whether the session store is the gateway's, and its connection once the gateway session is open
func (c *CLI) automationConnection() (bool, *gatewaycli.GatewayConnection) {
	if c.sessionDB == nil {
		return false, nil
	}
	return true, gatewayAutomationConnection(c)
}

// Goal manager bound to the current session id, the gateway's on a gateway store and none before its session opens
func (c *CLI) getGoalManager() goals.Manager {
	gateway, connection := c.automationConnection()
	if gateway && connection == nil {
		return nil
	}
	c.uiMu.Lock()
	sessionID, existing := c.sessionID, c.goalManager
	c.uiMu.Unlock()
	if sessionID == "" {
		return nil
	}
	if existing != nil && existing.Session() == sessionID {
		return existing
	}
	var manager goals.Manager
	if gateway {
		remote, err := NewGatewayGoalManager(c.ctx, sessionID, connection)
		if err != nil {
			panic(err)
		}
		manager = remote
	} else {
		manager = goals.NewGoalManager(c.ctx, sessionID, int64(c.goalMaxTurns()))
	}
	c.uiMu.Lock()
	c.goalManager = manager
	c.uiMu.Unlock()
	return manager
}

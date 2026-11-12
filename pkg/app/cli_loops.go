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
	return automationManager(c, &c.goalManager,
		func(sid string) goals.Manager { return goals.NewGoalManager(c.ctx, sid, int64(c.goalMaxTurns())) },
		func(sid string, connection *gatewaycli.GatewayConnection) (goals.Manager, error) {
			return NewGatewayGoalManager(c.ctx, sid, connection)
		})
}

// Manager of a session bound slot, the gateway's on a gateway store and none before its session opens
func automationManager[M interface{ Session() string }](c *CLI, slot *M, local func(string) M,
	remote func(string, *gatewaycli.GatewayConnection) (M, error)) M {
	gateway, connection := c.automationConnection()
	if gateway && connection == nil {
		var none M
		return none
	}
	return sessionBoundManager(c, slot, func(sid string) M {
		if !gateway {
			return local(sid)
		}
		manager, err := remote(sid, connection)
		if err != nil {
			panic(err)
		}
		return manager
	})
}

// Manager cached in slot, rebuilt when the session id changed and none while the session id is empty
func sessionBoundManager[M interface{ Session() string }](c *CLI, slot *M, build func(string) M) M {
	c.uiMu.Lock()
	sessionID, existing := c.sessionID, *slot
	c.uiMu.Unlock()
	if sessionID == "" {
		var none M
		return none
	}
	if any(existing) != nil && existing.Session() == sessionID {
		return existing
	}
	manager := build(sessionID)
	c.uiMu.Lock()
	*slot = manager
	c.uiMu.Unlock()
	return manager
}

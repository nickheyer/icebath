package app

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reports whether this session owns a delegation event, a key from before a compression resolved to its continuation
func (c *CLI) ownsProcessNotificationEvent(event map[string]any) bool {
	eventKey := cliStr(event["session_key"], "")
	currentKey := c.sessionID
	if eventKey == "" || currentKey == "" {
		return false
	}
	if eventKey == currentKey {
		return true
	}
	resolved := eventKey
	if c.sessionDB != nil {
		if r, err := c.sessionDB.ResolveResumeSessionID(c.ctx, eventKey); err == nil && r != "" {
			resolved = r
		}
	}
	return resolved == currentKey
}

// Process registry drain and delivery claims, swapped by tests
var (
	drainRegistryNotifications = func(c *CLI) []tools.DrainedNotification {
		return tools.DefaultProcessRegistry().DrainNotifications(c.env, c.sessionID, c.ownsProcessNotificationEvent, false)
	}
	claimEventDelivery    = tools.ClaimEventDelivery
	completeEventDelivery = tools.CompleteEventDelivery
)

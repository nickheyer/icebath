package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
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
		if r, err := c.sessionDB.ResolveResumeSessionID(eventKey); err == nil && r != "" {
			resolved = r
		}
	}
	return resolved == currentKey
}

// Reports whether display.background_process_notifications is off for this session
func (c *CLI) backgroundNotificationsSuppressed() bool {
	mode := cliStr(cliMap(CLIConfig(c.ctx), "display")["background_process_notifications"], "")
	return strings.ToLower(strings.TrimSpace(mode)) == "off"
}

// Process registry drain and delivery claims, swapped by tests
var (
	drainRegistryNotifications = func(c *CLI) []tools.DrainedNotification {
		return tools.DefaultProcessRegistry().DrainNotifications(c.env, c.sessionID, c.ownsProcessNotificationEvent, false)
	}
	claimEventDelivery    = tools.ClaimEventDelivery
	completeEventDelivery = tools.CompleteEventDelivery
)

// Claims this session's finished process and delegation events and queues each batch as pending input
func (c *CLI) drainProcessNotifications(consumer string) {
	var claimed []tools.DrainedNotification
	for _, n := range drainRegistryNotifications(c) {
		claim, ok, err := claimEventDelivery(c.env, n.Event, consumer)
		if err != nil || !ok {
			continue
		}
		claimed = append(claimed, n)
		_ = completeEventDelivery(c.env, n.Event, claim)
	}
	if c.backgroundNotificationsSuppressed() {
		kept := claimed[:0]
		for _, n := range claimed {
			if cliStr(n.Event["type"], "") == "async_delegation" {
				kept = append(kept, n)
			}
		}
		claimed = kept
	}
	for _, batch := range tools.GroupProcessNotifications(claimed) {
		first := batch.Notifications[0]
		var pending any
		switch cliStr(first.Event["type"], "completion") {
		case "completion":
			pending = batch
		case "heartbeat":
			pending = tools.NewTimelineNotification(first.Text, tools.HeartbeatDisplayText(first.Event), tools.HeartbeatDisplayKind, "")
		case "async_delegation":
			pending = tools.TimelineNotificationForDelegation(first.Text, first.Event)
		default:
			pending = first.Text
			if agent.DiagnosticProcessEvent(first.Event) {
				pending = tools.NewTimelineNotification(first.Text, first.Text, "internal_notification", "diagnostic")
			}
		}
		c.pendingInput.Put(pending)
	}
}

// Unwraps a queued input into its text or notification, and whether it was voice transcribed or a seeded query
func (c *CLI) tuiUnwrapInput(userInput any) (any, bool, bool) {
	if batch, ok := userInput.(tools.ProcessNotificationBatch); ok {
		registry := tools.DefaultProcessRegistry()
		rendered := batch.Render(registry)
		if rendered == "" {
			return nil, false, false
		}
		userInput = tools.NewTimelineNotification(rendered, batch.DisplayText(registry), tools.ProcessCompleteDisplayKind, "")
	}
	voice, isVoice := userInput.(*voiceInputMessage)
	if isVoice {
		userInput = voice.Text
	}
	seeded, isSeeded := userInput.(*seededQueryMessage)
	if isSeeded {
		if len(seeded.Images) > 0 {
			userInput = submittedInput{Text: seeded.Text, Images: seeded.Images}
		} else {
			userInput = seeded.Text
		}
	}
	return userInput, isVoice, isSeeded
}

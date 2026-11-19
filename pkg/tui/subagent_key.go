package tui

import (
	"strconv"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Id the store gives a subagent of an event payload
func subagentKey(p tuigateway.SubagentEventPayload) string {
	if id := val(p.SubagentID); id != "" {
		return id
	}
	label := p.Goal
	if label == "" {
		label = "subagent"
	}
	return "sa:" + strconv.FormatInt(p.TaskIndex, 10) + ":" + label
}

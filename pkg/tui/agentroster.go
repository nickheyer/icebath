package tui

import (
	"reflect"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Subagent list of the session it belongs to, as subagent.list reports it
type agentSnapshot struct {
	Sid  string
	Data tuigateway.SubagentListResult
}

// Replaces the agent snapshot only when its session or list changed
func (s *tuiStore) applyAgentSnapshot(sid string, data tuigateway.SubagentListResult) {
	if s.agentSnapshot.Sid != sid || !reflect.DeepEqual(s.agentSnapshot.Data, data) {
		s.agentSnapshot = agentSnapshot{Sid: sid, Data: data}
	}
}

// Roster of the live subagent events merged with the polled list, events keeping their own progress
func mergeAgentRoster(events []subagentProgress, data tuigateway.SubagentListResult) []subagentProgress {
	var order []string
	merged := map[string]subagentProgress{}
	for _, e := range events {
		if _, seen := merged[e.ID]; !seen {
			order = append(order, e.ID)
		}
		merged[e.ID] = e
	}
	for index, s := range data.Subagents {
		id := s.SubagentID
		previous, had := merged[id]
		var next subagentProgress
		if had {
			next = previous
		} else {
			next = subagentProgress{Depth: int(val(s.Depth)), Index: index, ParentID: val(s.ParentID), Notes: []string{}, Thinking: []string{}, TaskCount: 1}
			order = append(order, id)
		}
		next.ID = id
		switch goal := val(s.Goal); {
		case goal != "":
			next.Goal = goal
		case had && previous.Goal != "":
			next.Goal = previous.Goal
		default:
			next.Goal = tr(txt.HubsAgentsStartingGoal)
		}
		if s.DelegationID != nil {
			next.DelegationID = *s.DelegationID
		}
		if s.Model != nil {
			next.Model = *s.Model
		}
		if s.StartedAt != nil {
			ms := *s.StartedAt * 1000
			next.StartedAt = &ms
		}
		switch {
		case had && previous.Status != "queued":
			next.Status = previous.Status
		case val(s.Status) == "queued":
			next.Status = "queued"
		default:
			next.Status = "running"
		}
		next.ToolCount = max(int(val(s.ToolCount)), previous.ToolCount)
		switch last := val(s.LastTool); {
		case had && len(previous.Tools) > 0:
			next.Tools = slices.Clone(previous.Tools)
		case last != "":
			next.Tools = []string{last}
		default:
			next.Tools = []string{}
		}
		merged[id] = next
	}
	out := make([]subagentProgress, 0, len(order))
	for _, id := range order {
		out = append(out, merged[id])
	}
	return out
}

// Roster of the current session from the turn's subagent events and the polled snapshot
func (s *tuiStore) currentAgentRoster() []subagentProgress {
	var data tuigateway.SubagentListResult
	if s.agentSnapshot.Sid == s.ui.Sid {
		data = s.agentSnapshot.Data
	}
	return mergeAgentRoster(s.turn.Subagents, data)
}

// Failed delegations of the session in view as the last subagent.list reported them, newest first
func (s *tuiStore) failedDelegations() []tuigateway.FailedDelegation {
	if s.agentSnapshot.Sid != s.ui.Sid {
		return nil
	}
	return s.agentSnapshot.Data.Delegations
}

// Status of a failed delegation in the words of the spawn tree, the ledger's synonyms folded into them
func failedDelegationStatus(d tuigateway.FailedDelegation) string {
	switch status := strings.ToLower(d.Status); status {
	case "failure":
		return "failed"
	case "":
		return "error"
	default:
		return status
	}
}

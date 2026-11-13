package tui

import (
	"math"
	"reflect"
	"slices"
)

// Subagent list of the session it belongs to, as subagent.list reports it
type agentSnapshot struct {
	Sid  string
	Data map[string]any
}

func emptyAgentList() map[string]any {
	return map[string]any{"subagents": []any{}, "delegations": []any{}}
}

// Replaces the agent snapshot only when its session or list changed
func (s *tuiStore) applyAgentSnapshot(sid string, data map[string]any) {
	if data == nil {
		data = emptyAgentList()
	}
	if s.agentSnapshot.Sid != sid || !reflect.DeepEqual(s.agentSnapshot.Data, data) {
		s.agentSnapshot = agentSnapshot{Sid: sid, Data: data}
	}
}

// Roster of the live subagent events merged with the polled list, events keeping their own progress
func mergeAgentRoster(events []subagentProgress, data map[string]any) []subagentProgress {
	var order []string
	merged := map[string]subagentProgress{}
	for _, e := range events {
		if _, seen := merged[e.ID]; !seen {
			order = append(order, e.ID)
		}
		merged[e.ID] = e
	}
	list, _ := data["subagents"].([]any)
	for index, raw := range list {
		s, _ := raw.(map[string]any)
		if s == nil {
			s = map[string]any{}
		}
		id, _ := s["subagent_id"].(string)
		previous, had := merged[id]
		var next subagentProgress
		if had {
			next = previous
		} else {
			depth := 0
			if d, isNum := s["depth"].(float64); isNum {
				depth = int(d)
			}
			parent, _ := s["parent_id"].(string)
			next = subagentProgress{Depth: depth, Index: index, ParentID: parent, Notes: []string{}, Thinking: []string{}, TaskCount: 1}
			order = append(order, id)
		}
		next.ID = id
		switch goal, _ := s["goal"].(string); {
		case goal != "":
			next.Goal = goal
		case had && previous.Goal != "":
			next.Goal = previous.Goal
		default:
			next.Goal = "Starting agent"
		}
		if delegation, isStr := s["delegation_id"].(string); isStr {
			next.DelegationID = delegation
		}
		if model, isStr := s["model"].(string); isStr {
			next.Model = model
		}
		if started, isNum := s["started_at"].(float64); isNum {
			ms := started * 1000
			next.StartedAt = &ms
		}
		switch {
		case had && previous.Status != "queued":
			next.Status = previous.Status
		case s["status"] == "queued":
			next.Status = "queued"
		default:
			next.Status = "running"
		}
		toolCount := 0.0
		if n, isNum := s["tool_count"].(float64); isNum {
			toolCount = n
		}
		next.ToolCount = int(math.Max(toolCount, float64(previous.ToolCount)))
		switch last, _ := s["last_tool"].(string); {
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
	data := emptyAgentList()
	if s.agentSnapshot.Sid == s.ui.Sid && s.agentSnapshot.Data != nil {
		data = s.agentSnapshot.Data
	}
	return mergeAgentRoster(s.turn.Subagents, data)
}

package agent

import (
	"encoding/json"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Subagent rows of the list payload for the parent's own spawn tree, each as a plain map
func SubagentListRows(parent *AIAgent) []map[string]any {
	payload := listSubagentsPayload(parent)
	raw, _ := payload.Get("subagents")
	entries, _ := raw.([]any)
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		obj, ok := e.(*pyjson.Object)
		if !ok {
			continue
		}
		row := make(map[string]any, obj.Len())
		for _, k := range obj.Keys {
			row[k], _ = obj.Get(k)
		}
		rows = append(rows, row)
	}
	return rows
}

// Decoded result of a steer or stop control action over the parent's spawn tree
func SubagentControl(action, subagentID string, message any, parent *AIAgent) (map[string]any, error) {
	text, err := handleDelegateControlAction(action, subagentID, message, parent)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Registers a live subagent record under its id the way a delegated child does
func RegisterSubagent(record map[string]any) { registerSubagent(record) }

// Drops every live subagent record
func ClearActiveSubagents() {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	activeSubagents.keys, activeSubagents.m = nil, map[string]map[string]any{}
}

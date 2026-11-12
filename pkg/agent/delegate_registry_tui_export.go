package agent

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Copies of the live child records of one session generation, the exact owner, transport and record authority
func OwnedSubagentRecords(sessionID string, transport, owner any) []map[string]any {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	out := []map[string]any{}
	for _, record := range activeSubagents.values() {
		if pyrepr.Equal(record["owner_session_id"], sessionID) && subagentTransportMatches(record, transport) &&
			pyrepr.Is(record["owner_session_record"], owner) {
			out = append(out, copySubagentRecord(record))
		}
	}
	return out
}

// Copies of the exact owner records plus the children whose durable lineage is the parent agent's conversation
func VisibleSubagentRecords(sessionID string, transport, owner any, parent *AIAgent) []map[string]any {
	activeSubagentsMu.Lock()
	records := make([]map[string]any, 0, len(activeSubagents.keys))
	for _, record := range activeSubagents.values() {
		records = append(records, copySubagentRecord(record))
	}
	activeSubagentsMu.Unlock()
	out := []map[string]any{}
	for _, record := range records {
		exact := pyrepr.Equal(record["owner_session_id"], sessionID) && subagentTransportMatches(record, transport) &&
			pyrepr.Is(record["owner_session_record"], owner)
		if exact || ownsSubagentRecord(record, parent) {
			out = append(out, record)
		}
	}
	return out
}

// Durable owner conversation of every live top level child, in registration order
func TopLevelSubagentOwners() []string {
	activeSubagentsMu.Lock()
	defer activeSubagentsMu.Unlock()
	out := []string{}
	for _, record := range activeSubagents.values() {
		if !pyTruthy(record["depth"]) {
			owner := ""
			if pyTruthy(record["owner_agent_session_id"]) {
				owner = pyrepr.Str(record["owner_agent_session_id"])
			}
			out = append(out, owner)
		}
	}
	return out
}

// Live transcript path of a child agent, empty when it has none
func LiveTranscriptPathOf(child any) string {
	holder, isHolder := child.(liveTranscriptHolder)
	if !isHolder || isNilAgent(child) {
		return ""
	}
	path, _ := holder.liveTranscript().(string)
	return path
}

func copySubagentRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		out[key] = value
	}
	return out
}

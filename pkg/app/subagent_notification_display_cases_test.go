package app

import (
	"github.com/nickheyer/icebath/pkg/agent"
)

// CLI agent whose staging lands on an in process agent the way the hermes CLI shares one with its turn
type inProcessStageAgent struct {
	*GatewayAgent
	real *agent.AIAgent
}

func (d *inProcessStageAgent) StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func()) {
	d.real.StagePendingCLIUserMessage(staged, appendToTranscript)
}

// One async delegation event of the display session whose second goal is the finished task
func delegationDisplayEvent(status string, truncated bool) map[string]any {
	return map[string]any{"type": "async_delegation", "session_key": "display-session", "delegation_id": "deleg-test",
		"goals":   []any{"Do not display this sibling", "Review [bold]changes[/bold]"},
		"results": []any{map[string]any{"task_index": 1, "status": status, "truncated": truncated, "summary": "Full result evidence"}}}
}

// Status, truncation and the label the display line names for each delegation outcome
var delegationDisplayRows = []struct {
	status    string
	truncated bool
	label     string
}{{"completed", false, "Completed"}, {"failed", false, "Failed"}, {"cancelled", false, "Cancelled"}, {"completed", true, "Incomplete"},
	{"stalled", false, "Stalled"}, {"unknown", false, "Unknown"}, {"rejected", false, "Failed"}, {"other_terminal_state", false, "Incomplete"}}

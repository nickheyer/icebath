package app

import (
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

// Gateway session store of the CLI, nil when none is open
func (c *CLI) gatewayStore() *gatewaycli.GatewaySessionStore { return c.sessionDB }

// Remembers a queued admission's start dropped outside its turn so the idle tick paints it from its receipt
func missedStart(state *gatewaycli.SessionState, params map[string]any) {
	if params["type"] != "message.start" {
		return
	}
	admission, isStr := params["admission_id"].(string)
	if !isStr {
		return
	}
	state.Lock()
	defer state.Unlock()
	if _, queued := state.QueuedInputs.Get(admission); queued {
		state.Missed[admission] = true
	}
}

// Admitted turn a run replays instead of submitting, nil for any other turn
func takeAdmittedTurn(a *GatewayAgent, text any) map[string]any {
	state := a.state()
	state.Lock()
	defer state.Unlock()
	armed := state.ArmedAdmission
	state.ArmedAdmission = ""
	if armed != "" {
		if adopted, ok := state.Adopted[armed]; ok {
			delete(state.Adopted, armed)
			state.QueuedInputs.Delete(armed)
			row, _ := adopted.(map[string]any)
			return row
		}
	}
	adopted := state.AdmittedTurn
	state.AdmittedTurn = nil
	if adopted == nil || !anyutil.Equal(adopted["text"], text) {
		return nil
	}
	return adopted
}

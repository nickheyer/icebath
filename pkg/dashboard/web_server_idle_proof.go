package dashboard

import (
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Prompts waiting on a human, the open requests plus the queued gateway approvals
func PendingHumanInput() (int, bool) {
	return tuigateway.OpenRequestCount() + tools.PendingGatewayApprovalCount(), true
}

// Answer of a turn probe, a flag or the busy ledger name, not ok when it cannot be read
type TurnProbe func() (busy bool, detail string, ok bool)

// Busy ledger as a turn probe, its name the detail
func busyLedgerProbe() (bool, string, bool) {
	ledger, ok := BusyLedger()
	return ledger != "", ledger, ok
}

// Idle verdict and reason, true only with no turn and no waiting human, None when a probe fails
func IdleProof(turnProbe TurnProbe, inputProbe func() (int, bool)) map[string]any {
	if turnProbe == nil {
		turnProbe = busyLedgerProbe
	}
	if inputProbe == nil {
		inputProbe = PendingHumanInput
	}
	busy, detail, ok := turnProbe()
	if !ok {
		return map[string]any{"idle": nil, "reason": "turn_probe_unavailable"}
	}
	if busy {
		var d any
		if detail != "" {
			d = detail
		}
		return map[string]any{"idle": false, "reason": "turn_in_flight", "detail": d}
	}
	pending, ok := inputProbe()
	if !ok {
		return map[string]any{"idle": nil, "reason": "input_probe_unavailable"}
	}
	if pending > 0 {
		return map[string]any{"idle": false, "reason": "awaiting_human_input"}
	}
	return map[string]any{"idle": true, "reason": nil}
}

// Idle proof of the live ledgers, the retirement fence hook
func RetirementIdleProof() (map[string]any, error) {
	return IdleProof(nil, nil), nil
}

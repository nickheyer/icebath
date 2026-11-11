package app

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Prepared owner mutations for the ACP adapter, a derived payload given as a plain function of the snapshot
type ACPMutations struct{ inner *PreparedMutations }

// Empty retained mutation table for one ACP agent
func NewACPMutations() *ACPMutations { return &ACPMutations{inner: NewPreparedMutations()} }

func acpMutationPayload(payload any) any {
	if derive, ok := payload.(func(map[string]any) (map[string]any, error)); ok {
		return mutationDerive(derive)
	}
	return payload
}

// Owner answer for the prepared mutation, re-sent with the confirmation token once a guarded target is confirmed
func (m *ACPMutations) Apply(ctx context.Context, rpc func(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error),
	sessionID, operation string, payload any, confirm func(ctx context.Context, refusal map[string]any) (bool, error)) (map[string]any, error) {
	return m.inner.Apply(ctx, rpc, sessionID, operation, acpMutationPayload(payload), confirm)
}

// Drops the retained identity of this exact mutation
func (m *ACPMutations) Acknowledge(sessionID, operation string, payload any) {
	m.inner.Acknowledge(sessionID, operation, acpMutationPayload(payload))
}

// Retires every retained identity of the session
func (m *ACPMutations) Retire(sessionID string) { m.inner.Retire(sessionID, nil) }

// Operation and payload of a /branch, /model or /compress line
func ACPSlashMutation(command, arg string) (string, *pyjson.Object, error) {
	return slashMutation(command, arg)
}

// Why a guarded model target was not applied, the owner guard text first
func ACPModelRefusalText(refusal map[string]any, confirmed bool) string {
	return modelRefusalText(refusal, confirmed)
}

// One prompt title for the owner guard warnings
func ACPConfirmationTitle(refusal map[string]any) string { return confirmationTitle(refusal) }

// Refusal naming the parity table for a command the shared gateway does not serve yet
func ACPCommandRefusal(name string) string { return gatewayRefusal(name) }

// Rewind payload naming the Nth newest user turn of the snapshot, a retry refusing a turn it cannot resend
func ACPRewindTo(turnsBack int64, retry bool) func(snapshot map[string]any) (map[string]any, error) {
	return gatewayRewindTo(turnsBack, retry)
}

// Rest of the final reply after the painted exact prefix, else the resumed marker and the whole reply
func ACPReplayGapOwedReply(shown, final string) string { return replayGapOwedReply(shown, final) }

// Status of the admission in the snapshot pending rows, nil when it ended in the hole
func ACPReplayGapPendingStatus(pending []any, admission any) any {
	return replayGapPendingStatus(pending, admission)
}

// True when terminal with its reply, false while running, nil when the receipt is unreadable
func ACPReplayGapSettled(receipt map[string]any) *bool { return replayGapSettled(receipt) }

// Number of retained identities
func (m *ACPMutations) Len() int { return m.inner.Len() }

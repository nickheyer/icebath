package acp

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One owner request answered as a plain map within a budget, zero the default
type MutationRPC = func(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error)

// Asks the editor to confirm a guarded mutation the owner refused
type MutationConfirm = func(ctx context.Context, refusal map[string]any) (bool, error)

// Payload of a mutation derived from the snapshot its fence is read from, nil for nothing to do
type MutationDerive = func(snapshot map[string]any) (map[string]any, error)

// Prepared owner mutations retaining their identity across ambiguous replies
type PreparedMutations interface {
	Apply(ctx context.Context, rpc MutationRPC, sessionID, operation string, payload any, confirm MutationConfirm) (map[string]any, error)
	Acknowledge(sessionID, operation string, payload any)
	Retire(sessionID string)
}

// Wiring of the classic gateway client modules that live above this package, assigned by cmd/icebath
var (
	NewPreparedMutations   func() PreparedMutations
	SlashMutation          func(command, arg string) (string, *pyjson.Object, error)
	ModelRefusalText       func(refusal map[string]any, confirmed bool) string
	ConfirmationTitle      func(refusal map[string]any) string
	CommandRefusal         func(command string) string
	RewindTo               func(turnsBack int64, retry bool) MutationDerive
	ReplayGapOwedReply     func(shown, final string) string
	ReplayGapPendingStatus func(pending []any, admission any) any
	ReplayGapSettled       func(receipt map[string]any) *bool
	EnvLaunchFlags         func() *pyjson.Object
	ResolveCommand         func(name string) (string, bool)
	RunModelSetup          func(ctx context.Context) int
)

package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Reserves a staged local session's row, route and immutable policy together before publication
func commitLocalSession(ctx context.Context, db *state.SessionDB, epoch int64, receipt *pyjson.Object) error {
	_, err := db.CommitLocalSession(ctx, epoch, state.LocalCreateReceipt{Receipt: receipt})
	return err
}

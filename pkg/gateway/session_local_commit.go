package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Reserves a staged local session's row, route and immutable policy together before publication
func commitLocalSession(ctx context.Context, db *state.SessionDB, epoch int64, receipt *jsonutil.Object) error {
	_, err := db.CommitLocalSession(ctx, epoch, state.LocalCreateReceipt{Receipt: receipt})
	return err
}

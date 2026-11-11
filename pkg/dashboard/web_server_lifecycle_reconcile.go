package dashboard

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/state"
)

// Brings this process's own state.db schema current through the read only opener, a failure only logged
func eagerReconcileOwnSessionDB(ctx context.Context) {
	db, err := openSessionDBForProfile(ctx, nil, true)
	if err != nil {
		webServerLog().Warn(fmt.Sprintf("startup schema reconcile of state.db failed (%s); session "+
			"reads will report the store unavailable until owner recovery", err))
		return
	}
	state.ReleaseOrClose(db)
}

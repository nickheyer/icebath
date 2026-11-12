package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type ownerLoopKey struct{}

// Context of a caller running on the owner loop, where a contended writer would stall every profile it serves
func withOwnerLoop(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownerLoopKey{}, true)
}

// True for a caller running on the owner loop
func onOwnerLoop(ctx context.Context) bool {
	on, _ := ctx.Value(ownerLoopKey{}).(bool)
	return on
}

// Stored receipt of a session when a catch up would commit for it on this owner, nil otherwise
func adoptionDue(ctx context.Context, a *SessionAuthority, sid string) (*pyjson.Object, error) {
	receipt, _, _, _, err := ownedReceipt(ctx, a, sid)
	due := false
	if err == nil {
		err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			children, routed, err := state.AdoptableCompression(ctx, conn, receipt)
			due = children != nil || routed != nil
			return err
		})
	}
	if err != nil {
		if receiptCheckFailure(err) {
			return nil, nil
		}
		return nil, err
	}
	if !due {
		return nil, nil
	}
	return receipt, nil
}

// Commits the compression catch up of a receipt at this owner's epoch
func adoptReceipt(ctx context.Context, a *SessionAuthority, sid string, receipt *pyjson.Object) error {
	return a.DB.AdoptUntrackedCompression(ctx, a.Epoch, sid, receipt)
}

// Commits a session's due catch up in a worker so the restore that follows only reads
func adoptBeforeRestore(ctx context.Context, a *SessionAuthority, sid string, tracked bool) error {
	receipt, err := adoptionDue(ctx, a, sid)
	if err != nil || receipt == nil {
		return err
	}
	write := func(writeCtx context.Context) (any, error) { return nil, adoptReceipt(writeCtx, a, sid, receipt) }
	if tracked {
		_, err = trackedWrite(ctx, a, write, nil, false, nil)
	} else {
		_, err = write(ctx)
	}
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		slog.Warn(fmt.Sprintf("Local session %s compression catch-up failed: %s", sid, refusal.Reason))
		return nil
	}
	return err
}

// Commits every boot receipt's due catch up before local recovery restores them
func adoptLocalCompressions(ctx context.Context, a *SessionAuthority) error {
	ids, err := localReceiptIDs(ctx, a)
	if err != nil {
		return err
	}
	for _, sid := range ids {
		if err := adoptBeforeRestore(ctx, a, sid, false); err != nil {
			return err
		}
	}
	return nil
}

// Commits a due catch up in one tracked worker per session, then publishes the session on the owner loop
func scheduleAdoption(ctx context.Context, a *SessionAuthority, sid string, receipt *pyjson.Object) {
	a.localAdoptionsMu.Lock()
	defer a.localAdoptionsMu.Unlock()
	if pending := a.localAdoptions[sid]; pending != nil {
		select {
		case <-pending.done:
		default:
			return
		}
	}
	slog.Info(fmt.Sprintf("Local session %s: committing its compression catch-up off the owner loop", sid))
	if a.localAdoptions == nil {
		a.localAdoptions = map[string]*trackedTask{}
	}
	a.localAdoptions[sid] = trackMutation(ctx, a, func(taskCtx context.Context) (any, error) {
		err := adoptReceipt(taskCtx, a, sid, receipt)
		if err == nil && !a.Retiring {
			_, err = restoreLocalSession(withOwnerLoop(taskCtx), a, sid)
		}
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			slog.Warn(fmt.Sprintf("Local session %s paused after its compression catch-up: %s", sid, refusal.Reason))
			return nil, nil
		}
		return nil, err
	})
}

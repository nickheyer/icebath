package state

import "context"

// Ids of the given sessions under a live turn lease or compression lock, read on a reader connection
func (d *SessionDB) GuardedIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	var out map[string]bool
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		out, err = d.guardedIDs(ctx, conn, ids)
		return err
	})
	return out, err
}

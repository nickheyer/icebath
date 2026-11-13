package state

import "context"

// Repairs a malformed sqlite_master in place after taking a backup, the offline repair layer performing the surgery
func repairMalformedSchema(ctx context.Context, e *Engine, path string) (bool, error) {
	return e.repairStateDBSchema(ctx, path)
}

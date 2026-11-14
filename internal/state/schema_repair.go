package state

import "context"

// Repairs a malformed sqlite_master in place after taking a backup, the offline repair layer performing the surgery
func repairMalformedSchema(ctx context.Context, s *Store) (bool, error) {
	return s.eng.repairStateDBSchema(ctx, s.path, s.settings, s.tables.state.options.StartupProgress)
}

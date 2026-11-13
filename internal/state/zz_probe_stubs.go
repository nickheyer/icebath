package state

import (
	"context"
	"time"
)

func (e *Engine) repairStateDBSchema(ctx context.Context, path string) (bool, error) {
	return false, nil
}

func (s *Store) reapInactiveOrphanDesktopHolders(ctx context.Context, holders []Holder, minAge time.Duration) ([]int, error) {
	return nil, nil
}

func (s *Store) flushQueuedTokenDeltas(ctx context.Context, forRead bool) {}

func (s *Store) guardedIDs(ctx context.Context, c Conn, ids []string) (map[string]bool, error) {
	return nil, nil
}

func retireSessions(ctx context.Context, c Conn, ids, expiring []string) error { return nil }

func retirePrunable(ctx context.Context, c Conn, ids []string) ([]string, error) { return ids, nil }

func (s *Store) collectRetiredMedia(ctx context.Context) error { return nil }

type ArchiveSelection struct{ IDs []string }

func (s *Store) archiveCandidates(ctx context.Context, sel ArchiveSelection) ([]string, error) {
	return sel.IDs, nil
}

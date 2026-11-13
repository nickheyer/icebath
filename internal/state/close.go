package state

import (
	"context"
	"fmt"
	"log/slog"
)

// Closes the store, releasing one reference instead when the shared registry lent it
func (s *Store) Close(ctx context.Context) error {
	if s.shared.Load() {
		s.eng.releaseShared(ctx, s)
		return nil
	}
	return s.closeHandle(ctx)
}

// Physical close, a passive checkpoint on a healthy writer, a lost generation captured before its connection goes
func (s *Store) closeHandle(ctx context.Context) error {
	if s.tables.drain != nil {
		s.tables.drain(ctx, s)
	}
	s.readMu.Lock()
	s.readClosed = true
	s.readMu.Unlock()
	for s.evictIdleRead() {
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.conn != nil {
		if err := s.settleConnection(ctx); err != nil {
			return err
		}
	}
	s.budget.unregister(s)
	if !s.budgetDropped {
		s.budgetDropped = true
		s.eng.dropBudget(s.budget)
	}
	return nil
}

// Settles the writer at close, caller holding the writer lock, a failed capture leaving the handle open for a retry
func (s *Store) settleConnection(ctx context.Context) error {
	s.flagMu.Lock()
	lost := s.walLost
	s.flagMu.Unlock()
	lost = !s.readOnly && (lost || (len(s.sidecars) > 0 && s.walGenerationLost()))
	if lost {
		if err := s.settleLostGeneration(ctx); err != nil {
			return err
		}
	} else if !s.readOnly {
		s.latchReplaced()
	}
	reason := ""
	if !lost {
		reason = s.quarantineReason()
	}
	switch {
	case reason != "":
		slog.Warn(fmt.Sprintf("Skipping the close-time WAL checkpoint for %s: this handle observed %s. Take a snapshot "+
			"of state.db, -wal and -shm before restarting, then run `icebath sessions recover --source %s --inspect-only`.",
			s.path, reason, s.path))
	case !s.readOnly && !lost:
		if _, err := s.conn.q().ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			slog.Debug(fmt.Sprintf("WAL checkpoint (PASSIVE) at close failed: %s", errText(err)))
		}
	}
	s.dropWriter()
	return nil
}

// Captures the lost generation before close, the close-time checkpoint switched off first so no close can merge it
func (s *Store) settleLostGeneration(ctx context.Context) error {
	s.flagMu.Lock()
	s.walLost = true
	s.flagMu.Unlock()
	s.fenceWriter()
	artifact, err := s.captureGeneration(ctx, "close")
	if err != nil {
		slog.Error(fmt.Sprintf("Could not capture the retired WAL generation of %s at close: %s. The handle stays open "+
			"and close retries the capture; those frames are NOT yet preserved.", s.path, errText(err)))
		return err
	}
	slog.Warn(fmt.Sprintf("Skipping the close-time WAL checkpoint for %s: this handle's WAL/SHM generation "+
		"was deleted or replaced; the retired generation is captured at %s. Stop the other "+
		"writers before reopening and inspect the capture before deciding its disposition.", s.path, artifact))
	return nil
}

// Latches a replacement first noticed at close so the connection never checkpoints into the file now at the path
func (s *Store) latchReplaced() {
	s.flagMu.Lock()
	replaced := s.replaced
	s.flagMu.Unlock()
	if replaced || !s.fileWasReplaced() {
		return
	}
	s.flagMu.Lock()
	s.replaced = true
	s.flagMu.Unlock()
	slog.Error(replacedMsg)
}

// Switches off SQLite's close-time checkpoint on a quarantined writer, false when it stays open because that failed
func (s *Store) fenceWriter() bool {
	if s.pinned {
		return false
	}
	if s.conn == nil {
		return true
	}
	err := s.conn.disableCloseCheckpoint()
	if err == nil {
		return true
	}
	s.eng.retain(s.conn)
	s.pinned = true
	slog.Error(fmt.Sprintf("Could not switch off SQLite's close-time checkpoint on the quarantined connection for %s (%s); "+
		"keeping it open until the process exits so no close can checkpoint it into the wrong file.", s.path, errText(err)))
	return false
}

// Lets go of the writer, closing a quarantined one only once its close-time checkpoint is off
func (s *Store) dropWriter() {
	s.eng.guardRelease(s.guard)
	if s.readOnly || s.quarantineReason() == "" || s.fenceWriter() {
		if err := s.conn.close(); err != nil {
			slog.Debug(fmt.Sprintf("Could not close a state store connection: %s", errText(err)))
		}
		s.sidecars = map[string]fileIdentity{}
	}
	s.conn = nil
	s.writerUp.Store(false)
}

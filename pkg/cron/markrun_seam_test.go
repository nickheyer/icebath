package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Advances one recurring job before it runs so a crash cannot re-fire it
func (s *Store) advanceNextRun(ctx context.Context, jobID string) (bool, error) {
	n, err := s.advanceNextRuns(home.WithOverride(ctx, s.home), []string{jobID})
	return n >= 1, err
}

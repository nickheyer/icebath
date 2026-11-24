package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

const defaultScriptTimeout = 3600

// Script timeout a caller patched over the default, read before env and config
var scriptTimeoutOverride any = defaultScriptTimeout

// Inactivity limit of a run in seconds from cron.inactivity_timeout, zero or below unlimited
func (s *Store) cronInactivitySeconds(ctx context.Context) float64 {
	return settings.CronInactivityTimeout.Get(home.WithOverride(ctx, s.home)).Seconds()
}

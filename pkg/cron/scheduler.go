package cron

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const defaultScriptTimeout = 3600

// Script timeout a caller patched over the default, read before env and config
var scriptTimeoutOverride any = defaultScriptTimeout

// Inactivity limit from ICEBATH_CRON_TIMEOUT in seconds, 0 unlimited and 600 for missing or bad input
func (s *Store) cronInactivitySeconds(ctx context.Context) float64 {
	raw := pystr.Strip(s.cronEnvSetting(ctx, "ICEBATH_CRON_TIMEOUT", ""))
	if raw == "" {
		return 600.0
	}
	v, err := pyint.Float(raw)
	if err != nil {
		s.log.Warn(fmt.Sprintf("Invalid ICEBATH_CRON_TIMEOUT=%s; using default 600s", pyrepr.StrRepr(raw)))
		return 600.0
	}
	return v
}

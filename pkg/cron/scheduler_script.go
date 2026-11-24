package cron

import (
	"context"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

const defaultMediaSendTimeout = 300

// Integer part of a positive number, false for zero or below, an error for unparsable input
func positiveInt(raw any) (int, bool, error) {
	v, err := anyutil.ToFloat(raw)
	if err != nil {
		return 0, false, err
	}
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false, fmt.Errorf("cannot convert float %v to integer", v)
	}
	timeout := int(math.Trunc(v))
	return timeout, timeout > 0, nil
}

// Pre run script timeout from a patched override, cron.script_timeout_seconds, then the one hour default
func (s *Store) getScriptTimeout(ctx context.Context) int {
	if scriptTimeoutOverride != defaultScriptTimeout {
		if v, ok, err := positiveInt(scriptTimeoutOverride); err != nil {
			s.log.Warn(fmt.Sprintf("Invalid patched _SCRIPT_TIMEOUT=%v; using config/default", scriptTimeoutOverride))
		} else if ok {
			return v
		}
	}
	if v := int(math.Trunc(settings.CronScriptTimeoutSeconds.Get(home.WithOverride(ctx, s.home)).Seconds())); v > 0 {
		return v
	}
	return defaultScriptTimeout
}

// Per attachment media send timeout from cron.media_send_timeout_seconds, five minutes when not positive
func (s *Store) getMediaSendTimeout(ctx context.Context) int {
	if v := int(math.Trunc(settings.CronMediaSendTimeoutSeconds.Get(home.WithOverride(ctx, s.home)).Seconds())); v > 0 {
		return v
	}
	return defaultMediaSendTimeout
}

package cron

import (
	"context"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const defaultMediaSendTimeout = 300

// Integer part of a positive number, false for zero or below, an error for unparsable input
func positiveInt(raw any) (int, bool, error) {
	v, err := pyint.Float(raw)
	if err != nil {
		return 0, false, err
	}
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false, fmt.Errorf("cannot convert float %v to integer", v)
	}
	timeout := int(math.Trunc(v))
	return timeout, timeout > 0, nil
}

// Positive timeout from the env var then cron.<configKey>, false when neither yields one
func (s *Store) timeoutFromEnvOrConfig(ctx context.Context, envVar, configKey, label string) (int, bool) {
	return timeoutSetting(s, ctx, envVar, configKey, label, positiveInt)
}

// Value parse yields from the env var then cron.<configKey>, false when neither yields one
func timeoutSetting[T any](s *Store, ctx context.Context, envVar, configKey, label string, parse func(any) (T, bool, error)) (T, bool) {
	var zero T
	if envValue := pystr.Strip(s.cronEnvSetting(ctx, envVar, "")); envValue != "" {
		v, ok, err := parse(envValue)
		if err != nil {
			s.log.Warn(fmt.Sprintf("Invalid %s=%s; using config/default", envVar, pyrepr.StrRepr(envValue)))
		} else if ok {
			return v, true
		}
	}
	cfg, err := s.loadConfig()
	if err != nil {
		s.log.Debug(fmt.Sprintf("Failed to load %s from config: %v", label, err))
		return zero, false
	}
	cron, isMap := cfg["cron"].(map[string]any)
	if raw, present := cfg["cron"]; present && !isMap {
		s.log.Debug(fmt.Sprintf("Failed to load %s from config: %T object has no attribute get", label, raw))
		return zero, false
	}
	configured, present := cron[configKey]
	if !present || configured == nil {
		return zero, false
	}
	v, ok, err := parse(configured)
	if err != nil {
		s.log.Debug(fmt.Sprintf("Failed to load %s from config: %v", label, err))
		return zero, false
	}
	return v, ok
}

// Pre run script timeout from a patched override, env, config, then the one hour default
func (s *Store) getScriptTimeout(ctx context.Context) int {
	if scriptTimeoutOverride != defaultScriptTimeout {
		if v, ok, err := positiveInt(scriptTimeoutOverride); err != nil {
			s.log.Warn(fmt.Sprintf("Invalid patched _SCRIPT_TIMEOUT=%v; using env/config/default", scriptTimeoutOverride))
		} else if ok {
			return v
		}
	}
	if v, ok := s.timeoutFromEnvOrConfig(ctx, "ICEBATH_CRON_SCRIPT_TIMEOUT", "script_timeout_seconds", "cron script timeout"); ok {
		return v
	}
	return defaultScriptTimeout
}

// Per attachment media send timeout from env, config, then five minutes
func (s *Store) getMediaSendTimeout(ctx context.Context) int {
	if v, ok := s.timeoutFromEnvOrConfig(ctx, "ICEBATH_CRON_MEDIA_SEND_TIMEOUT", "media_send_timeout_seconds", "cron media-send timeout"); ok {
		return v
	}
	return defaultMediaSendTimeout
}

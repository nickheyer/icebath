package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	_ "time/tzdata"
)

const EnvTimeZone = "ICEBATH_TIMEZONE"

func (s *Store) timeZoneName(ctx context.Context) string {
	if !s.envs.scope.Multiplexed(ctx) {
		if v := strings.TrimSpace(os.Getenv(EnvTimeZone)); v != "" {
			return v
		}
	}
	doc, err := s.UserEffective(ctx, false)
	if err != nil || doc == nil {
		return ""
	}
	if v, ok := doc.Get("timezone"); ok && isStringNode(v) {
		return strings.TrimSpace(v.Value)
	}
	return ""
}

func (s *Store) TimeZone(ctx context.Context) (string, *time.Location) {
	name := s.timeZoneName(ctx)
	if name == "" {
		return "", nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		slog.Warn(fmt.Sprintf("Invalid timezone '%s': %v. Falling back to server local time.", name, err))
		return name, nil
	}
	return name, loc
}

func (s *Store) Now(ctx context.Context) time.Time {
	if _, loc := s.TimeZone(ctx); loc != nil {
		return time.Now().In(loc)
	}
	return time.Now()
}

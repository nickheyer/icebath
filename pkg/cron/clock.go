package cron

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Clock supplies the current instant and the configured zone to every cron component
type clock struct {
	nowFn  func() time.Time
	zoneFn func() *time.Location
}

// Current instant rendered in the configured zone
func (c *clock) now() time.Time {
	t := time.Now()
	if c != nil && c.nowFn != nil {
		t = c.nowFn()
	}
	return t.In(c.zone())
}

// Configured zone, the system local offset of the current moment when none is configured
func (c *clock) zone() *time.Location {
	if z := c.configuredZone(); z != nil {
		return z
	}
	t := time.Now()
	if c != nil && c.nowFn != nil {
		t = c.nowFn()
	}
	return systemFixedZone(t)
}

// Zone of the timezone setting, nil for server local time
func (c *clock) configuredZone() *time.Location {
	if c != nil && c.zoneFn != nil {
		return c.zoneFn()
	}
	return nil
}

// Current instant as the store's RFC 3339 stamp text
func (c *clock) nowISO() string { return FormatISO(c.now()) }

// Zone the timezone setting names for the profile of ctx, nil for server local time or a name that does not load
func configuredZoneOf(ctx context.Context) *time.Location {
	name := strings.TrimSpace(settings.Timezone.Get(ctx))
	if name == "" {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}

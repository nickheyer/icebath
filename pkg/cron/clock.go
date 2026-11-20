package cron

import (
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Environment variable naming the IANA zone every stamp is rendered in
const EnvTimezone = "ICEBATH_TIMEZONE"

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

// Zone named by the environment or config, nil for server local time
func (c *clock) configuredZone() *time.Location {
	if c != nil && c.zoneFn != nil {
		return c.zoneFn()
	}
	return nil
}

// Current instant as Python isoformat text
func (c *clock) nowISO() string { return FormatISO(c.now()) }

// ResolveZone reads the environment then config.timezone, returning nil for server local time
func ResolveZone(cfg config.Tree, env func(string) string) (*time.Location, string) {
	if env == nil {
		env = os.Getenv
	}
	name := strings.TrimSpace(env(EnvTimezone))
	if name == "" && cfg != nil {
		name = strings.TrimSpace(cfg.String("timezone", ""))
	}
	if name == "" {
		return nil, ""
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, name
	}
	return loc, name
}

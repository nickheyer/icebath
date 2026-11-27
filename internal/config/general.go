package config

import (
	"fmt"
	"strings"
	"time"
)

// Settings of icebath itself that every part of it reads, kept for each profile
const (
	GeneralKey = "general"
	// IANA name of the zone the profile's dates, times and schedules are in, empty for this computer's own
	Timezone = GeneralKey + ".timezone"
)

func general() Sections {
	return Sections{{
		Key:   GeneralKey,
		Title: "General",
		About: "Time zone",
		Group: GroupApp,
		Order: 20,
		Settings: []Setting{
			{Key: Timezone, Title: "Time zone", About: "The time zone for dates, times and schedules.", Help: "A name such as Europe/Berlin.",
				Default: "", Zero: "This computer's", Env: []string{"ICEBATH_TIMEZONE"}},
		},
	}}
}

// Zone the profile's dates, times and schedules are in, nil for this computer's own; an error, with nil, when Timezone names a zone the
// time zone database does not have
func (c *Config) Zone() (*time.Location, error) {
	name := strings.TrimSpace(c.GetString(Timezone))
	if name == "" {
		return nil, nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a time zone icebath knows. Set Time zone in /settings under General to a name such as Europe/Berlin.", name)
	}
	return zone, nil
}

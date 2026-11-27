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
		About: "Settings every part of icebath reads",
		Settings: []Setting{
			{Key: Timezone, Title: "Timezone", About: "Time zone for dates, times and schedules, such as Europe/Berlin; empty uses this computer's", Default: "",
				Env: []string{"ICEBATH_TIMEZONE"}},
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
		return nil, fmt.Errorf("Invalid timezone %q: %v.", name, err)
	}
	return zone, nil
}

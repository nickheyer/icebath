package config

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Settings of icebath itself that every part of it reads, kept for each profile
const (
	GeneralKey = "general"
	// IANA name of the zone the profile's dates, times and schedules are in, empty for this computer's own
	Timezone = GeneralKey + ".timezone"
)

// Registers the settings of icebath itself, a time zone checked as it is saved
func (c *Config) general() {
	c.Register(Sections{{
		Key:   GeneralKey,
		Title: "General",
		About: "Colors, keys, time zone and terminal windows",
		Group: GroupApp,
		Order: 20,
		Settings: []Setting{
			{Key: Timezone, Title: "Time zone", About: "The time zone for dates, times and schedules.",
				Help: "Picked from the zones this computer knows, such as Europe/Berlin.", Default: "", Zero: "This computer's",
				Env: []string{"ICEBATH_TIMEZONE"}, Names: zones},
		},
	}})
	c.Guard(Timezone, Guard{Check: func(_ *Config, _ string, value any) error {
		name, _ := value.(string)
		if name = strings.TrimSpace(name); name == "" {
			return nil
		}
		if _, err := time.LoadLocation(name); err != nil {
			return fmt.Errorf("%s is not a time zone this computer knows. Pick one from the list, such as Europe/Berlin.", name)
		}
		return nil
	}})
}

// Zone the profile's dates, times and schedules are in, nil for this computer's own; an error, with nil, when Timezone names a zone the
// time zone database does not have, such as one ICEBATH_TIMEZONE or a hand edit of the settings file gave
func (c *Config) Zone() (*time.Location, error) {
	name := strings.TrimSpace(c.GetString(Timezone))
	if name == "" {
		return nil, nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q, the Time zone of General, is not a time zone this computer knows.", name)
	}
	return zone, nil
}

// Folders the zones of this computer may be kept in, the one ZONEINFO names first
func zoneFolders() []string {
	folders := []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo"}
	if named := os.Getenv("ZONEINFO"); named != "" {
		folders = append([]string{named}, folders...)
	}
	return folders
}

// Zones this computer knows, by name, each with the offset from UTC it has now: those its time zone database lists, else every zone file
// in its folder; UTC always
func zones() []Option {
	var names []string
	for _, folder := range zoneFolders() {
		if names = listed(folder); len(names) > 0 {
			break
		}
		if names = files(folder); len(names) > 0 {
			break
		}
	}
	if !slices.Contains(names, "UTC") {
		names = append(names, "UTC")
	}
	slices.Sort(names)
	now := time.Now()
	options := make([]Option, 0, len(names))
	for _, name := range names {
		zone, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		options = append(options, Option{Value: name, Title: strings.ReplaceAll(name, "_", " "), About: offset(now.In(zone))})
	}
	return options
}

// Zones the table of a time zone database names, one a line after the countries it covers and its place
func listed(folder string) []string {
	file, err := os.Open(filepath.Join(folder, "zone1970.tab"))
	if err != nil {
		if file, err = os.Open(filepath.Join(folder, "zone.tab")); err != nil {
			return nil
		}
	}
	defer file.Close()
	var names []string
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		if fields := strings.Fields(lines.Text()); len(fields) >= 3 && !strings.HasPrefix(fields[0], "#") && !slices.Contains(names, fields[2]) {
			names = append(names, fields[2])
		}
	}
	return names
}

// Zone files of a folder of zones, by their path under it: the files named with a capital first letter that start as a zone file does,
// outside the folders of other formats
func files(folder string) []string {
	var names []string
	filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case entry.IsDir() && (entry.Name() == "posix" || entry.Name() == "right"):
			return filepath.SkipDir
		case entry.IsDir() || entry.Name()[0] < 'A' || entry.Name()[0] > 'Z':
			return nil
		}
		head := make([]byte, 4)
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		if n, _ := file.Read(head); n == 4 && string(head) == "TZif" {
			rel, _ := filepath.Rel(folder, path)
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	return names
}

// Offset of a time from UTC, such as UTC+01:00
func offset(at time.Time) string {
	_, seconds := at.Zone()
	sign := "+"
	if seconds < 0 {
		sign, seconds = "−", -seconds
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, seconds/3600, seconds%3600/60)
}

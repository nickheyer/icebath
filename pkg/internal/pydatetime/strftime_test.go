package pydatetime

import (
	"testing"
	"time"
)

func TestStrftimeMatchesCPythonGlibc(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("zoneinfo unavailable")
	}
	d := time.Date(2026, 3, 8, 7, 5, 9, 123456000, ny)
	cases := map[string]string{
		"%Y-%m-%d %H:%M %Z":                    "2026-03-08 07:05 EDT",
		"%a %A %b %B %c":                       "Sun Sunday Mar March Sun Mar  8 07:05:09 2026",
		"%C %d %D %e %f %F %G %g":              "20 08 03/08/26  8 123456 2026-03-08 2026 26",
		"%I %j %k %l %p %P %r %R":              "07 067  7  7 AM am 07:05:09 AM 07:05",
		"%T %u %U %V %w %W %x %X %y %z %:z %%": "07:05:09 7 10 10 0 09 03/08/26 07:05:09 26 -0400 -04:00 %",
		"%-d %_m %^a %#b %#p %10Y %05d %Q":     "8  3 SUN MAR am 0000002026 00008 %Q",
		"%":                                    "%",
	}
	for format, want := range cases {
		if got := Strftime(d, format); got != want {
			t.Errorf("Strftime(%q) = %q, want %q", format, got, want)
		}
	}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", 5*3600+30*60))
	if got := Strftime(fixed, "%Z %z %U %W %j %I %p"); got != "UTC+05:30 +0530 00 00 001 12 AM" {
		t.Errorf("fixed zone = %q", got)
	}
	utc := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	if got := Strftime(utc, "%Z %z %U %W %l"); got != "UTC +0000 00 00  1" {
		t.Errorf("utc = %q", got)
	}
}

package pydatetime

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestStrptimeNumericDirectivesMatchCPython(t *testing.T) {
	got, aware, err := Strptime("2026-04-28 13:40:53", "%Y-%m-%d %H:%M:%S")
	if err != nil || aware || !got.Equal(time.Date(2026, 4, 28, 13, 40, 53, 0, time.UTC)) {
		t.Fatalf("naive %v %v %v", got, aware, err)
	}
	got, _, err = Strptime("٢٠٢٦-04-28　 13:40:53", "%Y-%m-%d %H:%M:%S")
	if err != nil || got.Year() != 2026 {
		t.Fatalf("unicode year and whitespace run %v %v", got, err)
	}
	got, aware, err = Strptime("2026-04-28t13:40:53+0200", "%Y-%m-%dT%H:%M:%S%z")
	if _, off := got.Zone(); err != nil || !aware || off != 7200 || got.Unix() != time.Date(2026, 4, 28, 11, 40, 53, 0, time.UTC).Unix() {
		t.Fatalf("aware %v %v %v", got, aware, err)
	}
	got, _, err = Strptime("2026-04-28T13:40:53-02:00:30.5", "%Y-%m-%dT%H:%M:%S%z")
	if err != nil || got.UnixNano() != time.Date(2026, 4, 28, 15, 41, 23, 500_000_000, time.UTC).UnixNano() {
		t.Fatalf("fractional offset %v %v", got, err)
	}
	got, _, err = Strptime("2026-04-28T13:40:53.25Z", "%Y-%m-%dT%H:%M:%S.%f%z")
	if err != nil || got.Nanosecond() != 250_000_000 {
		t.Fatalf("fraction %v %v", got, err)
	}
	for _, c := range [][3]string{
		{"2026-٠٤-28 13:40:53", "%Y-%m-%d %H:%M:%S", "does not match format"},
		{"2026-04-28T13:40:53z", "%Y-%m-%dT%H:%M:%S%z", "does not match format"},
		{"2026-04-28T13:40:53+02:0030", "%Y-%m-%dT%H:%M:%S%z", "Inconsistent use of :"},
		{"2026-04-28T13:40:53+2400", "%Y-%m-%dT%H:%M:%S%z", "strictly between"},
		{"2026-02-30 13:40:53", "%Y-%m-%d %H:%M:%S", "must be in range"},
		{"0000-01-01 00:00:00", "%Y-%m-%d %H:%M:%S", "out of range"},
		{"2026-04-28 13:40:60", "%Y-%m-%d %H:%M:%S", "second must be"},
		{"2026-04-28 13:40:53 extra", "%Y-%m-%d %H:%M:%S", "unconverted data remains:  extra"},
		{"2026", "%Y %q", "bad directive"},
	} {
		_, _, err := Strptime(c[0], c[1])
		var pe *Error
		if !errors.As(err, &pe) || pe.Kind != "ValueError" || !strings.Contains(err.Error(), c[2]) {
			t.Errorf("%q %q: %v", c[0], c[1], err)
		}
	}
}

func TestLocalToSecondsPicksTheFoldInstant(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	wall := func(s string) time.Time { w, _ := time.Parse("2006-01-02 15:04", s); return w }
	for _, c := range []struct {
		wall string
		fold int
		utc  string
	}{
		{"2026-03-08 02:30", 0, "2026-03-08 07:30"},
		{"2026-03-08 02:30", 1, "2026-03-08 06:30"},
		{"2026-11-01 01:30", 0, "2026-11-01 05:30"},
		{"2026-11-01 01:30", 1, "2026-11-01 06:30"},
		{"2026-07-01 12:00", 0, "2026-07-01 16:00"},
	} {
		got, ok := LocalToSeconds(wall(c.wall), ny, c.fold, nil)
		if !ok || got != wall(c.utc).Unix() {
			t.Errorf("%s fold %d gave %v, want %s", c.wall, c.fold, time.Unix(got, 0).UTC(), c.utc)
		}
	}
	if _, ok := LocalToSeconds(wall("1970-01-01 03:00"), time.FixedZone("", 5*3600), 0, func(u int64) bool { return u >= 0 }); ok {
		t.Fatal("refused probe still produced an instant")
	}
}

func TestFromTimestampInRoundsAndRangeChecks(t *testing.T) {
	got, err := FromTimestampIn(0.9999996, time.UTC, nil)
	if err != nil || got.Unix() != 1 || got.Nanosecond() != 0 {
		t.Fatalf("half even carry %v %v", got, err)
	}
	got, err = FromTimestampIn(-1.5e-6, time.UTC, nil)
	if err != nil || got.Unix() != -1 || got.Nanosecond() != 999_998_000 {
		t.Fatalf("negative fraction %v %v", got, err)
	}
	for _, ts := range []float64{math.NaN(), math.Inf(1), 1e20, -62135596801} {
		if _, err := FromTimestampIn(ts, time.UTC, nil); err == nil {
			t.Errorf("%v accepted", ts)
		}
	}
	if _, err := FromTimestampIn(253402300799, time.FixedZone("", 3600), nil); err == nil {
		t.Fatal("conversion past year 9999 accepted")
	}
	if _, err := FromTimestampIn(-1, time.UTC, WindowsLocaltimeAccepts); err == nil || err.(*Error).Kind != "OSError" {
		t.Fatalf("windows localtime accepted a negative instant %v", err)
	}
	if ts := Timestamp(time.Date(9999, 12, 31, 23, 59, 59, 999_999_000, time.UTC)); ts != 253402300799.999999 {
		t.Fatalf("exact division %v", ts)
	}
}

package pydatetime

import (
	"testing"
	"time"
)

func TestFromISOFormatMatchesCPython(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2026-01-02T03:04:05+00:00", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), true},
		{"2026-01-02T03:04:05.123456+02:00", time.Date(2026, 1, 2, 1, 4, 5, 123456000, time.UTC), true},
		{"2026-01-02", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), true},
		{"2026-01-02T03:04:05Z", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), true},
		{"20260102T030405", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), true},
		{"not-a-date", time.Time{}, false},
		{"2026-13-01", time.Time{}, false},
	}
	for _, c := range cases {
		got, err := FromISOFormat(c.in)
		if (err == nil) != c.ok || (c.ok && !got.Equal(c.want)) {
			t.Fatalf("%q: got %v %v", c.in, got, err)
		}
	}
	if _, err := FromISOFormat("bad"); err == nil || err.Error() != "Invalid isoformat string: 'bad'" {
		t.Fatalf("error text %v", err)
	}
}

func TestISOFormatUTCOmitsZeroMicroseconds(t *testing.T) {
	if got := ISOFormatUTC(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); got != "2026-01-02T03:04:05+00:00" {
		t.Fatal(got)
	}
	if got := ISOFormatUTC(time.Date(2026, 1, 2, 3, 4, 5, 7000, time.UTC)); got != "2026-01-02T03:04:05.000007+00:00" {
		t.Fatal(got)
	}
}

func TestParseISOFormatKeepsOffsetAndAwareness(t *testing.T) {
	got, aware, err := ParseISOFormat("2026-03-08T09:30:00-05:00")
	if err != nil || !aware {
		t.Fatalf("aware parse: %v %v", aware, err)
	}
	if _, off := got.Zone(); off != -5*3600 || got.Hour() != 9 || !got.Equal(time.Date(2026, 3, 8, 14, 30, 0, 0, time.UTC)) {
		t.Fatalf("offset or instant lost: %v", got)
	}
	naive, aware, err := ParseISOFormat("2026-03-08 09:30")
	if err != nil || aware || naive.Location() != time.UTC || naive.Hour() != 9 {
		t.Fatalf("naive parse: %v %v %v", naive, aware, err)
	}
	if _, _, err := ParseISOFormat("2026-13-01"); err == nil {
		t.Fatal("month 13 accepted")
	}
}

func TestISOFormatKeepsZoneOffset(t *testing.T) {
	cases := map[string]time.Time{
		"2026-03-08T09:30:00-05:00":        time.Date(2026, 3, 8, 9, 30, 0, 0, time.FixedZone("", -5*3600)),
		"2026-03-08T09:30:00.000250+05:30": time.Date(2026, 3, 8, 9, 30, 0, 250000, time.FixedZone("", 5*3600+1800)),
		"2026-03-08T09:30:00+00:00:15":     time.Date(2026, 3, 8, 9, 30, 0, 0, time.FixedZone("", 15)),
	}
	for want, in := range cases {
		if got := ISOFormat(in); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestISOFormatNaiveDropsZoneAndZeroMicroseconds(t *testing.T) {
	loc := time.FixedZone("x", 3600)
	if got := ISOFormatNaive(time.Date(2026, 1, 2, 3, 4, 5, 0, loc)); got != "2026-01-02T03:04:05" {
		t.Fatalf("got %q", got)
	}
	if got := ISOFormatNaive(time.Date(2026, 1, 2, 3, 4, 5, 7000, loc)); got != "2026-01-02T03:04:05.000007" {
		t.Fatalf("got %q", got)
	}
}

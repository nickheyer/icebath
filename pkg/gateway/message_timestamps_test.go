package gateway

import (
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"
	_ "time/tzdata"

	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Pins the system zone of the timestamp helpers for one test
func pinTimestampZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	prevZone := timestampLocalZone
	t.Cleanup(func() { timestampLocalZone = prevZone })
	timestampLocalZone = func() *time.Location { return loc }
	return loc
}

func eachTimestampPlatform(t *testing.T, body func(t *testing.T, local *time.Location)) {
	for _, zone := range []string{"America/Los_Angeles", "Europe/Berlin", "UTC"} {
		t.Run(zone, func(t *testing.T) { body(t, pinTimestampZone(t, zone)) })
	}
}

func renderTS(t *testing.T, content, value any, tz *time.Location) any {
	t.Helper()
	out, err := RenderUserContentWithTimestamp(content, value, tz)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Naive isoformat of the local wall time of an epoch
func naiveLocalISO(epoch float64, local *time.Location) string {
	wall := timeutil.FromEpoch(epoch).In(local)
	text := wall.Format("2006-01-02T15:04:05")
	if us := wall.Nanosecond() / 1000; us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text
}

func TestRenderNumericTimestampPreservesInstantInSystemTimezone(t *testing.T) {
	eachTimestampPlatform(t, func(t *testing.T, local *time.Location) {
		for _, epoch := range []float64{1.0, 1_000_000_000.0} {
			prefix := time.Unix(int64(epoch), 0).In(local).Format("Mon 2006-01-02 15:04:05 MST")
			if got := renderTS(t, "hello", epoch, nil); got != "["+prefix+"] hello" {
				t.Errorf("%v rendered %q", epoch, got)
			}
		}
	})
}

func TestEarlyNaiveISOPreservesSystemLocalInstant(t *testing.T) {
	eachTimestampPlatform(t, func(t *testing.T, local *time.Location) {
		for _, epoch := range []float64{0.0, 1.123456, 86_399.0} {
			got, ok, err := CoerceMessageTimestamp(naiveLocalISO(epoch, local), nil)
			if err != nil || !ok || math.Abs(got-epoch) > 1e-6 {
				t.Errorf("%v coerced to %v %v %v", epoch, got, ok, err)
			}
		}
	})
}

func TestEarlyEmbeddedLocalTimePreservesInstant(t *testing.T) {
	eachTimestampPlatform(t, func(t *testing.T, local *time.Location) {
		for _, epoch := range []float64{0.0, 1.0, 86_399.0} {
			wall := timeutil.FromEpoch(epoch).In(local)
			for _, stamp := range []string{wall.Format("Mon 2006-01-02 15:04:05"), naiveLocalISO(epoch, local)} {
				if got, want := renderTS(t, "["+stamp+"] hello", 1_000_000_000.0, nil), renderTS(t, "hello", epoch, nil); got != want {
					t.Errorf("%q rendered %q, want %q", stamp, got, want)
				}
			}
		}
	})
}

func TestModernLocalConversionKeepsExistingFoldAndGapBehavior(t *testing.T) {
	pinTimestampZone(t, "America/New_York")
	for _, c := range []struct {
		wall string
		utc  string
	}{
		{"2026-01-15 12:00", "2026-01-15 17:00"},
		{"2026-07-15 12:00", "2026-07-15 16:00"},
		{"2026-03-08 02:30", "2026-03-08 06:30"},
		{"2026-11-01 01:30", "2026-11-01 05:30"},
	} {
		want, _ := time.Parse("2006-01-02 15:04", c.utc)
		if got, ok := parseTimestampISO(c.wall, nil); !ok || got != float64(want.Unix()) {
			t.Errorf("%s gave %v", c.wall, time.Unix(int64(got), 0).UTC())
		}
	}
}

func TestExplicitTimezoneParsingPreservesWallTime(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	for text, want := range map[string]time.Time{
		"1969-06-01T12:00:00": time.Date(1969, 6, 1, 12, 0, 0, 0, berlin),
		"2026-07-15T12:00:00": time.Date(2026, 7, 15, 12, 0, 0, 0, berlin),
	} {
		if got, ok, err := CoerceMessageTimestamp(text, berlin); err != nil || !ok || got != float64(want.Unix()) {
			t.Errorf("%s coerced to %v %v %v", text, got, ok, err)
		}
	}
}

func TestUnrenderableEpochPreservesTheMessageBody(t *testing.T) {
	eachTimestampPlatform(t, func(t *testing.T, _ *time.Location) {
		for _, epoch := range []float64{math.NaN(), math.Inf(1)} {
			if got, err := FormatMessageTimestamp(epoch, nil); got != "" || err != nil {
				t.Errorf("%v formatted %q %v", epoch, got, err)
			}
			if got := renderTS(t, "hello", epoch, nil); got != "hello" {
				t.Errorf("%v rendered %q", epoch, got)
			}
		}
	})
}

func TestRenderUserContentDeduplicatesExistingTimestampAndPreservesEmbeddedTime(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	processing := float64(time.Date(2026, 4, 27, 15, 55, 36, 0, berlin).Unix())
	stored := "[Mon 2026-04-27 15:54:44 CEST] [Example User] This should go on our todo list"
	if got := renderTS(t, stored, processing, berlin); got != stored {
		t.Fatalf("rendered %q", got)
	}
}

func TestTimestampHelpersEdgeCases(t *testing.T) {
	pinTimestampZone(t, "UTC")
	for value, want := range map[any]float64{
		true: 1, int64(7): 7, " 1_000.5 ": 1000.5, "[Tue 2026-04-28 13:40:53 CEST] x": 1777383653,
		"2026-04-28T13:40:53+0200": 1777376453, "[2026-04-28T13:40:53+02:00] x": 1777376453, time.Unix(5, 250_000_000): 5.25,
	} {
		if got, ok, err := CoerceMessageTimestamp(value, nil); err != nil || !ok || got != want {
			t.Errorf("%#v coerced to %v %v %v", value, got, ok, err)
		}
	}
	for _, value := range []any{nil, "", "  ", "soon", []any{1}, "[Tue 2026-02-30 13:40:53] x"} {
		if got, ok, err := CoerceMessageTimestamp(value, nil); ok || err != nil {
			t.Errorf("%#v coerced to %v %v", value, got, err)
		}
	}
	if _, _, err := CoerceMessageTimestamp(new(big.Int).Lsh(big.NewInt(1), 1100), nil); err == nil {
		t.Fatal("huge int converted without a range error")
	}
	clean, embedded := StripLeadingMessageTimestamps("[Tue 2026-04-28 13:40:53] [Wed 2026-04-29 10:00:00 UTC]  [sender] hi", nil)
	if clean != "[sender] hi" || embedded == nil || *embedded != 1777456800 {
		t.Fatalf("closest prefix wins %q %v", clean, embedded)
	}
	parts := []any{map[string]any{"type": "text"}}
	if got := renderTS(t, parts, 0.0, nil); got != `[Thu 1970-01-01 00:00:00 UTC] [{"type":"text"}]` {
		t.Fatalf("list content %q", got)
	}
	if got := renderTS(t, parts, nil, nil); fmt.Sprint(got) != fmt.Sprint(parts) {
		t.Fatalf("unstamped list content %#v", got)
	}
	if got := renderTS(t, "", 0.0, nil); got != "[Thu 1970-01-01 00:00:00 UTC]" {
		t.Fatalf("empty content %q", got)
	}
}

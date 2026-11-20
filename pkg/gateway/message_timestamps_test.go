package gateway

import (
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
)

// Pins the system zone and the localtime model of the timestamp helpers for one test
func pinTimestampZone(t *testing.T, name string, windows bool) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	prevZone, prevAccepts := timestampLocalZone, timestampLocaltimeAccepts
	t.Cleanup(func() { timestampLocalZone, timestampLocaltimeAccepts = prevZone, prevAccepts })
	timestampLocalZone = func() *time.Location { return loc }
	timestampLocaltimeAccepts = func(int64) bool { return true }
	if windows {
		timestampLocaltimeAccepts = pydatetime.WindowsLocaltimeAccepts
	}
	return loc
}

func eachTimestampPlatform(t *testing.T, windowsOnly bool, body func(t *testing.T, local *time.Location)) {
	for _, zone := range []string{"America/Los_Angeles", "Europe/Berlin", "UTC"} {
		for _, windows := range []bool{false, true} {
			if windowsOnly && !windows {
				continue
			}
			t.Run(fmt.Sprintf("%s/windows=%v", zone, windows), func(t *testing.T) { body(t, pinTimestampZone(t, zone, windows)) })
		}
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
	wall, _ := pydatetime.FromTimestampIn(epoch, local, nil)
	text := wall.Format("2006-01-02T15:04:05")
	if us := wall.Nanosecond() / 1000; us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text
}

func TestRenderNumericTimestampPreservesInstantInSystemTimezone(t *testing.T) {
	eachTimestampPlatform(t, false, func(t *testing.T, local *time.Location) {
		for _, epoch := range []float64{1.0, 1_000_000_000.0} {
			prefix := config.SafeStrftime(time.Unix(int64(epoch), 0).In(local), "%a %Y-%m-%d %H:%M:%S %Z")
			if got := renderTS(t, "hello", epoch, nil); got != "["+prefix+"] hello" {
				t.Errorf("%v rendered %q", epoch, got)
			}
		}
	})
}

func TestEarlyNaiveISOPreservesSystemLocalInstant(t *testing.T) {
	eachTimestampPlatform(t, true, func(t *testing.T, local *time.Location) {
		for _, epoch := range []float64{0.0, 1.123456, 86_399.0} {
			got, ok, err := CoerceMessageTimestamp(naiveLocalISO(epoch, local), nil)
			if err != nil || !ok || math.Abs(got-epoch) > 1e-6 {
				t.Errorf("%v coerced to %v %v %v", epoch, got, ok, err)
			}
		}
	})
}

func TestEarlyEmbeddedLocalTimePreservesInstant(t *testing.T) {
	eachTimestampPlatform(t, true, func(t *testing.T, local *time.Location) {
		for _, epoch := range []float64{0.0, 1.0, 86_399.0} {
			wall, _ := pydatetime.FromTimestampIn(epoch, local, nil)
			for _, stamp := range []string{config.SafeStrftime(wall, "%a %Y-%m-%d %H:%M:%S"), naiveLocalISO(epoch, local)} {
				if got, want := renderTS(t, "["+stamp+"] hello", 1_000_000_000.0, nil), renderTS(t, "hello", epoch, nil); got != want {
					t.Errorf("%q rendered %q, want %q", stamp, got, want)
				}
			}
		}
	})
}

func TestModernLocalConversionKeepsExistingFoldAndGapBehavior(t *testing.T) {
	for _, windows := range []bool{false, true} {
		pinTimestampZone(t, "America/New_York", windows)
		for _, c := range []struct {
			wall string
			fold int
			utc  string
		}{
			{"2026-01-15 12:00", 0, "2026-01-15 17:00"}, {"2026-01-15 12:00", 1, "2026-01-15 17:00"},
			{"2026-07-15 12:00", 0, "2026-07-15 16:00"}, {"2026-07-15 12:00", 1, "2026-07-15 16:00"},
			{"2026-03-08 02:30", 0, "2026-03-08 07:30"}, {"2026-03-08 02:30", 1, "2026-03-08 06:30"},
			{"2026-11-01 01:30", 0, "2026-11-01 05:30"}, {"2026-11-01 01:30", 1, "2026-11-01 06:30"},
		} {
			wall, _ := time.Parse("2006-01-02 15:04", c.wall)
			want, _ := time.Parse("2006-01-02 15:04", c.utc)
			if got, ok := localizeTimestamp(wall, false, c.fold, nil); !ok || got != float64(want.Unix()) {
				t.Errorf("%s fold %d windows %v gave %v", c.wall, c.fold, windows, time.Unix(int64(got), 0).UTC())
			}
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

func TestUnsupportedLocalISOIsUninterpretableInsteadOfRaising(t *testing.T) {
	eachTimestampPlatform(t, true, func(t *testing.T, _ *time.Location) {
		if got, ok, err := CoerceMessageTimestamp("1969-06-01T12:00:00", nil); ok || err != nil {
			t.Fatalf("pre epoch local iso coerced to %v %v", got, err)
		}
	})
}

func TestUnrenderableEpochPreservesTheMessageBody(t *testing.T) {
	eachTimestampPlatform(t, true, func(t *testing.T, _ *time.Location) {
		for _, epoch := range []float64{math.NaN(), math.Inf(1), -1.0} {
			if got, err := FormatMessageTimestamp(epoch, nil); got != "" || err != nil {
				t.Errorf("%v formatted %q %v", epoch, got, err)
			}
			if got := renderTS(t, "hello", epoch, nil); got != "hello" {
				t.Errorf("%v rendered %q", epoch, got)
			}
		}
	})
}

func TestUnsupportedEmbeddedTimeKeepsMetadataFallback(t *testing.T) {
	eachTimestampPlatform(t, true, func(t *testing.T, _ *time.Location) {
		if got, want := renderTS(t, "[Sun 1969-06-01 12:00:00] hello", 1_000_000_000.0, nil), renderTS(t, "hello", 1_000_000_000.0, nil); got != want {
			t.Fatalf("rendered %q, want %q", got, want)
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

func TestTimestampHelpersFollowPythonEdgeCases(t *testing.T) {
	pinTimestampZone(t, "UTC", false)
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
		t.Fatal("huge int converted without OverflowError")
	}
	clean, embedded := StripLeadingMessageTimestamps("[Tue 2026-04-28 13:40:53] [Wed 2026-04-29 10:00:00 UTC]  [sender] hi", nil)
	if clean != "[sender] hi" || embedded == nil || *embedded != 1777456800 {
		t.Fatalf("closest prefix wins %q %v", clean, embedded)
	}
	parts := []any{map[string]any{"type": "text"}}
	if got := renderTS(t, parts, 0.0, nil); got != "[Thu 1970-01-01 00:00:00 UTC] [{'type': 'text'}]" {
		t.Fatalf("list content %q", got)
	}
	if got := renderTS(t, parts, nil, nil); fmt.Sprint(got) != fmt.Sprint(parts) {
		t.Fatalf("unstamped list content %#v", got)
	}
	if got := renderTS(t, "", 0.0, nil); got != "[Thu 1970-01-01 00:00:00 UTC]" {
		t.Fatalf("empty content %q", got)
	}
}

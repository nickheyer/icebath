package state

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMintedIDsAreKnownSessionIDs(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	for hexLen, expected := range map[int]string{6: `^\d{8}_\d{6}_[0-9a-f]{6}$`, 8: `^\d{8}_\d{6}_[0-9a-f]{8}$`, 12: `^\d{8}_\d{6}_[0-9a-f]{12}$`} {
		sid := NewSessionIDAt(at, hexLen)
		if !regexp.MustCompile(expected).MatchString(sid) || !strings.HasPrefix(sid, "20260102_030405_") {
			t.Errorf("hex %d minted %q", hexLen, sid)
		}
		if !IsKnownSessionID(sid) {
			t.Errorf("%q not recognised", sid)
		}
	}
	if got := len(NewSessionID()); got != len("20060102_150405_")+DefaultHexLen {
		t.Errorf("default id length %d", got)
	}
}

func TestKnownSessionIDShapes(t *testing.T) {
	known := []string{"cron_job.1_20260102_030405", "bg_123456_abcdef", "bg_abcdef", "room_" + strings.Repeat("a", 32),
		"api_123456789_abcdef01", "api-0123456789abcdef", "run_" + strings.Repeat("f", 32),
		"12345678-1234-4abc-8def-123456789abc", "bg_abcdef\n", "\u0661\u0662\u0663\u0664\u0665\u0666\u0667\u0668_123456_x"}
	for _, id := range known {
		if !IsKnownSessionID(id) {
			t.Errorf("%q should be known", id)
		}
	}
	for _, id := range []string{"bg_abcdeg", "api_12345678_abcdef01", "12345678-1234-5abc-8def-123456789abc", "session", "bg_abcdef\n\n"} {
		if IsKnownSessionID(id) {
			t.Errorf("%q should be unknown", id)
		}
	}
}

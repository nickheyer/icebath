package pydatetime

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCoerceEpochAcceptsAndRejects(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{nil, 0, false}, {"", 0, false}, {1700000000.5, 1700000000.5, true}, {int64(42), 42, true},
		{" 12.5 ", 12.5, true}, {"not-a-timestamp", 0, false}, {8.4e252, 0, false}, {math.Inf(1), 0, false},
		{math.NaN(), 0, false}, {-1.0, 0, false}, {time.Unix(100, 0), 100, true}, {true, 1, true},
	}
	for _, c := range cases {
		got, ok := CoerceEpoch(c.in, "sid-1", "")
		if ok != c.ok || got != c.want {
			t.Fatalf("CoerceEpoch(%v) = %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
	out := buf.String()
	if !strings.Contains(out, "Ignoring corrupt timestamp 'not-a-timestamp' on session sid-1") || !strings.Contains(out, "8.4e+252") {
		t.Fatalf("warnings %q", out)
	}
	if strings.Count(out, "level=WARN") != 5 {
		t.Fatalf("unset values must stay silent, got %q", out)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	n := float64(now.Unix())
	cases := map[any]string{
		nil: "?", 0.0: "?", "garbage": "?", n - 10: "just now", n - 120: "2m ago", n - 7200: "2h ago",
		n - 90000: "yesterday", n - 3*86400: "3d ago",
	}
	for in, want := range cases {
		if got := RelativeTime(in, "", now); got != want {
			t.Fatalf("RelativeTime(%v) = %q, want %q", in, got, want)
		}
	}
	old := n - 30*86400
	if got, want := RelativeTime(old, "", now), FromTimestamp(old).Format("2006-01-02"); got != want {
		t.Fatalf("old = %q, want %q", got, want)
	}
}

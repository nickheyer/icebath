package cron

import (
	"testing"
	"time"
)

func wall(y int, m time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, time.UTC)
}

func TestParseCronRejectsBadInput(t *testing.T) {
	for _, expr := range []string{"", "0 9 * *", "0 9 * * FUNDAY", "60 * * * *", "* 24 * * *", "* * 32 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "a b c d e f g h", "0 9 * * 5#6", "H * * * *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) accepted", expr)
		}
	}
}

func TestParseCronAcceptsHermesForms(t *testing.T) {
	for _, expr := range []string{"0 9 * * *", "0 9 * * MON", "*/15 9-17 * * MON-FRI", "0 9 1 JAN *", "0 9 * * MON,WED,FRI", "0 9 * * 7", "0 9 L * *", "0 9 * * 5L", "0 9 * * 1#2", "0 9 ? * ?", "0 0 * * 0 30", "0 0 1 1 * 0 2030", "@daily", "30 1 * * *", "0 9-17/2 * * *", "5/15 * * * *"} {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q): %v", expr, err)
		}
	}
}

func TestNextTable(t *testing.T) {
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"0 9 * * *", wall(2026, 3, 7, 9, 0, 0), wall(2026, 3, 8, 9, 0, 0)},
		{"0 9 * * *", wall(2026, 3, 7, 9, 0, 30), wall(2026, 3, 8, 9, 0, 0)},
		{"0 9 * * *", wall(2026, 3, 7, 8, 59, 59), wall(2026, 3, 7, 9, 0, 0)},
		{"*/15 9-17 * * MON-FRI", wall(2026, 8, 22, 10, 0, 0), wall(2026, 8, 24, 9, 0, 0)},
		{"0 7 * * 1-5", wall(2026, 8, 22, 6, 0, 0), wall(2026, 8, 24, 7, 0, 0)},
		{"0 9 1 JAN *", wall(2026, 2, 1, 0, 0, 0), wall(2027, 1, 1, 9, 0, 0)},
		{"0 9 L * *", wall(2026, 2, 10, 0, 0, 0), wall(2026, 2, 28, 9, 0, 0)},
		{"0 9 L * *", wall(2028, 2, 10, 0, 0, 0), wall(2028, 2, 29, 9, 0, 0)},
		{"0 9 * * 5L", wall(2026, 8, 1, 0, 0, 0), wall(2026, 8, 28, 9, 0, 0)},
		{"0 9 * * 1#2", wall(2026, 8, 1, 0, 0, 0), wall(2026, 8, 10, 9, 0, 0)},
		{"0 9 15 * 1", wall(2026, 8, 11, 0, 0, 0), wall(2026, 8, 15, 9, 0, 0)},
		{"0 9 15 * 1", wall(2026, 8, 16, 0, 0, 0), wall(2026, 8, 17, 9, 0, 0)},
		{"0 9 * * 7", wall(2026, 8, 20, 0, 0, 0), wall(2026, 8, 23, 9, 0, 0)},
		{"0 0 * * 0 30", wall(2026, 8, 23, 0, 0, 0), wall(2026, 8, 23, 0, 0, 30)},
		{"0 0 1 1 * 0 2030", wall(2026, 1, 1, 0, 0, 0), wall(2030, 1, 1, 0, 0, 0)},
		{"5/15 * * * *", wall(2026, 1, 1, 0, 0, 0), wall(2026, 1, 1, 0, 5, 0)},
		{"5/15 * * * *", wall(2026, 1, 1, 0, 5, 0), wall(2026, 1, 1, 0, 20, 0)},
		{"0 9-17/2 * * *", wall(2026, 1, 1, 9, 0, 0), wall(2026, 1, 1, 11, 0, 0)},
		{"@hourly", wall(2026, 1, 1, 9, 10, 0), wall(2026, 1, 1, 10, 0, 0)},
		{"* * * * *", wall(2026, 11, 1, 1, 59, 0), wall(2026, 11, 1, 2, 0, 0)},
	}
	for _, c := range cases {
		e, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", c.expr, err)
		}
		got, err := e.Next(c.from)
		if err != nil {
			t.Fatalf("next %q from %v: %v", c.expr, c.from, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("Next(%q, %v) = %v, want %v", c.expr, c.from, got, c.want)
		}
	}
}

func TestPrevTable(t *testing.T) {
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"0 9 * * *", wall(2026, 3, 8, 9, 0, 1), wall(2026, 3, 8, 9, 0, 0)},
		{"0 9 * * *", wall(2026, 3, 8, 9, 0, 0), wall(2026, 3, 7, 9, 0, 0)},
		{"0 7 * * 1-5", wall(2026, 8, 22, 7, 0, 1), wall(2026, 8, 21, 7, 0, 0)},
		{"0 0 * * 0 30", wall(2026, 8, 23, 0, 0, 31), wall(2026, 8, 23, 0, 0, 30)},
	}
	for _, c := range cases {
		e, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", c.expr, err)
		}
		got, err := e.Prev(c.from)
		if err != nil {
			t.Fatalf("prev %q: %v", c.expr, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("Prev(%q, %v) = %v, want %v", c.expr, c.from, got, c.want)
		}
	}
}

func TestNextUnreachableFails(t *testing.T) {
	e, err := ParseCron("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(wall(2026, 1, 1, 0, 0, 0)); err == nil {
		t.Fatal("expected no occurrence for Feb 31")
	}
}

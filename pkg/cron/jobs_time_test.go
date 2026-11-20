package cron

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTimezoneContract(t *testing.T) {
	for in, want := range map[string]string{"America/New_York": "America/New_York", "  Asia/Hong_Kong  ": "Asia/Hong_Kong"} {
		got, err := NormalizeTimezone(in)
		if err != nil || got == nil || *got != want {
			t.Fatalf("NormalizeTimezone(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []any{nil, "", "   "} {
		if got, err := NormalizeTimezone(in); err != nil || got != nil {
			t.Fatalf("empty %q must inherit, got %v %v", in, got, err)
		}
	}
	for _, in := range []string{"Mars/Olympus_Mons", "not a zone", "/etc/passwd"} {
		if _, err := NormalizeTimezone(in); err == nil || !strings.Contains(err.Error(), "timezone") {
			t.Fatalf("unknown zone %q accepted: %v", in, err)
		}
	}
	if _, err := NormalizeTimezone(9); err == nil || !strings.Contains(err.Error(), "IANA name") {
		t.Fatalf("non string accepted: %v", err)
	}
}

func TestNormalizeGraceMinutesContract(t *testing.T) {
	for _, in := range []any{nil, "", "  "} {
		if got, err := NormalizeGraceMinutes(in); err != nil || got != nil {
			t.Fatalf("empty %q must defer, got %v %v", in, got, err)
		}
	}
	for _, in := range []any{0, "0"} {
		got, err := NormalizeGraceMinutes(in)
		if err != nil || got == nil || *got != 0 {
			t.Fatalf("zero %v must survive, got %v %v", in, got, err)
		}
	}
	if got, err := NormalizeGraceMinutes(24 * 60); err != nil || *got != 24*60 {
		t.Fatalf("one day rejected: %v", err)
	}
	for in, frag := range map[any]string{24*60 + 1: "between", -1: "between", true: "boolean", false: "boolean", "soon": "grace_minutes", "1.5": "grace_minutes", 1.5: "whole number"} {
		if _, err := NormalizeGraceMinutes(in); err == nil || !strings.Contains(err.Error(), frag) {
			t.Fatalf("NormalizeGraceMinutes(%v) error %v lacks %q", in, err, frag)
		}
	}
	if _, err := NormalizeGraceMinutes([]any{30}); err == nil || !strings.Contains(err.Error(), "grace_minutes") {
		t.Fatalf("list accepted: %v", err)
	}
}

func capturingWarner() (*knobWarner, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return newKnobWarner(slog.New(slog.NewTextHandler(buf, nil))), buf
}

func TestZoneForJobContract(t *testing.T) {
	w, buf := capturingWarner()
	empty := ""
	if w.zoneForJob(nil) != nil || w.zoneForJob(&empty) != nil {
		t.Fatal("absent zone must inherit")
	}
	ny := "America/New_York"
	if loc := w.zoneForJob(&ny); loc == nil || loc.String() != ny {
		t.Fatalf("pinned zone lost: %v", loc)
	}
	bad := "Mars/Olympus_Mons"
	for i := 0; i < 3; i++ {
		if w.zoneForJob(&bad) != nil {
			t.Fatal("corrupt zone must degrade")
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), bad) {
		t.Fatalf("bad zone must warn once naming it, got %d: %s", n, buf.String())
	}
}

func TestGraceSecondsOverrideContract(t *testing.T) {
	w, buf := capturingWarner()
	if w.graceSecondsOverride(nil, nil) != nil {
		t.Fatal("absent override must defer")
	}
	if got := w.graceSecondsOverride(intPtr(30), nil); got == nil || *got != 1800 {
		t.Fatalf("30 minutes = %v", got)
	}
	if got := w.graceSecondsOverride(intPtr(0), nil); got == nil || *got != 0 {
		t.Fatalf("zero override = %v", got)
	}
	if w.graceSecondsOverride(nil, "tomorrow") != nil || w.graceSecondsOverride(nil, "tomorrow") != nil {
		t.Fatal("corrupt override must be ignored")
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("corrupt override must warn once, got %d", n)
	}
}

func TestPruneInheritedKnobsContract(t *testing.T) {
	job := &Job{ID: "a", Timezone: strPtr(""), GraceMinutes: intPtr(0)}
	pruneInheritedKnobs(job)
	if job.Timezone != nil || job.GraceMinutes == nil || *job.GraceMinutes != 0 {
		t.Fatalf("prune = %v %v", job.Timezone, job.GraceMinutes)
	}
	job = &Job{Timezone: strPtr("Asia/Hong_Kong")}
	pruneInheritedKnobs(job)
	if job.Timezone == nil || *job.Timezone != "Asia/Hong_Kong" {
		t.Fatal("pinned zone dropped")
	}
}

func TestOneshotInstantContract(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	c := &clock{}
	got, ok := c.oneshotInstant("2026-06-01T09:00:00", ny)
	if !ok || !got.Equal(time.Date(2026, 6, 1, 9, 0, 0, 0, ny)) {
		t.Fatalf("offsetless = %v", got)
	}
	got, ok = c.oneshotInstant("2026-06-01T09:00:00+00:00", ny)
	if !ok || !got.Equal(time.Date(2026, 6, 1, 5, 0, 0, 0, ny)) {
		t.Fatalf("offset bearing = %v", got)
	}
	if _, ok := c.oneshotInstant("not-a-time", ny); ok {
		t.Fatal("garbage parsed")
	}
}

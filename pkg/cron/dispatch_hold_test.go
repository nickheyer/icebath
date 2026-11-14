package cron

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func heldOneshot(id string, runAt time.Time) map[string]any {
	return map[string]any{
		"id": id, "name": id, "prompt": "x",
		"schedule":    map[string]any{"kind": "once", "run_at": FormatISO(runAt)},
		"next_run_at": FormatISO(runAt), "last_run_at": nil,
		"enabled": true, "state": "scheduled", "repeat": map[string]any{"times": 1, "completed": 0},
		"deliver": "local", "run_claim": nil, "fire_claim": nil,
	}
}

func mustNoteHeld(t *testing.T, s *Store, at *float64) {
	t.Helper()
	if err := s.noteHeld(context.Background(), at); err != nil {
		t.Fatal(err)
	}
}

func TestOneshotDueDuringADrainFiresOnceTheNextGatewayTicks(t *testing.T) {
	t0 := float64(time.Now().UnixNano()) / 1e9
	start := time.Unix(0, int64(t0*1e9)).UTC()
	scan := start.Add(100 * time.Second)
	s := newClockStore(t, &scan, time.UTC)
	os.MkdirAll(s.cronDir, 0o700)
	began := t0 - 100
	mustNoteHeld(t, s, &began)
	mustNoteHeld(t, s, nil)
	t.Cleanup(s.noteDispatching)
	seedJobs(t, s, heldOneshot("reminder", start.Add(-90*time.Second)))
	if due := jobIDs(mustDue(t, s)); !reflect.DeepEqual(due, []string{"reminder"}) {
		t.Fatalf("due = %v", due)
	}
	if ids := diskJobIDs(t, s); !reflect.DeepEqual(ids, []string{"reminder"}) {
		t.Fatalf("stored = %v", ids)
	}
	if held, err := readDispatchHeld(s.cronDir); err != nil || held == nil || held.Since != began {
		t.Fatalf("hold stamp %+v", held)
	}
}

func TestStaleOneshotIsRetiredDespiteAHold(t *testing.T) {
	var live []float64
	for ago := 5 * 3600; ago > 89; ago -= 60 {
		live = append(live, float64(ago))
	}
	for name, tc := range map[string]struct {
		ticksAgo []float64
		dueAgo   float64
	}{
		"gateway-never-returned": {[]float64{3*3600 + 60, 3 * 3600}, 3*3600 + 30},
		"hours-long-live-hold":   {live, 5 * 3600},
	} {
		t.Run(name, func(t *testing.T) {
			t0 := float64(time.Now().UnixNano()) / 1e9
			start := time.Unix(0, int64(t0*1e9)).UTC()
			s := newClockStore(t, &start, time.UTC)
			os.MkdirAll(s.cronDir, 0o700)
			t.Cleanup(s.noteDispatching)
			for _, ago := range tc.ticksAgo {
				at := t0 - ago
				mustNoteHeld(t, s, &at)
			}
			seedJobs(t, s, heldOneshot("reminder", start.Add(-time.Duration(tc.dueAgo*float64(time.Second)))))
			if due := mustDue(t, s); len(due) != 0 {
				t.Fatalf("stale one shot fired %v", jobIDs(due))
			}
			if ids := diskJobIDs(t, s); len(ids) != 0 {
				t.Fatalf("stale one shot kept %v", ids)
			}
		})
	}
}

func TestDispatchHoldGraceStartsAt(t *testing.T) {
	dir := t.TempDir()
	grace := func(dueAt float64) float64 {
		t.Helper()
		got, err := dispatchHoldGraceStartsAt(dir, dueAt, 120)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := grace(1000); got != 1000 {
		t.Fatalf("no hold = %v", got)
	}
	os.WriteFile(dir+"/"+dispatchHeldFile, []byte("\ufeff{\"since\": 900, \"last\": \"1100\"}"), 0o600)
	if got := grace(1000); got != 1160 {
		t.Fatalf("held = %v", got)
	}
	if got := grace(700); got != 700 {
		t.Fatalf("expired before the hold = %v", got)
	}
	for _, malformed := range []string{"[1, 2]", `{"since": 900}`, `{"since": null, "last": 1}`, `{"since": "soon", "last": 1}`,
		"{\"since\": 900, \"last\": 1100, \"x\": \"\xff\"}"} {
		os.WriteFile(dir+"/"+dispatchHeldFile, []byte(malformed), 0o600)
		if held, err := readDispatchHeld(dir); held != nil || err != nil {
			t.Fatalf("malformed stamp %q read as %+v (%v)", malformed, held, err)
		}
	}
	os.WriteFile(dir+"/"+dispatchHeldFile, []byte(`{"since": 1`+strings.Repeat("0", 400)+`, "last": 1}`), 0o600)
	if _, err := dispatchHoldGraceStartsAt(dir, 1000, 120); err == nil || err.Error() != "int too large to convert to float" {
		t.Fatalf("overflowing stamp = %v", err)
	}
	s := newClockStore(t, nil, time.UTC)
	s.cronDir = dir
	t.Cleanup(s.noteDispatching)
	if err := s.noteHeld(context.Background(), nil); err == nil {
		t.Fatal("an overflowing stamp started a hold")
	}
}

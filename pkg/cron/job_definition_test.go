package cron

import (
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestDistributionZoneChangeRecomputesTheInstant(t *testing.T) {
	s, fc, _ := laStore(t)
	ny := mustZone(t, "America/New_York")
	fc.Set(time.Date(2026, 10, 22, 12, 0, 0, 0, ny))
	local := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: daily9AM, Model: "fixture", Deliver: prompt("local"), Timezone: "Asia/Hong_Kong"})
	merged, err := s.mergeJobDefinition(local, map[string]any{"prompt": "p", "schedule": local.Schedule, "timezone": "America/New_York"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parseInstant(t, merged.NextRunAt); !got.Equal(time.Date(2026, 10, 23, 9, 0, 0, 0, ny)) {
		t.Fatalf("next run %s", *merged.NextRunAt)
	}
}

func TestAProfileDistributionCarriesTheJobsMissedRunPolicy(t *testing.T) {
	s, _ := newTestStore(t)
	local := mustCreate(t, s, CreateJobOptions{Prompt: prompt("brief"), Schedule: "0 7 * * *", Deliver: prompt("local"), CatchUp: true})
	merged, err := s.mergeJobDefinition(local, map[string]any{"prompt": "brief", "schedule": local.Schedule, "catch_up": false})
	if err != nil {
		t.Fatal(err)
	}
	if merged.CatchUp == nil || *merged.CatchUp {
		t.Fatalf("catch_up %v", merged.CatchUp)
	}
}

func TestMergeKeepsSchedulerStateAndDropsUnshippedDefinitionFields(t *testing.T) {
	s, _ := newTestStore(t)
	local := mustCreate(t, s, CreateJobOptions{Prompt: prompt("old"), Schedule: "every 1h", Deliver: prompt("local"), Model: "m", Repeat: 5})
	mustMark(t, s, local.ID, true, MarkOptions{})
	local = mustGet(t, s, local.ID)
	merged, err := s.mergeJobDefinition(local, map[string]any{"prompt": "new", "schedule": "every 1h", "repeat": map[string]any{"times": 9}})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Prompt != "new" || merged.Model != nil || deref(merged.LastRunAt) != deref(local.LastRunAt) {
		t.Fatalf("merged %+v", merged)
	}
	if merged.Repeat == nil || merged.Repeat.Completed != 1 || merged.Repeat.Times == nil || *merged.Repeat.Times != 9 {
		t.Fatalf("repeat %+v", merged.Repeat)
	}
	if deref(merged.NextRunAt) != deref(local.NextRunAt) {
		t.Fatal("unchanged schedule moved next_run_at")
	}
	if _, err := s.mergeJobDefinition(local, map[string]any{"schedule": "not a schedule"}); err == nil {
		t.Fatal("unparseable schedule merged")
	}
}

func TestImportJobDefinitionsSeedsNewIdsPausedAndNamesRefusals(t *testing.T) {
	s, _ := newTestStore(t)
	kept := mustCreate(t, s, CreateJobOptions{Prompt: prompt("mine"), Schedule: "every 1h", Deliver: prompt("local")})
	shipped := pyjson.NewObject()
	shipped.Set("newjob", map[string]any{"name": "fresh", "prompt": "hello", "schedule": map[string]any{"kind": "interval", "minutes": 30, "display": "every 30m"}})
	if err := s.importJobDefinitions(shipped, "review first"); err != nil {
		t.Fatal(err)
	}
	fresh := mustGet(t, s, "newjob")
	if fresh.Enabled || fresh.State != "paused" || deref(fresh.PausedReason) != "review first" || fresh.NextRunAt != nil || fresh.Prompt != "hello" {
		t.Fatalf("seeded %+v", fresh)
	}
	if mustGet(t, s, kept.ID).Prompt != "mine" {
		t.Fatal("unshipped local job changed")
	}
	bad := pyjson.NewObject()
	bad.Set(kept.ID, map[string]any{"name": "broken", "schedule": "not a schedule"})
	err := s.importJobDefinitions(bad, "review first")
	if err == nil || !strings.HasPrefix(err.Error(), "cron job 'broken': ") {
		t.Fatalf("refusal %v", err)
	}
	if mustGet(t, s, kept.ID).Prompt != "mine" {
		t.Fatal("refused import wrote the store")
	}
}

func TestMergeRaisesThePythonErrorForMalformedRecords(t *testing.T) {
	s, _ := newTestStore(t)
	local := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: "every 1h"})
	cases := []struct {
		authored any
		want     string
	}{
		{nil, "argument of type 'NoneType' is not iterable"},
		{5, "argument of type 'int' is not iterable"},
		{"username", "string indices must be integers, not 'str'"},
		{"xyz", "'str' object has no attribute 'get'"},
		{[]any{"prompt"}, "list indices must be integers or slices, not str"},
		{[]any{1}, "'list' object has no attribute 'get'"},
		{map[string]any{"repeat": 3}, "'int' object has no attribute 'get'"},
	}
	for _, tc := range cases {
		_, err := s.mergeOrName(local, tc.authored)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%v merged with %v want %q", tc.authored, err, tc.want)
		}
	}
}

func TestImportKeepsALocalJobShippedAsNull(t *testing.T) {
	s, _ := newTestStore(t)
	local := mustCreate(t, s, CreateJobOptions{Prompt: prompt("mine"), Schedule: "every 1h"})
	shipped := pyjson.NewObject()
	shipped.Set(local.ID, nil)
	if err := s.importJobDefinitions(shipped, "review"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, local.ID); got.Prompt != "mine" || !got.Enabled {
		t.Fatalf("null record changed %+v", got)
	}
	orphan := pyjson.NewObject()
	orphan.Set("ghost", nil)
	if err := s.importJobDefinitions(orphan, "review"); err == nil || err.Error() != "argument of type 'NoneType' is not iterable" {
		t.Fatalf("unmatched null record %v", err)
	}
}

package app

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Namespace of the filter flags, every one unset but the overrides
func filterNamespace(overrides map[string]any) map[string]any {
	ns := map[string]any{}
	for _, name := range filterArgs {
		ns[name] = nil
	}
	for k, v := range overrides {
		ns[k] = v
	}
	return ns
}

func TestParseDurationSecondsValid(t *testing.T) {
	cases := map[string]float64{"30m": 1800, "5h": 18000, "2d": 172800, "1w": 604800, "90": 90 * 86400, "1.5h": 5400, "10 min": 600, "2 hours": 7200}
	for value, expected := range cases {
		got, ok := parseDurationSeconds(value)
		if !ok || math.Abs(got-expected) > expected*1e-6 {
			t.Fatalf("%q parsed to %v %v, want %v", value, got, ok, expected)
		}
	}
	if _, ok := parseDurationSeconds("nonsense"); ok {
		t.Fatal("nonsense parsed as a duration")
	}
}

func TestParsePointInTimeDurationIsRelativeToNow(t *testing.T) {
	ts, err := parsePointInTime("5h", "--before")
	if want := float64(time.Now().Unix()) - 18000; err != nil || math.Abs(ts-want) > 5 {
		t.Fatalf("ts %v err %v, want about %v", ts, err, want)
	}
}

func TestParsePointInTimeInvalidRaisesWithFlagName(t *testing.T) {
	_, err := parsePointInTime("nonsense", "--older-than")
	if !isValueError(err) || !strings.Contains(err.Error(), "--older-than") {
		t.Fatalf("err %v", err)
	}
}

func TestBuildPruneFiltersOlderThanBareDays(t *testing.T) {
	f, err := buildPruneFilters(filterNamespace(map[string]any{"older_than": "90"}))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f["last_active_before"].(float64)
	if want := float64(time.Now().Unix()) - 90*86400; math.Abs(before-want) > 5 {
		t.Fatalf("last_active_before %v, want about %v", f["last_active_before"], want)
	}
	if v, ok := f["started_before"]; !ok || v != nil {
		t.Fatalf("started_before %v present %v", v, ok)
	}
	if v, ok := f["started_after"]; !ok || v != nil {
		t.Fatalf("started_after %v present %v", v, ok)
	}
}

package hostinfo

import "testing"

func fakeSensor(t *testing.T, percent float64, plugged bool) {
	t.Helper()
	sensorsBattery = func() (sensorReading, bool) { return sensorReading{percent: &percent, plugged: &plugged}, true }
}

func TestReadBatteryCaches(t *testing.T) {
	prev := sensorsBattery
	t.Cleanup(func() { sensorsBattery = prev; ClearBatteryCache() })
	ClearBatteryCache()
	fakeSensor(t, 50, false)
	if first := ReadBattery(true); first.Percent == nil || *first.Percent != 50 {
		t.Fatalf("first reading %+v", first)
	}
	fakeSensor(t, 10, true)
	if cached := ReadBattery(true); cached.Percent == nil || *cached.Percent != 50 {
		t.Fatalf("a cached call returned %+v", cached)
	}
	if fresh := ReadBattery(false); fresh.Percent == nil || *fresh.Percent != 10 {
		t.Fatalf("an uncached call returned %+v", fresh)
	}
}

func TestBatteryCategoryThresholds(t *testing.T) {
	for _, tc := range []struct {
		percent  int
		plugged  bool
		expected string
	}{{100, false, "good"}, {51, false, "good"}, {50, false, "warn"}, {21, false, "warn"}, {20, false, "bad"}, {11, false, "bad"},
		{10, false, "critical"}, {1, false, "critical"}, {5, true, "good"}} {
		status := BatteryStatus{Available: true, Percent: &tc.percent, Plugged: &tc.plugged}
		if got := BatteryCategory(status); got != tc.expected {
			t.Fatalf("%d%% plugged %v gave %q, want %q", tc.percent, tc.plugged, got, tc.expected)
		}
	}
	if BatteryCategory(BatteryStatus{}) != "dim" || FormatBattery(BatteryStatus{}) != "" {
		t.Fatal("an unavailable reading must be dim and unlabelled")
	}
	percent, plugged := 82, true
	if got := FormatBattery(BatteryStatus{Available: true, Percent: &percent, Plugged: &plugged}); got != "⚡ 82%" {
		t.Fatalf("charging label %q", got)
	}
	plugged = false
	if got := FormatBattery(BatteryStatus{Available: true, Percent: &percent, Plugged: &plugged}); got != "🔋 82%" {
		t.Fatalf("discharging label %q", got)
	}
}

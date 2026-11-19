package gateway

import (
	"testing"
)

func TestClassifyPressure(t *testing.T) {
	cases := []struct {
		name         string
		avail, total any
		want         string
	}{
		{"plentiful_memory_is_ok", int64(1024 * 1024), int64(2048 * 1024), "ok"},
		{"low_absolute_available_is_critical", int64(32 * 1024), int64(8 * 1024 * 1024), "critical"},
		{"low_fraction_is_critical", int64(300 * 1024), int64(8 * 1024 * 1024), "critical"},
		{"elevated_band", int64(100 * 1024), int64(1024 * 1024), "elevated"},
		{"missing_sample_is_unknown", nil, nil, "unknown"},
		{"bool_is_not_an_int", true, int64(2048 * 1024), "unknown"},
		{"absolute_floor_without_total_critical", int64(32 * 1024), nil, "critical"},
		{"absolute_floor_without_total_ok", int64(1024 * 1024), nil, "ok"},
	}
	for _, c := range cases {
		if got := ClassifyPressure(c.avail, c.total); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

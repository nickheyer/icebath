package pynumpy

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

type numpyRef struct {
	Linspace map[string][]string `json:"linspace"`
	RMS      []struct {
		Data []int16 `json:"data"`
		RMS  float64 `json:"rms"`
	} `json:"rms"`
}

func loadRef(t *testing.T) numpyRef {
	raw, err := os.ReadFile("testdata/numpy_ref.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref numpyRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestLinspaceMatchesNumpyBitForBit(t *testing.T) {
	for key, want := range loadRef(t).Linspace {
		parts := strings.Split(strings.Trim(key, "()"), ", ")
		start, _ := strconv.ParseFloat(parts[0], 64)
		stop, _ := strconv.ParseFloat(parts[1], 64)
		num, _ := strconv.Atoi(parts[2])
		got, err := Linspace(start, stop, num, parts[3] == "True")
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s: %d values %v", key, len(got), err)
		}
		for i, h := range want {
			w, _ := strconv.ParseFloat(h, 64)
			if math.Float64bits(got[i]) != math.Float64bits(w) {
				t.Fatalf("%s[%d] = %v, numpy %v", key, i, got[i], w)
			}
		}
	}
	if _, err := Linspace(0, 1, -1, true); err == nil || err.Error() != "Number of samples, -1, must be non-negative." {
		t.Fatalf("negative count %v", err)
	}
}

func TestRMSInt16MatchesNumpy(t *testing.T) {
	for _, c := range loadRef(t).RMS {
		if got := RMSInt16(c.Data); got != c.RMS {
			t.Fatalf("rms of %d samples = %v, numpy %v", len(c.Data), got, c.RMS)
		}
	}
}

func TestPercentileMatchesNumpyLinear(t *testing.T) {
	raw, err := os.ReadFile("testdata/percentile_ref.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Values []float64 `json:"values"`
		P90    string    `json:"p90"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		want, _ := strconv.ParseFloat(c.P90, 64)
		if got := Percentile(c.Values, 90); math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("percentile of %d values = %v, numpy %v", len(c.Values), got, want)
		}
	}
}

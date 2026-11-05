package state

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundActivityDescriptionTruncates(t *testing.T) {
	out := BoundActivityDescription(strings.Repeat("x", ActivityDescriptionMax+80))
	if utf8.RuneCountInString(out) != ActivityDescriptionMax || !strings.HasSuffix(out, "…") {
		t.Fatalf("bounded %q", out)
	}
	if got := BoundActivityDescription("  starting API call #1 \n"); got != "starting API call #1" {
		t.Fatalf("stripped %q", got)
	}
}

func TestNormalizeActivityProvenanceDefaultsToUnknown(t *testing.T) {
	for _, raw := range []string{"", "not-a-real-source", "agent.activity"} {
		if got := NormalizeActivityProvenance(raw); got != ActivityProvenanceUnknown {
			t.Fatalf("%q normalized to %q", raw, got)
		}
	}
	if got := NormalizeActivityProvenance(string(ActivityProvenanceAgentCompression)); got != ActivityProvenanceAgentCompression {
		t.Fatalf("compression normalized to %q", got)
	}
	if got := NormalizeActivityProvenance(" agent.compression_timeout "); got != ActivityProvenanceAgentCompressionTimeout {
		t.Fatalf("timeout normalized to %q", got)
	}
}

func TestAwakeIdleMeterSubtractsHostSleep(t *testing.T) {
	wall, mono := 1000.0, 50.0
	m := newAwakeIdleMeter(func() float64 { return wall }, func() float64 { return mono })
	wall, mono = 1010.0, 60.0
	if got := m.Measure(10); got != 10 {
		t.Fatalf("awake idle %v", got)
	}
	wall, mono = 1910.0, 70.0
	if got := m.Measure(910); got != 20 {
		t.Fatalf("idle after sleep %v", got)
	}
	wall, mono = 1915.0, 75.0
	if got := m.Measure(2); got != 2 {
		t.Fatalf("fresh activity keeps old sleep credit %v", got)
	}
	wall, mono = 1920.0, 80.0
	if got := m.Measure(7); got != 7 {
		t.Fatalf("idle after renewed activity %v", got)
	}
}

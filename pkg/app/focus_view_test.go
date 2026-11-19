package app

import "testing"

func TestComposesWithVerboseModesNewModeSkipsConsecutiveRepeatsLikeTheRenderer(t *testing.T) {
	if wouldDisplayToolLine("new", "terminal", "terminal") {
		t.Fatal("repeat counted in new mode")
	}
	if !wouldDisplayToolLine("new", "read_file", "terminal") {
		t.Fatal("new tool not counted in new mode")
	}
	if !wouldDisplayToolLine("all", "terminal", "terminal") {
		t.Fatal("all mode must count repeats")
	}
	if wouldDisplayToolLine("off", "terminal", "") || wouldDisplayToolLine(false, "terminal", "") || wouldDisplayToolLine("all", "", "") {
		t.Fatal("off modes or an empty name counted")
	}
}

func TestHiddenCountFormatterZeroAndNegativeProduceNoLine(t *testing.T) {
	if formatHiddenLine(0) != "" || formatHiddenLine(-3) != "" || formatHiddenLine("x") != "" {
		t.Fatal("no line expected")
	}
	if formatHiddenLine(1) != "⋯ 1 tool line hidden · /focus off to show" || formatHiddenLine("4") != "⋯ 4 tool lines hidden · /focus off to show" {
		t.Fatalf("lines %q %q", formatHiddenLine(1), formatHiddenLine("4"))
	}
}

func TestToolProgressModeNormalization(t *testing.T) {
	if normalizeToolProgressMode(false, "all") != "off" || normalizeToolProgressMode(true, "all") != "all" || normalizeToolProgressMode("LOG", "all") != "log" || normalizeToolProgressMode("bogus", "new") != "new" || normalizeToolProgressMode(nil, "all") != "all" {
		t.Fatal("normalize wrong")
	}
}

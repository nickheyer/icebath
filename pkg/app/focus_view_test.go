package app

import "testing"

func TestToggleStateMachineBareTogglesFromOffToOn(t *testing.T) {
	action, target := resolveFocusArg("", false)
	if action != "set" || target == nil || !*target {
		t.Fatalf("got %s %v", action, target)
	}
	action, target = resolveFocusArg("toggle", true)
	if action != "set" || target == nil || *target {
		t.Fatalf("toggle from on gave %s %v", action, target)
	}
}

func TestToggleStateMachineStatusWordsNeverMutate(t *testing.T) {
	for _, word := range []string{"status", "show", "?", "STATUS"} {
		action, target := resolveFocusArg(word, true)
		if action != "status" || target != nil {
			t.Fatalf("%q gave %s %v", word, action, target)
		}
	}
}

func TestToggleStateMachineGarbageReportsUsage(t *testing.T) {
	for _, word := range []string{"sideways", "onn", "--global", "2"} {
		action, target := resolveFocusArg(word, false)
		if action != "usage" || target != nil {
			t.Fatalf("%q gave %s %v", word, action, target)
		}
	}
	action, target := resolveFocusArg("Off", true)
	if action != "set" || target == nil || *target {
		t.Fatalf("off word gave %s %v", action, target)
	}
}

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

func TestFocusStatusAndToggleMessagesNameTheConfiguredMode(t *testing.T) {
	if focusStatusbarSegment(true) != FocusStatusbarLabel || focusStatusbarSegment(false) != "" {
		t.Fatal("segment wrong")
	}
	if normalizeToolProgressMode(false, "all") != "off" || normalizeToolProgressMode(true, "all") != "all" || normalizeToolProgressMode("LOG", "all") != "log" || normalizeToolProgressMode("bogus", "new") != "new" || normalizeToolProgressMode(nil, "all") != "all" {
		t.Fatal("normalize wrong")
	}
	if formatFocusStatus(true, "verbose") != "Focus view: ON — only your prompt and the final response.\n  /focus off restores tool progress: VERBOSE" {
		t.Fatalf("status %q", formatFocusStatus(true, "verbose"))
	}
	if formatFocusStatus(false, "new") != "Focus view: OFF — tool progress: NEW" || formatFocusToggleMessage(true, "all") != "Focus view enabled — just your prompt and the final response" || formatFocusToggleMessage(false, "bogus") != "Focus view disabled — tool progress: ALL" {
		t.Fatal("messages wrong")
	}
}

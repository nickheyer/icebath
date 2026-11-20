package home

import "testing"

func TestDetachAttrBreaksAwayOnlyWhenAsked(t *testing.T) {
	if DetachAttr(true).CreationFlags != createNewProcessGroup|createNoWindow|createBreakawayFromJob {
		t.Fatal("breakaway flags")
	}
	if DetachAttr(false).CreationFlags != createNewProcessGroup|createNoWindow {
		t.Fatal("plain flags")
	}
}

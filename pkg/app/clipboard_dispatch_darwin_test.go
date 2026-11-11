package app

import "testing"

func TestHasClipboardImageMacosDispatch(t *testing.T) {
	calls := 0
	swapClip(t, &clipMacosHasImage, func() bool { calls++; return true })
	if !hasClipboardImage() || calls != 1 {
		t.Fatalf("calls %d", calls)
	}
}

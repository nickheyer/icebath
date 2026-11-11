package app

import "testing"

func TestHasClipboardImageWslFallsThroughToWaylandWhenWindowsPathEmpty(t *testing.T) {
	wsl, wl := 0, 0
	swapClip(t, &clipIsWSL, func() bool { return true })
	swapClip(t, &clipWSLHasImage, func() bool { wsl++; return false })
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	swapClip(t, &clipWaylandHasImage, func() bool { wl++; return true })
	if !hasClipboardImage() || wsl != 1 || wl != 1 {
		t.Fatalf("wsl %d wayland %d", wsl, wl)
	}
}

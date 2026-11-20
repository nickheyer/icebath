package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Swaps a clipboard dispatch function for the test
func swapClip[T any](t *testing.T, target *T, fn T) {
	saved := *target
	*target = fn
	t.Cleanup(func() { *target = saved })
}

func TestLinuxSaveWslTriedFirst(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	var calls []string
	swapClip(t, &clipIsWSL, func() bool { return true })
	swapClip(t, &clipWSLSave, func(d string) bool { calls = append(calls, d); return true })
	if !linuxSave(dest) || !reflect.DeepEqual(calls, []string{dest}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestLinuxSaveWaylandFailsFallsThroughToXclip(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	var calls []string
	swapClip(t, &clipIsWSL, func() bool { return false })
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	swapClip(t, &clipWaylandSave, func(string) bool { return false })
	swapClip(t, &clipXclipSave, func(d string) bool { calls = append(calls, d); return true })
	if !linuxSave(dest) || !reflect.DeepEqual(calls, []string{dest}) {
		t.Fatalf("calls %v", calls)
	}
}

// Fake PNG bytes the vision cases attach
func clipFakeImage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPreprocessImagesWithVisionSingleImageWithText(t *testing.T) {
	tempHome(t)
	img := clipFakeImage(t)
	swapClip(t, &visionAnalyzeTool, func(context.Context, *tools.Env, string, string) string {
		return `{"success": true, "analysis": "A test image with colored pixels."}`
	})
	c := &CLI{ctx: context.Background()}
	result := c.preprocessImagesWithVision("Describe this", []string{img}, true)
	for _, want := range []string{"A test image with colored pixels.", "Describe this", img} {
		if !strings.Contains(result, want) {
			t.Fatalf("result lacks %q: %s", want, result)
		}
	}
	if strings.Contains(result, "base64,") {
		t.Fatalf("raw image content: %s", result)
	}
}

func TestPreprocessImagesWithVisionVisionExceptionIncludesPath(t *testing.T) {
	tempHome(t)
	img := clipFakeImage(t)
	swapClip(t, &visionAnalyzeTool, func(context.Context, *tools.Env, string, string) string {
		panic(errors.New("API down"))
	})
	c := &CLI{ctx: context.Background()}
	if result := c.preprocessImagesWithVision("check this", []string{img}, true); !strings.Contains(result, img) {
		t.Fatalf("path dropped: %s", result)
	}
}

package gateway

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One transparent pixel PNG written to a fresh temp dir
func writeMediaPNG(t *testing.T) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQABh6FO1AAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(t.TempDir(), "icebath_media_test")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMediaTagInlined(t *testing.T) {
	out := APIResolveMediaToDataURLs(context.Background(), "Here you go: MEDIA:"+writeMediaPNG(t))
	if !strings.Contains(out, "data:image/png;base64,") || strings.Contains(out, "MEDIA:") {
		t.Fatalf("out = %q", out)
	}
}

func TestBacktickWrappedTag(t *testing.T) {
	out := APIResolveMediaToDataURLs(context.Background(), "See `MEDIA:"+writeMediaPNG(t)+"` above")
	if !strings.Contains(out, "data:image/png;base64,") {
		t.Fatalf("out = %q", out)
	}
}

func TestTerminalEOSSentinelDoesNotBlockInlining(t *testing.T) {
	clean := "Here you go: MEDIA:" + writeMediaPNG(t)
	ctx := context.Background()
	if got, want := APIResolveMediaToDataURLs(ctx, clean+"<|eos|>"), APIResolveMediaToDataURLs(ctx, clean); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMissingFileLeftUntouched(t *testing.T) {
	text := "MEDIA:/nonexistent/path/shot.png"
	if got := APIResolveMediaToDataURLs(context.Background(), text); got != text {
		t.Fatalf("got %q", got)
	}
}

func TestNonImageLeftUntouched(t *testing.T) {
	text := "MEDIA:/tmp/archive.zip"
	if got := APIResolveMediaToDataURLs(context.Background(), text); got != text {
		t.Fatalf("got %q", got)
	}
}

package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Profile root with the four media cache directories
func makeMediaProfile(t *testing.T, root string) string {
	t.Helper()
	for _, sub := range []string{"cache/images", "cache/audio", "cache/videos", "cache/documents"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestValidatorFollowsOverrideSwitch(t *testing.T) {
	hermeticHome(t)
	root := t.TempDir()
	profA := makeMediaProfile(t, filepath.Join(root, "profA"))
	profB := makeMediaProfile(t, filepath.Join(root, "profB"))
	aMedia := filepath.Join(profA, "cache", "images", "img_a.jpg")
	bMedia := filepath.Join(profB, "cache", "images", "img_b.jpg")
	for _, p := range []string{aMedia, bMedia} {
		if err := os.WriteFile(p, []byte{0xff, 0xd8, 0xff, 0x00}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := home.WithOverride(context.Background(), profB)
	if !isAllowedBridgePath(ctx, bMedia) {
		t.Fatal("media under the active profile was rejected")
	}
	if isAllowedBridgePath(ctx, aMedia) {
		t.Fatal("media under another profile was accepted")
	}
}

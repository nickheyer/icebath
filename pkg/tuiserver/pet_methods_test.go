package tuiserver

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/pet"
)

func TestPetGalleryQuotedFalseEnabledReportsDisabled(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display:\n  pet:\n    enabled: 'false'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "pet.gallery", "params": map[string]any{}})
	result, _ := resp["result"].(map[string]any)
	if result == nil || result["enabled"] != false {
		t.Fatalf("response %v", resp)
	}
}

func TestPetInfoKnownRevisionElidesSpritesheet(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	directory := filepath.Join(h, "pets", "codex")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	var sheet bytes.Buffer
	if err := png.Encode(&sheet, image.NewNRGBA(image.Rect(0, 0, pet.FrameW, pet.FrameH))); err != nil {
		t.Fatal(err)
	}
	sprite := filepath.Join(directory, "spritesheet.png")
	if err := os.WriteFile(sprite, sheet.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	codex := &pet.InstalledPet{Slug: "codex", DisplayName: "Codex", Directory: directory, Spritesheet: sprite}
	saved := petActiveSelection
	petActiveSelection = func(context.Context) (bool, *pet.InstalledPet, float64, error) { return true, codex, 0.33, nil }
	t.Cleanup(func() { petActiveSelection = saved })
	revision := petSheetRevision(sprite)
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "pet.info", "params": map[string]any{"knownRevision": revision}})
	result, _ := resp["result"].(map[string]any)
	if result["enabled"] != true || result["spritesheetUnchanged"] != true || result["spritesheetRevision"] != revision {
		t.Fatalf("response %v", resp)
	}
	if _, present := result["spritesheetBase64"]; present {
		t.Fatal("the sheet bytes were resent for a known revision")
	}
	resp = handleRequest(context.Background(), map[string]any{"id": "2", "method": "pet.info", "params": map[string]any{"knownRevision": "999:999"}})
	result, _ = resp["result"].(map[string]any)
	if encoded, _ := result["spritesheetBase64"].(string); encoded == "" {
		t.Fatalf("a stale revision got no sheet: %v", resp)
	}
}

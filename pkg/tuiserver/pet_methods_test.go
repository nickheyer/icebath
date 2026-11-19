//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
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

func TestPetSigStaysOffWithoutARenderablePet(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	watchWriteFile(t, filepath.Join(h, "config.yaml"), "display:\n  pet:\n    enabled: true\n    slug: boba\n")
	watchPass(10)
	if got := events.named("pet.changed"); len(got) != 0 {
		t.Fatalf("pet events = %v", got)
	}
}

func TestRenderablePetBroadcastsMetaPayload(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchWriteFile(t, filepath.Join(h, "config.yaml"), "display:\n  pet:\n    enabled: true\n    slug: boba\n")
	watchPass(0)
	sheet := filepath.Join(h, "sheet.png")
	watchWriteFile(t, sheet, "png")
	prev := petActiveSelection
	petActiveSelection = func(context.Context) (bool, *pet.InstalledPet, float64, error) {
		return true, &pet.InstalledPet{Slug: "boba", DisplayName: "Boba", Spritesheet: sheet}, 0.33, nil
	}
	t.Cleanup(func() { petActiveSelection = prev })
	watchPass(10)
	got := events.named("pet.changed")
	if len(got) == 0 {
		t.Fatalf("events = %v", events.all())
	}
	payload := got[0].payload
	if payload["enabled"] != true || payload["slug"] != "boba" || payload["spritesheetRevision"] == "" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestBrokenProbeNeverKillsThePass(t *testing.T) {
	h, events := watcherHomeFixture(t)
	watchPass(0)
	prev := slices.Clone(changeWatches)
	for i := range changeWatches {
		if changeWatches[i].event == "pet.changed" {
			changeWatches[i] = changeWatch{"pet.changed", 2.0, func(context.Context) any { panic(errors.New("boom")) }, emptyChangePayload}
		}
	}
	t.Cleanup(func() { changeWatches = prev })
	writeSessionChange(t, filepath.Join(h, "state.db"), "created")
	watchPass(10)
	if !events.has("sessions.changed") {
		t.Fatalf("events = %v", events.all())
	}
}

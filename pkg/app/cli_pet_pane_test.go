package app

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"

	"github.com/nickheyer/icebath/pkg/pet"
)

// Installs a synthetic eight column nine row pet named boba into the test home and returns its spritesheet
func installBoba(t *testing.T) string {
	t.Helper()
	h := os.Getenv("ICEBATH_HOME")
	root, err := pet.PetsDir(h)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "boba")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sheet := image.NewNRGBA(image.Rect(0, 0, pet.FrameW*8, pet.FrameH*9))
	for r := 0; r < 9; r++ {
		draw.Draw(sheet, image.Rect(0, r*pet.FrameH, pet.FrameW*8, (r+1)*pet.FrameH), &image.Uniform{C: color.NRGBA{uint8(20 + r*25), 60, 120, 255}}, image.Point{}, draw.Src)
	}
	path := filepath.Join(dir, "spritesheet.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, sheet); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pet.json"), []byte(`{"id":"boba","displayName":"Boba","description":"d","spritesheetPath":"spritesheet.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func petCLI(t *testing.T) *CLI {
	t.Helper()
	recordPrints(t)
	c := bareCLI()
	c.petCols, c.petScale = 18, 0.7
	c.petFramesCache, c.petKittyCache = map[string][][][]pet.Cell{}, map[string]*petKittyEntry{}
	return c
}

func writePetConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPetStateTracksAgentRunning(t *testing.T) {
	c := petCLI(t)
	if got := c.derivePetState(); got != "idle" {
		t.Fatalf("idle CLI gave %q", got)
	}
	c.agentRunning = true
	if got := c.derivePetState(); got != "run" {
		t.Fatalf("running CLI gave %q", got)
	}
}

func TestPetStateWaitsOnABlockingModal(t *testing.T) {
	c := petCLI(t)
	c.agentRunning = true
	c.clarifyState = &clarifyModalState{}
	if got := c.derivePetState(); got != "waiting" {
		t.Fatalf("a blocking modal gave %q", got)
	}
}

func goalCLI(t *testing.T, status string, turnsUsed, maxTurns int64) *CLI {
	t.Helper()
	c := statusBarCLI(t, nil)
	c.sessionID = "sess-goal-test"
	st := goals.NewGoalState("ship it")
	st.Status, st.TurnsUsed, st.MaxTurns = status, turnsUsed, maxTurns
	goals.SaveGoal(c.ctx, c.sessionID, st)
	return c
}

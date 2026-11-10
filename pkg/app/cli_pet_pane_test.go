package app

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
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

func TestPetPaneCollapsedWhenDisabled(t *testing.T) {
	c := petCLI(t)
	if c.petWidgetHeight() != 0 || len(c.petFragments()) != 0 {
		t.Fatal("a disabled pet must take no rows and draw nothing")
	}
}

func TestPetFragmentsRenderHalfBlocks(t *testing.T) {
	c := petCLI(t)
	c.petRenderer = pet.NewRenderer(installBoba(t), "unicode", 0.4, 14)
	c.petCols, c.petEnabled = 14, true
	height := c.petWidgetHeight()
	frags := c.petFragments()
	if height <= 0 || len(frags) == 0 {
		t.Fatalf("height %d fragments %d", height, len(frags))
	}
	newlines, coloredUpper := 0, false
	for _, f := range frags {
		if !strings.Contains("▀▄ \n", f.Text) || f.Text == "" {
			t.Fatalf("unexpected glyph %q", f.Text)
		}
		if f.Text == "\n" {
			newlines++
		}
		coloredUpper = coloredUpper || (f.Text == "▀" && strings.Contains(f.Style, "fg:#"))
	}
	if !coloredUpper || newlines != height-1 {
		t.Fatalf("upper half blocks %v, %d newlines for %d rows", coloredUpper, newlines, height)
	}
}

func TestPetResolveConfigEnablesAndDisables(t *testing.T) {
	c := petCLI(t)
	installBoba(t)
	writePetConfig(t, "display:\n  pet:\n    enabled: true\n    slug: boba\n")
	c.petResolveConfig()
	if !c.petEnabled || c.petRenderer == nil || c.petSlug != "boba" {
		t.Fatalf("enabled %v renderer %v slug %q", c.petEnabled, c.petRenderer, c.petSlug)
	}
	writePetConfig(t, "display:\n  pet:\n    enabled: false\n    slug: boba\n")
	c.petResolveConfig()
	if c.petEnabled || c.petRenderer != nil {
		t.Fatal("a disabled pet must drop its renderer")
	}
}

func TestPetFragmentsRenderKittyPlaceholders(t *testing.T) {
	c := petCLI(t)
	c.petRenderer = pet.NewRenderer(installBoba(t), "kitty", 0.4, 20)
	c.petSlug, c.petKittyImageID, c.petEnabled = "boba", pet.KittyImageID("boba"), true
	frags := c.petFragments()
	entry := c.petKittyPayloadFor("idle")
	if len(frags) == 0 || entry == nil {
		t.Fatalf("fragments %d entry %v", len(frags), entry)
	}
	fg, sawCell := "fg:"+pet.KittyColorHex(entry.imageID), false
	for _, f := range frags {
		sawCell = sawCell || strings.Contains(f.Text, "\U0010eeee")
		if f.Text != "\n" && !strings.Contains(f.Style, fg) {
			t.Fatalf("fragment style %q lacks %q", f.Style, fg)
		}
	}
	if !sawCell || c.petWidgetHeight() <= 0 {
		t.Fatal("the kitty pane must draw its image cells and take rows")
	}
	c.petQueueKittyFrame()
	if !strings.HasPrefix(c.petKittyPending, "\x1b_G") {
		t.Fatalf("pending frame %q", c.petKittyPending)
	}
	app := &fakeTUIApp{}
	c.app = app
	c.petFlushKittyFrame()
	if len(app.raw) == 0 || !strings.HasPrefix(app.raw[0], "\x1b_G") || c.petKittyPending != "" {
		t.Fatalf("flushed %d writes, pending %q", len(app.raw), c.petKittyPending)
	}
}

func TestPetOffClearsPendingKittyFrame(t *testing.T) {
	c := petCLI(t)
	installBoba(t)
	c.petKittyPending = "stale-apc"
	writePetConfig(t, "display:\n  pet:\n    enabled: true\n    slug: boba\n    render_mode: 'off'\n")
	c.petResolveConfig()
	if c.petEnabled || c.petRenderer != nil || c.petKittyPending != "" {
		t.Fatalf("enabled %v renderer %v pending %q", c.petEnabled, c.petRenderer, c.petKittyPending)
	}
}

func TestPetResolveWeztermStaysUnicode(t *testing.T) {
	c := petCLI(t)
	installBoba(t)
	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("WEZTERM_PANE", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_PROGRAM", "WezTerm")
	writePetConfig(t, "display:\n  pet:\n    enabled: true\n    slug: boba\n    render_mode: auto\n")
	c.petResolveConfig()
	if c.petRenderer == nil || c.petRenderer.Mode != "unicode" {
		t.Fatalf("renderer %+v", c.petRenderer)
	}
}

func TestPetResolveGhosttyUsesKitty(t *testing.T) {
	c := petCLI(t)
	installBoba(t)
	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("WEZTERM_PANE", "")
	t.Setenv("TERM", "xterm-ghostty")
	t.Setenv("TERM_PROGRAM", "ghostty")
	writePetConfig(t, "display:\n  pet:\n    enabled: true\n    slug: boba\n    render_mode: auto\n")
	c.petResolveConfig()
	if c.petRenderer == nil || c.petRenderer.Mode != "kitty" {
		t.Fatalf("renderer %+v", c.petRenderer)
	}
}

func goalCLI(t *testing.T, status string, turnsUsed, maxTurns int64) *CLI {
	t.Helper()
	c := statusBarCLI(t, nil)
	c.sessionID = "sess-goal-test"
	st := agent.NewGoalState("ship it")
	st.Status, st.TurnsUsed, st.MaxTurns = status, turnsUsed, maxTurns
	agent.SaveGoal(c.ctx, c.sessionID, st)
	return c
}

func TestStatusBarGoalSegment(t *testing.T) {
	t.Run("test_goal_segment_composition", func(t *testing.T) {
		c := goalCLI(t, "active", 3, 20)
		snapshot := c.getStatusBarSnapshot()
		if snapshot["goal_active"] != true || snapshot["goal_turns_used"] != 3 || snapshot["goal_max_turns"] != 20 {
			t.Fatalf("snapshot %v %v %v", snapshot["goal_active"], snapshot["goal_turns_used"], snapshot["goal_max_turns"])
		}
		if got := c.statusBarGoalSegment(snapshot); got != "⊙ goal 3/20" {
			t.Fatalf("segment %q", got)
		}
	})
	t.Run("test_goal_segment_absent_when_paused", func(t *testing.T) {
		c := goalCLI(t, "paused", 3, 20)
		snapshot := c.getStatusBarSnapshot()
		if snapshot["goal_active"] != false || c.statusBarGoalSegment(snapshot) != "" {
			t.Fatalf("a paused goal took the bar: %v", snapshot["goal_active"])
		}
	})
	t.Run("test_active_goal_rendered_in_wide_status_bar", func(t *testing.T) {
		c := goalCLI(t, "active", 5, 20)
		if text := barText(c, 120); !strings.Contains(text, "⊙ goal 5/20") {
			t.Fatalf("text %q", text)
		}
	})
}

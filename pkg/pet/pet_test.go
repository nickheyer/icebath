package pet

import (
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const twoColumnSheetWebP = "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"

func TestDerivePriorityOrder(t *testing.T) {
	cases := []struct {
		signals Signals
		want    State
	}{
		{Signals{Error: true, Celebrate: true, Busy: true}, StateFailed},
		{Signals{Celebrate: true, JustCompleted: true, ToolRunning: true}, StateJump},
		{Signals{JustCompleted: true, AwaitingInput: true}, StateWave},
		{Signals{AwaitingInput: true, ToolRunning: true, Busy: true}, StateWaiting},
		{Signals{ToolRunning: true, Reasoning: true}, StateRun},
		{Signals{Reasoning: true, Busy: true}, StateReview},
		{Signals{Busy: true}, StateRun},
		{Signals{}, StateIdle},
	}
	for _, tc := range cases {
		if got := DeriveState(tc.signals); got != tc.want {
			t.Fatalf("%+v gave %q, want %q", tc.signals, got, tc.want)
		}
	}
}

func TestTodosAllDone(t *testing.T) {
	if TodosAllDone(nil) || TodosAllDone([]string{}) {
		t.Fatal("an empty task list is not done")
	}
	if TodosAllDone([]string{"completed", "pending"}) || TodosAllDone([]string{"in_progress"}) {
		t.Fatal("an open item is not done")
	}
	if !TodosAllDone([]string{"completed", "cancelled"}) || !TodosAllDone([]string{"completed"}) {
		t.Fatal("every item terminal is done")
	}
}

func TestStateRowIndexMapsToSupportedAtlasTaxonomies(t *testing.T) {
	codex := map[State]int{StateIdle: 0, StateWave: 3, StateJump: 4, StateFailed: 5, StateWaiting: 6, StateRun: 7, StateReview: 8}
	for state, want := range codex {
		if got := StateRowIndex(string(state), 9); got != want {
			t.Fatalf("%s on a nine row sheet gave row %d, want %d", state, got, want)
		}
	}
	legacy := map[State]int{StateWave: 1, StateRun: 2, StateFailed: 3, StateReview: 4, StateJump: 5, StateWaiting: 0}
	for state, want := range legacy {
		if got := StateRowIndex(string(state), 8); got != want {
			t.Fatalf("%s on an eight row sheet gave row %d, want %d", state, got, want)
		}
	}
	for canonical, alias := range map[string]string{"wave": "waving", "jump": "jumping", "run": "running"} {
		if StateRowIndex(canonical, 9) != StateRowIndex(alias, 9) {
			t.Fatalf("%s and %s resolve to different rows", canonical, alias)
		}
	}
	if StateRowIndex("nonsense", 0) != 0 {
		t.Fatal("an unknown row name must clamp to the idle row")
	}
}

// Writes a spritesheet whose rows hold the given number of opaque frames, the rest transparent
func writeSheet(t *testing.T, path string, cols, rows int, real map[int]int, paint func(row int) color.NRGBA) {
	t.Helper()
	sheet := image.NewNRGBA(image.Rect(0, 0, FrameW*cols, FrameH*rows))
	for r := 0; r < rows; r++ {
		count, ok := real[r]
		if real == nil {
			count, ok = cols, true
		}
		if !ok {
			continue
		}
		for c := 0; c < count; c++ {
			draw.Draw(sheet, image.Rect(c*FrameW, r*FrameH, (c+1)*FrameW, (r+1)*FrameH), &image.Uniform{C: paint(r)}, image.Point{}, draw.Src)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, sheet); err != nil {
		t.Fatal(err)
	}
}

// Installs a synthetic eight column nine row pet named boba into a fresh home
func bobaLike(t *testing.T) (string, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	root, err := PetsDir(h)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "boba")
	writeSheet(t, filepath.Join(dir, "spritesheet.png"), 8, 9, nil, func(r int) color.NRGBA { return color.NRGBA{uint8(20 + r*25), 60, 120, 255} })
	if err := os.WriteFile(filepath.Join(dir, "pet.json"), []byte(`{"id":"boba","displayName":"Boba","description":"d","spritesheetPath":"spritesheet.png"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return h, dir
}

func TestTrimsTrailingBlankFrames(t *testing.T) {
	sprite := filepath.Join(t.TempDir(), "ragged.png")
	writeSheet(t, sprite, 8, 9, map[int]int{0: 6, 3: 4, 4: 5, 5: 8, 7: 6, 8: 5}, func(int) color.NRGBA { return color.NRGBA{200, 80, 80, 255} })
	r := NewRenderer(sprite, "unicode", 0.5, 20)
	for state, want := range map[string]int{"idle": FramesPerState, "run": FramesPerState, "wave": 4, "jump": 5, "failed": FramesPerState, "review": 5} {
		if got := r.FrameCount(state); got != want {
			t.Fatalf("%s has %d frames, want %d", state, got, want)
		}
	}
	for _, state := range []string{"wave", "jump", "review"} {
		for i := 0; i < r.FrameCount(state); i++ {
			if r.Frame(state, i) == "" {
				t.Fatalf("%s[%d] rendered blank", state, i)
			}
		}
	}
	counts := StateFrameCounts(sprite)
	want := map[string]int{"idle": 6, "wave": 4, "run": 6, "failed": 6, "review": 5, "jump": 5, "waiting": 0}
	if len(counts) != len(want) {
		t.Fatalf("counts %v", counts)
	}
	for state, n := range want {
		if counts[state] != n {
			t.Fatalf("counts %v, want %v", counts, want)
		}
	}
}

func TestCellsGridShape(t *testing.T) {
	h, _ := bobaLike(t)
	boba, err := LoadPet(h, "boba")
	if err != nil || boba == nil || boba.DisplayName != "Boba" || !boba.Exists() {
		t.Fatalf("load %+v %v", boba, err)
	}
	r := NewRenderer(boba.Spritesheet, "unicode", 0.4, 14)
	grid := r.Cells("run", 0, 14)
	if len(grid) == 0 {
		t.Fatal("no cells produced")
	}
	for _, row := range grid {
		if len(row) != 14 {
			t.Fatalf("row width %d", len(row))
		}
	}
	if top := grid[0][0][0]; top != [4]uint8{195, 60, 120, 255} {
		t.Fatalf("top pixel of the run row %v", top)
	}
	if got := NewRenderer(filepath.Join(filepath.Dir(boba.Spritesheet), "missing.webp"), "unicode", DefaultScale, 20).Cells("idle", 0, 0); len(got) != 0 {
		t.Fatalf("a missing sheet yielded cells %v", got)
	}
}

func TestKittyPayloadStructure(t *testing.T) {
	h, _ := bobaLike(t)
	boba, _ := LoadPet(h, "boba")
	imageID := KittyImageID("boba")
	r := NewRenderer(boba.Spritesheet, "kitty", 0.4, 18)
	payload := r.KittyPayload("run", imageID)
	if payload == nil {
		t.Fatal("no payload")
	}
	if payload.Cols < 1 || payload.Rows < 1 || payload.Cols >= 18 || len(payload.Placeholder) != payload.Rows || len(payload.Frames) != r.FrameCount("run") {
		t.Fatalf("payload geometry cols %d rows %d grid rows %d frames %d", payload.Cols, payload.Rows, len(payload.Placeholder), len(payload.Frames))
	}
	for _, esc := range payload.Frames {
		for _, want := range []string{"i=" + strconv.Itoa(imageID), "a=T", "U=1", "c=" + strconv.Itoa(payload.Cols), "r=" + strconv.Itoa(payload.Rows)} {
			if !strings.Contains(esc, want) {
				t.Fatalf("escape lacks %q", want)
			}
		}
		if !strings.HasPrefix(esc, "\x1b_G") || !strings.HasSuffix(esc, "\x1b\\") {
			t.Fatalf("escape framing %q", esc[:8])
		}
	}
	if KittyImageID("boba") < 1 || KittyImageID("boba") > 0x7FFF || KittyColorHex(0x1a2b) != "#001a2b" {
		t.Fatalf("image id %d colour %s", KittyImageID("boba"), KittyColorHex(0x1a2b))
	}
}

func clearGraphicsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"KITTY_WINDOW_ID", "TERM_PROGRAM", "ITERM_SESSION_ID", "WEZTERM_PANE", "TERM"} {
		t.Setenv(key, "")
	}
}

func TestVscodeTerminalIgnoresLeakedGraphicsEnv(t *testing.T) {
	clearGraphicsEnv(t)
	t.Setenv("TERM_PROGRAM", "vscode")
	if DetectTerminalGraphics() != "unicode" {
		t.Fatal("vscode must render half blocks")
	}
	for _, leaked := range []string{"ITERM_SESSION_ID", "KITTY_WINDOW_ID", "WEZTERM_PANE"} {
		t.Setenv(leaked, "1")
		if DetectTerminalGraphics() != "unicode" {
			t.Fatalf("a leaked %s won over vscode", leaked)
		}
		t.Setenv(leaked, "")
		if SupportsKittyPlaceholders() {
			t.Fatal("vscode cannot paint the image cells")
		}
	}
}

func TestKittyPlaceholdersOnKittyAndGhostty(t *testing.T) {
	clearGraphicsEnv(t)
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("KITTY_WINDOW_ID", "1")
	if !SupportsKittyPlaceholders() {
		t.Fatal("kitty paints the image cells")
	}
	clearGraphicsEnv(t)
	t.Setenv("TERM", "xterm-ghostty")
	t.Setenv("TERM_PROGRAM", "ghostty")
	if !SupportsKittyPlaceholders() {
		t.Fatal("ghostty paints the image cells")
	}
}

func TestWeztermIsNotPlaceholderCapable(t *testing.T) {
	clearGraphicsEnv(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_PROGRAM", "WezTerm")
	if DetectTerminalGraphics() != "kitty" || SupportsKittyPlaceholders() {
		t.Fatalf("wezterm graphics %q image cells %v", DetectTerminalGraphics(), SupportsKittyPlaceholders())
	}
	t.Setenv("WEZTERM_PANE", "1")
	if SupportsKittyPlaceholders() {
		t.Fatal("wezterm cannot paint the image cells")
	}
}

func TestWebPSpritesheetDecodesAndResolvesAsTheActivePet(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	root, err := PetsDir(h)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(twoColumnSheetWebP)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "zed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spritesheet.webp"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	active, err := ResolveActivePet(h, "missing")
	if err != nil || active == nil || active.Slug != "zed" || active.DisplayName != "zed" {
		t.Fatalf("active %+v %v", active, err)
	}
	if got := NewRenderer(active.Spritesheet, "unicode", DefaultScale, 20).FrameCount("idle"); got != 1 {
		t.Fatalf("the one opaque column gave %d frames", got)
	}
	if escaped, _ := LoadPet(h, "../zed"); escaped == nil || escaped.Slug != "zed" {
		t.Fatalf("a path shaped slug must reduce to its last segment: %+v", escaped)
	}
	if none, _ := LoadPet(h, ".."); none != nil {
		t.Fatal("a dot dot slug must not load")
	}
	if ColsForScale(0) != 16 || ColsForScale(1) != 24 || ResolveCols(1, 30) != 30 || ClampScale(9) != MaxScale {
		t.Fatalf("scale geometry %d %d %d %v", ColsForScale(0), ColsForScale(1), ResolveCols(1, 30), ClampScale(9))
	}
}

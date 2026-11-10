package termlab

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Environment variable naming the directory failing tests save screenshots and streams into
const artifactsEnv = "TERMLAB_ARTIFACTS"

// Longest the TUI may take to answer a key on screen
const answerLimit = 300 * time.Millisecond

// Screens of the konsole and the xterm labs, xterm needing 1604 pixels across for 160 cells at Monospace 10
var (
	konsoleScreen = image.Pt(1600, 1000)
	xtermScreen   = image.Pt(1680, 1000)
)

// Runs the helper modes of the binary, builds icebath once when the lab tools exist, then the tests
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	RunHelper()
	if Missing("Xvfb", "import") == nil {
		Icebath()
	}
	code := m.Run()
	RemoveBuild()
	os.Exit(code)
}

// Session of one emulator test with the directory its evidence goes to
type rig struct {
	t   *testing.T
	s   *Session
	dir string
	n   int
}

// Starts a lab with the emulator running icebath under a config holding the settings, skipping when a tool is missing
func newRig(t *testing.T, emu Emulator, screen image.Point, settings map[string]string) *rig {
	t.Helper()
	return newRigWithEnv(t, emu, screen, settings)
}

// Rig whose settings map may also carry environment variables for the program, named in upper case
func newRigWithEnv(t *testing.T, emu Emulator, screen image.Point, settings map[string]string) *rig {
	t.Helper()
	if err := Missing("Xvfb", "import", emu.Name); err != nil {
		t.Skipf("termlab needs Xvfb, import and %s: %v", emu.Name, err)
	}
	bin, err := Icebath()
	if err != nil {
		t.Fatal(err)
	}
	cfg := t.TempDir()
	env := []string{"ICEBATH_CONFIG_DIR=" + cfg}
	plain := map[string]string{}
	for k, v := range settings {
		if strings.HasPrefix(k, "ICEBATH_") {
			env = append(env, k+"="+v)
			continue
		}
		plain[k] = v
	}
	if err := WriteConfig(cfg, plain); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	dir := os.Getenv(artifactsEnv)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "termlab")
	}
	lab, err := Start(screen.X, screen.Y)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lab.Close(); err != nil {
			t.Error(err)
		}
	})
	s, err := lab.Open(emu, Program{Path: bin, Env: env, Dir: proj})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, s: s, dir: dir}
	t.Cleanup(func() {
		if t.Failed() {
			r.dump()
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	cols, rows := s.Size()
	pw, ph := s.Pitch()
	t.Logf("%s on %s, %dx%d cells of %.0fx%.0f pixels, icebath %s", emu.Name, lab.Name(), cols, rows, pw, ph, bin)
	return r
}

// Path of an artifact of the running test, creating the artifact directory on first use
func (r *rig) path(name string) string {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		r.t.Errorf("artifact directory %s: %v", r.dir, err)
	}
	return filepath.Join(r.dir, strings.ReplaceAll(r.t.Name(), "/", "_")+"-"+name)
}

// Saves the screenshots an assertion looked at, returning their paths
func (r *rig) save(name string, shots ...image.Image) []string {
	var out []string
	for i, img := range shots {
		if img == nil {
			continue
		}
		r.n++
		p := r.path(fmt.Sprintf("%02d-%s-%d.png", r.n, name, i))
		if err := SavePNG(p, img); err != nil {
			r.t.Errorf("save %s: %v", p, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// Fails the test with the message when the assertion does not hold, saving the screenshots it looked at
func (r *rig) check(ok bool, name string, shots []image.Image, format string, args ...any) bool {
	r.t.Helper()
	if ok {
		return true
	}
	msg := name + ": " + fmt.Sprintf(format, args...)
	if paths := r.save(name, shots...); len(paths) > 0 {
		msg += ", screenshots " + strings.Join(paths, " ")
	}
	r.t.Error(msg)
	return false
}

// Fails the test now when the assertion does not hold, saving the screenshots it looked at
func (r *rig) must(ok bool, name string, shots []image.Image, format string, args ...any) {
	r.t.Helper()
	if !r.check(ok, name, shots, format, args...) {
		r.t.FailNow()
	}
}

// Saves the last screen, the mirrored text and both byte streams of a failed test
func (r *rig) dump() {
	if img, err := r.s.Shot(); err == nil {
		if p := r.save("final", img); len(p) > 0 {
			r.t.Logf("final screenshot %s", p[0])
		}
	}
	var text bytes.Buffer
	for _, row := range r.s.Screen() {
		text.WriteString(Line(row) + "\n")
	}
	var in []byte
	for _, c := range r.s.Input() {
		in = append(in, c.Data...)
	}
	for name, data := range map[string][]byte{"screen.txt": text.Bytes(), "output.bin": r.s.Output(), "input.bin": in} {
		p := r.path(name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			r.t.Errorf("save %s: %v", p, err)
			continue
		}
		r.t.Logf("saved %s", p)
	}
	if why := r.s.Ended(); why != "" {
		r.t.Logf("session ended, %s", why)
	}
}

// Screenshot that fails the test when it cannot be taken
func (r *rig) shot() *image.RGBA {
	r.t.Helper()
	img, err := r.s.Shot()
	if err != nil {
		r.t.Fatal(err)
	}
	return img
}

// Waits for the top bar with the palette hint the app shows once it is up
func (r *rig) waitBar() {
	r.t.Helper()
	err := r.s.WaitFor(func([]byte) bool {
		_, _, ok := r.s.Find("palette (alt+space)")
		return ok
	}, 20*time.Second)
	r.must(err == nil, "bar", []image.Image{r.shot()}, "the top bar with the palette hint never showed, %v", err)
}

// Opens the arena with the slash command and returns its frame once the mirror shows it settled
func (r *rig) openArena() image.Rectangle {
	r.t.Helper()
	if _, err := r.s.Type("/arena"); err != nil {
		r.t.Fatal(err)
	}
	err := r.s.WaitFor(func([]byte) bool {
		_, _, ok := r.s.Find("/arena")
		return ok
	}, 5*time.Second)
	r.must(err == nil, "typed", []image.Image{r.shot()}, "the composer never showed the typed /arena, %v", err)
	if _, err := r.s.Key([]byte("\r")); err != nil {
		r.t.Fatal(err)
	}
	err = r.s.WaitFor(func([]byte) bool {
		_, _, ok := r.s.Find("ARENA")
		return ok
	}, 15*time.Second)
	r.must(err == nil, "arena", []image.Image{r.shot()}, "the output never drew the arena title ARENA, %v", err)
	var frame, last image.Rectangle
	err = r.s.WaitFor(func([]byte) bool {
		time.Sleep(250 * time.Millisecond)
		f, ok := findCard(r.s.Screen(), "ARENA")
		frame, last = last, f
		return ok && f == frame
	}, 10*time.Second)
	r.must(err == nil, "frame", []image.Image{r.shot()}, "no settled frame titled ARENA on the mirrored screen, last %v, %v", last, err)
	return last
}

// Frame of the card whose top border carries the title, in cells with the corners inside
func findCard(screen [][]string, title string) (image.Rectangle, bool) {
	want := []rune(title)
	for y, row := range screen {
		for x := range row {
			match := x+len(want) <= len(row)
			for k := 0; match && k < len(want); k++ {
				match = row[x+k] == string(want[k])
			}
			if !match {
				continue
			}
			l, r := -1, -1
			for i := x - 1; i >= 0 && l < 0; i-- {
				if oneOf(row[i], "┌", "╭", "┏", "╔") {
					l = i
				}
			}
			for i := x + len(want); i < len(row) && r < 0; i++ {
				if oneOf(row[i], "┐", "╮", "┓", "╗") {
					r = i
				}
			}
			if l < 0 || r < 0 {
				continue
			}
			for b := y + 1; b < len(screen); b++ {
				if oneOf(screen[b][l], "└", "╰", "┗", "╚") {
					return image.Rect(l, y, r+1, b+1), true
				}
			}
		}
	}
	return image.Rectangle{}, false
}

// Reports whether the cell text is one of the given ones
func oneOf(c string, set ...string) bool {
	for _, s := range set {
		if c == s {
			return true
		}
	}
	return false
}

// Box the card paints into, the frame less its border and the two by one padding the window manager keeps
func content(frame image.Rectangle) image.Rectangle {
	return image.Rect(frame.Min.X+3, frame.Min.Y+2, frame.Max.X-3, frame.Max.Y-2)
}

// World inside the card under the toolbar row, skipping the left half where a dock shows from 90 columns
func worldBox(frame image.Rectangle) image.Rectangle {
	c := content(frame)
	box := image.Rect(c.Min.X, c.Min.Y+1, c.Max.X, c.Max.Y)
	if c.Dx() >= 90 {
		box.Min.X += c.Dx() / 2
	}
	return box
}

// Reports whether a cell holds visible ASCII text
func textual(c uv.Cell) bool { return len(c.Content) == 1 && c.Content[0] > ' ' && c.Content[0] <= '~' }

// Cells of the box with no text in or beside them, for images only blanks in the commonest blank style
func worldMask(grid [][]uv.Cell, box image.Rectangle, images bool) []image.Point {
	blank := func(c uv.Cell) bool { return strings.TrimSpace(c.Content) == "" }
	var styles []uv.Style
	var counts []int
	for y := box.Min.Y; y < box.Max.Y && images; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			c := grid[y][x]
			if !blank(c) {
				continue
			}
			k := slices.IndexFunc(styles, func(s uv.Style) bool { return s.Equal(&c.Style) })
			if k < 0 {
				styles, counts, k = append(styles, c.Style), append(counts, 0), len(styles)
			}
			counts[k]++
		}
	}
	var plain *uv.Style
	if len(counts) > 0 {
		plain = &styles[slices.Index(counts, slices.Max(counts))]
	}
	var out []image.Point
	for y := box.Min.Y; y < box.Max.Y; y++ {
		row := grid[y]
		for x := box.Min.X; x < box.Max.X; x++ {
			if textual(row[x]) || x > 0 && textual(row[x-1]) || x+1 < len(row) && textual(row[x+1]) {
				continue
			}
			if images && (plain == nil || !blank(row[x]) || !row[x].Style.Equal(plain)) {
				continue
			}
			out = append(out, image.Pt(x, y))
		}
	}
	return out
}

// Waits for the world of the card to show a drawn scene, returning the screenshot judged
func (r *rig) waitWorld(frame image.Rectangle, images bool, timeout time.Duration) (*image.RGBA, bool) {
	r.t.Helper()
	box := worldBox(frame)
	deadline := time.Now().Add(timeout)
	for {
		grid := r.s.Grid()
		img := r.shot()
		mask := worldMask(grid, box, images)
		rects := make([]image.Rectangle, len(mask))
		for i, c := range mask {
			rects[i] = r.s.Cells(c.X, c.Y, 1, 1)
		}
		colors, off := scene(Sample(img, rects))
		enough := 2*len(mask) >= box.Dx()*box.Dy()
		ok := enough && colors >= 16 && off >= 0.05
		if ok || time.Now().After(deadline) {
			r.t.Logf("world box %v, %d of %d cells judged, %d colors, %.1f%% off the dominant color", box, len(mask), box.Dx()*box.Dy(), colors, 100*off)
			return img, ok
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Distinct colors among the pixels, capped at 256, and the share of them off the most frequent color
func scene(px []color.RGBA) (int, float64) {
	if len(px) == 0 {
		return 0, 0
	}
	count := map[color.RGBA]int{}
	top := 0
	for _, c := range px {
		count[c]++
		top = max(top, count[c])
	}
	return min(len(count), 256), 1 - float64(top)/float64(len(px))
}

// One screenshot with the times its capture started and finished
type timedShot struct {
	img        *image.RGBA
	start, end time.Time
}

// One d keystroke with the screen before it, its output latency and the screenshots of the second after
type press struct {
	name    string
	at      time.Time
	settled time.Time
	lat     time.Duration
	latErr  error
	before  *image.RGBA
	shots   []timedShot
}

// Presses d and records the screen for a second after
func (r *rig) press(name string) press {
	r.t.Helper()
	p := press{name: name, before: r.shot()}
	at, err := r.s.Type("d")
	if err != nil {
		r.t.Fatal(err)
	}
	p.at = at
	p.lat, p.latErr = r.s.Latency(answerLimit)
	for len(p.shots) == 0 || time.Since(at) < time.Second {
		start := time.Now()
		img := r.shot()
		p.shots = append(p.shots, timedShot{img, start, time.Now()})
	}
	p.settled = time.Now()
	return p
}

// Presses d in pairs, asserting pty output, the mirrored cells it toggles and the screen all answer within the limit
func (r *rig) pressDock(frame image.Rectangle, pairs int, during func(at, until time.Time)) {
	r.t.Helper()
	w := r.s.Watch(frame)
	defer w.Stop()
	idleFrom := time.Now()
	time.Sleep(2 * time.Second)
	animated := w.Moved(idleFrom, time.Now())
	r.t.Logf("%d of %d card cells changed in 2s without input", len(animated), frame.Dx()*frame.Dy())
	for pair := range pairs {
		on := r.press(fmt.Sprintf("press%d", 2*pair+1))
		off := r.press(fmt.Sprintf("press%d", 2*pair+2))
		start, shown, back := w.At(on.at), w.At(on.settled), w.At(off.settled)
		var toggled []int
		for i := range start {
			if !animated[i] && !same(&start[i], &shown[i]) && same(&start[i], &back[i]) {
				toggled = append(toggled, i)
			}
		}
		if !r.check(len(toggled) > 0, on.name+"-effect", []image.Image{on.before, off.before, off.shots[len(off.shots)-1].img}, "d toggled no cell of the card outside the animated world") {
			continue
		}
		cells := make([]image.Rectangle, len(toggled))
		for k, i := range toggled {
			c := w.Cell(i)
			cells[k] = r.s.Cells(c.X, c.Y, 1, 1)
		}
		for _, p := range []press{on, off} {
			r.judge(w, p, toggled, cells)
			if during != nil {
				during(p.at, p.settled)
			}
		}
	}
}

// Asserts output, half the toggled mirror cells and half of them on screen answered one keystroke within the limit
func (r *rig) judge(w *Watch, p press, toggled []int, cells []image.Rectangle) {
	r.t.Helper()
	final := p.shots[len(p.shots)-1].img
	r.check(p.latErr == nil && p.lat <= answerLimit, p.name+"-output", []image.Image{p.before, final}, "no pty output within %s of d, latency %s, %v", answerLimit, p.lat, p.latErr)
	ans, ok := w.Answer(p.at, toggled, 0.5)
	if r.check(ok, p.name+"-mirror", []image.Image{p.before, final}, "half of the %d cells d toggles never changed on the mirror", len(toggled)) {
		r.check(ans.Sub(p.at) <= answerLimit, p.name+"-mirror", []image.Image{p.before, final}, "half of the %d cells d toggles took %s to change on the mirror", len(toggled), ans.Sub(p.at))
	}
	share := func(img image.Image) float64 {
		n := 0
		for _, c := range cells {
			if Differs(p.before, img, c) > 0 {
				n++
			}
		}
		return float64(n) / float64(len(cells))
	}
	r.check(share(final) >= 0.5, p.name+"-shot", []image.Image{p.before, final}, "the emulator redrew %.0f%% of the %d cells d toggles", 100*share(final), len(cells))
	shots := []image.Image{p.before}
	var seen time.Time
	lacked := "no earlier capture"
	for _, sh := range p.shots {
		if share(sh.img) >= 0.5 {
			seen = sh.end
			shots = append(shots, sh.img)
			break
		}
		lacked = fmt.Sprintf("a capture started %s after it", sh.start.Sub(p.at))
		shots = append(shots[:1], sh.img)
	}
	if r.check(!seen.IsZero(), p.name+"-screen", append(shots, final), "the screen never showed the answer to d in the second after the press") {
		r.check(seen.Sub(p.at) <= answerLimit, p.name+"-screen", shots, "the screen showed the answer to d by %s after the press, %s still lacked it", seen.Sub(p.at), lacked)
	}
	if ok && !seen.IsZero() {
		r.t.Logf("%s: output after %s, mirror answer after %s, on screen by %s, %d toggled cells", p.name, p.lat, ans.Sub(p.at), seen.Sub(p.at), len(toggled))
	}
}

// Kitty graphics command in the output with its control keys and the read time of its first chunk
type graphic struct {
	at   time.Time
	keys map[string]string
}

// Every kitty command the session output holds, each with its control keys
func graphics(s *Session) []graphic {
	chunks := s.Chunks()
	out := s.Output()
	starts := make([]int, len(chunks))
	total := 0
	for i, c := range chunks {
		starts[i], total = total, total+len(c.Data)
	}
	all := out[:total]
	var found []graphic
	ci := 0
	for off := 0; ; {
		i := bytes.Index(all[off:], []byte("\x1b_G"))
		if i < 0 {
			return found
		}
		i += off
		for ci+1 < len(starts) && starts[ci+1] <= i {
			ci++
		}
		end := i + 3
		for end < len(all) && all[end] != ';' && all[end] != 0x1b {
			end++
		}
		g := graphic{at: chunks[ci].At, keys: map[string]string{}}
		for _, kv := range strings.Split(string(all[i+3:end]), ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				g.keys[k] = v
			}
		}
		found = append(found, g)
		off = end
	}
}

// Reports whether the command starts an image transmission rather than a query, delete, placement or later chunk
func (g graphic) transmits() bool {
	switch g.keys["a"] {
	case "t", "T":
		return true
	case "":
		for _, k := range []string{"f", "s", "v", "i", "t", "o", "S"} {
			if _, ok := g.keys[k]; ok {
				return true
			}
		}
	}
	return false
}

// Sixel device control strings, DCS with numeric parameters and the q final
var sixelStart = regexp.MustCompile(`\x1bP[0-9;]*q`)

// Opens the arena in konsole and asserts every d keystroke is answered on screen within 300ms
func TestKonsoleResponsive(t *testing.T) {
	r := newRig(t, Konsole, konsoleScreen, map[string]string{})
	r.waitBar()
	frame := r.openArena()
	time.Sleep(time.Second)
	r.pressDock(frame, 2, nil)
}

// Asserts the glyph display draws the world in konsole and sends no image of any protocol
func TestKonsoleArenaGlyphs(t *testing.T) {
	r := newRig(t, Konsole, konsoleScreen, map[string]string{"sprint.arena.display": "glyphs", "sprint.arena.glyphs": "shapes", "sprint.arena.pixels": "shapes"})
	r.waitBar()
	frame := r.openArena()
	img, ok := r.waitWorld(frame, false, 10*time.Second)
	r.check(ok, "world", []image.Image{img}, "the glyph world never showed a drawn scene")
	time.Sleep(2 * time.Second)
	var sent []string
	for _, g := range graphics(r.s) {
		if g.transmits() || g.keys["a"] == "p" {
			sent = append(sent, fmt.Sprint(g.keys))
		}
	}
	r.check(len(sent) == 0, "kitty", nil, "%d kitty transmissions or placements went out, first %v", len(sent), sent[:min(len(sent), 1)])
	out := r.s.Output()
	r.check(!sixelStart.Match(out), "sixel", nil, "sixel data went out")
	r.check(!bytes.Contains(out, []byte("\x1b]1337;File=")), "iterm", nil, "iTerm2 images went out")
}

// Asserts the image display streams direct kitty frames konsole draws while d stays answered within 300ms
func TestKonsoleArenaImage(t *testing.T) {
	r := newRig(t, Konsole, konsoleScreen, map[string]string{"sprint.arena.display": "image", "sprint.arena.pixels": "image"})
	r.waitBar()
	frame := r.openArena()
	direct := func(g graphic) bool {
		return g.transmits() && (g.keys["t"] == "d" || g.keys["t"] == "" || g.keys["o"] == "z")
	}
	err := r.s.WaitFor(func([]byte) bool {
		for _, g := range graphics(r.s) {
			if direct(g) {
				return true
			}
		}
		return false
	}, 15*time.Second)
	r.check(err == nil, "direct", []image.Image{r.shot()}, "no direct kitty transmission, t=d or o=z, reached konsole, %v", err)
	img, ok := r.waitWorld(frame, true, 10*time.Second)
	r.check(ok, "world", []image.Image{img}, "the blank world cells never showed a drawn image")
	r.pressDock(frame, 2, func(at, until time.Time) {
		n := 0
		for _, g := range graphics(r.s) {
			if g.transmits() && g.at.After(at) && !g.at.After(until) {
				n++
			}
		}
		r.check(n > 0, "stream", nil, "no kitty frame streamed in the second after d")
	})
	refused, said := arrival(r.s.Input(), "ENOTSUPPORTED")
	shared, placeholders, total := 0, 0, 0
	var late []time.Duration
	for _, g := range graphics(r.s) {
		if g.transmits() {
			total++
		}
		if g.keys["t"] == "s" {
			shared++
			if said && g.at.After(refused) {
				late = append(late, g.at.Sub(refused))
			}
		}
		if g.keys["U"] == "1" {
			placeholders++
		}
	}
	t.Logf("%d transmissions, %d by shared memory, konsole refused shared memory %v", total, shared, said)
	r.check(len(late) == 0, "shared", nil, "%d shared memory transmissions left after konsole answered ENOTSUPPORTED, %v after the answer", len(late), late)
	r.check(placeholders == 0, "placeholders", nil, "%d kitty commands with U=1 went to konsole", placeholders)
}

// Read time of the chunk that completed the first occurrence of the text in the stream
func arrival(chunks []Chunk, text string) (time.Time, bool) {
	var all []byte
	var ends []int
	for _, c := range chunks {
		all = append(all, c.Data...)
		ends = append(ends, len(all))
	}
	i := bytes.Index(all, []byte(text))
	if i < 0 {
		return time.Time{}, false
	}
	k, _ := slices.BinarySearch(ends, i+len(text))
	return chunks[k].At, true
}

// Asserts the image display sends sixel to a vt340 xterm and the world shows it
func TestXtermSixel(t *testing.T) {
	r := newRig(t, Xterm, xtermScreen, map[string]string{"sprint.arena.display": "image", "sprint.arena.pixels": "image"})
	r.waitBar()
	frame := r.openArena()
	err := r.s.WaitFor(sixelStart.Match, 15*time.Second)
	r.check(err == nil, "sixel", []image.Image{r.shot()}, "no sixel data reached xterm, %v", err)
	img, ok := r.waitWorld(frame, true, 10*time.Second)
	r.check(ok, "world", []image.Image{img}, "the blank world cells never showed a drawn sixel image")
}

// Drives the probe program in konsole to check input, latency, calibration, screenshots and resize forwarding
func TestKonsoleRig(t *testing.T) {
	if err := Missing("Xvfb", "import", "konsole"); err != nil {
		t.Skipf("termlab needs Xvfb, import and konsole: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lab, err := Start(konsoleScreen.X, konsoleScreen.Y)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lab.Close(); err != nil {
			t.Error(err)
		}
	})
	s, err := lab.Open(Konsole, Program{Path: self, Env: []string{ProbeEnv + "=1"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	wait := func(text string) {
		t.Helper()
		if err := s.WaitFor(func(out []byte) bool { return bytes.Contains(out, []byte(text)) }, 5*time.Second); err != nil {
			t.Fatalf("waiting for %q: %v, output %q", text, err, s.Output())
		}
	}
	sent := func(_ time.Time, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	wait("size 160x48")
	sent(s.Type("hello"))
	lat, err := s.Latency(answerLimit)
	if err != nil || lat > answerLimit {
		t.Errorf("echo latency %s, %v", lat, err)
	}
	wait(fmt.Sprintf("in %x", "hello"))
	sent(s.Key([]byte("\x1b[A")))
	wait(fmt.Sprintf("in %x", "\x1b[A"))
	sent(s.Mouse(3, 4, 0, true))
	wait(fmt.Sprintf("in %x", "\x1b[<0;3;4M"))
	sent(s.Mouse(3, 4, 0, false))
	wait(fmt.Sprintf("in %x", "\x1b[<0;3;4m"))
	before, err := s.Shot()
	if err != nil {
		t.Fatal(err)
	}
	sent(s.Type("mark 40 30\r"))
	after, err := s.WaitChange(before, s.Cells(40, 30, 1, 1), 5*time.Second)
	if err != nil {
		t.Fatalf("mark never showed, %v, output %q", err, s.Output())
	}
	if got, want := DiffBounds(before, after, s.Cells(30, 25, 20, 10)), s.Cells(40, 30, 1, 1); got != want {
		t.Errorf("marked cell drew at %v, calibration puts it at %v", got, want)
	}
	sent(s.Type("resize 100 30\r"))
	wait("size 100x30")
}
